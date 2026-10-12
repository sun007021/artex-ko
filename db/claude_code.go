package db

import (
	"fmt"
	"strconv"
	"strings"
)

// claude-code 인증 방식(공식 claude CLI 헤드리스 구동)의 설치 단위 설정. 프로필별 값(모델·추론
// 강도)은 llm_profiles 의 Model/ReasoningEffort 를 그대로 쓰고, 여기에는 설치 전체에 걸리는
// 값만 둔다. 자격 증명은 어디에도 저장하지 않는다 — 로그인은 claude 바이너리 안에서만 일어난다.
const (
	settingClaudeCodeEnabled       = "claude_code_enabled"
	settingClaudeCodeBinary        = "claude_code_bin"
	settingClaudeCodeMaxConcurrent = "claude_code_max_concurrent"

	// ClaudeCodeDefaultConcurrent 는 동시에 모델 응답을 만드는 claude 프로세스 수의 기본 상한,
	// ClaudeCodeMaxConcurrentHard 는 설정으로도 넘을 수 없는 하드 상한이다. 구독 한도는
	// "ordinary, individual usage" 를 전제로 하므로 작게 묶는다.
	ClaudeCodeDefaultConcurrent = 1
	ClaudeCodeMaxConcurrentHard = 3
)

// ClaudeCodeSettings 는 claude-code 인증 방식의 설치 단위 설정이다.
type ClaudeCodeSettings struct {
	// Enabled 는 사용자가 "개인 로컬 사용" 고지를 확인하고 기능을 켰는지 여부다. 기본 false(옵트인).
	// false 이면 claude-code 프로필을 저장·실행할 수 없다.
	Enabled bool `json:"enabled"`
	// BinaryPath 는 claude 실행 파일 경로. 비우면 PATH 에서 "claude" 를 찾는다.
	BinaryPath string `json:"binary_path"`
	// MaxConcurrent 는 동시에 모델 응답을 만드는 claude 프로세스 수 상한(1..ClaudeCodeMaxConcurrentHard).
	MaxConcurrent int `json:"max_concurrent"`
}

// ClampClaudeCodeConcurrent 는 동시 실행 상한을 [1, 하드 상한] 으로 맞춘다(0 이하 = 기본값).
func ClampClaudeCodeConcurrent(n int) int {
	if n <= 0 {
		return ClaudeCodeDefaultConcurrent
	}
	if n > ClaudeCodeMaxConcurrentHard {
		return ClaudeCodeMaxConcurrentHard
	}
	return n
}

// ValidateClaudeCodeBinary 는 바이너리 경로 설정을 검사한다. 빈 값(= PATH 의 "claude")이거나,
// 줄바꿈·NUL 이 없는 값이어야 한다. 셸을 거치지 않고 exec 로만 실행되므로 인용은 필요 없다.
func ValidateClaudeCodeBinary(path string) error {
	if strings.ContainsAny(path, "\x00\r\n") {
		return fmt.Errorf("invalid claude binary path")
	}
	if len(path) > 1024 {
		return fmt.Errorf("claude binary path too long")
	}
	return nil
}

// ClaudeCodeSettings 는 저장된 설정을 읽는다. 없는 키는 기본값(꺼짐 / PATH / 동시 1)이다.
func (d *DB) ClaudeCodeSettings() ClaudeCodeSettings {
	s := ClaudeCodeSettings{MaxConcurrent: ClaudeCodeDefaultConcurrent}
	s.Enabled = d.GetBool(settingClaudeCodeEnabled, false)
	if v, ok, err := d.GetSetting(settingClaudeCodeBinary); err == nil && ok {
		s.BinaryPath = strings.TrimSpace(v)
	}
	if v, ok, err := d.GetSetting(settingClaudeCodeMaxConcurrent); err == nil && ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			s.MaxConcurrent = ClampClaudeCodeConcurrent(n)
		}
	}
	return s
}

// SaveClaudeCodeSettings 는 설정을 저장한다. 동시 실행 상한은 저장 전에 맞춘다.
func (d *DB) SaveClaudeCodeSettings(s ClaudeCodeSettings) (ClaudeCodeSettings, error) {
	s.BinaryPath = strings.TrimSpace(s.BinaryPath)
	if err := ValidateClaudeCodeBinary(s.BinaryPath); err != nil {
		return s, err
	}
	s.MaxConcurrent = ClampClaudeCodeConcurrent(s.MaxConcurrent)
	if err := d.SetBool(settingClaudeCodeEnabled, s.Enabled); err != nil {
		return s, err
	}
	if err := d.SetSetting(settingClaudeCodeBinary, s.BinaryPath); err != nil {
		return s, err
	}
	if err := d.SetSetting(settingClaudeCodeMaxConcurrent, strconv.Itoa(s.MaxConcurrent)); err != nil {
		return s, err
	}
	return s, nil
}
