package claudecode

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Manager 는 claude 구독 경로의 실행 환경이다: 동시 실행 슬롯, 한도 게이트, loopback MCP 서버, 세션 표,
// 사전 점검 캐시, 정리 고루틴. 한도와 슬롯은 계정/설치 단위라서 프로필이 여럿이어도 같은 Manager 를
// 공유한다. 프로세스 전역 상태가 아니라 값이다: 서버가 하나 만들어 쥐고 Provider 에 주입한다(테스트는
// 자기 Manager 를 만든다).
type Manager struct {
	lim   *limiter
	gates *gateSet
	probe *prober

	enabled atomic.Bool
	logger  atomic.Pointer[logFunc]

	mu       sync.Mutex
	hub      *mcpHub
	sessions map[string]*session
	reaper   sync.Once
	stopCh   chan struct{}
	stopOnce sync.Once

	maxLive   atomic.Int64  // 동시에 살아 있는 대기 프로세스 상한(초과 시 정리: 유휴 우선, 오래된 순)
	idleTTL   time.Duration // 응답을 기다리지 않는 세션의 유휴 만료
	parkedTTL time.Duration // 도구 결과를 기다리는 세션의 만료(긴 도구 실행을 감안해 더 길다)
	stall     time.Duration // 응답 중 stdout 이 이 시간 동안 조용하면 멈춘 것으로 보고 정리한다
}

type logFunc func(format string, a ...any)

const (
	// DefaultMaxLive 는 동시에 살아 있을 수 있는 claude 대기 프로세스 수의 기본 상한이다.
	DefaultMaxLive = 8
	// HardMaxLive 는 SetMaxLive 로도 넘을 수 없는 상한이다.
	HardMaxLive = 64

	defaultStallTimeout = 10 * time.Minute
)

// NewManager 는 꺼진 상태(SetEnabled(true) 전에는 모든 호출을 거절)의 Manager 를 만든다.
func NewManager() *Manager {
	m := &Manager{
		lim: newLimiter(DefaultMaxConcurrent), gates: newGateSet(), probe: newProber(Probe),
		sessions: map[string]*session{}, stopCh: make(chan struct{}),
		idleTTL: 10 * time.Minute, parkedTTL: 30 * time.Minute, stall: defaultStallTimeout,
	}
	m.maxLive.Store(DefaultMaxLive)
	return m
}

// ---- 설정(서버가 부른다) ----

// SetEnabled 는 기능 전체 스위치다(기본 꺼짐). 꺼진 동안 모든 호출은 거절되고 살아 있는 세션은 정리된다.
func (m *Manager) SetEnabled(on bool) {
	m.enabled.Store(on)
	if !on {
		m.CloseAll()
	}
}

// Enabled 는 기능이 켜져 있는지 알려 준다. nil Manager 는 꺼진 것으로 본다.
func (m *Manager) Enabled() bool { return m != nil && m.enabled.Load() }

// SetMaxConcurrent 는 동시에 모델 응답을 만드는 claude 프로세스 수 상한을 정한다(1..HardMaxConcurrent).
func (m *Manager) SetMaxConcurrent(n int) { m.lim.setMax(n) }

// MaxConcurrent 는 현재 상한이다.
func (m *Manager) MaxConcurrent() int { return m.lim.maxSlots() }

// ActiveTurns 는 지금 모델 응답을 만드는 중인 세션 수다.
func (m *Manager) ActiveTurns() int { return m.lim.active() }

// SetMaxLive 는 동시에 살아 있는 대기 프로세스 수 상한을 정한다(1..HardMaxLive, 0 이하 = 기본값).
func (m *Manager) SetMaxLive(n int) {
	switch {
	case n <= 0:
		n = DefaultMaxLive
	case n > HardMaxLive:
		n = HardMaxLive
	}
	m.maxLive.Store(int64(n))
}

// MaxLive 는 현재 상한이다.
func (m *Manager) MaxLive() int { return int(m.maxLive.Load()) }

// SetLogger 는 진단 로그 함수를 정한다(프롬프트·응답 본문은 기록하지 않는다). 동시에 불러도 안전하다.
func (m *Manager) SetLogger(f func(format string, a ...any)) {
	if f == nil {
		m.logger.Store(nil)
		return
	}
	lf := logFunc(f)
	m.logger.Store(&lf)
}

func (m *Manager) logf(format string, a ...any) {
	if lf := m.logger.Load(); lf != nil {
		(*lf)(format, a...)
	}
}

// InvalidateProbe 는 점검 결과 캐시를 비운다(설정을 바꾸거나 사용자가 다시 로그인한 뒤).
func (m *Manager) InvalidateProbe() { m.probe.invalidate() }

// CloseSession 은 ARTEX 세션 ID 에 해당하는 claude 프로세스를 정리한다(없으면 아무 일도 없다). 응답 중이던
// 호출이 있으면 그 호출은 실패하고 새 프로세스를 띄우지 못한다.
func (m *Manager) CloseSession(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if s != nil {
		s.close()
	}
}

// CloseAll 은 모든 claude 프로세스를 정리한다(서버 종료·기능 끄기 때). 응답 중이던 호출은 실패하고
// 새 프로세스를 띄우지 못한다.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.sessions = map[string]*session{}
	m.mu.Unlock()
	for _, s := range all {
		s.close()
	}
}

// Shutdown 은 모든 프로세스와 loopback MCP 서버, 정리 고루틴을 멈춘다(종료·테스트 경로).
func (m *Manager) Shutdown() {
	m.CloseAll()
	m.mu.Lock()
	h := m.hub
	m.hub = nil
	m.mu.Unlock()
	if h != nil {
		h.close()
	}
	m.stopOnce.Do(func() { close(m.stopCh) })
}

// NewProvider 는 이 Manager 위에서 claude CLI 로 모델 턴을 만드는 Provider 를 돌려준다.
func (m *Manager) NewProvider(opts Options) *Provider {
	return newProvider(m, opts)
}

// ---- 내부 ----

func (m *Manager) getHub() (*mcpHub, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hub != nil {
		return m.hub, nil
	}
	h, err := startHub(m.logf)
	if err != nil {
		return nil, err
	}
	m.hub = h
	return h, nil
}

func (m *Manager) hubIfAny() *mcpHub {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hub
}

// session 은 key 의 세션을 돌려준다(없으면 만든다).
func (m *Manager) session(key string) *session {
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

// acquire 는 key 의 세션을 잡아(turn 잠금) 돌려준다. 기다리는 사이에 세션이 닫히거나 정리되어 표에서 빠졌으면
// 새 세션으로 다시 잡는다.
func (m *Manager) acquire(key string) (*session, error) {
	for {
		if !m.enabled.Load() {
			return nil, errf(KindRefused, "claude-code 인증 방식이 꺼져 있습니다. 설정에서 개인 로컬 사용 고지를 확인하고 켜세요")
		}
		s := m.session(key)
		s.turn.Lock()
		if !s.isRetired() {
			return s, nil
		}
		s.turn.Unlock()
	}
}

// evictForRoom 은 살아 있는 대기 프로세스가 상한을 넘으면 정리한다. 도구 결과를 기다리는(parked) 세션보다 유휴
// (idle) 세션을 먼저, 같은 부류에서는 가장 오래 놀던 것부터 정리한다. 정리된 세션의 다음 호출은
// 재동기화(새 프로세스 + 전사 재생)로 이어지므로 정확성에는 영향이 없다.
func (m *Manager) evictForRoom(keep *session) {
	type cand struct {
		s      *session
		at     time.Time
		parked bool
	}
	var live []cand
	m.mu.Lock()
	for _, s := range m.sessions {
		if s == keep || !s.turn.TryLock() {
			continue // 지금 응답 중인 세션은 건드리지 않는다
		}
		s.mu.Lock()
		if s.p != nil {
			live = append(live, cand{s, s.lastActive, s.state == stParked})
		}
		s.mu.Unlock()
		s.turn.Unlock()
	}
	m.mu.Unlock()
	limit := m.MaxLive()
	over := len(live) + 1 - limit
	if over <= 0 {
		return
	}
	sort.Slice(live, func(i, j int) bool {
		if live[i].parked != live[j].parked {
			return !live[i].parked // 유휴가 먼저
		}
		return live[i].at.Before(live[j].at)
	})
	for i := 0; i < over && i < len(live); i++ {
		c := live[i]
		if !c.s.turn.TryLock() {
			continue
		}
		state := "idle"
		if c.parked {
			state = "parked"
		}
		m.logf("[claude-code] 살아 있는 프로세스 상한(%d)을 넘어 정리 session=%s state=%s 마지막 사용=%s 전", limit, c.s.key, state, time.Since(c.at).Round(time.Second))
		c.s.kill()
		c.s.turn.Unlock()
	}
}

func (m *Manager) reapLoop() {
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

// reapOnce 는 오래 놀거나 도구 결과를 너무 오래 기다린 세션의 프로세스를 정리하고, 프로세스가 없는 채로
// 오래 놀던 세션은 표에서 뺀다(키가 늘 새로 생기는 호출이 표를 키우지 않게).
func (m *Manager) reapOnce(now time.Time) {
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
		idle := now.Sub(s.lastActive)
		hasProc := s.p != nil
		s.mu.Unlock()
		switch {
		case hasProc && idle > ttl:
			s.logf("유휴 만료로 프로세스 정리 session=%s", s.key)
			s.kill()
			m.retire(s)
		case !hasProc && idle > m.idleTTL:
			m.retire(s)
		}
		s.turn.Unlock()
	}
}

// retire 는 세션을 표에서 빼고 닫는다(호출자가 turn 잠금을 쥐고 있다). 그 세션을 기다리던 호출은 acquire 에서
// 새 세션을 받는다.
func (m *Manager) retire(s *session) {
	s.mu.Lock()
	s.retired = true
	s.mu.Unlock()
	m.mu.Lock()
	if m.sessions[s.key] == s {
		delete(m.sessions, s.key)
	}
	m.mu.Unlock()
}

// ---- 사전 점검(설치·버전·구독 로그인) ----

// preflight 는 호출 직전에 기능 스위치와 claude 상태를 확인한다. 성공은 잠시 캐시하고, 실패는 짧게만
// 캐시한다(사용자가 터미널에서 로그인하자마자 다시 시도할 수 있게).
func (m *Manager) preflight(ctx context.Context, bin string) *Error {
	if !m.enabled.Load() {
		return errf(KindRefused, "claude-code 인증 방식이 꺼져 있습니다. 설정에서 개인 로컬 사용 고지를 확인하고 켜세요")
	}
	st, err := m.probe.get(ctx, bin)
	if err != nil { // 호출자가 기다리다 취소했다 — 점검 결과가 아니다
		return errf(KindProtocol, "claude 상태 점검이 취소되었습니다: %v", err)
	}
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

// noteResult 는 결과를 모델 범위의 게이트에 반영하고 그대로 돌려준다.
func (m *Manager) noteResult(model string, err error) error {
	var ce *Error
	if errors.As(err, &ce) {
		m.gates.record(model, ce)
	}
	return err
}
