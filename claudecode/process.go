package claudecode

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- 프로세스 수명 ----
//
// proc 은 claude 프로세스 하나(와 그 MCP 엔드포인트)의 수명 동안만 유효한 상태다. 세션이 프로세스를 다시
// 띄워도 이전 프로세스의 늦은 이벤트·호출이 새 상태를 건드리지 못하도록 프로세스별로 분리한다.
// 구성: 프로세스 자체(spawn/kill/종료 감시) + 호출 짝맞춤(callPairer) + 출력 번역(streamTranslator).
// mcpEndpoint 를 구현한다.

// turnLimits 는 시간 상수다(테스트에서 줄인다).
var (
	resultWaitTimeout = 15 * time.Second
	claimWait         = 3 * time.Second
	killGrace         = 3 * time.Second
	// exitDrainWait 는 프로세스가 끝난 뒤 남은 출력을 읽는 데 기다리는 최대 시간이다(자식의 자식이
	// 파이프를 쥐고 있어도 무한정 기다리지 않는다).
	exitDrainWait = 2 * time.Second
)

type proc struct {
	m   *Manager
	key string // 세션 키(로그용)

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	dir    string
	token  string
	events chan sEvent
	stderr *tailBuffer

	pair  *callPairer
	tr    *streamTranslator
	tools *toolBox // 세션이 지금 노출하는 도구(MCP tools/list·검증용)
	// shown 은 이 프로세스가 시작될 때 claude 에 보여 준 도구 집합이다. claude 는 tools/list 를 한 번만 읽는다.
	shown toolSet

	readDone  chan struct{} // stdout 읽기가 끝남(EOF)
	closed    chan struct{} // 프로세스가 끝났거나 kill 됨
	closeOnce sync.Once
	exited    chan struct{} // cmd.Wait 가 반환함
	exitErr   atomic.Pointer[Error]

	broken atomic.Bool // claude 가 호출을 포기했다 → 다음 호출에서 재동기화
	act    activityClock
}

func (p *proc) logf(format string, a ...any) { p.m.logf("[claude-code] "+format, a...) }

func (p *proc) markClosed() { p.closeOnce.Do(func() { close(p.closed) }) }

func (p *proc) isClosed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

// push 는 번역기의 이벤트를 소비자에게 넘긴다. 프로세스가 이미 끝났으면 버린다.
func (p *proc) push(ev sEvent) {
	select {
	case p.events <- ev:
	case <-p.closed:
	}
}

// send 는 stdin 에 stream-json 사용자 메시지 한 줄을 쓴다.
func (p *proc) send(line []byte) error {
	if _, err := p.stdin.Write(line); err != nil {
		return errf(KindCrash, "claude 에 입력을 쓰지 못했습니다: %v", err)
	}
	return nil
}

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

// spawn 은 claude 프로세스를 띄운다. withMCP 가 false 면 도구 없는 단발 호출용이다. 세션에 붙이는 일은
// 호출자(session.start)가 한다.
func (m *Manager) spawn(key string, opts Options, sys string, tools *toolBox, withMCP bool) (*proc, error) {
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
		m: m, key: key, dir: dir, events: make(chan sEvent, 512), tools: tools, shown: tools.fingerprint(),
		closed: make(chan struct{}), exited: make(chan struct{}), readDone: make(chan struct{}),
		stderr: &tailBuffer{max: 8 << 10},
	}
	p.pair = newCallPairer(p.logf)
	p.tr = newStreamTranslator(p.pair, translatorHooks{
		lookup: func(name string) (string, bool) {
			ti, ok := tools.lookup(name)
			return ti.artexName, ok
		},
		sink: p.push, onLine: p.act.touch, onRetry: p.act.retry, logf: p.logf,
	})
	p.act.touch()

	spec := SpawnSpec{Model: opts.Model, Effort: opts.Effort}
	spec.SystemPromptFile = filepath.Join(dir, "system-prompt.txt")
	if err := os.WriteFile(spec.SystemPromptFile, []byte(sys), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, errf(KindProtocol, "시스템 프롬프트 파일을 쓸 수 없습니다: %v", err)
	}
	if withMCP {
		hub, err := m.getHub()
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
			if hub := m.hubIfAny(); hub != nil {
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
	// 자식의 자식이 stderr 파이프를 쥐고 있어도 Wait 가 영원히 막히지 않게 한다(특히 Windows).
	cmd.WaitDelay = killGrace
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

	go func() {
		defer close(p.readDone)
		p.tr.run(stdout)
	}()
	go func() {
		werr := cmd.Wait()
		close(p.exited)
		// 남은 출력을 다 읽은 뒤에 종료를 알린다(순서 보존). 자식의 자식이 파이프를 쥐고 있어도 무한정 기다리지 않는다.
		select {
		case <-p.readDone:
		case <-time.After(exitDrainWait):
		}
		_ = stdout.Close()
		ee := p.exitError(werr)
		p.exitErr.Store(ee)
		p.markClosed()
		select {
		case p.events <- sEvent{kind: evExit, err: ee}:
		default: // 가득 찼다 — 소비자는 closed 로 종료를 알아챈다(consume)
		}
	}()
	p.logf("프로세스 시작 pid=%d session=%s", cmd.Process.Pid, key)
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

// terminated 는 프로세스가 끝난 이유를 오류로 돌려준다. 종료 감시가 아직 못 썼으면(kill 로 닫힌 경우 등)
// 일반 문구를 쓴다.
func (p *proc) terminated() *Error {
	if e := p.exitErr.Load(); e != nil {
		return e
	}
	return errf(KindCrash, "claude 프로세스가 종료되었습니다")
}

// shutdown 은 프로세스를 정리하고 보류 중인 호출을 모두 풀어 준다. 여러 번 불러도 안전하다.
func (p *proc) shutdown() {
	p.markClosed()
	if p.token != "" {
		if hub := p.m.hubIfAny(); hub != nil {
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
	p.logf("프로세스 종료 session=%s", p.key)
}

// ---- MCP 엔드포인트 ----

// listTools 는 mcpEndpoint 구현이다(프로세스가 세션의 최신 도구 목록을 돌려준다).
func (p *proc) listTools() []mcpTool { return p.tools.snapshot() }

// callTool 은 claude 의 도구 호출을 보류했다가, agentcore 가 같은 호출의 결과를 넘겨 주면 그것을 돌려준다.
// 아무것도 실행하지 않는다.
func (p *proc) callTool(ctx context.Context, name string, args json.RawMessage, toolUseID string) mcpResult {
	known := func(n string) bool { _, ok := p.tools.lookup(n); return ok }
	out := p.pair.match(ctx, p.closed, known, name, args, toolUseID)
	if out.res != nil {
		return *out.res
	}
	res, abandoned := p.pair.result(ctx, p.closed, out.aw)
	if abandoned {
		// claude 가 호출을 포기했다(타임아웃 등). 결과를 영영 못 돌려주므로 다음 호출에서 재동기화한다.
		p.broken.Store(true)
	}
	return res
}

// ---- 무활동 감시용 시계 ----

// maxRetryResets 는 api_retry 줄이 무활동 시계를 되돌릴 수 있는 턴당 최대 횟수다. claude 가 API 를 재시도하는
// 동안은 stdout 이 조용해도 정상이지만, 끝없는 재시도가 슬롯을 영원히 쥐지는 못하게 한다.
const maxRetryResets = 10

type activityClock struct {
	mu      sync.Mutex
	last    time.Time
	retries int
}

// touch 는 stdout 에서 줄을 읽었음을 기록한다(api_retry 는 retry 로 따로 센다).
func (a *activityClock) touch() {
	a.mu.Lock()
	a.last = time.Now()
	a.mu.Unlock()
}

// retry 는 api_retry 줄을 읽었음을 기록한다. 턴당 maxRetryResets 번까지만 시계를 되돌린다.
func (a *activityClock) retry() {
	a.mu.Lock()
	if a.retries < maxRetryResets {
		a.retries++
		a.last = time.Now()
	}
	a.mu.Unlock()
}

// beginTurn 은 새 턴이 시작됐음을 기록한다.
func (a *activityClock) beginTurn() {
	a.mu.Lock()
	a.last, a.retries = time.Now(), 0
	a.mu.Unlock()
}

func (a *activityClock) idle() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Since(a.last)
}
