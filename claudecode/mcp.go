package claudecode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loopback MCP 서버. claude 는 이 서버로 ARTEX 도구 목록을 받고 tools/call 을 보낸다.
//
// 이 서버의 핸들러는 아무것도 실행하지 않는다. tools/call 은 해당 세션이 "agentcore 가 도구를 실행한
// 결과"를 건네줄 때까지 응답을 열어 둔 채 기다린다(session.go). 그래서 권한 검사·훅·캡처·전사·승인
// 같은 부작용은 전부 API 경로와 같은 agentcore 쪽에서 일어난다.
//
// 보안: 127.0.0.1 에만 바인드한다. 세션마다 256비트 무작위 토큰 경로(/mcp/<token>)를 쓰고, Host 헤더가
// 루프백이 아니거나 Origin 헤더가 있으면(브라우저/DNS 리바인딩) 거절한다. 토큰은 ARTEX 자신의 도구
// 엔드포인트를 가리키는 값일 뿐 Claude 자격 증명이 아니다.

// mcpTool 은 tools/list 의 한 항목이다.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
	Meta        map[string]any `json:"_meta,omitempty"`
}

// mcpResult 는 tools/call 의 결과다.
type mcpResult struct {
	Text    []string
	IsError bool
}

func (r mcpResult) wire() map[string]any {
	content := make([]map[string]any, 0, len(r.Text))
	for _, t := range r.Text {
		content = append(content, map[string]any{"type": "text", "text": t})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	return map[string]any{"content": content, "isError": r.IsError}
}

// mcpEndpoint 는 세션 하나가 구현하는 MCP 서버 쪽 동작이다.
type mcpEndpoint interface {
	listTools() []mcpTool
	// toolUseID 는 tools/call 의 `_meta` 가 실어 준 tool_use ID 다(없으면 "").
	callTool(ctx context.Context, name string, args json.RawMessage, toolUseID string) mcpResult
}

type mcpHub struct {
	mu        sync.RWMutex
	endpoints map[string]mcpEndpoint
	ln        net.Listener
	srv       *http.Server
	addr      string // 127.0.0.1:port
}

// startHub 는 루프백 HTTP 서버를 띄운다.
func startHub(logf func(string, ...any)) (*mcpHub, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("claude-code: MCP 루프백 서버를 시작하지 못했습니다: %w", err)
	}
	h := &mcpHub{endpoints: map[string]mcpEndpoint{}, ln: ln, addr: ln.Addr().String()}
	h.srv = &http.Server{
		Handler:           http.HandlerFunc(h.serve),
		ReadHeaderTimeout: 10 * time.Second,
		// 응답 타임아웃을 두지 않는다: tools/call 은 ARTEX 가 도구를 끝낼 때까지(수 시간) 열려 있다.
	}
	go func() {
		if err := h.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && logf != nil {
			logf("[claude-code] MCP 루프백 서버가 멈췄습니다: %v", err)
		}
	}()
	return h, nil
}

func (h *mcpHub) close() {
	_ = h.srv.Close()
}

func newToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (h *mcpHub) register(ep mcpEndpoint) (token string) {
	token = newToken()
	h.mu.Lock()
	h.endpoints[token] = ep
	h.mu.Unlock()
	return token
}

func (h *mcpHub) unregister(token string) {
	h.mu.Lock()
	delete(h.endpoints, token)
	h.mu.Unlock()
}

func (h *mcpHub) url(token string) string {
	return "http://" + h.addr + "/mcp/" + token
}

func (h *mcpHub) loopbackHost(host string) bool {
	hn, _, err := net.SplitHostPort(host)
	if err != nil {
		hn = host
	}
	hn = strings.Trim(hn, "[]")
	return hn == "127.0.0.1" || hn == "localhost" || hn == "::1"
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const maxMCPBody = 16 << 20

func (h *mcpHub) serve(w http.ResponseWriter, r *http.Request) {
	if !h.loopbackHost(r.Host) || r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	tok, ok := strings.CutPrefix(r.URL.Path, "/mcp/")
	if !ok || tok == "" {
		http.NotFound(w, r)
		return
	}
	h.mu.RLock()
	ep := h.endpoints[tok]
	h.mu.RUnlock()
	if ep == nil {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodGet, http.MethodDelete:
		// 서버 발신 SSE 스트림과 세션 종료는 지원하지 않는다(스펙상 405 허용).
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMCPBody+1))
	if err != nil || len(body) > maxMCPBody {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) > 0 && body[0] == '[' { // JSON-RPC 배치
		var reqs []rpcRequest
		if json.Unmarshal(body, &reqs) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var resps []rpcResponse
		for _, q := range reqs {
			if resp, has := h.dispatch(r.Context(), ep, q); has {
				resps = append(resps, resp)
			}
		}
		if len(resps) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, resps)
		return
	}
	var q rpcRequest
	if json.Unmarshal(body, &q) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	resp, has := h.dispatch(r.Context(), ep, q)
	if !has {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

var supportedProtocols = map[string]bool{
	"2025-11-25": true, "2025-06-18": true, "2025-03-26": true, "2024-11-05": true,
}

// dispatch 는 JSON-RPC 요청 하나를 처리한다. has=false 는 알림(응답 없음)이다.
func (h *mcpHub) dispatch(ctx context.Context, ep mcpEndpoint, q rpcRequest) (resp rpcResponse, has bool) {
	if len(q.ID) == 0 || string(q.ID) == "null" {
		return rpcResponse{}, false // notifications/initialized, notifications/cancelled 등
	}
	resp = rpcResponse{JSONRPC: "2.0", ID: q.ID}
	switch q.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(q.Params, &p)
		pv := "2025-06-18"
		if supportedProtocols[p.ProtocolVersion] {
			pv = p.ProtocolVersion
		}
		resp.Result = map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": "1"},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": ep.listTools()}
	case "resources/list":
		resp.Result = map[string]any{"resources": []any{}}
	case "resources/templates/list":
		resp.Result = map[string]any{"resourceTemplates": []any{}}
	case "prompts/list":
		resp.Result = map[string]any{"prompts": []any{}}
	case "tools/call":
		var p struct {
			Name      string                     `json:"name"`
			Arguments json.RawMessage            `json:"arguments"`
			Meta      map[string]json.RawMessage `json:"_meta"`
		}
		if json.Unmarshal(q.Params, &p) != nil || p.Name == "" {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
			return resp, true
		}
		resp.Result = ep.callTool(ctx, p.Name, p.Arguments, toolUseIDFromMeta(p.Meta)).wire()
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	return resp, true
}

// toolUseMetaKeys 는 tools/call 의 `_meta` 에서 tool_use ID 를 찾아 볼 키들이다. Claude Code 는
// "claudecode/toolUseId" 로 실어 보낸다(문서화된 계약이 아니라서 흔한 변형도 함께 본다). ID 가 없으면
// 짝맞춤은 (이름, 인자) 기준으로 대체된다.
var toolUseMetaKeys = []string{"claudecode/toolUseId", "claudecode/toolUseID", "toolUseId", "tool_use_id", "anthropic/toolUseId"}

func toolUseIDFromMeta(meta map[string]json.RawMessage) string {
	for _, k := range toolUseMetaKeys {
		var id string
		if raw, ok := meta[k]; ok && json.Unmarshal(raw, &id) == nil && strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	return ""
}
