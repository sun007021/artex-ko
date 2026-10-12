package claudecode

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSanitizedEnvStripsAuthAndRouting(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin", "HOME=/home/u", "LANG=C",
		"ANTHROPIC_API_KEY=k1", "anthropic_auth_token=k2", "CLAUDE_CODE_OAUTH_TOKEN=k3",
		"ANTHROPIC_BASE_URL=http://x", "ANTHROPIC_CUSTOM_HEADERS=a:b", "ANTHROPIC_MODEL=m", "CLAUDE_CODE_SIMPLE=1",
		"CLAUDE_CODE_USE_VERTEX=1", "ANTHROPIC_VERTEX_PROJECT_ID=p", "ANTHROPIC_BEDROCK_BASE_URL=b",
		"CLAUDE_CODE_EFFORT_LEVEL=max", "MAX_THINKING_TOKENS=1", "CLAUDE_CODE_DISABLE_CLAUDE_MDS=0", "BADLINE",
	}
	env := SanitizedEnv(parent, "HTTPS_PROXY=http://p")
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := m[k]; dup {
			t.Errorf("중복 키 %s", k)
		}
		m[k] = v
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "anthropic_auth_token", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
		"ANTHROPIC_MODEL", "CLAUDE_CODE_SIMPLE", "CLAUDE_CODE_USE_VERTEX", "ANTHROPIC_VERTEX_PROJECT_ID", "ANTHROPIC_BEDROCK_BASE_URL",
		"CLAUDE_CODE_EFFORT_LEVEL", "MAX_THINKING_TOKENS", "BADLINE"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s 가 남아 있다", k)
		}
	}
	if m["PATH"] != "/usr/bin" || m["HOME"] != "/home/u" || m["LANG"] != "C" {
		t.Errorf("일반 변수 유실: %v", m)
	}
	if m["CLAUDE_CODE_DISABLE_CLAUDE_MDS"] != "1" {
		t.Errorf("강화 변수가 사용자 값을 덮어써야 한다: %q", m["CLAUDE_CODE_DISABLE_CLAUDE_MDS"])
	}
	if m["HTTPS_PROXY"] != "http://p" {
		t.Errorf("extra 미반영")
	}
}

func TestLoopbackNoProxy(t *testing.T) {
	got := strings.Join(loopbackNoProxy([]string{"no_proxy=corp.example"}), " ")
	if !strings.Contains(got, "corp.example,127.0.0.1,localhost,::1") {
		t.Fatal(got)
	}
	got = strings.Join(loopbackNoProxy(nil), " ")
	if !strings.Contains(got, "NO_PROXY=127.0.0.1,localhost,::1") {
		t.Fatal(got)
	}
}

func TestBuildArgsMinimal(t *testing.T) {
	a := buildArgs(SpawnSpec{})
	j := strings.Join(a, " ")
	if strings.Contains(j, "--model") || strings.Contains(j, "--effort") || strings.Contains(j, "--mcp-config") || strings.Contains(j, "--system-prompt-file") {
		t.Fatalf("빈 값은 플래그를 만들지 않는다: %s", j)
	}
	if !strings.Contains(j, "--strict-mcp-config") {
		t.Fatal("MCP 가 없어도 사용자 MCP 설정은 무시해야 한다")
	}
	a = buildArgs(SpawnSpec{Model: " m ", Effort: "low", MCPConfigFile: "/x/mcp.json", SystemPromptFile: "/x/sys"})
	j = strings.Join(a, " ")
	for _, w := range []string{"--model m", "--effort low", "--mcp-config /x/mcp.json", "--system-prompt-file /x/sys"} {
		if !strings.Contains(j, w) {
			t.Errorf("%q 없음: %s", w, j)
		}
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]Version{
		"2.1.290 (Claude Code)": {2, 1, 290},
		"v2.1.259":              {2, 1, 259},
		"claude 3.0.1-beta.2":   {3, 0, 1},
	} {
		v, ok := ParseVersion(in)
		if !ok || v != want {
			t.Errorf("%q → %v %v", in, v, ok)
		}
	}
	if _, ok := ParseVersion("no version"); ok {
		t.Error("버전 없음인데 ok")
	}
	if !(Version{2, 1, 258}).Less(MinVersion) || (Version{2, 1, 259}).Less(MinVersion) || (Version{3, 0, 0}).Less(MinVersion) {
		t.Error("Less 비교 오류")
	}
}

func TestLimiterResizeWakesWaiters(t *testing.T) {
	l := newLimiter(1)
	rel, err := l.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() {
		r, err := l.acquire(context.Background())
		if err == nil {
			r()
			close(got)
		}
	}()
	select {
	case <-got:
		t.Fatal("cap=1 인데 두 번째 슬롯을 얻었다")
	case <-time.After(100 * time.Millisecond):
	}
	l.setMax(2)
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("상한을 올렸는데 대기자가 깨어나지 않았다")
	}
	rel()
	if l.maxSlots() != 2 {
		t.Fatal()
	}
	l.setMax(99)
	if l.maxSlots() != HardMaxConcurrent {
		t.Fatalf("하드 상한 초과: %d", l.maxSlots())
	}
	l.setMax(0)
	if l.maxSlots() != DefaultMaxConcurrent {
		t.Fatalf("0 은 기본값: %d", l.maxSlots())
	}
}

func TestLimiterAcquireCancel(t *testing.T) {
	l := newLimiter(1)
	rel, _ := l.acquire(context.Background())
	defer rel()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(ctx); err == nil {
		t.Fatal("취소되어야 한다")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		cat, text string
		reset     time.Time
		kind      ErrKind
	}{
		{"rate_limit", "You've hit your usage limit", time.Time{}, KindUsageLimit},
		{"rate_limit", "429 slow down", time.Time{}, KindRateLimit},
		{"rate_limit", "slow down", time.Unix(1, 0), KindUsageLimit},
		{"overloaded", "x", time.Time{}, KindRateLimit},
		{"authentication_failed", "x", time.Time{}, KindAuth},
		{"billing_error", "x", time.Time{}, KindUsageLimit},
		{"", "Claude usage limit reached|1700000000", time.Time{}, KindUsageLimit},
		{"", "something odd", time.Time{}, KindUnknown},
	}
	for _, c := range cases {
		if e := classify(c.cat, c.text, c.reset); e.Kind != c.kind {
			t.Errorf("%s/%q → %v want %v", c.cat, c.text, e.Kind, c.kind)
		}
	}
	e := &Error{Kind: KindUsageLimit, Message: "m"}
	if !strings.HasPrefix(e.Error(), UsageLimitMarker) {
		t.Error("마커 접두")
	}
}

func TestUsageLimitResetTimeHonored(t *testing.T) {
	g := newGate()
	now := time.Unix(1_700_000_000, 0)
	g.nowFn = func() time.Time { return now }
	g.record(&Error{Kind: KindUsageLimit, Message: "x", ResetAt: now.Add(2 * time.Hour)})
	if err := g.check(context.Background()); err == nil {
		t.Fatal("한도 구간인데 통과")
	}
	now = now.Add(2*time.Hour + time.Second)
	if err := g.check(context.Background()); err != nil {
		t.Fatalf("해제 시각이 지났는데 막힘: %v", err)
	}
	// 상한: 먼 미래 시각도 12시간을 넘지 않는다.
	g2 := newGate()
	g2.nowFn = func() time.Time { return now }
	g2.record(&Error{Kind: KindUsageLimit, Message: "x", ResetAt: now.Add(100 * time.Hour)})
	now = now.Add(usageLimitCap + time.Minute)
	if err := g2.check(context.Background()); err != nil {
		t.Fatalf("상한(%v) 뒤에도 막힘: %v", usageLimitCap, err)
	}
	// 시각을 모르면 기본값.
	g3 := newGate()
	g3.nowFn = func() time.Time { return now }
	g3.record(&Error{Kind: KindUsageLimit, Message: "x"})
	now = now.Add(usageLimitFallback - time.Second)
	if g3.check(context.Background()) == nil {
		t.Fatal("기본 구간 안에서는 막혀야 한다")
	}
	now = now.Add(2 * time.Second)
	if g3.check(context.Background()) != nil {
		t.Fatal("기본 구간 뒤에는 풀려야 한다")
	}
}

func TestRateBackoffDoublesAndCaps(t *testing.T) {
	g := newGate()
	now := time.Unix(1_700_000_000, 0)
	g.nowFn = func() time.Time { return now }
	var last time.Duration
	g.sleepFn = func(_ context.Context, d time.Duration) error { last = d; return nil }
	want := []time.Duration{rateBackoffMin, 2 * rateBackoffMin, 4 * rateBackoffMin, 8 * rateBackoffMin, rateBackoffMax, rateBackoffMax}
	for i, w := range want {
		g.record(&Error{Kind: KindRateLimit, Message: "x"})
		_ = g.check(context.Background())
		if last != w {
			t.Fatalf("step %d: %v want %v", i, last, w)
		}
	}
	g.success()
	g.record(&Error{Kind: KindRateLimit, Message: "x"})
	_ = g.check(context.Background())
	if last != rateBackoffMin {
		t.Fatalf("성공 뒤 초기화 안 됨: %v", last)
	}
}

func TestErrorStringMarkers(t *testing.T) {
	for kind, want := range map[ErrKind]string{
		KindUsageLimit: UsageLimitMarker, KindAuth: "status 401", KindNotFound: SetupErrorMarker,
		KindTooOld: SetupErrorMarker, KindRefused: SetupErrorMarker,
	} {
		if s := (&Error{Kind: kind, Message: "m"}).Error(); !strings.Contains(s, want) {
			t.Errorf("kind %d: %q 에 %q 없음", kind, s, want)
		}
	}
	for _, kind := range []ErrKind{KindRateLimit, KindCrash, KindProtocol, KindUnknown} {
		s := (&Error{Kind: kind, Message: "m"}).Error()
		if strings.Contains(s, SetupErrorMarker) || strings.Contains(s, UsageLimitMarker) || strings.Contains(s, "status 401") {
			t.Errorf("재시도 가능한 오류에 고정 표지가 붙음: %q", s)
		}
	}
}
