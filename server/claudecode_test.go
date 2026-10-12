package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/claudecode"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
)

// claude-code 인증 방식 API 의 통합 테스트(실제 Postgres, 가짜 claude 스크립트). 실제 claude 나 Anthropic 과는
// 통신하지 않는다. DB 설정이 없으면 건너뛴다.

type ccRig struct {
	t     *testing.T
	s     *Server
	d     *db.DB
	h     http.Handler
	token string
}

func newCCRig(t *testing.T) *ccRig {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	// 저장 설정을 테스트 전후로 초기 상태로 되돌린다. claude-code 실행 환경(Manager)은 서버마다 따로다.
	reset := func() { _, _ = d.SaveClaudeCodeSettings(db.ClaudeCodeSettings{}) }
	reset()
	t.Cleanup(reset)

	s := &Server{m: &Manager{pg: d}, jwtKey: []byte(strings.Repeat("c", 32)), llmHealth: newLLMHealthRegistry(d)}
	t.Cleanup(s.CloseClaudeCode)
	tok, err := signJWT(s.signingKey(s.readPassHash()))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/llm/profiles", s.pgListProfiles)
	mux.HandleFunc("POST /api/llm/profiles", s.pgSaveProfile)
	mux.HandleFunc("POST /api/llm/test", s.testLLM)
	mux.HandleFunc("GET /api/llm/claude-code/settings", s.claudeCodeGetSettings)
	mux.HandleFunc("PUT /api/llm/claude-code/settings", s.claudeCodePutSettings)
	mux.HandleFunc("GET /api/llm/claude-code/status", s.claudeCodeStatus)
	root := http.NewServeMux()
	root.Handle("/api/", s.requireAuth(mux))
	return &ccRig{t: t, s: s, d: d, h: root, token: tok}
}

func (r *ccRig) do(method, path string, body any) (int, map[string]any) {
	r.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.token)
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (r *ccRig) enable() {
	r.t.Helper()
	if code, out := r.do("PUT", "/api/llm/claude-code/settings", map[string]any{"enabled": true, "acknowledged": true, "max_concurrent": 1}); code != 200 {
		r.t.Fatalf("enable = %d %v", code, out)
	}
}

// fakeClaudeScript 는 sh 로 만든 최소 claude 다: --version, auth status, 그리고 한 줄 입력에 "OK" 로 답하는 stream-json.
func fakeClaudeScript(t *testing.T, authJSON string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sh 기반 가짜 claude 는 윈도우에서 쓰지 않는다")
	}
	script := `#!/bin/sh
case "$1" in
--version) echo "2.1.300 (Claude Code)"; exit 0;;
auth) echo '` + authJSON + `'; exit 0;;
esac
read line
echo '{"type":"system","subtype":"init","session_id":"x","mcp_servers":[]}'
echo '{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":3,"output_tokens":1}}}}'
echo '{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}}'
echo '{"type":"stream_event","event":{"type":"content_block_stop","index":0}}'
echo '{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}}'
echo '{"type":"stream_event","event":{"type":"message_stop"}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"OK"}'
`
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClaudeCodeSettingsOptIn(t *testing.T) {
	r := newCCRig(t)
	code, out := r.do("GET", "/api/llm/claude-code/settings", nil)
	if code != 200 || out["enabled"] != false || out["effective_enabled"] != false || out["max_concurrent"] != float64(1) || out["max_concurrent_hard"] != float64(3) {
		t.Fatalf("기본값은 꺼짐/동시 1: %d %v", code, out)
	}
	// 고지 확인 없이는 켤 수 없다.
	if code, out := r.do("PUT", "/api/llm/claude-code/settings", map[string]any{"enabled": true}); code != 400 {
		t.Fatalf("확인 없이 켜짐: %d %v", code, out)
	}
	if r.s.claudeManager().Enabled() {
		t.Fatal("거절됐는데 켜졌다")
	}
	code, out = r.do("PUT", "/api/llm/claude-code/settings", map[string]any{"enabled": true, "acknowledged": true, "max_concurrent": 99, "binary_path": " /opt/claude "})
	if code != 200 || out["enabled"] != true || out["max_concurrent"] != float64(3) || out["binary_path"] != "/opt/claude" {
		t.Fatalf("%d %v", code, out)
	}
	if !r.s.claudeManager().Enabled() || r.s.claudeManager().MaxConcurrent() != 3 {
		t.Fatalf("패키지에 반영되지 않음: enabled=%v max=%d", r.s.claudeManager().Enabled(), r.s.claudeManager().MaxConcurrent())
	}
	// 이미 켜진 상태의 갱신에는 확인이 다시 필요 없다.
	if code, _ := r.do("PUT", "/api/llm/claude-code/settings", map[string]any{"enabled": true, "max_concurrent": 2}); code != 200 {
		t.Fatalf("갱신 = %d", code)
	}
	if r.s.claudeManager().MaxConcurrent() != 2 {
		t.Fatal("상한 변경 미반영")
	}
	if code, _ := r.do("PUT", "/api/llm/claude-code/settings", map[string]any{"enabled": true, "binary_path": "a\nb"}); code != 400 {
		t.Fatalf("줄바꿈 경로가 통과: %d", code)
	}
	code, out = r.do("PUT", "/api/llm/claude-code/settings", map[string]any{"enabled": false})
	if code != 200 || r.s.claudeManager().Enabled() {
		t.Fatalf("끄기 실패: %d %v", code, out)
	}
}

func TestClaudeCodeRefusesNonLoopbackListen(t *testing.T) {
	r := newCCRig(t)
	r.s.listenAddr = ":8787"
	code, out := r.do("PUT", "/api/llm/claude-code/settings", map[string]any{"enabled": true, "acknowledged": true})
	if code != 400 || r.s.claudeManager().Enabled() {
		t.Fatalf("loopback 이 아닌데 켜짐: %d %v", code, out)
	}
	if _, out := r.do("GET", "/api/llm/claude-code/settings", nil); out["loopback_only"] != false {
		t.Fatalf("%v", out)
	}
	// 소유자가 확인한 배포는 환경변수로 허용한다.
	t.Setenv(claudeCodeAllowRemoteEnv, "1")
	if code, out := r.do("PUT", "/api/llm/claude-code/settings", map[string]any{"enabled": true, "acknowledged": true}); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	// 시작 시 저장 값이 켜져 있어도 loopback 이 아니면 켜지지 않는다.
	t.Setenv(claudeCodeAllowRemoteEnv, "")
	r.s.claudeManager().SetEnabled(false)
	r.s.applyClaudeCodeSettings()
	if r.s.claudeManager().Enabled() {
		t.Fatal("loopback 이 아닌 주소에서 저장 값만 보고 켜졌다")
	}
	r.s.listenAddr = "127.0.0.1:8787"
	r.s.applyClaudeCodeSettings()
	if !r.s.claudeManager().Enabled() {
		t.Fatal("loopback 에서는 저장 값대로 켜져야 한다")
	}
}

func TestClaudeCodeProfileSaveNormalizes(t *testing.T) {
	r := newCCRig(t)
	body := map[string]any{
		"name": "cc-test-profile", "model": " sonnet ", "auth_mode": "claude-code", "format": "openai",
		"base_url": "https://evil.example/v1", "api_key": "sk-should-not-be-kept", "max_tokens": 4096, "max_tokens_field": "max_completion_tokens",
		"streaming": false, "session_header_key": "x-sess", "rate_per_second": 5, "rate_per_minute": 10, "thinking_type": "enabled",
		"reasoning_effort": "HIGH",
	}
	// 꺼져 있으면 새로 만들 수 없다(옵트인).
	if code, out := r.do("POST", "/api/llm/profiles", body); code != 400 {
		t.Fatalf("꺼진 상태에서 생성 = %d %v", code, out)
	}
	r.enable()
	code, out := r.do("POST", "/api/llm/profiles", body)
	if code != 200 {
		t.Fatalf("저장 = %d %v", code, out)
	}
	id := int64(out["id"].(float64))
	t.Cleanup(func() { _ = r.d.DeleteProfile(id) })
	p, err := r.d.ProfileByID(id)
	if err != nil || p == nil {
		t.Fatal(err)
	}
	if p.AuthMode != db.AuthModeClaudeCode || p.Format != "anthropic" || p.BaseURL != "" || p.APIKey != "" || p.APIKeyHint != "" ||
		p.MaxTokens != 0 || p.MaxTokensField != "" || p.SessionHeaderKey != "" || !p.Streaming ||
		p.RatePerSecond != 0 || p.RatePerMinute != 0 || p.ThinkingType != "" {
		t.Fatalf("정규화 안 됨: %+v", p)
	}
	if p.Model != "sonnet" || p.ReasoningEffort != "high" {
		t.Fatalf("모델/강도는 사용자가 정한 값 그대로여야 한다: %q %q", p.Model, p.ReasoningEffort)
	}
	// 잘못된 강도.
	bad := map[string]any{"name": "cc-test-bad", "auth_mode": "claude-code", "reasoning_effort": "ultra"}
	if code, _ := r.do("POST", "/api/llm/profiles", bad); code != 400 {
		t.Fatalf("잘못된 강도가 통과: %d", code)
	}
	// 꺼진 뒤에도 기존 claude-code 프로필 수정은 가능하다.
	r.do("PUT", "/api/llm/claude-code/settings", map[string]any{"enabled": false})
	body["id"] = id
	body["model"] = "opus"
	if code, out := r.do("POST", "/api/llm/profiles", body); code != 200 {
		t.Fatalf("기존 프로필 수정 = %d %v", code, out)
	}
	// 목록 응답에 자격 증명 관련 값이 없다.
	_, list := r.do("GET", "/api/llm/profiles", nil)
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "sk-should-not-be-kept") {
		t.Fatal("API Key 가 응답에 있다")
	}
}

func TestClaudeCodeProfileConfigAndResolution(t *testing.T) {
	r := newCCRig(t)
	p := &db.LLMProfile{ID: 7, Name: "n", AuthMode: db.AuthModeClaudeCode, Model: "", ReasoningEffort: "max", Format: "anthropic"}
	cfg, ready := r.s.profileAgentConfig(p)
	if !cfg.UsesClaudeCode() || cfg.Model != "" || cfg.ReasoningEffort != "max" || !cfg.Stream {
		t.Fatalf("%+v", cfg)
	}
	if ready {
		t.Fatal("꺼져 있으면 준비되지 않은 것으로 본다")
	}
	if res := r.s.resolutionFromProfile(p, "test"); res.Available || res.Reason != reasonLLMProfileClaudeCodeOff {
		t.Fatalf("%+v", res)
	}
	r.enable()
	if _, ready := r.s.profileAgentConfig(p); !ready {
		t.Fatal("켜면 준비됨")
	}
	bin := fakeClaudeScript(t, `{"loggedIn":true,"authMethod":"claude.ai"}`)
	r.do("PUT", "/api/llm/claude-code/settings", map[string]any{"enabled": true, "binary_path": bin})
	cfg, _ = r.s.profileAgentConfig(p)
	if cfg.ClaudeBinary != bin {
		t.Fatalf("바이너리 경로 미반영: %q", cfg.ClaudeBinary)
	}
}

func TestClaudeCodeStatusEndpoint(t *testing.T) {
	r := newCCRig(t)
	for _, tc := range []struct {
		auth    string
		usable  bool
		problem string
	}{
		{`{"loggedIn":true,"authMethod":"claude.ai"}`, true, ""},
		{`{"loggedIn":true,"authMethod":"api_key"}`, false, claudecode.ProblemAPIBilling},
		{`{"loggedIn":false,"authMethod":"none"}`, false, claudecode.ProblemNotLoggedIn},
	} {
		bin := fakeClaudeScript(t, tc.auth)
		if _, err := r.d.SaveClaudeCodeSettings(db.ClaudeCodeSettings{BinaryPath: bin}); err != nil {
			t.Fatal(err)
		}
		code, out := r.do("GET", "/api/llm/claude-code/status", nil)
		st, _ := out["status"].(map[string]any)
		if code != 200 || st["usable"] != tc.usable || (tc.problem != "" && st["problem"] != tc.problem) {
			t.Fatalf("%s → %d %v", tc.auth, code, out)
		}
		raw, _ := json.Marshal(out)
		if strings.Contains(strings.ToLower(string(raw)), "token") {
			t.Fatalf("상태 응답에 token 이 언급된다: %s", raw)
		}
	}
	// 미설치.
	r.d.SaveClaudeCodeSettings(db.ClaudeCodeSettings{BinaryPath: "/no/such/claude"})
	_, out := r.do("GET", "/api/llm/claude-code/status", nil)
	if st, _ := out["status"].(map[string]any); st["problem"] != claudecode.ProblemNotInstalled {
		t.Fatalf("%v", out)
	}
}

func TestClaudeCodeConnectionTest(t *testing.T) {
	r := newCCRig(t)
	bin := fakeClaudeScript(t, `{"loggedIn":true,"authMethod":"claude.ai"}`)
	// 꺼진 상태.
	_, out := r.do("POST", "/api/llm/test", map[string]any{"auth_mode": "claude-code"})
	if out["ok"] != false {
		t.Fatalf("꺼져 있는데 통과: %v", out)
	}
	r.do("PUT", "/api/llm/claude-code/settings", map[string]any{"enabled": true, "acknowledged": true, "binary_path": bin})
	_, out = r.do("POST", "/api/llm/test", map[string]any{"auth_mode": "claude-code", "model": "sonnet", "reasoning_effort": "low"})
	if out["ok"] != true || !strings.Contains(out["reply"].(string), "OK") {
		t.Fatalf("연결 테스트: %v", out)
	}
	// 구독이 아닌 인증이면 과금 경로이므로 실행하기 전에 막는다.
	apiBin := fakeClaudeScript(t, `{"loggedIn":true,"authMethod":"api_key"}`)
	r.do("PUT", "/api/llm/claude-code/settings", map[string]any{"enabled": true, "binary_path": apiBin})
	_, out = r.do("POST", "/api/llm/test", map[string]any{"auth_mode": "claude-code"})
	if out["ok"] != false || !strings.Contains(out["error"].(string), "API") {
		t.Fatalf("api_key 인증이 통과: %v", out)
	}
	if _, out = r.do("POST", "/api/llm/test", map[string]any{"auth_mode": "claude-code", "reasoning_effort": "bogus"}); out["ok"] != false {
		t.Fatalf("%v", out)
	}
}

func TestClaudeCodeRetryClassification(t *testing.T) {
	for _, e := range []error{
		&claudecode.Error{Kind: claudecode.KindUsageLimit, Message: "limit"},
		&claudecode.Error{Kind: claudecode.KindAuth, Message: "login"},
		&claudecode.Error{Kind: claudecode.KindNotFound, Message: "missing"},
		&claudecode.Error{Kind: claudecode.KindTooOld, Message: "old"},
		&claudecode.Error{Kind: claudecode.KindRefused, Message: "off"},
	} {
		if isRetryableStreamError(e) {
			t.Errorf("재시도하면 안 된다: %v", e)
		}
	}
	for _, e := range []error{
		&claudecode.Error{Kind: claudecode.KindCrash, Message: "exit 3"},
		&claudecode.Error{Kind: claudecode.KindRateLimit, Message: "overloaded"},
		errors.New("claude-code: protocol"),
	} {
		if !isRetryableStreamError(e) {
			t.Errorf("재시도 가능해야 한다: %v", e)
		}
	}
	if !agent.IsQuotaExhaustedMessage((&claudecode.Error{Kind: claudecode.KindUsageLimit, Message: "limit"}).Error()) {
		t.Error("사용량 한도는 장애 전환 대상이어야 한다")
	}
}

func TestClaudeCodeMessagesTranslated(t *testing.T) {
	hasHangul := func(s string) bool {
		for _, r := range s {
			if unicode.Is(unicode.Hangul, r) {
				return true
			}
		}
		return false
	}
	for _, ko := range []string{
		errClaudeCodeAckRequired, errClaudeCodeDisabled, errClaudeCodeNotLoopback, errClaudeCodeBadEffort, errClaudeCodeBadBinary,
		reasonLLMProfileClaudeCodeOff, msgClaudeCodeNotInstalled, msgClaudeCodeTooOld, msgClaudeCodeNotLoggedIn,
		msgClaudeCodeAPIBilling, msgClaudeCodeUnknown,
	} {
		if !hasHangul(ko) {
			t.Errorf("원문이 한국어가 아닙니다: %q", ko)
		}
		for _, lang := range []string{"en", "zh", "es"} {
			v := errCatalog[ko][lang]
			if v == "" || v == ko {
				t.Errorf("%s 번역이 없습니다: %q", lang, ko)
			}
			if (lang == "en" || lang == "es") && hasHangul(v) {
				t.Errorf("%s 번역에 한글이 남았습니다: %q", lang, v)
			}
		}
	}
	t.Cleanup(func() { config.CurrentLanguage = nil })
	config.CurrentLanguage = func() string { return "en" }
	if got := trMsg(errClaudeCodeDisabled); !strings.Contains(got, "turned off") {
		t.Fatalf("got %q", got)
	}
}
