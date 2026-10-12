package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/siwc"
	"github.com/Autumn-27/norma/llm"
)

func decodeObj(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return m
}

// Request direction, fixture in the documented namespace shape: every function tool moves into one
// namespace; history function_calls carry it; other tool types and unknown names stay put.
func TestSanitizeGroupsFunctionToolsInANamespace(t *testing.T) {
	in := `{"model":"m","input":[
	  {"role":"user","content":"u"},
	  {"type":"function_call","call_id":"c1","name":"run_cmd","arguments":"{}"},
	  {"type":"function_call_output","call_id":"c1","output":"ok"},
	  {"type":"function_call","call_id":"c2","name":"retired_tool","arguments":"{}"},
	  {"type":"function_call","call_id":"c3","name":"read_file","namespace":"other","arguments":"{}"}],
	 "tools":[
	  {"type":"function","name":"run_cmd","description":"d","parameters":{"type":"object"}},
	  {"type":"web_search"},
	  {"type":"function","name":"read_file","parameters":{"type":"object"}}]}`
	out, _, err := sanitizeChatGPTRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeObj(t, out)
	tools := m["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("namespace + the untouched web_search expected: %v", tools)
	}
	ns := tools[0].(map[string]any)
	if ns["type"] != "namespace" || ns["name"] != "artex" || ns["description"] == nil {
		t.Fatalf("namespace: %v", ns)
	}
	var names []string
	for _, ft := range ns["tools"].([]any) {
		f := ft.(map[string]any)
		if f["type"] != "function" {
			t.Fatalf("inner tool: %v", f)
		}
		names = append(names, f["name"].(string))
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"read_file", "run_cmd"}) {
		t.Fatalf("namespaced functions: %v", names)
	}
	if tools[1].(map[string]any)["type"] != "web_search" {
		t.Fatalf("non-function tools stay top-level: %v", tools[1])
	}
	// no top-level function tool remains
	for _, tl := range tools {
		if tl.(map[string]any)["type"] == "function" {
			t.Fatal("top-level function tool must not be sent")
		}
	}
	input := m["input"].([]any)
	byCall := map[string]map[string]any{}
	for _, it := range input {
		o := it.(map[string]any)
		if id, _ := o["call_id"].(string); id != "" && o["type"] == "function_call" {
			byCall[id] = o
		}
	}
	if byCall["c1"]["namespace"] != "artex" {
		t.Fatalf("history call of a namespaced tool must carry the namespace: %v", byCall["c1"])
	}
	if _, ok := byCall["c2"]["namespace"]; ok {
		t.Fatalf("a call whose tool is not in the request is left alone: %v", byCall["c2"])
	}
	if byCall["c3"]["namespace"] != "other" {
		t.Fatalf("an existing namespace is kept: %v", byCall["c3"])
	}
}

func TestSanitizeWithoutToolsLeavesInputUntouched(t *testing.T) {
	in := `{"model":"m","input":[{"type":"function_call","call_id":"c","name":"run_cmd","arguments":"{}"}]}`
	out, _, err := sanitizeChatGPTRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "namespace") {
		t.Fatalf("no tools, no namespace: %s", out)
	}
}

// Every field the preview-limitations page lists as unsupported is removed.
func TestSanitizeStripsEveryDocumentedUnsupportedField(t *testing.T) {
	documented := []string{"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata", "moderation",
		"multi_agent", "prompt", "prompt_cache_retention", "safety_identifier", "temperature", "top_logprobs", "top_p",
		"truncation", "user", "previous_response_id"}
	body := map[string]any{"model": "m", "input": []any{}}
	for _, k := range documented {
		body[k] = "x"
	}
	raw, _ := json.Marshal(body)
	out, _, err := sanitizeChatGPTRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeObj(t, out)
	for _, k := range documented {
		if _, ok := m[k]; ok {
			t.Errorf("%s must be stripped", k)
		}
	}
}

// Response direction through the real stream/aggregate paths: the documented shape (plain name + a separate
// namespace field) and defensively a prefixed name both reach the agent as the ARTEX tool name.
func TestChatGPTNamespacedToolCallsMapBackToARTEXNames(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, added, completed string }{
		{"documented shape",
			`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"run_cmd","namespace":"artex"}}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"run_cmd","namespace":"artex","arguments":"{\"cmd\":\"id\"}"}]}}`},
		{"dotted name",
			`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"artex.run_cmd","namespace":"artex"}}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"artex.run_cmd","namespace":"artex","arguments":"{\"cmd\":\"id\"}"}]}}`},
		{"prefixed name without namespace field",
			`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"artex__run_cmd"}}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"artex__run_cmd","arguments":"{\"cmd\":\"id\"}"}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rs := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				sse(w, tc.added, evToolArgs, tc.completed)
			})
			_, tools, _, _, err := collect(t, rs.provider(t, &fakeSource{tok: "t"}, -1), baseReq())
			if err != nil || len(tools) != 1 || tools[0] != "run_cmd" {
				t.Fatalf("streamed tool names %v %v", tools, err)
			}
			msg, stop, _, err := rs.provider(t, &fakeSource{tok: "t"}, -1).Complete(context.Background(), baseReq())
			if err != nil || stop != "tool_use" {
				t.Fatalf("%q %v", stop, err)
			}
			var got []string
			for _, b := range msg.Content {
				if b.Type == llm.BlockToolUse {
					got = append(got, b.Name)
				}
			}
			if !reflect.DeepEqual(got, []string{"run_cmd"}) {
				t.Fatalf("aggregated tool names %v", got)
			}
		})
	}
}

func TestPlainFunctionCallLeavesForeignAndPlainNamesAlone(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"type":"function_call","name":"run_cmd","namespace":"artex"}`, `run_cmd`},
		{`{"type":"function_call","name":"artex.run_cmd","namespace":"artex"}`, `run_cmd`},
		{`{"type":"function_call","name":"artex.run_cmd","namespace":"crm"}`, `artex.run_cmd`},
		{`{"type":"function_call","name":"artex_scan"}`, `artex_scan`},
		{`{"type":"function_call","name":"artex."}`, `artex.`},
		{`{"type":"message","name":"artex.run_cmd"}`, `artex.run_cmd`},
	} {
		var it map[string]json.RawMessage
		_ = json.Unmarshal([]byte(tc.in), &it)
		plainFunctionCall(it)
		var got string
		_ = json.Unmarshal(it["name"], &got)
		if got != tc.want {
			t.Errorf("%s -> %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A 401 that persists on a fresh token is not retried: the session hands back the same token.
func TestChatGPTPersistent401WithFreshTokenIsNotRepeated(t *testing.T) {
	t.Parallel()
	rs := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"subscription_sharing_invalid_user"}}`))
	})
	src := &fakeSource{tok: "a", next: "a"} // refusing to refresh: same token comes back
	_, _, _, _, err := collect(t, rs.provider(t, src, 0), baseReq())
	if err == nil || !strings.Contains(err.Error(), "status 401") || !strings.Contains(err.Error(), "subscription_sharing_invalid_user") {
		t.Fatalf("the server's own 401 must be reported: %v", err)
	}
	if rs.hits.Load() != 1 {
		t.Fatalf("the same token must not be re-sent, got %d requests", rs.hits.Load())
	}
}

func TestChatGPTInvalidClientIsA401WithoutRetries(t *testing.T) {
	t.Parallel()
	rs := newResponsesServer(t, func(_ int, w http.ResponseWriter, r *http.Request) { t.Error("server must not be called") })
	_, _, _, _, err := collect(t, rs.provider(t, &fakeSource{err: &siwc.Error{Code: siwc.CodeInvalidClient}}, 3), baseReq())
	if err == nil || !strings.Contains(err.Error(), "status 401") || !strings.Contains(err.Error(), "chatgpt_invalid_client") {
		t.Fatalf("got %v", err)
	}
}

type closeTracker struct {
	io.Reader
	closed atomic.Bool
}

func (c *closeTracker) Close() error { c.closed.Store(true); return nil }

func TestChatGPTTransportClosesTheRequestBodyOnEarlyReturn(t *testing.T) {
	t.Parallel()
	tr := &chatgptTransport{base: http.DefaultTransport, src: &fakeSource{tok: "t"}, endpoint: chatGPTResponsesEndpoint}
	body := &closeTracker{Reader: strings.NewReader(`{}`)}
	req := httptest.NewRequest(http.MethodPost, "https://evil.example/v1/responses", body)
	if _, err := tr.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("got %v", err)
	}
	if !body.closed.Load() {
		t.Fatal("request body not closed on the refusal path")
	}
	body2 := &closeTracker{Reader: strings.NewReader(`[1]`)}
	req2 := httptest.NewRequest(http.MethodPost, chatGPTResponsesEndpoint, body2)
	req2.GetBody = nil
	if _, err := tr.RoundTrip(req2); err == nil {
		t.Fatal("non-object body must fail")
	}
	if !body2.closed.Load() {
		t.Fatal("request body not closed when reading it")
	}
}

func TestProxyErrorsNeverEchoCredentials(t *testing.T) {
	for _, proxy := range []string{
		"http://user:s3cret@bad host:8080",  // unparseable
		"ftp://user:s3cret@proxy.example:1", // unsupported scheme
		"user:s3cret@proxy.example:8080",    // missing scheme
		"http://user:s3cret@",               // no host
	} {
		_, err := baseTransport(proxy)
		if err == nil {
			t.Fatalf("%q must be rejected", proxy)
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "user:") {
			t.Errorf("%q leaked credentials: %v", proxy, err)
		}
	}
	if got := DescribeProxy("http://user:s3cret@proxy.example:8080"); got != "http://proxy.example:8080" {
		t.Fatalf("describe: %q", got)
	}
	if got := DescribeProxy("http://user:s3cret@bad host"); strings.Contains(got, "s3cret") {
		t.Fatalf("describe: %q", got)
	}
}

func TestSharedTransportIsReused(t *testing.T) {
	a, err := sharedTransport("http://proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := sharedTransport(" http://proxy.example:8080 ")
	if a != b {
		t.Fatal("same proxy must share one transport")
	}
	if c, _ := sharedTransport(""); c == a {
		t.Fatal("different proxies must not share")
	}
	if _, err := sharedTransport("not a proxy"); err == nil {
		t.Fatal("invalid proxy must fail closed")
	}
}
