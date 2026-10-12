package db

import "testing"

func claudeProfile(t *testing.T, d *DB, name string) int64 {
	t.Helper()
	id, err := d.SaveProfile(&LLMProfile{
		Name: name, Format: "openai", Model: "sonnet", AuthMode: AuthModeClaudeCode,
		BaseURL: "https://evil.example/v1", APIKey: "sk-should-be-dropped",
		ReasoningEffort: "xhigh", Streaming: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	return id
}

// claude-code 프로필은 저장 시 format=anthropic 으로 고정되고 base_url/api_key 가 지워지며,
// 모델·추론 강도는 그대로 남는다.
func TestClaudeCodeProfileNormalizedAndRoundTrip(t *testing.T) {
	d := openOAuthTestDB(t)
	id := claudeProfile(t, d, "t-cc-norm")
	p, err := d.ProfileByID(id)
	if err != nil || p == nil {
		t.Fatalf("ProfileByID: %v %v", err, p)
	}
	if p.AuthMode != AuthModeClaudeCode || p.Format != "anthropic" || p.BaseURL != "" || p.APIKey != "" || p.APIKeyHint != "" {
		t.Fatalf("not normalized: %+v", p)
	}
	if p.Model != "sonnet" || p.ReasoningEffort != "xhigh" {
		t.Fatalf("model/effort lost: %+v", p)
	}
}

// API Key 프로필을 claude-code 로 바꾸면(갱신, api_key 비움) 예전 키와 힌트가 지워진다.
func TestClaudeCodeSwitchDropsOldKey(t *testing.T) {
	d := openOAuthTestDB(t)
	id, err := d.SaveProfile(&LLMProfile{Name: "t-cc-switch", Format: "anthropic", Model: "m", APIKey: "sk-ant-secret-1234", Streaming: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	if _, err := d.SaveProfile(&LLMProfile{ID: id, Name: "t-cc-switch", Format: "anthropic", Model: "m", AuthMode: AuthModeClaudeCode, Streaming: true}); err != nil {
		t.Fatal(err)
	}
	p, _ := d.ProfileByID(id)
	if p.APIKey != "" || p.APIKeyHint != "" || p.AuthMode != AuthModeClaudeCode {
		t.Fatalf("old key survived: %+v", p)
	}
}

func TestUnknownAuthModeRejected(t *testing.T) {
	d := openOAuthTestDB(t)
	if _, err := d.SaveProfile(&LLMProfile{Name: "t-cc-bad", Format: "anthropic", Model: "m", AuthMode: "claude"}); err == nil {
		t.Fatal("unknown auth_mode must be rejected")
	}
}

// DB CHECK 가 최종 방어선이다: claude-code 인데 format 이 anthropic 이 아닌 행은 직접 넣어도 거부된다.
func TestClaudeCodeFormatCheckConstraint(t *testing.T) {
	d := openOAuthTestDB(t)
	_, err := d.Exec(`INSERT INTO llm_profiles(name,format,model,auth_mode) VALUES ('t-cc-check','openai','m','claude-code')`)
	if err == nil {
		d.Exec(`DELETE FROM llm_profiles WHERE name='t-cc-check'`)
		t.Fatal("CHECK must reject claude-code with a non-anthropic format")
	}
}

// claude-code 프로필은 자격 증명이 DB 에 없어도 풀 후보다(실제 가용성은 호출 시점에 드러난다).
func TestClaudeCodeProfileInPool(t *testing.T) {
	d := openOAuthTestDB(t)
	id := claudeProfile(t, d, "t-cc-pool")
	ps, err := d.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.ID == id {
			return
		}
	}
	t.Fatal("claude-code profile missing from pool")
}

func TestClaudeCodeSettingsDefaultsAndClamp(t *testing.T) {
	d := openOAuthTestDB(t)
	t.Cleanup(func() {
		d.Exec(`DELETE FROM settings WHERE key IN ('claude_code_enabled','claude_code_bin','claude_code_max_concurrent')`)
	})
	d.Exec(`DELETE FROM settings WHERE key IN ('claude_code_enabled','claude_code_bin','claude_code_max_concurrent')`)
	s := d.ClaudeCodeSettings()
	if s.Enabled || s.BinaryPath != "" || s.MaxConcurrent != ClaudeCodeDefaultConcurrent {
		t.Fatalf("defaults wrong (must be opt-in, off): %+v", s)
	}
	got, err := d.SaveClaudeCodeSettings(ClaudeCodeSettings{Enabled: true, BinaryPath: " /opt/claude ", MaxConcurrent: 99})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxConcurrent != ClaudeCodeMaxConcurrentHard || got.BinaryPath != "/opt/claude" {
		t.Fatalf("not clamped/trimmed: %+v", got)
	}
	if r := d.ClaudeCodeSettings(); r != got {
		t.Fatalf("round trip: %+v != %+v", r, got)
	}
	if _, err := d.SaveClaudeCodeSettings(ClaudeCodeSettings{BinaryPath: "a\nb"}); err == nil {
		t.Fatal("newline in binary path must be rejected")
	}
	if ClampClaudeCodeConcurrent(0) != 1 || ClampClaudeCodeConcurrent(-5) != 1 || ClampClaudeCodeConcurrent(2) != 2 {
		t.Fatal("clamp")
	}
}
