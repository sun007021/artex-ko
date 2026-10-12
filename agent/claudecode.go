package agent

import (
	"errors"
	"strings"

	"github.com/Autumn-27/artex/claudecode"
	"github.com/Autumn-27/artex/llmauth"
	"github.com/Autumn-27/norma/llm"
)

// Claude 구독(claude-code) 인증 방식.
//
// 에이전트 루프(agentcore)는 그대로이고, 모델 한 턴만 사용자가 로그인해 둔 공식 claude CLI 가 만든다
// (claudecode.Provider). 권한·훅·전사·압축·발견 기록·트래픽 캡처는 API 경로와 같은 코드가 처리한다.
// ARTEX 는 Claude 자격 증명을 읽거나 저장하지 않는다 — 로그인은 claude 가 한다.

// AuthModeClaudeCode 는 db.AuthModeClaudeCode 와 같은 값이다(둘 다 llmauth.ModeClaudeCode 의 별칭).
const AuthModeClaudeCode = llmauth.ModeClaudeCode

// UsesClaudeCode 는 이 Config 가 claude CLI(구독) 경로를 쓰는지 알려 준다.
func (c Config) UsesClaudeCode() bool { return c.AuthMode == AuthModeClaudeCode }

// NewClaudeCodeConfig 는 claude-code 프로필용 Config 를 만든다. 모델과 추론 강도는 프로필 값을 그대로
// --model / --effort 로 넘기며(비면 claude 의 기본값), 모델 식별자를 코드에 두지 않는다.
// 스트리밍은 항상 켠다(부분 메시지 스트림이 이 경로의 유일한 출력 형태다). mgr 는 서버가 쥔
// claudecode.Manager(동시 실행 슬롯·한도 게이트·세션 표)이며 Config 의 == 비교를 위해 포인터로 둔다.
func NewClaudeCodeConfig(model, effort, proxy, binary, workDir string, mgr *claudecode.Manager) Config {
	return Config{
		Format: llm.FormatAnthropic, Model: strings.TrimSpace(model), ReasoningEffort: strings.TrimSpace(effort),
		Proxy: strings.TrimSpace(proxy), Stream: true, AuthMode: AuthModeClaudeCode,
		ClaudeBinary: strings.TrimSpace(binary), ClaudeWorkDir: strings.TrimSpace(workDir), ClaudeManager: mgr,
	}
}

func (c Config) newClaudeCodeProvider() (llm.Provider, error) {
	if c.ClaudeManager == nil {
		return nil, errors.New("claude-code: manager is not configured")
	}
	return c.ClaudeManager.NewProvider(claudecode.Options{
		Binary: c.ClaudeBinary, Model: c.Model, Effort: c.ReasoningEffort, Proxy: c.Proxy, WorkDir: c.ClaudeWorkDir,
	}), nil
}
