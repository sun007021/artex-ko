package claudecode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestProbeVariants(t *testing.T) {
	h := newHarness(t)
	bin := h.p.opts.Binary
	cases := []struct {
		name    string
		env     map[string]string
		usable  bool
		problem string
		method  string
	}{
		{"claude.ai", nil, true, "", AuthClaudeAI},
		{"oauth_token", map[string]string{"FAKE_AUTH": "oauth_token"}, true, "", AuthOAuthToken},
		{"none", map[string]string{"FAKE_AUTH": "none"}, false, ProblemNotLoggedIn, AuthNone},
		{"api_key", map[string]string{"FAKE_AUTH": "api_key"}, false, ProblemAPIBilling, AuthAPIKey},
		{"api_key_helper", map[string]string{"FAKE_AUTH": "api_key_helper"}, false, ProblemAPIBilling, AuthAPIKeyHelper},
		{"third_party", map[string]string{"FAKE_AUTH": "third_party"}, false, ProblemAPIBilling, AuthThirdParty},
		{"too_old", map[string]string{"FAKE_VERSION": "2.1.100 (Claude Code)"}, false, ProblemTooOld, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			st := Probe(context.Background(), bin)
			if st.Usable != tc.usable || st.Problem != tc.problem || (tc.method != "" && st.Method != tc.method) {
				t.Fatalf("%+v", st)
			}
			if !tc.usable && st.Detail == "" {
				t.Fatal("사용자에게 보여 줄 설명이 없다")
			}
		})
	}
	t.Run("garbage", func(t *testing.T) {
		t.Setenv("FAKE_AUTH", "garbage")
		st := Probe(context.Background(), bin)
		// JSON 을 못 읽어도 종료 코드 0 이면 로그인으로 보지만, 방식을 모르므로 구독으로 인정하지 않는다.
		if st.Usable || st.Problem != ProblemUnknown {
			t.Fatalf("authMethod 를 모르는데 사용 가능으로 판정: %+v", st)
		}
	})
	t.Run("missing", func(t *testing.T) {
		st := Probe(context.Background(), "/definitely/not/here/claude")
		if st.Installed || st.Problem != ProblemNotInstalled {
			t.Fatalf("%+v", st)
		}
	})
}

func TestProbeEnvIsSanitized(t *testing.T) {
	// auth status 도 정제 환경에서 실행된다 — ARTEX 환경의 API 키가 "인증 방식"을 api_key 로 바꾸지 못한다.
	h := newHarness(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-x")
	st := Probe(context.Background(), h.p.opts.Binary)
	if !st.Usable {
		t.Fatalf("%+v", st)
	}
}

func TestPreflightCaching(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.m.probeFn = func(ctx context.Context, bin string) Status {
		calls++
		return Status{Usable: false, Problem: ProblemNotLoggedIn, Detail: "no"}
	}
	for i := 0; i < 3; i++ {
		if e := h.m.preflight(context.Background(), "x"); e == nil || e.Kind != KindAuth {
			t.Fatalf("%v", e)
		}
	}
	if calls != 1 {
		t.Fatalf("실패 결과를 짧게 캐시해야 한다: %d", calls)
	}
	reset := func(m *manager) { m.mu.Lock(); m.probes = map[string]probeEntry{}; m.mu.Unlock() }
	reset(h.m)
	h.m.probeFn = func(ctx context.Context, bin string) Status { calls++; return Status{Usable: true} }
	if e := h.m.preflight(context.Background(), "x"); e != nil {
		t.Fatal(e)
	}
}

func TestMCPHubSecurity(t *testing.T) {
	h := newHarness(t)
	hub, err := h.m.getHub()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hub.addr, "127.0.0.1:") {
		t.Fatalf("루프백 이외에 바인드: %s", hub.addr)
	}
	s := newSession(h.m, "x")
	s.setTools(testTools())
	p := &proc{s: s, closed: make(chan struct{}), changed: make(chan struct{})}
	tok := hub.register(p)
	if len(tok) != 64 {
		t.Fatalf("토큰 길이 %d (256비트 hex 여야 한다)", len(tok))
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	do := func(method, path, host, origin string) (int, string) {
		req, _ := http.NewRequest(method, "http://"+hub.addr+path, strings.NewReader(body))
		if host != "" {
			req.Host = host
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, out := do("POST", "/mcp/"+tok, "", ""); code != 200 || !strings.Contains(out, `"Bash"`) || !strings.Contains(out, "inputSchema") {
		t.Fatalf("정상 요청: %d %s", code, out)
	}
	if code, _ := do("POST", "/mcp/"+tok, "evil.example.com", ""); code != http.StatusForbidden {
		t.Fatalf("비루프백 Host: %d", code)
	}
	if code, _ := do("POST", "/mcp/"+tok, "", "http://evil.example.com"); code != http.StatusForbidden {
		t.Fatalf("Origin 헤더: %d", code)
	}
	if code, _ := do("POST", "/mcp/"+strings.Repeat("0", 64), "", ""); code != http.StatusNotFound {
		t.Fatalf("잘못된 토큰: %d", code)
	}
	if code, _ := do("POST", "/mcp/", "", ""); code != http.StatusNotFound {
		t.Fatalf("토큰 없음: %d", code)
	}
	if code, _ := do("POST", "/other", "", ""); code != http.StatusNotFound {
		t.Fatalf("다른 경로: %d", code)
	}
	if code, _ := do("GET", "/mcp/"+tok, "", ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", code)
	}
	if code, _ := do("DELETE", "/mcp/"+tok, "", ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE: %d", code)
	}
	hub.unregister(tok)
	if code, _ := do("POST", "/mcp/"+tok, "", ""); code != http.StatusNotFound {
		t.Fatalf("해제된 토큰: %d", code)
	}
}

func TestMCPDispatchMethods(t *testing.T) {
	h := newHarness(t)
	hub, _ := h.m.getHub()
	s := newSession(h.m, "x")
	s.setTools(testTools())
	p := &proc{s: s, closed: make(chan struct{}), changed: make(chan struct{})}
	for _, c := range []struct{ method, want string }{
		{"initialize", "protocolVersion"}, {"ping", "result"}, {"resources/list", "resources"},
		{"prompts/list", "prompts"}, {"tools/list", "tools"}, {"nope/nope", "-32601"},
	} {
		resp, has := hub.dispatch(context.Background(), p, rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: c.method})
		raw, _ := json.Marshal(resp)
		if !has || !strings.Contains(string(raw), c.want) {
			t.Errorf("%s → %s", c.method, raw)
		}
	}
	if _, has := hub.dispatch(context.Background(), p, rpcRequest{Method: "notifications/initialized"}); has {
		t.Error("알림에는 응답하지 않는다")
	}
	// 알 수 없는 도구 호출은 오류 결과.
	r := p.callTool(context.Background(), "ghost", json.RawMessage(`{}`))
	if !r.IsError {
		t.Error("알 수 없는 도구는 오류여야 한다")
	}
}

func TestSafeToolName(t *testing.T) {
	taken := map[string]toolInfo{}
	a := safeToolName("mcp.server/tool name", taken)
	if a != "mcp_server_tool_name" {
		t.Fatal(a)
	}
	taken[a] = toolInfo{}
	if b := safeToolName("mcp.server/tool name", taken); b == a || !strings.HasPrefix(b, "mcp_server_tool_name") {
		t.Fatal(b)
	}
	long := strings.Repeat("x", 80)
	if n := safeToolName(long, map[string]toolInfo{}); len(n) != 52 {
		t.Fatalf("%d", len(n))
	}
	if safeToolName("", map[string]toolInfo{}) == "" {
		t.Fatal("빈 이름")
	}
}

func TestToolNameAliasRoundTrip(t *testing.T) {
	// ARTEX 이름에 허용 밖 문자가 있어도 ARTEX 로는 원래 이름이 돌아간다.
	h := newHarness(t)
	h.tools = []llm.ToolSchema{{Name: "mcp:weird.tool", Description: "d", InputSchema: map[string]any{"type": "object"}}}
	h.script(0, fakeMsg{Blocks: []fakeBlock{tool("mcp_weird_tool", `{}`)}, Stop: "tool_use"})
	h.user("go")
	r := h.run()
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := toolUseNames(r.msg); len(got) != 1 || got[0] != "mcp:weird.tool" {
		t.Fatalf("%v", got)
	}
}

func TestCanonArgs(t *testing.T) {
	if canonArgs(json.RawMessage(`{"b":1, "a":[1,2]}`)) != canonArgs(json.RawMessage(`{"a":[1,2],"b":1}`)) {
		t.Fatal("키 순서/공백에 무관해야 한다")
	}
	if canonArgs(nil) != "{}" || canonArgs(json.RawMessage("null")) != "{}" {
		t.Fatal("빈 인자는 {}")
	}
}

func TestRenderTranscriptAndSignatures(t *testing.T) {
	asst := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "secret", Signature: "s"},
		llm.TextBlock("calling"),
		{Type: llm.BlockToolUse, ID: "t1", Name: "Bash", Input: json.RawMessage(`{"cmd":"id"}`)},
	}}
	res := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("t1", "uid=0", false)}}
	out := renderTranscript(nil, []llm.Message{llm.UserText("task"), asst, res})
	for _, w := range []string{"<restored-conversation>", "[user]\ntask", "tool_use id=t1 name=Bash", `{"cmd":"id"}`, "tool_result id=t1 status=ok", "uid=0"} {
		if !strings.Contains(out, w) {
			t.Errorf("%q 없음:\n%s", w, out)
		}
	}
	if strings.Contains(out, "secret") {
		t.Error("thinking 은 전사에 넣지 않는다")
	}
	if renderTranscript(nil, []llm.Message{llm.UserText("just this")}) != "just this" {
		t.Error("단일 사용자 텍스트는 그대로")
	}
	// 서명: 도구 결과 본문이 달라도(MicroCompact) 같고, 텍스트가 달라지면 다르다.
	res2 := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("t1", "[cleared]", false)}}
	if msgSig(res) != msgSig(res2) {
		t.Error("결과 본문은 서명에서 제외")
	}
	if msgSig(llm.UserText("a")) == msgSig(llm.UserText("b")) {
		t.Error("텍스트 변화는 서명에 반영")
	}
	asstNoThinking := asst
	asstNoThinking.Content = asst.Content[1:]
	if msgSig(asst) != msgSig(asstNoThinking) {
		t.Error("thinking 은 서명에서 제외")
	}
	rem, rest := splitReminder([]llm.Message{llm.UserText("<system-reminder>x</system-reminder>"), llm.UserText("q")})
	if rem == nil || len(rest) != 1 || rest[0].Text() != "q" {
		t.Error("splitReminder")
	}
	if rem, rest := splitReminder([]llm.Message{llm.UserText("q")}); rem != nil || len(rest) != 1 {
		t.Error("리마인더가 없으면 그대로")
	}
}

func TestUserInputLine(t *testing.T) {
	b := userInputLine("a", "  ", "b")
	if !strings.HasSuffix(string(b), "\n") {
		t.Fatal("개행 필요")
	}
	var v struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct{ Type, Text string }
		} `json:"message"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.Type != "user" || v.Message.Role != "user" || len(v.Message.Content) != 2 {
		t.Fatalf("%v %s", err, b)
	}
}
