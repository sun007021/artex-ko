package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ---------- ChatGPT 구독(Sign in with ChatGPT) OAuth 자격 증명 ----------
//
// llm_profile_oauth 는 AuthMode=chatgpt 프로필 하나당 최대 한 행이다. 토큰 필드는 모두
// json:"-" 이며 API 응답·로그로 나가면 안 된다. UI 에는 ProfileOAuthStatus(토큰 없음)만 준다.

// OAuth 연결 상태.
const (
	OAuthStatusDisconnected = "disconnected"    // 토큰 없음(로그아웃 또는 등록만 끝난 상태)
	OAuthStatusConnected    = "connected"       // 토큰 보유
	OAuthStatusReauthNeeded = "reauth_required" // 갱신 불가 — 다시 로그인해야 함

	oauthSettingHostIDKey  = "siwc_host_id"
	oauthLastErrorMaxRunes = 300
)

// ProfileOAuth 는 한 프로필의 OAuth 자격 증명 전체다(토큰 포함). 서버 내부에서만 쓴다.
type ProfileOAuth struct {
	ProfileID int64 `json:"-"`
	// ClientID 는 최초 등록 뒤 발급된 oaiapp_... 값이다. 비밀이 아니며 로그아웃 뒤에도
	// 남겨 같은 계정으로 재로그인할 때 재사용한다.
	ClientID string `json:"-"`
	Subject  string `json:"-"`
	Email    string `json:"-"`
	// IDToken 은 직전 로그인(또는 검증을 마친 갱신)의 ID 토큰이다. 재인가의 id_token_hint 용.
	IDToken      string    `json:"-"`
	AccessToken  string    `json:"-"`
	RefreshToken string    `json:"-"`
	ExpiresAt    time.Time `json:"-"` // 영값 = 없음
	// EarliestRefreshAt 은 토큰 응답의 earliest_refresh_at (영값 = 제한 없음).
	EarliestRefreshAt time.Time `json:"-"`
	Scopes            string    `json:"-"` // 공백으로 구분된 허가 스코프
	// PendingIDToken/PendingReceivedAt: 갱신 응답으로 받았지만 JWKS 장애 등으로 아직 서명을
	// 검증하지 못한 ID 토큰. 회전된 refresh_token 은 이미 소비됐으므로 새 토큰 세트를 먼저
	// 저장하고 검증은 나중에 마친다.
	PendingIDToken    string    `json:"-"`
	PendingReceivedAt time.Time `json:"-"`
	Status            string    `json:"-"`
	LastError         string    `json:"-"`
	SavedAt           time.Time `json:"-"`
}

// ProfileOAuthStatus 는 UI 에 내보내도 되는 요약이다(토큰·ID 토큰 없음).
type ProfileOAuthStatus struct {
	Status      string     `json:"status"` // "" | disconnected | connected | reauth_required
	Email       string     `json:"email,omitempty"`
	Registered  bool       `json:"registered"` // client_id 를 이미 발급받았는가
	Scopes      []string   `json:"scopes,omitempty"`
	PlanUsageOK bool       `json:"plan_usage"` // chatgpt.tokens.use.direct 가 허가됐는가
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

// Connected 는 지금 추론에 쓸 수 있는 연결인지(토큰 보유 + 상태 connected) 알려 준다.
func (o *ProfileOAuth) Connected() bool {
	return o != nil && o.Status == OAuthStatusConnected && o.RefreshToken != ""
}

const oauthCols = `profile_id,client_id,subject,email,id_token,access_token,refresh_token,expires_at,earliest_refresh_at,scopes,pending_id_token,pending_received_at,status,last_error,saved_at`

func scanProfileOAuth(sc interface{ Scan(...any) error }) (*ProfileOAuth, error) {
	var o ProfileOAuth
	var exp, earliest, pending sql.NullTime
	if err := sc.Scan(&o.ProfileID, &o.ClientID, &o.Subject, &o.Email, &o.IDToken, &o.AccessToken, &o.RefreshToken,
		&exp, &earliest, &o.Scopes, &o.PendingIDToken, &pending, &o.Status, &o.LastError, &o.SavedAt); err != nil {
		return nil, err
	}
	o.ExpiresAt, o.EarliestRefreshAt, o.PendingReceivedAt = exp.Time, earliest.Time, pending.Time
	return &o, nil
}

// nullTime 은 영값을 SQL NULL 로 바꾼다.
func nullTime(t time.Time) sql.NullTime { return sql.NullTime{Time: t, Valid: !t.IsZero()} }

// GetProfileOAuth 는 프로필의 OAuth 자격 증명을 돌려준다. 행이 없으면 (nil, nil).
func (d *DB) GetProfileOAuth(ctx context.Context, profileID int64) (*ProfileOAuth, error) {
	o, err := scanProfileOAuth(d.QueryRowContext(ctx, `SELECT `+oauthCols+` FROM llm_profile_oauth WHERE profile_id=$1`, profileID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return o, err
}

// SaveProfileOAuth 는 자격 증명 행 전체를 upsert 한다(토큰 회전·로그인 완료 때 사용).
func (d *DB) SaveProfileOAuth(ctx context.Context, o *ProfileOAuth) error {
	_, err := d.ExecContext(ctx, `INSERT INTO llm_profile_oauth(`+oauthCols+`)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,now())
ON CONFLICT (profile_id) DO UPDATE SET
  client_id=EXCLUDED.client_id, subject=EXCLUDED.subject, email=EXCLUDED.email, id_token=EXCLUDED.id_token,
  access_token=EXCLUDED.access_token, refresh_token=EXCLUDED.refresh_token, expires_at=EXCLUDED.expires_at,
  earliest_refresh_at=EXCLUDED.earliest_refresh_at, scopes=EXCLUDED.scopes, pending_id_token=EXCLUDED.pending_id_token,
  pending_received_at=EXCLUDED.pending_received_at, status=EXCLUDED.status, last_error=EXCLUDED.last_error, saved_at=now()`,
		o.ProfileID, o.ClientID, o.Subject, o.Email, o.IDToken, o.AccessToken, o.RefreshToken,
		nullTime(o.ExpiresAt), nullTime(o.EarliestRefreshAt), o.Scopes, o.PendingIDToken, nullTime(o.PendingReceivedAt),
		o.Status, truncateRunes(o.LastError, oauthLastErrorMaxRunes))
	return err
}

// SaveProfileOAuthRegistration 은 등록 직후 발급된 client_id 만 먼저 저장한다. 일회용 인가
// 코드 교환이 실패해도 발급된 클라이언트를 잃지 않아, 다음 로그인이 새 앱을 또 등록하지 않게
// 하기 위해서다. 이미 토큰이 있는 행의 client_id 는 바꾸지 않는다.
func (d *DB) SaveProfileOAuthRegistration(ctx context.Context, profileID int64, clientID string) error {
	_, err := d.ExecContext(ctx, `INSERT INTO llm_profile_oauth(profile_id,client_id,status,saved_at)
VALUES ($1,$2,'disconnected',now())
ON CONFLICT (profile_id) DO UPDATE SET client_id=EXCLUDED.client_id, saved_at=now()
WHERE llm_profile_oauth.refresh_token = ''`, profileID, clientID)
	return err
}

// ClearProfileOAuthTokens 는 로그아웃: 액세스/갱신/ID 토큰을 지우고 연결 상태를
// disconnected 로 되돌린다. client_id·계정 식별(sub/email)은 남겨 같은 계정으로 재로그인할 수
// 있게 한다. 행이 없으면 아무 일도 하지 않는다.
func (d *DB) ClearProfileOAuthTokens(ctx context.Context, profileID int64) error {
	_, err := d.ExecContext(ctx, `UPDATE llm_profile_oauth SET
  id_token='', access_token='', refresh_token='', expires_at=NULL, earliest_refresh_at=NULL, scopes='',
  pending_id_token='', pending_received_at=NULL, status='disconnected', last_error='', saved_at=now()
WHERE profile_id=$1`, profileID)
	return err
}

// DeleteProfileOAuth 는 프로필의 OAuth 행을 통째로 지운다(client_id 포함).
func (d *DB) DeleteProfileOAuth(ctx context.Context, profileID int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM llm_profile_oauth WHERE profile_id=$1`, profileID)
	return err
}

// ListProfileOAuthStatuses 는 모든 프로필의 연결 요약을 id 로 키잉해 돌려준다(토큰 미포함 SELECT).
func (d *DB) ListProfileOAuthStatuses(ctx context.Context) (map[int64]ProfileOAuthStatus, error) {
	rows, err := d.QueryContext(ctx, `SELECT profile_id,client_id<>'',email,scopes,expires_at,status,last_error,refresh_token<>'' FROM llm_profile_oauth`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]ProfileOAuthStatus{}
	for rows.Next() {
		var id int64
		var st ProfileOAuthStatus
		var scopes string
		var exp sql.NullTime
		var hasRefresh bool
		if err := rows.Scan(&id, &st.Registered, &st.Email, &scopes, &exp, &st.Status, &st.LastError, &hasRefresh); err != nil {
			return nil, err
		}
		if exp.Valid {
			t := exp.Time
			st.ExpiresAt = &t
		}
		st.Scopes = strings.Fields(scopes)
		st.PlanUsageOK = hasScope(st.Scopes, "chatgpt.tokens.use.direct")
		if st.Status == OAuthStatusConnected && !hasRefresh {
			st.Status = OAuthStatusDisconnected
		}
		out[id] = st
	}
	return out, rows.Err()
}

// EnsureLLMOAuthHostID 는 이 설치의 영속 호스트 식별자(ext_agent_host_id)를 돌려준다. 없으면
// gen 으로 만들어 저장한다. 호스트 ID 는 불투명 값이며 인증 자격 증명이 아니다. 동시에 두
// 요청이 만들어도 InsertSettingIfAbsent 덕에 하나만 남는다.
func (d *DB) EnsureLLMOAuthHostID(gen func() string) (string, error) {
	if v, ok, err := d.GetSetting(oauthSettingHostIDKey); err != nil {
		return "", err
	} else if ok && v != "" {
		return v, nil
	}
	if _, err := d.InsertSettingIfAbsent(oauthSettingHostIDKey, gen()); err != nil {
		return "", err
	}
	v, _, err := d.GetSetting(oauthSettingHostIDKey)
	return v, err
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
