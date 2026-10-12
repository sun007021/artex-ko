package claudecode

import (
	"context"
	"errors"
	"time"

	"github.com/Autumn-27/norma/llm"
)

var errConsumerStopped = errors.New("claudecode: consumer stopped")

type turnOut struct {
	sig    string // 이번 턴에 만든 assistant 메시지의 서명
	parked bool   // 도구 호출로 끝났다(결과 대기)
}

// turnReader 는 한 턴 동안 proc 의 이벤트를 ARTEX 로 흘려 보내는 소비자다.
type turnReader struct {
	s     *session
	p     *proc
	yield func(llm.StreamEvent, error) bool

	acc        *llm.Accumulator
	ended      bool // 모델 메시지가 끝났다(result 또는 종료를 기다리는 중)
	resultWait <-chan time.Time
}

func (r *turnReader) finish(parked bool) turnOut {
	return turnOut{sig: msgSig(r.acc.Message()), parked: parked}
}

// handle 은 이벤트 하나를 처리한다. done 이면 턴이 끝난 것이다(out/err 가 결과).
func (r *turnReader) handle(ev sEvent) (out turnOut, done bool, err error) {
	switch ev.kind {
	case evEmit:
		if r.ended {
			return turnOut{}, false, nil
		}
		r.acc.Add(ev.ev)
		if !r.yield(ev.ev, nil) {
			return turnOut{}, true, errConsumerStopped
		}
	case evEnd:
		r.ended = true
		if ev.stop == "tool_use" && len(ev.toolIDs) > 0 {
			return r.finish(true), true, nil
		}
		r.resultWait = time.After(resultWaitTimeout)
	case evResult:
		if ev.isErr {
			if ev.err != nil && (!r.ended || ev.err.Kind != KindUnknown) {
				return turnOut{}, true, ev.err
			}
			r.s.logf("result 가 오류를 보고했지만 응답은 완결됨: %v", ev.err)
		}
		if !r.ended { // 메시지 없이 끝난 턴: 빈 응답으로 마감한다
			for _, e := range []llm.StreamEvent{{Type: llm.SEMessageDelta, StopReason: "end_turn"}, {Type: llm.SEMessageStop}} {
				r.acc.Add(e)
				if !r.yield(e, nil) {
					return turnOut{}, true, errConsumerStopped
				}
			}
		}
		return r.finish(false), true, nil
	case evFail:
		return turnOut{}, true, ev.err
	case evExit:
		return r.exited(ev.err)
	}
	return turnOut{}, false, nil
}

// exited 는 프로세스가 끝났을 때의 마감이다: 응답이 완결됐으면 정상, 아니면 크래시 오류.
func (r *turnReader) exited(cause *Error) (turnOut, bool, error) {
	if r.ended {
		return r.finish(false), true, nil
	}
	if cause == nil {
		cause = r.p.terminated()
	}
	return turnOut{}, true, cause
}

// consume 은 한 턴의 이벤트를 ARTEX 로 흘려 보낸다. 모델이 ARTEX 도구를 부르며 끝나면 parked 로 돌아오고,
// 보류된 tools/call 은 열어 둔다. 그 외 종료는 claude 가 result 를 낼 때까지 기다려 stdout 을 비운다.
//
// 세 가지 안전장치가 있다. (1) 프로세스가 끝났는데 종료 이벤트가 유실됐어도(이벤트 채널이 가득 차 있었다 등)
// closed 로 알아채 남은 이벤트를 비우고 크래시 오류를 만든다. (2) 응답 중 stdout 이 stall 동안 조용하면 멈춘
// 프로세스로 보고 오류로 끝낸다(슬롯을 무기한 쥐지 않게). (3) ctx 취소.
func (s *session) consume(ctx context.Context, p *proc, yield func(llm.StreamEvent, error) bool) (turnOut, error) {
	r := &turnReader{s: s, p: p, yield: yield, acc: llm.NewAccumulator()}
	p.act.beginTurn()
	stall := s.m.stall
	var tick <-chan time.Time
	if stall > 0 {
		t := time.NewTicker(max(stall/5, 5*time.Millisecond))
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case ev := <-p.events:
			if out, done, err := r.handle(ev); done {
				return out, err
			}
		case <-p.closed:
			// 종료 이벤트가 채널 안에 있으면 순서대로 처리하고, 유실됐으면 직접 만든다.
			for {
				select {
				case ev := <-p.events:
					if out, done, err := r.handle(ev); done {
						return out, err
					}
					continue
				default:
				}
				break
			}
			out, _, err := r.exited(nil)
			return out, err
		case <-r.resultWait:
			s.logf("result 를 기다리다 시간 초과(응답은 완결됨) — 다음 호출에서 재동기화")
			p.broken.Store(true)
			return r.finish(false), nil
		case <-tick:
			if idle := p.act.idle(); idle > stall {
				s.logf("claude 출력이 %s 동안 없어 멈춘 것으로 보고 정리합니다 session=%s", idle.Round(time.Second), s.key)
				return turnOut{}, errf(KindStalled, "claude 가 %s 동안 출력이 없어 중단했습니다(응답을 만들다 멈춘 것으로 보입니다)", stall.Round(time.Second))
			}
		case <-ctx.Done():
			return turnOut{}, ctx.Err()
		}
	}
}
