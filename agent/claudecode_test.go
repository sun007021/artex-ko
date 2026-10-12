package agent

import (
	"testing"

	"github.com/Autumn-27/artex/claudecode"
)

func TestClaudeCodeConfigBuildsBridgeProvider(t *testing.T) {
	c := NewClaudeCodeConfig(" sonnet ", "high", "", "/opt/claude", "/tmp/w")
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
	if c != NewClaudeCodeConfig("sonnet", "high", "", "/opt/claude", "/tmp/w") {
		t.Fatal("동일 입력의 Config 가 같지 않다")
	}
}

func TestClaudeCodeEmptyModelStaysEmpty(t *testing.T) {
	// 비어 있으면 claude 의 기본 모델을 쓴다 — API 경로의 기본 모델이 --model 로 새면 안 된다.
	if c := NewClaudeCodeConfig("", "", "", "", ""); c.Model != "" || c.ReasoningEffort != "" {
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
