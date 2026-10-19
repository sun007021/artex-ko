package db

import (
	"errors"
	"testing"
)

func TestNormalizeSubscription(t *testing.T) {
	api := LLMProfile{AuthMode: "", Format: "openai", BaseURL: "http://x", APIKey: "k", MaxTokens: 10, Model: " m "}
	cp := api
	if err := cp.NormalizeSubscription(); err != nil || cp != api {
		t.Fatalf("API Key 프로필은 건드리지 않는다: %v %+v", err, cp)
	}

	g := LLMProfile{AuthMode: AuthModeChatGPT, Format: "openai", BaseURL: "http://evil", APIKey: "sk", APIKeyHint: "…abcd", MaxTokens: 5,
		MaxTokensField: "max_completion_tokens", SessionHeaderKey: "X", Model: "  some-model "}
	if err := g.NormalizeSubscription(); err != nil {
		t.Fatal(err)
	}
	if g.Format != "openai-responses" || g.BaseURL != "" || g.APIKey != "" || g.APIKeyHint != "" || g.MaxTokens != 0 ||
		g.MaxTokensField != "" || g.SessionHeaderKey != "" || !g.Streaming || g.Model != "some-model" {
		t.Fatalf("%+v", g)
	}
	// Daybreak 는 ChatGPT 구독에서만 유지되고, 다른 인증 방식에서는 꺼진다.
	dg := LLMProfile{AuthMode: AuthModeChatGPT, Model: "m", Daybreak: true}
	if err := dg.NormalizeSubscription(); err != nil || !dg.Daybreak {
		t.Fatalf("chatgpt 는 Daybreak 를 유지: %v %+v", err, dg)
	}
	for _, mode := range []string{"", AuthModeClaudeCode} {
		o := LLMProfile{AuthMode: mode, Model: "m", Daybreak: true}
		_ = o.NormalizeSubscription()
		if o.Daybreak {
			t.Fatalf("auth_mode=%q 에서는 Daybreak 가 꺼져야 한다", mode)
		}
	}
	empty := LLMProfile{AuthMode: AuthModeChatGPT, Model: "  "}
	if err := empty.NormalizeSubscription(); !errors.Is(err, ErrChatGPTModelRequired) {
		t.Fatalf("빈 모델: %v", err)
	}

	c := LLMProfile{AuthMode: AuthModeClaudeCode, Format: "openai", BaseURL: "http://x", APIKey: "k", ReasoningEffort: " HIGH ", RatePerSecond: 3, RatePerMinute: 4, ThinkingType: "enabled", Model: " sonnet "}
	if err := c.NormalizeSubscription(); err != nil {
		t.Fatal(err)
	}
	if c.Format != "anthropic" || c.BaseURL != "" || c.APIKey != "" || c.ReasoningEffort != "high" || c.Model != "sonnet" ||
		c.RatePerSecond != 0 || c.RatePerMinute != 0 || c.ThinkingType != "" || !c.Streaming {
		t.Fatalf("%+v", c)
	}
	bad := LLMProfile{AuthMode: AuthModeClaudeCode, ReasoningEffort: "ultra"}
	if err := bad.NormalizeSubscription(); !errors.Is(err, ErrBadClaudeEffort) {
		t.Fatalf("%v", err)
	}
	if err := (&LLMProfile{AuthMode: AuthModeClaudeCode}).NormalizeSubscription(); err != nil {
		t.Fatalf("모델·강도 비움은 허용: %v", err)
	}
}
