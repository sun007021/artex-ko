package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

// ---- 테스트 하네스 ----
//
// harness 는 agentcore 가 하는 일을 흉내 낸다: 대화를 쌓아 두고, 매 호출마다 전체를 Provider.Stream 에 넘기고,
// 돌아온 assistant 메시지를 대화에 붙이고, 도구 결과를 user 메시지로 붙인다.

type harness struct {
	t     *testing.T
	dir   string
	m     *manager
	p     *Provider
	ctx   context.Context
	sys   []string
	tools []llm.ToolSchema
	msgs  []llm.Message
}

type turnResult struct {
	msg    llm.Message
	stop   string
	usage  llm.Usage
	events []llm.StreamEvent
	err    error
}

func testTools() []llm.ToolSchema {
	obj := map[string]any{"type": "object", "properties": map[string]any{}}
	return []llm.ToolSchema{
		{Name: "Bash", Description: "run a command", InputSchema: obj},
		{Name: "report_finding", Description: "record a finding", InputSchema: obj},
		{Name: "Read", Description: "read", InputSchema: obj},
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDECODE_FAKE", "1")
	t.Setenv("FAKE_DIR", dir)
	m := newManager()
	m.enabled.Store(true)
	old := defaultManager
	defaultManager = m
	t.Cleanup(func() { m.shutdown(); defaultManager = old })
	h := &harness{
		t: t, dir: dir, m: m,
		ctx:   transcript.WithSessionID(context.Background(), "sess-1"),
		sys:   []string{"ROLE PROMPT", "second segment"},
		tools: testTools(),
	}
	h.p = NewProvider(Options{Binary: exe, Model: "model-x", Effort: "high", WorkDir: filepath.Join(dir, "work")})
	return h
}

func (h *harness) script(idx int, msgs ...fakeMsg) {
	h.t.Helper()
	b, _ := json.Marshal(msgs)
	if err := os.WriteFile(filepath.Join(h.dir, fmt.Sprintf("script-%d.json", idx)), b, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) user(text string) { h.msgs = append(h.msgs, llm.UserText(text)) }

// run 은 현재 대화로 모델 한 턴을 돌리고 assistant 메시지를 대화에 붙인다.
func (h *harness) run() turnResult { return h.runCtx(h.ctx) }

func (h *harness) runCtx(ctx context.Context) turnResult {
	h.t.Helper()
	var r turnResult
	acc := llm.NewAccumulator()
	req := llm.CompletionRequest{System: h.sys, Messages: h.msgs, Tools: h.tools}
	for ev, err := range h.p.Stream(ctx, req) {
		if err != nil {
			r.err = err
			break
		}
		r.events = append(r.events, ev)
		acc.Add(ev)
	}
	r.msg, r.stop, r.usage = acc.Message(), acc.StopReason, acc.Usage
	if r.err == nil {
		h.msgs = append(h.msgs, r.msg)
	}
	return r
}

// answer 는 마지막 assistant 메시지의 모든 tool_use 에 결과를 붙인다.
func (h *harness) answer(f func(name string, input json.RawMessage) string, extra ...string) {
	h.t.Helper()
	last := h.msgs[len(h.msgs)-1]
	var blocks []llm.ContentBlock
	for _, u := range last.ToolUses() {
		blocks = append(blocks, llm.ToolResultText(u.ID, f(u.Name, u.Input), false))
	}
	if len(blocks) == 0 {
		h.t.Fatal("answer: 마지막 메시지에 tool_use 가 없다")
	}
	for _, e := range extra {
		blocks = append(blocks, llm.TextBlock(e))
	}
	h.msgs = append(h.msgs, llm.Message{Role: llm.RoleUser, Content: blocks})
}

func (h *harness) state() sessState {
	s := h.m.session("sess-1")
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

type logEntry map[string]any

func (h *harness) readLog(idx int) []logEntry {
	h.t.Helper()
	f, err := os.Open(filepath.Join(h.dir, fmt.Sprintf("proc-%d.log", idx)))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []logEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var e logEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func (h *harness) logs(idx int, kind string) []logEntry {
	var out []logEntry
	for _, e := range h.readLog(idx) {
		if e["k"] == kind {
			out = append(out, e)
		}
	}
	return out
}

type procMeta struct {
	Args   []string `json:"args"`
	Env    []string `json:"env"`
	System string   `json:"system"`
	Cwd    string   `json:"cwd"`
}

func (h *harness) meta(idx int) procMeta {
	h.t.Helper()
	var m procMeta
	b, err := os.ReadFile(filepath.Join(h.dir, fmt.Sprintf("proc-%d.meta.json", idx)))
	if err != nil {
		h.t.Fatalf("proc-%d 메타 없음: %v", idx, err)
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func (h *harness) procs() int {
	n := 0
	for {
		if _, err := os.Stat(filepath.Join(h.dir, fmt.Sprintf("proc-%d.claimed", n))); err != nil {
			return n
		}
		n++
	}
}

func inputTexts(e logEntry) []string {
	var out []string
	if arr, ok := e["texts"].([]any); ok {
		for _, x := range arr {
			out = append(out, fmt.Sprint(x))
		}
	}
	return out
}

func tool(name, args string) fakeBlock {
	return fakeBlock{Type: "tool", Name: name, Args: json.RawMessage(args)}
}

func textBlock(s string) fakeBlock { return fakeBlock{Type: "text", Text: s} }

func toolUseNames(m llm.Message) []string {
	var out []string
	for _, u := range m.ToolUses() {
		out = append(out, u.Name)
	}
	return out
}

func eventTypes(evs []llm.StreamEvent) []llm.StreamEventType {
	var out []llm.StreamEventType
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// ---- 테스트 ----

func TestToolRoundTripAndFollowUp(t *testing.T) {
	h := newHarness(t)
	h.script(0,
		fakeMsg{Blocks: []fakeBlock{{Type: "thinking", Text: "hmm"}, textBlock("let me look"), tool("Bash", `{"cmd":"ls"}`)}, Stop: "tool_use"},
		fakeMsg{Blocks: []fakeBlock{textBlock("all done")}, Stop: "end_turn"},
	)
	h.user("scan target")
	r := h.run()
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stop != "tool_use" {
		t.Fatalf("stop=%q", r.stop)
	}
	if got := toolUseNames(r.msg); len(got) != 1 || got[0] != "Bash" {
		t.Fatalf("tool_use 이름은 ARTEX 원래 이름이어야 한다(접두사 없음): %v", got)
	}
	u := r.msg.ToolUses()[0]
	if string(u.Input) != `{"cmd":"ls"}` {
		t.Fatalf("input=%s", u.Input)
	}
	if u.ID == "" {
		t.Fatal("tool_use id 비어 있음")
	}
	if r.msg.Text() != "let me look" {
		t.Fatalf("text=%q", r.msg.Text())
	}
	var hasThinking bool
	for _, b := range r.msg.Content {
		if b.Type == llm.BlockThinking && b.Thinking == "hmm" && b.Signature == "sig" {
			hasThinking = true
		}
	}
	if !hasThinking {
		t.Fatalf("thinking 블록 누락: %+v", r.msg.Content)
	}
	// 사용량: message_start 는 입력(캐시 포함 합산), message_delta 는 출력.
	if r.usage.InputTokens != 17 || r.usage.CacheReadTokens != 5 || r.usage.CacheWriteTokens != 2 || r.usage.OutputTokens != 7 {
		t.Fatalf("usage=%+v", r.usage)
	}
	if h.state() != stParked {
		t.Fatalf("state=%v, parked 여야 한다", h.state())
	}
	if len(r.events) == 0 || r.events[0].Type != llm.SEMessageStart || r.events[len(r.events)-1].Type != llm.SEMessageStop {
		t.Fatalf("이벤트 순서: %v", eventTypes(r.events))
	}
	var sawToolStart bool
	for _, e := range r.events {
		if e.Type == llm.SEToolUseStart && e.ToolName == "Bash" {
			sawToolStart = true
		}
	}
	if !sawToolStart {
		t.Fatal("SEToolUseStart(Bash) 없음")
	}

	h.answer(func(string, json.RawMessage) string { return "file1\nfile2" })
	r = h.run()
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.stop != "end_turn" || r.msg.Text() != "all done" {
		t.Fatalf("stop=%q text=%q", r.stop, r.msg.Text())
	}
	if h.state() != stIdle {
		t.Fatalf("state=%v, idle 여야 한다", h.state())
	}
	tr := h.logs(0, "tool_result")
	if len(tr) != 1 || tr[0]["text"] != "file1\nfile2" || tr[0]["isError"] != false || tr[0]["name"] != "Bash" {
		t.Fatalf("MCP 로 돌려준 결과: %v", tr)
	}

	// 같은 프로세스에 이어 붙는 후속 사용자 메시지.
	h.user("next question")
	r = h.run()
	if r.err != nil || r.msg.Text() != "done" {
		t.Fatalf("후속 턴: err=%v text=%q", r.err, r.msg.Text())
	}
	if h.procs() != 1 {
		t.Fatalf("프로세스가 %d 개 떴다(1개여야 한다)", h.procs())
	}
	in := h.logs(0, "input")
	if len(in) != 2 || inputTexts(in[0])[0] != "scan target" || strings.Join(inputTexts(in[1]), "|") != "next question" {
		t.Fatalf("stdin 입력: %v", in)
	}
}

func TestFlagsEnvAndSystemPrompt(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-not-leak")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-should-not-leak")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-should-not-leak")
	t.Setenv("ANTHROPIC_BASE_URL", "https://evil.example")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "p")
	t.Setenv("MY_HARMLESS_VAR", "keep")
	t.Setenv("NO_PROXY", "internal.example")
	h := newHarness(t)
	h.p = NewProvider(Options{Binary: h.p.opts.Binary, Model: "model-x", Effort: "xhigh", WorkDir: h.p.opts.WorkDir, Proxy: "http://127.0.0.1:3128"})
	h.script(0, fakeMsg{Blocks: []fakeBlock{textBlock("hi")}, Stop: "end_turn"})
	h.user("go")
	if r := h.run(); r.err != nil {
		t.Fatal(r.err)
	}
	m := h.meta(0)
	args := strings.Join(m.Args, "\x00")
	for _, want := range []string{"-p", "--input-format\x00stream-json", "--output-format\x00stream-json", "--verbose", "--include-partial-messages",
		"--tools\x00\x00--permission-mode", "--permission-mode\x00dontAsk", "--allowedTools\x00mcp__artex__*", "--permission-prompts\x00none",
		"--no-session-persistence", "--strict-mcp-config", "--mcp-config", "--system-prompt-file", "--model\x00model-x", "--effort\x00xhigh",
		"--setting-sources", "--disable-slash-commands"} {
		if !strings.Contains(args, want) {
			t.Errorf("인자에 %q 없음: %q", want, m.Args)
		}
	}
	for _, a := range m.Args {
		if a == "--bare" {
			t.Error("--bare 는 쓰면 안 된다")
		}
	}
	envMap := map[string]string{}
	for _, kv := range m.Env {
		k, v, _ := strings.Cut(kv, "=")
		envMap[k] = v
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "ANTHROPIC_VERTEX_PROJECT_ID"} {
		if _, ok := envMap[k]; ok {
			t.Errorf("자식 환경에 %s 가 남아 있다", k)
		}
	}
	for _, kv := range m.Env {
		if strings.Contains(kv, "should-not-leak") {
			t.Errorf("자격 증명 값이 자식 환경에 남음: %s", kv)
		}
	}
	if envMap["MY_HARMLESS_VAR"] != "keep" {
		t.Error("일반 환경변수는 보존해야 한다")
	}
	for _, k := range []string{"CLAUDE_CODE_DISABLE_CLAUDE_MDS", "CLAUDE_CODE_DISABLE_AUTO_MEMORY", "ENABLE_TOOL_SEARCH", "CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS", "CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT"} {
		if _, ok := envMap[k]; !ok {
			t.Errorf("강화 환경변수 %s 가 없다", k)
		}
	}
	if envMap["ENABLE_TOOL_SEARCH"] != "false" {
		t.Errorf("ENABLE_TOOL_SEARCH=%q", envMap["ENABLE_TOOL_SEARCH"])
	}
	if np := envMap["NO_PROXY"]; !strings.Contains(np, "internal.example") || !strings.Contains(np, "127.0.0.1") {
		t.Errorf("NO_PROXY=%q (기존 값 보존 + loopback 추가)", np)
	}
	if envMap["HTTPS_PROXY"] != "http://127.0.0.1:3128" {
		t.Errorf("프로필 프록시 미반영: %q", envMap["HTTPS_PROXY"])
	}
	if want := "ROLE PROMPT\n\nsecond segment" + toolNote; m.System != want {
		t.Errorf("시스템 프롬프트 파일 내용:\n%q\nwant\n%q", m.System, want)
	}
	if !strings.HasPrefix(m.Cwd, filepath.Join(h.dir, "work")) && !strings.Contains(m.Cwd, "work") {
		t.Errorf("전용 작업 디렉터리가 아니다: %s", m.Cwd)
	}
}

func TestMultiToolParallelAndSerial(t *testing.T) {
	for _, tc := range []struct {
		name     string
		parallel bool
		hasty    bool
	}{{"serial", false, false}, {"parallel", true, false}, {"serial-hasty", false, true}, {"parallel-hasty", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			a, b := tool("Bash", `{"cmd":"a"}`), tool("report_finding", `{"title":"b","sev":1}`)
			a.Hasty = tc.hasty
			h.script(0,
				fakeMsg{Blocks: []fakeBlock{a, b}, Stop: "tool_use", Parallel: tc.parallel},
				fakeMsg{Blocks: []fakeBlock{textBlock("fin")}, Stop: "end_turn"},
			)
			h.user("go")
			r := h.run()
			if r.err != nil {
				t.Fatal(r.err)
			}
			if got := toolUseNames(r.msg); len(got) != 2 || got[0] != "Bash" || got[1] != "report_finding" {
				t.Fatalf("tool_use=%v", got)
			}
			h.answer(func(name string, in json.RawMessage) string { return "res-" + name })
			r = h.run()
			if r.err != nil || r.msg.Text() != "fin" {
				t.Fatalf("err=%v text=%q", r.err, r.msg.Text())
			}
			got := map[string]string{}
			for _, e := range h.logs(0, "tool_result") {
				got[fmt.Sprint(e["name"])] = fmt.Sprint(e["text"])
			}
			if got["Bash"] != "res-Bash" || got["report_finding"] != "res-report_finding" {
				t.Fatalf("결과가 짝에 맞게 돌아가지 않음: %v", got)
			}
		})
	}
}

func TestDifferentArgsPairing(t *testing.T) {
	// 같은 도구를 다른 인자로 병렬 호출 — 도착 순서와 무관하게 인자로 짝을 짓는다.
	h := newHarness(t)
	h.script(0,
		fakeMsg{Blocks: []fakeBlock{tool("Bash", `{"cmd":"one"}`), tool("Bash", `{"cmd":"two"}`), tool("Bash", `{"cmd":"three"}`)}, Stop: "tool_use", Parallel: true},
		fakeMsg{Blocks: []fakeBlock{textBlock("fin")}, Stop: "end_turn"},
	)
	h.user("go")
	if r := h.run(); r.err != nil {
		t.Fatal(r.err)
	}
	h.answer(func(_ string, in json.RawMessage) string { return "out:" + string(in) })
	if r := h.run(); r.err != nil {
		t.Fatal(r.err)
	}
	for _, e := range h.logs(0, "tool_result") {
		if want := "out:" + fmt.Sprint(e["args"]); e["text"] != want {
			t.Fatalf("args=%v 에 엉뚱한 결과 %v", e["args"], e["text"])
		}
	}
	if len(h.logs(0, "tool_result")) != 3 {
		t.Fatal("결과 3개가 필요하다")
	}
}

func TestExtraTextFoldedIntoLastResult(t *testing.T) {
	h := newHarness(t)
	h.script(0,
		fakeMsg{Blocks: []fakeBlock{tool("Bash", `{}`)}, Stop: "tool_use"},
		fakeMsg{Blocks: []fakeBlock{textBlock("ok")}, Stop: "end_turn"},
	)
	h.user("go")
	if r := h.run(); r.err != nil {
		t.Fatal(r.err)
	}
	h.answer(func(string, json.RawMessage) string { return "result" }, "<system-reminder>background job finished</system-reminder>")
	if r := h.run(); r.err != nil {
		t.Fatal(r.err)
	}
	tr := h.logs(0, "tool_result")
	if len(tr) != 1 || !strings.HasPrefix(fmt.Sprint(tr[0]["text"]), "result") || !strings.Contains(fmt.Sprint(tr[0]["text"]), "background job finished") {
		t.Fatalf("추가 텍스트가 결과에 접히지 않음: %v", tr)
	}
	if h.procs() != 1 {
		t.Fatal("접어 넣기는 재동기화 없이 이어져야 한다")
	}
}

func TestToolErrorResultIsError(t *testing.T) {
	h := newHarness(t)
	h.script(0,
		fakeMsg{Blocks: []fakeBlock{tool("Bash", `{}`)}, Stop: "tool_use"},
		fakeMsg{Blocks: []fakeBlock{textBlock("ok")}, Stop: "end_turn"},
	)
	h.user("go")
	h.run()
	last := h.msgs[len(h.msgs)-1]
	h.msgs = append(h.msgs, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(last.ToolUses()[0].ID, "denied by policy", true)}})
	if r := h.run(); r.err != nil {
		t.Fatal(r.err)
	}
	tr := h.logs(0, "tool_result")
	if len(tr) != 1 || tr[0]["isError"] != true || tr[0]["text"] != "denied by policy" {
		t.Fatalf("%v", tr)
	}
}

func TestHiddenToolIsRejectedAndStreamContinues(t *testing.T) {
	// 수습 단계처럼 일부 도구가 숨겨진 요청: 모델이 숨겨진 도구를 부르면 즉시 거절되고, 노출된 도구 호출만 ARTEX 로 넘어간다.
	h := newHarness(t)
	h.tools = testTools()[:2] // Read 는 숨김
	h.script(0,
		fakeMsg{Blocks: []fakeBlock{tool("Read", `{"p":"x"}`)}, Stop: "tool_use"},
		fakeMsg{Blocks: []fakeBlock{textBlock("giving up"), tool("Bash", `{"cmd":"id"}`)}, Stop: "tool_use"},
	)
	h.user("go")
	r := h.run()
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := toolUseNames(r.msg); len(got) != 1 || got[0] != "Bash" {
		t.Fatalf("숨겨진 도구가 ARTEX 로 노출됨: %v", got)
	}
	if r.msg.Text() != "giving up" {
		t.Fatalf("숨김 도구 호출 앞의 메시지는 ARTEX 턴에 섞이면 안 된다: %q", r.msg.Text())
	}
	h.answer(func(string, json.RawMessage) string { return "uid=0" })
	if r := h.run(); r.err != nil {
		t.Fatal(r.err)
	}
	tr := h.logs(0, "tool_result")
	var names []string
	for _, e := range tr {
		names = append(names, fmt.Sprint(e["name"]))
	}
	if len(tr) != 2 || names[0] != "Read" || tr[0]["isError"] != true {
		t.Fatalf("숨김 도구는 오류로 거절되어야 한다: %v", tr)
	}
}

func TestResyncOnCompaction(t *testing.T) {
	h := newHarness(t)
	h.script(0,
		fakeMsg{Blocks: []fakeBlock{textBlock("first")}, Stop: "end_turn"},
	)
	h.script(1,
		fakeMsg{Blocks: []fakeBlock{textBlock("second")}, Stop: "end_turn"},
	)
	h.user("original task")
	if r := h.run(); r.err != nil || r.msg.Text() != "first" {
		t.Fatalf("%v %q", r.err, r.msg.Text())
	}
	oldPid := h.pid(0)
	// AutoCompact: 앞부분이 요약으로 교체된다.
	h.msgs = []llm.Message{llm.UserText("<summary>of everything so far</summary>"), llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ack")}}}
	h.user("continue please")
	r := h.run()
	if r.err != nil || r.msg.Text() != "second" {
		t.Fatalf("%v %q", r.err, r.msg.Text())
	}
	if h.procs() != 2 {
		t.Fatalf("재동기화로 새 프로세스가 떠야 한다: %d", h.procs())
	}
	in := h.logs(1, "input")
	if len(in) != 1 {
		t.Fatalf("%v", in)
	}
	txt := inputTexts(in[0])[0]
	for _, want := range []string{"<restored-conversation>", "of everything so far", "continue please"} {
		if !strings.Contains(txt, want) {
			t.Errorf("전사에 %q 없음:\n%s", want, txt)
		}
	}
	// 옛 프로세스는 정리된다.
	waitFor(t, 8*time.Second, "옛 프로세스 종료", func() bool { return !pidAlive(oldPid) })
}

func TestResyncOnSystemPromptChange(t *testing.T) {
	h := newHarness(t)
	h.script(0, fakeMsg{Blocks: []fakeBlock{textBlock("a")}, Stop: "end_turn"})
	h.script(1, fakeMsg{Blocks: []fakeBlock{textBlock("b")}, Stop: "end_turn"})
	h.user("q1")
	h.run()
	h.sys = []string{"CHANGED ROLE"}
	h.user("q2")
	if r := h.run(); r.err != nil || r.msg.Text() != "b" {
		t.Fatalf("%v %q", r.err, r.msg.Text())
	}
	if h.procs() != 2 {
		t.Fatalf("procs=%d", h.procs())
	}
	if !strings.HasPrefix(h.meta(1).System, "CHANGED ROLE") {
		t.Fatalf("새 프로세스의 시스템 프롬프트: %q", h.meta(1).System)
	}
}

func TestReminderMessageDoesNotForceResync(t *testing.T) {
	// agentcore 는 매 호출 맨 앞에 날짜가 든 <system-reminder> 메시지를 끼운다. 내용이 바뀌어도 재동기화하지 않는다.
	h := newHarness(t)
	h.script(0, fakeMsg{Blocks: []fakeBlock{textBlock("a")}, Stop: "end_turn"}, fakeMsg{Blocks: []fakeBlock{textBlock("b")}, Stop: "end_turn"})
	rem := func(day string) llm.Message {
		return llm.UserText("<system-reminder>today is " + day + "</system-reminder>")
	}
	h.msgs = []llm.Message{rem("monday"), llm.UserText("q1")}
	h.run()
	h.msgs[0] = rem("tuesday")
	h.user("q2")
	if r := h.run(); r.err != nil {
		t.Fatal(r.err)
	}
	if h.procs() != 1 {
		t.Fatalf("리마인더 변화로 재동기화됨: procs=%d", h.procs())
	}
}

func TestUsageLimitFailsFastWithMarker(t *testing.T) {
	h := newHarness(t)
	h.script(0, fakeMsg{ErrCat: "rate_limit", ErrText: "You've hit your usage limit. Your limit will reset at 5pm."})
	h.user("go")
	r := h.run()
	if r.err == nil {
		t.Fatal("오류가 있어야 한다")
	}
	if !strings.Contains(r.err.Error(), UsageLimitMarker) {
		t.Fatalf("마커 누락: %v", r.err)
	}
	if low := strings.ToLower(r.err.Error()); strings.Contains(low, "rate limit") || strings.Contains(low, "too many requests") {
		t.Fatalf("일시적 한도 문구가 섞이면 장애 전환이 막힌다: %v", r.err)
	}
	if h.procs() != 1 {
		t.Fatalf("procs=%d", h.procs())
	}
	// 두 번째 호출은 프로세스를 띄우지 않고 즉시 같은 오류를 낸다(촘촘한 재시도 없음).
	start := time.Now()
	r2 := h.run()
	if r2.err == nil || !strings.Contains(r2.err.Error(), UsageLimitMarker) {
		t.Fatalf("게이트가 막지 않았다: %v", r2.err)
	}
	if h.procs() != 1 {
		t.Fatalf("한도 중에 새 프로세스를 띄웠다: %d", h.procs())
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("즉시 실패해야 한다")
	}
}

func TestTransientRateLimitBacksOff(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var slept []time.Duration
	h.m.gate.sleepFn = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
		return nil
	}
	h.script(0, fakeMsg{ErrCat: "overloaded", ErrText: "Overloaded"})
	h.script(1, fakeMsg{Blocks: []fakeBlock{textBlock("recovered")}, Stop: "end_turn"})
	h.user("go")
	r := h.run()
	if r.err == nil || strings.Contains(r.err.Error(), UsageLimitMarker) {
		t.Fatalf("일시적 과부하는 사용량 한도로 분류되면 안 된다: %v", r.err)
	}
	if low := strings.ToLower(r.err.Error()); !strings.Contains(low, "overloaded") {
		t.Fatalf("err=%v", r.err)
	}
	r = h.run()
	if r.err != nil || r.msg.Text() != "recovered" {
		t.Fatalf("백오프 뒤 재시도: err=%v text=%q", r.err, r.msg.Text())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 1 || slept[0] < 25*time.Second || slept[0] > rateBackoffMin+time.Second {
		t.Fatalf("백오프 대기=%v (최소 %v 여야 한다)", slept, rateBackoffMin)
	}
}

func TestAuthErrorNotRetryableMarker(t *testing.T) {
	h := newHarness(t)
	h.script(0, fakeMsg{ErrCat: "authentication_failed", ErrText: "Not logged in"})
	h.user("go")
	r := h.run()
	if r.err == nil || !strings.Contains(r.err.Error(), "status 401") {
		t.Fatalf("인증 오류는 status 401 표지를 달아야 한다(재시도 제외): %v", r.err)
	}
	if !strings.Contains(r.err.Error(), "claude auth login") {
		t.Fatalf("사용자가 직접 로그인하라는 안내 누락: %v", r.err)
	}
}

func TestCrashThenResync(t *testing.T) {
	h := newHarness(t)
	h.script(0, fakeMsg{Crash: true})
	h.script(1, fakeMsg{Blocks: []fakeBlock{textBlock("alive")}, Stop: "end_turn"})
	h.user("go")
	r := h.run()
	if r.err == nil || !strings.Contains(r.err.Error(), "boom: simulated crash") {
		t.Fatalf("크래시 오류에 stderr 꼬리가 있어야 한다: %v", r.err)
	}
	if strings.Contains(r.err.Error(), UsageLimitMarker) || strings.Contains(r.err.Error(), "status 401") {
		t.Fatalf("크래시는 재시도 가능한 일반 오류여야 한다: %v", r.err)
	}
	r = h.run()
	if r.err != nil || r.msg.Text() != "alive" {
		t.Fatalf("크래시 뒤 재동기화: err=%v text=%q", r.err, r.msg.Text())
	}
	if h.procs() != 2 {
		t.Fatalf("procs=%d", h.procs())
	}
}

func TestCrashWhileParkedResyncs(t *testing.T) {
	h := newHarness(t)
	h.script(0, fakeMsg{Blocks: []fakeBlock{tool("Bash", `{}`)}, Stop: "tool_use"})
	h.script(1, fakeMsg{Blocks: []fakeBlock{textBlock("resumed")}, Stop: "end_turn"})
	h.user("go")
	if r := h.run(); r.err != nil {
		t.Fatal(r.err)
	}
	// 도구 실행 중에 claude 가 죽는다.
	s := h.m.session("sess-1")
	s.mu.Lock()
	p := s.p
	s.mu.Unlock()
	killNow(p.cmd)
	<-p.exited
	h.answer(func(string, json.RawMessage) string { return "late result" })
	r := h.run()
	if r.err != nil || r.msg.Text() != "resumed" {
		t.Fatalf("err=%v text=%q", r.err, r.msg.Text())
	}
	txt := inputTexts(h.logs(1, "input")[0])[0]
	if !strings.Contains(txt, "late result") || !strings.Contains(txt, "tool_result") {
		t.Fatalf("전사에 도구 결과가 있어야 한다:\n%s", txt)
	}
}

func TestCancellationKillsProcessPromptly(t *testing.T) {
	h := newHarness(t)
	h.script(0, fakeMsg{Blocks: []fakeBlock{textBlock("slow")}, Stop: "end_turn", DelayMS: 20000})
	h.user("go")
	ctx, cancel := context.WithCancel(h.ctx)
	var cancelAt atomic.Int64
	go func() {
		for len(h.logs(0, "gen_start")) == 0 { // 응답 생성이 시작된 뒤에 취소한다
			time.Sleep(10 * time.Millisecond)
		}
		cancelAt.Store(time.Now().UnixNano())
		cancel()
	}()
	r := h.runCtx(ctx)
	if r.err == nil || ctx.Err() == nil {
		t.Fatalf("취소 오류가 와야 한다: %v", r.err)
	}
	if d := time.Since(time.Unix(0, cancelAt.Load())); d > 5*time.Second {
		t.Fatalf("취소가 늦다: %v", d)
	}
	var pid int
	if h.procs() == 1 {
		pid = h.pid(0)
	}
	if pid == 0 {
		t.Fatal("pid 를 알 수 없다")
	}
	waitFor(t, 8*time.Second, "취소된 claude 프로세스 종료", func() bool { return !pidAlive(pid) })
	if n := h.m.lim.active(); n != 0 {
		t.Fatalf("슬롯이 반납되지 않음: %d", n)
	}
}

func TestCancelWhileParkedFreesNothingLeaks(t *testing.T) {
	h := newHarness(t)
	h.script(0, fakeMsg{Blocks: []fakeBlock{tool("Bash", `{}`)}, Stop: "tool_use"})
	h.user("go")
	h.run()
	pid := h.pid(0)
	h.m.closeSession("sess-1")
	waitFor(t, 8*time.Second, "정리된 프로세스 종료", func() bool { return !pidAlive(pid) })
}

func (h *harness) pid(idx int) int {
	h.t.Helper()
	var m struct {
		Pid int `json:"pid"`
	}
	b, err := os.ReadFile(filepath.Join(h.dir, fmt.Sprintf("proc-%d.meta.json", idx)))
	if err != nil {
		h.t.Fatal(err)
	}
	_ = json.Unmarshal(b, &m)
	return m.Pid
}

func TestConcurrencyCap(t *testing.T) {
	for _, tc := range []struct {
		max       int
		wantOverl bool
	}{{1, false}, {2, true}} {
		t.Run(fmt.Sprintf("cap%d", tc.max), func(t *testing.T) {
			h := newHarness(t)
			h.m.lim.setMax(tc.max)
			// 세션 두 개, 각각 400ms 걸리는 응답 하나.
			for i := 0; i < 2; i++ {
				h.script(i, fakeMsg{Blocks: []fakeBlock{textBlock("x")}, Stop: "end_turn", DelayMS: 400})
			}
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					ctx := transcript.WithSessionID(context.Background(), fmt.Sprintf("sess-c%d", i))
					req := llm.CompletionRequest{System: h.sys, Messages: []llm.Message{llm.UserText(fmt.Sprintf("q%d", i))}, Tools: h.tools}
					for _, err := range h.p.Stream(ctx, req) {
						if err != nil {
							errs <- err
							return
						}
					}
				}(i)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
			type win struct{ a, b int64 }
			var ws []win
			for i := 0; i < h.procs(); i++ {
				st, sp := h.logs(i, "gen_start"), h.logs(i, "gen_stop")
				if len(st) != 1 || len(sp) != 1 {
					t.Fatalf("proc %d 로그: %v", i, h.readLog(i))
				}
				ws = append(ws, win{int64(st[0]["t"].(float64)), int64(sp[0]["t"].(float64))})
			}
			if len(ws) != 2 {
				t.Fatalf("프로세스 %d 개", len(ws))
			}
			overlap := ws[0].a < ws[1].b && ws[1].a < ws[0].b
			if overlap != tc.wantOverl {
				t.Fatalf("cap=%d overlap=%v want %v (%v)", tc.max, overlap, tc.wantOverl, ws)
			}
		})
	}
}

func TestParkedProcessDoesNotHoldSlot(t *testing.T) {
	// 도구 결과를 기다리는(parked) 세션이 슬롯을 쥐고 있으면 cap=1 에서 다른 에이전트가 영원히 못 돈다.
	h := newHarness(t)
	h.script(0, fakeMsg{Blocks: []fakeBlock{tool("Bash", `{}`)}, Stop: "tool_use"})
	h.script(1, fakeMsg{Blocks: []fakeBlock{textBlock("other")}, Stop: "end_turn"})
	h.user("go")
	h.run()
	if n := h.m.lim.active(); n != 0 {
		t.Fatalf("parked 인데 슬롯 %d 개를 쥐고 있다", n)
	}
	ctx := transcript.WithSessionID(context.Background(), "sess-other")
	done := make(chan error, 1)
	go func() {
		for _, err := range h.p.Stream(ctx, llm.CompletionRequest{System: h.sys, Messages: []llm.Message{llm.UserText("hi")}, Tools: h.tools}) {
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("다른 세션이 parked 세션 때문에 막혔다")
	}
}

func TestOneShotWithoutTools(t *testing.T) {
	h := newHarness(t)
	h.script(0, fakeMsg{Blocks: []fakeBlock{textBlock("summary text")}, Stop: "end_turn"})
	req := llm.CompletionRequest{System: []string{"summarize"}, Messages: []llm.Message{llm.UserText("long conversation")}}
	msg, stop, usage, err := h.p.Complete(context.Background(), req)
	if err != nil || msg.Text() != "summary text" || stop != "end_turn" || usage.OutputTokens != 7 {
		t.Fatalf("err=%v msg=%q stop=%q usage=%+v", err, msg.Text(), stop, usage)
	}
	m := h.meta(0)
	for _, a := range m.Args {
		if a == "--mcp-config" {
			t.Fatal("도구 없는 요청에 MCP 가 붙으면 안 된다")
		}
	}
	if strings.Contains(m.System, "mcp__artex__") {
		t.Fatalf("도구 없는 요청의 시스템 프롬프트에 도구 이름 규약이 들어감: %q", m.System)
	}
	pid := h.pid(0)
	waitFor(t, 8*time.Second, "일회용 프로세스 종료", func() bool { return !pidAlive(pid) })
}

func TestDisabledRefuses(t *testing.T) {
	h := newHarness(t)
	h.m.enabled.Store(false)
	h.user("go")
	r := h.run()
	if r.err == nil {
		t.Fatal("꺼져 있으면 거절해야 한다")
	}
	if h.procs() != 0 {
		t.Fatal("꺼져 있는데 프로세스를 띄웠다")
	}
}

func TestPreflightProblems(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"FAKE_AUTH": "none"}, "claude auth login"},
		{map[string]string{"FAKE_AUTH": "api_key"}, "API"},
		{map[string]string{"FAKE_AUTH": "api_key_helper"}, "API"},
		{map[string]string{"FAKE_AUTH": "third_party"}, "API"},
		{map[string]string{"FAKE_VERSION": "2.0.1 (Claude Code)"}, "2.1.259"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.env), func(t *testing.T) {
			h := newHarness(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			h.user("go")
			r := h.run()
			if r.err == nil || !strings.Contains(r.err.Error(), tc.want) {
				t.Fatalf("err=%v want contains %q", r.err, tc.want)
			}
			if h.procs() != 0 {
				t.Fatal("사전 점검 실패인데 세션 프로세스를 띄웠다")
			}
			if h.m.lim.active() != 0 {
				t.Fatal("슬롯 누수")
			}
		})
	}
	t.Run("oauth_token", func(t *testing.T) {
		h := newHarness(t)
		t.Setenv("FAKE_AUTH", "oauth_token")
		h.script(0, fakeMsg{Blocks: []fakeBlock{textBlock("ok")}, Stop: "end_turn"})
		h.user("go")
		if r := h.run(); r.err != nil {
			t.Fatalf("oauth_token 방식은 허용되어야 한다: %v", r.err)
		}
	})
}

func TestMissingBinary(t *testing.T) {
	h := newHarness(t)
	h.p = NewProvider(Options{Binary: filepath.Join(h.dir, "no-such-claude")})
	h.user("go")
	r := h.run()
	if r.err == nil || !strings.Contains(r.err.Error(), "claude") {
		t.Fatalf("err=%v", r.err)
	}
}

func TestStreamConsumerStopsEarly(t *testing.T) {
	h := newHarness(t)
	h.script(0, fakeMsg{Blocks: []fakeBlock{textBlock("some long text here")}, Stop: "end_turn"})
	h.user("go")
	req := llm.CompletionRequest{System: h.sys, Messages: h.msgs, Tools: h.tools}
	n := 0
	for _, err := range h.p.Stream(h.ctx, req) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		if n == 2 {
			break
		}
	}
	// 소비자가 멈춰도 슬롯은 반납되고 세션은 다음 호출에서 재동기화로 복구된다.
	if a := h.m.lim.active(); a != 0 {
		t.Fatalf("슬롯 누수: %d", a)
	}
	h.script(1, fakeMsg{Blocks: []fakeBlock{textBlock("again")}, Stop: "end_turn"})
	if r := h.run(); r.err != nil || r.msg.Text() != "again" {
		t.Fatalf("err=%v text=%q", r.err, r.msg.Text())
	}
}

func TestCloseSessionAndReap(t *testing.T) {
	h := newHarness(t)
	h.script(0, fakeMsg{Blocks: []fakeBlock{textBlock("a")}, Stop: "end_turn"})
	h.user("go")
	h.run()
	pid := h.pid(0)
	h.m.reapOnce(time.Now()) // 아직 유휴 만료 전
	if !pidAlive(pid) {
		t.Fatal("만료 전에 정리됨")
	}
	h.m.reapOnce(time.Now().Add(h.m.idleTTL + time.Minute))
	waitFor(t, 8*time.Second, "유휴 만료 정리", func() bool { return !pidAlive(pid) })
}

func TestEvictOldestWhenOverMaxLive(t *testing.T) {
	h := newHarness(t)
	h.m.maxLive = 2
	for i := 0; i < 3; i++ {
		h.script(i, fakeMsg{Blocks: []fakeBlock{textBlock("a")}, Stop: "end_turn"})
	}
	var pids []int
	for i := 0; i < 3; i++ {
		ctx := transcript.WithSessionID(context.Background(), fmt.Sprintf("s%d", i))
		for _, err := range h.p.Stream(ctx, llm.CompletionRequest{System: h.sys, Messages: []llm.Message{llm.UserText("q")}, Tools: h.tools}) {
			if err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		pids = append(pids, h.pid(i))
	}
	waitFor(t, 8*time.Second, "가장 오래 놀던 세션 정리", func() bool { return !pidAlive(pids[0]) })
	if !pidAlive(pids[1]) || !pidAlive(pids[2]) {
		t.Fatal("최근 세션은 살아 있어야 한다")
	}
}

func TestSessionKeyFallback(t *testing.T) {
	a := sessionKey(context.Background(), "sys", []llm.Message{llm.UserText("hello")})
	b := sessionKey(context.Background(), "sys", []llm.Message{llm.UserText("hello"), llm.UserText("more")})
	c := sessionKey(context.Background(), "sys", []llm.Message{llm.UserText("other")})
	if a != b || a == c || !strings.HasPrefix(a, "anon-") {
		t.Fatalf("%q %q %q", a, b, c)
	}
	if got := sessionKey(transcript.WithSessionID(context.Background(), "exp1-worker-i3"), "sys", nil); got != "exp1-worker-i3" {
		t.Fatalf("%q", got)
	}
}

// ---- 도우미 ----

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("시간 초과: %s", what)
}

func TestOutputNotLostWhenProcessExitsRightAfterResult(t *testing.T) {
	// Wait 가 stdout 읽기를 끊어 마지막 줄들이 사라지던 경합의 회귀 테스트.
	for i := 0; i < 20; i++ {
		h := newHarness(t)
		h.script(0, fakeMsg{Blocks: []fakeBlock{textBlock("complete answer")}, Stop: "end_turn", ExitAfter: true})
		h.script(1, fakeMsg{Blocks: []fakeBlock{textBlock("second")}, Stop: "end_turn"})
		h.user("go")
		r := h.run()
		if r.err != nil || r.msg.Text() != "complete answer" || r.stop != "end_turn" {
			t.Fatalf("iter %d: err=%v text=%q stop=%q", i, r.err, r.msg.Text(), r.stop)
		}
		// 죽은 프로세스는 다음 호출에서 재동기화로 이어진다.
		h.user("again")
		if r := h.run(); r.err != nil || r.msg.Text() != "second" {
			t.Fatalf("iter %d 후속: err=%v text=%q", i, r.err, r.msg.Text())
		}
	}
}
