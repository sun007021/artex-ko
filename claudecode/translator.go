package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// ---- 이벤트 번역 ----
//
// claude 의 stdout(stream-json 한 줄씩)을 ARTEX 의 llm.StreamEvent 와 턴 제어 이벤트(sEvent)로 바꾼다.
// 이 파일은 프로세스도 세션도 모른다: 줄을 받아 sink 로 이벤트를 내보내고, 노출된 도구 호출은 callPairer 에
// 등록할 뿐이다. 프로토콜 가정은 protocol.go 에 모여 있다.

type evKind int

const (
	evEmit   evKind = iota // llm.StreamEvent 하나
	evEnd                  // ARTEX 턴을 끝내는 메시지 종료(stop, toolIDs)
	evResult               // result 라인(턴 종료)
	evFail                 // 오류(assistant.error, SSE error, init 실패)
	evExit                 // 프로세스 종료
)

type sEvent struct {
	kind    evKind
	ev      llm.StreamEvent
	stop    string
	toolIDs []string
	isErr   bool
	err     *Error
}

// readState 는 한 줄 단위 번역에서 유지하는 블록 상태다.
type readState struct {
	blocks   map[int]*blockState
	exposed  []string // 이번 메시지에서 노출한 tool_use id
	stop     string
	outUsage int
}

type blockState struct {
	typ     string
	id      string
	name    string
	exposed bool
	input   strings.Builder
}

// translatorHooks 는 번역기가 바깥에 기대는 것들이다.
type translatorHooks struct {
	lookup  func(mcpName string) (artexName string, ok bool) // 이번 요청이 노출한 도구인가
	sink    func(sEvent)                                     // 이벤트 출구
	onLine  func()                                           // 줄 하나를 읽었다(무활동 감시용)
	onRetry func()                                           // api_retry 줄을 읽었다
	logf    func(string, ...any)
}

type streamTranslator struct {
	h     translatorHooks
	pair  *callPairer
	rs    readState
	limit limitHint // 마지막으로 거절된 rate_limit_event (판독 고루틴 하나만 읽고 쓴다)
}

func newStreamTranslator(pair *callPairer, h translatorHooks) *streamTranslator {
	return &streamTranslator{h: h, pair: pair, rs: readState{blocks: map[int]*blockState{}}}
}

func (t *streamTranslator) logf(format string, a ...any) {
	if t.h.logf != nil {
		t.h.logf(format, a...)
	}
}

// run 은 r 의 끝(EOF)까지 줄을 읽어 번역한다.
func (t *streamTranslator) run(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if tl := bytes.TrimSpace(line); len(tl) > 0 {
			t.line(tl)
		}
		if err != nil {
			return
		}
	}
}

func (t *streamTranslator) line(line []byte) {
	var l rawLine
	if json.Unmarshal(line, &l) != nil {
		return // JSON 이 아닌 줄(경고 등)은 무시
	}
	isRetry := l.Type == "system" && l.Subtype == "api_retry"
	if !isRetry && t.h.onLine != nil { // api_retry 는 횟수 상한이 있는 별도 경로(onRetry)로 센다
		t.h.onLine()
	}
	if l.ParentToolUseID != nil && *l.ParentToolUseID != "" {
		return // 서브에이전트 메시지(우리 세션엔 없다)
	}
	switch l.Type {
	case "system":
		switch l.Subtype {
		case "init":
			for _, srv := range l.MCPServers {
				if srv.Name == mcpServerName && (srv.Status == "failed" || srv.Status == "needs-auth") {
					msg := "claude 가 ARTEX MCP 서버에 연결하지 못했습니다(" + srv.Status + ")"
					if srv.Error != "" {
						msg += ": " + srv.Error
					}
					t.h.sink(sEvent{kind: evFail, err: &Error{Kind: KindProtocol, Message: msg}})
				}
			}
		case "api_retry":
			if t.h.onRetry != nil {
				t.h.onRetry()
			}
			t.logf("API 재시도 attempt=%d/%d status=%v kind=%s delay=%dms", l.Attempt, l.MaxRetries, l.ErrorStatus, l.errCategory(), l.RetryDelayMS)
		}
	case "stream_event":
		t.sse(l.Event)
	case "assistant":
		// max_output_tokens 는 장애가 아니라 출력 상한 도달이다 — stop_reason(max_tokens)으로 ARTEX 가 직접
		// 처리하므로 오류로 올리지 않는다.
		if cat := l.errCategory(); cat != "" && cat != "max_output_tokens" {
			t.h.sink(sEvent{kind: evFail, err: classify(cat, l.assistantText(), t.limit)})
		}
	case "rate_limit_event":
		if ri, ok := l.rateLimit(); ok && ri.Status == "rejected" {
			t.limit = limitHint{Reset: ri.resetTime(), Type: ri.RateLimitType}
		}
	case "result":
		var e *Error
		if l.IsError {
			text := l.Result
			if text == "" && len(l.Errors) > 0 {
				text = strings.Join(l.Errors, "; ")
			}
			e = classify("", text, t.limit)
			if e.Kind == KindUnknown {
				e.Message = orDefault(l.Subtype, "error") + ": " + e.Message
			}
		}
		t.h.sink(sEvent{kind: evResult, isErr: l.IsError, err: e})
	}
}

func (t *streamTranslator) sse(raw json.RawMessage) {
	rs := &t.rs
	var e sseEvent
	if len(raw) == 0 || json.Unmarshal(raw, &e) != nil {
		return
	}
	emit := func(ev llm.StreamEvent) { t.h.sink(sEvent{kind: evEmit, ev: ev}) }
	switch e.Type {
	case "message_start":
		// 새 메시지가 시작됐다는 건 직전 메시지의 모든 도구 호출이 끝났다는 뜻이다.
		t.pair.reset()
		rs.blocks = map[int]*blockState{}
		rs.exposed, rs.stop, rs.outUsage = nil, "", 0
		u := llm.Usage{}
		if e.Message.Usage != nil {
			u.InputTokens = e.Message.Usage.total()
			u.CacheReadTokens = e.Message.Usage.CacheReadInputTokens
			u.CacheWriteTokens = e.Message.Usage.CacheCreationInputTokens
		}
		emit(llm.StreamEvent{Type: llm.SEMessageStart, Usage: u})
	case "content_block_start":
		b := &blockState{typ: e.ContentBlock.Type, id: e.ContentBlock.ID, name: e.ContentBlock.Name}
		rs.blocks[e.Index] = b
		if b.typ == "tool_use" {
			if mcp, ok := strings.CutPrefix(b.name, toolPrefix); ok {
				if artex, known := t.h.lookup(mcp); known {
					b.exposed, b.name = true, mcp
					rs.exposed = append(rs.exposed, b.id)
					emit(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.id, ToolName: artex})
				}
			}
		}
	case "content_block_delta":
		b := rs.blocks[e.Index]
		if b == nil {
			return
		}
		switch e.Delta.Type {
		case "text_delta":
			if b.typ == "text" {
				emit(llm.StreamEvent{Type: llm.SETextDelta, Text: e.Delta.Text})
			}
		case "thinking_delta":
			if b.typ == "thinking" {
				emit(llm.StreamEvent{Type: llm.SEThinkingDelta, Text: e.Delta.Thinking})
			}
		case "signature_delta":
			if b.typ == "thinking" {
				emit(llm.StreamEvent{Type: llm.SEThinkingSignature, Text: e.Delta.Signature})
			}
		case "input_json_delta":
			if b.typ == "tool_use" {
				b.input.WriteString(e.Delta.PartialJSON)
				if b.exposed {
					emit(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: e.Delta.PartialJSON})
				}
			}
		}
	case "content_block_stop":
		b := rs.blocks[e.Index]
		if b == nil || b.typ != "tool_use" {
			return
		}
		in := json.RawMessage(strings.TrimSpace(b.input.String()))
		if len(in) == 0 || !json.Valid(in) {
			in = json.RawMessage("{}")
		}
		if b.exposed {
			t.pair.expose(b.id, b.name, in)
		} else if mcp, ok := strings.CutPrefix(b.name, toolPrefix); ok {
			// 이번 요청에선 숨겨진 ARTEX 도구(수습 단계의 Bash 금지 등): 실행하지 않고 즉시 거절한다.
			t.pair.hide(mcp, in)
		}
	case "message_delta":
		rs.stop = e.Delta.StopReason
		if e.Usage != nil {
			rs.outUsage = e.Usage.OutputTokens
		}
	case "message_stop":
		// 노출한 도구가 없는 tool_use 종료(내장 도구 거절 등)는 claude 가 알아서 이어 가므로
		// ARTEX 의 턴을 끝내지 않는다(출력만 합산하고 계속 읽는다).
		if rs.stop == "tool_use" && len(rs.exposed) == 0 {
			emit(llm.StreamEvent{Type: llm.SEMessageDelta, Usage: llm.Usage{OutputTokens: rs.outUsage}})
			return
		}
		emit(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: rs.stop, Usage: llm.Usage{OutputTokens: rs.outUsage}})
		emit(llm.StreamEvent{Type: llm.SEMessageStop})
		var ids []string
		if rs.stop == "tool_use" {
			ids = append(ids, rs.exposed...)
		}
		t.h.sink(sEvent{kind: evEnd, stop: rs.stop, toolIDs: ids})
	case "error":
		msg := "stream error"
		cat := ""
		if e.Error != nil {
			msg, cat = e.Error.Message, e.Error.Type
		}
		switch cat {
		case "overloaded_error":
			cat = "overloaded"
		case "rate_limit_error":
			cat = "rate_limit"
		case "authentication_error", "permission_error":
			cat = "authentication_failed"
		}
		t.h.sink(sEvent{kind: evFail, err: classify(cat, msg, t.limit)})
	}
}

// limitHint 는 rate_limit_event(status=rejected)가 알려 준 한도 정보다.
type limitHint struct {
	Reset time.Time // 한도가 풀리는 시각(모르면 zero)
	Type  string    // five_hour | seven_day | seven_day_opus | ... (모르면 "")
}
