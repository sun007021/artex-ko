package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Autumn-27/artex/llmauth"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/artex/siwc"
	"github.com/Autumn-27/norma/llm"
)

// ChatGPT 구독(Sign in with ChatGPT) 인증 방식.
//
// 에이전트 루프·도구 호출·결과 품질은 기존 openai-responses 경로(norma 의
// FormatOpenAIResponses)를 그대로 쓴다. 바뀌는 것은 인증뿐이다:
//
//   - Authorization 의 Bearer 값을 정적 API Key 대신 계정의 OAuth 액세스 토큰으로, 요청 직전에
//     채운다(만료 임박 시 갱신, 401 이면 1회 갱신 후 재시도) — chatgptTransport.
//   - 공식 문서(models-and-inference, preview-limitations)의 요청 규칙을 요청 본문에 강제한다:
//     store=false, stream=true, 지원되지 않는 필드(max_output_tokens, temperature 등) 제거,
//     system 역할 입력 항목 금지, function 도구는 namespace 로 묶기(chatgpt_tools.go).
//   - 응답 스트림은 response.completed 로 끝나야 성공이다. response.failed / error /
//     response.incomplete / 도중에 끊긴 스트림은 오류로 바꿔 norma 에 전한다.
//   - 사용량 한도(subscription_sharing_usage_limit_exceeded)는 오류 문구에 코드를 남기고 사용자에게 한도
//     도달을 알리는 안내를 붙인다. 공식 문서가 구독 요청을 다른 결제 경로로 자동 전환하지 말라고 하므로
//     API Key 프로필로의 자동 장애 전환(IsQuotaExhaustedMessage, LLM 순환)의 대상이 아니다
//     (IsNoFailoverMessage 가 순환도 막는다).
//
// 자격 증명이 api.openai.com/v1/responses 이외의 곳으로 나가는 일은 전송 계층이 거부한다.

// AuthModeChatGPT 는 db.AuthModeChatGPT 와 같은 값이다(둘 다 llmauth.ModeChatGPT 의 별칭).
const AuthModeChatGPT = llmauth.ModeChatGPT

// ChatGPTBaseURL 은 구독 토큰으로 호출하는 유일한 API 베이스다. 프로필의 base_url 은 무시한다.
const ChatGPTBaseURL = siwc.Resource

const chatGPTResponsesEndpoint = ChatGPTBaseURL + "/responses"

// chatGPTPlaceholderKey 는 norma 가 Authorization 헤더를 만들 때 쓰는 자리표시자다(비밀 아님).
// 전송 계층이 실제 액세스 토큰으로 덮어쓴다.
const chatGPTPlaceholderKey = "chatgpt-oauth"

// TokenSource 는 계정의 유효한 액세스 토큰을 돌려준다. rejected 가 비어 있지 않으면 그 토큰이
// 401 을 받았다는 뜻이며, 구현은 1회 강제 갱신한다. *siwc.Session 이 구현한다.
// Config 는 == 로 비교되므로 구현은 포인터여야 한다.
type TokenSource interface {
	Token(ctx context.Context, rejected string) (string, error)
}

// NewChatGPTConfig 는 구독 프로필용 Config 를 만든다. 형식은 openai-responses 로 고정되고
// 스트리밍은 항상 켜진다(비스트리밍은 이 경로에서 지원되지 않는다).
//
// 모델 식별자는 코드에 두지 않는다: 계정의 모델 목록에서 사용자가 고른 값만 쓰며(서버가 저장 시
// 필수로 요구한다), 비어 있으면 그대로 비워 둬 요청이 명확하게 거부되게 한다.
func NewChatGPTConfig(model, proxy string, src TokenSource) Config {
	return Config{
		Format: llm.FormatOpenAIResponses, BaseURL: ChatGPTBaseURL, Model: strings.TrimSpace(model),
		Proxy: strings.TrimSpace(proxy), Stream: true, AuthMode: AuthModeChatGPT, TokenSource: src,
	}
}

// UsesChatGPT 는 이 Config 가 ChatGPT 구독 인증을 쓰는지 알려 준다.
func (c Config) UsesChatGPT() bool { return c.AuthMode == AuthModeChatGPT }

// chatGPTUnsupportedFields 는 구독 직접 경로가 거부하는 요청 최상위 필드다
// (siwc/token-sharing-open-source/preview-limitations "Unsupported request fields").
// 보내면 subscription_sharing_unsupported_capability(400)가 되므로 전송 직전에 지운다.
// 특히 max_output_tokens: 프로필의 max_tokens 는 이 모드에서 적용되지 않는다.
var chatGPTUnsupportedFields = []string{
	"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata", "moderation",
	"multi_agent", "prompt", "prompt_cache_retention", "safety_identifier", "temperature",
	"top_logprobs", "top_p", "truncation", "user", "previous_response_id",
}

// sanitizeChatGPTRequest 는 norma 가 만든 Responses 요청 본문을 구독 경로 규칙에 맞춘다
// (unsupported 필드 제거, store/stream 고정, system→developer, function 도구 namespace 화).
// wantStream 은 호출자가 원래 스트리밍을 요청했는지다(false 면 응답을 JSON 으로 모아 돌려준다).
func sanitizeChatGPTRequest(body []byte) (out []byte, wantStream bool, err error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return nil, false, fmt.Errorf("chatgpt: request body is not a JSON object")
	}
	wantStream = string(m["stream"]) == "true"
	for _, k := range chatGPTUnsupportedFields {
		delete(m, k)
	}
	m["store"] = json.RawMessage("false")
	m["stream"] = json.RawMessage("true")
	// 입력 항목 정리: 명시적 system 역할 입력은 거부된다 — 지침은 instructions 로 가야 하며, 혹시 남아 있으면
	// 같은 의미의 developer 역할로 바꾼다. function_call 이력에는 도구가 속한 namespace 를 붙인다.
	var items []map[string]json.RawMessage
	haveItems := false
	if raw, ok := m["input"]; ok && json.Unmarshal(raw, &items) == nil {
		haveItems = true
	}
	changed := false
	for _, it := range items {
		if string(it["role"]) == `"system"` {
			it["role"] = json.RawMessage(`"developer"`)
			changed = true
		}
	}
	if groupChatGPTTools(m, items) {
		changed = true
	}
	if haveItems && changed {
		if nb, err := json.Marshal(items); err == nil {
			m["input"] = nb
		}
	}
	out, err = json.Marshal(m)
	return out, wantStream, err
}

// chatgptTransport 는 구독 토큰을 주입하는 RoundTripper 다.
type chatgptTransport struct {
	base     http.RoundTripper
	src      TokenSource
	endpoint string // 허용되는 유일한 대상. 운영에서는 chatGPTResponsesEndpoint
}

func (t *chatgptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	dest := req.URL.Scheme + "://" + req.URL.Host + req.URL.Path
	if dest != t.endpoint || req.Method != http.MethodPost {
		// 구독 자격 증명은 정해진 Responses 엔드포인트 이외로는 절대 보내지 않는다
		// (리다이렉트로 다른 호스트에 닿는 경우도 같은 검사에 걸린다).
		closeRequestBody(req)
		return nil, fmt.Errorf("chatgpt: refusing to send plan credentials to %s", req.URL.Host)
	}
	raw, err := readRequestBody(req)
	if err != nil {
		return nil, err
	}
	body, wantStream, err := sanitizeChatGPTRequest(raw)
	if err != nil {
		return nil, err
	}
	ctx := req.Context()
	capt := llmrec.CaptureFrom(ctx)

	send := func(tok string) (*http.Response, error) {
		capt.AddSecret(tok) // 로깅·캡처에 토큰이 남지 않도록 정확한 값을 마스킹 목록에 올린다
		r2 := req.Clone(ctx)
		r2.Body = io.NopCloser(bytes.NewReader(body))
		r2.ContentLength = int64(len(body))
		r2.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		r2.Header.Set("Content-Type", "application/json")
		r2.Header.Set("Accept", "text/event-stream")
		r2.Header.Set("Authorization", "Bearer "+tok)
		return t.base.RoundTrip(r2)
	}

	tok, err := t.src.Token(ctx, "")
	if err != nil {
		return tokenErrorResponse(req, err)
	}
	resp, err := send(tok)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		// 토큰이 거부됐다(폐기·서버측 만료). 세션이 새 토큰을 내주면 한 번만 재시도한다. 방금 발급한
		// 토큰이 거부된 것이면(계정·워크스페이스·권한 문제) 세션은 같은 토큰을 돌려주고, 그때는 같은
		// 요청을 되풀이하지 않고 서버가 준 401 응답을 그대로 전한다.
		tok2, terr := t.src.Token(ctx, tok)
		switch {
		case terr != nil:
			discardBody(resp)
			return tokenErrorResponse(req, terr)
		case tok2 != tok:
			discardBody(resp)
			resp, err = send(tok2)
		}
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		resp = usageLimitResponse(req, resp)
	}
	if resp.StatusCode != http.StatusOK {
		return resp, nil // 오류 본문(코드 포함)은 norma 가 그대로 오류 문자열에 담는다
	}
	if wantStream {
		resp.Body = newResponsesGuard(resp.Body)
		return resp, nil
	}
	return aggregateStream(req, resp), nil
}

func discardBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

func closeRequestBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

// usageLimitResponse: 429 본문이 사용량 한도(subscription_sharing_usage_limit_exceeded)이면 402 로 바꾸고
// 사용자 안내를 덧붙인다. norma 는 429 를 일시적 한도로 보고 몇 번 재시도하지만, 이 한도는 사용자가
// ChatGPT 설정에서 풀기 전까지 계속되므로 재시도는 무의미하다. 한도 오류는 다른 결제 경로(API Key)로
// 자동 전환하지 않는다(IsNoFailoverMessage).
func usageLimitResponse(req *http.Request, resp *http.Response) *http.Response {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if err == nil && siwc.IsUsageLimit(siwc.ParseAPIError(resp.StatusCode, body, "").Code) {
		return synthResponse(req, http.StatusPaymentRequired, siwc.CodeUsageLimitExceeded,
			usageLimitMessage(siwc.ParseAPIError(resp.StatusCode, body, resp.Header.Get("x-request-id")).RequestID))
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return resp
}

// chatGPTUsageLimitHint 는 사용량 한도 오류에 붙는 사용자 안내다.
const chatGPTUsageLimitHint = "ChatGPT 플랜(또는 이 앱)의 사용량 한도에 도달했습니다. ChatGPT 설정의 Usage 에서 한도를 확인하세요. " +
	"ARTEX 는 요청을 다른 결제 경로(API Key)로 자동 전환하지 않습니다"

func usageLimitMessage(requestID string) string {
	if requestID != "" {
		return chatGPTUsageLimitHint + " (request_id=" + requestID + ")"
	}
	return chatGPTUsageLimitHint
}

func readRequestBody(req *http.Request) ([]byte, error) {
	defer closeRequestBody(req)
	if req.GetBody != nil {
		rc, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	if req.Body == nil {
		return nil, fmt.Errorf("chatgpt: empty request body")
	}
	return io.ReadAll(req.Body)
}

// tokenErrorResponse 는 토큰을 못 얻은 실패를 HTTP 응답으로 바꿔 준다. 전송 오류로 돌려주면 norma 가
// 같은 요청을 연결 오류로 재시도하므로 상태 코드로 의미를 나눠 합성한다:
//   - 재시도가 무의미한 실패(재로그인 필요, 연결 안 됨, 권한 없음, 등록 거부)는 401. norma 는 401 을
//     재시도하지 않고 곧바로 오류로 올린다. LLM 순환(llmpool)은 401 도 장애 전환 대상으로 보고
//     "고쳐질 때까지 실패하는" 하드 실패로 첫 발생에 회로를 연 채 다음 설정으로 넘어간다
//     (llmpool.shouldFailover / isHardFailure). 할당량 소진 전환(IsQuotaExhaustedMessage)은 문구
//     기반이라 이 오류로는 움직이지 않는다.
//   - 일시적 장애(인증 서버 연결 실패, 갱신 대기 중 등)는 503. norma 가 자체 백오프로 몇 번 재시도하고
//     (세션도 갱신 실패 뒤 백오프를 두므로 그 사이 인증 서버를 두드리지 않는다), 그래도 안 되면 순환이
//     켜진 경우 llmpool 이 5xx 를 일시 실패로 보고 다음 설정으로 넘어간다.
func tokenErrorResponse(req *http.Request, err error) (*http.Response, error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || req.Context().Err() != nil {
		return nil, err
	}
	code := siwc.CodeOf(err)
	status, msg := http.StatusServiceUnavailable, "ChatGPT 인증 서버에 일시적으로 연결할 수 없습니다. 잠시 후 다시 시도합니다"
	switch code {
	case siwc.CodeReauthRequired:
		status, msg = http.StatusUnauthorized, "ChatGPT 로그인이 만료되었거나 취소되었습니다. LLM 설정에서 다시 로그인해 주세요"
	case siwc.CodeNotConnected:
		status, msg = http.StatusUnauthorized, "ChatGPT 계정이 연결되어 있지 않습니다. LLM 설정에서 ChatGPT로 로그인해 주세요"
	case siwc.CodePlanUsageDisabled:
		status, msg = http.StatusUnauthorized, "ChatGPT 플랜 사용 권한("+llmauth.ScopePlanUsage+")이 허가되지 않았습니다. LLM 설정에서 다시 로그인해 권한을 허용해 주세요"
	case siwc.CodeInvalidClient:
		status, msg = http.StatusUnauthorized, "ChatGPT 가 이 앱 등록을 더는 받아들이지 않습니다. LLM 설정에서 다시 로그인해 앱을 새로 등록해 주세요"
	case "":
		code = "chatgpt_token_unavailable"
	}
	return synthResponse(req, status, "chatgpt_"+code, msg), nil
}

func synthResponse(req *http.Request, status int, code, msg string) *http.Response {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": msg}})
	return &http.Response{
		StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(b)),
		ContentLength: int64(len(b)), Request: req,
	}
}

// ---- 스트림 검사 ----

type eventKind int

const (
	evPass     eventKind = iota // 그대로 통과
	evComplete                  // response.completed — 유일한 정상 종료
	evMaxOut                    // response.incomplete(max_output_tokens) — norma 가 max_tokens 로 처리
	evFail                      // response.failed / error / 그 밖의 incomplete — 오류로 바꾼다
)

type eventInfo struct {
	kind     eventKind
	failMsg  string          // evFail: "<code>: <message>"
	response json.RawMessage // evComplete/evMaxOut: response 객체
}

const maxFailMsg = 500

// inspectEvent 는 SSE 이벤트 하나를 분류한다. event 는 `event:` 줄 이름, data 는 JSON 본문이다.
func inspectEvent(event string, data []byte) eventInfo {
	var f struct {
		Type     string `json:"type"`
		Code     string `json:"code"`
		Message  string `json:"message"`
		Response struct {
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &f) != nil {
		return eventInfo{kind: evPass} // 킵얼라이브·비JSON 은 norma 처럼 무시
	}
	typ := event
	if typ == "" {
		typ = f.Type
	}
	respRaw := func() json.RawMessage {
		var w struct {
			Response json.RawMessage `json:"response"`
		}
		_ = json.Unmarshal(data, &w)
		return w.Response
	}
	switch typ {
	case "response.completed":
		return eventInfo{kind: evComplete, response: respRaw()}
	case "response.incomplete":
		reason := ""
		if f.Response.IncompleteDetails != nil {
			reason = f.Response.IncompleteDetails.Reason
		}
		if reason == "max_output_tokens" {
			return eventInfo{kind: evMaxOut, response: respRaw()}
		}
		if reason == "" {
			reason = "unknown"
		}
		return eventInfo{kind: evFail, failMsg: "response_incomplete: " + reason}
	case "response.failed":
		code, msg := "", ""
		if f.Response.Error != nil {
			code, msg = f.Response.Error.Code, f.Response.Error.Message
		} else if f.Error != nil {
			code, msg = f.Error.Code, f.Error.Message
		}
		return eventInfo{kind: evFail, failMsg: failMessage(code, msg)}
	case "error":
		code, msg := f.Code, f.Message
		if f.Error != nil {
			if f.Error.Code != "" {
				code = f.Error.Code
			}
			if f.Error.Message != "" {
				msg = f.Error.Message
			}
		}
		return eventInfo{kind: evFail, failMsg: failMessage(code, msg)}
	}
	return eventInfo{kind: evPass}
}

// failMessage 는 "<code>: <message>" 를 만든다. norma 는 오류 이벤트에서 message 만 읽으므로
// 코드를 메시지 앞에 붙여야 subscription_sharing_usage_limit_exceeded 같은 코드가 오류 문자열
// (과 IsNoFailoverMessage)까지 전달된다. 사용량 한도는 원문 대신 사용자 안내를 싣는다.
func failMessage(code, msg string) string {
	if code == "" {
		code = "unknown_error"
	}
	if siwc.IsUsageLimit(code) {
		msg = chatGPTUsageLimitHint
	}
	s := code
	if msg = strings.TrimSpace(msg); msg != "" {
		s += ": " + msg
	}
	if len(s) > maxFailMsg {
		s = s[:maxFailMsg]
	}
	return s
}

func errorEventBlock(msg string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"message": msg}})
	return []byte("event: error\ndata: " + string(b) + "\n\n")
}

// maxSSEBlock 은 SSE 이벤트 한 건의 상한이다(response.completed 는 전체 출력을 담는다).
const maxSSEBlock = 64 << 20

// responsesGuard 는 구독 경로의 SSE 응답을 검사하며 중계한다. response.completed 가 보이기 전에
// 스트림이 끝나면 정상 EOF 대신 io.ErrUnexpectedEOF 를 돌려 norma 가 오류로 처리하게 한다.
type responsesGuard struct {
	src      io.ReadCloser
	r        *bufio.Reader
	out      bytes.Buffer
	terminal bool
	err      error
}

func newResponsesGuard(src io.ReadCloser) *responsesGuard {
	return &responsesGuard{src: src, r: bufio.NewReaderSize(src, 64*1024)}
}

func (g *responsesGuard) Close() error { return g.src.Close() }

func (g *responsesGuard) Read(p []byte) (int, error) {
	for g.out.Len() == 0 {
		if g.err != nil {
			return 0, g.err
		}
		g.fill()
	}
	return g.out.Read(p)
}

// fill 는 이벤트 한 블록(빈 줄까지)을 읽어 검사한 뒤 g.out 에 쓴다.
func (g *responsesGuard) fill() {
	var block [][]byte
	size := 0
	for {
		line, err := g.r.ReadBytes('\n')
		size += len(line)
		if size > maxSSEBlock {
			g.err = fmt.Errorf("chatgpt: SSE event too large")
			return
		}
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) > 0 {
			block = append(block, append([]byte(nil), trimmed...))
		}
		if err != nil {
			if len(block) > 0 {
				g.process(block)
			}
			switch {
			case errors.Is(err, io.EOF) && g.terminal:
				g.err = io.EOF
			case errors.Is(err, io.EOF):
				// response.completed 없이 끝난 스트림은 실패다.
				g.err = io.ErrUnexpectedEOF
			default:
				g.err = err
			}
			return
		}
		if len(trimmed) == 0 && len(block) > 0 {
			g.process(block)
			return
		}
	}
}

func (g *responsesGuard) process(block [][]byte) {
	event, data := parseSSEBlock(block)
	if data == nil {
		writeBlock(&g.out, block) // 주석·킵얼라이브
		return
	}
	info := inspectEvent(event, data)
	switch info.kind {
	case evComplete, evMaxOut:
		g.terminal = true
		writeBlock(&g.out, block)
	case evFail:
		g.terminal = true // 오류 이벤트가 종료다. 뒤따르는 EOF 를 또 오류로 만들지 않는다
		g.out.Write(errorEventBlock(info.failMsg))
	default:
		// 도구 호출 이름에 namespace 접두사가 붙어 오면 ARTEX 도구 이름으로 되돌린다.
		if nd, ok := plainEventTools(data); ok {
			block = replaceData(block, nd)
		}
		writeBlock(&g.out, block)
	}
}

// replaceData swaps the data lines of an SSE block for a single data line carrying nd.
func replaceData(block [][]byte, nd []byte) [][]byte {
	out := make([][]byte, 0, len(block))
	done := false
	for _, l := range block {
		if bytes.HasPrefix(l, []byte("data:")) {
			if !done {
				out = append(out, append([]byte("data: "), nd...))
				done = true
			}
			continue
		}
		out = append(out, l)
	}
	return out
}

func writeBlock(w *bytes.Buffer, block [][]byte) {
	for _, l := range block {
		w.Write(l)
		w.WriteByte('\n')
	}
	w.WriteByte('\n')
}

// parseSSEBlock 은 `event:` 이름과 `data:` 줄들을 합친 본문을 돌려준다. data 줄이 없으면 nil.
func parseSSEBlock(block [][]byte) (event string, data []byte) {
	var buf bytes.Buffer
	has := false
	for _, l := range block {
		switch {
		case bytes.HasPrefix(l, []byte("event:")):
			event = strings.TrimSpace(string(l[len("event:"):]))
		case bytes.HasPrefix(l, []byte("data:")):
			d := l[len("data:"):]
			d = bytes.TrimPrefix(d, []byte(" "))
			if has {
				buf.WriteByte('\n')
			}
			buf.Write(d)
			has = true
		}
	}
	if !has {
		return event, nil
	}
	return event, buf.Bytes()
}

// aggregateStream 은 비스트리밍 호출자(Provider.Complete)를 위해 SSE 를 끝까지 읽어, 완료된
// response 객체를 일반 JSON 응답처럼 돌려준다. 구독 경로는 항상 stream=true 로 호출되므로
// 이 변환이 비스트리밍 요청을 같은 품질로 지원한다. 실패는 {"error":{"message":..}} 로 돌려
// norma 가 오류로 처리한다.
func aggregateStream(req *http.Request, resp *http.Response) *http.Response {
	defer resp.Body.Close()
	col := newSSECollector(resp.Body)
	respObj, failMsg := col.collect()
	if failMsg == "" {
		respObj, _ = plainResponseTools(respObj)
	}
	status := http.StatusOK
	var payload []byte
	switch {
	case failMsg != "":
		payload, _ = json.Marshal(map[string]any{"error": map[string]any{"message": failMsg}})
	default:
		payload = respObj
	}
	h := resp.Header.Clone()
	h.Set("Content-Type", "application/json")
	h.Del("Content-Length")
	return &http.Response{
		StatusCode: status, Status: "200 OK", Proto: resp.Proto, ProtoMajor: resp.ProtoMajor, ProtoMinor: resp.ProtoMinor,
		Header: h, Body: io.NopCloser(bytes.NewReader(payload)), ContentLength: int64(len(payload)), Request: req,
	}
}

// sseCollector reads events without rewriting them.
type sseCollector struct{ r *bufio.Reader }

func newSSECollector(src io.Reader) *sseCollector {
	return &sseCollector{r: bufio.NewReaderSize(src, 64*1024)}
}

func (c *sseCollector) collect() (response []byte, failMsg string) {
	var block [][]byte
	size := 0
	flush := func() (done bool) {
		if len(block) == 0 {
			return false
		}
		ev, data := parseSSEBlock(block)
		block, size = nil, 0
		if data == nil {
			return false
		}
		info := inspectEvent(ev, data)
		switch info.kind {
		case evComplete, evMaxOut:
			response = info.response
			return true
		case evFail:
			failMsg = info.failMsg
			return true
		}
		return false
	}
	for {
		line, err := c.r.ReadBytes('\n')
		size += len(line)
		if size > maxSSEBlock {
			return nil, "stream_interrupted: event too large"
		}
		if t := bytes.TrimRight(line, "\r\n"); len(t) > 0 {
			block = append(block, append([]byte(nil), t...))
		} else if flush() {
			return response, failMsg
		}
		if err != nil {
			if flush() {
				return response, failMsg
			}
			return nil, "stream_interrupted: response ended before response.completed"
		}
	}
}

// ---- 계정 모델 카탈로그 ----

// ChatGPTModel 은 계정에서 선택할 수 있는 모델 한 건이다.
type ChatGPTModel struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

// ErrChatGPTModelCatalog 는 카탈로그 응답이 문서화된 모양이 아닐 때의 오류다.
var ErrChatGPTModelCatalog = errors.New("chatgpt: unexpected model catalog")

// FetchChatGPTModels 는 같은 토큰으로 GET https://api.openai.com/v1/models 를 호출해
// visibility == "list" 인 항목만 서버 순서대로 돌려준다(slug 를 model 로, display_name 을 UI 에).
// 계정을 바꾸면 목록을 다시 불러와야 한다.
func FetchChatGPTModels(ctx context.Context, proxy string, src TokenSource) ([]ChatGPTModel, error) {
	tr, err := sharedTransport(proxy)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Transport: tr, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return fetchChatGPTModels(ctx, hc, ChatGPTBaseURL+"/models", src)
}

func fetchChatGPTModels(ctx context.Context, hc *http.Client, url string, src TokenSource) ([]ChatGPTModel, error) {
	get := func(tok string) (*http.Response, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("chatgpt: model catalog request failed: %w", scrubURLError(err))
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return resp, b, err
	}
	tok, err := src.Token(ctx, "")
	if err != nil {
		return nil, err
	}
	resp, body, err := get(tok)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		// 세션이 새 토큰을 내줄 때만 다시 시도한다(방금 발급한 토큰의 401 은 토큰 문제가 아니다).
		tok2, terr := src.Token(ctx, tok)
		if terr != nil {
			return nil, terr
		}
		if tok2 != tok {
			resp, body, err = get(tok2)
		}
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, siwc.ParseAPIError(resp.StatusCode, body, resp.Header.Get("x-request-id"))
	}
	var doc struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Models == nil {
		return nil, ErrChatGPTModelCatalog
	}
	out := []ChatGPTModel{}
	for _, m := range doc.Models {
		if m.Visibility != "list" {
			continue
		}
		if strings.TrimSpace(m.Slug) == "" || len(m.Slug) > 200 || strings.TrimSpace(m.DisplayName) == "" || len(m.DisplayName) > 200 {
			return nil, ErrChatGPTModelCatalog
		}
		out = append(out, ChatGPTModel{Slug: m.Slug, DisplayName: m.DisplayName})
	}
	return out, nil
}

// scrubURLError drops the request URL/headers from a *url.Error so a transport
// failure never echoes more than the cause.
func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}
