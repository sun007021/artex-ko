// Package agent wires real LLM-driven planner and work agents (on top of the
// agent-core SDK) to the dual SQLite graph. See docs/ARTEX-架构设计.md
// §4.3 (planner) and §4.4 (work agent).
//
// Provider configuration is read from the environment so the system runs with
// any Anthropic- or OpenAI-format endpoint. If no key is configured, FromEnv
// returns ok=false and the exploration engine stays idle (an LLM is required).
package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/claudecode"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/artex/siwc"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/transcript"
)

// Config describes the LLM backend resolved from the environment.
type Config struct {
	Format  llm.Format
	BaseURL string
	APIKey  string
	Model   string
	// Proxy routes all LLM requests through the given proxy URL (http/https/socks5,
	// optionally with user:pass@ credentials). Empty means direct — it does NOT
	// fall back to the standard *_PROXY environment variables.
	Proxy string
	// RatePerSecond / RatePerMinute cap the shared request rate across ALL agents
	// using the provider (0 = that window unlimited).
	RatePerSecond float64
	RatePerMinute float64
	// ContextWindowK is the model's context window in K tokens (user-configured),
	// used to size compaction thresholds. 0 = default; see CompactionWindow.
	ContextWindowK int
	// ThinkingType 独立控制思考「开关」字段(thinking.type):
	//   "" = 不发送(默认,兼容不支持该字段的模型); "disabled" = 显式关闭;
	//   "enabled" = 开启. 与 ReasoningEffort 完全解耦——有些接口没有 thinking 字段、
	//   只靠强度参数就能激活思考,故两者可各自单独设置.
	ThinkingType string
	// ReasoningEffort 独立控制思考「强度」字段:
	//   "" = 不发送(默认); "low"/"medium"/"high"/"xhigh"/"max" = 对应强度.
	//   OpenAI 映射为顶层 reasoning_effort;Anthropic 映射为 output_config.effort.
	ReasoningEffort string
	// Stream 控制该 profile 是否使用流式(SSE)接口。true(默认)= 流式;false = 真·
	// 非流式(发 stream:false,一次性拿完整 JSON,走 Provider.Complete)。非流式可绕开
	// 某些网关糟糕的 SSE 实现(空帧、思考字段丢帧),代价是失去运行中的实时进度/实时
	// token 计数。映射为 agentcore.Options.NonStreaming = !Stream。
	Stream bool
	// MaxTokens 是单次回复的输出上限(token)。0 = 不发送该字段,由服务端默认值决定
	// (历史行为)。与 ContextWindowK 不同:后者是模型总容量,只在本地用来算压缩阈值,
	// 不出现在请求里;本值随每次请求发出。映射为 agentcore.Options.MaxTokens。
	MaxTokens int
	// MaxTokensField 选择 MaxTokens 用哪个请求字段名,仅对 format=openai 生效:
	//   "" = max_tokens(默认); "max_completion_tokens" = 新字段。
	// OpenAI 推理模型(o 系列/GPT-5)只认后者,收到 max_tokens 会直接报
	// unsupported_parameter;而多数兼容网关只认前者,故不做自动推断,交由用户按端点选。
	MaxTokensField string
	// SessionHeaderKey,非空时,让每次 LLM 请求带上一个自定义 HTTP 头,头名为该值、
	// 头值为【当前会话的 session id】(chat 会话=conv-<id>,worker=exp<x>-worker-i<intent>
	// 等,见 WorkerSessionID)。用于某些按 session-id 头做提示缓存/粘性路由的网关。
	// 空 = 不发送。值由 transcript.WithSessionID 挂在请求 context 上,由 RoundTripper
	// 读取填入,因此同一共享 provider 也能按会话发出不同的头值。
	SessionHeaderKey string
	// Retry 是该配置解析后的重试参数(profile 覆盖 → 全局策略 → 内置默认,由
	// server 侧解析)。三层的含义见 RetryConfig;零值 = 完全沿用内置默认。
	Retry RetryConfig
	// AuthMode: "" = 정적 API Key(기본), AuthModeChatGPT = ChatGPT 구독(Sign in with
	// ChatGPT). 구독 모드는 openai-responses 경로를 그대로 쓰되 Bearer 를 TokenSource 의
	// OAuth 액세스 토큰으로 요청마다 채운다(chatgpt.go). BaseURL/APIKey 는 무시되고,
	// MaxTokens·Temperature 등 지원되지 않는 필드는 전송 직전에 제거된다.
	AuthMode string
	// TokenSource 는 AuthMode=chatgpt 일 때 필수다. Config 는 == 로 비교되므로(server 가
	// 캐시된 구성과 비교한다) 포인터 구현이어야 한다.
	TokenSource TokenSource
	// ClaudeBinary/ClaudeWorkDir 는 AuthMode=claude-code 일 때만 쓴다(claudecode.go). 둘 다
	// 문자열이라 Config 는 계속 == 로 비교된다. 비어 있으면 PATH 의 "claude" / 임시 디렉터리.
	ClaudeBinary  string
	ClaudeWorkDir string
	// ClaudeManager 는 claude-code 의 실행 환경(슬롯·게이트·세션 표)이다. 서버가 하나 만들어 모든
	// 프로필의 Config 에 넣는다. 포인터라서 Config 는 계속 == 로 비교된다.
	ClaudeManager *claudecode.Manager
}

// RetryConfig 是随一个 LLM 配置走的重试参数。每层的「次数」统一语义:
// 0 = 用内置默认次数;负数 = 关闭该层重试;>0 = 用该值。每层的「间隔」:
// 0 = 用该层原本的指数退避;>0 = 改用这个固定间隔。
type RetryConfig struct {
	// ConnectAttempts/ConnectInterval:SDK 建连重试(连接重置/超时/429/5xx,流开始前),
	// 直接映射为 llm.Config.MaxRetries / RetryInterval。默认 3 次、0.5s 起指数(封顶 8s)。
	ConnectAttempts int
	ConnectInterval time.Duration
	// EmptyAttempts/EmptyInterval:SDK 空响应重试(完成但无 content block,仅 openai
	// 格式),映射为 llm.Config.EmptyResponseRetries / EmptyResponseInterval。
	// 默认 2 次、同一条指数梯度。
	EmptyAttempts int
	EmptyInterval time.Duration
	// StreamAttempts/StreamInterval:同 provider 安全窗口重试——本项目在 SDK 之上补的
	// 一层,只在「还没向调用方交付任何输出」时重放断流/过载/流内 429。SDK 看不到它,
	// 由 server/task_llm.go 消费。默认 2 次、0.5s 起指数(封顶 4s)。
	StreamAttempts int
	StreamInterval time.Duration
}

// compaction window resolution bounds (in K tokens). Below the floor the
// threshold math (window − summary reserve − buffer) would go non-positive and
// compaction would fire every turn; above the cap it would never fire.
const (
	defaultWindowK = 200  // unset → assume a 200K window (Claude default)
	minWindowK     = 32   // floor so effectiveWindow stays comfortably positive
	maxWindowK     = 1000 // cap at 1M tokens (user request)
)

// CompactionWindow returns the model context window in TOKENS for compaction
// thresholds, resolved from the user-configured size (ContextWindowK). 0/unset →
// a 200K default; otherwise clamped to [32K, 1M] so compaction stays effective.
func (c Config) CompactionWindow() int {
	k := c.ContextWindowK
	if k <= 0 {
		k = defaultWindowK
	}
	if k < minWindowK {
		k = minWindowK
	}
	if k > maxWindowK {
		k = maxWindowK
	}
	return k * 1000
}

// compactionConfig builds the agent-core compaction config for a context window
// in tokens. agentcore.NewSession wires the summarizer (same provider) when this
// is set on Options.Compaction.
func compactionConfig(windowTokens int) *compaction.Config {
	if windowTokens <= 0 {
		windowTokens = defaultWindowK * 1000
	}
	return &compaction.Config{ContextWindow: windowTokens}
}

// FromEnv resolves the LLM provider config:
//
//	ARTEX_LLM_PROVIDER = anthropic|openai (default: inferred from keys)
//	ARTEX_LLM_MODEL    = model id        (default: per provider)
//	ARTEX_LLM_BASE_URL = endpoint        (optional)
//	ARTEX_LLM_PROXY    = proxy URL        (optional; http/https/socks5)
//	ANTHROPIC_API_KEY / OPENAI_API_KEY         = credentials
func FromEnv() (Config, bool) {
	prov := os.Getenv("ARTEX_LLM_PROVIDER")
	anthKey := os.Getenv("ANTHROPIC_API_KEY")
	oaiKey := os.Getenv("OPENAI_API_KEY")

	if prov == "" {
		switch {
		case anthKey != "":
			prov = "anthropic"
		case oaiKey != "":
			prov = "openai"
		default:
			return Config{}, false
		}
	}

	c := Config{
		BaseURL: os.Getenv("ARTEX_LLM_BASE_URL"),
		Model:   os.Getenv("ARTEX_LLM_MODEL"),
		Proxy:   strings.TrimSpace(os.Getenv("ARTEX_LLM_PROXY")),
		// 默认流式;ARTEX_LLM_STREAM=false/0/off 显式关闭走非流式。
		Stream: !isFalsy(os.Getenv("ARTEX_LLM_STREAM")),
	}
	switch prov {
	case "openai":
		c.Format = llm.FormatOpenAI
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		c.APIKey = anthKey
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	if c.APIKey == "" {
		return Config{}, false
	}
	return c, true
}

// ConfigFrom builds a Config from UI-provided strings (provider defaults to
// anthropic; model defaults per provider). Inputs are trimmed and the base URL
// is normalized to the API base the provider expects (the provider appends the
// endpoint path itself), so a full endpoint URL is tolerated.
func ConfigFrom(provider, model, baseURL, apiKey, proxy string) Config {
	c := Config{
		Model:   strings.TrimSpace(model),
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		Proxy:   strings.TrimSpace(proxy),
		Stream:  true, // 默认流式;调用方按 profile 覆盖
	}
	switch strings.TrimSpace(provider) {
	case "openai":
		c.Format = llm.FormatOpenAI
		// provider appends "/chat/completions"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/chat/completions"), "/")
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		// provider appends "/responses"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/responses"), "/")
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		// provider appends "/v1/messages".
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/v1/messages"), "/")
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	return c
}

// isFalsy reports whether an env-var string explicitly requests "off". Empty or
// unrecognized → false (so an unset var keeps the streaming default).
func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// Provider returns the short provider name ("anthropic"/"openai").
func (c Config) Provider() string {
	switch c.Format {
	case llm.FormatOpenAI:
		return "openai"
	case llm.FormatOpenAIResponses:
		return "openai-responses"
	}
	return "anthropic"
}

// NewProvider builds an llm.Provider from the config. When a rate is set, the
// limiter lives on the single provider instance — so planner + all workers +
// main agent (which share this provider) are bounded by one shared rate limit.
func (c Config) NewProvider() (llm.Provider, error) {
	if c.UsesClaudeCode() {
		// 구독으로 로그인된 claude CLI 가 모델 턴을 만든다(HTTP 클라이언트·API Key 없음).
		return c.newClaudeCodeProvider()
	}
	var client *http.Client
	var err error
	baseURL, apiKey, format := c.BaseURL, c.APIKey, c.Format
	if c.UsesChatGPT() {
		if c.TokenSource == nil {
			return nil, fmt.Errorf("llm: chatgpt auth requires a token source")
		}
		// 구독 토큰은 api.openai.com/v1 로만 간다 — 프로필의 base_url 은 무시한다.
		// 세션 헤더 같은 비규격 헤더도 보내지 않는다.
		client, err = chatGPTHTTPClient(c.Proxy, c.TokenSource)
		baseURL, apiKey, format = ChatGPTBaseURL, chatGPTPlaceholderKey, llm.FormatOpenAIResponses
	} else {
		client, err = quotaAwareHTTPClient(c.Proxy, c.SessionHeaderKey)
	}
	if err != nil {
		return nil, err
	}
	lc := llm.Config{
		Format:     format,
		BaseURL:    baseURL,
		APIKey:     apiKey,
		Model:      c.Model,
		HTTPClient: client,
	}
	// 思考开关与强度两个字段各自透传(空 = 该字段不发送)。二者解耦:
	// 可只发 thinking.type、只发 effort、都发、或都不发。
	lc.ThinkingType = c.ThinkingType
	lc.ReasoningEffort = c.ReasoningEffort
	// 输出上限的字段名选择(空 = 用 max_tokens)。上限的「值」不在这里:它每轮随
	// agentcore.Options.MaxTokens 走,provider 只决定把它塞进哪个键。
	lc.MaxTokensField = c.MaxTokensField
	// 重试参数与 SDK 同语义(次数 0=默认/负=关闭,间隔 0=指数退避/>0=固定),原样透传。
	lc.MaxRetries = c.Retry.ConnectAttempts
	lc.RetryInterval = c.Retry.ConnectInterval
	lc.EmptyResponseRetries = c.Retry.EmptyAttempts
	lc.EmptyResponseInterval = c.Retry.EmptyInterval
	if c.RatePerSecond > 0 || c.RatePerMinute > 0 {
		lc.RateLimit = &llm.RateLimit{PerSecond: c.RatePerSecond, PerMinute: c.RatePerMinute}
	}
	return llm.NewProvider(lc)
}

// IsQuotaExhaustedMessage deliberately recognizes only explicit balance,
// billing, credit, or quota-exhaustion signals. Generic 429/rate-limit text,
// authentication failures, network errors, and server failures are excluded.
var nonFailoverHTTPStatus = regexp.MustCompile(`(?:status(?:\s+code)?|http(?:\s+status)?)\s*[=:]?\s*(?:401|403|5\d\d)\b`)
var transientQuotaLimit = regexp.MustCompile(`(?i)(?:\b(?:rpm|tpm|rpd|qps)\b|quota[_\s-]*metric|rate[_\s-]*limit|too many requests|(?:requests?|tokens?)\s+(?:per|/)\s*(?:second|minute)|(?:per|/)\s*(?:second|minute)\s+(?:requests?|tokens?)|generate[_\s-]*requests[_\s-]*per[_\s-]*(?:minute|second)|tokens?[_\s-]*per[_\s-]*(?:minute|second))`)

func IsQuotaExhaustedMessage(message string) bool {
	message = strings.ToLower(message)
	// ChatGPT 구독의 사용량 한도(subscription_sharing_usage_limit_exceeded)는 일부러 소진으로 보지 않는다.
	// 공식 문서가 구독 요청을 다른 결제 경로(API Key)로 자동 전환하지 말라고 하므로, 한도에 닿으면
	// 사용자에게 오류로 알리고 장애 전환·순환도 막는다(IsNoFailoverMessage). 이 코드를 담은 오류에
	// 다른 소진 문구가 섞여 있어도 마찬가지다.
	if IsNoFailoverMessage(message) {
		return false
	}
	// Claude 구독(claude-code): 5시간/주간 사용량 한도 소진. claudecode 가 오류 앞에 이 고정 마커를
	// 붙인다. 시간이 지나야 풀리는 한도이므로 장애 전환 대상이다.
	if strings.Contains(message, claudecode.UsageLimitMarker) {
		return true
	}
	// Authentication/authorization and provider-side 5xx failures never rotate,
	// even when a gateway happens to echo a quota-looking phrase in the body.
	if nonFailoverHTTPStatus.MatchString(message) {
		return false
	}
	// Provider APIs frequently describe an ordinary rate limit as "quota
	// exceeded", especially Google-style responses containing a quota metric.
	// These limits recover with time and must stay on the current provider.
	if transientQuotaLimit.MatchString(message) {
		return false
	}
	markers := []string{
		"insufficient_quota", "quota_exceeded", "quota exceeded", "quota exhausted",
		"exceeded your current quota", "billing_hard_limit_reached",
		"billing hard limit", "billing_not_active", "credit balance", "insufficient credit",
		"insufficient balance", "balance is too low", "payment required", "status 402",
		"余额不足", "额度不足", "额度已用尽", "欠费",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	// gRPC RESOURCE_EXHAUSTED is overloaded for both account quota and ordinary
	// request-rate limiting. Preserve it as an explicit exhaustion signal only
	// when the same error does not identify a transient rate limit.
	return strings.Contains(message, "resource_exhausted") &&
		!strings.Contains(message, "rate limit") &&
		!strings.Contains(message, "too many requests")
}

// IsQuotaExhaustedError is IsQuotaExhaustedMessage for an error value. A typed claude-code
// usage-limit error is recognized without relying on its message text; everything else (and
// errors that crossed a string boundary) goes through the message classifier unchanged.
func IsQuotaExhaustedError(err error) bool {
	if err == nil {
		return false
	}
	if IsNoFailoverError(err) {
		return false
	}
	if claudecode.IsUsageLimit(err) {
		return true
	}
	return IsQuotaExhaustedMessage(err.Error())
}

// IsNoFailoverMessage reports whether an error must not make a request move to another
// profile automatically, whatever its HTTP status. Today that is the ChatGPT plan usage
// limit: the Sign in with ChatGPT documentation says an exhausted plan is not to be
// silently switched to another billing path, so the error is surfaced instead.
func IsNoFailoverMessage(message string) bool {
	return strings.Contains(strings.ToLower(message), siwc.CodeUsageLimitExceeded)
}

// IsNoFailoverError is IsNoFailoverMessage for an error value.
func IsNoFailoverError(err error) bool { return err != nil && IsNoFailoverMessage(err.Error()) }

// quotaAwareTransport preserves Norma's normal retry behavior except for a 429
// whose body explicitly says the account quota/balance is exhausted. Norma's
// retry loop treats every 429 as transient; normalizing only that response to
// 402 lets a task router fail over immediately while retaining the original
// response body for provider-specific classification and audit logs.
type quotaAwareTransport struct {
	base http.RoundTripper
	// sessionHeaderKey, when non-empty, is the HTTP header name each request
	// carries; its value is the session id read from the request context. Empty
	// disables it. See Config.SessionHeaderKey.
	sessionHeaderKey string
}

func (t quotaAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Custom session-id header: name is user-configured, value is THIS run's
	// session id (norma stashes it on the context via transcript.WithSessionID).
	// Stable across a session's turns and distinct across sessions — exactly what
	// a session-keyed prompt cache wants. Skipped when no session id is present.
	if t.sessionHeaderKey != "" {
		if sid := transcript.SessionIDFrom(req.Context()); sid != "" {
			req.Header.Set(t.sessionHeaderKey, sid)
		}
	}
	// When LLM recording is on, the Recorder puts a Capture on the context so the
	// raw wire bodies can be persisted. This is the only layer that still sees
	// them: norma builds the request body internally and decodes the SSE response
	// before either reaches the recorder.
	capt := llmrec.CaptureFrom(req.Context())
	capt.SetRequest(requestBodySnapshot(req))

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Tee rather than read: a 200 is an SSE stream that must keep streaming. The
	// 429 branch below reads through this wrapper, so its body lands in the
	// capture before being replaced.
	resp.Body = capt.TeeResponse(resp.StatusCode, resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if readErr != nil {
		return resp, nil
	}
	if IsQuotaExhaustedMessage(string(body)) {
		resp.StatusCode = http.StatusPaymentRequired
		resp.Status = "402 Payment Required"
	}
	return resp, nil
}

// requestBodySnapshot copies an outgoing request body without consuming it.
// norma builds every model request from a *bytes.Reader, so net/http populates
// GetBody and the copy has no effect on what gets sent.
func requestBodySnapshot(req *http.Request) string {
	if req.GetBody == nil {
		return ""
	}
	rc, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return ""
	}
	return string(b)
}

// baseTransport builds an http.Transport: direct unless an explicit proxy is configured
// (it never falls back to the *_PROXY environment variables). Errors never echo the
// proxy value — it may carry credentials (user:pass@host); see describeProxy.
func baseTransport(proxy string) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		transport.Proxy = nil // 留空=直连,不回退 HTTP_PROXY/HTTPS_PROXY 环境变量
		return transport, nil
	}
	proxyURL, err := url.Parse(proxy)
	if err != nil {
		return nil, fmt.Errorf("llm: invalid proxy URL")
	}
	switch proxyURL.Scheme {
	case "http", "https", "socks5":
	case "":
		return nil, fmt.Errorf("llm: proxy %s missing scheme (use http://, https:// or socks5://)", describeProxy(proxy))
	default:
		return nil, fmt.Errorf("llm: unsupported proxy scheme %q (use http, https or socks5)", proxyURL.Scheme)
	}
	if proxyURL.Host == "" {
		return nil, fmt.Errorf("llm: proxy %s has no host", describeProxy(proxy))
	}
	transport.Proxy = http.ProxyURL(proxyURL)
	return transport, nil
}

// describeProxy renders a proxy setting for logs and errors without its credentials:
// scheme://host only. Anything that does not parse is not shown at all.
func describeProxy(proxy string) string {
	u, err := url.Parse(strings.TrimSpace(proxy))
	if err != nil || u.Host == "" {
		return "<redacted>"
	}
	return u.Scheme + "://" + u.Host
}

// DescribeProxy is describeProxy for other packages (server logs).
func DescribeProxy(proxy string) string { return describeProxy(proxy) }

// transports caches one http.Transport per proxy setting for the clients that
// outlive a single call (the model catalog, the auth server), so repeated use shares
// connections instead of building — and leaking — a transport every time.
var transports = struct {
	sync.Mutex
	m map[string]*http.Transport
}{m: map[string]*http.Transport{}}

const maxCachedTransports = 16

func sharedTransport(proxy string) (*http.Transport, error) {
	key := strings.TrimSpace(proxy)
	transports.Lock()
	defer transports.Unlock()
	if tr := transports.m[key]; tr != nil {
		return tr, nil
	}
	tr, err := baseTransport(key)
	if err != nil {
		return nil, err
	}
	if len(transports.m) >= maxCachedTransports { // a changing proxy setting must not accumulate idle pools
		for k, old := range transports.m {
			old.CloseIdleConnections()
			delete(transports.m, k)
		}
	}
	transports.m[key] = tr
	return tr, nil
}

// ProxyHTTPClient builds a plain *http.Client that honours the same explicit-proxy
// rule as the LLM providers (empty = direct, never the *_PROXY environment
// variables). The sign-in code uses it so auth.openai.com calls follow a profile's
// proxy setting too.
func ProxyHTTPClient(proxy string, timeout time.Duration) (*http.Client, error) {
	transport, err := sharedTransport(proxy)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

func quotaAwareHTTPClient(proxy, sessionHeaderKey string) (*http.Client, error) {
	transport, err := baseTransport(proxy)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: quotaAwareTransport{base: transport, sessionHeaderKey: strings.TrimSpace(sessionHeaderKey)}}, nil
}

// chatGPTHTTPClient is quotaAwareHTTPClient for subscription profiles: the quota
// layer stays outermost (raw capture) and the
// token-injecting transport sits between it and the network.
func chatGPTHTTPClient(proxy string, src TokenSource) (*http.Client, error) {
	transport, err := sharedTransport(proxy)
	if err != nil {
		return nil, err
	}
	inner := &chatgptTransport{base: transport, src: src, endpoint: chatGPTResponsesEndpoint}
	return &http.Client{
		Transport: quotaAwareTransport{base: inner},
		// 리다이렉트를 따라가지 않는다 — 자격 증명이 다른 호스트로 갈 길을 아예 막는다.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// logTestConnection prints the raw HTTP status code(s) and response body of a
// connection test to the server log, so "点击测试" leaves a diagnosable trail of
// exactly what the gateway returned — 401 bodies, quota text, empty frames — not
// just the collapsed ok/err the UI shows. Bodies are clipped to keep a chatty
// SSE stream from flooding the log.
func logTestConnection(c Config, capt *llmrec.Capture) {
	attempts := capt.Attempts()
	if len(attempts) == 0 {
		log.Printf("[llm-test] %s / %s @ %s — 未发出任何 HTTP 请求(配置解析或建连即失败)",
			c.Provider(), c.Model, c.BaseURL)
		return
	}
	for i, a := range attempts {
		log.Printf("[llm-test] %s / %s @ %s — 尝试 %d/%d HTTP %d\n响应体: %s",
			c.Provider(), c.Model, c.BaseURL, i+1, len(attempts), a.Status, clipBody(a.Body))
	}
}

// clipBody trims a wire body for logging. 4K is plenty to show an error JSON or
// the head of an SSE stream while bounding a runaway response.
func clipBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(空)"
	}
	const max = 4096
	if len(s) > max {
		return s[:max] + fmt.Sprintf("…(截断,共 %d 字节)", len(s))
	}
	return s
}

// TestConnection makes a minimal real completion to verify the provider/model/
// endpoint/key actually work. Returns the round-trip latency and the model's
// reply text.
func TestConnection(ctx context.Context, c Config) (time.Duration, string, error) {
	prov, err := c.NewProvider()
	if err != nil {
		return 0, "", err
	}
	timeout := 30 * time.Second
	if c.UsesClaudeCode() {
		timeout = 90 * time.Second // claude 프로세스 기동 + 첫 응답까지 API 호출보다 오래 걸린다
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// 抓取原始 wire 报文:连接测试最需要看到的就是网关到底回了什么(状态码+响应体),
	// 而 norma 把响应解码成 StreamEvent 后这些就没了。quotaAwareTransport 会在
	// context 里找到这个 Capture 并填入每次 HTTP 尝试的状态码与 body。
	ctx, capt := llmrec.NewCapture(ctx)
	defer logTestConnection(c, capt)
	// 连接测试是一条单发路径,不经过 agentcore 的会话循环,因此没人往 context 上挂
	// session id。对配了 SessionHeaderKey 的端点(如 opencode zen 强制要求
	// x-opencode-session 头,缺了直接 400 MissingSessionID),这会导致"对话正常、
	// 点击测试却 400"的落差。这里补挂一个一次性随机 session id,让测试与真实对话走同
	// 一套发头逻辑;未配 SessionHeaderKey 的端点不读它,无副作用。
	sid := "conntest-" + transcript.NewSessionID()
	ctx = transcript.WithSessionID(ctx, sid)
	if c.UsesClaudeCode() && c.ClaudeManager != nil {
		defer c.ClaudeManager.CloseSession(sid) // 테스트가 끝나면 이 세션의 claude 프로세스를 바로 정리한다
	}
	start := time.Now()
	// MaxTokens 要给足：推理模型(如 deepseek-v4-pro)在给出答案前会先产出一大段
	// 思考(实测对一句 "ping" 也能烧 ~2900 token)。若只给 32,模型会一直卡在"思考阶段"
	// 就撞到输出上限(finish=length)、被截断,连接测试虽仍算通(err=nil)但显示成
	// "已中断/length/resume" 一团糟。给足预算让它把 OK 干净吐完(finish=stop)。
	// EscalateMaxTokens 保持 false:不因截断而抬额重试,避免 resume 循环空烧。
	reply, err := agentcore.Run(ctx, agentcore.Options{
		Provider:       prov,
		SystemPrompt:   []string{"你是连接测试。直接输出两个字符 OK 即可，不要思考、不要解释、不要别的。"},
		PermissionMode: acperm.ModeBypass,
		MaxTurns:       1,
		MaxTokens:      8192,
		NonStreaming:   !c.Stream, // 用该 profile 的真实收发模式做连接测试
	}, "ping")
	lat := time.Since(start)
	if err != nil {
		return lat, "", err
	}
	// err==nil 还不够：请求通了但模型一个字都不吐的情况真实存在(思考把预算烧光、
	// 正文被安全策略吞掉、兼容层把 content 丢了)。这种配置在会话里就是"不回话",
	// 测试却报成功——正是本项要消除的落差。没有可见正文一律判失败。
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return lat, "", fmt.Errorf("模型无回复内容（请求已通，但未返回任何文本）")
	}
	return lat, reply, nil
}
