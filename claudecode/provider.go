package claudecode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"iter"
	"strings"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

// Options 는 프로필 하나에서 오는 claude 구동 설정이다. 모델과 추론 강도는 프로필의 Model /
// ReasoningEffort 를 그대로 --model / --effort 로 넘긴다(비면 claude 의 기본값).
type Options struct {
	Binary  string // claude 실행 파일(비면 PATH 의 "claude")
	Model   string
	Effort  string // low | medium | high | xhigh | max
	Proxy   string // 프로필에 명시된 프록시(비면 사용자 환경을 그대로 상속)
	WorkDir string // 세션별 임시 디렉터리를 만들 위치(ARTEX 데이터 디렉터리 아래)
}

// Provider 는 llm.Provider 구현이다. 프로필마다 하나 만들어지지만 동시 실행 상한·한도 게이트·세션 표는
// 모두 전역(defaultManager)을 공유한다.
type Provider struct {
	opts Options
	m    *manager
}

// NewProvider 는 claude CLI 로 모델 턴을 만드는 llm.Provider 를 돌려준다.
func NewProvider(opts Options) *Provider {
	opts.Binary = strings.TrimSpace(opts.Binary)
	return &Provider{opts: opts, m: defaultManager}
}

var _ llm.Provider = (*Provider)(nil)

// toolNote 는 시스템 프롬프트 꼬리에 붙는 도구 이름 규약이다. ARTEX 역할 프롬프트는 도구를 이름
// 그대로(Bash, report_finding ...) 부르지만 claude 는 MCP 도구에 mcp__artex__ 접두사를 붙여 보여 준다.
// 프롬프트 본문은 건드리지 않고(번역·수정 금지), 이 한 단락만 코드로 덧붙인다.
const toolNote = "\n\n**工具名约定（claude-code）**：本会话里 ARTEX 的全部工具都以前缀 `" + toolPrefix + "` 暴露给你（例如提示词里写的 `Bash` 就是 `" + toolPrefix + "Bash`，`report_finding` 就是 `" + toolPrefix + "report_finding`）。提示词中出现的任何工具名都按此前缀调用；不要调用列表之外的工具。"

func systemText(segs []string) string {
	parts := make([]string, 0, len(segs))
	for _, s := range segs {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

func shortHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

func (p *Provider) cfgKey() string {
	return shortHash(p.opts.Binary, p.opts.Model, p.opts.Effort, p.opts.Proxy)
}

// sessionKey 는 요청을 장수명 세션에 대응시킨다. agentcore 는 전사를 쓰는 세션이면 ctx 에 ARTEX
// 세션 ID(exp1-worker-i3, conv-12 ...)를 실어 준다. 없으면 시스템 프롬프트와 첫 사용자 메시지로 만든다.
func sessionKey(ctx context.Context, sys string, msgs []llm.Message) string {
	if id := transcript.SessionIDFrom(ctx); id != "" {
		return id
	}
	first := ""
	for _, m := range msgs {
		if m.Role == llm.RoleUser {
			first = m.Text()
			break
		}
	}
	return "anon-" + shortHash(sys, first)
}

// Stream 은 모델 한 턴을 만든다. 도구가 있는 요청은 장수명 세션(공급자 브리지)으로, 도구가 없는 요청
// (압축 요약, 연결 테스트 등)은 일회용 프로세스로 처리한다.
func (p *Provider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		var err error
		if len(req.Tools) == 0 {
			err = p.streamOneShot(ctx, req, yield)
		} else {
			err = p.streamSession(ctx, req, yield)
		}
		if err != nil && !errors.Is(err, errConsumerStopped) {
			yield(llm.StreamEvent{}, err)
		}
	}
}

// Complete 는 Stream 을 모아 완성된 메시지를 돌려준다(프로필이 비스트리밍일 때 agentcore 가 부른다).
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		acc.Add(ev)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

// ---- 일회용(도구 없음) ----

func (p *Provider) streamOneShot(ctx context.Context, req llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) error {
	m := p.m
	if e := m.preflight(ctx, p.opts.Binary); e != nil {
		return e
	}
	release, err := m.lim.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := m.gate.check(ctx); err != nil {
		return err
	}
	s := newSession(m, "oneshot")
	s.setTools(nil)
	proc, e := s.start(p.opts, systemText(req.System), false)
	if e != nil {
		return e
	}
	defer s.kill()
	reminder, msgs := splitReminder(req.Messages)
	if err := proc.send(userInputLine(renderTranscript(reminder, msgs))); err != nil {
		return err
	}
	_, err = s.consume(ctx, proc, yield)
	if err == nil {
		m.gate.success()
	}
	return m.noteResult(err)
}

// ---- 장수명 세션(공급자 브리지) ----

func (p *Provider) streamSession(ctx context.Context, req llm.CompletionRequest, yield func(llm.StreamEvent, error) bool) error {
	m := p.m
	if e := m.preflight(ctx, p.opts.Binary); e != nil {
		return e
	}
	sys := systemText(req.System) + toolNote
	key := sessionKey(ctx, sys, req.Messages)
	reminder, msgs := splitReminder(req.Messages)
	sg := sigs(msgs)

	s := m.session(key)
	s.turn.Lock()
	defer s.turn.Unlock()
	m.evictForRoom(s)

	release, err := m.lim.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := m.gate.check(ctx); err != nil {
		return err
	}
	s.setTools(req.Tools)

	cfgKey, sysHash := p.cfgKey(), shortHash(sys)
	proc, reason := s.plan(cfgKey, sysHash, sg)
	if proc != nil {
		sent, perr := s.resume(proc, msgs[len(s.syncedLen()):])
		if perr != nil {
			return s.failTurn(perr)
		}
		if !sent {
			reason, proc = "대화가 이어붙일 수 없는 형태로 바뀜", nil
		}
	}
	if proc == nil {
		s.logf("재동기화 session=%s (%s)", key, reason)
		s.kill()
		np, serr := s.start(p.opts, sys, true)
		if serr != nil {
			return serr
		}
		proc = np
		s.mu.Lock()
		s.cfgKey, s.sysHash = cfgKey, sysHash
		s.mu.Unlock()
		if err := proc.send(userInputLine(renderTranscript(reminder, msgs))); err != nil {
			return s.failTurn(err)
		}
	}
	s.mu.Lock()
	s.state = stBusy
	s.synced = sg
	s.lastActive = time.Now()
	s.mu.Unlock()

	out, err := s.consume(ctx, proc, yield)
	if err != nil {
		return s.failTurn(m.noteResult(err))
	}
	m.gate.success()
	s.mu.Lock()
	s.synced = append(append([]string(nil), sg...), out.sig)
	s.lastActive = time.Now()
	if out.parked {
		s.state = stParked
	} else {
		s.state = stIdle
	}
	s.mu.Unlock()
	return nil
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

// noteResult 는 결과를 전역 게이트에 반영하고 그대로 돌려준다.
func (m *manager) noteResult(err error) error {
	var ce *Error
	if errors.As(err, &ce) {
		m.gate.record(ce)
	}
	return err
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
	case func() bool {
		select {
		case <-p.closed:
			return true
		default:
			return false
		}
	}():
		return nil, "프로세스가 종료됨"
	case p.broken:
		return nil, "이전 호출이 중단됨"
	case s.cfgKey != cfgKey:
		return nil, "모델/강도/바이너리 설정이 바뀜"
	case s.sysHash != sysHash:
		return nil, "시스템 프롬프트가 바뀜"
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
		s.mu.Lock()
		aws := append([]*await(nil), p.awaiting...)
		s.mu.Unlock()
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
			s.deliver(a, r)
		}
		return true, nil
	}
	if len(results) > 0 || len(extra) == 0 {
		return false, nil // 짝이 없는 tool_result 이거나 보낼 것이 없다
	}
	return true, p.send(userInputLine(extra...))
}

// ---- 이벤트 소비 ----

type turnOut struct {
	sig    string // 이번 턴에 만든 assistant 메시지의 서명
	parked bool   // 도구 호출로 끝났다(결과 대기)
}

// consume 은 한 턴의 이벤트를 ARTEX 로 흘려 보낸다. 모델이 ARTEX 도구를 부르며 끝나면 parked 로 돌아오고,
// 보류된 tools/call 은 열어 둔다. 그 외 종료는 claude 가 result 를 낼 때까지 기다려 stdout 을 비운다.
func (s *session) consume(ctx context.Context, p *proc, yield func(llm.StreamEvent, error) bool) (turnOut, error) {
	acc := llm.NewAccumulator()
	ended := false
	var resultWait <-chan time.Time
	finish := func(parked bool) turnOut {
		return turnOut{sig: msgSig(acc.Message()), parked: parked}
	}
	for {
		select {
		case ev := <-p.events:
			switch ev.kind {
			case evEmit:
				if ended {
					continue
				}
				acc.Add(ev.ev)
				if !yield(ev.ev, nil) {
					return turnOut{}, errConsumerStopped
				}
			case evEnd:
				ended = true
				if ev.stop == "tool_use" && len(ev.toolIDs) > 0 {
					return finish(true), nil
				}
				resultWait = time.After(resultWaitTimeout)
			case evResult:
				if ev.isErr {
					if ev.err != nil && (!ended || ev.err.Kind != KindUnknown) {
						return turnOut{}, ev.err
					}
					s.logf("result 가 오류를 보고했지만 응답은 완결됨: %v", ev.err)
				}
				if !ended { // 메시지 없이 끝난 턴: 빈 응답으로 마감한다
					for _, e := range []llm.StreamEvent{{Type: llm.SEMessageDelta, StopReason: "end_turn"}, {Type: llm.SEMessageStop}} {
						acc.Add(e)
						if !yield(e, nil) {
							return turnOut{}, errConsumerStopped
						}
					}
				}
				return finish(false), nil
			case evFail:
				return turnOut{}, ev.err
			case evExit:
				if ended {
					return finish(false), nil
				}
				return turnOut{}, ev.err
			}
		case <-resultWait:
			s.logf("result 를 기다리다 시간 초과(응답은 완결됨) — 다음 호출에서 재동기화")
			s.mu.Lock()
			p.broken = true
			s.mu.Unlock()
			return finish(false), nil
		case <-ctx.Done():
			return turnOut{}, ctx.Err()
		}
	}
}
