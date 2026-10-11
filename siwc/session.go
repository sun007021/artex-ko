package siwc

import (
	"context"
	"sync"
	"time"
)

// Record is the persisted credential state of one ChatGPT registration (one
// ARTEX LLM profile). It holds secrets: never log it or return it to a UI.
type Record struct {
	ClientID string
	Subject  string
	Email    string
	// IDToken is the verified ID token retained for id_token_hint on reauthorization.
	IDToken      string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	// EarliestRefreshAt: the server will not accept a refresh before this instant.
	EarliestRefreshAt time.Time
	Scopes            []string
	// PendingIDToken is a refreshed ID token received but not yet verified (JWKS
	// unreachable). The rotated refresh token was already consumed, so the new token
	// set is saved first and identity verification finishes later. The credentials
	// are not used until it does.
	PendingIDToken    string
	PendingReceivedAt time.Time
	Status            string
	LastError         string // sanitized code of the last terminal failure
}

// HasPlanUsage reports whether chatgpt.tokens.use.direct was granted.
func (r *Record) HasPlanUsage() bool { return r != nil && hasScope(r.Scopes, ScopePlanUsage) }

// Connected reports whether the record holds a renewable session.
func (r *Record) Connected() bool {
	return r != nil && r.Status == StatusConnected && r.RefreshToken != ""
}

func (r *Record) clone() *Record {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Scopes = append([]string(nil), r.Scopes...)
	return &cp
}

// CompleteLogin exchanges the callback code and builds the new Record: it
// verifies the ID token, and when prev carries an account (reauthorization) it
// requires the same subject before the credentials may replace prev's. The
// returned record may lack the plan-usage scope — check Record.HasPlanUsage and
// offer a re-consent instead of running inference.
func (c *Client) CompleteLogin(ctx context.Context, p *Pending, cb *Callback, prev *Record) (*Record, error) {
	ts, id, err := c.Exchange(ctx, p, cb)
	if err != nil {
		return nil, err
	}
	if prev != nil && prev.Subject != "" && prev.Subject != id.Subject {
		return nil, newErr(CodeAccountMismatch, false)
	}
	return &Record{
		ClientID: cb.ClientID, Subject: id.Subject, Email: id.Email, IDToken: ts.IDToken,
		AccessToken: ts.AccessToken, RefreshToken: ts.RefreshToken, ExpiresAt: ts.ExpiresAt,
		EarliestRefreshAt: ts.EarliestRefreshAt, Scopes: ts.Scopes, Status: StatusConnected,
	}, nil
}

// Store persists one registration's Record. Implementations must not return the
// same pointer they keep internally.
type Store interface {
	Load(ctx context.Context) (*Record, error)
	Save(ctx context.Context, r *Record) error
}

// refreshSkew: refresh when the access token is within this window of expiry.
const refreshSkew = time.Minute

// Session hands out fresh access tokens for one registration. It serializes
// refreshes (refresh tokens rotate; two concurrent redemptions would burn the
// session) and persists every rotation before using the new credentials.
type Session struct {
	c     *Client
	store Store

	mu     sync.Mutex
	rec    *Record
	loaded bool
}

// NewSession binds a Store to the client.
func (c *Client) NewSession(store Store) *Session { return &Session{c: c, store: store} }

// Reload drops the in-memory copy so the next call re-reads the store (use after
// an out-of-band change to the stored record).
func (s *Session) Reload() {
	s.mu.Lock()
	s.loaded, s.rec = false, nil
	s.mu.Unlock()
}

func (s *Session) load(ctx context.Context) (*Record, error) {
	if !s.loaded {
		r, err := s.store.Load(ctx)
		if err != nil {
			return nil, err
		}
		s.rec, s.loaded = r, true
	}
	return s.rec, nil
}

// Snapshot returns a copy of the current record (nil when none). It holds secrets.
func (s *Session) Snapshot(ctx context.Context) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.load(ctx)
	return r.clone(), err
}

// Replace installs and persists a record produced by a completed login.
func (s *Session) Replace(ctx context.Context, r *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.Save(ctx, r); err != nil {
		return err
	}
	s.rec, s.loaded = r.clone(), true
	return nil
}

func (s *Session) save(ctx context.Context, r *Record) {
	s.rec = r
	if err := s.store.Save(ctx, r); err != nil {
		// The in-memory copy stays authoritative for this process; losing a rotated
		// refresh token would force a new sign-in after a restart.
		s.c.logf("[siwc] persisting credentials failed: %s", CodeOf(err))
	}
}

// Token returns a valid access token. rejected is "" for normal use; pass the
// token that just got an HTTP 401 to make the session refresh once (and return
// the already-refreshed token if another caller beat you to it).
func (s *Session) Token(ctx context.Context, rejected string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.load(ctx)
	if err != nil {
		return "", err
	}
	if rec != nil && rec.Status == StatusReauth {
		return "", newErr(CodeReauthRequired, false)
	}
	if rec == nil || rec.RefreshToken == "" || rec.Status != StatusConnected {
		return "", newErr(CodeNotConnected, false)
	}
	now := s.c.now()

	// A refreshed ID token must be verified before its credentials are used.
	if rec.PendingIDToken != "" {
		if err := s.finishPending(ctx, rec); err != nil {
			return "", err
		}
		rec = s.rec
	}
	if !rec.HasPlanUsage() {
		return "", newErr(CodePlanUsageDisabled, false)
	}
	if rejected != "" && rec.AccessToken != rejected {
		return rec.AccessToken, nil // someone already refreshed past the rejected token
	}
	needs := rejected != "" || !now.Before(rec.ExpiresAt.Add(-refreshSkew))
	if !needs {
		return rec.AccessToken, nil
	}
	usable := now.Before(rec.ExpiresAt) && rejected == ""
	if now.Before(rec.EarliestRefreshAt) {
		if usable {
			return rec.AccessToken, nil
		}
		return "", newErr(CodeRefreshNotReady, true)
	}

	ts, err := s.c.Refresh(ctx, rec.ClientID, rec.RefreshToken, rec.Scopes)
	if err != nil {
		if RequiresReauth(err) {
			dead := rec.clone()
			dead.AccessToken, dead.RefreshToken, dead.IDToken = "", "", ""
			dead.PendingIDToken, dead.PendingReceivedAt = "", time.Time{}
			dead.ExpiresAt = time.Time{}
			dead.Status, dead.LastError = StatusReauth, CodeOf(err)
			s.save(ctx, dead)
			return "", newErr(CodeReauthRequired, false)
		}
		if usable { // transient refresh failure: the current token still works
			return rec.AccessToken, nil
		}
		return "", err
	}

	next := rec.clone()
	next.AccessToken, next.RefreshToken = ts.AccessToken, ts.RefreshToken
	next.ExpiresAt, next.EarliestRefreshAt = ts.ExpiresAt, ts.EarliestRefreshAt
	next.Scopes, next.Status, next.LastError = ts.Scopes, StatusConnected, ""
	if ts.IDToken != "" {
		next.PendingIDToken, next.PendingReceivedAt = ts.IDToken, ts.ReceivedAt
	}
	// Persist the rotation BEFORE identity verification can fail or the process can
	// be interrupted: the old refresh token is already spent.
	s.save(ctx, next)
	if next.PendingIDToken != "" {
		if err := s.finishPending(ctx, next); err != nil {
			return "", err
		}
		next = s.rec
	}
	if !next.HasPlanUsage() {
		return "", newErr(CodePlanUsageDisabled, false)
	}
	return next.AccessToken, nil
}

// finishPending verifies the pending refreshed ID token. Unavailable key service →
// retryable error with the connection preserved; verification failure or an
// identity change → reauthentication required.
func (s *Session) finishPending(ctx context.Context, rec *Record) error {
	id, err := s.c.VerifyIDToken(ctx, rec.PendingIDToken, rec.ClientID, "", rec.PendingReceivedAt)
	if err != nil {
		if CodeOf(err) == CodeIdentityUnavailable {
			return err
		}
		return s.failAccount(ctx, rec, CodeInvalidIDToken)
	}
	if rec.Subject != "" && id.Subject != rec.Subject {
		return s.failAccount(ctx, rec, CodeAccountMismatch)
	}
	ok := rec.clone()
	ok.IDToken, ok.PendingIDToken, ok.PendingReceivedAt = rec.PendingIDToken, "", time.Time{}
	if ok.Subject == "" {
		ok.Subject = id.Subject
	}
	if id.Email != "" {
		ok.Email = id.Email
	}
	s.save(ctx, ok)
	return nil
}

func (s *Session) failAccount(ctx context.Context, rec *Record, code string) error {
	dead := rec.clone()
	dead.AccessToken, dead.RefreshToken, dead.IDToken = "", "", ""
	dead.PendingIDToken, dead.PendingReceivedAt, dead.ExpiresAt = "", time.Time{}, time.Time{}
	dead.Status, dead.LastError = StatusReauth, code
	s.save(ctx, dead)
	return newErr(CodeReauthRequired, false)
}

// Logout signs the registration out: it revokes the refresh token at the issuer
// and clears every locally held token. The issued client id and the account
// identity are kept so the same account can sign in again without registering a
// new app. revoked=false means remote revocation could not be confirmed; local
// tokens are cleared regardless and the user can disconnect the app in ChatGPT
// settings.
func (s *Session) Logout(ctx context.Context) (revoked bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.load(ctx)
	if err != nil {
		return false, err
	}
	if rec == nil {
		return true, nil
	}
	revoked = true
	if rec.RefreshToken != "" {
		if rerr := s.c.Revoke(ctx, rec.ClientID, rec.RefreshToken); rerr != nil {
			revoked = false
			s.c.logf("[siwc] remote revocation not confirmed: %s", CodeOf(rerr))
		}
	}
	out := rec.clone()
	out.AccessToken, out.RefreshToken, out.IDToken = "", "", ""
	out.PendingIDToken, out.PendingReceivedAt = "", time.Time{}
	out.ExpiresAt, out.EarliestRefreshAt = time.Time{}, time.Time{}
	out.Scopes, out.Status, out.LastError = nil, StatusDisconnected, ""
	if err := s.store.Save(ctx, out); err != nil {
		return revoked, err
	}
	s.rec = out
	return revoked, nil
}
