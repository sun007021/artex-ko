// Package llmauth holds the values that describe how an LLM profile authenticates and that more than one
// layer (db, agent, server, siwc, claudecode) must agree on: the auth-mode strings, the OAuth connection
// status values, the plan-usage scope and the claude-code concurrency bounds.
//
// It is a leaf package (no imports from this module) so every layer can depend on it without cycles. The
// layers keep their own named constants (db.AuthModeChatGPT, siwc.StatusConnected, ...) as aliases of
// these, so call sites stay readable while the literal lives in exactly one place.
package llmauth

// Auth modes stored in llm_profiles.auth_mode.
const (
	// ModeAPIKey is the default: a static API key and an explicit base URL.
	ModeAPIKey = ""
	// ModeChatGPT runs on the user's ChatGPT plan through "Sign in with ChatGPT".
	ModeChatGPT = "chatgpt"
	// ModeClaudeCode drives the official, user-logged-in claude CLI.
	ModeClaudeCode = "claude-code"
)

// ValidMode reports whether m is a storable auth mode.
func ValidMode(m string) bool {
	return m == ModeAPIKey || m == ModeChatGPT || m == ModeClaudeCode
}

// OAuth connection status values (llm_profile_oauth.status).
const (
	StatusDisconnected = "disconnected" // no tokens (signed out, or registration only)
	StatusConnected    = "connected"    // tokens held
	StatusReauth       = "reauth_required"
)

// ScopePlanUsage is the OAuth scope that authorizes plan-billed inference.
const ScopePlanUsage = "chatgpt.tokens.use.direct"

// HasScope reports whether want is among scopes.
func HasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// claude-code: how many claude processes may produce a model response at the same time. The subscription
// assumes "ordinary, individual usage", so the ceiling is small and cannot be raised by configuration.
const (
	ClaudeCodeDefaultConcurrent = 1
	ClaudeCodeHardConcurrent    = 3
)

// ClampClaudeCodeConcurrent keeps n within [1, ClaudeCodeHardConcurrent]; n <= 0 means the default.
func ClampClaudeCodeConcurrent(n int) int {
	if n <= 0 {
		return ClaudeCodeDefaultConcurrent
	}
	if n > ClaudeCodeHardConcurrent {
		return ClaudeCodeHardConcurrent
	}
	return n
}
