package siwc

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Error is a sanitized failure from the SiwC endpoints. It carries OpenAI's
// machine-readable code plus HTTP status and request id for diagnosis, and never
// a token, an authorization code or a response body.
type Error struct {
	Code      string // OAuth `error` or Responses `error.code`; local codes below
	Status    int
	RequestID string
	Param     string
	Retryable bool
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("siwc: ")
	b.WriteString(e.Code)
	if e.Status != 0 {
		fmt.Fprintf(&b, " (http %d)", e.Status)
	}
	if e.Param != "" {
		fmt.Fprintf(&b, " param=%s", e.Param)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " request_id=%s", e.RequestID)
	}
	return b.String()
}

// Local error codes (not sent by OpenAI).
const (
	CodeNetwork                = "network_error"
	CodeInvalidResponse        = "invalid_response"
	CodeInvalidTokenResponse   = "invalid_token_response"
	CodeInvalidState           = "invalid_state"
	CodeRegistrationIncomplete = "registration_incomplete"
	CodeInvalidIDToken         = "invalid_id_token"
	CodeIdentityUnavailable    = "identity_verification_unavailable"
	CodeAccountMismatch        = "account_mismatch"
	CodeNotConnected           = "not_connected"
	CodeReauthRequired         = "reauth_required"
	CodePlanUsageDisabled      = "plan_usage_disabled"
	CodeRefreshNotReady        = "refresh_not_ready"
	CodeDiscoveryFailed        = "discovery_failed"
	CodeInvalidRedirectURI     = "invalid_redirect_uri"
	CodeAccessDenied           = "access_denied"
)

func newErr(code string, retryable bool) *Error { return &Error{Code: code, Retryable: retryable} }

// Sentinel-style helpers (errors.Is works through Code equality, see Is).
var (
	ErrNotConnected        = newErr(CodeNotConnected, false)
	ErrReauthRequired      = newErr(CodeReauthRequired, false)
	ErrPlanUsageDisabled   = newErr(CodePlanUsageDisabled, false)
	ErrIdentityUnavailable = newErr(CodeIdentityUnavailable, true)
)

// Is lets errors.Is(err, ErrReauthRequired) match any *Error with the same code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// CodeOf returns the siwc error code carried by err, or "".
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// refreshDeadCodes are the OAuth token-endpoint errors after which the refresh
// token is unusable and the user must sign in again (errors-and-recovery §5).
var refreshDeadCodes = map[string]bool{
	"invalid_grant": true, "invalid_refresh_token": true, "token_expired": true,
	"refresh_token_expired": true, "refresh_token_invalidated": true, "refresh_token_reused": true,
}

// RequiresReauth reports whether err is a refresh failure that makes the saved
// refresh token unusable. Apply it only to errors from grant_type=refresh_token,
// not to code exchange or API admission failures (including HTTP 401).
func RequiresReauth(err error) bool {
	return refreshDeadCodes[CodeOf(err)]
}

var safeIdent = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.:\[\]-]{0,159}$`)

func safeIdentifier(s string) string {
	if safeIdent.MatchString(s) {
		return s
	}
	return ""
}

// ParseAPIError builds an *Error from an HTTP error body, covering the three
// shapes in use: OAuth `{"error":"code"}`, Responses `{"error":{"code":..,"param":..}}`
// and admission `{"detail":"..."}` (whose text is diagnostic only, never a code).
func ParseAPIError(status int, body []byte, requestID string) *Error {
	e := &Error{Status: status, RequestID: safeIdentifier(requestID)}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(body, &raw)
	code, param := "", ""
	for depth := 0; depth < 4 && raw != nil; depth++ {
		if v, ok := raw["error"]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil {
				code = s
				break
			}
			var inner map[string]json.RawMessage
			if json.Unmarshal(v, &inner) == nil {
				if c, ok := inner["code"]; ok {
					var cs string
					_ = json.Unmarshal(c, &cs)
					if cs != "" {
						code = cs
					}
				}
				if p, ok := inner["param"]; ok {
					var ps string
					_ = json.Unmarshal(p, &ps)
					param = ps
				}
				if code != "" {
					break
				}
				raw = inner
				continue
			}
		}
		if v, ok := raw["code"]; ok {
			var cs string
			if json.Unmarshal(v, &cs) == nil && cs != "" {
				code = cs
				break
			}
		}
		break
	}
	e.Code = safeIdentifier(code)
	if e.Code == "" {
		e.Code = "api_error"
	}
	e.Param = safeIdentifier(param)
	switch e.Code {
	case "subscription_sharing_usage_unavailable", "subscription_sharing_user_unavailable":
		e.Retryable = true
	case "subscription_sharing_usage_limit_exceeded":
		e.Retryable = false
	default:
		e.Retryable = status == 429 || status >= 500
	}
	return e
}

// IsUsageLimit reports whether code means the user's ChatGPT plan / app usage
// limit is exhausted (quota exhaustion for failover purposes).
func IsUsageLimit(code string) bool { return code == "subscription_sharing_usage_limit_exceeded" }
