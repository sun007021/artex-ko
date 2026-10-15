package claudecode

import (
	"strings"
)

// 자식 claude 프로세스의 환경변수 정책.
//
// 원칙: 허용 목록(allowlist). 자식에게는 실행에 필요한 일반 환경(PATH, HOME, 로캘, 임시 디렉터리,
// Windows 시스템 변수, 프록시·TLS 변수, CLAUDE_CONFIG_DIR, XDG_*)만 넘기고 나머지는 전부 뺀다.
// 거부 목록 방식은 새 인증·라우팅 변수(CLAUDE_CODE_USE_MANTLE 처럼 나중에 생기는 것)가 생길 때마다
// 구멍이 되므로 쓰지 않는다. 특히
//   - ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN 이 있으면 claude 는 구독 대신 그것을 쓴다
//     (-p 모드에서는 항상). ARTEX 자신의 환경(.env, docker-compose 등)에 이 값이 있어도
//     의도치 않게 API 과금 경로로 새지 않는다.
//   - CLAUDE_CODE_OAUTH_TOKEN 은 `claude setup-token` 으로 만든 토큰이다. ARTEX 는 이 토큰을
//     읽지도, 자식에 전달하지도 않는다(구독 자격 증명을 중개하지 않는다).
//   - ANTHROPIC_BASE_URL / ANTHROPIC_CUSTOM_HEADERS / 페더레이션 변수 / CLAUDE_CODE_USE_* 클라우드 공급자
//     변수, 모델·강도 오버라이드(ANTHROPIC_MODEL, CLAUDE_CODE_EFFORT_LEVEL ...)는 요청 목적지·자격 증명·
//     프로필 설정을 바꾸므로 허용 목록에 없다. CLAUDE_CODE_SIMPLE 은 --bare 와 같은 효과(구독 로그인 무시)다.
//
// 허용 목록은 이름 비교를 대소문자 무시로 한다(Windows 의 환경 변수 이름이 그렇다).
var allowedEnv = map[string]bool{
	// 실행·사용자 식별
	"PATH": true, "HOME": true, "USER": true, "USERNAME": true, "LOGNAME": true, "SHELL": true, "TERM": true,
	"TZ": true, "LANG": true, "LANGUAGE": true, "__CF_USER_TEXT_ENCODING": true,
	// 임시 디렉터리
	"TMPDIR": true, "TMP": true, "TEMP": true,
	// Windows
	"USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true, "PROGRAMDATA": true, "PROGRAMFILES": true,
	"PROGRAMFILES(X86)": true, "PROGRAMW6432": true, "COMMONPROGRAMFILES": true, "COMMONPROGRAMFILES(X86)": true,
	"SYSTEMROOT": true, "SYSTEMDRIVE": true, "WINDIR": true, "COMSPEC": true, "PATHEXT": true, "OS": true,
	"PROCESSOR_ARCHITECTURE": true, "NUMBER_OF_PROCESSORS": true, "COMPUTERNAME": true, "USERDOMAIN": true,
	"HOMEDRIVE": true, "HOMEPATH": true, "PUBLIC": true, "ALLUSERSPROFILE": true,
	// 프록시·TLS (네트워크 경로만 바꾸고 인증은 바꾸지 않는다)
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true, "REQUESTS_CA_BUNDLE": true, "CURL_CA_BUNDLE": true,
	// 사용자가 옮겨 둔 claude 설정 위치(로그인 상태가 여기 있다), Windows 의 git-bash 경로
	"CLAUDE_CONFIG_DIR": true, "CLAUDE_CODE_GIT_BASH_PATH": true,
}

// allowedEnvPrefixes 는 접두사 단위로 허용하는 변수다(로캘, XDG 디렉터리).
var allowedEnvPrefixes = []string{"LC_", "XDG_"}

// hardenedEnv 는 항상 덮어쓰는 변수다. 사용자의 개인 설정(CLAUDE.md, 자동 메모리, 훅, 플러그인,
// 백그라운드 작업 등)이 ARTEX 에이전트에 새어 들어오지 않게 하고, MCP 도구 호출이 시간 제한이나
// 자동 백그라운드 전환으로 끊기지 않게 한다(ARTEX 도구는 수 분~수 시간 걸릴 수 있고, 그 사이
// MCP 호출은 ARTEX 가 결과를 돌려줄 때까지 열려 있어야 한다).
var hardenedEnv = [][2]string{
	{"CLAUDE_CODE_DISABLE_CLAUDE_MDS", "1"},
	{"CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1"},
	{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1"},
	{"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS", "1"},
	{"CLAUDE_CODE_DISABLE_GIT_INSTRUCTIONS", "1"},
	{"CLAUDE_CODE_DISABLE_ATTACHMENTS", "1"},
	{"CLAUDE_CODE_DISABLE_CRON", "1"},
	{"CLAUDE_CODE_DISABLE_BUNDLED_SKILLS", "1"},
	{"CLAUDE_CODE_DISABLE_WORKFLOWS", "1"},
	{"CLAUDE_CODE_DISABLE_EXPLORE_PLAN_AGENTS", "1"},
	{"CLAUDE_AGENT_SDK_DISABLE_BUILTIN_AGENTS", "1"},
	{"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY", "1"},
	{"CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING", "1"},
	{"CLAUDE_CODE_DISABLE_ADVISOR_TOOL", "1"},
	{"CLAUDE_CODE_DISABLE_ARTIFACT", "1"},
	{"CLAUDE_CODE_DISABLE_WEB_FETCH", "1"},
	{"CLAUDE_CODE_SKIP_PROMPT_HISTORY", "1"},
	{"DISABLE_TELEMETRY", "1"},
	{"DISABLE_ERROR_REPORTING", "1"},
	// MCP 도구 스키마를 처음부터 전부 싣는다(도구 검색으로 미루지 않는다). ARTEX 가 자체
	// 지연 로딩(SearchExtraTools)을 이미 갖고 있고, API 경로와 같은 도구 목록을 보게 하기 위함.
	{"ENABLE_TOOL_SEARCH", "false"},
	{"CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS", "0"},
	{"CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT", "0"},
	{"CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH", "100000"},
	{"MAX_MCP_OUTPUT_TOKENS", "200000"},
	{"MCP_TIMEOUT", "60000"},
}

// SanitizedEnv 는 parent("K=V" 목록)에서 허용 목록의 변수만 남긴 자식용 환경을 돌려준다.
// extra 는 마지막에 덧붙여 덮어쓴다(예: 프로필 프록시). 순서가 유지되므로 같은 키는 뒤가 이긴다.
func SanitizedEnv(parent []string, extra ...string) []string {
	out := make([]string, 0, len(parent)+len(hardenedEnv)+len(extra))
	for _, kv := range parent {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if !envAllowed(k) {
			continue
		}
		out = append(out, kv)
	}
	for _, h := range hardenedEnv {
		out = append(out, h[0]+"="+h[1])
	}
	out = append(out, extra...)
	return dedupeEnv(out)
}

func envAllowed(k string) bool {
	uk := strings.ToUpper(k)
	if allowedEnv[uk] {
		return true
	}
	for _, p := range allowedEnvPrefixes {
		if strings.HasPrefix(uk, p) {
			return true
		}
	}
	return false
}

// dedupeEnv 는 같은 키가 여럿이면 마지막 값만 남긴다(os/exec 도 마지막 값을 쓰지만, 테스트와
// 로그 가독성을 위해 정리한다).
func dedupeEnv(env []string) []string {
	last := map[string]int{}
	for i, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		last[k] = i
	}
	out := env[:0:0]
	for i, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if last[k] == i {
			out = append(out, kv)
		}
	}
	return out
}

// proxyEnv 는 프로필에 명시된 프록시를 자식 환경으로 옮긴다(비면 아무것도 하지 않는다 — 사용자
// 환경의 프록시 변수는 그대로 상속된다).
func proxyEnv(proxy string) []string {
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		return nil
	}
	return []string{
		"HTTPS_PROXY=" + proxy, "HTTP_PROXY=" + proxy,
		"https_proxy=" + proxy, "http_proxy=" + proxy,
	}
}

// loopbackNoProxy 는 NO_PROXY 에 루프백을 더한다. 사용자 환경에 HTTP(S)_PROXY 가 있어도 claude 가
// 127.0.0.1 의 ARTEX MCP 엔드포인트에는 직접 접속하게 하기 위함이다(프록시를 거치면 연결 실패).
func loopbackNoProxy(parent []string) []string {
	cur := ""
	for _, kv := range parent {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "NO_PROXY") && v != "" {
			cur = v
		}
	}
	add := "127.0.0.1,localhost,::1"
	if cur != "" {
		add = cur + "," + add
	}
	return []string{"NO_PROXY=" + add, "no_proxy=" + add}
}
