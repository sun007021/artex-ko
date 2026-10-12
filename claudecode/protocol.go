package claudecode

import (
	"encoding/json"
	"strings"
	"time"
)

// claude -p --output-format stream-json 의 한 줄(NDJSON). 이 파일이 출력 프로토콜에 대한 모든
// 가정을 담는다. 출처: https://code.claude.com/docs/en/headless (스트림 이벤트, api_retry,
// system/init), https://code.claude.com/docs/en/agent-sdk/typescript (SDKMessage 타입).
//
// 알려진 형태:
//
//	{"type":"system","subtype":"init","session_id":"..","model":"..","mcp_servers":[{"name":"artex","status":"connected"}],"tools":[..]}
//	{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":..,"error_status":429,"error":"rate_limit"}
//	{"type":"stream_event","event":{<Anthropic Messages SSE 이벤트>},"parent_tool_use_id":null,"session_id":".."}
//	{"type":"assistant","message":{..},"error":"rate_limit","parent_tool_use_id":null}   // 블록 단위 전체 메시지(중복이라 본문은 무시)
//	{"type":"user","message":{..}}                                                          // 도구 결과 에코(무시)
//	{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1760000000,"rateLimitType":"five_hour"}}
//	{"type":"result","subtype":"success","is_error":false,"result":"..","usage":{..},"total_cost_usd":..}
//
// 알 수 없는 타입·필드는 모두 무시한다(전방 호환). 필드 이름이 문서와 다르면 아래 구조체만 고치면 된다.
type rawLine struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	Event           json.RawMessage `json:"event"`
	Message         json.RawMessage `json:"message"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	Error           json.RawMessage `json:"error"` // assistant/api_retry: 문자열 범주
	ErrorStatus     *int            `json:"error_status"`
	IsError         bool            `json:"is_error"`
	Result          string          `json:"result"`
	Errors          []string        `json:"errors"`
	Model           string          `json:"model"`
	MCPServers      []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"mcp_servers"`
	Tools         []string        `json:"tools"`
	Attempt       int             `json:"attempt"`
	MaxRetries    int             `json:"max_retries"`
	RetryDelayMS  int             `json:"retry_delay_ms"`
	RateLimitInfo json.RawMessage `json:"rate_limit_info"`
	TotalCostUSD  float64         `json:"total_cost_usd"`
	DurationMS    int             `json:"duration_ms"`
	NumTurns      int             `json:"num_turns"`
	StopReason    string          `json:"stop_reason"`
}

// errCategory 는 assistant/api_retry 의 error 필드(문자열)를 읽는다.
func (l *rawLine) errCategory() string {
	var s string
	if len(l.Error) > 0 && json.Unmarshal(l.Error, &s) == nil {
		return s
	}
	return ""
}

// assistantText 는 assistant 메시지 본문의 text 블록을 이어 붙인다(오류 문구를 읽을 때 쓴다).
func (l *rawLine) assistantText() string {
	var m struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(l.Message, &m) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// rateLimitInfo 는 rate_limit_event 의 본문이다.
type rateLimitInfo struct {
	Status        string `json:"status"` // allowed | allowed_warning | rejected
	ResetsAt      int64  `json:"resetsAt"`
	RateLimitType string `json:"rateLimitType"`
}

func (l *rawLine) rateLimit() (rateLimitInfo, bool) {
	var ri rateLimitInfo
	if len(l.RateLimitInfo) == 0 || json.Unmarshal(l.RateLimitInfo, &ri) != nil {
		return ri, false
	}
	return ri, true
}

func (ri rateLimitInfo) resetTime() time.Time {
	if ri.ResetsAt <= 0 {
		return time.Time{}
	}
	// 초 단위 epoch 가 일반적이다. 밀리초로 보이면 변환한다.
	if ri.ResetsAt > 1e12 {
		return time.UnixMilli(ri.ResetsAt)
	}
	return time.Unix(ri.ResetsAt, 0)
}

// sseEvent 는 stream_event.event (Anthropic Messages 스트리밍 이벤트)다.
type sseEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage   *sseUsage `json:"usage"`
	Message struct {
		Usage *sseUsage `json:"usage"`
	} `json:"message"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type sseUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// input 은 llm.Usage 의 입력 쪽(전체 입력 = 캐시 포함). norma 의 Anthropic 어댑터와 같은 규칙이다:
// message_start 는 입력만, message_delta 는 출력만 담아 합산 시 이중 계산을 피한다.
func (u *sseUsage) total() int {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}
