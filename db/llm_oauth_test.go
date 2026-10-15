package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func openOAuthTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func chatgptTestProfile(t *testing.T, d *DB, name string) int64 {
	t.Helper()
	id, err := d.SaveProfile(&LLMProfile{
		Name: name, Format: "openai", Model: "gpt-x", AuthMode: AuthModeChatGPT,
		BaseURL: "https://evil.example/v1", APIKey: "sk-should-be-dropped",
		ReasoningEffort: "high", Streaming: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	return id
}

// 구독 프로필은 저장 시 format/base_url/api_key 가 정규화되고, 기본 프로필의 auth_mode 는 ""다.
func TestProfileAuthModeNormalizedAndRoundTrip(t *testing.T) {
	d := openOAuthTestDB(t)
	id := chatgptTestProfile(t, d, "t-oauth-norm")

	p, err := d.ProfileByID(id)
	if err != nil || p == nil {
		t.Fatalf("ProfileByID: %v %v", err, p)
	}
	if p.AuthMode != AuthModeChatGPT || p.Format != "openai-responses" || p.BaseURL != "" || p.APIKey != "" {
		t.Fatalf("not normalized: mode=%q format=%q base=%q key=%q", p.AuthMode, p.Format, p.BaseURL, p.APIKey)
	}
	if p.ReasoningEffort != "high" || p.Model != "gpt-x" {
		t.Fatalf("model/effort lost: %+v", p)
	}

	ps, err := d.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range ps {
		if x.ID == id {
			found = x.AuthMode == AuthModeChatGPT
		}
	}
	if !found {
		t.Fatal("list load lost auth_mode")
	}

	// 일반 API Key 프로필은 auth_mode 가 비어 있고 키가 유지된다.
	kid, err := d.SaveProfile(&LLMProfile{Name: "t-oauth-key", Format: "openai", Model: "m", APIKey: "sk-keep-1234", Streaming: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, kid) })
	kp, _ := d.ProfileByID(kid)
	if kp.AuthMode != "" || kp.APIKey != "sk-keep-1234" {
		t.Fatalf("api key profile changed: %+v", kp)
	}

	// 빈 키로 갱신하면 키가 유지된다(기존 동작)...
	kp.APIKey = ""
	if _, err := d.SaveProfile(kp); err != nil {
		t.Fatal(err)
	}
	if kp2, _ := d.ProfileByID(kid); kp2.APIKey != "sk-keep-1234" {
		t.Fatalf("blank-key update dropped the key: %q", kp2.APIKey)
	}
	// ...구독 모드로 전환하면 예전 키와 힌트가 지워진다.
	kp.AuthMode = AuthModeChatGPT
	if _, err := d.SaveProfile(kp); err != nil {
		t.Fatal(err)
	}
	kp3, _ := d.ProfileByID(kid)
	if kp3.APIKey != "" || kp3.AuthMode != AuthModeChatGPT || kp3.Format != "openai-responses" {
		t.Fatalf("switch to chatgpt kept the key: %+v", kp3)
	}
	if hint := listHint(t, d, kid); hint != "" {
		t.Fatalf("api_key_hint kept after switching to chatgpt: %q", hint)
	}

	if _, err := d.SaveProfile(&LLMProfile{Name: "t-oauth-bad", Format: "openai", Model: "m", AuthMode: "magic"}); err == nil {
		t.Fatal("unknown auth_mode must be rejected")
	}
}

func listHint(t *testing.T, d *DB, id int64) string {
	t.Helper()
	ps, err := d.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.ID == id {
			return p.APIKeyHint
		}
	}
	t.Fatalf("profile %d missing", id)
	return ""
}

func TestProfileOAuthRoundTripAndClear(t *testing.T) {
	d := openOAuthTestDB(t)
	ctx := context.Background()
	id := chatgptTestProfile(t, d, "t-oauth-rt")

	if o, err := d.GetProfileOAuth(ctx, id); err != nil || o != nil {
		t.Fatalf("expected no row yet: %v %v", o, err)
	}

	// 등록만 먼저 저장(코드 교환 실패 대비).
	if err := d.SaveProfileOAuthRegistration(ctx, id, "oaiapp_first"); err != nil {
		t.Fatal(err)
	}
	o, _ := d.GetProfileOAuth(ctx, id)
	if o == nil || o.ClientID != "oaiapp_first" || o.Connected() || o.Status != OAuthStatusDisconnected {
		t.Fatalf("registration row wrong: %+v", o)
	}

	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	full := &ProfileOAuth{
		ProfileID: id, ClientID: "oaiapp_first", Subject: "sub-1", Email: "a@example.com",
		IDToken: "idt", AccessToken: "at", RefreshToken: "rt", ExpiresAt: exp,
		Scopes: "openid offline_access chatgpt.tokens.use.direct", Status: OAuthStatusConnected,
	}
	if err := d.SaveProfileOAuth(ctx, full); err != nil {
		t.Fatal(err)
	}
	// 토큰이 있는 행의 client_id 는 등록 저장으로 덮어쓰지 않는다.
	if err := d.SaveProfileOAuthRegistration(ctx, id, "oaiapp_other"); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetProfileOAuth(ctx, id)
	if got.ClientID != "oaiapp_first" || got.AccessToken != "at" || got.RefreshToken != "rt" || got.IDToken != "idt" ||
		!got.ExpiresAt.Equal(exp) || !got.Connected() || got.Subject != "sub-1" {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	sts, err := d.ListProfileOAuthStatuses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st := sts[id]
	if st.Status != OAuthStatusConnected || st.Email != "a@example.com" || !st.Registered || !st.PlanUsageOK {
		t.Fatalf("status wrong: %+v", st)
	}
	// 상태 JSON 에는 토큰이 없다.
	raw, _ := json.Marshal(st)
	for _, secret := range []string{"idt", "\"at\"", "rt\""} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("status leaks %q: %s", secret, raw)
		}
	}
	// ProfileOAuth 자체도 json 으로 토큰을 내보내지 않는다.
	if raw, _ := json.Marshal(got); string(raw) != "{}" {
		t.Fatalf("ProfileOAuth must serialize to {}: %s", raw)
	}

	// 로그아웃: 토큰은 지우고 client_id·계정은 남긴다.
	if err := d.ClearProfileOAuthTokens(ctx, id); err != nil {
		t.Fatal(err)
	}
	cl, _ := d.GetProfileOAuth(ctx, id)
	if cl.AccessToken != "" || cl.RefreshToken != "" || cl.IDToken != "" || !cl.ExpiresAt.IsZero() || cl.Connected() ||
		cl.ClientID != "oaiapp_first" || cl.Subject != "sub-1" || cl.Status != OAuthStatusDisconnected {
		t.Fatalf("clear wrong: %+v", cl)
	}
	// 이제 등록 저장은 client_id 를 갱신할 수 있다.
	if err := d.SaveProfileOAuthRegistration(ctx, id, "oaiapp_second"); err != nil {
		t.Fatal(err)
	}
	if cl, _ = d.GetProfileOAuth(ctx, id); cl.ClientID != "oaiapp_second" {
		t.Fatalf("registration not updated after logout: %q", cl.ClientID)
	}
}

// 프로필을 지우면 OAuth 행도 CASCADE 로 사라지고, 연결된 구독 프로필만 풀에 들어간다.
func TestProfileOAuthCascadeAndPool(t *testing.T) {
	d := openOAuthTestDB(t)
	ctx := context.Background()
	on := chatgptTestProfile(t, d, "t-oauth-pool-on")
	off := chatgptTestProfile(t, d, "t-oauth-pool-off")
	re := chatgptTestProfile(t, d, "t-oauth-pool-reauth")
	for id, st := range map[int64]string{on: OAuthStatusConnected, re: OAuthStatusReauthNeeded} {
		o := &ProfileOAuth{ProfileID: id, ClientID: "oaiapp_x", RefreshToken: "rt", AccessToken: "at", Status: st}
		if st == OAuthStatusReauthNeeded {
			o.RefreshToken = ""
		}
		if err := d.SaveProfileOAuth(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.SaveProfileOAuthRegistration(ctx, off, "oaiapp_y"); err != nil {
		t.Fatal(err)
	}

	chain, err := d.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	in := map[int64]bool{}
	for _, p := range chain {
		in[p.ID] = true
	}
	if !in[on] || in[off] || in[re] {
		t.Fatalf("pool membership wrong: on=%v off=%v reauth=%v", in[on], in[off], in[re])
	}

	if _, err := d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, on); err != nil {
		t.Fatal(err)
	}
	if o, _ := d.GetProfileOAuth(ctx, on); o != nil {
		t.Fatalf("oauth row survived profile delete: %+v", o)
	}
}

func TestStatusWithoutRefreshTokenIsNotConnected(t *testing.T) {
	d := openOAuthTestDB(t)
	ctx := context.Background()
	id := chatgptTestProfile(t, d, "t-oauth-norefresh")
	if err := d.SaveProfileOAuth(ctx, &ProfileOAuth{ProfileID: id, ClientID: "oaiapp_z", Status: OAuthStatusConnected}); err != nil {
		t.Fatal(err)
	}
	sts, _ := d.ListProfileOAuthStatuses(ctx)
	if sts[id].Status != OAuthStatusDisconnected {
		t.Fatalf("status without refresh token = %q", sts[id].Status)
	}
}

func TestEnsureLLMOAuthHostIDIsStable(t *testing.T) {
	d := openOAuthTestDB(t)
	// 이미 값이 있으면(다른 테스트·실서버가 만든 값) 그대로 돌려줘야 한다 — 삭제하지 않는다.
	a, err := d.EnsureLLMOAuthHostID(func() string { return "urn:uuid:first" })
	if err != nil || a == "" {
		t.Fatalf("host id: %q %v", a, err)
	}
	b, err := d.EnsureLLMOAuthHostID(func() string { return "urn:uuid:second" })
	if err != nil || b != a {
		t.Fatalf("host id changed: %q -> %q (%v)", a, b, err)
	}
}
