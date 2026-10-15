package agent

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Autumn-27/artex/claudecode"
)

func TestClaudeCodeConfigBuildsBridgeProvider(t *testing.T) {
	mgr := claudecode.NewManager()
	t.Cleanup(mgr.Shutdown)
	c := NewClaudeCodeConfig(" sonnet ", "high", "", "/opt/claude", "/tmp/w", mgr)
	if !c.UsesClaudeCode() || c.Model != "sonnet" || c.ReasoningEffort != "high" || !c.Stream {
		t.Fatalf("%+v", c)
	}
	p, err := c.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*claudecode.Provider); !ok {
		t.Fatalf("provider=%T", p)
	}
	// Config 는 == 로 비교된다.
	if c != NewClaudeCodeConfig("sonnet", "high", "", "/opt/claude", "/tmp/w", mgr) {
		t.Fatal("동일 입력의 Config 가 같지 않다")
	}
}

func TestClaudeCodeEmptyModelStaysEmpty(t *testing.T) {
	// 비어 있으면 claude 의 기본 모델을 쓴다 — API 경로의 기본 모델이 --model 로 새면 안 된다.
	if c := NewClaudeCodeConfig("", "", "", "", "", nil); c.Model != "" || c.ReasoningEffort != "" {
		t.Fatalf("%+v", c)
	}
}

func TestQuotaMarkerForClaudeCode(t *testing.T) {
	for _, msg := range []string{
		claudecode.UsageLimitMarker + ": You've hit your usage limit",
		(&claudecode.Error{Kind: claudecode.KindUsageLimit, Message: "limit"}).Error(),
	} {
		if !IsQuotaExhaustedMessage(msg) {
			t.Errorf("장애 전환 대상이어야 한다: %q", msg)
		}
	}
	// 인증 오류와 일시적 과부하는 전환 대상이 아니다.
	for _, e := range []*claudecode.Error{
		{Kind: claudecode.KindAuth, Message: "login needed"},
		{Kind: claudecode.KindRateLimit, Message: "overloaded"},
		{Kind: claudecode.KindCrash, Message: "boom"},
	} {
		if IsQuotaExhaustedMessage(e.Error()) {
			t.Errorf("전환 대상이면 안 된다: %q", e.Error())
		}
	}
}

func TestIsQuotaExhaustedErrorTyped(t *testing.T) {
	typed := &claudecode.Error{Kind: claudecode.KindUsageLimit, Message: "limit"}
	if !IsQuotaExhaustedError(typed) || !IsQuotaExhaustedError(fmt.Errorf("wrap: %w", typed)) {
		t.Fatal("claude-code 사용량 한도는 타입으로 판정한다")
	}
	if IsQuotaExhaustedError(&claudecode.Error{Kind: claudecode.KindRateLimit, Message: "overloaded"}) {
		t.Fatal("일시적 과부하는 한도 소진이 아니다")
	}
	if IsQuotaExhaustedError(nil) {
		t.Fatal("nil")
	}
	// API 키 경로의 문자열 판정은 그대로다.
	if !IsQuotaExhaustedError(errors.New("insufficient_quota: you exceeded your current quota")) {
		t.Fatal("API 키 경로의 한도 소진 문구")
	}
}
