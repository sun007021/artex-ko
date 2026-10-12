package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// ---- 세션 ----
//
// session 은 ARTEX 세션 ID 하나에 대응하는 장수명 `claude -p` 프로세스와, 그 위의 턴 상태기계다.
//
//	idle   : 프로세스가 stdin 의 다음 사용자 메시지를 기다린다.
//	busy   : 모델 응답을 만드는 중(Provider.Stream 이 이벤트를 소비 중).
//	parked : 모델이 ARTEX 도구를 불렀다. tools/call 들이 응답을 연 채 보류 중이고, agentcore 가 도구를
//	         실행해 다음 Stream 호출에 결과를 넘겨 주기를 기다린다.
//
// 보류 중인 tools/call 은 (도구 이름, 인자) 로 tool_use 블록과 짝을 짓는다. tool_use 블록은 stream_event 의
// content_block_stop 에서 reader 고루틴이 먼저 등록하므로(MCP 호출은 항상 그 뒤에 온다) 핸들러는 기껏해야
// 짧게 기다리면 된다.

type sessState int

const (
	stIdle sessState = iota
	stBusy
	stParked
)

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

type toolInfo struct {
	artexName string
	tool      mcpTool
}

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

// turnLimits 는 시간 상수다(테스트에서 줄인다).
var (
	resultWaitTimeout = 15 * time.Second
	claimWait         = 3 * time.Second
	killGrace         = 3 * time.Second
)

// proc 은 claude 프로세스 하나(와 그 MCP 엔드포인트)의 수명 동안만 유효한 상태다. 세션이 프로세스를 다시 띄워도
// 이전 프로세스의 늦은 이벤트·호출이 새 상태를 건드리지 못하도록 프로세스별로 분리한다. mcpEndpoint 를 구현한다.
type proc struct {
	s *session

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	dir    string
	token  string
	events chan sEvent
	stderr *tailBuffer

	readDone  chan struct{} // stdout 읽기가 끝남(EOF)
	closed    chan struct{} // 프로세스가 끝났거나 kill 됨
	closeOnce sync.Once
	exited    chan struct{} // cmd.Wait 가 반환함

	// 아래는 s.mu 로 보호한다.
	awaiting  []*await
	rejected  map[string]bool // 이번 응답에서 노출하지 않은(숨겨진) 도구 호출의 name+key
	changed   chan struct{}   // awaiting/rejected 가 바뀌면 닫힌다
	resetHint time.Time       // rate_limit_event 에서 읽은 한도 해제 시각
	broken    bool            // claude 가 호출을 포기했다 → 다음 호출에서 재동기화
}

func (p *proc) markClosed() { p.closeOnce.Do(func() { close(p.closed) }) }

type session struct {
	m   *manager
	key string

	turn sync.Mutex // Stream 호출 직렬화

	mu          sync.Mutex
	p           *proc
	state       sessState
	cfgKey      string
	sysHash     string
	synced      []string            // claude 세션이 흡수한 ARTEX 메시지 서명(리마인더 제외)
	toolsByName map[string]toolInfo // claude 가 부르는 이름(접두사 제외) → 정보
	toolList    []mcpTool
	lastActive  time.Time
}

func newSession(m *manager, key string) *session {
	return &session{m: m, key: key, lastActive: time.Now()}
}

func (s *session) logf(format string, a ...any) {
	if s.m.logf != nil {
		s.m.logf("[claude-code] "+format, a...)
	}
}

// signalLocked 는 awaiting/rejected 변화를 기다리는 핸들러를 깨운다. s.mu 를 쥐고 부른다.
func (p *proc) signalLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// ---- 도구 목록 / 노출 ----

// setTools 는 이번 요청의 도구 스키마를 MCP 이름으로 바꿔 둔다. claude 가 이름을 접두사 없이 보내므로
// ARTEX 이름과 1:1 인 안전한 별칭을 만든다(허용 문자 밖의 문자는 '_' 로, 충돌 시 번호).
func (s *session) setTools(schemas []llm.ToolSchema) {
	by := map[string]toolInfo{}
	list := make([]mcpTool, 0, len(schemas))
	for _, sc := range schemas {
		name := safeToolName(sc.Name, by)
		schema := sc.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if _, ok := schema["type"]; !ok {
			cp := make(map[string]any, len(schema)+1)
			for k, v := range schema {
				cp[k] = v
			}
			cp["type"] = "object"
			schema = cp
		}
		t := mcpTool{
			Name: name, Description: sc.Description, InputSchema: schema,
			// 결과가 길어도 파일로 빼 두지 않고 그대로 모델에 주도록 한도를 올린다(ARTEX 가 이미 자체 상한을 건다).
			Meta: map[string]any{"anthropic/maxResultSizeChars": 500000},
		}
		by[name] = toolInfo{artexName: sc.Name, tool: t}
		list = append(list, t)
	}
	s.mu.Lock()
	s.toolsByName, s.toolList = by, list
	s.mu.Unlock()
}

// safeToolName 은 MCP/API 도구 이름 규칙([A-Za-z0-9_-], 최대 52자: 접두사 12자 + 52 = 64)에 맞춘다.
func safeToolName(name string, taken map[string]toolInfo) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	n := b.String()
	if n == "" {
		n = "tool"
	}
	if len(n) > 52 {
		n = n[:52]
	}
	base := n
	for i := 2; ; i++ {
		if _, dup := taken[n]; !dup {
			return n
		}
		suffix := fmt.Sprintf("_%d", i)
		if len(base)+len(suffix) > 52 {
			n = base[:52-len(suffix)] + suffix
		} else {
			n = base + suffix
		}
	}
}

// listTools 는 mcpEndpoint 구현이다(프로세스가 세션의 최신 도구 목록을 돌려준다).
func (p *proc) listTools() []mcpTool {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	return append([]mcpTool(nil), p.s.toolList...)
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

// ---- MCP tools/call (아무것도 실행하지 않는다) ----

// callTool 은 claude 의 도구 호출을 보류했다가, agentcore 가 같은 호출의 결과를 넘겨 주면 그것을 돌려준다.
func (p *proc) callTool(ctx context.Context, name string, args json.RawMessage) mcpResult {
	s := p.s
	key := canonArgs(args)
	deadline := time.NewTimer(claimWait)
	defer deadline.Stop()

	var aw *await
	for aw == nil {
		s.mu.Lock()
		if _, known := s.toolsByName[name]; !known {
			s.mu.Unlock()
			return mcpResult{Text: []string{"unknown tool: " + name}, IsError: true}
		}
		if p.rejected[name+"\x00"+key] {
			s.mu.Unlock()
			return mcpResult{Text: []string{"This tool is not available right now."}, IsError: true}
		}
		aw = p.claimLocked(name, key, false)
		ch := p.changed
		s.mu.Unlock()
		if aw != nil {
			break
		}
		select {
		case <-ch:
		case <-deadline.C:
			// 인자가 정규화 과정에서 달라졌을 수 있다(claude 가 스키마로 기본값을 채우는 등).
			// 같은 이름의 아직 안 붙은 호출 중 가장 오래된 것에 붙인다.
			s.mu.Lock()
			aw = p.claimLocked(name, key, true)
			s.mu.Unlock()
			if aw == nil {
				return mcpResult{Text: []string{"ARTEX could not match this tool call to a pending request; retry the call."}, IsError: true}
			}
		case <-p.closed:
			return mcpResult{Text: []string{"session closed"}, IsError: true}
		case <-ctx.Done():
			return mcpResult{Text: []string{"cancelled"}, IsError: true}
		}
	}
	select {
	case <-aw.done:
		return *aw.res
	case <-p.closed:
		return mcpResult{Text: []string{"session closed"}, IsError: true}
	case <-ctx.Done():
		// claude 가 호출을 포기했다(타임아웃 등). 결과를 영영 못 돌려주므로 다음 호출에서 재동기화한다.
		s.mu.Lock()
		p.broken = true
		s.mu.Unlock()
		return mcpResult{Text: []string{"cancelled"}, IsError: true}
	}
}

// claimLocked 는 아직 안 붙은 await 를 하나 골라 claimed 로 표시한다(s.mu 필요).
func (p *proc) claimLocked(name, key string, byNameOnly bool) *await {
	for _, a := range p.awaiting {
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

// deliver 는 await 에 결과를 건넨다.
func (s *session) deliver(a *await, r mcpResult) {
	s.mu.Lock()
	if a.res == nil {
		a.res = &r
		close(a.done)
	}
	s.mu.Unlock()
}

// ---- 프로세스 ----

// tailBuffer 는 stderr 의 마지막 N 바이트만 보관한다.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// start 는 claude 프로세스를 띄워 세션에 붙인다. withMCP 가 false 면 도구 없는 단발 호출용이다.
func (s *session) start(opts Options, sys string, withMCP bool) (*proc, error) {
	bin, err := ResolveBinary(opts.Binary)
	if err != nil {
		return nil, err
	}
	base := opts.WorkDir
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, errf(KindProtocol, "작업 디렉터리를 만들 수 없습니다: %v", err)
	}
	dir, err := os.MkdirTemp(base, "s-")
	if err != nil {
		return nil, errf(KindProtocol, "작업 디렉터리를 만들 수 없습니다: %v", err)
	}
	p := &proc{
		s: s, dir: dir, events: make(chan sEvent, 512),
		closed: make(chan struct{}), exited: make(chan struct{}),
		changed: make(chan struct{}), stderr: &tailBuffer{max: 8 << 10},
	}
	spec := SpawnSpec{Model: opts.Model, Effort: opts.Effort}
	spec.SystemPromptFile = filepath.Join(dir, "system-prompt.txt")
	if err := os.WriteFile(spec.SystemPromptFile, []byte(sys), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, errf(KindProtocol, "시스템 프롬프트 파일을 쓸 수 없습니다: %v", err)
	}
	if withMCP {
		hub, err := s.m.getHub()
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, errf(KindProtocol, "%v", err)
		}
		p.token = hub.register(p)
		cfg := map[string]any{"mcpServers": map[string]any{mcpServerName: map[string]any{
			"type": "http", "url": hub.url(p.token),
			// 도구 한 번이 오래 걸려도(ARTEX 가 수 시간 걸릴 수 있다) 연결을 끊지 않도록 하드 타임아웃을 크게 둔다.
			"timeout": 12 * 3600 * 1000,
		}}}
		raw, _ := json.Marshal(cfg)
		spec.MCPConfigFile = filepath.Join(dir, "mcp.json")
		if err := os.WriteFile(spec.MCPConfigFile, raw, 0o600); err != nil {
			hub.unregister(p.token)
			_ = os.RemoveAll(dir)
			return nil, errf(KindProtocol, "MCP 설정 파일을 쓸 수 없습니다: %v", err)
		}
	}
	fail := func(e *Error) (*proc, error) {
		if p.token != "" {
			if hub := s.m.hubIfAny(); hub != nil {
				hub.unregister(p.token)
			}
		}
		_ = os.RemoveAll(dir)
		return nil, e
	}

	parent := os.Environ()
	extra := append(loopbackNoProxy(parent), proxyEnv(opts.Proxy)...)
	cmd := exec.Command(bin, buildArgs(spec)...)
	cmd.Dir = dir // ARTEX 전용 빈 디렉터리: 프로젝트/로컬 설정이 비어 있다
	cmd.Env = SanitizedEnv(parent, extra...)
	setProcAttrs(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail(errf(KindProtocol, "stdin 파이프 실패: %v", err))
	}
	// stdout 은 StdoutPipe 대신 직접 만든 파이프를 쓴다: StdoutPipe 는 Wait 가 읽기 끝을 닫아 버려서, 프로세스가
	// 마지막 줄을 쓰자마자 종료하면 아직 읽지 못한 출력이 사라질 수 있다(Wait 는 읽기가 끝난 뒤에 불러야 한다).
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return fail(errf(KindProtocol, "stdout 파이프 실패: %v", err))
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stdoutW.Close()
		return fail(errf(KindNotFound, "claude 를 실행할 수 없습니다: %v", err))
	}
	_ = stdoutW.Close() // 쓰기 끝은 자식만 갖는다 → 자식이 끝나면 읽기 쪽이 EOF 를 본다
	p.cmd, p.stdin = cmd, stdin
	p.readDone = make(chan struct{})

	s.mu.Lock()
	s.p = p
	s.state = stIdle
	s.lastActive = time.Now()
	s.mu.Unlock()

	go p.readLoop(stdout)
	go func() {
		werr := cmd.Wait()
		close(p.exited)
		// 남은 출력을 다 읽은 뒤에 종료를 알린다(순서 보존). 자식의 자식이 파이프를 쥐고 있어도 무한정 기다리지 않는다.
		select {
		case <-p.readDone:
		case <-time.After(2 * time.Second):
		}
		_ = stdout.Close()
		p.markClosed()
		select {
		case p.events <- sEvent{kind: evExit, err: p.exitError(werr)}:
		default:
		}
	}()
	s.logf("프로세스 시작 pid=%d session=%s", cmd.Process.Pid, s.key)
	return p, nil
}

func (p *proc) exitError(waitErr error) *Error {
	tail := p.stderr.String()
	if len(tail) > 1500 {
		tail = tail[len(tail)-1500:]
	}
	msg := "claude 프로세스가 비정상 종료했습니다"
	if waitErr != nil {
		msg += ": " + waitErr.Error()
	}
	if tail != "" {
		msg += " | stderr: " + tail
	}
	return &Error{Kind: KindCrash, Message: msg}
}

// kill 은 현재 프로세스를 정리하고 보류 중인 호출을 모두 풀어 준다. 여러 번 불러도 안전하다.
func (s *session) kill() {
	s.mu.Lock()
	p := s.p
	s.p = nil
	s.synced, s.state = nil, stIdle
	s.mu.Unlock()
	if p == nil {
		return
	}
	p.markClosed()
	if p.token != "" {
		if hub := s.m.hubIfAny(); hub != nil {
			hub.unregister(p.token)
		}
	}
	_ = p.stdin.Close()
	terminate(p.cmd)
	go func() {
		select {
		case <-p.exited:
		case <-time.After(killGrace):
			killNow(p.cmd)
			<-p.exited
		}
		_ = os.RemoveAll(p.dir)
	}()
	s.logf("프로세스 종료 session=%s", s.key)
}

// send 는 stdin 에 stream-json 사용자 메시지 한 줄을 쓴다.
func (p *proc) send(line []byte) error {
	if _, err := p.stdin.Write(line); err != nil {
		return errf(KindCrash, "claude 에 입력을 쓰지 못했습니다: %v", err)
	}
	return nil
}

// ---- stdout 읽기 ----

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

func (p *proc) push(ev sEvent) {
	select {
	case p.events <- ev:
	case <-p.closed:
	}
}

func (p *proc) readLoop(r io.Reader) {
	defer close(p.readDone)
	br := bufio.NewReaderSize(r, 1<<20)
	rs := &readState{blocks: map[int]*blockState{}}
	for {
		line, err := br.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 {
			p.handleLine(t, rs)
		}
		if err != nil {
			return
		}
	}
}

func (p *proc) handleLine(line []byte, rs *readState) {
	s := p.s
	var l rawLine
	if json.Unmarshal(line, &l) != nil {
		return // JSON 이 아닌 줄(경고 등)은 무시
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
					p.push(sEvent{kind: evFail, err: &Error{Kind: KindProtocol, Message: msg}})
				}
			}
		case "api_retry":
			s.logf("API 재시도 attempt=%d/%d status=%v kind=%s delay=%dms", l.Attempt, l.MaxRetries, l.ErrorStatus, l.errCategory(), l.RetryDelayMS)
		}
	case "stream_event":
		p.handleSSE(l.Event, rs)
	case "assistant":
		// max_output_tokens 는 장애가 아니라 출력 상한 도달이다 — stop_reason(max_tokens)으로 ARTEX 가 직접
		// 처리하므로 오류로 올리지 않는다.
		if cat := l.errCategory(); cat != "" && cat != "max_output_tokens" {
			s.mu.Lock()
			reset := p.resetHint
			s.mu.Unlock()
			p.push(sEvent{kind: evFail, err: classify(cat, l.assistantText(), reset)})
		}
	case "rate_limit_event":
		if ri, ok := l.rateLimit(); ok && ri.Status == "rejected" {
			s.mu.Lock()
			p.resetHint = ri.resetTime()
			s.mu.Unlock()
		}
	case "result":
		var e *Error
		if l.IsError {
			text := l.Result
			if text == "" && len(l.Errors) > 0 {
				text = strings.Join(l.Errors, "; ")
			}
			s.mu.Lock()
			reset := p.resetHint
			s.mu.Unlock()
			e = classify("", text, reset)
			if e.Kind == KindUnknown {
				e.Message = orDefault(l.Subtype, "error") + ": " + e.Message
			}
		}
		p.push(sEvent{kind: evResult, isErr: l.IsError, err: e})
	}
}

func (p *proc) handleSSE(raw json.RawMessage, rs *readState) {
	s := p.s
	var e sseEvent
	if len(raw) == 0 || json.Unmarshal(raw, &e) != nil {
		return
	}
	emit := func(ev llm.StreamEvent) { p.push(sEvent{kind: evEmit, ev: ev}) }
	switch e.Type {
	case "message_start":
		// 새 메시지가 시작됐다는 건 직전 메시지의 모든 도구 호출이 끝났다는 뜻이다.
		s.mu.Lock()
		p.awaiting, p.rejected = nil, nil
		p.signalLocked()
		s.mu.Unlock()
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
				s.mu.Lock()
				ti, known := s.toolsByName[mcp]
				s.mu.Unlock()
				if known {
					b.exposed, b.name = true, mcp
					rs.exposed = append(rs.exposed, b.id)
					emit(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.id, ToolName: ti.artexName})
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
		s.mu.Lock()
		if b.exposed {
			p.awaiting = append(p.awaiting, &await{id: b.id, mcpName: b.name, key: canonArgs(in), input: in, done: make(chan struct{})})
		} else if mcp, ok := strings.CutPrefix(b.name, toolPrefix); ok {
			// 이번 요청에선 숨겨진 ARTEX 도구(수습 단계의 Bash 금지 등): 실행하지 않고 즉시 거절한다.
			if p.rejected == nil {
				p.rejected = map[string]bool{}
			}
			p.rejected[mcp+"\x00"+canonArgs(in)] = true
		}
		p.signalLocked()
		s.mu.Unlock()
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
		p.push(sEvent{kind: evEnd, stop: rs.stop, toolIDs: ids})
	case "error":
		msg := "stream error"
		cat := ""
		if e.Error != nil {
			msg, cat = e.Error.Message, e.Error.Type
		}
		s.mu.Lock()
		reset := p.resetHint
		s.mu.Unlock()
		switch cat {
		case "overloaded_error":
			cat = "overloaded"
		case "rate_limit_error":
			cat = "rate_limit"
		case "authentication_error", "permission_error":
			cat = "authentication_failed"
		}
		p.push(sEvent{kind: evFail, err: classify(cat, msg, reset)})
	}
}

var errConsumerStopped = errors.New("claudecode: consumer stopped")
