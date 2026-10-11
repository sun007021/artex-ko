package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/siwc"
)

// ChatGPT 구독 로그인 API 의 통합 테스트. auth.openai.com 대신 프로세스 안의 가짜 발급자를 쓰고,
// 실제 Postgres 에 프로필·토큰을 저장한다(DB 설정이 없으면 건너뜀). 실서버와는 통신하지 않는다.

const (
	testClientID = "oaiapp_test123"
	testRedirect = "http://127.0.0.1:8787/auth/callback"
)

// fakeIssuer 는 OpenID discovery·JWKS·토큰·폐기 엔드포인트를 흉내 낸다.
type fakeIssuer struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu        sync.Mutex
	nonce     string // 마지막 인가 요청의 nonce/챌린지(테스트가 auth_url 에서 읽어 둔다)
	challenge string
	sub       string
	scope     string
	nAccess   int
	revokes   []url.Values
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{t: t, key: key, sub: "user-1", scope: siwc.Scopes}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/api/accounts/authorize",
			"token_endpoint": f.srv.URL + "/api/accounts/oauth/token", "jwks_uri": f.srv.URL + "/jwks",
			"revocation_endpoint": f.srv.URL + "/revoke",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		pub := f.key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/api/accounts/oauth/token", f.token)
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.revokes = append(f.revokes, r.PostForm)
		f.mu.Unlock()
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	form := r.PostForm
	f.mu.Lock()
	defer f.mu.Unlock()
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "the-code" ||
		form.Get("client_id") != testClientID || form.Get("redirect_uri") != testRedirect {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	f.nAccess++
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": f.srv.URL, "aud": testClientID, "sub": f.sub, "email": f.sub + "@example.com",
		"nonce": f.nonce, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "k1"
	idt, err := tok.SignedString(f.key)
	if err != nil {
		f.t.Fatal(err)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": fmt.Sprintf("SECRET-access-%d", f.nAccess), "refresh_token": fmt.Sprintf("SECRET-refresh-%d", f.nAccess),
		"id_token": idt, "token_type": "Bearer", "expires_in": 3600, "scope": f.scope,
	})
}

// learn 은 login/start 가 돌려준 인가 URL 을 검증하고 가짜 발급자에 nonce·챌린지를 알려 준다.
func (f *fakeIssuer) learn(t *testing.T, authURL string) url.Values {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(authURL, f.srv.URL+"/api/accounts/authorize?") {
		t.Fatalf("auth_url 이 발급자의 authorize 가 아닙니다: %s", u.Path)
	}
	q := u.Query()
	f.mu.Lock()
	f.nonce, f.challenge = q.Get("nonce"), q.Get("code_challenge")
	f.mu.Unlock()
	return q
}

func (f *fakeIssuer) revokeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.revokes)
}

type cgptRig struct {
	t      *testing.T
	s      *Server
	d      *db.DB
	iss    *fakeIssuer
	h      http.Handler
	token  string
	bodies []string // 지금까지 받은 모든 응답 본문(토큰 유출 검사용)

	changes int // afterAuthChange 호출 수
}

func newCGPTRig(t *testing.T) *cgptRig {
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

	rig := &cgptRig{t: t, d: d, iss: newFakeIssuer(t)}
	s := &Server{m: &Manager{pg: d}, jwtKey: []byte(strings.Repeat("c", 32)), llmHealth: newLLMHealthRegistry(d)}
	s.cgpt.issuer = rig.iss.srv.URL
	s.afterAuthChange = func() { rig.changes++ }
	rig.s = s
	tok, err := signJWT(s.signingKey(s.readPassHash()))
	if err != nil {
		t.Fatal(err)
	}
	rig.token = tok

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/llm/profiles", s.pgListProfiles)
	mux.HandleFunc("POST /api/llm/profiles", s.pgSaveProfile)
	mux.HandleFunc("DELETE /api/llm/profiles/{id}", s.pgDeleteProfile)
	mux.HandleFunc("POST /api/llm/chatgpt/login/start", s.chatgptLoginStart)
	mux.HandleFunc("POST /api/llm/chatgpt/login/complete", s.chatgptLoginComplete)
	mux.HandleFunc("GET /api/llm/chatgpt/status", s.chatgptStatus)
	mux.HandleFunc("POST /api/llm/chatgpt/logout", s.chatgptLogout)
	mux.HandleFunc("GET /api/llm/chatgpt/models", s.chatgptModels)
	root := http.NewServeMux()
	root.Handle("/api/", s.requireAuth(mux))
	root.HandleFunc("GET /auth/callback", s.chatgptCallback)
	root.HandleFunc("GET /auth/start", s.chatgptStart)
	rig.h = root
	return rig
}

func (r *cgptRig) raw(method, path, token string, body any) *httptest.ResponseRecorder {
	r.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	r.bodies = append(r.bodies, rec.Body.String())
	return rec
}

func (r *cgptRig) do(method, path string, body any) (int, map[string]any) {
	r.t.Helper()
	rec := r.raw(method, path, r.token, body)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// profile 은 구독 프로필을 만들어 id 를 돌려준다. 정리 때 지운다.
func (r *cgptRig) profile(name string) int64 {
	r.t.Helper()
	code, out := r.do("POST", "/api/llm/profiles", map[string]any{
		"name": name, "model": "gpt-test-model", "auth_mode": "chatgpt",
		"base_url": "https://evil.example/v1", "api_key": "sk-should-not-be-kept", "format": "anthropic",
		"max_tokens": 4096, "streaming": false, "reasoning_effort": "high",
	})
	if code != 200 {
		r.t.Fatalf("프로필 저장 = %d %v", code, out)
	}
	id := int64(out["id"].(float64))
	r.t.Cleanup(func() { _ = r.d.DeleteProfile(id) })
	return id
}

func (r *cgptRig) start(id int64, extra map[string]any) (string, url.Values) {
	r.t.Helper()
	body := map[string]any{"profile_id": id}
	for k, v := range extra {
		body[k] = v
	}
	code, out := r.do("POST", "/api/llm/chatgpt/login/start", body)
	if code != 200 {
		r.t.Fatalf("login/start = %d %v", code, out)
	}
	authURL, _ := out["auth_url"].(string)
	if strings.Contains(authURL, "id_token_hint") || strings.Contains(authURL, "eyJ") {
		r.t.Fatalf("UI 로 가는 auth_url 에 ID 토큰이 있으면 안 됩니다: %s", authURL)
	}
	if strings.HasPrefix(authURL, "/auth/start?") { // 재인가: 서버가 브라우저를 302 로 보낸다
		rec := r.raw("GET", authURL, "", nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Cache-Control") != "no-store" {
			r.t.Fatalf("/auth/start = %d %v", rec.Code, rec.Header())
		}
		authURL = rec.Header().Get("Location")
	}
	return authURL, r.iss.learn(r.t, authURL)
}

func (r *cgptRig) status(id int64) map[string]any {
	r.t.Helper()
	code, out := r.do("GET", fmt.Sprintf("/api/llm/chatgpt/status?profile_id=%d", id), nil)
	if code != 200 {
		r.t.Fatalf("status = %d %v", code, out)
	}
	return out
}

func (r *cgptRig) noSecretsLeaked() {
	r.t.Helper()
	for _, b := range r.bodies {
		for _, bad := range []string{"SECRET-access", "SECRET-refresh", "sk-should-not-be-kept", "eyJ", "the-code"} {
			if strings.Contains(b, bad) {
				r.t.Fatalf("응답에 비밀 값(%q)이 들어 있습니다: %.200s", bad, b)
			}
		}
	}
}

func TestChatGPTLoginFlowDB(t *testing.T) {
	r := newCGPTRig(t)
	id := r.profile("cgpt-flow-" + fmt.Sprint(time.Now().UnixNano()))

	// 저장 시 정규화: 형식 고정, base_url·키 제거, 출력 상한 0, 스트리밍 on.
	p, err := r.d.ProfileByID(id)
	if err != nil || p == nil {
		t.Fatal(err)
	}
	if p.AuthMode != db.AuthModeChatGPT || p.Format != "openai-responses" || p.BaseURL != "" || p.APIKey != "" ||
		p.MaxTokens != 0 || !p.Streaming || p.ReasoningEffort != "high" {
		t.Fatalf("정규화되지 않았습니다: %+v", p)
	}
	if st := r.status(id); st["status"] != "disconnected" || st["registered"] != false {
		t.Fatalf("초기 상태: %v", st)
	}

	// 로그인 시작.
	_, q := r.start(id, nil)
	if q.Get("client_id") != siwc.DynamicClientID || q.Get("agent_name_hint") != "ARTEX" ||
		q.Get("redirect_uri") != testRedirect || q.Get("code_challenge_method") != "S256" ||
		q.Get("resource") != siwc.Resource || q.Get("response_type") != "code" ||
		!strings.Contains(q.Get("scope"), siwc.ScopePlanUsage) || !strings.HasPrefix(q.Get("ext_agent_host_id"), "urn:uuid:") ||
		q.Get("state") == "" || q.Get("nonce") == "" || q.Get("prompt") != "" || q.Has("id_token_hint") {
		t.Fatalf("인가 요청 파라미터가 다릅니다: %v", q)
	}
	if st := r.status(id); st["login"].(map[string]any)["state"] != "pending" {
		t.Fatalf("진행 중 상태가 아닙니다: %v", st)
	}
	state := q.Get("state")

	// 콜백 보호: state 가 없거나 모르는 값이면 아무 것도 소모하지 않는다(무인증 경로).
	for _, bad := range []string{"", "?state=nope&code=the-code&client_id=" + testClientID, "?code=the-code&client_id=" + testClientID} {
		rec := r.raw("GET", "/auth/callback"+bad, "", nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("잘못된 콜백 %q = %d", bad, rec.Code)
		}
	}
	if st := r.status(id); st["login"].(map[string]any)["state"] != "pending" || st["status"] != "disconnected" {
		t.Fatalf("잘못된 콜백이 진행 중 로그인에 영향을 줬습니다: %v", st)
	}

	// 정상 콜백.
	rec := r.raw("GET", "/auth/callback?code=the-code&client_id="+testClientID+"&state="+state, "", nil)
	if rec.Code != 200 {
		t.Fatalf("콜백 = %d %s", rec.Code, rec.Body.String())
	}
	page := rec.Body.String()
	if strings.Contains(page, "the-code") || strings.Contains(page, state) || strings.Contains(page, "SECRET") {
		t.Fatal("콜백 페이지에 코드·state·토큰이 있으면 안 됩니다")
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("콜백 보안 헤더: %v", rec.Header())
	}
	if r.changes != 1 {
		t.Fatalf("로그인 뒤 provider 재구성이 %d 번 호출됨", r.changes)
	}
	st := r.status(id)
	if st["status"] != "connected" || st["email"] != "user-1@example.com" || st["registered"] != true || st["plan_usage"] != true ||
		st["login"].(map[string]any)["state"] != "done" || st["expires_at"] == nil {
		t.Fatalf("로그인 뒤 상태: %v", st)
	}
	o, err := r.d.GetProfileOAuth(context.Background(), id)
	if err != nil || o == nil || o.ClientID != testClientID || o.RefreshToken != "SECRET-refresh-1" || o.Status != db.OAuthStatusConnected {
		t.Fatalf("저장된 자격 증명: %+v %v", o, err)
	}

	// state 는 일회용이다. 같은 콜백을 다시 보내면 거부된다.
	if rec := r.raw("GET", "/auth/callback?code=the-code&client_id="+testClientID+"&state="+state, "", nil); rec.Code != 400 {
		t.Fatalf("재사용된 state = %d", rec.Code)
	}

	// 목록에는 UI 안전 요약만 실리고 키·토큰은 없다.
	rec = r.raw("GET", "/api/llm/profiles", r.token, nil)
	var listBody struct {
		Profiles []map[string]any `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listBody); err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, it := range listBody.Profiles {
		if it["id"] == fmt.Sprint(id) {
			found = it
		}
	}
	if found == nil || found["auth_mode"] != "chatgpt" || found["api_key"] != nil {
		t.Fatalf("목록 항목: %v", found)
	}
	if cg, _ := found["chatgpt"].(map[string]any); cg == nil || cg["email"] != "user-1@example.com" {
		t.Fatalf("목록의 ChatGPT 요약: %v", found["chatgpt"])
	}

	// 에이전트 구성: 준비됨, 토큰 소스 연결, 사용자가 고른 추론 강도 유지.
	cfg, ready := r.s.profileAgentConfig(p)
	if !ready || !cfg.UsesChatGPT() || cfg.TokenSource == nil || cfg.MaxTokens != 0 || !cfg.Stream || cfg.ReasoningEffort != "high" {
		t.Fatalf("agent.Config: ready=%v %+v", ready, cfg)
	}
	cfg2, _ := r.s.profileAgentConfig(p)
	if cfg != cfg2 {
		t.Fatal("같은 프로필의 구성은 == 로 같아야 합니다(캐시 판별)")
	}

	// 재로그인(재인가)은 저장된 client_id·id_token_hint 를 쓰고 agent_name_hint 는 뺀다.
	_, q = r.start(id, nil)
	if q.Get("client_id") != testClientID || q.Has("agent_name_hint") || q.Get("id_token_hint") == "" || q.Get("login_hint") != "user-1@example.com" {
		t.Fatalf("재인가 파라미터: %v", q)
	}
	_, q = r.start(id, map[string]any{"reconsent": true, "redirect_port": 9001})
	if q.Get("prompt") != "consent" || q.Get("redirect_uri") != "http://127.0.0.1:9001/auth/callback" {
		t.Fatalf("재동의 파라미터: %v", q)
	}

	// 로그아웃: 원격 폐기 + 로컬 토큰 삭제.
	code, out := r.do("POST", "/api/llm/chatgpt/logout", map[string]any{"profile_id": id})
	if code != 200 || out["ok"] != true || out["revoked"] != true {
		t.Fatalf("logout = %d %v", code, out)
	}
	if r.iss.revokeCount() != 1 {
		t.Fatalf("원격 폐기 호출 수 = %d", r.iss.revokeCount())
	}
	if st := r.status(id); st["status"] != "disconnected" {
		t.Fatalf("로그아웃 뒤 상태: %v", st)
	}
	o, _ = r.d.GetProfileOAuth(context.Background(), id)
	if o != nil && (o.RefreshToken != "" || o.AccessToken != "") {
		t.Fatalf("로그아웃 뒤에도 토큰이 남아 있습니다: %+v", o)
	}
	if _, ready := r.s.profileAgentConfig(p); ready {
		t.Fatal("로그아웃한 프로필은 준비 상태가 아니어야 합니다")
	}
	r.noSecretsLeaked()
}

func TestChatGPTManualPasteDB(t *testing.T) {
	r := newCGPTRig(t)
	id := r.profile("cgpt-paste-" + fmt.Sprint(time.Now().UnixNano()))

	// 진행 중 로그인이 없으면 거부.
	code, out := r.do("POST", "/api/llm/chatgpt/login/complete", map[string]any{"profile_id": id, "callback_url": testRedirect + "?code=x"})
	if code != 409 {
		t.Fatalf("진행 중 로그인 없음 = %d %v", code, out)
	}

	_, q := r.start(id, nil)
	state := q.Get("state")

	// 빈 값·state 불일치는 거부하되 진행 중 로그인을 소모하지 않는다.
	code, _ = r.do("POST", "/api/llm/chatgpt/login/complete", map[string]any{"profile_id": id, "callback_url": " "})
	if code != 400 {
		t.Fatalf("빈 값 = %d", code)
	}
	code, out = r.do("POST", "/api/llm/chatgpt/login/complete", map[string]any{"profile_id": id,
		"callback_url": testRedirect + "?code=the-code&client_id=" + testClientID + "&state=wrong"})
	if code != 400 || out["code"] != siwc.CodeInvalidState {
		t.Fatalf("state 불일치 = %d %v", code, out)
	}
	if st := r.status(id); st["login"].(map[string]any)["state"] != "pending" {
		t.Fatalf("state 불일치가 로그인을 소모했습니다: %v", st)
	}

	// 주소창 전체 URL 을 그대로 붙여 넣으면 완료된다.
	code, out = r.do("POST", "/api/llm/chatgpt/login/complete", map[string]any{"profile_id": id,
		"callback_url": testRedirect + "?code=the-code&client_id=" + testClientID + "&state=" + state})
	if code != 200 || out["status"] != "connected" {
		t.Fatalf("붙여넣기 완료 = %d %v", code, out)
	}
	// 다시 붙이면 진행 중 로그인이 없다.
	code, _ = r.do("POST", "/api/llm/chatgpt/login/complete", map[string]any{"profile_id": id,
		"callback_url": testRedirect + "?code=the-code&client_id=" + testClientID + "&state=" + state})
	if code != 409 {
		t.Fatalf("재사용 = %d", code)
	}
	r.noSecretsLeaked()
}

func TestChatGPTDeniedAndExchangeFailureDB(t *testing.T) {
	r := newCGPTRig(t)
	id := r.profile("cgpt-deny-" + fmt.Sprint(time.Now().UnixNano()))

	// 사용자가 거부 → 코드 교환 없이 실패, 상태에 오류 코드가 남는다.
	_, q := r.start(id, nil)
	rec := r.raw("GET", "/auth/callback?error=access_denied&state="+q.Get("state"), "", nil)
	if rec.Code != 400 {
		t.Fatalf("거부 콜백 = %d", rec.Code)
	}
	st := r.status(id)
	lg := st["login"].(map[string]any)
	if lg["state"] != "error" || lg["code"] != siwc.CodeAccessDenied || lg["error"] == "" {
		t.Fatalf("거부 상태: %v", st)
	}
	if r.iss.nAccess != 0 {
		t.Fatal("거부된 로그인에서 코드 교환을 시도했습니다")
	}

	// 코드 교환 실패(invalid_grant)여도 발급된 client_id 는 남는다: 다음 로그인은 새 앱을 또 등록하지 않는다.
	_, q = r.start(id, nil)
	rec = r.raw("GET", "/auth/callback?code=bad-code&client_id="+testClientID+"&state="+q.Get("state"), "", nil)
	if rec.Code != 400 {
		t.Fatalf("교환 실패 콜백 = %d", rec.Code)
	}
	if st := r.status(id); st["status"] != "disconnected" || st["registered"] != true || st["login"].(map[string]any)["code"] != "invalid_grant" {
		t.Fatalf("교환 실패 상태: %v", st)
	}
	_, q = r.start(id, nil)
	if q.Get("client_id") != testClientID || q.Has("agent_name_hint") {
		t.Fatalf("저장된 client_id 를 재사용해야 합니다: %v", q)
	}

	// 다른 계정으로 재로그인하면 거부하고 기존 연결을 유지한다.
	r.iss.mu.Lock()
	r.iss.sub = "user-1"
	r.iss.mu.Unlock()
	rec = r.raw("GET", "/auth/callback?code=the-code&client_id="+testClientID+"&state="+q.Get("state"), "", nil)
	if rec.Code != 200 {
		t.Fatalf("첫 로그인 = %d", rec.Code)
	}
	_, q = r.start(id, nil)
	r.iss.mu.Lock()
	r.iss.sub = "someone-else"
	r.iss.mu.Unlock()
	rec = r.raw("GET", "/auth/callback?code=the-code&client_id="+testClientID+"&state="+q.Get("state"), "", nil)
	if rec.Code != 400 {
		t.Fatalf("다른 계정 = %d", rec.Code)
	}
	st = r.status(id)
	if st["status"] != "connected" || st["email"] != "user-1@example.com" || st["login"].(map[string]any)["code"] != siwc.CodeAccountMismatch {
		t.Fatalf("다른 계정이 기존 연결을 덮어썼습니다: %v", st)
	}
	r.noSecretsLeaked()
}

func TestChatGPTEndpointsRequireAuthDB(t *testing.T) {
	r := newCGPTRig(t)
	id := r.profile("cgpt-auth-" + fmt.Sprint(time.Now().UnixNano()))
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/llm/chatgpt/login/start"},
		{"POST", "/api/llm/chatgpt/login/complete"},
		{"GET", fmt.Sprintf("/api/llm/chatgpt/status?profile_id=%d", id)},
		{"POST", "/api/llm/chatgpt/logout"},
		{"GET", fmt.Sprintf("/api/llm/chatgpt/models?profile_id=%d", id)},
	} {
		if rec := r.raw(c.method, c.path, "", map[string]any{"profile_id": id}); rec.Code != 401 {
			t.Fatalf("%s %s 무인증 = %d, 401 이어야 합니다", c.method, c.path, rec.Code)
		}
		if rec := r.raw(c.method, c.path, "not-a-valid-token", map[string]any{"profile_id": id}); rec.Code != 401 {
			t.Fatalf("%s %s 잘못된 토큰 = %d, 401 이어야 합니다", c.method, c.path, rec.Code)
		}
	}
	// /auth/start 도 state 없이는 아무 곳으로도 보내지 않는다.
	for _, path := range []string{"/auth/start", "/auth/start?state=unknown"} {
		if rec := r.raw("GET", path, "", nil); rec.Code != 400 || rec.Header().Get("Location") != "" {
			t.Fatalf("%s = %d %v", path, rec.Code, rec.Header())
		}
	}
	// 반대로 콜백은 JWT 없이 도달하지만(OpenAI 가 부르는 주소) 모르는 state 로는 아무 것도 못 한다.
	if rec := r.raw("GET", "/auth/callback?code=x&state=y", "", nil); rec.Code != 400 {
		t.Fatalf("콜백 = %d", rec.Code)
	}
}

func TestChatGPTRejectsNonSubscriptionProfileDB(t *testing.T) {
	r := newCGPTRig(t)
	code, out := r.do("POST", "/api/llm/profiles", map[string]any{
		"name": "plain-" + fmt.Sprint(time.Now().UnixNano()), "format": "openai-responses", "model": "m", "api_key": "sk-plain-key",
	})
	if code != 200 {
		t.Fatalf("저장 = %d %v", code, out)
	}
	pid := int64(out["id"].(float64))
	t.Cleanup(func() { _ = r.d.DeleteProfile(pid) })
	for _, path := range []string{"/api/llm/chatgpt/login/start", "/api/llm/chatgpt/logout"} {
		if code, _ := r.do("POST", path, map[string]any{"profile_id": pid}); code != 404 {
			t.Fatalf("%s 는 API Key 프로필에 404 여야 합니다: %d", path, code)
		}
	}
	if code, _ := r.do("GET", fmt.Sprintf("/api/llm/chatgpt/status?profile_id=%d", pid), nil); code != 404 {
		t.Fatalf("status = %d", code)
	}
	if code, _ := r.do("POST", "/api/llm/chatgpt/login/start", map[string]any{"profile_id": 987654321}); code != 404 {
		t.Fatalf("없는 프로필 = %d", code)
	}
	// 알 수 없는 인증 방식은 저장 단계에서 막는다.
	if code, _ := r.do("POST", "/api/llm/profiles", map[string]any{"name": "bad-mode", "model": "m", "format": "openai", "auth_mode": "oauth2"}); code != 400 {
		t.Fatalf("알 수 없는 auth_mode = %d", code)
	}
}

func TestChatGPTModelsEndpointDB(t *testing.T) {
	r := newCGPTRig(t)
	id := r.profile("cgpt-models-" + fmt.Sprint(time.Now().UnixNano()))
	var gotSrc agent.TokenSource
	r.s.cgpt.fetchModels = func(_ context.Context, _ string, src agent.TokenSource) ([]agent.ChatGPTModel, error) {
		gotSrc = src
		return []agent.ChatGPTModel{{Slug: "m-a", DisplayName: "Model A"}}, nil
	}
	code, out := r.do("GET", fmt.Sprintf("/api/llm/chatgpt/models?profile_id=%d", id), nil)
	if code != 200 || out["ok"] != true || gotSrc == nil {
		t.Fatalf("models = %d %v", code, out)
	}
	ms, _ := out["models"].([]any)
	if len(ms) != 1 || ms[0].(map[string]any)["slug"] != "m-a" {
		t.Fatalf("models 본문: %v", out)
	}

	// 로그인이 필요하면 200 + ok:false + 안정된 코드(UI 인라인 표시).
	r.s.cgpt.fetchModels = func(context.Context, string, agent.TokenSource) ([]agent.ChatGPTModel, error) {
		return nil, siwc.ErrNotConnected
	}
	code, out = r.do("GET", fmt.Sprintf("/api/llm/chatgpt/models?profile_id=%d", id), nil)
	if code != 200 || out["ok"] != false || out["code"] != siwc.CodeNotConnected || out["error"] == "" {
		t.Fatalf("models 실패 = %d %v", code, out)
	}
	// 알 수 없는 오류는 코드만 덧붙이고 원문(내부 메시지)을 싣지 않는다.
	r.s.cgpt.fetchModels = func(context.Context, string, agent.TokenSource) ([]agent.ChatGPTModel, error) {
		return nil, errors.New("Bearer SECRET-access-1 leaked in transport error")
	}
	_, out = r.do("GET", fmt.Sprintf("/api/llm/chatgpt/models?profile_id=%d", id), nil)
	if out["ok"] != false || strings.Contains(fmt.Sprint(out), "SECRET") {
		t.Fatalf("내부 오류 원문이 노출됩니다: %v", out)
	}
}

func TestChatGPTSwitchToAPIKeyAndDeleteRevokeDB(t *testing.T) {
	r := newCGPTRig(t)
	log := func(id int64) {
		_, q := r.start(id, nil)
		rec := r.raw("GET", "/auth/callback?code=the-code&client_id="+testClientID+"&state="+q.Get("state"), "", nil)
		if rec.Code != 200 {
			t.Fatalf("로그인 = %d", rec.Code)
		}
	}

	// 구독 → API Key 로 전환하면 자격 증명을 폐기·삭제한다.
	id := r.profile("cgpt-switch-" + fmt.Sprint(time.Now().UnixNano()))
	log(id)
	code, out := r.do("POST", "/api/llm/profiles", map[string]any{
		"id": id, "name": "cgpt-switch-renamed-" + fmt.Sprint(time.Now().UnixNano()), "format": "openai-responses", "model": "m", "api_key": "sk-new-key-1234",
	})
	if code != 200 {
		t.Fatalf("전환 저장 = %d %v", code, out)
	}
	if r.iss.revokeCount() != 1 {
		t.Fatalf("전환 시 원격 폐기 호출 수 = %d", r.iss.revokeCount())
	}
	if o, _ := r.d.GetProfileOAuth(context.Background(), id); o != nil {
		t.Fatalf("전환 뒤에도 OAuth 행이 남았습니다: %+v", o)
	}
	if p, _ := r.d.ProfileByID(id); p == nil || p.AuthMode != "" || p.APIKey != "sk-new-key-1234" {
		t.Fatalf("전환된 프로필: %+v", p)
	}

	// 프로필을 지우면 원격 폐기를 시도하고 행은 CASCADE 로 사라진다.
	id2 := r.profile("cgpt-delete-" + fmt.Sprint(time.Now().UnixNano()))
	log(id2)
	before := r.iss.revokeCount()
	if rec := r.raw("DELETE", fmt.Sprintf("/api/llm/profiles/%d", id2), r.token, nil); rec.Code != 200 {
		t.Fatalf("삭제 = %d %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.iss.revokeCount() == before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if r.iss.revokeCount() != before+1 {
		t.Fatal("프로필 삭제 뒤 원격 폐기가 호출되지 않았습니다")
	}
	if o, _ := r.d.GetProfileOAuth(context.Background(), id2); o != nil {
		t.Fatalf("삭제 뒤에도 OAuth 행이 남았습니다: %+v", o)
	}
	r.noSecretsLeaked()
}

func TestChatGPTTaskResolutionReadinessDB(t *testing.T) {
	r := newCGPTRig(t)
	id := r.profile("cgpt-resolve-" + fmt.Sprint(time.Now().UnixNano()))
	p, _ := r.d.ProfileByID(id)
	if res := r.s.resolutionFromProfile(p, "task_chain"); res.Available || res.Reason != reasonLLMProfileChatGPTOff {
		t.Fatalf("로그인 전: %+v", res)
	}
	_, q := r.start(id, nil)
	if rec := r.raw("GET", "/auth/callback?code=the-code&client_id="+testClientID+"&state="+q.Get("state"), "", nil); rec.Code != 200 {
		t.Fatalf("로그인 = %d", rec.Code)
	}
	if res := r.s.resolutionFromProfile(p, "task_chain"); !res.Available {
		t.Fatalf("로그인 뒤: %+v", res)
	}
	// 풀(failover) 후보에도 로그인된 구독 프로필이 들어간다.
	pool, err := r.d.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	var inPool bool
	for _, pp := range pool {
		inPool = inPool || pp.ID == id
	}
	if !inPool {
		t.Fatal("로그인된 구독 프로필이 풀 후보에 없습니다")
	}
}

func TestChatGPTPendingLoginsAreBounded(t *testing.T) {
	var rt chatgptRuntime
	rt.pending = map[string]*chatgptLogin{}
	for i := 0; i < chatgptMaxPending+5; i++ {
		rt.pending[fmt.Sprint(i)] = &chatgptLogin{profileID: int64(i), p: &siwc.Pending{State: fmt.Sprint(i)}, expires: time.Now().Add(-time.Second)}
	}
	if rt.findLogin("3", false) != nil {
		t.Fatal("만료된 로그인은 찾아지면 안 됩니다")
	}
	if rt.loginForProfile(4) != nil {
		t.Fatal("만료된 로그인은 프로필 조회에서도 제외됩니다")
	}
	live := &chatgptLogin{profileID: 7, p: &siwc.Pending{State: "live"}, expires: time.Now().Add(time.Minute)}
	rt.pending["live"] = live
	if rt.findLogin("live", false) != live || rt.findLogin("live", true) != live || rt.findLogin("live", false) != nil {
		t.Fatal("consume=true 는 한 번만 가져가야 합니다")
	}
}

func TestChatGPTRedirectPort(t *testing.T) {
	s := &Server{}
	mk := func(host string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		return r
	}
	if p := s.chatgptRedirectPort(mk("127.0.0.1:9100"), 0); p != 9100 {
		t.Fatalf("loopback 접속 포트를 따라야 합니다: %d", p)
	}
	if p := s.chatgptRedirectPort(mk("localhost:9100"), 0); p != 9100 {
		t.Fatalf("localhost: %d", p)
	}
	if p := s.chatgptRedirectPort(mk("artex.lan:9100"), 0); p != chatgptDefaultPort {
		t.Fatalf("외부 이름은 리슨 포트/기본값: %d", p)
	}
	s.SetListenAddr("0.0.0.0:8800")
	if p := s.chatgptRedirectPort(mk("artex.lan:9100"), 0); p != 8800 {
		t.Fatalf("리슨 포트: %d", p)
	}
	if p := s.chatgptRedirectPort(mk("127.0.0.1:9100"), 9300); p != 9300 {
		t.Fatalf("명시값 우선: %d", p)
	}
}

func TestChatGPTCallbackPageEscapesAndHasNoQueryEcho(t *testing.T) {
	rec := httptest.NewRecorder()
	chatgptCallbackPage(rec, 400, `<script>alert(1)</script>`, `"><img src=x>`)
	b := rec.Body.String()
	if strings.Contains(b, "<img") || strings.Contains(b, "<script>alert") {
		t.Fatalf("제목·본문이 이스케이프되지 않았습니다: %s", b)
	}
	// CSP 해시가 실제 스크립트와 일치해야 주소창 정리 스크립트가 돈다.
	sum := sha256.Sum256([]byte(chatgptCallbackScript))
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sha256-"+base64.StdEncoding.EncodeToString(sum[:])) {
		t.Fatal("CSP 해시가 스크립트와 다릅니다")
	}
}

func TestChatGPTMessagesTranslated(t *testing.T) {
	hasHangul := func(s string) bool {
		for _, r := range s {
			if unicode.Is(unicode.Hangul, r) {
				return true
			}
		}
		return false
	}
	for _, ko := range []string{
		errChatGPTInvalidState, errChatGPTDenied, errChatGPTRegistration, errChatGPTInvalidGrant,
		errChatGPTInvalidIDToken, errChatGPTIdentityDown, errChatGPTAccountMismatch, errChatGPTLoginRequired,
		errChatGPTPlanDisabled, errChatGPTNetwork, errChatGPTNotEligible, errChatGPTGeneric, errChatGPTNoProfile,
		errChatGPTNoPending, errChatGPTBadCallback, errChatGPTTooManyLogins, errMgmtLLMBadAuthMode,
		reasonLLMProfileChatGPTOff, msgChatGPTCallbackDoneTitle, msgChatGPTCallbackDoneBody,
		msgChatGPTCallbackFailTitle, msgChatGPTCallbackBadReq,
	} {
		if !hasHangul(ko) {
			t.Errorf("원문이 한국어가 아닙니다: %q", ko)
		}
		tr := errCatalog[ko]
		for _, lang := range []string{"en", "zh", "es"} {
			v := tr[lang]
			if v == "" || v == ko {
				t.Errorf("%s 번역이 없습니다: %q", lang, ko)
			}
			if (lang == "en" || lang == "es") && hasHangul(v) {
				t.Errorf("%s 번역에 한글이 남았습니다: %q", lang, v)
			}
		}
	}
	// 코드가 덧붙은 일반 실패 문구도 접두부가 번역된다.
	t.Cleanup(func() { config.CurrentLanguage = nil })
	config.CurrentLanguage = func() string { return "en" }
	if got := trChatGPTMsg(errChatGPTGeneric + " (some_code)"); got != "The ChatGPT request failed (some_code)" {
		t.Fatalf("got %q", got)
	}
}
