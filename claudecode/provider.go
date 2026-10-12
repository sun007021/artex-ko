package claudecode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"iter"
	"regexp"
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
// 모두 주입받은 Manager 를 공유한다.
type Provider struct {
	opts Options
	m    *Manager
}

func newProvider(m *Manager, opts Options) *Provider {
	opts.Binary = strings.TrimSpace(opts.Binary)
	return &Provider{opts: opts, m: m}
}

var _ llm.Provider = (*Provider)(nil)

// CloseSession 은 ARTEX 세션 ID 의 claude 프로세스를 정리한다(Manager.CloseSession).
func (p *Provider) CloseSession(id string) { p.m.CloseSession(id) }

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
	// 맨 앞 system-reminder(날짜 등)는 호출마다 달라지므로 키에서 뺀다.
	_, msgs = splitReminder(msgs)
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

// stableSystem 은 시스템 프롬프트에서 호출마다 달라지는 시각을 시 단위로 뭉갠 문자열이다. 프롬프트의
// {{.Now}} 는 초 단위로 갱신되는데(agent/prompt.go), 그대로 해시에 넣으면 대화 턴마다 프로세스를 다시
// 띄워 전사를 재생하게 된다. 해시에만 쓰며 claude 에 넘기는 프롬프트 문구는 바꾸지 않는다 — 프로세스는
// 시작 때의 시각을 그대로 갖고, 시각이 시 단위로 넘어가면 한 번 재동기화한다.
func stableSystem(sys string) string { return timestampRE.ReplaceAllString(sys, "${1} ${2}h") }

var timestampRE = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})[ T](\d{2}):\d{2}(?::\d{2}(?:\.\d+)?)?(?:\s?(?:Z|[A-Z]{2,5}|[+-]\d{2}:?\d{2}))?`)

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
	if err := m.gates.check(ctx, p.opts.Model); err != nil {
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
		m.gates.success(p.opts.Model)
	}
	return m.noteResult(p.opts.Model, err)
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

	s, err := m.acquire(key)
	if err != nil {
		return err
	}
	defer s.turn.Unlock()
	m.evictForRoom(s)

	release, err := m.lim.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := m.gates.check(ctx, p.opts.Model); err != nil {
		return err
	}
	s.setTools(req.Tools)

	t := &sessionTurn{
		p: p, s: s, ctx: ctx, key: key, sys: sys, reminder: reminder, msgs: msgs, sg: sigs(msgs),
		cfgKey: p.cfgKey(), sysHash: shortHash(stableSystem(sys)),
	}
	t.yield = func(ev llm.StreamEvent, err error) bool { t.yielded = true; return yield(ev, err) }

	// 호출자에게 아직 아무것도 넘기지 않았다면 재사용한 프로세스가 턴 사이에 죽은 경우를 투명하게 복구한다
	// (새 프로세스 + 전사 재생). 한 번만 시도하며, 새로 띄운 프로세스의 크래시는 그대로 오류가 된다.
	forceFresh := false
	for {
		out, retry, err := t.attempt(forceFresh)
		if retry && !forceFresh && t.mayRetry() {
			forceFresh = true
			continue
		}
		if err != nil {
			return err
		}
		t.complete(out)
		return nil
	}
}

// sessionTurn 은 세션 위에서 모델 한 턴을 만드는 한 번의 호출이다. 이어 붙이기(attempt)와 마무리(complete)를
// 나눠, 재시도 판단을 호출 루프(streamSession)에 둔다.
type sessionTurn struct {
	p   *Provider
	s   *session
	ctx context.Context
	key string

	sys, cfgKey, sysHash string
	reminder             *llm.Message
	msgs                 []llm.Message
	sg                   []string

	yield   func(llm.StreamEvent, error) bool
	yielded bool // 호출자에게 이미 이벤트를 넘겼다(투명한 복구가 더는 불가능)
}

// mayRetry 는 복구용 재동기화를 시도해도 되는지 알려 준다: 호출자가 아직 살아 있고, 기능이 켜져 있고, 세션이
// 닫히지 않았을 때만이다(끈 직후에 새 프로세스를 띄우지 않는다).
func (t *sessionTurn) mayRetry() bool {
	return t.ctx.Err() == nil && t.p.m.enabled.Load() && !t.s.isClosed()
}

// attempt 는 한 번 시도한다. retry=true 는 호출자에게 아무것도 넘기지 않은 채 재사용한 프로세스가 죽어
// 새 프로세스로 한 번 더 시도해 볼 만하다는 뜻이다(err 는 그 원인; 재시도하지 않으면 그대로 반환된다).
func (t *sessionTurn) attempt(forceFresh bool) (out turnOut, retry bool, err error) {
	s, m := t.s, t.p.m
	var pr *proc
	reason := "새 세션"
	if !forceFresh {
		pr, reason = s.plan(t.cfgKey, t.sysHash, t.sg)
	}
	reused := pr != nil
	if reused {
		sent, perr := s.resume(pr, t.msgs[len(s.syncedLen()):])
		switch {
		case perr != nil && !t.yielded:
			s.logf("재사용한 프로세스에 입력을 쓰지 못함 — 재동기화: %v", perr)
			s.kill()
			return turnOut{}, true, perr
		case perr != nil:
			return turnOut{}, false, s.failTurn(perr)
		case !sent:
			reason, pr, reused = "대화가 이어붙일 수 없는 형태로 바뀜", nil, false
		}
	}
	if pr == nil {
		s.logf("재동기화 session=%s (%s)", t.key, reason)
		s.kill()
		np, serr := s.start(t.p.opts, t.sys, true)
		if serr != nil {
			return turnOut{}, false, serr
		}
		pr = np
		s.mu.Lock()
		s.cfgKey, s.sysHash = t.cfgKey, t.sysHash
		s.mu.Unlock()
		if err := pr.send(userInputLine(renderTranscript(t.reminder, t.msgs))); err != nil {
			return turnOut{}, false, s.failTurn(err)
		}
	}
	s.mu.Lock()
	s.state = stBusy
	s.synced = t.sg
	s.lastActive = time.Now()
	s.mu.Unlock()

	out, err = s.consume(t.ctx, pr, t.yield)
	if err == nil {
		return out, false, nil
	}
	var ce *Error
	if reused && !t.yielded && !forceFresh && t.ctx.Err() == nil && errors.As(err, &ce) && ce.Kind == KindCrash && t.mayRetry() {
		s.logf("재사용한 프로세스가 응답 전에 종료됨 — 재동기화 (%v)", err)
		s.kill()
		return turnOut{}, true, err
	}
	return turnOut{}, false, s.failTurn(m.noteResult(t.p.opts.Model, err))
}

// complete 는 성공한 턴을 세션 상태에 반영한다.
func (t *sessionTurn) complete(out turnOut) {
	s := t.s
	t.p.m.gates.success(t.p.opts.Model)
	s.mu.Lock()
	s.synced = append(append([]string(nil), t.sg...), out.sig)
	s.lastActive = time.Now()
	if out.parked {
		s.state = stParked
	} else {
		s.state = stIdle
	}
	s.mu.Unlock()
}
