package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/artex/siwc"
	"github.com/Autumn-27/norma/llm"
)

// fakeSource is a TokenSource whose behaviour the test controls.
type fakeSource struct {
	mu       sync.Mutex
	tok      string
	next     string // returned for a "rejected" call
	err      error
	calls    []string // rejected arguments, in order
	refreshN int
}

func (f *fakeSource) Token(_ context.Context, rejected string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rejected)
	if f.err != nil {
		return "", f.err
	}
	if rejected != "" && rejected == f.tok {
		f.refreshN++
		f.tok = f.next
	}
	return f.tok, nil
}

// responsesServer is a stand-in for api.openai.com/v1/responses.
type responsesServer struct {
	*httptest.Server
	mu     sync.Mutex
	auths  []string
	bodies []map[string]any
	hits   atomic.Int32
}

func newResponsesServer(t *testing.T, handle func(n int, w http.ResponseWriter, r *http.Request)) *responsesServer {
	t.Helper()
	rs := &responsesServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(rs.hits.Add(1))
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		rs.mu.Lock()
		rs.auths = append(rs.auths, r.Header.Get("Authorization"))
		rs.bodies = append(rs.bodies, m)
		rs.mu.Unlock()
		handle(n, w, r)
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *responsesServer) provider(t *testing.T, src TokenSource, retries int) llm.Provider {
	t.Helper()
	inner := &chatgptTransport{base: http.DefaultTransport, src: src, endpoint: rs.URL + "/v1/responses"}
	p, err := llm.NewProvider(llm.Config{
		Format: llm.FormatOpenAIResponses, BaseURL: rs.URL + "/v1", APIKey: chatGPTPlaceholderKey, Model: "gpt-test",
		ReasoningEffort: "high", MaxRetries: retries,
		HTTPClient: &http.Client{Transport: quotaAwareTransport{base: inner}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		name := ""
		var f struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(e), &f)
		name = f.Type
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, e)
	}
}

const (
	evTextDelta = `{"type":"response.output_text.delta","delta":"hello"}`
	evToolAdded = `{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"run_cmd"}}`
	evToolArgs  = `{"type":"response.function_call_arguments.delta","delta":"{\"cmd\":\"id\"}"}`
	evCompleted = `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":3}},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","call_id":"call_1","name":"run_cmd","arguments":"{\"cmd\":\"id\"}"}]}}`
)

func collect(t *testing.T, p llm.Provider, req llm.CompletionRequest) (text string, tools []string, stop string, usage llm.Usage, err error) {
	t.Helper()
	for ev, e := range p.Stream(context.Background(), req) {
		if e != nil {
			return text, tools, stop, usage, e
		}
		switch ev.Type {
		case llm.SETextDelta:
			text += ev.Text
		case llm.SEToolUseStart:
			tools = append(tools, ev.ToolName)
		case llm.SEMessageDelta:
			stop, usage = ev.StopReason, ev.Usage
		}
	}
	return
}

func baseReq() llm.CompletionRequest {
	temp := 0.2
	return llm.CompletionRequest{
		System:      []string{"system prompt"},
		Messages:    []llm.Message{llm.UserText("hi")},
		MaxTokens:   8192,
		Temperature: &temp,
		Tools:       []llm.ToolSchema{{Name: "run_cmd", Description: "d", InputSchema: map[string]any{"type": "object"}}},
	}
}

func TestSanitizeChatGPTRequest(t *testing.T) {
	in := `{"model":"m","instructions":"s","input":[{"role":"system","content":"x"},{"role":"user","content":"u"},{"type":"function_call","call_id":"c","name":"n","arguments":"{}"}],
"tools":[{"type":"function","name":"n"}],"max_output_tokens":4096,"temperature":0.3,"top_p":1,"metadata":{"a":"b"},"user":"u","truncation":"auto",
"previous_response_id":"r","reasoning":{"effort":"high"},"stream":false,"store":true}`
	out, want, err := sanitizeChatGPTRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if want {
		t.Fatal("original stream:false must be reported so the response is aggregated")
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(out, &m)
	for _, k := range chatGPTUnsupportedFields {
		if _, ok := m[k]; ok {
			t.Errorf("%s still present", k)
		}
	}
	if string(m["store"]) != "false" || string(m["stream"]) != "true" {
		t.Fatalf("store=%s stream=%s", m["store"], m["stream"])
	}
	for _, k := range []string{"model", "instructions", "input", "tools", "reasoning"} {
		if _, ok := m[k]; !ok {
			t.Errorf("%s dropped", k)
		}
	}
	if strings.Contains(string(m["input"]), `"system"`) || !strings.Contains(string(m["input"]), `"developer"`) {
		t.Fatalf("system role must become developer: %s", m["input"])
	}
	if _, _, err := sanitizeChatGPTRequest([]byte(`[1]`)); err == nil {
		t.Fatal("non-object body must be rejected")
	}
}

func TestChatGPTStreamEndToEnd(t *testing.T) {
	t.Parallel()
	rs := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		sse(w, `{"type":"response.created","response":{}}`, evToolAdded, evToolArgs, evTextDelta, evCompleted)
	})
	src := &fakeSource{tok: "tok-1"}
	text, tools, stop, usage, err := collect(t, rs.provider(t, src, 0), baseReq())
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello" || len(tools) != 1 || tools[0] != "run_cmd" || stop != "end_turn" || usage.InputTokens != 11 || usage.OutputTokens != 7 {
		t.Fatalf("text=%q tools=%v stop=%q usage=%+v", text, tools, stop, usage)
	}
	body := rs.bodies[0]
	if rs.auths[0] != "Bearer tok-1" {
		t.Fatalf("auth %q", rs.auths[0])
	}
	if body["store"] != false || body["stream"] != true || body["model"] != "gpt-test" || body["instructions"] != "system prompt" {
		t.Fatalf("body: %v", body)
	}
	for _, k := range []string{"max_output_tokens", "temperature"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s must be stripped", k)
		}
	}
	if r, _ := body["reasoning"].(map[string]any); r["effort"] != "high" {
		t.Fatalf("reasoning effort must still be sent: %v", body["reasoning"])
	}
	if tl, _ := body["tools"].([]any); len(tl) != 1 {
		t.Fatalf("function tools must be sent unchanged: %v", body["tools"])
	}
}

func TestChatGPTRefreshesOnceOn401(t *testing.T) {
	t.Parallel()
	rs := newResponsesServer(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"detail":"expired"}`))
			return
		}
		sse(w, evTextDelta, evCompleted)
	})
	src := &fakeSource{tok: "old", next: "new"}
	text, _, _, _, err := collect(t, rs.provider(t, src, -1), baseReq())
	if err != nil || text != "hello" {
		t.Fatalf("%q %v", text, err)
	}
	if got := rs.auths; len(got) != 2 || got[0] != "Bearer old" || got[1] != "Bearer new" {
		t.Fatalf("auth sequence %v", got)
	}
	if src.calls[len(src.calls)-1] != "old" || src.refreshN != 1 {
		t.Fatalf("source calls %v", src.calls)
	}
}

func TestChatGPTSecond401IsNotRetriedForever(t *testing.T) {
	t.Parallel()
	rs := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"subscription_sharing_invalid_user"}}`))
	})
	src := &fakeSource{tok: "a", next: "b"}
	_, _, _, _, err := collect(t, rs.provider(t, src, 0), baseReq())
	if err == nil || !strings.Contains(err.Error(), "status 401") || !strings.Contains(err.Error(), "subscription_sharing_invalid_user") {
		t.Fatalf("got %v", err)
	}
	if rs.hits.Load() != 2 {
		t.Fatalf("expected exactly one retry, got %d requests", rs.hits.Load())
	}
}

func TestChatGPTStreamWithoutCompletedIsAnError(t *testing.T) {
	t.Parallel()
	rs := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		sse(w, evTextDelta) // cut off before response.completed
	})
	_, _, _, _, err := collect(t, rs.provider(t, &fakeSource{tok: "t"}, -1), baseReq())
	if err == nil {
		t.Fatal("a stream that ends without response.completed must fail")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) && !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("got %v", err)
	}
}

func TestChatGPTFailedAndIncompleteEvents(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		event string
		want  string // substring of the error
		quota bool
	}{
		{"usage limit", `{"type":"response.failed","response":{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"A sharing limit was reached"}}}`, "subscription_sharing_usage_limit_exceeded", true},
		{"usage unavailable", `{"type":"response.failed","response":{"error":{"code":"subscription_sharing_usage_unavailable","message":"try later"}}}`, "subscription_sharing_usage_unavailable", false},
		{"failed without error", `{"type":"response.failed","response":{}}`, "unknown_error", false},
		{"error event", `{"type":"error","code":"subscription_sharing_usage_limit_exceeded","message":"limit"}`, "subscription_sharing_usage_limit_exceeded", true},
		{"nested error event", `{"type":"error","error":{"code":"server_error","message":"boom"}}`, "server_error: boom", false},
		{"incomplete other", `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"content_filter"}}}`, "response_incomplete: content_filter", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rs := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) { sse(w, evTextDelta, tc.event) })
			_, _, _, _, err := collect(t, rs.provider(t, &fakeSource{tok: "t"}, -1), baseReq())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if IsQuotaExhaustedMessage(err.Error()) != tc.quota {
				t.Fatalf("quota classification of %q = %v", err, !tc.quota)
			}
		})
	}
}

func TestChatGPTIncompleteMaxOutputIsMaxTokensStop(t *testing.T) {
	t.Parallel()
	rs := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		sse(w, evTextDelta, `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1,"output_tokens":2}}}`)
	})
	text, _, stop, _, err := collect(t, rs.provider(t, &fakeSource{tok: "t"}, -1), baseReq())
	if err != nil || text != "hello" || stop != "max_tokens" {
		t.Fatalf("%q %q %v", text, stop, err)
	}
}

func TestChatGPTUsageLimit429FailsOverWithoutRetry(t *testing.T) {
	t.Parallel()
	rs := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"limit"}}`))
	})
	start := time.Now()
	_, _, _, _, err := collect(t, rs.provider(t, &fakeSource{tok: "t"}, 3), baseReq())
	if err == nil || !IsQuotaExhaustedMessage(err.Error()) {
		t.Fatalf("usage-limit 429 must classify as quota exhaustion: %v", err)
	}
	if rs.hits.Load() != 1 || time.Since(start) > 2*time.Second {
		t.Fatalf("429 usage limit must not be retried (hits=%d, %v)", rs.hits.Load(), time.Since(start))
	}
	if !strings.Contains(err.Error(), "status 402") {
		t.Fatalf("should be normalized to 402 like other quota errors: %v", err)
	}
}

func TestChatGPTTokenFailuresDoNotHitTheNetworkOrRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{siwc.ErrReauthRequired, "status 401: " + `{"error":{"code":"chatgpt_reauth_required"`},
		{siwc.ErrNotConnected, "chatgpt_not_connected"},
		{siwc.ErrPlanUsageDisabled, "chatgpt_plan_usage_disabled"},
	} {
		rs := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) { t.Error("server must not be called") })
		start := time.Now()
		_, _, _, _, err := collect(t, rs.provider(t, &fakeSource{err: tc.err}, 3), baseReq())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("got %v want %q", err, tc.want)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("terminal token errors must not go through norma's retry backoff (%v)", time.Since(start))
		}
		if IsQuotaExhaustedMessage(err.Error()) {
			t.Fatal("a login problem is not quota exhaustion")
		}
	}
	// a transient identity outage is a 503 (retryable by the caller), not a login error
	rs := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) { t.Error("server must not be called") })
	_, _, _, _, err := collect(t, rs.provider(t, &fakeSource{err: siwc.ErrIdentityUnavailable}, -1), baseReq())
	if err == nil || !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("got %v", err)
	}
}

func TestChatGPTCompleteAggregatesTheStream(t *testing.T) {
	t.Parallel()
	rs := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		sse(w, evToolAdded, evToolArgs, evTextDelta, evCompleted)
	})
	msg, stop, usage, err := rs.provider(t, &fakeSource{tok: "t"}, 0).Complete(context.Background(), baseReq())
	if err != nil {
		t.Fatal(err)
	}
	if rs.bodies[0]["stream"] != true || rs.bodies[0]["store"] != false {
		t.Fatalf("non-streaming callers must still be sent stream:true store:false: %v", rs.bodies[0])
	}
	var text, tool string
	for _, b := range msg.Content {
		switch b.Type {
		case llm.BlockText:
			text += b.Text
		case llm.BlockToolUse:
			tool = b.Name
		}
	}
	if text != "hello" || tool != "run_cmd" || stop != "tool_use" || usage.OutputTokens != 7 {
		t.Fatalf("text=%q tool=%q stop=%q usage=%+v", text, tool, stop, usage)
	}

	// failures surface with their code
	rs2 := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		sse(w, `{"type":"response.failed","response":{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"x"}}}`)
	})
	_, _, _, err = rs2.provider(t, &fakeSource{tok: "t"}, 0).Complete(context.Background(), baseReq())
	if err == nil || !IsQuotaExhaustedMessage(err.Error()) {
		t.Fatalf("got %v", err)
	}
	rs3 := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) { sse(w, evTextDelta) })
	if _, _, _, err = rs3.provider(t, &fakeSource{tok: "t"}, 0).Complete(context.Background(), baseReq()); err == nil {
		t.Fatal("an aggregated stream without response.completed must fail")
	}
}

func TestChatGPTTransportRefusesOtherDestinations(t *testing.T) {
	t.Parallel()
	src := &fakeSource{tok: "secret-token-value"}
	var reached atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Store(true) }))
	defer other.Close()
	tr := &chatgptTransport{base: http.DefaultTransport, src: src, endpoint: chatGPTResponsesEndpoint}
	for _, target := range []string{other.URL + "/v1/responses", "https://api.openai.com/v1/chat/completions", "https://evil.example/v1/responses", "http://api.openai.com/v1/responses"} {
		req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(`{}`))
		if _, err := tr.RoundTrip(req); err == nil || strings.Contains(err.Error(), "secret-token-value") {
			t.Fatalf("%s: err=%v", target, err)
		}
	}
	if reached.Load() || len(src.calls) != 0 {
		t.Fatalf("credentials were requested/sent to a foreign host (calls=%v)", src.calls)
	}
	// the production client never follows redirects
	c, err := chatGPTHTTPClient("", src)
	if err != nil {
		t.Fatal(err)
	}
	if c.CheckRedirect == nil || c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirects must not be followed")
	}
}

func TestChatGPTCaptureNeverContainsTheToken(t *testing.T) {
	t.Parallel()
	const secret = "eyJsecret-access-token-value"
	rs := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		// a hostile/buggy upstream echoing the credential back
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"error":{"code":"bad","message":"auth was %s"}}`, r.Header.Get("Authorization"))
	})
	ctx, capt := llmrec.NewCapture(context.Background())
	p := rs.provider(t, &fakeSource{tok: secret}, -1)
	for _, e := range p.Stream(ctx, baseReq()) {
		_ = e
	}
	all := capt.RawRequest() + "\n" + capt.RawResponse()
	for _, a := range capt.Attempts() {
		all += "\n" + a.Body
	}
	if strings.Contains(all, secret) {
		t.Fatalf("capture leaked the access token:\n%s", all)
	}
	if !strings.Contains(all, "[REDACTED]") {
		t.Fatalf("expected the echoed token to be masked:\n%s", all)
	}
	// unrelated look-alike data (a pentest target's own bearer/JWT) is left alone
	other := &llmrec.Capture{}
	other.AddSecret(secret)
	other.SetRequest(`{"tool_result":"Authorization: Bearer eyJtargetapp.token.value"}`)
	if !strings.Contains(other.RawRequest(), "eyJtargetapp.token.value") {
		t.Fatal("masking must be by exact value only")
	}
}

func TestChatGPTNewProviderConfig(t *testing.T) {
	src := &fakeSource{tok: "t"}
	cfg := NewChatGPTConfig("gpt-x", "", src)
	if cfg.Format != llm.FormatOpenAIResponses || !cfg.Stream || !cfg.UsesChatGPT() || cfg.BaseURL != ChatGPTBaseURL || cfg.Provider() != "openai-responses" {
		t.Fatalf("%+v", cfg)
	}
	cfg.BaseURL = "https://evil.example/v1" // must be ignored
	cfg.APIKey = "sk-ignored"
	if _, err := cfg.NewProvider(); err != nil {
		t.Fatal(err)
	}
	cfg.TokenSource = nil
	if _, err := cfg.NewProvider(); err == nil {
		t.Fatal("chatgpt auth without a token source must fail")
	}
	// configs sharing one source compare equal (server's cache check uses ==)
	a, b := NewChatGPTConfig("m", "", src), NewChatGPTConfig("m", "", src)
	if a != b {
		t.Fatal("configs with the same source pointer must be equal")
	}
	if c := NewChatGPTConfig("m", "", &fakeSource{}); c == a {
		t.Fatal("different sources must differ")
	}
	if NewChatGPTConfig(" m ", "", src).Model != "m" || NewChatGPTConfig("", "", src).Model != "" {
		t.Fatal("model must be trimmed and never defaulted to a hard-coded identifier")
	}
	if _, err := NewChatGPTConfig("m", "ftp://nope", src).NewProvider(); err == nil {
		t.Fatal("bad proxy must be reported")
	}
}

func TestFetchChatGPTModels(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Authorization") != "Bearer new" || r.URL.Path != "/v1/models" {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":{"code":"subscription_sharing_route_not_supported"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[
			{"slug":"gpt-a","display_name":"GPT A","visibility":"list"},
			{"slug":"hidden-b","display_name":"Hidden","visibility":"hide"},
			{"slug":"gpt-c","display_name":"GPT C","visibility":"list"}]}`))
	}))
	defer srv.Close()
	src := &fakeSource{tok: "old", next: "new"}
	got, err := fetchChatGPTModels(context.Background(), srv.Client(), srv.URL+"/v1/models", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Slug != "gpt-a" || got[0].DisplayName != "GPT A" || got[1].Slug != "gpt-c" {
		t.Fatalf("filter/order wrong: %+v", got)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected one refresh retry, hits=%d", hits.Load())
	}

	// error shapes
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/forbidden":
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":{"code":"subscription_sharing_user_not_eligible"}}`))
		case "/nomodels":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case "/incomplete":
			_, _ = w.Write([]byte(`{"models":[{"slug":"","display_name":"x","visibility":"list"}]}`))
		}
	}))
	defer bad.Close()
	if _, err := fetchChatGPTModels(context.Background(), bad.Client(), bad.URL+"/forbidden", &fakeSource{tok: "t"}); siwc.CodeOf(err) != "subscription_sharing_user_not_eligible" {
		t.Fatalf("got %v", err)
	}
	for _, p := range []string{"/nomodels", "/incomplete"} {
		if _, err := fetchChatGPTModels(context.Background(), bad.Client(), bad.URL+p, &fakeSource{tok: "t"}); !errors.Is(err, ErrChatGPTModelCatalog) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if _, err := fetchChatGPTModels(context.Background(), bad.Client(), bad.URL+"/forbidden", &fakeSource{err: siwc.ErrReauthRequired}); siwc.CodeOf(err) != siwc.CodeReauthRequired {
		t.Fatalf("token errors must pass through: %v", err)
	}
}

func TestIsQuotaExhaustedMessageChatGPTUsageLimit(t *testing.T) {
	t.Parallel()
	for _, m := range []string{
		`openai-responses: subscription_sharing_usage_limit_exceeded: A ChatGPT sharing limit has been reached`,
		`openai-responses: status 402: {"error":{"code":"subscription_sharing_usage_limit_exceeded"}}`,
		`SUBSCRIPTION_SHARING_USAGE_LIMIT_EXCEEDED rate limit`, // wins over the transient-limit exclusions
	} {
		if !IsQuotaExhaustedMessage(m) {
			t.Errorf("should be quota: %q", m)
		}
	}
	for _, m := range []string{
		`openai-responses: subscription_sharing_usage_unavailable: try later`,
		`subscription_sharing_user_not_eligible`,
		`status 429: too many requests`,
	} {
		if IsQuotaExhaustedMessage(m) {
			t.Errorf("must not be quota: %q", m)
		}
	}
}
