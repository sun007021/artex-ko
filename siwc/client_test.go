package siwc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestValidateRedirectURI(t *testing.T) {
	ok := []string{"http://127.0.0.1:1455/auth/callback", "http://127.0.0.1:8787/auth/callback"}
	bad := []string{
		"http://localhost:1455/auth/callback",     // must be 127.0.0.1
		"https://127.0.0.1:1455/auth/callback",    // http only
		"http://127.0.0.1/auth/callback",          // port required
		"http://127.0.0.1:1455/callback",          // path must be /auth/callback
		"http://127.0.0.1:1455/auth/callback?x=1", // no query
		"http://127.0.0.1:1455/auth/callback#f",   // no fragment
		"http://127.0.0.1:99999/auth/callback",
		"http://evil.example:1455/auth/callback",
		"",
	}
	for _, u := range ok {
		if err := ValidateRedirectURI(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range bad {
		if err := ValidateRedirectURI(u); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
}

func TestNewHostID(t *testing.T) {
	re := regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	a, b := NewHostID(), NewHostID()
	if !re.MatchString(a) || a == b {
		t.Fatalf("bad host ids %q %q", a, b)
	}
}

func TestBeginLoginFirstRegistration(t *testing.T) {
	m := newMockIssuer(t)
	c := m.client()
	raw, p, err := c.BeginLogin(BeginOptions{RedirectURI: "http://127.0.0.1:8787/auth/callback", HostID: "urn:uuid:h"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	if u.Path != "/api/accounts/authorize" {
		t.Fatalf("path %s", u.Path)
	}
	q := u.Query()
	want := map[string]string{
		"client_id": "dynamic_agent_client", "response_type": "code", "scope": Scopes,
		"resource": "https://api.openai.com/v1", "redirect_uri": "http://127.0.0.1:8787/auth/callback",
		"code_challenge_method": "S256", "ext_agent_host_id": "urn:uuid:h", "agent_name_hint": "ARTEX",
		"state": p.State, "nonce": p.Nonce, "code_challenge": pkceChallenge(p.Verifier),
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	for _, k := range []string{"id_token_hint", "login_hint", "prompt"} {
		if q.Has(k) {
			t.Errorf("unexpected %s", k)
		}
	}
	for _, f := range []string{"openid", "profile", "email", "offline_access", "resource.invoke", "chatgpt.tokens.use.direct"} {
		if !strings.Contains(q.Get("scope"), f) {
			t.Errorf("scope lacks %s", f)
		}
	}
	// fresh values per attempt
	_, p2, _ := c.BeginLogin(BeginOptions{RedirectURI: "http://127.0.0.1:8787/auth/callback", HostID: "urn:uuid:h"})
	if p.State == p2.State || p.Nonce == p2.Nonce || p.Verifier == p2.Verifier {
		t.Fatal("state/nonce/verifier must be fresh per attempt")
	}
}

func TestBeginLoginReauthorization(t *testing.T) {
	c := newMockIssuer(t).client()
	raw, _, err := c.BeginLogin(BeginOptions{
		RedirectURI: "http://127.0.0.1:9000/auth/callback", HostID: "urn:uuid:h", ClientID: "oaiapp_abc",
		IDTokenHint: "hint.jwt.value", LoginHint: "a@example.com", Reconsent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	q := func() url.Values { u, _ := url.Parse(raw); return u.Query() }()
	if q.Get("client_id") != "oaiapp_abc" || q.Has("agent_name_hint") {
		t.Fatalf("reauth must use the saved client and omit agent_name_hint: %v", q)
	}
	if q.Get("id_token_hint") != "hint.jwt.value" || q.Get("login_hint") != "a@example.com" || q.Get("prompt") != "consent" {
		t.Fatalf("hints missing: %v", q)
	}
	if red := RedactAuthURL(raw); strings.Contains(red, "hint.jwt.value") || !strings.Contains(red, "id_token_hint=REDACTED") {
		t.Fatalf("redaction failed: %s", red)
	}
	if _, _, err := c.BeginLogin(BeginOptions{RedirectURI: "http://127.0.0.1:9000/auth/callback", HostID: "h", ClientID: DynamicClientID}); err == nil {
		t.Fatal("the dynamic client id must never be reused as a saved id")
	}
	if _, _, err := c.BeginLogin(BeginOptions{RedirectURI: "http://localhost:9000/auth/callback", HostID: "h"}); err == nil {
		t.Fatal("localhost redirect must be rejected")
	}
	if _, _, err := c.BeginLogin(BeginOptions{RedirectURI: "http://127.0.0.1:9000/auth/callback"}); err == nil {
		t.Fatal("host id is required")
	}
}

func TestParseCallback(t *testing.T) {
	c := newMockIssuer(t).client()
	newP := func(client string) *Pending {
		_, p, _ := c.BeginLogin(BeginOptions{RedirectURI: "http://127.0.0.1:8787/auth/callback", HostID: "h", ClientID: client})
		return p
	}
	t.Run("registration full url", func(t *testing.T) {
		p := newP("")
		cb, err := c.ParseCallback(p, "http://127.0.0.1:8787/auth/callback?code=abc&state="+p.State+"&client_id=oaiapp_new&scope=openid")
		if err != nil || cb.Code != "abc" || cb.ClientID != "oaiapp_new" {
			t.Fatalf("%+v %v", cb, err)
		}
	})
	t.Run("pasted query only and fragment", func(t *testing.T) {
		p := newP("")
		cb, err := c.ParseCallback(p, "  code=abc&state="+p.State+"&client_id=oaiapp_new#frag ")
		if err != nil || cb.Code != "abc" {
			t.Fatalf("%+v %v", cb, err)
		}
	})
	t.Run("reauth keeps saved client", func(t *testing.T) {
		p := newP("oaiapp_saved")
		cb, err := c.ParseCallback(p, "/auth/callback?code=abc&state="+p.State)
		if err != nil || cb.ClientID != "oaiapp_saved" {
			t.Fatalf("%+v %v", cb, err)
		}
		if _, err := c.ParseCallback(p, "?code=abc&state="+p.State+"&client_id=oaiapp_other"); err == nil || CodeOf(err) != CodeRegistrationIncomplete {
			t.Fatalf("different client id must be rejected: %v", err)
		}
	})
	t.Run("rejections", func(t *testing.T) {
		p := newP("")
		cases := map[string]string{
			"wrong state":          "code=abc&state=nope&client_id=oaiapp_x",
			"duplicate state":      "code=abc&state=" + p.State + "&state=" + p.State + "&client_id=oaiapp_x",
			"missing code":         "state=" + p.State + "&client_id=oaiapp_x",
			"no client on new reg": "code=abc&state=" + p.State,
			"dynamic id returned":  "code=abc&state=" + p.State + "&client_id=dynamic_agent_client",
			"bad client chars":     "code=abc&state=" + p.State + "&client_id=a%20b",
			"empty":                "",
			"url without query":    "http://127.0.0.1:8787/auth/callback",
		}
		for name, raw := range cases {
			if _, err := c.ParseCallback(p, raw); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
		_, err := c.ParseCallback(p, "error=access_denied&state="+p.State)
		if CodeOf(err) != CodeAccessDenied {
			t.Errorf("access_denied: %v", err)
		}
		// state is validated BEFORE the error parameter is honoured
		if _, err := c.ParseCallback(p, "error=access_denied&state=forged"); CodeOf(err) != CodeInvalidState {
			t.Errorf("forged error callback: %v", err)
		}
	})
}

// codeHook builds a token-endpoint hook that validates the exchange against the
// authorize request that BeginLogin produced.
func (m *mockIssuer) codeHook(p *Pending, clientID, sub string, scope string, over map[string]any) {
	m.onCode = func(form url.Values) map[string]any {
		if form.Get("grant_type") != "authorization_code" || form.Get("client_id") != clientID ||
			form.Get("code") != "the-code" || form.Get("redirect_uri") != p.RedirectURI ||
			form.Get("resource") != Resource || pkceChallenge(form.Get("code_verifier")) != pkceChallenge(p.Verifier) {
			return map[string]any{"_error": "invalid_grant"}
		}
		a, r := m.nextTokens(scope)
		resp := map[string]any{
			"access_token": a, "refresh_token": r, "token_type": "Bearer", "expires_in": 3600,
			"scope": scope, "id_token": m.idToken(clientID, sub, p.Nonce, over),
		}
		return resp
	}
}

func beginAndCallback(t *testing.T, c *Client, client string) (*Pending, *Callback) {
	t.Helper()
	_, p, err := c.BeginLogin(BeginOptions{RedirectURI: "http://127.0.0.1:8787/auth/callback", HostID: "h", ClientID: client})
	if err != nil {
		t.Fatal(err)
	}
	cb, err := c.ParseCallback(p, "code=the-code&state="+p.State+"&client_id=oaiapp_new")
	if err != nil {
		t.Fatal(err)
	}
	return p, cb
}

func TestCompleteLoginSuccess(t *testing.T) {
	m := newMockIssuer(t)
	c := m.client()
	p, cb := beginAndCallback(t, c, "")
	m.codeHook(p, "oaiapp_new", "user-1", Scopes, nil)
	rec, err := c.CompleteLogin(context.Background(), p, cb, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ClientID != "oaiapp_new" || rec.Subject != "user-1" || rec.Email != "user-1@example.com" ||
		rec.AccessToken != "access-1" || rec.RefreshToken != "refresh-1" || rec.Status != StatusConnected ||
		rec.IDToken == "" || !rec.HasPlanUsage() || !rec.Connected() {
		t.Fatalf("record: %+v", rec)
	}
	if d := time.Until(rec.ExpiresAt); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("expiry %v", d)
	}
}

func TestCompleteLoginWithoutPlanUsageScope(t *testing.T) {
	m := newMockIssuer(t)
	c := m.client()
	p, cb := beginAndCallback(t, c, "")
	m.codeHook(p, "oaiapp_new", "user-1", "openid profile email offline_access", nil)
	rec, err := c.CompleteLogin(context.Background(), p, cb, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.HasPlanUsage() {
		t.Fatal("must report that plan usage is not granted")
	}
	st := &memStore{}
	s := c.NewSession(st)
	_ = s.Replace(context.Background(), rec)
	if _, err := s.Token(context.Background(), ""); CodeOf(err) != CodePlanUsageDisabled {
		t.Fatalf("inference must not run without chatgpt.tokens.use.direct: %v", err)
	}
	if len(m.reqs()) != 1 {
		t.Fatal("no refresh should be attempted")
	}
}

func TestCompleteLoginRejectsBadIdentity(t *testing.T) {
	cases := map[string]map[string]any{
		"wrong issuer":   {"iss": "https://evil.example"},
		"wrong audience": {"aud": "oaiapp_someone_else"},
		"expired":        {"exp": time.Now().Add(-time.Hour).Unix()},
		"wrong nonce":    {"nonce": "replayed"},
		"no subject":     {"sub": ""},
		"no iat":         {"iat": nil},
		"wrong azp":      {"azp": "oaiapp_other"},
	}
	for name, over := range cases {
		t.Run(name, func(t *testing.T) {
			m := newMockIssuer(t)
			c := m.client()
			p, cb := beginAndCallback(t, c, "")
			m.codeHook(p, "oaiapp_new", "user-1", Scopes, over)
			if _, err := c.CompleteLogin(context.Background(), p, cb, nil); CodeOf(err) != CodeInvalidIDToken {
				t.Fatalf("got %v", err)
			}
		})
	}
	t.Run("bad signature", func(t *testing.T) {
		m := newMockIssuer(t)
		c := m.client()
		p, cb := beginAndCallback(t, c, "")
		other, _ := rsa.GenerateKey(rand.Reader, 2048)
		m.onCode = func(url.Values) map[string]any {
			a, r := m.nextTokens("")
			return map[string]any{"access_token": a, "refresh_token": r, "token_type": "Bearer", "expires_in": 3600,
				"scope": Scopes, "id_token": m.idTokenWith(other, "oaiapp_new", "user-1", p.Nonce, nil)}
		}
		if _, err := c.CompleteLogin(context.Background(), p, cb, nil); CodeOf(err) != CodeInvalidIDToken {
			t.Fatalf("forged signature accepted: %v", err)
		}
	})
	t.Run("alg none / HS256 are refused", func(t *testing.T) {
		m := newMockIssuer(t)
		c := m.client()
		p, cb := beginAndCallback(t, c, "")
		m.onCode = func(url.Values) map[string]any {
			a, r := m.nextTokens("")
			// header {"alg":"none"} payload with valid-looking claims
			none := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." +
				"eyJpc3MiOiJ4Iiwic3ViIjoiYSJ9."
			return map[string]any{"access_token": a, "refresh_token": r, "token_type": "Bearer", "expires_in": 3600,
				"scope": Scopes, "id_token": none}
		}
		if _, err := c.CompleteLogin(context.Background(), p, cb, nil); CodeOf(err) != CodeInvalidIDToken {
			t.Fatalf("alg none accepted: %v", err)
		}
	})
	t.Run("missing id token", func(t *testing.T) {
		m := newMockIssuer(t)
		c := m.client()
		p, cb := beginAndCallback(t, c, "")
		m.onCode = func(url.Values) map[string]any {
			a, r := m.nextTokens("")
			return map[string]any{"access_token": a, "refresh_token": r, "token_type": "Bearer", "expires_in": 3600, "scope": Scopes}
		}
		if _, err := c.CompleteLogin(context.Background(), p, cb, nil); CodeOf(err) != CodeInvalidIDToken {
			t.Fatalf("%v", err)
		}
	})
}

func TestCompleteLoginJWKSUnavailableIsRetryable(t *testing.T) {
	m := newMockIssuer(t)
	c := m.client()
	p, cb := beginAndCallback(t, c, "")
	m.codeHook(p, "oaiapp_new", "user-1", Scopes, nil)
	m.jwksStatus = 503
	_, err := c.CompleteLogin(context.Background(), p, cb, nil)
	if CodeOf(err) != CodeIdentityUnavailable {
		t.Fatalf("got %v", err)
	}
	if e := err.(*Error); !e.Retryable {
		t.Fatal("must be retryable")
	}
}

func TestJWKSRotationRefetchesOnUnknownKid(t *testing.T) {
	m := newMockIssuer(t)
	c := m.client()
	p, cb := beginAndCallback(t, c, "")
	m.codeHook(p, "oaiapp_new", "user-1", Scopes, nil)
	if _, err := c.CompleteLogin(context.Background(), p, cb, nil); err != nil {
		t.Fatal(err)
	}
	hits := m.jwksHits
	// rotate: new key, new kid
	m.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	m.kid = "k2"
	p2, cb2 := beginAndCallback(t, c, "")
	m.codeHook(p2, "oaiapp_new", "user-1", Scopes, nil)
	if _, err := c.CompleteLogin(context.Background(), p2, cb2, nil); err != nil {
		t.Fatalf("rotated key must be picked up: %v", err)
	}
	if m.jwksHits != hits+1 {
		t.Fatalf("jwks hits %d -> %d", hits, m.jwksHits)
	}
}

func TestCompleteLoginAccountMismatch(t *testing.T) {
	m := newMockIssuer(t)
	c := m.client()
	p, cb := beginAndCallback(t, c, "oaiapp_new")
	m.codeHook(p, "oaiapp_new", "user-2", Scopes, nil)
	prev := &Record{ClientID: "oaiapp_new", Subject: "user-1", Status: StatusDisconnected}
	if _, err := c.CompleteLogin(context.Background(), p, cb, prev); CodeOf(err) != CodeAccountMismatch {
		t.Fatalf("got %v", err)
	}
	// the other account's grant is not left alive at the issuer
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.revokes) != 1 || m.revokes[0].Get("token") != "refresh-1" || m.revokes[0].Get("client_id") != "oaiapp_new" {
		t.Fatalf("the discarded refresh token must be revoked: %v", m.revokes)
	}
}

func TestCompleteLoginRevokesTokensWhenIdentityFails(t *testing.T) {
	m := newMockIssuer(t)
	c := m.client()
	p, cb := beginAndCallback(t, c, "")
	m.codeHook(p, "oaiapp_new", "user-1", Scopes, map[string]any{"aud": "oaiapp_other"})
	if _, err := c.CompleteLogin(context.Background(), p, cb, nil); CodeOf(err) != CodeInvalidIDToken {
		t.Fatalf("got %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.revokes) != 1 || m.revokes[0].Get("token") != "refresh-1" {
		t.Fatalf("revocations: %v", m.revokes)
	}
}

func TestExchangeErrorsAreSanitized(t *testing.T) {
	m := newMockIssuer(t)
	c := m.client()
	p, cb := beginAndCallback(t, c, "")
	m.tokenStatus = 400
	m.tokenBody = `{"error":"invalid_grant","error_description":"code the-code refresh-secret-xyz"}`
	_, err := c.CompleteLogin(context.Background(), p, cb, nil)
	if CodeOf(err) != "invalid_grant" {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), "refresh-secret-xyz") || strings.Contains(err.Error(), "the-code") {
		t.Fatalf("error leaks response content: %v", err)
	}
}

func TestRedirectsAreNeverFollowed(t *testing.T) {
	m := newMockIssuer(t)
	c := m.client()
	p, cb := beginAndCallback(t, c, "")
	m.tokenRedirect = true
	if _, err := c.CompleteLogin(context.Background(), p, cb, nil); err == nil {
		t.Fatal("a redirect from the token endpoint must fail")
	}
}

func TestParseAPIError(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		code      string
		param     string
		retryable bool
	}{
		{400, `{"error":"invalid_grant"}`, "invalid_grant", "", false},
		{429, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"x"}}`, "subscription_sharing_usage_limit_exceeded", "", false},
		{503, `{"error":{"code":"subscription_sharing_usage_unavailable"}}`, "subscription_sharing_usage_unavailable", "", true},
		{400, `{"error":{"code":"subscription_sharing_unsupported_capability","param":"max_output_tokens"}}`, "subscription_sharing_unsupported_capability", "max_output_tokens", false},
		{401, `{"detail":"nope"}`, "api_error", "", false},
		{502, `not json`, "api_error", "", true},
		{400, `{"error":{"code":"bad code with spaces"}}`, "api_error", "", false},
	}
	for _, tc := range cases {
		e := ParseAPIError(tc.status, []byte(tc.body), "req_123")
		if e.Code != tc.code || e.Param != tc.param || e.Retryable != tc.retryable || e.RequestID != "req_123" {
			t.Errorf("%s: %+v", tc.body, e)
		}
	}
	if !IsUsageLimit("subscription_sharing_usage_limit_exceeded") || IsUsageLimit("subscription_sharing_usage_unavailable") {
		t.Fatal("IsUsageLimit")
	}
}
