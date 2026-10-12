package siwc

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func shiftedClock() (func() time.Time, *atomic.Int64) {
	var offset atomic.Int64
	return func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }, &offset
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *memStore) current() *Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.clone()
}

// A caller that gives up must not lose the rotation: the refresh keeps running detached from the
// caller's context and the new refresh token reaches the store.
func TestSessionRefreshSurvivesCallerCancellation(t *testing.T) {
	m := newMockIssuer(t)
	now, offset := shiftedClock()
	s, st, _ := loggedIn(t, m, now)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.onRefresh = func(url.Values) map[string]any {
		once.Do(func() { close(entered) })
		<-release
		a, r := m.nextTokens("")
		return map[string]any{"access_token": a, "refresh_token": r, "token_type": "Bearer", "expires_in": 3600,
			"id_token": m.idToken("oaiapp_new", "user-1", "", nil)}
	}
	offset.Store(int64(time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := s.Token(ctx, ""); errc <- err }()
	<-entered
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller must see its own cancellation, got %v", err)
	}
	close(release)
	waitFor(t, "rotation to be persisted", func() bool {
		r := st.current()
		return r != nil && r.RefreshToken == "refresh-2" && r.PendingIDToken == ""
	})
	// the next caller uses the rotated credentials without another redemption
	if tok, err := s.Token(context.Background(), ""); err != nil || tok != "access-2" {
		t.Fatalf("%q %v", tok, err)
	}
	if n := len(m.reqs()); n != 2 { // code exchange + exactly one refresh
		t.Fatalf("token requests: %d", n)
	}
}

// Snapshot is served from memory and never waits for a refresh in flight.
func TestSessionSnapshotDoesNotWaitForRefresh(t *testing.T) {
	m := newMockIssuer(t)
	now, offset := shiftedClock()
	s, _, _ := loggedIn(t, m, now)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.onRefresh = func(url.Values) map[string]any {
		once.Do(func() { close(entered) })
		<-release
		return map[string]any{"_error": "server_error"}
	}
	offset.Store(int64(time.Hour))
	go func() { _, _ = s.Token(context.Background(), "") }()
	<-entered
	done := make(chan *Record, 1)
	go func() { r, _ := s.Snapshot(context.Background()); done <- r }()
	select {
	case r := <-done:
		if r == nil || r.RefreshToken != "refresh-1" {
			t.Fatalf("snapshot: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Snapshot blocked behind the refresh")
	}
	close(release)
}

// A second Session over the same store (a stale one) must adopt the rotated refresh token instead of
// redeeming the spent one.
func TestSessionAdoptsNewerStoredRefreshToken(t *testing.T) {
	m := newMockIssuer(t)
	now, offset := shiftedClock()
	s1, st, c := loggedIn(t, m, now)
	s2 := c.NewSession(st)
	if r, err := s2.Snapshot(context.Background()); err != nil || r.RefreshToken != "refresh-1" { // s2 now holds refresh-1
		t.Fatalf("%+v %v", r, err)
	}
	seen := m.refreshHook("user-1", nil, true)
	offset.Store(int64(time.Hour))
	if tok, err := s1.Token(context.Background(), ""); err != nil || tok != "access-2" {
		t.Fatalf("%q %v", tok, err)
	}
	tok, err := s2.Token(context.Background(), "")
	if err != nil || tok != "access-2" {
		t.Fatalf("stale session: %q %v", tok, err)
	}
	if len(*seen) != 1 || (*seen)[0] != "refresh-1" {
		t.Fatalf("a spent refresh token was redeemed again: %v", *seen)
	}
	// ...and keeps working from the adopted token on the next rotation
	offset.Store(int64(3 * time.Hour))
	if tok, err := s2.Token(context.Background(), ""); err != nil || tok != "access-3" {
		t.Fatalf("%q %v", tok, err)
	}
	if got := *seen; len(got) != 2 || got[1] != "refresh-2" {
		t.Fatalf("redeemed %v", got)
	}
}

func TestSessionInvalidClientIsTerminalAndKeepsTokens(t *testing.T) {
	m := newMockIssuer(t)
	now, offset := shiftedClock()
	s, st, _ := loggedIn(t, m, now)
	m.onRefresh = func(url.Values) map[string]any { return map[string]any{"_error": "invalid_client"} }
	offset.Store(int64(2 * time.Hour))
	if _, err := s.Token(context.Background(), ""); CodeOf(err) != CodeInvalidClient {
		t.Fatalf("got %v", err)
	}
	r := st.current()
	if r.Status != StatusReauth || r.LastError != CodeInvalidClient || r.RefreshToken != "refresh-1" || r.ClientID != "oaiapp_new" {
		t.Fatalf("record: %+v", r)
	}
	before := len(m.reqs())
	for i := 0; i < 3; i++ {
		if _, err := s.Token(context.Background(), ""); CodeOf(err) != CodeInvalidClient {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if len(m.reqs()) != before {
		t.Fatal("a terminal client error must not be retried")
	}
	if rec, _ := s.Snapshot(context.Background()); rec.Connected() {
		t.Fatal("must not count as connected")
	}
}

func TestSessionTransientRefreshFailureBacksOff(t *testing.T) {
	m := newMockIssuer(t)
	now, offset := shiftedClock()
	s, _, _ := loggedIn(t, m, now)
	m.tokenStatus, m.tokenBody = 503, `{"error":"server_error"}`
	offset.Store(int64(2 * time.Hour)) // access token expired
	n0 := len(m.reqs())
	if _, err := s.Token(context.Background(), ""); err == nil {
		t.Fatal("expected a transient failure")
	}
	if len(m.reqs()) != n0+1 {
		t.Fatalf("first attempt must reach the network")
	}
	for i := 0; i < 3; i++ { // inside the backoff window: answered without any request
		_, err := s.Token(context.Background(), "")
		if err == nil || CodeOf(err) == CodeReauthRequired {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if len(m.reqs()) != n0+1 {
		t.Fatalf("requests during backoff: %d", len(m.reqs())-n0-1)
	}
	offset.Add(int64(refreshBackoffBase + time.Second))
	_, _ = s.Token(context.Background(), "")
	if len(m.reqs()) != n0+2 {
		t.Fatalf("must retry after the backoff window")
	}
	// the window doubles
	offset.Add(int64(refreshBackoffBase + time.Second))
	_, _ = s.Token(context.Background(), "")
	if len(m.reqs()) != n0+2 {
		t.Fatalf("second backoff must be longer than the first")
	}
}

func TestSessionRegisterRecordsClientIDWithoutTouchingALiveConnection(t *testing.T) {
	m := newMockIssuer(t)
	c := m.client()
	st := &memStore{}
	s := c.NewSession(st)
	if err := s.Register(context.Background(), "oaiapp_first"); err != nil {
		t.Fatal(err)
	}
	if r := st.current(); r == nil || r.ClientID != "oaiapp_first" || r.Status != StatusDisconnected || r.RefreshToken != "" {
		t.Fatalf("%+v", r)
	}
	if snap, _ := s.Snapshot(context.Background()); snap.ClientID != "oaiapp_first" {
		t.Fatalf("memory not updated: %+v", snap)
	}
	live, lst, _ := loggedIn(t, m, nil)
	if err := live.Register(context.Background(), "oaiapp_other"); err != nil {
		t.Fatal(err)
	}
	if r := lst.current(); r.ClientID != "oaiapp_new" || r.RefreshToken == "" {
		t.Fatalf("a live connection's client id must not change: %+v", r)
	}
}

// failingStore fails Save on demand.
type failingStore struct {
	*memStore
	fail atomic.Bool
}

func (f *failingStore) Save(ctx context.Context, r *Record) error {
	if f.fail.Load() {
		return errors.New("db down")
	}
	return f.memStore.Save(ctx, r)
}

// A failed write keeps memory authoritative (no regression to the spent token) and is written again later.
func TestSessionRetriesFailedPersistenceBeforeAdoptingTheStore(t *testing.T) {
	m := newMockIssuer(t)
	now, offset := shiftedClock()
	c := New(Config{AppName: "ARTEX", Issuer: m.srv.URL, HTTPClient: m.srv.Client(), Now: now})
	m.clock = now
	p, cb := beginAndCallback(t, c, "")
	m.codeHook(p, "oaiapp_new", "user-1", Scopes, nil)
	rec, err := c.CompleteLogin(context.Background(), p, cb, nil)
	if err != nil {
		t.Fatal(err)
	}
	fs := &failingStore{memStore: &memStore{}}
	s := c.NewSession(fs)
	if err := s.Replace(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	seen := m.refreshHook("user-1", nil, false)
	fs.fail.Store(true)
	offset.Store(int64(time.Hour))
	if tok, err := s.Token(context.Background(), ""); err != nil || tok != "access-2" {
		t.Fatalf("%q %v", tok, err)
	}
	if fs.current().RefreshToken != "refresh-1" {
		t.Fatal("test setup: the failing store must still hold the old token")
	}
	fs.fail.Store(false)
	offset.Store(int64(3 * time.Hour))
	if tok, err := s.Token(context.Background(), ""); err != nil || tok != "access-3" {
		t.Fatalf("%q %v", tok, err)
	}
	if got := *seen; len(got) != 2 || got[1] != "refresh-2" {
		t.Fatalf("the in-memory (newer) token must be redeemed, not the stale stored one: %v", got)
	}
	if fs.current().RefreshToken != "refresh-3" {
		t.Fatalf("store not caught up: %+v", fs.current())
	}
}

func TestSessionSetClientKeepsState(t *testing.T) {
	m := newMockIssuer(t)
	s, _, _ := loggedIn(t, m, nil)
	s.SetClient(m.client())
	if tok, err := s.Token(context.Background(), ""); err != nil || tok != "access-1" {
		t.Fatalf("%q %v", tok, err)
	}
	s.SetClient(nil) // ignored
	if tok, _ := s.Token(context.Background(), ""); tok != "access-1" {
		t.Fatal("nil client must be ignored")
	}
}

// While identity verification cannot run, an expired access token cannot be served.
func TestSessionPendingIdentityWithExpiredAccessTokenWaits(t *testing.T) {
	m := newMockIssuer(t)
	now, offset := shiftedClock()
	s, st, c := loggedIn(t, m, now)
	m.refreshHook("user-1", nil, true)
	c.keysMu.Lock()
	c.keys = nil
	c.keysMu.Unlock()
	m.jwksStatus = 503
	offset.Store(int64(time.Hour))
	if tok, err := s.Token(context.Background(), ""); err != nil || tok != "access-2" {
		t.Fatalf("%q %v", tok, err)
	}
	offset.Store(int64(3 * time.Hour)) // access-2 expired too
	if _, err := s.Token(context.Background(), ""); CodeOf(err) != CodeIdentityUnavailable {
		t.Fatalf("got %v", err)
	}
	if st.current().PendingIDToken == "" || st.current().Status != StatusConnected {
		t.Fatalf("connection must be preserved: %+v", st.current())
	}
}
