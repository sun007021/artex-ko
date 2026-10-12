package server

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/claudecode"
	"github.com/Autumn-27/artex/db"
)

// claude-code 인증 방식(공식 claude CLI 를 사용자가 직접 로그인한 구독으로 구동)의 서버 쪽.
//
// ARTEX 는 Claude 자격 증명을 읽거나 저장하거나 중계하지 않는다. 여기서 하는 일은 (1) 설치 단위 옵트인과
// 동시 실행 상한의 저장·적용, (2) claude 의 로그인 상태 조회(`claude auth status`), (3) 프로필 → Config
// 변환이다. 로그인은 사용자가 터미널에서 직접 한다.

const (
	errClaudeCodeAckRequired = "개인 로컬 사용 고지를 확인해야 claude-code 를 켤 수 있습니다"
	errClaudeCodeDisabled    = "claude-code 인증 방식이 꺼져 있습니다. 먼저 LLM 설정에서 켜세요"
	errClaudeCodeNotLoopback = "이 ARTEX 가 loopback(127.0.0.1) 이외의 주소에서 접속 가능하게 실행 중이라 claude-code 를 켤 수 없습니다. 본인 PC 에서만 접근되도록 127.0.0.1 에 바인딩해 실행하세요"
	errClaudeCodeBadEffort   = "추론 강도는 low, medium, high, xhigh, max 중 하나여야 합니다"
	errClaudeCodeBadBinary   = "claude 실행 파일 경로가 올바르지 않습니다"

	reasonLLMProfileClaudeCodeOff = "LLM 설정의 claude-code 인증 방식이 꺼져 있습니다"

	msgClaudeCodeNotInstalled = "claude 실행 파일을 찾을 수 없습니다. Claude Code 를 설치하고 터미널에서 `claude auth login` 으로 로그인하세요"
	msgClaudeCodeTooOld       = "설치된 Claude Code 버전이 너무 낮습니다. `claude update` 로 갱신하세요"
	msgClaudeCodeNotLoggedIn  = "claude 가 로그인되어 있지 않습니다. 터미널에서 직접 `claude auth login` 을 실행해 본인 구독으로 로그인하세요"
	msgClaudeCodeAPIBilling   = "claude 가 구독이 아닌 인증 방식(API Key 등)을 쓰고 있어 API 과금 경로가 됩니다. 구독 로그인(claude.ai)만 허용됩니다"
	msgClaudeCodeUnknown      = "claude 상태를 확인하지 못했습니다. 터미널에서 `claude auth status` 를 직접 실행해 보세요"
)

// claudeCodeAllowRemoteEnv 는 loopback 이 아닌 주소로 듣는 배포(예: 호스트의 127.0.0.1 로만 포트를 게시한
// 컨테이너)에서 소유자가 직접 확인한 뒤 켤 수 있게 하는 환경변수다.
const claudeCodeAllowRemoteEnv = "ARTEX_CLAUDE_CODE_ALLOW_NONLOOPBACK"

var validClaudeEfforts = map[string]bool{"": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// claudeCodeWorkDir 는 claude 프로세스가 쓰는 전용 작업 디렉터리의 부모다.
func (s *Server) claudeCodeWorkDir() string {
	if s.m == nil || s.m.dir == "" {
		return ""
	}
	return filepath.Join(s.m.dir, "claude-code")
}

// listenLoopback 은 서버가 이 PC 의 loopback 에서만 접속 가능하게 실행 중인지 알려 준다. 주소를 모르면(테스트,
// 임베드) 제한하지 않는다.
func (s *Server) listenLoopback() bool {
	if s.listenAddr == "" || isLoopbackAddr(s.listenAddr) {
		return true
	}
	return os.Getenv(claudeCodeAllowRemoteEnv) == "1"
}

// applyClaudeCodeSettings 는 저장된 설정을 claudecode 패키지에 반영한다. loopback 이 아닌 주소로 듣고 있으면
// 저장 값과 무관하게 꺼 둔다.
func (s *Server) applyClaudeCodeSettings() {
	if s.m == nil || s.m.pg == nil {
		return
	}
	cs := s.m.pg.ClaudeCodeSettings()
	on := cs.Enabled
	if on && !s.listenLoopback() {
		log.Printf("[claude-code] 서버가 loopback 이 아닌 주소(%s)에서 접속 가능해 claude-code 를 켜지 않습니다. 본인 PC 에서만 접근되도록 127.0.0.1 로 바인딩하세요", s.listenAddr)
		on = false
	}
	claudecode.SetLogger(log.Printf)
	claudecode.SetMaxConcurrent(cs.MaxConcurrent)
	claudecode.SetEnabled(on)
	claudecode.InvalidateProbe()
	if on {
		// 이전 실행이 남긴 임시 작업 디렉터리를 비운다(프로세스는 서버 종료 때 정리된다).
		if wd := s.claudeCodeWorkDir(); wd != "" {
			_ = os.RemoveAll(wd)
		}
	}
}

type claudeCodeSettingsDTO struct {
	db.ClaudeCodeSettings
	EffectiveEnabled  bool   `json:"effective_enabled"` // 설정이 켜져 있고 loopback 조건도 충족
	MaxConcurrentHard int    `json:"max_concurrent_hard"`
	MinVersion        string `json:"min_version"`
	LoopbackOnly      bool   `json:"loopback_only"` // 서버가 loopback 에서만 듣는가(false 면 켤 수 없다)
}

func (s *Server) claudeCodeSettingsDTO() claudeCodeSettingsDTO {
	cs := s.m.pg.ClaudeCodeSettings()
	lb := s.listenLoopback()
	return claudeCodeSettingsDTO{
		ClaudeCodeSettings: cs, EffectiveEnabled: cs.Enabled && lb,
		MaxConcurrentHard: db.ClaudeCodeMaxConcurrentHard, MinVersion: claudecode.MinVersion.String(), LoopbackOnly: lb,
	}
}

// claudeCodeGetSettings: GET /api/llm/claude-code/settings
func (s *Server) claudeCodeGetSettings(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	writeJSON(w, 200, s.claudeCodeSettingsDTO())
}

// claudeCodePutSettings: PUT /api/llm/claude-code/settings
// 켜는 순간(꺼짐 → 켜짐)에는 acknowledged=true(개인 로컬 사용 고지 확인)가 필요하다.
func (s *Server) claudeCodePutSettings(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		Enabled       bool   `json:"enabled"`
		BinaryPath    string `json:"binary_path"`
		MaxConcurrent int    `json:"max_concurrent"`
		Acknowledged  bool   `json:"acknowledged"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	prev := pg.ClaudeCodeSettings()
	if body.Enabled && !prev.Enabled {
		if !body.Acknowledged {
			writeErr(w, 400, errClaudeCodeAckRequired)
			return
		}
		if !s.listenLoopback() {
			writeErr(w, 400, errClaudeCodeNotLoopback)
			return
		}
	}
	if err := db.ValidateClaudeCodeBinary(strings.TrimSpace(body.BinaryPath)); err != nil {
		writeErr(w, 400, errClaudeCodeBadBinary)
		return
	}
	if _, err := pg.SaveClaudeCodeSettings(db.ClaudeCodeSettings{
		Enabled: body.Enabled, BinaryPath: body.BinaryPath, MaxConcurrent: body.MaxConcurrent,
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.applyClaudeCodeSettings()
	// 켜짐/꺼짐과 바이너리 경로는 프로필이 만드는 provider 와 준비 상태를 바꾼다.
	s.invalidateProfileAgents()
	s.reapplyActiveProfile()
	writeJSON(w, 200, s.claudeCodeSettingsDTO())
}

// claudeCodeStatus: GET /api/llm/claude-code/status — `claude --version` 과 `claude auth status` 만 실행한다.
// 자격 증명은 읽지도 돌려주지도 않는다(설치 여부, 버전, 로그인 여부와 방식뿐).
func (s *Server) claudeCodeStatus(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	st := claudecode.Probe(ctx, pg.ClaudeCodeSettings().BinaryPath)
	writeJSON(w, 200, map[string]any{"status": st, "settings": s.claudeCodeSettingsDTO()})
}

// claudeCodeProblemMessage 는 점검 결과의 문제 코드를 사용자 문구(카탈로그 키)로 바꾼다.
func claudeCodeProblemMessage(problem string) string {
	switch problem {
	case claudecode.ProblemNotInstalled:
		return msgClaudeCodeNotInstalled
	case claudecode.ProblemTooOld:
		return msgClaudeCodeTooOld
	case claudecode.ProblemNotLoggedIn:
		return msgClaudeCodeNotLoggedIn
	case claudecode.ProblemAPIBilling:
		return msgClaudeCodeAPIBilling
	}
	return msgClaudeCodeUnknown
}

// claudeCodeConfig 는 claude-code 프로필로 agent.Config 를 만든다.
func (s *Server) claudeCodeConfig(model, effort, proxy string) agent.Config {
	bin := ""
	if s.m != nil && s.m.pg != nil {
		bin = s.m.pg.ClaudeCodeSettings().BinaryPath
	}
	return agent.NewClaudeCodeConfig(model, effort, proxy, bin, s.claudeCodeWorkDir())
}

// testClaudeCode: 연결 테스트(auth_mode=claude-code). 실제로 claude 를 한 번 구동해 짧은 응답을 받는다.
func (s *Server) testClaudeCode(w http.ResponseWriter, r *http.Request, profileID *int64, model, effort, proxy string) {
	fail := func(msg string) { writeJSON(w, 200, map[string]any{"ok": false, "error": trMsg(msg)}) }
	if s.m == nil || s.m.pg == nil {
		fail(errMgmtPGUnavailable)
		return
	}
	if !claudecode.Enabled() {
		fail(errClaudeCodeDisabled)
		return
	}
	if !validClaudeEfforts[effort] {
		fail(errClaudeCodeBadEffort)
		return
	}
	if profileID != nil {
		if p, err := s.m.pg.ProfileByID(*profileID); err == nil && p != nil && p.AuthMode == db.AuthModeClaudeCode {
			if strings.TrimSpace(model) == "" {
				model = p.Model
			}
			if strings.TrimSpace(proxy) == "" {
				proxy = p.Proxy
			}
		}
	}
	cfg := s.claudeCodeConfig(model, effort, proxy)
	if st := claudecode.Probe(r.Context(), cfg.ClaudeBinary); !st.Usable {
		fail(claudeCodeProblemMessage(st.Problem))
		return
	}
	lat, reply, err := agent.TestConnection(r.Context(), cfg)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "latency_ms": lat.Milliseconds(), "model": cfg.Model, "reply": truncateReply(reply)})
}

// normalizeClaudeCodeProfile 은 저장 직전 claude-code 프로필을 정리하고 오류 문구(없으면 "")를 돌려준다.
// API 경로 전용 필드(키, base_url, 출력 상한, 세션 헤더, 속도 제한, thinking 방식)는 이 경로에서 쓰지 않으므로 비운다.
// 모델과 추론 강도는 사용자가 정한 값을 그대로 --model / --effort 로 넘긴다.
func normalizeClaudeCodeProfile(p *db.LLMProfile) string {
	p.Model = strings.TrimSpace(p.Model)
	p.ReasoningEffort = strings.ToLower(strings.TrimSpace(p.ReasoningEffort))
	if !validClaudeEfforts[p.ReasoningEffort] {
		return errClaudeCodeBadEffort
	}
	p.Format, p.BaseURL, p.APIKey = "anthropic", "", ""
	p.MaxTokens, p.MaxTokensField, p.SessionHeaderKey, p.Streaming = 0, "", "", true
	p.RatePerSecond, p.RatePerMinute, p.ThinkingType = 0, 0, ""
	return ""
}
