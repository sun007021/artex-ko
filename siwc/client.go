package siwc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/llmauth"
)

// Documented endpoints and parameters.
const (
	DefaultIssuer   = "https://auth.openai.com"
	authorizePath   = "/api/accounts/authorize"
	tokenPath       = "/api/accounts/oauth/token"
	discoveryPath   = "/.well-known/openid-configuration"
	Resource        = "https://api.openai.com/v1"
	DynamicClientID = "dynamic_agent_client"
	// CallbackPath is fixed: on later sign-ins only the PORT of the loopback redirect
	// URI may change, scheme/host/path must stay identical ("/callback" does not
	// match "/auth/callback").
	CallbackPath = "/auth/callback"

	// ScopePlanUsage is what authorizes plan-billed inference. A valid ID token
	// alone does not (sign-in.md: "A valid ID token does not authorize plan usage").
	ScopePlanUsage = llmauth.ScopePlanUsage
)

// Scopes is the full scope set requested on every authorization.
const Scopes = "openid profile email offline_access resource.invoke " + ScopePlanUsage

// Status values stored with a Record (identical to the db column values).
const (
	StatusDisconnected = llmauth.StatusDisconnected
	StatusConnected    = llmauth.StatusConnected
	StatusReauth       = llmauth.StatusReauth
)

// Config configures a Client.
type Config struct {
	// AppName is the display name hint sent on first registration only. Use the same
	// name on every installation. It is metadata, not identity.
	AppName string
	// Issuer defaults to DefaultIssuer. Tests point it at a mock server.
	Issuer string
	// HTTPClient is used for every request to the issuer (token, JWKS, revocation).
	// Redirects are never followed. nil = a default client with timeouts.
	HTTPClient *http.Client
	// Now is a test hook; nil = time.Now.
	Now func() time.Time
	// Logf receives sanitized diagnostics (never tokens); nil = silent.
	Logf func(format string, args ...any)
}

// Client talks to the SiwC authorization server. Safe for concurrent use.
type Client struct {
	appName string
	issuer  string
	hc      *http.Client
	now     func() time.Time
	logf    func(string, ...any)

	discMu sync.Mutex
	disc   *discovery
	discAt time.Time
	keysMu sync.Mutex
	keys   map[string]any // kid -> *rsa.PublicKey
	keysAt time.Time
}

type discovery struct {
	Issuer     string `json:"issuer"`
	JWKSURI    string `json:"jwks_uri"`
	Revocation string `json:"revocation_endpoint"`
}

// New builds a Client.
func New(cfg Config) *Client {
	c := &Client{appName: strings.TrimSpace(cfg.AppName), issuer: strings.TrimRight(cfg.Issuer, "/"), now: cfg.Now, logf: cfg.Logf}
	if c.issuer == "" {
		c.issuer = DefaultIssuer
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.logf == nil {
		c.logf = func(string, ...any) {}
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	// Never follow redirects: a redirect could forward form bodies (codes, refresh
	// tokens) to another origin.
	cp := *hc
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.hc = &cp
	return c
}

// NewHostID returns a fresh persistent-installation host identifier in the
// documented "urn:uuid:<uuidv4>" form. Generate it once per host, persist it and
// reuse it for every sign-in. It is an opaque value, not a credential.
func NewHostID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randomValue() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// ValidateRedirectURI enforces the documented loopback shape:
// http://127.0.0.1:<port>/auth/callback (not "localhost", no query/fragment).
func ValidateRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" ||
		u.Path != CallbackPath || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return newErr(CodeInvalidRedirectURI, false)
	}
	if n, err := strconv.Atoi(u.Port()); err != nil || n < 1 || n > 65535 {
		return newErr(CodeInvalidRedirectURI, false)
	}
	return nil
}

// RedirectURIForPort builds the loopback redirect URI for a port.
func RedirectURIForPort(port int) string {
	return "http://127.0.0.1:" + strconv.Itoa(port) + CallbackPath
}

// Pending is the state of one in-flight authorization attempt. state, nonce and
// the PKCE verifier are fresh per attempt and must be kept until the callback
// completes. Pending holds secrets (the verifier): never log or return it.
type Pending struct {
	State       string
	Nonce       string
	Verifier    string
	RedirectURI string
	// ClientID is the saved issued client id ("" on first registration, which uses
	// the dynamic client).
	ClientID  string
	CreatedAt time.Time
}

// BeginOptions parameterizes BeginLogin.
type BeginOptions struct {
	RedirectURI string // from RedirectURIForPort
	HostID      string // persistent ext_agent_host_id (required)
	ClientID    string // saved issued client id; "" = first-time registration
	// IDTokenHint is the retained ID token of this account (reauthorization only; it
	// may be expired). LoginHint is the account email. Both must refer to the same
	// account; omit them after sign-out when no token was kept.
	IDTokenHint string
	LoginHint   string
	// Reconsent adds prompt=consent (an OAuth authorization parameter, not the
	// Responses `prompt` field) to re-request plan-usage consent. Use only on an
	// explicit user choice, never on every sign-in.
	Reconsent bool
}

// BeginLogin creates a fresh attempt and returns the URL to open in the system
// browser. The returned URL may contain id_token_hint and must be redacted from
// logs (use RedactAuthURL).
func (c *Client) BeginLogin(o BeginOptions) (string, *Pending, error) {
	if err := ValidateRedirectURI(o.RedirectURI); err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(o.HostID) == "" {
		return "", nil, fmt.Errorf("siwc: host id required")
	}
	if o.ClientID == DynamicClientID {
		return "", nil, fmt.Errorf("siwc: the dynamic client id must not be saved or reused")
	}
	p := &Pending{
		State: randomValue(), Nonce: randomValue(), Verifier: randomValue(),
		RedirectURI: o.RedirectURI, ClientID: o.ClientID, CreatedAt: c.now(),
	}
	clientID := o.ClientID
	if clientID == "" {
		clientID = DynamicClientID
	}
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", o.RedirectURI)
	q.Set("scope", Scopes)
	q.Set("resource", Resource)
	q.Set("state", p.State)
	q.Set("nonce", p.Nonce)
	q.Set("code_challenge_method", "S256")
	sum := sha256.Sum256([]byte(p.Verifier))
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("ext_agent_host_id", o.HostID)
	if o.ClientID == "" && c.appName != "" {
		q.Set("agent_name_hint", c.appName) // initial registration only
	}
	if o.IDTokenHint != "" {
		q.Set("id_token_hint", o.IDTokenHint)
	}
	if o.LoginHint != "" {
		q.Set("login_hint", o.LoginHint)
	}
	if o.Reconsent {
		q.Set("prompt", "consent")
	}
	return c.issuer + authorizePath + "?" + q.Encode(), p, nil
}

var idTokenHintRe = regexp.MustCompile(`([?&]id_token_hint=)[^&]*`)

// RedactAuthURL masks id_token_hint (a token) so an authorization URL can be logged.
func RedactAuthURL(u string) string { return idTokenHintRe.ReplaceAllString(u, "${1}REDACTED") }

// Callback is the validated result of the browser redirect.
type Callback struct {
	Code     string
	ClientID string // issued client id (returned on registration; else the saved one)
}

var clientIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,200}$`)

// ParseCallback validates the redirect for pending attempt p. raw may be the full
// redirected URL (http://127.0.0.1:PORT/auth/callback?code=...&state=...), a
// path+query, or just the query string — which is what the "paste the final URL"
// fallback needs. State is validated first (constant time), then OAuth errors.
func (c *Client) ParseCallback(p *Pending, raw string) (*Callback, error) {
	q, err := callbackQuery(raw)
	if err != nil {
		return nil, newErr(CodeInvalidState, false)
	}
	states := q["state"]
	if len(states) != 1 || subtle.ConstantTimeCompare([]byte(states[0]), []byte(p.State)) != 1 {
		return nil, newErr(CodeInvalidState, false)
	}
	if e := q.Get("error"); e != "" {
		code := safeIdentifier(e)
		if code == "" {
			code = "api_error"
		}
		return nil, &Error{Code: code} // access_denied etc.: do not exchange a code
	}
	codes := q["code"]
	if len(codes) != 1 || codes[0] == "" {
		return nil, newErr(CodeRegistrationIncomplete, false)
	}
	returned := q["client_id"]
	if len(returned) > 1 {
		return nil, newErr(CodeRegistrationIncomplete, false)
	}
	clientID := p.ClientID
	if len(returned) == 1 && returned[0] != "" {
		// Reauthorization: a different id than the one tied to the pending request is rejected.
		if p.ClientID != "" && returned[0] != p.ClientID {
			return nil, newErr(CodeRegistrationIncomplete, false)
		}
		clientID = returned[0]
	}
	// A new registration must come back with an issued id; "dynamic_agent_client" is never one.
	if clientID == "" || clientID == DynamicClientID || !clientIDRe.MatchString(clientID) {
		return nil, newErr(CodeRegistrationIncomplete, false)
	}
	return &Callback{Code: codes[0], ClientID: clientID}, nil
}

func callbackQuery(raw string) (url.Values, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty")
	}
	if i := strings.Index(raw, "#"); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.Index(raw, "?"); i >= 0 {
		raw = raw[i+1:]
	} else if strings.Contains(raw, "://") || strings.HasPrefix(raw, "/") {
		return nil, fmt.Errorf("no query")
	}
	return url.ParseQuery(raw)
}

// ---- token endpoint ----

// TokenSet is a parsed token-endpoint response.
type TokenSet struct {
	AccessToken       string
	RefreshToken      string
	IDToken           string
	Scopes            []string
	ExpiresAt         time.Time
	EarliestRefreshAt time.Time
	ReceivedAt        time.Time
}

// HasScope reports whether scope was granted.
func (t *TokenSet) HasScope(scope string) bool { return hasScope(t.Scopes, scope) }

func hasScope(scopes []string, want string) bool { return llmauth.HasScope(scopes, want) }

// HasPlanUsage reports whether chatgpt.tokens.use.direct was granted.
func (t *TokenSet) HasPlanUsage() bool { return t.HasScope(ScopePlanUsage) }

func (c *Client) tokenRequest(ctx context.Context, form url.Values) (map[string]json.RawMessage, error) {
	resp, body, err := c.do(ctx, http.MethodPost, c.issuer+tokenPath, form)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, ParseAPIError(resp.StatusCode, body, resp.Header.Get("x-request-id"))
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return nil, newErr(CodeInvalidTokenResponse, false)
	}
	return m, nil
}

// do performs a form POST (or GET when form == nil) and reads at most 4 MiB. Errors
// are sanitized: a transport error never exposes the request body.
func (c *Client) do(ctx context.Context, method, rawURL string, form url.Values) (*http.Response, []byte, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, nil, newErr(CodeNetwork, true)
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, newErr(CodeNetwork, true)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, newErr(CodeNetwork, true)
	}
	return resp, b, nil
}

func jsonString(m map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(m[k], &s)
	return s
}

// parseTokenSet validates a token response. fallbackScopes is used when a refresh
// response omits `scope` (OAuth allows it when unchanged).
func (c *Client) parseTokenSet(m map[string]json.RawMessage, fallbackScopes []string, requireRefresh bool) (*TokenSet, error) {
	ts := &TokenSet{ReceivedAt: c.now()}
	scope := jsonString(m, "scope")
	if scope == "" && fallbackScopes != nil {
		ts.Scopes = append([]string(nil), fallbackScopes...)
	} else {
		ts.Scopes = strings.Fields(scope)
	}
	if len(ts.Scopes) == 0 {
		return nil, newErr(CodeInvalidTokenResponse, false) // permissions not confirmed
	}
	ts.AccessToken = jsonString(m, "access_token")
	ts.RefreshToken = jsonString(m, "refresh_token")
	ts.IDToken = jsonString(m, "id_token")
	if ts.AccessToken == "" || !strings.EqualFold(jsonString(m, "token_type"), "bearer") {
		return nil, newErr(CodeInvalidTokenResponse, false)
	}
	var expiresIn float64
	if json.Unmarshal(m["expires_in"], &expiresIn) != nil || expiresIn <= 0 {
		return nil, newErr(CodeInvalidTokenResponse, false)
	}
	ts.ExpiresAt = ts.ReceivedAt.Add(time.Duration(expiresIn * float64(time.Second)))
	if (requireRefresh || hasScope(ts.Scopes, "offline_access")) && ts.RefreshToken == "" {
		return nil, newErr(CodeInvalidTokenResponse, false)
	}
	ts.EarliestRefreshAt = parseEarliest(m["earliest_refresh_at"])
	return ts, nil
}

// parseEarliest accepts a unix-seconds number, a numeric string, or RFC 3339.
func parseEarliest(raw json.RawMessage) time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return time.Unix(int64(n), 0)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return time.Unix(int64(f), 0)
		}
	}
	return time.Time{}
}

// Exchange trades the authorization code for tokens and verifies the ID token
// (signature, issuer, audience = issued client id, expiry, nonce). It does NOT
// check the granted scopes against plan usage — see TokenSet.HasPlanUsage.
func (c *Client) Exchange(ctx context.Context, p *Pending, cb *Callback) (*TokenSet, *Identity, error) {
	m, err := c.tokenRequest(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {cb.ClientID},
		"code":          {cb.Code},
		"code_verifier": {p.Verifier},
		"redirect_uri":  {p.RedirectURI},
		"resource":      {Resource},
	})
	if err != nil {
		return nil, nil, err
	}
	ts, err := c.parseTokenSet(m, nil, false)
	if err != nil {
		return nil, nil, err
	}
	if ts.IDToken == "" {
		c.discardRefreshToken(ctx, cb.ClientID, ts.RefreshToken)
		return nil, nil, newErr(CodeInvalidIDToken, false)
	}
	id, err := c.VerifyIDToken(ctx, ts.IDToken, cb.ClientID, p.Nonce, ts.ReceivedAt)
	if err != nil {
		// The identity could not be established, so these credentials will never be
		// used: do not leave the grant alive at the issuer.
		c.discardRefreshToken(ctx, cb.ClientID, ts.RefreshToken)
		return nil, nil, err
	}
	return ts, id, nil
}

// Refresh redeems a refresh token (standard OAuth, near expiry). The refresh
// token rotates: the caller must store the new one. Omitting `scope` keeps the
// existing grant. The returned set's ID token (if any) is NOT yet verified.
func (c *Client) Refresh(ctx context.Context, clientID, refreshToken string, prevScopes []string) (*TokenSet, error) {
	if clientID == "" || clientID == DynamicClientID {
		return nil, newErr(CodeInvalidClient, false)
	}
	m, err := c.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
		"resource":      {Resource},
	})
	if err != nil {
		return nil, err
	}
	return c.parseTokenSet(m, prevScopes, true)
}

// Revoke revokes a refresh token at the issuer's revocation_endpoint (found in the
// OpenID configuration). An empty 200 means success, including when the token
// was already invalid. 5xx / network failures are retried once.
func (c *Client) Revoke(ctx context.Context, clientID, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	d, err := c.getDiscovery(ctx)
	if err != nil || d.Revocation == "" {
		return newErr(CodeDiscoveryFailed, true)
	}
	for attempt := 0; attempt < 2; attempt++ {
		resp, _, err := c.do(ctx, http.MethodPost, d.Revocation, url.Values{
			"token": {refreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {clientID},
		})
		if err == nil && resp.StatusCode == http.StatusOK {
			return nil
		}
		retry := (err != nil && ctx.Err() == nil) || (err == nil && resp.StatusCode >= 500)
		if !retry {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return newErr("revocation_failed", true)
}

// ---- discovery ----

const discoveryTTL = time.Hour

func sameOrigin(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && ua.Scheme == ub.Scheme && ua.Host == ub.Host && ua.Host != ""
}

func (c *Client) getDiscovery(ctx context.Context) (*discovery, error) {
	c.discMu.Lock()
	defer c.discMu.Unlock()
	if c.disc != nil && c.now().Sub(c.discAt) < discoveryTTL {
		return c.disc, nil
	}
	resp, body, err := c.do(ctx, http.MethodGet, c.issuer+discoveryPath, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, newErr(CodeDiscoveryFailed, true)
	}
	var d discovery
	if json.Unmarshal(body, &d) != nil || d.Issuer != c.issuer || d.JWKSURI == "" ||
		!sameOrigin(d.JWKSURI, c.issuer) || (d.Revocation != "" && !sameOrigin(d.Revocation, c.issuer)) {
		return nil, newErr(CodeDiscoveryFailed, false)
	}
	c.disc, c.discAt = &d, c.now()
	return c.disc, nil
}
