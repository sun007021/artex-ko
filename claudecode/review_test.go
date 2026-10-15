package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

// 코드 리뷰(2026-10-12) 반영분의 회귀 테스트.

// streamAs 는 다른 ARTEX 세션 ID·공급자로 한 턴을 돌린다(오류만 돌려준다).
func (h *harness) streamAs(p *Provider, sessID string, msgs ...llm.Message) error {
	ctx := transcript.WithSessionID(context.Background(), sessID)
	for _, err := range p.Stream(ctx, llm.CompletionRequest{System: h.sys, Messages: msgs, Tools: h.tools}) {
		if err != nil {
			return err
		}
	}
	return nil
}

func asError(t *testing.T, err error) *Error {
	t.Helper()
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("*Error 가 아니다: %T %v", err, err)
	}
	return ce
}

// ---- 1. 도구 목록 ----

func TestToolSetChangeForcesResync(t *testing.T) {
	h := newHarness(t)
	h.script(0, fakeMsg{Blocks: []fakeBlock{textBlock("a")}, Stop: "end_turn"}, fakeMsg{Blocks: []fakeBlock{textBlock("b")}, Stop: "end_turn"})
	h.script(1, fakeMsg{Blocks: []fakeBlock{textBlock("c")}, Stop: "end_turn"})
	h.user("q1")
	if r := h.run(); r.err != nil {
		t.Fatal(r.err)
	}
	// 도구가 빠지는 것은 상관없다(호출이 거절된다): 같은 프로세스를 쓴다.
	h.tools = h.tools[:2]
	h.user("q2")
	if r := h.run(); r.err != nil || r.msg.Text() != "b" {
		t.Fatalf("%v %q", r.err, r.msg.Text())
	}
	if h.procs() != 1 {
		t.Fatalf("도구 제거로 재동기화됨: procs=%d", h.procs())
	}
	// 새 도구가 생기면 claude 는 tools/list 를 다시 읽지 않으므로 새 프로세스여야 한다.
	h.tools = append(testTools(), llm.ToolSchema{Name: "NewScanner", Description: "n", InputSchema: map[string]any{"type": "object"}})
	h.user("q3")
	if r := h.run(); r.err != nil {
		t.Fatal(r.err)
	}
	if h.procs() != 2 {
		t.Fatalf("도구 추가는 재동기화여야 한다: procs=%d", h.procs())
	}
	var names []string
	for _, e := range h.logs(1, "tools") {
		for _, n := range e["names"].([]any) {
			names = append(names, fmt.Sprint(n))
		}
	}
	if !strings.Contains(strings.Join(names, ","), "NewScanner") {
		t.Fatalf("새 프로세스가 새 도구를 봐야 한다: %v", names)
	}
}

func TestToolSetFingerprintChangesOnSchema(t *testing.T) {
	a := toolSet{"x": "h1"}
	if !a.covers(toolSet{"x": "h1"}) || a.covers(toolSet{"x": "h2"}) || a.covers(toolSet{"y": "h1"}) {
		t.Fatal("covers 규칙")
	}
	if !(toolSet{"x": "h1", "y": "h2"}).covers(toolSet{"x": "h1"}) {
		t.Fatal("부분집합은 덮는다")
	}
}

// ---- 2. 멈춘 claude ----

func TestStalledClaudeIsKilledAndSlotReleased(t *testing.T) {
	h := newHarness(t)
	h.m.stall = 400 * time.Millisecond
	h.script(0, fakeMsg{Blocks: []fakeBlock{textBlock("x")}, Stop: "end_turn", Hang: true})
	h.script(1, fakeMsg{Blocks: []fakeBlock{textBlock("back")}, Stop: "end_turn"})
	h.user("go")
	start := time.Now()
	r := h.run()
	if r.err == nil {
		t.Fatal("멈춘 claude 가 오류 없이 끝남")
	}
	if ce := asError(t, r.err); ce.Kind != KindStalled {
		t.Fatalf("kind=%v err=%v", ce.Kind, r.err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("감시가 늦다: %v", d)
	}
	if n := h.m.lim.active(); n != 0 {
		t.Fatalf("슬롯이 남음: %d", n)
	}
	waitFor(t, 8*time.Second, "멈춘 프로세스 정리", func() bool { return !pidAlive(h.pid(0)) })
	// 다음 호출은 새 프로세스로 이어진다.
	if r := h.run(); r.err != nil || r.msg.Text() != "back" {
		t.Fatalf("err=%v text=%q", r.err, r.msg.Text())
	}
}

func TestApiRetryResetsStallClockUpToCap(t *testing.T) {
	h := newHarness(t)
	h.m.stall = 300 * time.Millisecond
	// 80ms 간격 api_retry 는 시계를 되돌리지만 maxRetryResets 번까지만이다.
	h.script(0, fakeMsg{Retries: maxRetryResets * 4, RetryEveryMS: 80})
	h.user("go")
	start := time.Now()
	r := h.run()
	took := time.Since(start)
	if r.err == nil || asError(t, r.err).Kind != KindStalled {
		t.Fatalf("상한을 넘긴 재시도는 멈춘 것으로 본다: %v", r.err)
	}
	if min := time.Duration(maxRetryResets) * 80 * time.Millisecond; took < min {
		t.Fatalf("api_retry 가 시계를 되돌리지 않았다: %v < %v", took, min)
	}
	if max := time.Duration(maxRetryResets*4) * 80 * time.Millisecond; took > max {
		t.Fatalf("재시도가 시계를 무한정 되돌렸다: %v > %v", took, max)
	}
}

func TestConsumeNoticesClosedWhenExitEventDropped(t *testing.T) {
	h := newHarness(t)
	s := newSession(h.m, "x")
	p := &proc{m: h.m, key: "x", tools: &s.tools, pair: newCallPairer(h.m.logf),
		events: make(chan sEvent, 4), closed: make(chan struct{})}
	p.exitErr.Store(&Error{Kind: KindCrash, Message: "claude 프로세스가 비정상 종료했습니다: boom"})
	// 종료 이벤트가 유실된 상황: 프로세스는 닫혔는데 채널에는 종료 이벤트가 없다.
	p.markClosed()
	done := make(chan error, 1)
	go func() {
		_, err := s.consume(context.Background(), p, func(llm.StreamEvent, error) bool { return true })
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || asError(t, err).Kind != KindCrash || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("크래시 오류여야 한다: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("종료 이벤트가 유실되면 consume 이 영원히 기다린다")
	}
}

func TestConsumeDrainsQueuedEventsBeforeClosed(t *testing.T) {
	h := newHarness(t)
	s := newSession(h.m, "x")
	p := &proc{m: h.m, key: "x", tools: &s.tools, pair: newCallPairer(h.m.logf),
		events: make(chan sEvent, 4), closed: make(chan struct{})}
	// 결과(result)가 큐에 있는 채로 프로세스가 닫혔다 → 정상 마감이어야 한다.
	p.events <- sEvent{kind: evResult}
	p.markClosed()
	if _, err := s.consume(context.Background(), p, func(llm.StreamEvent, error) bool { return true }); err != nil {
		t.Fatalf("큐에 남은 result 를 먼저 처리해야 한다: %v", err)
	}
}

// ---- 3. 사용량 한도 게이트 ----

func TestUsageLimitGateIsPerModelUnlessGlobal(t *testing.T) {
	h := newHarness(t)
	opus := h.m.NewProvider(Options{Binary: h.p.opts.Binary, Model: "opus", WorkDir: h.p.opts.WorkDir})
	sonnet := h.m.NewProvider(Options{Binary: h.p.opts.Binary, Model: "sonnet", WorkDir: h.p.opts.WorkDir})
	user := llm.UserText("go")

	// Opus 전용 주간 한도: Sonnet 은 막히지 않는다.
	h.script(0, fakeMsg{ErrCat: "rate_limit", ErrText: "You've hit your usage limit", RateLimit: "seven_day_opus"})
	err := h.streamAs(opus, "o1", user)
	if err == nil || !IsUsageLimit(err) {
		t.Fatalf("사용량 한도여야 한다: %v", err)
	}
	if ce := asError(t, err); ce.Global() {
		t.Fatalf("모델별 한도가 전역으로 분류됨: %+v", ce)
	}
	if err := h.streamAs(opus, "o2", user); !IsUsageLimit(err) {
		t.Fatalf("Opus 는 게이트가 막아야 한다: %v", err)
	}
	h.script(1, fakeMsg{Blocks: []fakeBlock{textBlock("sonnet ok")}, Stop: "end_turn"})
	if err := h.streamAs(sonnet, "s1", user); err != nil {
		t.Fatalf("Sonnet 이 Opus 한도에 막힘: %v", err)
	}

	// 5시간 한도는 계정 전체다: Sonnet 도 막힌다.
	h.script(2, fakeMsg{ErrCat: "rate_limit", ErrText: "You've hit your usage limit", RateLimit: "five_hour"})
	if err := h.streamAs(sonnet, "s2", user); !IsUsageLimit(err) {
		t.Fatalf("%v", err)
	}
	if err := h.streamAs(opus, "o3", user); !IsUsageLimit(err) || !asError(t, err).Global() && !strings.Contains(err.Error(), UsageLimitMarker) {
		t.Fatalf("전역 한도는 모든 모델을 막아야 한다: %v", err)
	}
	if h.procs() != 3 {
		t.Fatalf("한도 중에 프로세스를 띄움: %d", h.procs())
	}
}

func TestErrorGlobalScopes(t *testing.T) {
	for scope, want := range map[string]bool{"five_hour": true, "seven_day": true, "seven_day_opus": false, "": false} {
		if got := (&Error{Kind: KindUsageLimit, Scope: scope}).Global(); got != want {
			t.Errorf("%q → %v", scope, got)
		}
	}
	if (&Error{Kind: KindRateLimit, Scope: "five_hour"}).Global() {
		t.Error("사용량 한도가 아닌 오류는 전역이 아니다")
	}
}

// ---- 4. {{.Now}} ----

func TestSecondResolutionTimeDoesNotForceResync(t *testing.T) {
	h := newHarness(t)
	h.script(0, fakeMsg{Blocks: []fakeBlock{textBlock("a")}, Stop: "end_turn"}, fakeMsg{Blocks: []fakeBlock{textBlock("b")}, Stop: "end_turn"})
	h.script(1, fakeMsg{Blocks: []fakeBlock{textBlock("c")}, Stop: "end_turn"})
	h.sys = []string{"ROLE. Now: 2026-10-12 13:45:10 KST."}
	h.user("q1")
	h.run()
	h.sys = []string{"ROLE. Now: 2026-10-12 13:45:59 KST."}
	h.user("q2")
	if r := h.run(); r.err != nil {
		t.Fatal(r.err)
	}
	if h.procs() != 1 {
		t.Fatalf("초 단위 시각 변화로 재동기화됨: procs=%d", h.procs())
	}
	// 프롬프트 문구는 바꾸지 않는다: claude 에는 시작 때의 문구가 그대로 간다.
	if got := h.meta(0).System; !strings.HasPrefix(got, "ROLE. Now: 2026-10-12 13:45:10 KST.") {
		t.Fatalf("시스템 프롬프트가 변형됨: %q", got)
	}
	// 시(hour)가 바뀌면 한 번 재동기화한다.
	h.sys = []string{"ROLE. Now: 2026-10-12 14:00:01 KST."}
	h.user("q3")
	if r := h.run(); r.err != nil {
		t.Fatal(r.err)
	}
	if h.procs() != 2 {
		t.Fatalf("시 단위가 넘어가면 재동기화: procs=%d", h.procs())
	}
}

func TestStableSystem(t *testing.T) {
	a := stableSystem("x 2026-10-12T13:45:10Z y")
	b := stableSystem("x 2026-10-12T13:59:59.123Z y")
	if a != b {
		t.Fatalf("%q != %q", a, b)
	}
	if stableSystem("x 2026-10-12 13:00:00 y") == stableSystem("x 2026-10-12 14:00:00 y") {
		t.Fatal("시가 다르면 달라야 한다")
	}
	if stableSystem("x 2026-10-13 13:00:00 y") == stableSystem("x 2026-10-12 13:00:00 y") {
		t.Fatal("날짜가 다르면 달라야 한다")
	}
	if stableSystem("no time here") != "no time here" {
		t.Fatal("시각이 없으면 그대로")
	}
}

// ---- 5. 닫기와 고아 프로세스 ----

func TestCloseAllMidTurnLeavesNoOrphan(t *testing.T) {
	for _, closeFn := range []struct {
		name string
		fn   func(h *harness)
	}{
		{"CloseAll", func(h *harness) { h.m.CloseAll() }},
		{"CloseSession", func(h *harness) { h.m.CloseSession("sess-1") }},
		{"SetEnabled(false)", func(h *harness) { h.m.SetEnabled(false) }},
	} {
		t.Run(closeFn.name, func(t *testing.T) {
			h := newHarness(t)
			h.script(0, fakeMsg{Blocks: []fakeBlock{textBlock("slow")}, Stop: "end_turn", DelayMS: 30000})
			h.script(1, fakeMsg{Blocks: []fakeBlock{textBlock("orphan?")}, Stop: "end_turn"})
			h.user("go")
			res := make(chan turnResult, 1)
			go func() { res <- h.run() }()
			waitFor(t, 8*time.Second, "응답 생성 시작", func() bool { return len(h.logs(0, "gen_start")) > 0 })
			pid := h.pid(0)
			closeFn.fn(h)
			select {
			case r := <-res:
				if r.err == nil {
					t.Fatal("닫힌 세션의 호출이 성공으로 끝남")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("닫은 뒤에도 호출이 끝나지 않는다")
			}
			waitFor(t, 8*time.Second, "프로세스 종료", func() bool { return !pidAlive(pid) })
			time.Sleep(300 * time.Millisecond) // 재동기화로 새 프로세스를 띄우려는 시도가 있었다면 여기서 보인다
			if n := h.procs(); n != 1 {
				t.Fatalf("닫은 뒤 새 프로세스가 떴다(고아): procs=%d", n)
			}
			if n := h.m.lim.active(); n != 0 {
				t.Fatalf("슬롯이 남음: %d", n)
			}
		})
	}
}

func TestCrashDoesNotRetryAfterFeatureDisabled(t *testing.T) {
	// 프로세스가 죽은 순간 기능이 꺼졌다면 재동기화 재시도로 새 프로세스를 띄우면 안 된다.
	h := newHarness(t)
	h.script(0, fakeMsg{Crash: true})
	h.script(1, fakeMsg{Blocks: []fakeBlock{textBlock("x")}, Stop: "end_turn"})
	h.m.SetEnabled(false)
	h.user("go")
	if r := h.run(); r.err == nil {
		t.Fatal("꺼진 기능이 성공함")
	}
	if h.procs() != 0 {
		t.Fatalf("꺼진 상태에서 프로세스가 떴다: %d", h.procs())
	}
}

// ---- 6. LRU ----

func TestEvictionPrefersIdleOverParked(t *testing.T) {
	h := newHarness(t)
	h.m.SetMaxLive(2)
	h.script(0, fakeMsg{Blocks: []fakeBlock{tool("Bash", `{}`)}, Stop: "tool_use"}) // s0: 도구 결과 대기(parked), 가장 오래됨
	h.script(1, fakeMsg{Blocks: []fakeBlock{textBlock("a")}, Stop: "end_turn"})     // s1: 유휴
	h.script(2, fakeMsg{Blocks: []fakeBlock{textBlock("a")}, Stop: "end_turn"})     // s2: 새로 들어옴
	var logged atomic.Value
	h.m.SetLogger(func(format string, a ...any) {
		if strings.Contains(format, "상한") {
			logged.Store(fmt.Sprintf(format, a...))
		}
	})
	for i := 0; i < 3; i++ {
		if err := h.streamAs(h.p, fmt.Sprintf("s%d", i), llm.UserText("q")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	p0, p1 := h.pid(0), h.pid(1)
	waitFor(t, 8*time.Second, "유휴 세션 정리", func() bool { return !pidAlive(p1) })
	if !pidAlive(p0) {
		t.Fatal("도구 결과를 기다리는 세션보다 유휴 세션을 먼저 정리해야 한다")
	}
	if s, _ := logged.Load().(string); !strings.Contains(s, "session=s1") || !strings.Contains(s, "idle") {
		t.Fatalf("축출 로그: %q", s)
	}
}

func TestMaxLiveConfigurable(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Shutdown)
	if m.MaxLive() <= 0 {
		t.Fatal("기본값")
	}
	m.SetMaxLive(3)
	if m.MaxLive() != 3 {
		t.Fatalf("%d", m.MaxLive())
	}
	m.SetMaxLive(0) // 잘못된 값은 기본으로
	if m.MaxLive() <= 0 {
		t.Fatalf("%d", m.MaxLive())
	}
}

func TestReapedSessionsLeaveTheMap(t *testing.T) {
	h := newHarness(t)
	h.script(0, fakeMsg{Blocks: []fakeBlock{textBlock("a")}, Stop: "end_turn"})
	h.user("go")
	h.run()
	pid := h.pid(0)
	h.m.reapOnce(time.Now().Add(h.m.idleTTL + time.Minute))
	waitFor(t, 8*time.Second, "유휴 만료 정리", func() bool { return !pidAlive(pid) })
	h.m.mu.Lock()
	_, ok := h.m.sessions["sess-1"]
	h.m.mu.Unlock()
	if ok {
		t.Fatal("정리된 세션이 맵에 남아 누수된다")
	}
}

// ---- 7. 환경 허용 목록 ----

func TestEnvAllowlist(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin", "HOME=/home/u", "LC_ALL=C", "XDG_CONFIG_HOME=/x", "HTTPS_PROXY=http://p:1", "CLAUDE_CONFIG_DIR=/c",
		"ANTHROPIC_API_KEY=sk-secret", "ANTHROPIC_AUTH_TOKEN=tok", "CLAUDE_CODE_OAUTH_TOKEN=oauth",
		"CLAUDE_CODE_USE_MANTLE=1", "CLAUDE_CODE_USE_BEDROCK=1", "ANTHROPIC_BASE_URL=http://evil",
		"CLAUDE_CODE_SIMPLE=1", "ANTHROPIC_MODEL=x", "AWS_ACCESS_KEY_ID=a", "OPENAI_API_KEY=k", "ARTEX_DB_DSN=postgres://u:p@h/db",
		"MY_HARMLESS_VAR=1",
	}
	env := SanitizedEnv(parent, "FOO=bar")
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for _, k := range []string{"PATH", "HOME", "LC_ALL", "XDG_CONFIG_HOME", "HTTPS_PROXY", "CLAUDE_CONFIG_DIR", "FOO"} {
		if _, ok := got[k]; !ok {
			t.Errorf("%s 는 넘겨야 한다", k)
		}
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_MANTLE",
		"CLAUDE_CODE_USE_BEDROCK", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_SIMPLE", "ANTHROPIC_MODEL", "AWS_ACCESS_KEY_ID", "OPENAI_API_KEY",
		"ARTEX_DB_DSN", "MY_HARMLESS_VAR"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s 가 자식 환경에 새어 들어간다", k)
		}
	}
	if got["CLAUDE_CODE_DISABLE_CLAUDE_MDS"] != "1" {
		t.Error("강화 변수 누락")
	}
	// 운영 허용 목록에 테스트용 변수가 없어야 한다는 보장은 init 이 연 항목만 해당한다 — 위험 이름은 절대 열리지 않는다.
	for k := range allowedEnv {
		up := strings.ToUpper(k)
		if strings.HasPrefix(up, "ANTHROPIC_") || strings.HasPrefix(up, "CLAUDE_CODE_USE_") || strings.Contains(up, "TOKEN") || strings.Contains(up, "KEY") {
			t.Errorf("허용 목록에 위험한 이름: %s", k)
		}
	}
	// Windows 는 이름의 대소문자를 구분하지 않는다.
	if !envAllowed("Path") || !envAllowed("SystemRoot") {
		t.Error("대소문자 무시")
	}
}

// ---- 8. 짝맞춤 ----

func TestPairingPrefersToolUseIDFromMeta(t *testing.T) {
	c := newCallPairer(nil)
	c.expose("tool_A", "Bash", json.RawMessage(`{"cmd":"same"}`))
	c.expose("tool_B", "Bash", json.RawMessage(`{"cmd":"same"}`))
	known := func(string) bool { return true }
	closed := make(chan struct{})
	out := c.match(context.Background(), closed, known, "Bash", json.RawMessage(`{"cmd":"same"}`), "tool_B")
	if out.aw == nil || out.aw.id != "tool_B" {
		t.Fatalf("ID 우선 짝맞춤 실패: %+v", out)
	}
	out = c.match(context.Background(), closed, known, "Bash", json.RawMessage(`{"cmd":"same"}`), "")
	if out.aw == nil || out.aw.id != "tool_A" {
		t.Fatalf("ID 가 없으면 (이름, 인자) 순서대로: %+v", out)
	}
	// 알 수 없는 ID 는 (이름, 인자)로 떨어진다.
	c2 := newCallPairer(nil)
	c2.expose("tool_C", "Bash", json.RawMessage(`{"cmd":"x"}`))
	out = c2.match(context.Background(), closed, known, "Bash", json.RawMessage(`{"cmd":"x"}`), "tool_nope")
	if out.aw == nil || out.aw.id != "tool_C" {
		t.Fatalf("%+v", out)
	}
}

func TestPairingFallbackIsLogged(t *testing.T) {
	old := claimWait
	claimWait = 50 * time.Millisecond
	defer func() { claimWait = old }()
	var mu sync.Mutex
	var logs []string
	c := newCallPairer(func(f string, a ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); mu.Unlock() })
	c.expose("tool_A", "Bash", json.RawMessage(`{"cmd":"a"}`))
	// claude 가 인자를 정규화해서 달라졌다.
	out := c.match(context.Background(), make(chan struct{}), func(string) bool { return true }, "Bash", json.RawMessage(`{"cmd":"a","extra":1}`), "")
	if out.aw == nil || out.aw.id != "tool_A" {
		t.Fatalf("%+v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 1 || !strings.Contains(logs[0], "tool_A") {
		t.Fatalf("대체 짝맞춤을 로그로 남겨야 한다: %v", logs)
	}
}

func TestMetaToolUseIDRoundTrip(t *testing.T) {
	// 같은 도구·같은 인자의 병렬 호출 둘: 도착 순서가 뒤바뀌어도 _meta 의 ID 로 제 결과를 받는다.
	for i := 0; i < 6; i++ {
		h := newHarness(t)
		a, b := tool("Bash", `{"cmd":"same"}`), tool("Bash", `{"cmd":"same"}`)
		a.ID, b.ID, a.Meta, b.Meta = "toolu_A", "toolu_B", true, true
		h.script(0,
			fakeMsg{Blocks: []fakeBlock{a, b}, Stop: "tool_use", Parallel: true},
			fakeMsg{Blocks: []fakeBlock{textBlock("fin")}, Stop: "end_turn"},
		)
		h.user("go")
		if r := h.run(); r.err != nil {
			t.Fatal(r.err)
		}
		last := h.msgs[len(h.msgs)-1]
		var blocks []llm.ContentBlock
		for _, u := range last.ToolUses() {
			blocks = append(blocks, llm.ToolResultText(u.ID, "res-"+u.ID, false))
		}
		h.msgs = append(h.msgs, llm.Message{Role: llm.RoleUser, Content: blocks})
		if r := h.run(); r.err != nil {
			t.Fatal(r.err)
		}
		got := h.logs(0, "tool_result")
		if len(got) != 2 {
			t.Fatalf("%v", got)
		}
		for _, e := range got {
			if e["text"] != "res-"+fmt.Sprint(e["id"]) {
				t.Fatalf("호출 %v 가 다른 호출의 결과를 받았다: %v", e["id"], e["text"])
			}
		}
	}
}

// ---- 9. sessionKey ----

func TestSessionKeyIgnoresReminder(t *testing.T) {
	rem := func(d string) llm.Message {
		return llm.UserText("<system-reminder>today is " + d + "</system-reminder>")
	}
	a := sessionKey(context.Background(), "sys", []llm.Message{rem("mon"), llm.UserText("hello")})
	b := sessionKey(context.Background(), "sys", []llm.Message{rem("tue"), llm.UserText("hello"), llm.UserText("more")})
	c := sessionKey(context.Background(), "sys", []llm.Message{rem("mon"), llm.UserText("other")})
	if a != b {
		t.Fatalf("리마인더가 달라졌다고 다른 세션이 되면 안 된다: %q %q", a, b)
	}
	if a == c {
		t.Fatalf("첫 사용자 메시지가 다르면 다른 세션이어야 한다")
	}
}

// ---- 10. 점검(preflight) ----

func TestPreflightSingleflightAndCancelDoesNotPoisonCache(t *testing.T) {
	h := newHarness(t)
	var calls atomic.Int32
	release := make(chan struct{})
	h.m.probe.fn = func(ctx context.Context, bin string) Status {
		calls.Add(1)
		<-release
		return Status{Usable: true}
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	errA := make(chan *Error, 1)
	errB := make(chan *Error, 1)
	go func() { errA <- h.m.preflight(ctxA, "bin") }()
	waitFor(t, 3*time.Second, "첫 점검 시작", func() bool { return calls.Load() == 1 })
	go func() { errB <- h.m.preflight(context.Background(), "bin") }()
	time.Sleep(50 * time.Millisecond)
	cancelA()
	select {
	case e := <-errA:
		if e == nil {
			t.Fatal("취소된 호출은 오류여야 한다")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("취소된 호출이 점검 완료를 기다린다")
	}
	close(release)
	select {
	case e := <-errB:
		if e != nil {
			t.Fatalf("다른 호출자의 취소가 이 호출을 실패시켰다: %v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("두 번째 호출이 끝나지 않는다")
	}
	if e := h.m.preflight(context.Background(), "bin"); e != nil {
		t.Fatalf("캐시가 취소 결과로 오염됨: %v", e)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("동시 점검은 하나만 실행해야 한다: %d", n)
	}
}

// ---- 11. 로거 경쟁 ----

func TestSetLoggerRace(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Shutdown)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.SetLogger(func(string, ...any) {})
				m.SetLogger(nil)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.logf("x %d", j)
			}
		}()
	}
	wg.Wait()
}
