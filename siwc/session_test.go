package siwc

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// connectedRecord logs in through the mock issuer and returns the session + store.
func loggedIn(t *testing.T, m *mockIssuer, now func() time.Time) (*Session, *memStore, *Client) {
	t.Helper()
	if now != nil {
		m.clock = now // mint ID tokens on the same (possibly shifted) clock as the client
	}
	c := New(Config{AppName: "ARTEX", Issuer: m.srv.URL, HTTPClient: m.srv.Client(), Now: now})
	p, cb := beginAndCallback(t, c, "")
	m.codeHook(p, "oaiapp_new", "user-1", Scopes, nil)
	rec, err := c.CompleteLogin(context.Background(), p, cb, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := &memStore{}
	s := c.NewSession(st)
	if err := s.Replace(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return s, st, c
}

// refreshHook answers refresh_token grants and records the refresh tokens redeemed.
func (m *mockIssuer) refreshHook(sub string, over map[string]any, withID bool) *[]string {
	var seen []string
	m.onRefresh = func(form url.Values) map[string]any {
		m.mu.Lock()
		seen = append(seen, form.Get("refresh_token"))
		m.mu.Unlock()
		if form.Get("client_id") != "oaiapp_new" || form.Get("resource") != Resource || form.Has("scope") {
			return map[string]any{"_error": "invalid_request"}
		}
		a, r := m.nextTokens("")
		resp := map[string]any{"access_token": a, "refresh_token": r, "token_type": "Bearer", "expires_in": 3600}
		if withID {
			resp["id_token"] = m.idToken("oaiapp_new", sub, "", over)
		}
		return resp
	}
	return &seen
}

func TestSessionServesCachedTokenWithoutRefreshing(t *testing.T) {
	m := newMockIssuer(t)
	s, _, _ := loggedIn(t, m, nil)
	for i := 0; i < 5; i++ {
		tok, err := s.Token(context.Background(), "")
		if err != nil || tok != "access-1" {
			t.Fatalf("%q %v", tok, err)
		}
	}
	if n := len(m.reqs()); n != 1 { // only the code exchange
		t.Fatalf("unexpected token requests: %d", n)
	}
}

func TestSessionRefreshesNearExpiryAndPersistsRotation(t *testing.T) {
	m := newMockIssuer(t)
	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	s, st, _ := loggedIn(t, m, now)
	seen := m.refreshHook("user-1", nil, true)

	offset.Store(int64(59*time.Minute + 30*time.Second)) // inside the 1 minute skew window
	tok, err := s.Token(context.Background(), "")
	if err != nil || tok != "access-2" {
		t.Fatalf("%q %v", tok, err)
	}
	if len(*seen) != 1 || (*seen)[0] != "refresh-1" {
		t.Fatalf("redeemed %v", *seen)
	}
	rec := st.rec
	if rec.RefreshToken != "refresh-2" || rec.AccessToken != "access-2" || rec.PendingIDToken != "" || rec.IDToken == "" ||
		rec.Status != StatusConnected || !rec.HasPlanUsage() {
		t.Fatalf("persisted record: %+v", rec)
	}
	// scopes kept although the refresh response omitted `scope`
	if len(rec.Scopes) == 0 {
		t.Fatal("scopes lost")
	}
	// the rotation was persisted BEFORE verification finished (pending checkpoint exists in history)
	sawPending := false
	for _, r := range st.saves {
		if r.PendingIDToken != "" && r.RefreshToken == "refresh-2" {
			sawPending = true
		}
	}
	if !sawPending {
		t.Fatal("rotated credentials must be checkpointed before identity verification")
	}
	// now fresh again: no further refresh
	if tok, _ := s.Token(context.Background(), ""); tok != "access-2" || len(*seen) != 1 {
		t.Fatalf("unexpected refresh: %q %v", tok, *seen)
	}
}

func TestSessionConcurrentCallersRefreshOnce(t *testing.T) {
	m := newMockIssuer(t)
	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	s, _, _ := loggedIn(t, m, now)
	seen := m.refreshHook("user-1", nil, true)
	offset.Store(int64(time.Hour))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tok, err := s.Token(context.Background(), ""); err != nil || tok != "access-2" {
				t.Errorf("%q %v", tok, err)
			}
		}()
	}
	wg.Wait()
	if len(*seen) != 1 {
		t.Fatalf("refresh token redeemed %d times: %v", len(*seen), *seen)
	}
}

func TestSessionRefreshesOnceAfter401(t *testing.T) {
	m := newMockIssuer(t)
	s, _, _ := loggedIn(t, m, nil)
	seen := m.refreshHook("user-1", nil, false) // no id_token on refresh is allowed
	tok, err := s.Token(context.Background(), "access-1")
	if err != nil || tok != "access-2" {
		t.Fatalf("%q %v", tok, err)
	}
	// a second caller that also got a 401 with the old token must reuse the new one
	tok, err = s.Token(context.Background(), "access-1")
	if err != nil || tok != "access-2" || len(*seen) != 1 {
		t.Fatalf("%q %v %v", tok, err, *seen)
	}
	// a 401 on the NEW token refreshes again
	if tok, _ = s.Token(context.Background(), "access-2"); tok != "access-3" || len(*seen) != 2 {
		t.Fatalf("%q %v", tok, *seen)
	}
}

func TestSessionRefreshDeadMarksReauthAndClearsTokens(t *testing.T) {
	for _, code := range []string{"invalid_grant", "refresh_token_reused", "refresh_token_expired", "refresh_token_invalidated", "invalid_refresh_token", "token_expired"} {
		t.Run(code, func(t *testing.T) {
			m := newMockIssuer(t)
			s, st, _ := loggedIn(t, m, nil)
			m.onRefresh = func(url.Values) map[string]any { return map[string]any{"_error": code} }
			if _, err := s.Token(context.Background(), "access-1"); CodeOf(err) != CodeReauthRequired {
				t.Fatalf("got %v", err)
			}
			r := st.rec
			if r.Status != StatusReauth || r.AccessToken != "" || r.RefreshToken != "" || r.IDToken != "" || r.ClientID != "oaiapp_new" || r.Subject != "user-1" || r.LastError != code {
				t.Fatalf("record: %+v", r)
			}
			before := len(m.reqs())
			if _, err := s.Token(context.Background(), ""); CodeOf(err) != CodeReauthRequired {
				t.Fatalf("second call: %v", err)
			}
			if len(m.reqs()) != before {
				t.Fatal("a dead session must not hit the network again")
			}
		})
	}
}

func TestSessionTransientRefreshFailureKeepsCredentials(t *testing.T) {
	m := newMockIssuer(t)
	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	s, st, _ := loggedIn(t, m, now)
	m.tokenStatus, m.tokenBody = 503, `{"error":"server_error"}`
	offset.Store(int64(59*time.Minute + 30*time.Second))
	// still-valid token: serve it despite the failed proactive refresh
	if tok, err := s.Token(context.Background(), ""); err != nil || tok != "access-1" {
		t.Fatalf("%q %v", tok, err)
	}
	// expired token: surface the (retryable) error, keep everything
	offset.Store(int64(2 * time.Hour))
	if _, err := s.Token(context.Background(), ""); err == nil || CodeOf(err) == CodeReauthRequired {
		t.Fatalf("got %v", err)
	}
	if st.rec.Status != StatusConnected || st.rec.RefreshToken != "refresh-1" {
		t.Fatalf("credentials must survive a 5xx: %+v", st.rec)
	}
	// recovery
	m.tokenStatus = 0
	m.clock = now // ID tokens are minted on the same (shifted) clock
	m.refreshHook("user-1", nil, true)
	if tok, err := s.Token(context.Background(), ""); err != nil || tok != "access-2" {
		t.Fatalf("%q %v", tok, err)
	}
}

func TestSessionPendingIdentityResumesAfterJWKSOutage(t *testing.T) {
	m := newMockIssuer(t)
	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	s, st, c := loggedIn(t, m, now)
	seen := m.refreshHook("user-1", nil, true)
	// drop the cached keys, then take the key service down
	c.keysMu.Lock()
	c.keys = nil
	c.keysMu.Unlock()
	m.jwksStatus = 503
	offset.Store(int64(time.Hour))

	_, err := s.Token(context.Background(), "")
	if CodeOf(err) != CodeIdentityUnavailable {
		t.Fatalf("got %v", err)
	}
	if st.rec.PendingIDToken == "" || st.rec.RefreshToken != "refresh-2" || st.rec.Status != StatusConnected {
		t.Fatalf("checkpoint missing: %+v", st.rec)
	}
	// service back: verification completes WITHOUT redeeming another refresh token
	m.jwksStatus = 0
	tok, err := s.Token(context.Background(), "")
	if err != nil || tok != "access-2" || len(*seen) != 1 {
		t.Fatalf("%q %v %v", tok, err, *seen)
	}
	if st.rec.PendingIDToken != "" {
		t.Fatal("pending token not cleared")
	}
}

func TestSessionRefreshWithDifferentAccountForcesReauth(t *testing.T) {
	m := newMockIssuer(t)
	s, st, _ := loggedIn(t, m, nil)
	m.refreshHook("someone-else", nil, true)
	if _, err := s.Token(context.Background(), "access-1"); CodeOf(err) != CodeReauthRequired {
		t.Fatalf("got %v", err)
	}
	if st.rec.Status != StatusReauth || st.rec.AccessToken != "" || st.rec.LastError != CodeAccountMismatch {
		t.Fatalf("%+v", st.rec)
	}
}

func TestSessionRefreshWithForgedIDTokenForcesReauth(t *testing.T) {
	m := newMockIssuer(t)
	s, st, _ := loggedIn(t, m, nil)
	m.refreshHook("user-1", map[string]any{"aud": "oaiapp_other"}, true)
	if _, err := s.Token(context.Background(), "access-1"); CodeOf(err) != CodeReauthRequired {
		t.Fatalf("got %v", err)
	}
	if st.rec.Status != StatusReauth || st.rec.LastError != CodeInvalidIDToken {
		t.Fatalf("%+v", st.rec)
	}
}

func TestSessionRefreshDroppingPlanScopeBlocksInference(t *testing.T) {
	m := newMockIssuer(t)
	s, _, _ := loggedIn(t, m, nil)
	m.onRefresh = func(url.Values) map[string]any {
		a, r := m.nextTokens("")
		return map[string]any{"access_token": a, "refresh_token": r, "token_type": "Bearer", "expires_in": 3600,
			"scope": "openid profile email offline_access"}
	}
	if _, err := s.Token(context.Background(), "access-1"); CodeOf(err) != CodePlanUsageDisabled {
		t.Fatalf("got %v", err)
	}
}

func TestSessionEarliestRefreshAt(t *testing.T) {
	m := newMockIssuer(t)
	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	c := New(Config{Issuer: m.srv.URL, HTTPClient: m.srv.Client(), Now: now})
	st := &memStore{}
	s := c.NewSession(st)
	_ = s.Replace(context.Background(), &Record{
		ClientID: "oaiapp_new", Subject: "user-1", AccessToken: "old", RefreshToken: "r", Status: StatusConnected,
		ExpiresAt: time.Now().Add(30 * time.Second), EarliestRefreshAt: time.Now().Add(10 * time.Minute),
		Scopes: strings.Fields(Scopes),
	})
	m.refreshHook("user-1", nil, false)
	// inside the skew window but the server refuses early refresh: keep serving the valid token
	if tok, err := s.Token(context.Background(), ""); err != nil || tok != "old" {
		t.Fatalf("%q %v", tok, err)
	}
	if len(m.reqs()) != 0 {
		t.Fatal("must not refresh before earliest_refresh_at")
	}
	// expired and still not allowed: retryable error, no request
	offset.Store(int64(time.Minute))
	if _, err := s.Token(context.Background(), ""); CodeOf(err) != CodeRefreshNotReady {
		t.Fatalf("got %v", err)
	}
	offset.Store(int64(11 * time.Minute))
	if tok, err := s.Token(context.Background(), ""); err != nil || tok != "access-1" {
		t.Fatalf("%q %v", tok, err)
	}
}

func TestSessionNotConnected(t *testing.T) {
	m := newMockIssuer(t)
	c := m.client()
	s := c.NewSession(&memStore{})
	if _, err := s.Token(context.Background(), ""); CodeOf(err) != CodeNotConnected {
		t.Fatalf("got %v", err)
	}
}

func TestSessionLogoutRevokesAndKeepsRegistration(t *testing.T) {
	m := newMockIssuer(t)
	s, st, _ := loggedIn(t, m, nil)
	revoked, err := s.Logout(context.Background())
	if err != nil || !revoked {
		t.Fatalf("%v %v", revoked, err)
	}
	if len(m.revokes) != 1 || m.revokes[0].Get("token") != "refresh-1" || m.revokes[0].Get("token_type_hint") != "refresh_token" ||
		m.revokes[0].Get("client_id") != "oaiapp_new" {
		t.Fatalf("revocation request: %v", m.revokes)
	}
	r := st.rec
	if r.AccessToken != "" || r.RefreshToken != "" || r.IDToken != "" || r.Status != StatusDisconnected ||
		r.ClientID != "oaiapp_new" || r.Subject != "user-1" || r.Email == "" {
		t.Fatalf("record after logout: %+v", r)
	}
	if _, err := s.Token(context.Background(), ""); CodeOf(err) != CodeNotConnected {
		t.Fatalf("got %v", err)
	}
}

func TestSessionLogoutClearsLocallyEvenWhenRevocationFails(t *testing.T) {
	m := newMockIssuer(t)
	s, st, _ := loggedIn(t, m, nil)
	m.revokeStatus = 500
	revoked, err := s.Logout(context.Background())
	if err != nil || revoked {
		t.Fatalf("revoked=%v err=%v (must report unconfirmed revocation)", revoked, err)
	}
	if st.rec.RefreshToken != "" || st.rec.AccessToken != "" {
		t.Fatalf("tokens must be cleared locally anyway: %+v", st.rec)
	}
	if len(m.revokes) != 2 {
		t.Fatalf("5xx should be retried once, got %d attempts", len(m.revokes))
	}
}
