package siwc

import (
	"context"
	"sync"
	"sync/atomic"
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
	// set is saved first and identity verification finishes later. While it is
	// pending the session keeps serving the access token for as long as that token
	// is valid (the account was verified at sign-in and a refresh cannot change it);
	// once the access token expires it waits for the verification.
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
//
// When the new tokens must be discarded (a different account signed in) the new
// refresh token is revoked at the issuer on a best-effort basis so a grant the
// app will never use does not stay alive.
func (c *Client) CompleteLogin(ctx context.Context, p *Pending, cb *Callback, prev *Record) (*Record, error) {
	ts, id, err := c.Exchange(ctx, p, cb)
	if err != nil {
		return nil, err
	}
	if prev != nil && prev.Subject != "" && prev.Subject != id.Subject {
		c.discardRefreshToken(ctx, cb.ClientID, ts.RefreshToken)
		return nil, newErr(CodeAccountMismatch, false)
	}
	return &Record{
		ClientID: cb.ClientID, Subject: id.Subject, Email: id.Email, IDToken: ts.IDToken,
		AccessToken: ts.AccessToken, RefreshToken: ts.RefreshToken, ExpiresAt: ts.ExpiresAt,
		EarliestRefreshAt: ts.EarliestRefreshAt, Scopes: ts.Scopes, Status: StatusConnected,
	}, nil
}

// discardRefreshToken revokes a refresh token that came back from an exchange that
// then had to be rejected. Best effort, bounded, and independent of the caller's
// cancellation: the tokens exist at the issuer whether or not the request aborts.
func (c *Client) discardRefreshToken(ctx context.Context, clientID, refreshToken string) {
	if refreshToken == "" {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardTimeout)
	defer cancel()
	if err := c.Revoke(rctx, clientID, refreshToken); err != nil {
		c.logf("[siwc] revoking the discarded credentials failed: %s", CodeOf(err))
	}
}

// Store persists one registration's Record. Implementations must not return the
// same pointer they keep internally.
type Store interface {
	Load(ctx context.Context) (*Record, error)
	Save(ctx context.Context, r *Record) error
}

const (
	// refreshSkew: refresh when the access token is within this window of expiry.
	refreshSkew = time.Minute
	// opTimeout bounds one credential operation (refresh / identity verification /
	// persisting / revocation). These run detached from the caller's context: a
	// refresh token that was redeemed at the issuer is spent, so the rotation must
	// reach the store even when the request that triggered it was cancelled.
	opTimeout = 15 * time.Second
	// discardTimeout bounds the best-effort revocation of rejected credentials.
	discardTimeout = 10 * time.Second
	// forcedRefreshCooldown: a 401 for a token this session minted less than this
	// long ago is not about the token (wrong account, workspace, scope ...), so it
	// does not trigger another refresh.
	forcedRefreshCooldown = 30 * time.Second
	// Backoff after transient refresh failures: refreshBackoffBase doubling up to
	// refreshBackoffMax; identity verification retries every verifyRetryEvery.
	refreshBackoffBase = 5 * time.Second
	refreshBackoffMax  = 2 * time.Minute
	verifyRetryEvery   = 30 * time.Second
)

// Session hands out fresh access tokens for one registration and is the only
// writer of its token state. Refresh tokens rotate, so everything that changes
// credentials runs one at a time (op) and persists every rotation before the new
// credentials are used. Readers (Snapshot, a still-valid Token) only take the
// short-lived state lock and never wait for network I/O.
//
// Use one Session per registration per process; a second one would redeem the
// same refresh token and be rejected as reuse. Token re-reads the store before
// refreshing and adopts a newer stored refresh token, which covers a store that
// was changed behind the session's back.
type Session struct {
	client atomic.Pointer[Client]
	store  Store
	op     chan struct{} // 1-slot semaphore: serializes credential changes

	mu             sync.Mutex // guards the fields below; never held across I/O
	rec            *Record    // immutable once installed (copy on write)
	loaded         bool
	dirty          bool      // the last store.Save failed: memory is ahead of the store
	mintedAt       time.Time // when this process last obtained or installed an access token
	failures       int       // consecutive transient refresh failures
	nextRefreshTry time.Time
	lastRefreshErr error
	nextVerifyTry  time.Time
}

// NewSession binds a Store to the client.
func (c *Client) NewSession(store Store) *Session {
	s := &Session{store: store, op: make(chan struct{}, 1)}
	s.client.Store(c)
	return s
}

// SetClient swaps the issuer client (e.g. after the profile's proxy changed) while
// keeping the session — and therefore the single rotating state — intact.
func (s *Session) SetClient(c *Client) {
	if c != nil {
		s.client.Store(c)
	}
}

func (s *Session) c() *Client { return s.client.Load() }

// detach returns a context that survives the caller's cancellation (keeping its
// values) and ends after d.
func detach(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), d)
}

// exclusive runs fn with the credential-change lock held. fn runs on its own
// goroutine under a detached context with its own timeout: the caller's ctx only
// abandons the wait (waiting for the lock or for fn), never fn itself.
func exclusive[T any](ctx context.Context, s *Session, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	select {
	case s.op <- struct{}{}:
	case <-ctx.Done():
		return zero, ctx.Err()
	}
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-s.op }()
		opCtx, cancel := detach(ctx, opTimeout)
		defer cancel()
		v, err := fn(opCtx)
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// view is a consistent copy of the state a decision needs.
type view struct {
	rec         *Record
	minted      time.Time
	nextRefresh time.Time
	nextVerify  time.Time
	refreshErr  error
}

func (s *Session) view(ctx context.Context) (view, error) {
	s.mu.Lock()
	if s.loaded {
		v := view{s.rec, s.mintedAt, s.nextRefreshTry, s.nextVerifyTry, s.lastRefreshErr}
		s.mu.Unlock()
		return v, nil
	}
	s.mu.Unlock()
	r, err := s.store.Load(ctx) // not under the lock: a slow store must not stall readers
	if err != nil {
		return view{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		s.rec, s.loaded = r, true
	}
	return view{s.rec, s.mintedAt, s.nextRefreshTry, s.nextVerifyTry, s.lastRefreshErr}, nil
}

func (s *Session) setRec(r *Record) {
	s.mu.Lock()
	s.rec, s.loaded = r, true
	s.mu.Unlock()
}

// Snapshot returns a copy of the current record (nil when none). It holds secrets.
// It never waits for a refresh or any other network call.
func (s *Session) Snapshot(ctx context.Context) (*Record, error) {
	v, err := s.view(ctx)
	return v.rec.clone(), err
}

// persist installs r in memory and writes it to the store under its own timeout,
// independent of the caller's cancellation. A failed write keeps the in-memory
// copy authoritative for this process (losing a rotated refresh token would force
// a new sign-in) and is retried before the next refresh.
func (s *Session) persist(ctx context.Context, r *Record) error {
	s.setRec(r)
	sctx, cancel := detach(ctx, opTimeout)
	defer cancel()
	err := s.store.Save(sctx, r)
	s.mu.Lock()
	s.dirty = err != nil
	s.mu.Unlock()
	if err != nil {
		s.c().logf("[siwc] persisting credentials failed: %s", CodeOf(err))
	}
	return err
}

// adoptStored re-reads the store and adopts what it holds when it differs from
// memory (another writer rotated the refresh token). If the last write failed the
// memory copy is newer, so it is written again instead.
func (s *Session) adoptStored(ctx context.Context) {
	s.mu.Lock()
	dirty, cur := s.dirty, s.rec
	s.mu.Unlock()
	if dirty {
		if cur != nil {
			_ = s.persist(ctx, cur)
		}
		return
	}
	stored, err := s.store.Load(ctx)
	if err != nil || stored == nil {
		return
	}
	if cur != nil && stored.RefreshToken == cur.RefreshToken && stored.AccessToken == cur.AccessToken &&
		stored.Status == cur.Status && stored.ClientID == cur.ClientID && stored.PendingIDToken == cur.PendingIDToken {
		return
	}
	s.setRec(stored)
}

// Replace installs and persists a record produced by a completed login.
func (s *Session) Replace(ctx context.Context, r *Record) error {
	_, err := exclusive(ctx, s, func(ctx context.Context) (struct{}, error) {
		if err := s.store.Save(ctx, r); err != nil {
			return struct{}{}, err
		}
		s.mu.Lock()
		s.rec, s.loaded, s.dirty = r.clone(), true, false
		s.mintedAt = s.c().now()
		s.failures, s.nextRefreshTry, s.lastRefreshErr, s.nextVerifyTry = 0, time.Time{}, nil, time.Time{}
		s.mu.Unlock()
		return struct{}{}, nil
	})
	return err
}

// Register records the client id that a registration just issued, before the
// one-shot code exchange that may still fail, so the next sign-in reuses it
// instead of registering a new app. A record that holds a live connection is left
// alone; so is one that already carries this client id.
func (s *Session) Register(ctx context.Context, clientID string) error {
	_, err := exclusive(ctx, s, func(ctx context.Context) (struct{}, error) {
		s.adoptStored(ctx)
		v, err := s.view(ctx)
		if err != nil {
			return struct{}{}, err
		}
		if v.rec != nil && (v.rec.ClientID == clientID || v.rec.Connected()) {
			return struct{}{}, nil
		}
		next := v.rec.clone()
		if next == nil {
			next = &Record{Status: StatusDisconnected}
		}
		next.ClientID = clientID
		return struct{}{}, s.persist(ctx, next)
	})
	return err
}

// decision is the outcome of looking at the state: an answer, or "needs work"
// (refresh and/or identity verification, which run exclusively).
type decision struct {
	token string
	err   error
	work  bool
}

// decide evaluates v without doing any I/O.
func decide(v view, rejected string, now time.Time) decision {
	rec := v.rec
	if rec != nil && rec.Status == StatusReauth {
		if rec.LastError == CodeInvalidClient {
			return decision{err: newErr(CodeInvalidClient, false)}
		}
		return decision{err: newErr(CodeReauthRequired, false)}
	}
	if rec == nil || rec.RefreshToken == "" || rec.Status != StatusConnected {
		return decision{err: newErr(CodeNotConnected, false)}
	}
	if rec.PendingIDToken != "" {
		if now.Before(v.nextVerify) { // verification just failed to run: keep serving a valid token meanwhile
			if tok, ok := pendingFallback(rec, rejected, now, newErr(CodeIdentityUnavailable, true)); ok {
				return decision{token: tok}
			}
		}
		return decision{work: true}
	}
	if !rec.HasPlanUsage() {
		return decision{err: newErr(CodePlanUsageDisabled, false)}
	}
	if rejected != "" {
		if rec.AccessToken != rejected {
			return decision{token: rec.AccessToken} // someone already refreshed past the rejected token
		}
		if !v.minted.IsZero() && now.Sub(v.minted) < forcedRefreshCooldown {
			// The token is fresh, so this 401 is not about the token: do not burn a
			// refresh on it. The caller sees the same token and gives up.
			return decision{token: rec.AccessToken}
		}
		return decision{work: true}
	}
	if now.Before(rec.ExpiresAt.Add(-refreshSkew)) {
		return decision{token: rec.AccessToken}
	}
	if now.Before(rec.ExpiresAt) && (now.Before(rec.EarliestRefreshAt) || now.Before(v.nextRefresh)) {
		return decision{token: rec.AccessToken} // refresh not possible yet / backing off, token still works
	}
	return decision{work: true}
}

// Token returns a valid access token. rejected is "" for normal use; pass the
// token that just got an HTTP 401 to make the session refresh once (and return
// the already-refreshed token if another caller beat you to it). When the
// rejected token was minted moments ago the same token comes back instead of
// another refresh: the 401 is then not about the token.
//
// Refreshing, identity verification and persisting run detached from ctx (with
// their own timeout); cancelling ctx only abandons the wait.
func (s *Session) Token(ctx context.Context, rejected string) (string, error) {
	v, err := s.view(ctx)
	if err != nil {
		return "", err
	}
	if d := decide(v, rejected, s.c().now()); !d.work {
		return d.token, d.err
	}
	return exclusive(ctx, s, func(ctx context.Context) (string, error) { return s.tokenExclusive(ctx, rejected) })
}

func (s *Session) tokenExclusive(ctx context.Context, rejected string) (string, error) {
	s.adoptStored(ctx)
	v, err := s.view(ctx)
	if err != nil {
		return "", err
	}
	c := s.c()
	now := c.now()
	if d := decide(v, rejected, now); !d.work {
		return d.token, d.err
	}
	rec := v.rec

	// A refreshed ID token must be verified before the account is trusted again.
	if rec.PendingIDToken != "" {
		next, err := s.verifyPending(ctx, rec, now)
		if err != nil {
			if tok, ok := pendingFallback(rec, rejected, now, err); ok {
				return tok, nil
			}
			return "", err
		}
		rec = next
		v.rec = rec
		if d := decide(v, rejected, now); !d.work {
			return d.token, d.err
		}
	}

	usable := rejected == "" && now.Before(rec.ExpiresAt)
	if now.Before(rec.EarliestRefreshAt) {
		if usable {
			return rec.AccessToken, nil
		}
		return "", newErr(CodeRefreshNotReady, true)
	}
	if now.Before(v.nextRefresh) { // backing off after a transient failure
		if usable {
			return rec.AccessToken, nil
		}
		if v.refreshErr != nil {
			return "", v.refreshErr
		}
		return "", newErr(CodeNetwork, true)
	}

	ts, err := c.Refresh(ctx, rec.ClientID, rec.RefreshToken, rec.Scopes)
	if err != nil {
		switch {
		case RequiresReauth(err):
			dead := rec.clone() // the verified ID token stays: it is the id_token_hint of the re-sign-in
			dead.AccessToken, dead.RefreshToken = "", ""
			dead.PendingIDToken, dead.PendingReceivedAt = "", time.Time{}
			dead.ExpiresAt = time.Time{}
			dead.Status, dead.LastError = StatusReauth, CodeOf(err)
			_ = s.persist(ctx, dead)
			return "", newErr(CodeReauthRequired, false)
		case CodeOf(err) == CodeInvalidClient:
			// The issuer no longer accepts this registration. Terminal, but not a
			// token problem: keep the credentials, surface the code, stop retrying.
			dead := rec.clone()
			dead.Status, dead.LastError = StatusReauth, CodeInvalidClient
			_ = s.persist(ctx, dead)
			return "", newErr(CodeInvalidClient, false)
		}
		if CodeOf(err) == "" { // our own timeout / cancellation of the detached call
			err = newErr(CodeNetwork, true)
		}
		s.noteRefreshFailure(now, err)
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
	_ = s.persist(ctx, next)
	s.mu.Lock()
	s.mintedAt, s.failures, s.nextRefreshTry, s.lastRefreshErr = now, 0, time.Time{}, nil
	s.mu.Unlock()
	if next.PendingIDToken != "" {
		verified, err := s.verifyPending(ctx, next, now)
		if err != nil {
			if tok, ok := pendingFallback(next, "", now, err); ok {
				return tok, nil
			}
			return "", err
		}
		next = verified
	}
	if !next.HasPlanUsage() {
		return "", newErr(CodePlanUsageDisabled, false)
	}
	return next.AccessToken, nil
}

// pendingFallback: while the identity check cannot run (key service down) a still
// valid access token of an account that was verified at sign-in keeps working.
func pendingFallback(rec *Record, rejected string, now time.Time, err error) (string, bool) {
	if CodeOf(err) != CodeIdentityUnavailable || rejected != "" || rec.Subject == "" ||
		!rec.HasPlanUsage() || rec.AccessToken == "" || !now.Before(rec.ExpiresAt) {
		return "", false
	}
	return rec.AccessToken, true
}

func (s *Session) noteRefreshFailure(now time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	d := refreshBackoffBase << min(s.failures-1, 10)
	if d > refreshBackoffMax {
		d = refreshBackoffMax
	}
	s.nextRefreshTry, s.lastRefreshErr = now.Add(d), err
}

// verifyPending verifies the pending refreshed ID token. Unavailable key service →
// retryable error with the connection preserved (and no new attempt for
// verifyRetryEvery); verification failure or an identity change → reauthentication
// required.
func (s *Session) verifyPending(ctx context.Context, rec *Record, now time.Time) (*Record, error) {
	s.mu.Lock()
	wait := now.Before(s.nextVerifyTry)
	s.mu.Unlock()
	if wait {
		return nil, newErr(CodeIdentityUnavailable, true)
	}
	id, err := s.c().VerifyIDToken(ctx, rec.PendingIDToken, rec.ClientID, "", rec.PendingReceivedAt)
	if err != nil {
		if CodeOf(err) == CodeIdentityUnavailable {
			s.mu.Lock()
			s.nextVerifyTry = now.Add(verifyRetryEvery)
			s.mu.Unlock()
			return nil, err
		}
		return nil, s.failAccount(ctx, rec, CodeInvalidIDToken)
	}
	if rec.Subject != "" && id.Subject != rec.Subject {
		return nil, s.failAccount(ctx, rec, CodeAccountMismatch)
	}
	ok := rec.clone()
	ok.IDToken, ok.PendingIDToken, ok.PendingReceivedAt = rec.PendingIDToken, "", time.Time{}
	if ok.Subject == "" {
		ok.Subject = id.Subject
	}
	if id.Email != "" {
		ok.Email = id.Email
	}
	_ = s.persist(ctx, ok)
	return ok, nil
}

func (s *Session) failAccount(ctx context.Context, rec *Record, code string) error {
	dead := rec.clone()
	dead.AccessToken, dead.RefreshToken, dead.IDToken = "", "", ""
	dead.PendingIDToken, dead.PendingReceivedAt, dead.ExpiresAt = "", time.Time{}, time.Time{}
	dead.Status, dead.LastError = StatusReauth, code
	_ = s.persist(ctx, dead)
	return newErr(CodeReauthRequired, false)
}

// Logout signs the registration out: it revokes the refresh token at the issuer
// and clears every locally held token. The issued client id and the account
// identity are kept so the same account can sign in again without registering a
// new app. revoked=false means remote revocation could not be confirmed; local
// tokens are cleared regardless and the user can disconnect the app in ChatGPT
// settings.
func (s *Session) Logout(ctx context.Context) (revoked bool, err error) {
	return exclusive(ctx, s, func(ctx context.Context) (bool, error) {
		v, err := s.view(ctx)
		if err != nil {
			return false, err
		}
		rec := v.rec
		if rec == nil {
			return true, nil
		}
		revoked := true
		if rec.RefreshToken != "" {
			if rerr := s.c().Revoke(ctx, rec.ClientID, rec.RefreshToken); rerr != nil {
				revoked = false
				s.c().logf("[siwc] remote revocation not confirmed: %s", CodeOf(rerr))
			}
		}
		out := rec.clone()
		out.AccessToken, out.RefreshToken, out.IDToken = "", "", ""
		out.PendingIDToken, out.PendingReceivedAt = "", time.Time{}
		out.ExpiresAt, out.EarliestRefreshAt = time.Time{}, time.Time{}
		out.Scopes, out.Status, out.LastError = nil, StatusDisconnected, ""
		sctx, cancel := detach(ctx, opTimeout)
		defer cancel()
		if err := s.store.Save(sctx, out); err != nil {
			return revoked, err
		}
		s.mu.Lock()
		s.rec, s.dirty = out, false
		s.failures, s.nextRefreshTry, s.lastRefreshErr, s.nextVerifyTry = 0, time.Time{}, nil, time.Time{}
		s.mu.Unlock()
		return revoked, nil
	})
}
