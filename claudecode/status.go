package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// AuthMethod 값은 `claude auth status` JSON 의 authMethod 필드다.
// 출처: https://code.claude.com/docs/en/cli-reference (claude auth status)
const (
	AuthNone         = "none"
	AuthClaudeAI     = "claude.ai"
	AuthOAuthToken   = "oauth_token"
	AuthAPIKey       = "api_key"
	AuthAPIKeyHelper = "api_key_helper"
	AuthThirdParty   = "third_party"
)

// Status 는 claude 바이너리 점검 결과다. 자격 증명은 담지 않는다 — 로그인 여부와 방식만 있다.
type Status struct {
	Binary    string `json:"binary"`           // 실제로 실행한 경로(해석 후)
	Installed bool   `json:"installed"`        // 실행 파일을 찾았다
	Version   string `json:"version"`          // x.y.z (모르면 "")
	VersionOK bool   `json:"version_ok"`       // MinVersion 이상
	MinVer    string `json:"min_version"`      // 요구 최소 버전
	LoggedIn  bool   `json:"logged_in"`        // claude auth status 의 loggedIn
	Method    string `json:"auth_method"`      // authMethod (none/claude.ai/oauth_token/api_key/...)
	Usable    bool   `json:"usable"`           // 설치+버전+구독 로그인(claude.ai/oauth_token) 모두 충족
	Problem   string `json:"problem"`          // 사용 불가 사유 코드: not_installed/too_old/not_logged_in/api_billing/unknown
	Detail    string `json:"detail,omitempty"` // 사람이 읽는 보충 설명(비밀 아님)
}

// 사용 불가 사유 코드.
const (
	ProblemNotInstalled = "not_installed"
	ProblemTooOld       = "too_old"
	ProblemNotLoggedIn  = "not_logged_in"
	ProblemAPIBilling   = "api_billing" // authMethod 가 구독이 아니다 → API 과금 경로
	ProblemUnknown      = "unknown"
)

// ResolveBinary 는 설정된 경로(또는 PATH 의 "claude")를 실행 파일 경로로 해석한다.
func ResolveBinary(configured string) (string, error) {
	name := strings.TrimSpace(configured)
	if name == "" {
		name = "claude"
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", errf(KindNotFound, "claude 실행 파일을 찾을 수 없습니다(%s). Claude Code 를 설치하고 터미널에서 `claude auth login` 으로 로그인하세요: https://code.claude.com/docs/en/setup", name)
	}
	return p, nil
}

// Probe 는 `claude --version` 과 `claude auth status` 를 실행해 사용 가능 여부를 판정한다.
// 두 명령은 세션과 같은 정제 환경(SanitizedEnv)으로 실행되므로, ARTEX 환경에 ANTHROPIC_API_KEY 가
// 있어도 "구독으로 실제 쓰일 인증"을 보고한다. 이 함수는 어떤 자격 증명 파일도 읽지 않는다.
func Probe(ctx context.Context, configuredBinary string) Status {
	st := Status{MinVer: MinVersion.String()}
	bin, err := ResolveBinary(configuredBinary)
	if err != nil {
		st.Problem, st.Detail = ProblemNotInstalled, err.Error()
		return st
	}
	st.Binary, st.Installed = bin, true
	env := SanitizedEnv(os.Environ())
	// 세션과 같은 격리: 같은 정제 환경에 ARTEX 전용 빈 작업 디렉터리에서 실행한다(ARTEX 를 띄운 디렉터리의
	// 프로젝트·로컬 설정이 상태 점검에 섞이지 않게).
	dir, derr := os.MkdirTemp("", "artex-claude-probe-")
	if derr != nil {
		st.Problem, st.Detail = ProblemUnknown, "점검용 작업 디렉터리를 만들 수 없습니다: "+derr.Error()
		return st
	}
	defer os.RemoveAll(dir)

	out, _, verr := runCapture(ctx, bin, env, dir, 15*time.Second, "--version")
	if verr != nil {
		st.Problem, st.Detail = ProblemUnknown, "claude --version 실행 실패: "+verr.Error()
		return st
	}
	if v, ok := ParseVersion(out); ok {
		st.Version = v.String()
		st.VersionOK = !v.Less(MinVersion)
	}
	if !st.VersionOK {
		st.Problem = ProblemTooOld
		st.Detail = "Claude Code " + MinVersion.String() + " 이상이 필요합니다(현재 " + orDefault(st.Version, "알 수 없음") + "). `claude update` 로 갱신하세요"
		return st
	}

	// 종료 코드: 로그인됨 0, 아니면 1. 두 경우 모두 stdout 의 JSON 을 읽는다.
	aout, _, aerr := runCapture(ctx, bin, env, dir, 20*time.Second, "auth", "status")
	var ee *exec.ExitError
	if aerr != nil && !errors.As(aerr, &ee) {
		st.Problem, st.Detail = ProblemUnknown, "claude auth status 실행 실패: "+aerr.Error()
		return st
	}
	var js struct {
		LoggedIn   *bool  `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	_ = json.Unmarshal(bytes.TrimSpace([]byte(aout)), &js)
	st.Method = js.AuthMethod
	switch {
	case js.LoggedIn != nil:
		st.LoggedIn = *js.LoggedIn
	default:
		st.LoggedIn = aerr == nil // JSON 을 못 읽으면 종료 코드로 판단
	}
	if st.Method == "" && !st.LoggedIn {
		st.Method = AuthNone
	}
	switch {
	case !st.LoggedIn || st.Method == AuthNone:
		st.Problem = ProblemNotLoggedIn
		st.Detail = "로그인되어 있지 않습니다. 터미널에서 직접 `claude auth login` 을 실행해 본인 구독으로 로그인하세요. ARTEX 는 로그인에 관여하지 않으며 토큰을 읽거나 저장하지 않습니다"
	case st.Method == "":
		st.Problem = ProblemUnknown
		st.Detail = "claude auth status 출력에서 인증 방식(authMethod)을 읽지 못했습니다. Claude Code 를 갱신하고 `claude auth status` 를 직접 실행해 보세요"
	case st.Method == AuthClaudeAI || st.Method == AuthOAuthToken:
		st.Usable = true
	default:
		st.Problem = ProblemAPIBilling
		st.Detail = "claude 가 구독이 아닌 인증 방식(" + st.Method + ")을 쓰고 있어 API 과금 경로가 됩니다. ARTEX 는 구독 로그인(claude.ai)만 허용합니다. `claude auth login` 으로 구독에 로그인하고, apiKeyHelper/클라우드 공급자 설정이 없는지 확인하세요"
	}
	return st
}

// runCapture 는 bin 을 args 로 실행해 stdout/stderr 를 돌려준다.
func runCapture(ctx context.Context, bin string, env []string, dir string, timeout time.Duration, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.WaitDelay = 3 * time.Second
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err = cmd.Run()
	return so.String(), se.String(), err
}
