package claudecode

import (
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// ---- 세션 ----
//
// session 은 ARTEX 세션 ID 하나에 대응하는 장수명 `claude -p` 프로세스를 쥐고, 그 위의 턴 상태기계를
// 맡는다. 프로세스 수명은 proc(process.go), 호출 짝맞춤은 callPairer(callpair.go), 출력 번역은
// streamTranslator(translator.go)가 맡고, 세션은 "이 요청을 기존 프로세스에 이어 붙일 수 있는가"를 판단하고
// 이어 붙이는 일만 한다.
//
//	idle   : 프로세스가 stdin 의 다음 사용자 메시지를 기다린다.
//	busy   : 모델 응답을 만드는 중(Provider.Stream 이 이벤트를 소비 중).
//	parked : 모델이 ARTEX 도구를 불렀다. tools/call 들이 응답을 연 채 보류 중이고, agentcore 가 도구를
//	         실행해 다음 Stream 호출에 결과를 넘겨 주기를 기다린다.

type sessState int

const (
	stIdle sessState = iota
	stBusy
	stParked
)

func (st sessState) String() string {
	switch st {
	case stBusy:
		return "busy"
	case stParked:
		return "parked"
	}
	return "idle"
}

type session struct {
	m   *Manager
	key string

	turn  sync.Mutex // Stream 호출 직렬화
	tools toolBox    // 이번 요청이 노출하는 도구

	mu         sync.Mutex
	p          *proc
	state      sessState
	cfgKey     string
	sysHash    string
	synced     []string // claude 세션이 흡수한 ARTEX 메시지 서명(리마인더 제외)
	lastActive time.Time
	closed     bool // CloseSession/CloseAll 로 닫혔다: 더는 프로세스를 띄우지 않는다
	retired    bool // 세션 표에서 빠졌다: 이 세션을 기다리던 호출은 새 세션을 받는다
}

func newSession(m *Manager, key string) *session {
	return &session{m: m, key: key, lastActive: time.Now()}
}

func (s *session) logf(format string, a ...any) { s.m.logf("[claude-code] "+format, a...) }

func (s *session) setTools(schemas []llm.ToolSchema) { s.tools.replace(schemas) }

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *session) isRetired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed || s.retired
}

var errSessionClosed = func() *Error {
	return errf(KindRefused, "claude-code 세션이 닫혀 새 프로세스를 띄우지 않습니다(기능이 꺼졌거나 세션이 끝났습니다)")
}

// start 는 claude 프로세스를 띄워 세션에 붙인다. withMCP 가 false 면 도구 없는 단발 호출용이다. 세션이 이미 닫혔으면
// 띄우지 않는다. 띄우는 사이에 닫혔다면 방금 띄운 프로세스를 바로 정리한다(고아 프로세스를 남기지 않는다).
func (s *session) start(opts Options, sys string, withMCP bool) (*proc, error) {
	if s.isClosed() {
		return nil, errSessionClosed()
	}
	p, err := s.m.spawn(s.key, opts, sys, &s.tools, withMCP)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		p.shutdown()
		return nil, errSessionClosed()
	}
	s.p = p
	s.state = stIdle
	s.lastActive = time.Now()
	s.mu.Unlock()
	return p, nil
}

// kill 은 현재 프로세스를 정리하고 보류 중인 호출을 모두 풀어 준다. 여러 번 불러도 안전하다.
func (s *session) kill() {
	s.mu.Lock()
	p := s.p
	s.p = nil
	s.synced, s.state = nil, stIdle
	s.mu.Unlock()
	if p != nil {
		p.shutdown()
	}
}

// close 는 세션을 닫는다: 응답 중인 호출이 있어도 프로세스를 죽이고, 이후 start 는 거절한다.
func (s *session) close() {
	s.mu.Lock()
	s.closed, s.retired = true, true
	s.mu.Unlock()
	s.kill()
}

// failTurn 은 실패한 턴 뒤에 세션을 정리한다. 프로세스가 어떤 상태인지 알 수 없으므로 죽이고, 다음 호출은
// 재동기화로 처음부터 이어 간다.
func (s *session) failTurn(err error) error {
	s.kill()
	return err
}

func (s *session) syncedLen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.synced
}

// plan 은 이 요청을 기존 프로세스에 이어 붙일 수 있는지 판단한다. 이어 붙일 수 있으면 그 프로세스를,
// 아니면 nil 과 이유를 돌려준다(호출자가 재동기화한다).
func (s *session) plan(cfgKey, sysHash string, sg []string) (*proc, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.p
	switch {
	case p == nil:
		return nil, "새 세션"
	case p.isClosed():
		return nil, "프로세스가 종료됨"
	case p.broken.Load():
		return nil, "이전 호출이 중단됨"
	case s.cfgKey != cfgKey:
		return nil, "모델/강도/바이너리 설정이 바뀜"
	case s.sysHash != sysHash:
		return nil, "시스템 프롬프트가 바뀜"
	case !p.shown.covers(s.tools.fingerprint()):
		// claude 는 tools/list 를 프로세스 시작 때 한 번만 읽는다(listChanged:false). 새 도구나 바뀐 스키마는
		// 새 프로세스여야 보인다. 요청에서 빠진 도구는 상관없다(호출이 거절된다).
		return nil, "도구 목록이 바뀜: " + strings.Join(p.shown.missing(s.tools.fingerprint()), ",")
	case len(sg) <= len(s.synced) || !hasPrefix(sg, s.synced):
		return nil, "대화 앞부분이 바뀜(압축/되감기)"
	}
	return p, ""
}

// resume 은 이미 흡수한 대화 뒤에 붙는 새 꼬리(delta)를 claude 세션에 넘긴다.
//
//	parked : delta 의 tool_result 를 보류 중인 tools/call 의 응답으로 돌려준다. tool_result 가 아닌 나머지 텍스트
//	         (스키마 밖 알림, 수습 지시, 백그라운드 작업 알림 등)는 마지막 결과의 추가 텍스트 블록으로 접어 넣는다.
//	         대기 중인 모든 호출에 결과가 있어야 한다.
//	idle   : delta 의 사용자 텍스트를 stream-json 사용자 메시지 하나로 보낸다.
//
// sent=false 는 이어 붙일 수 없는 형태라는 뜻이다(재동기화).
func (s *session) resume(p *proc, delta []llm.Message) (sent bool, err error) {
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()

	var extra []string
	results := map[string]llm.ContentBlock{}
	for _, m := range delta {
		if m.Role != llm.RoleUser {
			return false, nil
		}
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockToolResult:
				results[b.ToolUseID] = b
			case llm.BlockText:
				if strings.TrimSpace(b.Text) != "" {
					extra = append(extra, b.Text)
				}
			}
		}
	}

	if state == stParked {
		aws := p.pair.pending()
		if len(aws) == 0 {
			return false, nil
		}
		for _, a := range aws {
			if _, ok := results[a.id]; !ok {
				return false, nil
			}
		}
		for i, a := range aws {
			b := results[a.id]
			r := mcpResult{IsError: b.IsError}
			if t := resultText(b); t != "" || len(b.Content) == 0 {
				r.Text = append(r.Text, t)
			}
			if i == len(aws)-1 {
				r.Text = append(r.Text, extra...)
			}
			p.pair.deliver(a, r)
		}
		return true, nil
	}
	if len(results) > 0 || len(extra) == 0 {
		return false, nil // 짝이 없는 tool_result 이거나 보낼 것이 없다
	}
	return true, p.send(userInputLine(extra...))
}
