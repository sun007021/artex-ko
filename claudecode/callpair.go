package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"time"
)

// 호출 짝맞춤(call pairing).
//
// claude 는 모델이 만든 tool_use 블록(스트림)과 별개로 MCP tools/call 요청을 loopback 서버에 보낸다. 이
// 둘은 같은 호출이지만 서로를 가리키는 ID 가 (거의) 없다. callPairer 는 이 둘을 잇는다:
//
//   - 스트림 쪽(streamTranslator)이 노출된 tool_use 를 expose 로 등록한다(content_block_stop 시점).
//     이번 요청에 노출하지 않은(숨겨진) 도구는 hide 로 "거절할 호출"로 적어 둔다.
//   - MCP 쪽(proc.callTool)이 match 로 자기 호출을 찾아 붙고, ARTEX 가 도구를 실행해 결과를 deliver 하면
//     그 결과를 응답으로 돌려준다.
//
// 짝은 tools/call 의 `_meta` 가 tool_use ID 를 실어 주면 그것으로, 아니면 (도구 이름, 정규화한 인자) 로,
// 그것도 안 맞으면 같은 이름의 가장 오래된 미할당 호출로 짓는다(마지막 둘은 로그에 남는다).
//
// 스트림은 항상 expose 를 먼저 하므로(MCP 호출은 그 뒤에 온다) match 는 기껏해야 claimWait 만 기다린다.

// await 는 모델이 부른 ARTEX 도구 호출 하나다.
type await struct {
	id      string          // tool_use id (agentcore 로 돌아가는 ID)
	mcpName string          // claude 가 부른 이름(접두사 제외)
	key     string          // 인자의 정규화 JSON(짝짓기용)
	input   json.RawMessage // 원본 인자
	claimed bool            // tools/call 이 이미 붙었다
	res     *mcpResult      // 결과(전달되면 채워짐)
	done    chan struct{}   // res 가 채워지면 닫힌다
}

type callPairer struct {
	logf func(string, ...any)

	mu       sync.Mutex
	awaiting []*await
	rejected map[string]bool // 이번 응답에서 노출하지 않은(숨겨진) 도구 호출의 name+key
	changed  chan struct{}   // awaiting/rejected 가 바뀌면 닫힌다
}

func newCallPairer(logf func(string, ...any)) *callPairer {
	return &callPairer{logf: logf, changed: make(chan struct{})}
}

func (c *callPairer) signalLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// reset 은 새 메시지가 시작됐음을 알린다: 직전 메시지의 모든 도구 호출은 끝났다.
func (c *callPairer) reset() {
	c.mu.Lock()
	c.awaiting, c.rejected = nil, nil
	c.signalLocked()
	c.mu.Unlock()
}

// expose 는 노출된 tool_use 하나를 등록한다.
func (c *callPairer) expose(id, mcpName string, input json.RawMessage) {
	c.mu.Lock()
	c.awaiting = append(c.awaiting, &await{id: id, mcpName: mcpName, key: canonArgs(input), input: input, done: make(chan struct{})})
	c.signalLocked()
	c.mu.Unlock()
}

// hide 는 이번 요청에서 숨겨진 ARTEX 도구의 호출을 "거절할 호출"로 적는다(수습 단계의 Bash 금지 등).
func (c *callPairer) hide(mcpName string, input json.RawMessage) {
	c.mu.Lock()
	if c.rejected == nil {
		c.rejected = map[string]bool{}
	}
	c.rejected[mcpName+"\x00"+canonArgs(input)] = true
	c.signalLocked()
	c.mu.Unlock()
}

// pending 은 아직 결과를 기다리는 호출들의 복사본이다.
func (c *callPairer) pending() []*await {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*await(nil), c.awaiting...)
}

// deliver 는 await 에 결과를 건넨다.
func (c *callPairer) deliver(a *await, r mcpResult) {
	c.mu.Lock()
	if a.res == nil {
		a.res = &r
		close(a.done)
	}
	c.mu.Unlock()
}

// callOutcome 은 match 의 결과다: 짝을 찾은 await, 또는 바로 돌려줄 결과.
type callOutcome struct {
	aw  *await
	res *mcpResult
}

func fail(text string) *callOutcome {
	return &callOutcome{res: &mcpResult{Text: []string{text}, IsError: true}}
}

// match 는 tools/call 하나의 짝을 찾는다. known 은 이 요청이 노출한 도구인지 알려 준다. toolUseID 는
// `_meta` 가 실어 준 tool_use ID(없으면 "")다.
func (c *callPairer) match(ctx context.Context, closed <-chan struct{}, known func(string) bool, name string, args json.RawMessage, toolUseID string) *callOutcome {
	key := canonArgs(args)
	deadline := time.NewTimer(claimWait)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		if !known(name) {
			c.mu.Unlock()
			return fail("unknown tool: " + name)
		}
		if c.rejected[name+"\x00"+key] {
			c.mu.Unlock()
			return fail("This tool is not available right now.")
		}
		aw := c.claimLocked(name, key, toolUseID, false)
		ch := c.changed
		c.mu.Unlock()
		if aw != nil {
			return &callOutcome{aw: aw}
		}
		select {
		case <-ch:
		case <-deadline.C:
			// 인자가 정규화 과정에서 달라졌을 수 있다(claude 가 스키마로 기본값을 채우는 등).
			// 같은 이름의 아직 안 붙은 호출 중 가장 오래된 것에 붙인다.
			c.mu.Lock()
			aw = c.claimLocked(name, key, toolUseID, true)
			c.mu.Unlock()
			if aw == nil {
				return fail("ARTEX could not match this tool call to a pending request; retry the call.")
			}
			if c.logf != nil {
				c.logf("도구 호출을 인자 일치 없이 이름만으로 짝지음 tool=%s id=%s", name, aw.id)
			}
			return &callOutcome{aw: aw}
		case <-closed:
			return fail("session closed")
		case <-ctx.Done():
			return fail("cancelled")
		}
	}
}

// claimLocked 는 아직 안 붙은 await 를 하나 골라 claimed 로 표시한다(c.mu 필요). toolUseID 가 있고 그 ID 의
// 호출이 있으면 그것을 우선한다. 없으면 (이름, 인자)로, byNameOnly 면 이름만으로 고른다.
func (c *callPairer) claimLocked(name, key, toolUseID string, byNameOnly bool) *await {
	if toolUseID != "" {
		for _, a := range c.awaiting {
			if a.id == toolUseID && !a.claimed && a.mcpName == name {
				a.claimed = true
				return a
			}
		}
	}
	for _, a := range c.awaiting {
		if a.claimed || a.mcpName != name {
			continue
		}
		if !byNameOnly && a.key != key {
			continue
		}
		a.claimed = true
		return a
	}
	return nil
}

// result 는 짝을 지은 호출의 결과를 기다린다. abandoned 는 claude 가 호출을 포기했다는 뜻이다(ctx 종료).
func (c *callPairer) result(ctx context.Context, closed <-chan struct{}, a *await) (res mcpResult, abandoned bool) {
	select {
	case <-a.done:
		return *a.res, false
	case <-closed:
		return mcpResult{Text: []string{"session closed"}, IsError: true}, false
	case <-ctx.Done():
		return mcpResult{Text: []string{"cancelled"}, IsError: true}, true
	}
}

// canonArgs 는 인자 JSON 을 키 순서와 공백에 무관한 문자열로 만든다.
func canonArgs(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	out, err := json.Marshal(v) // map 키는 정렬되어 직렬화된다
	if err != nil {
		return string(raw)
	}
	return string(out)
}
