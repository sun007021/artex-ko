package claudecode

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// 에러 분류. 메시지 문구는 agent.IsQuotaExhaustedMessage 와 server 의 재시도 판정
// (isRetryableStreamError)이 문자열로 읽으므로 의도적으로 정해 둔다.
//
//   - 사용량 한도(구독 5시간/주간 한도 소진): 마커 claude_code_usage_limit 를 넣어 장애 전환
//     대상이 되게 한다. "rate limit"/"too many requests" 문구는 넣지 않는다(일시적 한도로
//     오분류되어 전환이 막힌다).
//   - 인증/계정 문제: "status 401" 을 넣어 같은 provider 재시도 대상에서 뺀다.
//   - 그 밖의 일시 오류(과부하·서버 오류·프로세스 크래시): 재시도 가능, 게이트가 간격을 둔다.

// UsageLimitMarker 는 구독 사용량 한도 오류를 표시하는 고정 토큰이다.
const UsageLimitMarker = "claude_code_usage_limit"

// SetupErrorMarker 는 설치·버전·정책 거부처럼 같은 요청을 되풀이해도 소용없는 오류에 붙는 고정 접두다.
// server 의 재시도 판정이 이 문자열을 보고 같은 provider 재시도를 건너뛴다.
const SetupErrorMarker = "claude_code_setup"

// ErrKind 는 오류의 성격이다.
type ErrKind int

const (
	KindUnknown    ErrKind = iota
	KindUsageLimit         // 구독 사용량 한도 소진(시간이 지나야 풀림)
	KindRateLimit          // 일시적 속도 제한/과부하
	KindAuth               // 로그인 만료·계정 문제 — 사용자가 claude 에서 직접 해결
	KindNotFound           // claude 바이너리 없음
	KindTooOld             // claude 버전이 너무 낮음
	KindCrash              // 프로세스 비정상 종료
	KindProtocol           // 예상 밖 출력 / MCP 연결 실패
	KindRefused            // ARTEX 정책 거부(꺼짐·API Key 인증 등)
	KindStalled            // 출력이 한동안 없어 감시가 프로세스를 정리했다
)

// Error 는 claude 구동 오류다.
type Error struct {
	Kind    ErrKind
	Message string
	// ResetAt 은 사용량 한도가 풀리는 시각(알 수 있을 때만, 아니면 zero).
	ResetAt time.Time
	// Scope 는 한도의 종류(rate_limit_event 의 rateLimitType: five_hour, seven_day, seven_day_opus ...).
	// 모르면 "". 계정 전체에 걸린 한도인지 모델 하나의 한도인지 가르는 데 쓴다(Global).
	Scope string
}

// Global 은 이 사용량 한도가 계정 전체(5시간/주간 공용 한도)에 걸린 것인지 알려 준다. 모델별 주간 한도
// (seven_day_opus 등)와 종류를 모르는 한도는 그 모델에만 적용한다.
func (e *Error) Global() bool {
	return e.Kind == KindUsageLimit && (e.Scope == "five_hour" || e.Scope == "seven_day")
}

func (e *Error) Error() string {
	switch e.Kind {
	case KindUsageLimit:
		return UsageLimitMarker + ": " + e.Message
	case KindAuth:
		return "claude-code status 401: " + e.Message
	case KindNotFound, KindTooOld, KindRefused:
		return SetupErrorMarker + ": " + e.Message
	}
	return "claude-code: " + e.Message
}

func errf(kind ErrKind, format string, a ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, a...)}
}

var usageLimitText = regexp.MustCompile(`(?i)(usage limit|hit your limit|limit reached|reached your .*limit|out of (extra )?usage|limit will reset|limit resets|resets? (at|in|on)\b|weekly limit|5-hour limit|five-hour limit)`)

// classify 는 claude 가 알려 준 오류 범주(assistant.error: rate_limit, authentication_failed,
// billing_error, ...)와 본문 텍스트로 Error 를 만든다.
func classify(category, text string, hint limitHint) *Error {
	text = strings.TrimSpace(text)
	resetAt := hint.Reset
	switch category {
	case "authentication_failed", "oauth_org_not_allowed", "account_on_hold":
		return &Error{Kind: KindAuth, Message: authHint(category, text)}
	case "billing_error":
		return &Error{Kind: KindUsageLimit, Message: orDefault(text, "billing_error"), ResetAt: resetAt, Scope: hint.Type}
	case "rate_limit":
		if usageLimitText.MatchString(text) || !resetAt.IsZero() {
			return &Error{Kind: KindUsageLimit, Message: orDefault(text, "usage limit reached"), ResetAt: resetAt, Scope: hint.Type}
		}
		return &Error{Kind: KindRateLimit, Message: orDefault(text, "request throttled")}
	case "overloaded", "server_error":
		return &Error{Kind: KindRateLimit, Message: category + ": " + text}
	case "":
		if usageLimitText.MatchString(text) {
			return &Error{Kind: KindUsageLimit, Message: text, ResetAt: resetAt, Scope: hint.Type}
		}
	}
	if category == "" {
		return &Error{Kind: KindUnknown, Message: orDefault(text, "unknown error")}
	}
	return &Error{Kind: KindUnknown, Message: category + ": " + text}
}

func authHint(category, text string) string {
	hint := "claude 로그인이 필요하거나 계정에 문제가 있습니다. 터미널에서 직접 `claude auth login` 을 실행해 로그인하세요(ARTEX 는 로그인에 관여하지 않습니다)"
	switch category {
	case "oauth_org_not_allowed":
		hint = "이 claude 로그인은 허용되지 않는 조직의 계정입니다. 터미널에서 `claude auth status` 로 확인하세요"
	case "account_on_hold":
		hint = "Anthropic 계정이 보류 상태입니다. claude.ai 에서 계정 상태를 확인하세요"
	}
	if text != "" {
		hint += " (" + text + ")"
	}
	return hint
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

// IsUsageLimit 은 err 가 claude 구독 사용량 한도 소진인지 타입으로 판정한다(문자열 마커가 아니다).
func IsUsageLimit(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == KindUsageLimit
}

// IsSetupError 는 err 가 설치·버전·정책 거부처럼 같은 요청을 되풀이해도 소용없는 오류인지 알려 준다.
func IsSetupError(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Kind {
	case KindNotFound, KindTooOld, KindRefused:
		return true
	}
	return false
}
