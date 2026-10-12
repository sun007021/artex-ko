package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 이 파일은 테스트용 가짜 `claude` 실행 파일이다. 테스트 바이너리 자신이 CLAUDECODE_FAKE=1 환경에서
// claude 처럼 동작한다(--version, auth status, 그리고 -p stream-json 세션). 실제 claude 의 출력
// 프로토콜에 대한 이 패키지의 가정(protocol.go)을 그대로 흉내 내며, 실제 바이너리와의 호환을 증명하지는
// 않는다 — 그건 사용자가 자기 claude 로 연결 테스트를 해서 확인해야 한다.

func TestMain(m *testing.M) {
	if os.Getenv("CLAUDECODE_FAKE") == "1" {
		os.Exit(fakeMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type fakeBlock struct {
	Type  string          `json:"type"` // text | thinking | tool
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"` // tool: ARTEX 이름(접두사 제외)
	Args  json.RawMessage `json:"args,omitempty"`
	ID    string          `json:"id,omitempty"`
	Hasty bool            `json:"hasty,omitempty"` // MCP 호출을 content_block_stop 직후에 바로 한다(실제 claude 의 스트리밍 실행)
}

type fakeMsg struct {
	Blocks   []fakeBlock `json:"blocks"`
	Stop     string      `json:"stop"`               // tool_use | end_turn | max_tokens
	Parallel bool        `json:"parallel,omitempty"` // 도구 호출을 동시에 보낸다(아니면 순서대로)
	DelayMS  int         `json:"delay_ms,omitempty"` // message_stop 전 지연(생성 시간 흉내)
	ErrCat   string      `json:"err_cat,omitempty"`  // assistant.error 범주
	ErrText  string      `json:"err_text,omitempty"`
	Crash    bool        `json:"crash,omitempty"`
	// ExitAfter 는 턴의 마지막 줄(result)을 쓰자마자 프로세스를 끝낸다 — 출력 끝부분 유실 경합을 재현한다.
	ExitAfter bool `json:"exit_after,omitempty"`
}

func fakeEmit(v any) {
	b, _ := json.Marshal(v)
	b = append(b, '\n')
	os.Stdout.Write(b)
}

func fakeSSE(ev map[string]any) {
	fakeEmit(map[string]any{"type": "stream_event", "event": ev, "session_id": "fake", "parent_tool_use_id": nil})
}

func fakeLog(dir string, idx int, v any) {
	f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("proc-%d.log", idx)), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(v)
	f.Write(append(b, '\n'))
}

func fakeMain(args []string) int {
	if len(args) > 0 && args[0] == "--version" {
		v := os.Getenv("FAKE_VERSION")
		if v == "" {
			v = "2.1.300 (Claude Code)"
		}
		fmt.Println(v)
		return 0
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		switch os.Getenv("FAKE_AUTH") {
		case "none":
			fmt.Println(`{"loggedIn":false,"authMethod":"none"}`)
			return 1
		case "api_key":
			fmt.Println(`{"loggedIn":true,"authMethod":"api_key"}`)
		case "api_key_helper":
			fmt.Println(`{"loggedIn":true,"authMethod":"api_key_helper"}`)
		case "third_party":
			fmt.Println(`{"loggedIn":true,"authMethod":"third_party"}`)
		case "oauth_token":
			fmt.Println(`{"loggedIn":true,"authMethod":"oauth_token"}`)
		case "garbage":
			fmt.Println(`not json`)
			return 0
		default:
			fmt.Println(`{"loggedIn":true,"authMethod":"claude.ai"}`)
		}
		return 0
	}

	dir := os.Getenv("FAKE_DIR")
	idx := 0
	for ; ; idx++ {
		f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("proc-%d.claimed", idx)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			break
		}
	}
	// 호출 인자와 환경을 기록한다(플래그/환경 정제 검증용).
	sysFile, mcpFile := "", ""
	for i, a := range args {
		if a == "--system-prompt-file" && i+1 < len(args) {
			sysFile = args[i+1]
		}
		if a == "--mcp-config" && i+1 < len(args) {
			mcpFile = args[i+1]
		}
	}
	sysText := ""
	if sysFile != "" {
		b, _ := os.ReadFile(sysFile)
		sysText = string(b)
	}
	cwd, _ := os.Getwd()
	meta := map[string]any{"args": args, "env": os.Environ(), "pid": os.Getpid(), "system": sysText, "cwd": cwd}
	mb, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(dir, fmt.Sprintf("proc-%d.meta.json", idx)), mb, 0o644)

	var script []fakeMsg
	if b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("script-%d.json", idx))); err == nil {
		_ = json.Unmarshal(b, &script)
	}

	// MCP 클라이언트 흉내
	var mcpURL string
	if mcpFile != "" {
		b, _ := os.ReadFile(mcpFile)
		var cfg struct {
			MCPServers map[string]struct {
				URL string `json:"url"`
			} `json:"mcpServers"`
		}
		_ = json.Unmarshal(b, &cfg)
		mcpURL = cfg.MCPServers["artex"].URL
	}
	status := "connected"
	if mcpURL != "" {
		if _, err := fakeRPC(mcpURL, 1, "initialize", map[string]any{"protocolVersion": "2025-06-18"}); err != nil {
			status = "failed"
		}
		fakeRPC(mcpURL, 0, "notifications/initialized", nil)
		if res, err := fakeRPC(mcpURL, 2, "tools/list", nil); err == nil {
			var names []string
			if tl, ok := res["tools"].([]any); ok {
				for _, t := range tl {
					names = append(names, t.(map[string]any)["name"].(string))
				}
			}
			fakeLog(dir, idx, map[string]any{"k": "tools", "names": names})
		}
	}
	fakeEmit(map[string]any{"type": "system", "subtype": "init", "session_id": "fake", "model": "fake",
		"mcp_servers": []map[string]string{{"name": "artex", "status": status}}})

	next := 0
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var in struct {
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &in) != nil {
			continue
		}
		var texts []string
		for _, c := range in.Message.Content {
			texts = append(texts, c.Text)
		}
		fakeLog(dir, idx, map[string]any{"k": "input", "texts": texts, "t": time.Now().UnixNano()})
		// 한 사용자 입력 = 한 턴: end 가 나올 때까지 스크립트 메시지를 소비한다.
		for {
			var m fakeMsg
			if next < len(script) {
				m = script[next]
			} else {
				m = fakeMsg{Blocks: []fakeBlock{{Type: "text", Text: "done"}}, Stop: "end_turn"}
			}
			next++
			if fakeRunMessage(dir, idx, mcpURL, m, next) {
				break
			}
		}
	}
	return 0
}

// fakeRunMessage 는 메시지 하나를 내보낸다. 턴이 끝났으면 true.
func fakeRunMessage(dir string, idx int, mcpURL string, m fakeMsg, n int) bool {
	if m.Crash {
		fmt.Fprintln(os.Stderr, "boom: simulated crash")
		os.Exit(3)
	}
	if m.ErrCat != "" {
		fakeEmit(map[string]any{"type": "assistant", "error": m.ErrCat, "parent_tool_use_id": nil,
			"message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": m.ErrText}}}})
		fakeEmit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": m.ErrText})
		return true
	}
	fakeLog(dir, idx, map[string]any{"k": "gen_start", "t": time.Now().UnixNano()})
	fakeSSE(map[string]any{"type": "message_start", "message": map[string]any{"id": fmt.Sprintf("msg_%d", n),
		"usage": map[string]any{"input_tokens": 10, "cache_read_input_tokens": 5, "cache_creation_input_tokens": 2, "output_tokens": 1}}})
	type pend struct {
		name string
		args json.RawMessage
		ch   chan [2]any
	}
	var pending []*pend
	var content []map[string]any
	for i, b := range m.Blocks {
		switch b.Type {
		case "text":
			fakeSSE(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
			half := len(b.Text) / 2
			fakeSSE(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": b.Text[:half]}})
			fakeSSE(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": b.Text[half:]}})
			fakeSSE(map[string]any{"type": "content_block_stop", "index": i})
			content = append(content, map[string]any{"type": "text", "text": b.Text})
		case "thinking":
			fakeSSE(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
			fakeSSE(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "thinking_delta", "thinking": b.Text}})
			fakeSSE(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "signature_delta", "signature": "sig"}})
			fakeSSE(map[string]any{"type": "content_block_stop", "index": i})
		case "tool":
			id := b.ID
			if id == "" {
				id = fmt.Sprintf("toolu_%d_%d", n, i)
			}
			args := string(b.Args)
			if args == "" {
				args = "{}"
			}
			fakeSSE(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": id, "name": toolPrefix + b.Name, "input": map[string]any{}}})
			half := len(args) / 2
			fakeSSE(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": args[:half]}})
			fakeSSE(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": args[half:]}})
			fakeSSE(map[string]any{"type": "content_block_stop", "index": i})
			content = append(content, map[string]any{"type": "tool_use", "id": id, "name": toolPrefix + b.Name, "input": json.RawMessage(args)})
			p := &pend{name: b.Name, args: json.RawMessage(args), ch: make(chan [2]any, 1)}
			pending = append(pending, p)
			if b.Hasty {
				go func() { text, isErr := fakeCall(mcpURL, p.name, p.args); p.ch <- [2]any{text, isErr} }()
			}
		}
	}
	if m.DelayMS > 0 {
		time.Sleep(time.Duration(m.DelayMS) * time.Millisecond)
	}
	fakeSSE(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": m.Stop}, "usage": map[string]any{"output_tokens": 7}})
	fakeSSE(map[string]any{"type": "message_stop"})
	fakeEmit(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"role": "assistant", "content": content}})
	fakeLog(dir, idx, map[string]any{"k": "gen_stop", "t": time.Now().UnixNano()})

	if m.Stop != "tool_use" {
		fakeEmit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "ok", "total_cost_usd": 0.0})
		if m.ExitAfter {
			os.Exit(0)
		}
		return true
	}
	// 도구 호출: 보류 중인 tools/call 이 ARTEX 의 결과를 받을 때까지 막힌다.
	var hasty []bool
	for _, b := range m.Blocks {
		if b.Type == "tool" {
			hasty = append(hasty, b.Hasty)
		}
	}
	run := func(i int, p *pend) {
		if hasty[i] {
			return // 이미 진행 중
		}
		text, isErr := fakeCall(mcpURL, p.name, p.args)
		p.ch <- [2]any{text, isErr}
	}
	var wg sync.WaitGroup
	for i, p := range pending {
		if m.Parallel {
			wg.Add(1)
			go func() { defer wg.Done(); run(i, p) }()
		} else {
			run(i, p)
		}
	}
	wg.Wait()
	for _, p := range pending {
		r := <-p.ch
		fakeLog(dir, idx, map[string]any{"k": "tool_result", "name": p.name, "args": string(p.args), "text": r[0], "isError": r[1]})
	}
	return false
}

func fakeCall(url, name string, args json.RawMessage) (string, bool) {
	res, err := fakeRPC(url, 9, "tools/call", map[string]any{"name": name, "arguments": json.RawMessage(args)})
	if err != nil {
		return "rpc error: " + err.Error(), true
	}
	var texts []string
	if cs, ok := res["content"].([]any); ok {
		for _, c := range cs {
			if m, ok := c.(map[string]any); ok {
				texts = append(texts, fmt.Sprint(m["text"]))
			}
		}
	}
	isErr, _ := res["isError"].(bool)
	return strings.Join(texts, "\n---\n"), isErr
}

func fakeRPC(url string, id int, method string, params any) (map[string]any, error) {
	body := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if id != 0 {
		body["id"] = id
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if id == 0 {
		return nil, nil
	}
	var out struct {
		Result map[string]any `json:"result"`
		Error  *rpcError      `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s", out.Error.Message)
	}
	return out.Result, nil
}
