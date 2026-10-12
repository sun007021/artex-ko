package claudecode

import (
	"context"
	"sync"
	"time"
)

// 구독 한도는 "ordinary, individual usage" 를 전제로 하므로(legal-and-compliance), 여러
// 에이전트가 동시에 모델 응답을 만드는 일을 전역으로 묶는다.
//
// 상한의 단위는 "동시에 모델 응답을 만들고 있는 claude 프로세스"다. 에이전트마다 장수명
// 프로세스를 하나씩 두므로(대화 기록·프롬프트 캐시를 유지하기 위해) 프로세스 개수 자체를 묶으면
// 한 에이전트가 슬롯을 쥔 채 다른 에이전트가 영원히 못 도는 교착이 생긴다. 그래서 슬롯은
// "모델 응답 한 번"(Provider.Stream 한 번)을 만드는 동안만 잡고, ARTEX 가 도구를 실행하는 동안
// 대기 중인 프로세스는 슬롯을 쥐지 않는다. 대기 프로세스 수는 별도 상한(maxLive)과 유휴 TTL 로 묶는다.

// DefaultMaxConcurrent 와 HardMaxConcurrent 는 슬롯 수의 기본값과 하드 상한이다.
const (
	DefaultMaxConcurrent = 1
	HardMaxConcurrent    = 3
)

// limiter 는 크기를 바꿀 수 있는 카운팅 세마포어다.
type limiter struct {
	mu     sync.Mutex
	max    int
	inUse  int
	wakeCh chan struct{} // 슬롯이 반납되거나 상한이 바뀌면 닫혀 대기자를 깨운다
}

func newLimiter(max int) *limiter {
	l := &limiter{wakeCh: make(chan struct{})}
	l.setMax(max)
	return l
}

func clampConcurrent(n int) int {
	if n <= 0 {
		return DefaultMaxConcurrent
	}
	if n > HardMaxConcurrent {
		return HardMaxConcurrent
	}
	return n
}

func (l *limiter) setMax(n int) {
	l.mu.Lock()
	l.max = clampConcurrent(n)
	l.broadcastLocked()
	l.mu.Unlock()
}

func (l *limiter) maxSlots() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.max
}

func (l *limiter) active() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inUse
}

func (l *limiter) broadcastLocked() {
	close(l.wakeCh)
	l.wakeCh = make(chan struct{})
}

// acquire 는 슬롯 하나를 잡는다. ctx 가 끝나면 ctx.Err() 를 돌려준다.
func (l *limiter) acquire(ctx context.Context) (release func(), err error) {
	for {
		l.mu.Lock()
		if l.inUse < l.max {
			l.inUse++
			l.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					l.mu.Lock()
					l.inUse--
					l.broadcastLocked()
					l.mu.Unlock()
				})
			}, nil
		}
		wake := l.wakeCh
		l.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// gate 는 한도/속도 제한 뒤의 물러남 규칙이다. 계정을 바꾸지 않고, 촘촘한 재시도도 하지 않는다.
//
//   - 사용량 한도(KindUsageLimit): 풀릴 때까지(알려 준 시각, 모르면 usageLimitFallback) 새 호출을
//     프로세스 기동 없이 즉시 같은 오류로 거절한다. 장애 전환(task LLM 체인)이 있으면 그쪽이
//     다음 프로필로 넘어가고, 없으면 오류가 그대로 드러난다.
//   - 일시적 속도 제한/과부하(KindRateLimit): 지수 백오프 구간(rateBackoffMin..Max) 동안 호출을
//     기다리게 한다(ctx 취소 가능). claude 자체도 내부적으로 재시도하므로 여기서는 그 뒤의 간격만 책임진다.
type gate struct {
	mu       sync.Mutex
	until    time.Time
	err      *Error
	backoff  time.Duration
	nowFn    func() time.Time
	sleepFn  func(ctx context.Context, d time.Duration) error
	fallback time.Duration
}

const (
	usageLimitFallback = 30 * time.Minute
	usageLimitCap      = 12 * time.Hour
	rateBackoffMin     = 30 * time.Second
	rateBackoffMax     = 5 * time.Minute
)

func newGate() *gate {
	return &gate{nowFn: time.Now, sleepFn: sleepCtx, fallback: usageLimitFallback}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// check 는 호출 직전에 부른다. 사용량 한도 구간이면 그 오류를, 속도 제한 구간이면 풀릴 때까지
// 기다린 뒤 nil 을, 아니면 nil 을 돌려준다.
func (g *gate) check(ctx context.Context) error {
	g.mu.Lock()
	until, e := g.until, g.err
	now := g.nowFn()
	g.mu.Unlock()
	if e == nil || !now.Before(until) {
		return nil
	}
	if e.Kind == KindUsageLimit {
		return &Error{Kind: KindUsageLimit, Message: e.Message + " (한도가 풀릴 때까지 새 요청을 보내지 않습니다)", ResetAt: e.ResetAt}
	}
	return g.sleepFn(ctx, until.Sub(now))
}

// record 는 실패 결과를 게이트에 반영한다. 사용량 한도/속도 제한이 아니면 아무 일도 하지 않는다.
func (g *gate) record(e *Error) {
	if e == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.nowFn()
	switch e.Kind {
	case KindUsageLimit:
		d := g.fallback
		if !e.ResetAt.IsZero() && e.ResetAt.After(now) {
			d = e.ResetAt.Sub(now)
		}
		if d > usageLimitCap {
			d = usageLimitCap
		}
		g.until, g.err = now.Add(d), e
	case KindRateLimit:
		if g.backoff < rateBackoffMin {
			g.backoff = rateBackoffMin
		} else if g.backoff < rateBackoffMax {
			g.backoff *= 2
			if g.backoff > rateBackoffMax {
				g.backoff = rateBackoffMax
			}
		}
		if cand := now.Add(g.backoff); cand.After(g.until) || g.err == nil || g.err.Kind != KindUsageLimit {
			g.until, g.err = cand, e
		}
	}
}

// success 는 정상 응답을 받았을 때 백오프와 구간을 걷는다.
func (g *gate) success() {
	g.mu.Lock()
	g.backoff, g.until, g.err = 0, time.Time{}, nil
	g.mu.Unlock()
}
