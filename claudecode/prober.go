package claudecode

import (
	"context"
	"sync"
	"time"
)

// prober 는 claude 상태 점검(Probe)을 바이너리 경로별로 캐시하고, 같은 점검을 여러 호출이 동시에 부르면 하나만
// 실행해 나눠 쓴다(singleflight). 점검은 호출자의 ctx 와 분리된 자체 시간 제한으로 돌므로, 한 호출자가 취소해도
// 그 취소가 다른 호출자의 결과나 캐시를 오염시키지 않는다(기다림만 접힌다).
type prober struct {
	fn    func(ctx context.Context, bin string) Status
	okTTL time.Duration

	mu       sync.Mutex
	cache    map[string]probeEntry
	inflight map[string]*probeCall
	gen      int // invalidate 마다 올라간다: 그 전에 시작한 점검 결과는 캐시하지 않는다
}

type probeEntry struct {
	st Status
	at time.Time
}

type probeCall struct {
	done chan struct{}
	st   Status
}

const (
	probeFailTTL = 10 * time.Second // 실패는 짧게만 캐시한다
	probeTimeout = 50 * time.Second
)

func newProber(fn func(ctx context.Context, bin string) Status) *prober {
	return &prober{fn: fn, okTTL: 5 * time.Minute, cache: map[string]probeEntry{}, inflight: map[string]*probeCall{}}
}

func (p *prober) invalidate() {
	p.mu.Lock()
	p.cache = map[string]probeEntry{}
	p.gen++
	p.mu.Unlock()
}

// get 은 bin 의 점검 결과를 돌려준다. err 는 호출자의 ctx 가 끝났다는 뜻일 뿐 점검 결과가 아니다.
func (p *prober) get(ctx context.Context, bin string) (Status, error) {
	p.mu.Lock()
	if e, ok := p.cache[bin]; ok {
		ttl := p.okTTL
		if !e.st.Usable {
			ttl = probeFailTTL
		}
		if time.Since(e.at) <= ttl {
			p.mu.Unlock()
			return e.st, nil
		}
	}
	call := p.inflight[bin]
	if call == nil {
		call = &probeCall{done: make(chan struct{})}
		p.inflight[bin] = call
		go p.run(bin, call, p.gen)
	}
	p.mu.Unlock()
	select {
	case <-call.done:
		return call.st, nil
	case <-ctx.Done():
		return Status{}, ctx.Err()
	}
}

func (p *prober) run(bin string, call *probeCall, gen int) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	st := p.fn(ctx, bin)
	p.mu.Lock()
	call.st = st
	if gen == p.gen {
		p.cache[bin] = probeEntry{st: st, at: time.Now()}
	}
	delete(p.inflight, bin)
	p.mu.Unlock()
	close(call.done)
}
