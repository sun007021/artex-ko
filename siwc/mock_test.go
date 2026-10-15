package siwc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// mockIssuer is an in-process stand-in for auth.openai.com: OpenID discovery,
// JWKS, token endpoint (authorization_code + refresh_token) and revocation.
type mockIssuer struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string

	mu        sync.Mutex
	tokenReqs []url.Values
	revokes   []url.Values
	jwksHits  int
	// behaviour switches
	jwksStatus    int // 0 = 200
	tokenStatus   int // when != 0, token endpoint returns this with tokenBody
	tokenBody     string
	revokeStatus  int
	tokenRedirect bool
	// per-grant hooks
	onCode    func(form url.Values) map[string]any
	onRefresh func(form url.Values) map[string]any
	challenge string // PKCE challenge the test registered for the next code exchange
	accessN   int
	refreshN  int
	clock     func() time.Time
}

func newMockIssuer(t *testing.T) *mockIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &mockIssuer{t: t, key: key, kid: "k1", clock: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": m.srv.URL, "authorization_endpoint": m.srv.URL + "/api/accounts/authorize",
			"token_endpoint": m.srv.URL + "/api/accounts/oauth/token", "jwks_uri": m.srv.URL + "/jwks",
			"revocation_endpoint": m.srv.URL + "/revoke",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.jwksHits++
		st := m.jwksStatus
		m.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			return
		}
		pub := m.key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": m.kid, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/api/accounts/oauth/token", m.token)
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		m.mu.Lock()
		m.revokes = append(m.revokes, r.PostForm)
		st := m.revokeStatus
		m.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
		}
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockIssuer) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	form := r.PostForm
	m.mu.Lock()
	m.tokenReqs = append(m.tokenReqs, form)
	redirect, st, body := m.tokenRedirect, m.tokenStatus, m.tokenBody
	m.mu.Unlock()
	if redirect {
		http.Redirect(w, r, "https://evil.example/steal", http.StatusFound)
		return
	}
	if st != 0 {
		w.WriteHeader(st)
		_, _ = w.Write([]byte(body))
		return
	}
	var resp map[string]any
	switch form.Get("grant_type") {
	case "authorization_code":
		if m.onCode == nil {
			http.Error(w, "no hook", 500)
			return
		}
		resp = m.onCode(form)
	case "refresh_token":
		if m.onRefresh == nil {
			http.Error(w, "no hook", 500)
			return
		}
		resp = m.onRefresh(form)
	default:
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"unsupported_grant_type"}`))
		return
	}
	if e, ok := resp["_error"]; ok {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": e})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// idToken signs an ID token with the mock key. over lets a test tamper claims.
func (m *mockIssuer) idToken(clientID, sub, nonce string, over map[string]any) string {
	return m.idTokenWith(m.key, clientID, sub, nonce, over)
}

func (m *mockIssuer) idTokenWith(key *rsa.PrivateKey, clientID, sub, nonce string, over map[string]any) string {
	now := m.clock()
	claims := jwt.MapClaims{
		"iss": m.srv.URL, "aud": clientID, "sub": sub, "email": sub + "@example.com",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	for k, v := range over {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = m.kid
	s, err := tok.SignedString(key)
	if err != nil {
		m.t.Fatal(err)
	}
	return s
}

func (m *mockIssuer) nextTokens(scope string) (access, refresh string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accessN++
	m.refreshN++
	return "access-" + itoa(m.accessN), "refresh-" + itoa(m.refreshN)
}

func itoa(n int) string { return big.NewInt(int64(n)).String() }

func (m *mockIssuer) reqs() []url.Values {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]url.Values(nil), m.tokenReqs...)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (m *mockIssuer) client() *Client {
	return New(Config{AppName: "ARTEX", Issuer: m.srv.URL, HTTPClient: m.srv.Client()})
}

// memStore is an in-memory Store that remembers every saved record.
type memStore struct {
	mu    sync.Mutex
	rec   *Record
	saves []*Record
}

func (s *memStore) Load(_ context.Context) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.clone(), nil
}

func (s *memStore) Save(_ context.Context, r *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec = r.clone()
	s.saves = append(s.saves, r.clone())
	return nil
}
