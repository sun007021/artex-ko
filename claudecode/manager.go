package claudecode

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// manager 는 프로세스 전체에서 하나다: 동시 실행 슬롯, 한도 게이트, loopback MCP 서버, 세션 표.
// 한도와 슬롯은 계정/설치 단위라서 프로필이 여럿이어도 공유한다.
type manager struct {
	lim  *limiter
	gate *gate

	enabled atomic.Bool
	logf    func(string, ...any)

	mu       sync.Mutex
	hub      *mcpHub
	sessions map[string]*session
	probes   map[string]probeEntry
	reaper   sync.Once
	stopCh   chan struct{}
	stopOnce sync.Once

	maxLive    int           // 동시에 살아 있는 대기 프로세스 상한(초과 시 가장 오래 놀던 것을 정리)
	idleTTL    time.Duration // 응답을 기다리지 않는 세션의 유휴 만료
	parkedTTL  time.Duration // 도구 결과를 기다리는 세션의 만료(긴 도구 실행을 감안해 더 길다)
	probeFn    func(ctx context.Context, bin string) Status
	probeOKTTL time.Duration
}

type probeEntry struct {
	st Status
	at time.Time
}

func newManager() *manager {
	return &manager{
		lim: newLimiter(DefaultMaxConcurrent), gate: newGate(),
		sessions: map[string]*session{}, probes: map[string]probeEntry{},
		stopCh:  make(chan struct{}),
		maxLive: 8, idleTTL: 10 * time.Minute, parkedTTL: 30 * time.Minute,
		probeFn: Probe, probeOKTTL: 5 * time.Minute,
	}
}

var defaultManager = newManager()

// ---- 패키지 수준 설정(서버가 부른다) ----

// SetEnabled 는 기능 전체 스위치다(기본 꺼짐). 꺼진 동안 모든 호출은 거절되고 살아 있는 세션은 정리된다.
func SetEnabled(on bool) {
	defaultManager.enabled.Store(on)
	if !on {
		CloseAll()
	}
}

// Enabled 는 기능이 켜져 있는지 알려 준다.
func Enabled() bool { return defaultManager.enabled.Load() }

// SetMaxConcurrent 는 동시에 모델 응답을 만드는 claude 프로세스 수 상한을 정한다(1..HardMaxConcurrent).
func SetMaxConcurrent(n int) { defaultManager.lim.setMax(n) }

// MaxConcurrent 는 현재 상한이다.
func MaxConcurrent() int { return defaultManager.lim.maxSlots() }

// ActiveTurns 는 지금 모델 응답을 만드는 중인 세션 수다.
func ActiveTurns() int { return defaultManager.lim.active() }

// SetLogger 는 진단 로그 함수를 정한다(프롬프트·응답 본문은 기록하지 않는다).
func SetLogger(f func(format string, a ...any)) { defaultManager.logf = f }

// CloseSession 은 ARTEX 세션 ID 에 해당하는 claude 프로세스를 정리한다(없으면 아무 일도 없다).
func CloseSession(id string) { defaultManager.closeSession(id) }

// CloseAll 은 모든 claude 프로세스를 정리한다(서버 종료·기능 끄기 때).
func CloseAll() { defaultManager.closeAll() }

// InvalidateProbe 는 점검 결과 캐시를 비운다(설정을 바꾸거나 사용자가 다시 로그인한 뒤).
func InvalidateProbe() {
	m := defaultManager
	m.mu.Lock()
	m.probes = map[string]probeEntry{}
	m.mu.Unlock()
}

// ---- 내부 ----

func (m *manager) getHub() (*mcpHub, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hub != nil {
		return m.hub, nil
	}
	h, err := startHub()
	if err != nil {
		return nil, err
	}
	m.hub = h
	return h, nil
}

func (m *manager) hubIfAny() *mcpHub {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hub
}

// session 은 key 의 세션을 돌려준다(없으면 만든다). 새로 만들 때 살아 있는 프로세스 수 상한을 지킨다.
func (m *manager) session(key string) *session {
	m.mu.Lock()
	s := m.sessions[key]
	if s == nil {
		s = newSession(m, key)
		m.sessions[key] = s
	}
	m.mu.Unlock()
	m.reaper.Do(func() { go m.reapLoop() })
	return s
}

// evictForRoom 은 살아 있는 대기 프로세스가 maxLive 를 넘으면 가장 오래 놀던 것부터 정리한다.
// 정리된 세션의 다음 호출은 재동기화(새 프로세스 + 전사 재생)로 이어지므로 정확성에는 영향이 없다.
func (m *manager) evictForRoom(keep *session) {
	m.mu.Lock()
	type cand struct {
		s  *session
		at time.Time
	}
	var live []cand
	for _, s := range m.sessions {
		if s == keep || !s.turn.TryLock() {
			continue // 지금 응답 중인 세션은 건드리지 않는다
		}
		s.mu.Lock()
		if s.p != nil {
			live = append(live, cand{s, s.lastActive})
		}
		s.mu.Unlock()
		s.turn.Unlock()
	}
	m.mu.Unlock()
	over := len(live) + 1 - m.maxLive
	if over <= 0 {
		return
	}
	sort.Slice(live, func(i, j int) bool { return live[i].at.Before(live[j].at) })
	for i := 0; i < over && i < len(live); i++ {
		if live[i].s.turn.TryLock() {
			live[i].s.kill()
			live[i].s.turn.Unlock()
		}
	}
}

func (m *manager) closeSession(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if s != nil {
		s.kill()
	}
}

func (m *manager) closeAll() {
	m.mu.Lock()
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.sessions = map[string]*session{}
	m.mu.Unlock()
	for _, s := range all {
		s.kill()
	}
}

// shutdown 은 모든 프로세스와 loopback MCP 서버, 정리 고루틴을 멈춘다(테스트·종료 경로).
func (m *manager) shutdown() {
	m.closeAll()
	m.mu.Lock()
	h := m.hub
	m.hub = nil
	m.mu.Unlock()
	if h != nil {
		h.close()
	}
	m.stopOnce.Do(func() { close(m.stopCh) })
}

func (m *manager) reapLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			m.reapOnce(time.Now())
		case <-m.stopCh:
			return
		}
	}
}

// reapOnce 는 오래 놀거나 도구 결과를 너무 오래 기다린 세션을 정리한다.
func (m *manager) reapOnce(now time.Time) {
	m.mu.Lock()
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		if !s.turn.TryLock() {
			continue // 응답 중
		}
		s.mu.Lock()
		ttl := m.idleTTL
		if s.state == stParked {
			ttl = m.parkedTTL
		}
		expired := s.p != nil && now.Sub(s.lastActive) > ttl
		s.mu.Unlock()
		if expired {
			s.logf("유휴 만료로 프로세스 정리 session=%s", s.key)
			s.kill()
		}
		s.turn.Unlock()
	}
}

// ---- 사전 점검(설치·버전·구독 로그인) ----

// preflight 는 호출 직전에 기능 스위치와 claude 상태를 확인한다. 성공은 잠시 캐시하고, 실패는 짧게만
// 캐시한다(사용자가 터미널에서 로그인하자마자 다시 시도할 수 있게).
func (m *manager) preflight(ctx context.Context, bin string) *Error {
	if !m.enabled.Load() {
		return errf(KindRefused, "claude-code 인증 방식이 꺼져 있습니다. 설정에서 개인 로컬 사용 고지를 확인하고 켜세요")
	}
	m.mu.Lock()
	e, ok := m.probes[bin]
	m.mu.Unlock()
	ttl := m.probeOKTTL
	if ok && !e.st.Usable {
		ttl = 10 * time.Second
	}
	if !ok || time.Since(e.at) > ttl {
		st := m.probeFn(ctx, bin)
		e = probeEntry{st: st, at: time.Now()}
		m.mu.Lock()
		m.probes[bin] = e
		m.mu.Unlock()
	}
	st := e.st
	if st.Usable {
		return nil
	}
	switch st.Problem {
	case ProblemNotInstalled:
		return &Error{Kind: KindNotFound, Message: st.Detail}
	case ProblemTooOld:
		return &Error{Kind: KindTooOld, Message: st.Detail}
	case ProblemNotLoggedIn:
		return &Error{Kind: KindAuth, Message: st.Detail}
	case ProblemAPIBilling:
		return &Error{Kind: KindRefused, Message: st.Detail}
	}
	return &Error{Kind: KindProtocol, Message: orDefault(st.Detail, "claude 상태를 확인하지 못했습니다")}
}
