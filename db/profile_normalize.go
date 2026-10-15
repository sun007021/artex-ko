package db

import (
	"errors"
	"strings"

	"github.com/Autumn-27/artex/llmauth"
)

// 구독 방식(ChatGPT, claude-code) 프로필의 정규화는 이 한 곳에서 한다. SaveProfile 이 저장 직전에, 서버 핸들러가
// 요청을 검증할 때 같은 함수를 부른다.

var (
	// ErrChatGPTModelRequired: 구독 프로필에는 계정의 모델 목록에서 고른 모델이 있어야 한다(모델 식별자를 코드에 두지 않는다).
	ErrChatGPTModelRequired = errors.New("chatgpt profile needs a model")
	// ErrBadClaudeEffort: claude-code 추론 강도가 허용 값이 아니다.
	ErrBadClaudeEffort = errors.New("invalid claude-code reasoning effort")
)

// NormalizeSubscription 은 구독 방식 프로필을 정리하고 검증한다. API Key 방식 프로필은 건드리지 않는다.
//
//   - 모델과 추론 강도를 다듬는다(앞뒤 공백, 소문자).
//   - API 경로 전용 필드(키, base_url, 출력 상한, 세션 헤더)는 이 경로에서 쓰지 않으므로 비우고 스트리밍을 켠다.
//     형식은 chatgpt=openai-responses, claude-code=anthropic 으로 고정한다. claude-code 는 속도 제한과
//     thinking 방식도 비운다(CLI 가 정한다).
//   - 검증 실패는 ErrChatGPTModelRequired / ErrBadClaudeEffort 로 돌려준다(필드는 이미 정리된 상태).
func (p *LLMProfile) NormalizeSubscription() error {
	switch p.AuthMode {
	case AuthModeChatGPT:
		p.Model = strings.TrimSpace(p.Model)
		p.Format, p.BaseURL, p.APIKey, p.APIKeyHint = "openai-responses", "", "", ""
		p.MaxTokens, p.MaxTokensField, p.SessionHeaderKey, p.Streaming = 0, "", "", true
		if p.Model == "" {
			return ErrChatGPTModelRequired
		}
	case AuthModeClaudeCode:
		p.Model = strings.TrimSpace(p.Model)
		p.ReasoningEffort = strings.ToLower(strings.TrimSpace(p.ReasoningEffort))
		p.Format, p.BaseURL, p.APIKey, p.APIKeyHint = "anthropic", "", "", ""
		p.MaxTokens, p.MaxTokensField, p.SessionHeaderKey, p.Streaming = 0, "", "", true
		p.RatePerSecond, p.RatePerMinute, p.ThinkingType = 0, 0, ""
		if !llmauth.ValidClaudeEffort(p.ReasoningEffort) {
			return ErrBadClaudeEffort
		}
	}
	return nil
}
