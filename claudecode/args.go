package claudecode

import (
	"strconv"
	"strings"
)

// MinVersion 은 이 패키지가 쓰는 플래그·환경변수가 모두 지원되는 최소 Claude Code 버전이다.
// 근거: https://code.claude.com/docs/en/cli-reference
//
//	--permission-prompts none          v2.1.259+ (가장 높은 요구)
//	CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS v2.1.212+
//	CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT  v2.1.187+
var MinVersion = Version{2, 1, 259}

// allowedToolsPattern 은 ARTEX MCP 도구만 권한 프롬프트 없이 실행되게 한다.
const (
	mcpServerName = "artex"
	toolPrefix    = "mcp__" + mcpServerName + "__"
)

// SpawnSpec 은 claude 프로세스 하나를 띄우는 데 필요한 값이다.
type SpawnSpec struct {
	Model  string // --model (비우면 claude 기본)
	Effort string // --effort (low|medium|high|xhigh|max, 비우면 claude 기본)
	// SystemPromptFile 은 --system-prompt-file 로 넘길 파일(ARTEX 역할 프롬프트 전체).
	SystemPromptFile string
	// MCPConfigFile 은 --mcp-config 로 넘길 파일. 비우면 MCP 없이(--strict-mcp-config 만) 띄운다.
	MCPConfigFile string
}

// buildArgs 는 문서화된 CLI 플래그만으로 헤드리스 세션 인자를 만든다.
//
//	-p --input-format stream-json --output-format stream-json --verbose --include-partial-messages
//	    stdin 으로 사용자 메시지를 계속 받고, 부분 메시지(원시 SSE 이벤트)까지 stdout 으로 낸다.
//	--system-prompt-file   ARTEX 역할 프롬프트로 Claude Code 기본 프롬프트를 통째로 대체한다.
//	--tools ""             Claude Code 내장 도구(Bash/Read/Edit 등)를 모두 끈다. 도구는 ARTEX 것만.
//	--strict-mcp-config --mcp-config <file>   사용자 MCP 설정을 무시하고 ARTEX 엔드포인트만 쓴다.
//	--permission-mode dontAsk --allowedTools "mcp__artex__*" --permission-prompts none
//	    ARTEX 도구만 허용, 그 외는 물어보지 않고 거부한다(대화형 프롬프트 없음).
//	--setting-sources project,local   사용자 전역 설정(~/.claude: 훅·플러그인·권한·env)을 읽지 않는다.
//	    작업 디렉터리는 ARTEX 전용 빈 디렉터리이므로 project/local 설정도 비어 있다.
//	--disable-slash-commands   스킬/커맨드 비활성.
//	--no-session-persistence   세션을 디스크에 남기지 않는다.
//
// --bare 는 쓰지 않는다(구독 로그인을 무시한다).
func buildArgs(sp SpawnSpec) []string {
	a := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--tools", "",
		"--permission-mode", "dontAsk",
		"--allowedTools", toolPrefix + "*",
		"--permission-prompts", "none",
		"--setting-sources", "project,local",
		"--disable-slash-commands",
		"--no-session-persistence",
		"--strict-mcp-config",
	}
	if sp.MCPConfigFile != "" {
		a = append(a, "--mcp-config", sp.MCPConfigFile)
	}
	if sp.SystemPromptFile != "" {
		a = append(a, "--system-prompt-file", sp.SystemPromptFile)
	}
	if m := strings.TrimSpace(sp.Model); m != "" {
		a = append(a, "--model", m)
	}
	if e := strings.TrimSpace(sp.Effort); e != "" {
		a = append(a, "--effort", e)
	}
	return a
}

// Version 은 claude --version 의 x.y.z 이다.
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
}

// Less 는 v < o 인지 알려 준다.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// ParseVersion 은 "2.1.290 (Claude Code)" 같은 출력에서 첫 x.y.z 를 뽑는다.
func ParseVersion(s string) (Version, bool) {
	for _, f := range strings.Fields(s) {
		f = strings.TrimPrefix(f, "v")
		parts := strings.Split(f, ".")
		if len(parts) < 3 {
			continue
		}
		var n [3]int
		ok := true
		for i := 0; i < 3; i++ {
			p := parts[i]
			// 3번째 자리에 "-beta" 같은 꼬리가 붙어도 숫자 접두부만 읽는다.
			end := 0
			for end < len(p) && p[end] >= '0' && p[end] <= '9' {
				end++
			}
			if end == 0 {
				ok = false
				break
			}
			v, err := strconv.Atoi(p[:end])
			if err != nil {
				ok = false
				break
			}
			n[i] = v
		}
		if ok {
			return Version{n[0], n[1], n[2]}, true
		}
	}
	return Version{}, false
}
