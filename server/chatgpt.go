package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/claudecode"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/siwc"
)

// ChatGPT 구독(Sign in with ChatGPT) 로그인 API.
//
//	POST /api/llm/chatgpt/login/start     로그인 시작 → 브라우저에서 열 인가 URL
//	GET  /auth/start                      재인가 URL 로 브라우저를 보냄(무인증, state 로 보호; ID 토큰 힌트를 UI 에 안 싣기 위함)
//	GET  /auth/callback                   OpenAI 가 127.0.0.1 로 되돌려 보내는 리디렉션(무인증, state 로 보호)
//	POST /api/llm/chatgpt/login/complete  리디렉션된 URL 을 붙여 넣어 마무리(다른 호스트·Docker 폴백)
//	GET  /api/llm/chatgpt/status          연결 상태(토큰 없음)
//	POST /api/llm/chatgpt/logout          refresh token 폐기 + 로컬 토큰 삭제
//	GET  /api/llm/chatgpt/models          계정이 쓸 수 있는 모델 목록
//
// /api/* 는 다른 관리자 API 와 같이 JWT 로 보호된다. /auth/callback 만 예외인데, 외부(OpenAI)가
// 브라우저를 통해 부르는 주소라 세션 쿠키가 없을 수 있기 때문이다. 대신 로그인 시작 때 만든
// 256비트 무작위 state 가 없는 요청은 아무 것도 하지 못하고(일회용, 10분 만료, 추측 불가),
// 응답 페이지에는 코드·토큰이 담기지 않는다.
//
// 리디렉션 URI 는 http://127.0.0.1:<포트>/auth/callback 로 고정된다(공식 규칙: 호스트는 127.0.0.1,
// 이후 로그인에서는 포트만 바뀔 수 있음). 포트는 브라우저가 이 서버에 접속한 포트(예: 8787,
// Docker 포트 매핑 포함)를 따른다. 브라우저가 서버와 같은 호스트에 있으면 콜백이 이 서버로 바로
// 돌아오고, 그렇지 않으면 사용자가 마지막 주소창 URL 을 붙여 넣는다.

const (
	chatgptPendingTTL  = 10 * time.Minute
	chatgptMaxPending  = 32
	chatgptDefaultPort = 8787
	chatgptStepTimeout = 45 * time.Second
	// siwc 에 알리는 앱 이름. 모든 설치에서 같아야 한다(표시용 메타데이터일 뿐 신원이 아니다).
	chatgptAppName = "ARTEX"
)

type chatgptLogin struct {
	profileID int64
	p         *siwc.Pending
	expires   time.Time
	// authURL 은 id_token_hint 를 담은 재인가 URL 일 때만 채운다. 이 값은 UI 로 보내지 않고
	// GET /auth/start 가 브라우저를 302 로 보낼 때만 쓴다(ID 토큰이 화면 스크립트에 노출되지 않게).
	authURL string
}

type chatgptLoginResult struct {
	State string // "done" | "error"
	Code  string
	At    time.Time
}

type chatgptSessionEntry struct {
	proxy string
	sess  *siwc.Session
}

// chatgptRuntime 은 Server 안의 구독 로그인 상태다(제로 값으로 쓸 수 있다).
type chatgptRuntime struct {
	mu       sync.Mutex
	clients  map[string]*siwc.Client // proxy → client (discovery/JWKS 캐시 공유)
	sessions map[int64]*chatgptSessionEntry
	pending  map[string]*chatgptLogin // state → 진행 중 로그인
	results  map[int64]chatgptLoginResult

	// 테스트 훅
	issuer      string
	fetchModels func(ctx context.Context, proxy string, src agent.TokenSource) ([]agent.ChatGPTModel, error)
}

// SetListenAddr 는 서버가 듣는 주소를 알려 준다. 브라우저가 loopback 이 아닌 이름으로 접속해
// 포트를 알 수 없을 때 리디렉션 포트의 기본값으로 쓴다.
func (s *Server) SetListenAddr(addr string) {
	s.listenAddr = addr
	s.applyClaudeCodeSettings() // 바인딩 주소를 알았으니 claude-code 의 loopback 조건을 다시 판정한다
	if _, port, err := net.SplitHostPort(addr); err == nil {
		if n, err := strconv.Atoi(port); err == nil && n > 0 && n < 65536 {
			s.listenPort = n
		}
	}
}

// ---- 저장소 어댑터 ----

// profileOAuthStore 는 siwc.Store 를 llm_profile_oauth 행에 연결한다.
type profileOAuthStore struct {
	pg *db.DB
	id int64
}

func (st profileOAuthStore) Load(ctx context.Context) (*siwc.Record, error) {
	o, err := st.pg.GetProfileOAuth(ctx, st.id)
	if err != nil || o == nil {
		return nil, err
	}
	return &siwc.Record{
		ClientID: o.ClientID, Subject: o.Subject, Email: o.Email, IDToken: o.IDToken,
		AccessToken: o.AccessToken, RefreshToken: o.RefreshToken, ExpiresAt: o.ExpiresAt,
		EarliestRefreshAt: o.EarliestRefreshAt, Scopes: strings.Fields(o.Scopes),
		PendingIDToken: o.PendingIDToken, PendingReceivedAt: o.PendingReceivedAt,
		Status: o.Status, LastError: o.LastError,
	}, nil
}

func (st profileOAuthStore) Save(ctx context.Context, r *siwc.Record) error {
	return st.pg.SaveProfileOAuth(ctx, &db.ProfileOAuth{
		ProfileID: st.id, ClientID: r.ClientID, Subject: r.Subject, Email: r.Email, IDToken: r.IDToken,
		AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, ExpiresAt: r.ExpiresAt,
		EarliestRefreshAt: r.EarliestRefreshAt, Scopes: strings.Join(r.Scopes, " "),
		PendingIDToken: r.PendingIDToken, PendingReceivedAt: r.PendingReceivedAt,
		Status: r.Status, LastError: r.LastError,
	})
}

// ---- 세션/클라이언트 ----

func (rt *chatgptRuntime) clientLocked(proxy string, logf func(string, ...any)) *siwc.Client {
	if c := rt.clients[proxy]; c != nil {
		return c
	}
	hc, err := agent.ProxyHTTPClient(proxy, 30*time.Second)
	if err != nil {
		log.Printf("[chatgpt] 프록시 설정을 쓸 수 없어 직접 연결로 인증 서버에 접속합니다: %v", err)
		hc = nil
	}
	c := siwc.New(siwc.Config{AppName: chatgptAppName, Issuer: rt.issuer, HTTPClient: hc, Logf: logf})
	if rt.clients == nil {
		rt.clients = map[string]*siwc.Client{}
	}
	rt.clients[proxy] = c
	return c
}

// chatgptSession 은 프로필의 토큰 세션을 돌려준다. 같은 프로필·프록시면 항상 같은 포인터라서
// agent.Config 의 == 비교(캐시된 구성 판별)가 유지된다.
func (s *Server) chatgptSession(p *db.LLMProfile) *siwc.Session {
	rt := &s.cgpt
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if e := rt.sessions[p.ID]; e != nil && e.proxy == p.Proxy {
		return e.sess
	}
	client := rt.clientLocked(p.Proxy, log.Printf)
	sess := client.NewSession(profileOAuthStore{pg: s.m.pg, id: p.ID})
	if rt.sessions == nil {
		rt.sessions = map[int64]*chatgptSessionEntry{}
	}
	rt.sessions[p.ID] = &chatgptSessionEntry{proxy: p.Proxy, sess: sess}
	return sess
}

func (rt *chatgptRuntime) forget(profileID int64) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.sessions, profileID)
	delete(rt.results, profileID)
	for st, pl := range rt.pending {
		if pl.profileID == profileID {
			delete(rt.pending, st)
		}
	}
}

// chatgptReady 는 세션이 지금 추론에 쓸 수 있는 상태(로그인됨 + 플랜 사용 권한 허가)인지 본다.
func chatgptReady(sess *siwc.Session) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec, err := sess.Snapshot(ctx)
	return err == nil && rec.Connected() && rec.HasPlanUsage()
}

// profileAgentConfig 는 프로필(API Key 또는 ChatGPT 구독)로 agent.Config 를 만든다.
// ready=false 는 지금 요청을 처리할 수 없다는 뜻이다(키 없음 / 로그인 안 됨).
func (s *Server) profileAgentConfig(p *db.LLMProfile) (cfg agent.Config, ready bool) {
	if p.AuthMode == db.AuthModeChatGPT {
		cfg = agent.NewChatGPTConfig(p.Model, p.Proxy, s.chatgptSession(p))
		ready = chatgptReady(s.chatgptSession(p))
		// 구독 경로: 스트리밍 고정, 출력 상한(max_output_tokens)과 세션 헤더는 지원되지 않아
		// 보내지 않는다(MaxTokens 는 0 = 상한 없음으로 둔다).
		cfg.Stream = true
	} else if p.AuthMode == db.AuthModeClaudeCode {
		// claude CLI(구독) 경로: 모델·추론 강도는 프로필 값 그대로 --model/--effort 로 간다. 켜져 있어야 준비됨.
		cfg = s.claudeCodeConfig(p.Model, p.ReasoningEffort, p.Proxy)
		ready = claudecode.Enabled()
	} else {
		cfg = agent.ConfigFrom(p.Format, p.Model, p.BaseURL, p.APIKey, p.Proxy)
		ready = cfg.APIKey != ""
		cfg.Stream = p.Streaming
		cfg.MaxTokens, cfg.MaxTokensField = p.MaxTokens, p.MaxTokensField
		cfg.SessionHeaderKey = p.SessionHeaderKey
	}
	cfg.RatePerSecond, cfg.RatePerMinute = p.RatePerSecond, p.RatePerMinute
	cfg.ContextWindowK = p.ContextWindowK
	cfg.ThinkingType = p.ThinkingType
	cfg.ReasoningEffort = p.ReasoningEffort
	s.applyProfileRetry(&cfg, p)
	return cfg, ready
}

// afterChatGPTChange 는 로그인/로그아웃 뒤 캐시된 provider·에이전트를 다시 만든다.
func (s *Server) afterChatGPTChange() {
	if s.afterAuthChange != nil {
		s.afterAuthChange()
		return
	}
	s.invalidateProfileAgents()
	s.reapplyActiveProfile()
}

// ---- 오류 문구 ----

// 사용자에게 보이는 한국어 원문. 영어·중국어·스페인어는 errmsg_catalog_chatgpt.go 에 있다.
const (
	errChatGPTInvalidState    = "로그인 응답의 state 가 일치하지 않습니다. 로그인을 처음부터 다시 시작해 주세요"
	errChatGPTDenied          = "ChatGPT 로그인이 취소되었거나 거부되었습니다"
	errChatGPTRegistration    = "ChatGPT 가 앱 등록을 마치지 않았습니다. 다시 로그인해 주세요"
	errChatGPTInvalidGrant    = "인가 코드가 만료되었거나 이미 사용되었습니다. 로그인을 다시 시작해 주세요"
	errChatGPTInvalidIDToken  = "ChatGPT 계정 신원을 검증하지 못했습니다. 다시 로그인해 주세요"
	errChatGPTIdentityDown    = "ChatGPT 신원 검증 서버에 일시적으로 연결할 수 없습니다. 잠시 후 다시 시도해 주세요"
	errChatGPTAccountMismatch = "이전과 다른 ChatGPT 계정으로 로그인되었습니다. 원래 계정으로 로그인하거나 새 프로필을 만드세요"
	errChatGPTLoginRequired   = "ChatGPT 로그인이 필요합니다. 다시 로그인해 주세요"
	errChatGPTPlanDisabled    = "ChatGPT 플랜 사용 권한이 허가되지 않았습니다. 다시 로그인해 권한을 허용해 주세요"
	errChatGPTNetwork         = "ChatGPT 서버에 연결할 수 없습니다. 네트워크와 프록시 설정을 확인해 주세요"
	errChatGPTNotEligible     = "이 ChatGPT 계정 또는 워크스페이스에서는 플랜 사용량 공유를 쓸 수 없습니다"
	errChatGPTGeneric         = "ChatGPT 요청이 실패했습니다"
	errChatGPTNoProfile       = "ChatGPT 구독 방식의 LLM 설정을 찾을 수 없습니다"
	errChatGPTNoPending       = "진행 중인 ChatGPT 로그인이 없습니다. 로그인을 다시 시작해 주세요"
	errChatGPTBadCallback     = "붙여 넣은 주소에서 로그인 정보를 읽을 수 없습니다"
	errChatGPTTooManyLogins   = "진행 중인 로그인이 너무 많습니다. 잠시 후 다시 시도해 주세요"
)

func chatgptErrCodeMessage(err error) (code, msg string) {
	code = siwc.CodeOf(err)
	switch code {
	case siwc.CodeInvalidState:
		return code, errChatGPTInvalidState
	case siwc.CodeAccessDenied:
		return code, errChatGPTDenied
	case siwc.CodeRegistrationIncomplete:
		return code, errChatGPTRegistration
	case "invalid_grant":
		return code, errChatGPTInvalidGrant
	case siwc.CodeInvalidIDToken:
		return code, errChatGPTInvalidIDToken
	case siwc.CodeIdentityUnavailable:
		return code, errChatGPTIdentityDown
	case siwc.CodeAccountMismatch:
		return code, errChatGPTAccountMismatch
	case siwc.CodeReauthRequired, siwc.CodeNotConnected:
		return code, errChatGPTLoginRequired
	case siwc.CodePlanUsageDisabled:
		return code, errChatGPTPlanDisabled
	case siwc.CodeNetwork, siwc.CodeDiscoveryFailed, siwc.CodeRefreshNotReady:
		return code, errChatGPTNetwork
	case "subscription_sharing_user_not_eligible":
		return code, errChatGPTNotEligible
	case "":
		if errors.Is(err, context.DeadlineExceeded) {
			return "timeout", errChatGPTNetwork
		}
		return "error", errChatGPTGeneric
	}
	return code, errChatGPTGeneric + " (" + code + ")"
}

// trChatGPTMsg 는 chatgptErrCodeMessage 의 문구를 활성 언어로 옮긴다. 코드가 덧붙은
// 일반 실패 문구("... (code)")는 접두부만 번역하고 코드는 그대로 둔다.
func trChatGPTMsg(msg string) string {
	if rest, ok := strings.CutPrefix(msg, errChatGPTGeneric+" ("); ok {
		return trMsg(errChatGPTGeneric) + " (" + rest
	}
	return trMsg(msg)
}

func writeChatGPTErr(w http.ResponseWriter, status int, err error) {
	code, msg := chatgptErrCodeMessage(err)
	writeJSON(w, status, map[string]any{"error": trChatGPTMsg(msg), "code": code})
}

// ---- 핸들러 ----

// chatgptProfile 은 profile_id 의 ChatGPT 구독 프로필을 불러온다. 실패하면 응답을 쓰고 nil.
func (s *Server) chatgptProfile(w http.ResponseWriter, id int64) *db.LLMProfile {
	pg := s.pg(w)
	if pg == nil {
		return nil
	}
	p, err := pg.ProfileByID(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return nil
	}
	if p == nil || p.AuthMode != db.AuthModeChatGPT {
		writeErr(w, 404, errChatGPTNoProfile)
		return nil
	}
	return p
}

func isLoopbackHostName(h string) bool {
	h = strings.ToLower(strings.Trim(h, "[]"))
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// chatgptRedirectPort 는 리디렉션 URI 의 포트를 고른다: 명시값 > 브라우저가 이 서버에 접속한
// 포트(loopback 주소로 접속한 경우) > 서버의 리슨 포트 > 8787.
func (s *Server) chatgptRedirectPort(r *http.Request, explicit int) int {
	if explicit > 0 && explicit < 65536 {
		return explicit
	}
	if host, port, err := net.SplitHostPort(r.Host); err == nil && isLoopbackHostName(host) {
		if n, err := strconv.Atoi(port); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	if s.listenPort > 0 {
		return s.listenPort
	}
	return chatgptDefaultPort
}

func (s *Server) chatgptLoginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProfileID    int64 `json:"profile_id"`
		Reconsent    bool  `json:"reconsent"`
		RedirectPort int   `json:"redirect_port"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p := s.chatgptProfile(w, req.ProfileID)
	if p == nil {
		return
	}
	hostID, err := s.m.pg.EnsureLLMOAuthHostID(siwc.NewHostID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	sess := s.chatgptSession(p)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rec, err := sess.Snapshot(ctx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	redirectURI := siwc.RedirectURIForPort(s.chatgptRedirectPort(r, req.RedirectPort))
	opts := siwc.BeginOptions{RedirectURI: redirectURI, HostID: hostID, Reconsent: req.Reconsent}
	if rec != nil && rec.ClientID != "" {
		// 재인가: 저장된 클라이언트와 보관한 ID 토큰/이메일 힌트를 쓴다(계정 선택 화면 생략).
		opts.ClientID, opts.IDTokenHint, opts.LoginHint = rec.ClientID, rec.IDToken, rec.Email
	}
	rt := &s.cgpt
	rt.mu.Lock()
	client := rt.clientLocked(p.Proxy, log.Printf)
	rt.mu.Unlock()
	authURL, pending, err := client.BeginLogin(opts)
	if err != nil {
		writeChatGPTErr(w, 400, err)
		return
	}
	rt.mu.Lock()
	now := time.Now()
	if rt.pending == nil {
		rt.pending = map[string]*chatgptLogin{}
	}
	for st, pl := range rt.pending { // 만료분과 같은 프로필의 이전 시도를 정리
		if now.After(pl.expires) || pl.profileID == p.ID {
			delete(rt.pending, st)
		}
	}
	if len(rt.pending) >= chatgptMaxPending {
		rt.mu.Unlock()
		writeErr(w, 429, errChatGPTTooManyLogins)
		return
	}
	login := &chatgptLogin{profileID: p.ID, p: pending, expires: now.Add(chatgptPendingTTL)}
	openURL := authURL
	if opts.IDTokenHint != "" {
		// 재인가 URL 에는 보관한 ID 토큰(id_token_hint)이 들어간다. UI 응답에는 싣지 않고,
		// 같은 서버의 /auth/start 가 브라우저를 그 주소로 보내게 한다.
		login.authURL = authURL
		openURL = "/auth/start?state=" + url.QueryEscape(pending.State)
	}
	rt.pending[pending.State] = login
	delete(rt.results, p.ID)
	rt.mu.Unlock()
	// 인가 URL 은 id_token_hint 를 담을 수 있으므로 로그에 남기지 않는다.
	log.Printf("[chatgpt] 로그인 시작 profile=%d redirect=%s reauth=%v", p.ID, redirectURI, opts.ClientID != "")
	writeJSON(w, 200, map[string]any{
		"auth_url":     openURL,
		"redirect_uri": redirectURI,
		"expires_in":   int(chatgptPendingTTL.Seconds()),
	})
}

// findLogin 은 state 로 진행 중 로그인을 찾는다(만료분은 정리). consume=true 면 제거한다.
func (rt *chatgptRuntime) findLogin(state string, consume bool) *chatgptLogin {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	pl := rt.pending[state]
	if pl == nil {
		return nil
	}
	if time.Now().After(pl.expires) {
		delete(rt.pending, state)
		return nil
	}
	if consume {
		delete(rt.pending, state)
	}
	return pl
}

func (rt *chatgptRuntime) loginForProfile(profileID int64) *chatgptLogin {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var best *chatgptLogin
	for st, pl := range rt.pending {
		if time.Now().After(pl.expires) {
			delete(rt.pending, st)
			continue
		}
		if pl.profileID == profileID && (best == nil || pl.expires.After(best.expires)) {
			best = pl
		}
	}
	return best
}

func (rt *chatgptRuntime) setResult(profileID int64, res chatgptLoginResult) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.results == nil {
		rt.results = map[int64]chatgptLoginResult{}
	}
	rt.results[profileID] = res
}

// finishChatGPTLogin 은 콜백 값(raw)을 검증하고 코드를 교환해 자격 증명을 저장한다.
// state 불일치는 대기 중인 로그인을 소모하지 않는다(엉뚱한 요청이 진행 중인 로그인을 죽이지 못하게).
// 그 밖에는 인가 코드가 일회용이라 성공이든 실패든 시도가 끝난다.
func (s *Server) finishChatGPTLogin(ctx context.Context, pl *chatgptLogin, raw string) error {
	rt := &s.cgpt
	p, err := s.m.pg.ProfileByID(pl.profileID)
	if err != nil {
		return err
	}
	if p == nil || p.AuthMode != db.AuthModeChatGPT {
		rt.findLogin(pl.p.State, true)
		return errors.New("profile is not a chatgpt profile")
	}
	rt.mu.Lock()
	client := rt.clientLocked(p.Proxy, log.Printf)
	rt.mu.Unlock()

	cb, err := client.ParseCallback(pl.p, raw)
	if err != nil {
		if siwc.CodeOf(err) != siwc.CodeInvalidState {
			rt.findLogin(pl.p.State, true)
			rt.setResult(pl.profileID, chatgptLoginResult{State: "error", Code: siwc.CodeOf(err), At: time.Now()})
		}
		return err
	}
	if rt.findLogin(pl.p.State, true) == nil { // 동시에 다른 경로(콜백/붙여넣기)가 먼저 가져갔다
		return siwc.ErrNotConnected
	}
	fail := func(err error) error {
		rt.setResult(pl.profileID, chatgptLoginResult{State: "error", Code: siwc.CodeOf(err), At: time.Now()})
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, chatgptStepTimeout)
	defer cancel()
	sess := s.chatgptSession(p)
	prev, err := sess.Snapshot(ctx)
	if err != nil {
		return fail(err)
	}
	// 코드 교환이 실패해도 발급된 클라이언트는 잃지 않는다 — 다음 로그인이 새 앱을 또 등록하지 않게.
	if prev == nil || prev.ClientID != cb.ClientID {
		if err := s.m.pg.SaveProfileOAuthRegistration(ctx, p.ID, cb.ClientID); err != nil {
			return fail(err)
		}
		sess.Reload()
		if prev, err = sess.Snapshot(ctx); err != nil {
			return fail(err)
		}
	}
	rec, err := client.CompleteLogin(ctx, pl.p, cb, prev)
	if err != nil {
		return fail(err)
	}
	if err := sess.Replace(ctx, rec); err != nil {
		return fail(err)
	}
	rt.setResult(pl.profileID, chatgptLoginResult{State: "done", At: time.Now()})
	log.Printf("[chatgpt] 로그인 완료 profile=%d plan_usage=%v", p.ID, rec.HasPlanUsage())
	s.afterChatGPTChange()
	return nil
}

// 콜백 페이지는 코드·토큰·State 를 담지 않는다. 주소창의 쿼리(code, state)는 해시로 허용한
// 한 줄 스크립트로 지운다.
const chatgptCallbackScript = `history.replaceState(null,"","/auth/complete");`

func chatgptCallbackPage(w http.ResponseWriter, status int, title, body string) {
	sum := sha256.Sum256([]byte(chatgptCallbackScript))
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>`+
		`<style>body{font:16px system-ui,sans-serif;max-width:34rem;margin:16vh auto;padding:24px;color:#1f2328}h1{font-size:22px}p{line-height:1.6}</style>`+
		`<h1>%s</h1><p>%s</p><script>%s</script></html>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(body), chatgptCallbackScript)
}

const (
	msgChatGPTCallbackDoneTitle = "ChatGPT 로그인이 완료되었습니다"
	msgChatGPTCallbackDoneBody  = "이 탭을 닫고 ARTEX 설정 화면으로 돌아가세요."
	msgChatGPTCallbackFailTitle = "ChatGPT 로그인을 마치지 못했습니다"
	msgChatGPTCallbackBadReq    = "유효하지 않은 로그인 요청입니다. ARTEX 설정 화면에서 로그인을 다시 시작해 주세요."
)

// chatgptStart 는 재인가 URL(id_token_hint 포함)로 브라우저를 보낸다(/auth/start, 무인증).
// 로그인 시작 때 만든 256비트 state 를 아는 쪽(= 시작을 요청한 관리자 화면)만 쓸 수 있다.
func (s *Server) chatgptStart(w http.ResponseWriter, r *http.Request) {
	var pl *chatgptLogin
	if states := r.URL.Query()["state"]; len(states) == 1 && s.m != nil && s.m.pg != nil {
		pl = s.cgpt.findLogin(states[0], false)
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	if pl == nil || pl.authURL == "" {
		chatgptCallbackPage(w, http.StatusBadRequest, trMsg(msgChatGPTCallbackFailTitle), trMsg(msgChatGPTCallbackBadReq))
		return
	}
	h.Set("Location", pl.authURL)
	w.WriteHeader(http.StatusFound) // 본문 없이: 리디렉션 안내문에 URL 이 다시 찍히지 않게
}

// chatgptCallback 은 OpenAI 가 브라우저를 되돌려 보내는 주소다(/auth/callback, 무인증).
func (s *Server) chatgptCallback(w http.ResponseWriter, r *http.Request) {
	states := r.URL.Query()["state"]
	var pl *chatgptLogin
	if len(states) == 1 && s.m != nil && s.m.pg != nil {
		pl = s.cgpt.findLogin(states[0], false)
	}
	if pl == nil {
		// 모르는 state 는 아무 것도 소모하지 않는다.
		chatgptCallbackPage(w, http.StatusBadRequest, trMsg(msgChatGPTCallbackFailTitle), trMsg(msgChatGPTCallbackBadReq))
		return
	}
	if err := s.finishChatGPTLogin(r.Context(), pl, r.URL.RawQuery); err != nil {
		_, msg := chatgptErrCodeMessage(err)
		chatgptCallbackPage(w, http.StatusBadRequest, trMsg(msgChatGPTCallbackFailTitle), trChatGPTMsg(msg))
		return
	}
	chatgptCallbackPage(w, http.StatusOK, trMsg(msgChatGPTCallbackDoneTitle), trMsg(msgChatGPTCallbackDoneBody))
}

func (s *Server) chatgptLoginComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProfileID   int64  `json:"profile_id"`
		CallbackURL string `json:"callback_url"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p := s.chatgptProfile(w, req.ProfileID)
	if p == nil {
		return
	}
	if strings.TrimSpace(req.CallbackURL) == "" || len(req.CallbackURL) > 8192 {
		writeErr(w, 400, errChatGPTBadCallback)
		return
	}
	pl := s.cgpt.loginForProfile(p.ID)
	if pl == nil {
		writeErr(w, 409, errChatGPTNoPending)
		return
	}
	if err := s.finishChatGPTLogin(r.Context(), pl, req.CallbackURL); err != nil {
		status := 400
		if c := siwc.CodeOf(err); c == siwc.CodeNetwork || c == siwc.CodeIdentityUnavailable {
			status = 502
		}
		writeChatGPTErr(w, status, err)
		return
	}
	s.writeChatGPTStatus(w, r, p.ID)
}

// chatgptStatusBody 는 UI 에 줄 상태다. 토큰·ID 토큰·인가 코드는 절대 담지 않는다.
func (s *Server) chatgptStatusBody(ctx context.Context, p *db.LLMProfile) map[string]any {
	st := map[string]any{"profile_id": p.ID, "status": siwc.StatusDisconnected, "registered": false, "plan_usage": false}
	if rec, err := s.chatgptSession(p).Snapshot(ctx); err == nil && rec != nil {
		status := rec.Status
		if status == "" || (status == siwc.StatusConnected && rec.RefreshToken == "") {
			status = siwc.StatusDisconnected
		}
		st["status"] = status
		st["email"] = rec.Email
		st["registered"] = rec.ClientID != ""
		st["plan_usage"] = rec.HasPlanUsage()
		if !rec.ExpiresAt.IsZero() && status == siwc.StatusConnected {
			st["expires_at"] = rec.ExpiresAt.UTC().Format(time.RFC3339)
		}
		if rec.LastError != "" {
			st["last_error"] = rec.LastError
		}
	}
	login := map[string]any{"state": "none"}
	rt := &s.cgpt
	if pl := rt.loginForProfile(p.ID); pl != nil {
		login = map[string]any{"state": "pending", "redirect_uri": pl.p.RedirectURI,
			"expires_in": int(time.Until(pl.expires).Seconds())}
	} else {
		rt.mu.Lock()
		res, ok := rt.results[p.ID]
		rt.mu.Unlock()
		if ok && time.Since(res.At) < 30*time.Minute {
			login = map[string]any{"state": res.State}
			if res.State == "error" {
				fake := &siwc.Error{Code: res.Code}
				_, msg := chatgptErrCodeMessage(fake)
				login["error"], login["code"] = trChatGPTMsg(msg), res.Code
			}
		}
	}
	st["login"] = login
	return st
}

func (s *Server) chatgptStatus(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("profile_id"), 10, 64)
	s.writeChatGPTStatus(w, r, id)
}

func (s *Server) writeChatGPTStatus(w http.ResponseWriter, r *http.Request, id int64) {
	p := s.chatgptProfile(w, id)
	if p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	writeJSON(w, 200, s.chatgptStatusBody(ctx, p))
}

func (s *Server) chatgptLogout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProfileID int64 `json:"profile_id"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p := s.chatgptProfile(w, req.ProfileID)
	if p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	revoked, err := s.chatgptSession(p).Logout(ctx)
	if err != nil {
		writeChatGPTErr(w, 500, err)
		return
	}
	s.cgpt.forgetLogin(p.ID)
	log.Printf("[chatgpt] 로그아웃 profile=%d 원격 폐기 확인=%v", p.ID, revoked)
	s.afterChatGPTChange()
	// revoked=false: 로컬 토큰은 지웠지만 원격 폐기를 확인하지 못했다 — ChatGPT 설정에서 앱 연결을 끊을 수 있다.
	writeJSON(w, 200, map[string]any{"ok": true, "revoked": revoked})
}

func (rt *chatgptRuntime) forgetLogin(profileID int64) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.results, profileID)
	for st, pl := range rt.pending {
		if pl.profileID == profileID {
			delete(rt.pending, st)
		}
	}
}

func (s *Server) chatgptModels(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("profile_id"), 10, 64)
	p := s.chatgptProfile(w, id)
	if p == nil {
		return
	}
	fetch := s.cgpt.fetchModels
	if fetch == nil {
		fetch = agent.FetchChatGPTModels
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	models, err := fetch(ctx, p.Proxy, s.chatgptSession(p))
	if err != nil {
		code, msg := chatgptErrCodeMessage(err)
		// 다른 모델 목록 API 처럼 200 + ok:false 로 알려 화면에서 인라인으로 보여 준다.
		writeJSON(w, 200, map[string]any{"ok": false, "error": trChatGPTMsg(msg), "code": code})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "models": models})
}

// revokeChatGPTTokens 는 삭제된 프로필의 refresh token 을 원격에서 폐기한다(최선 노력).
// 행이 CASCADE 로 사라지기 전에 읽어 둔 값을 쓴다.
func (s *Server) revokeChatGPTTokens(p *db.LLMProfile, rec *siwc.Record) {
	if rec == nil || rec.RefreshToken == "" {
		return
	}
	rt := &s.cgpt
	rt.mu.Lock()
	client := rt.clientLocked(p.Proxy, log.Printf)
	rt.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Revoke(ctx, rec.ClientID, rec.RefreshToken); err != nil {
		log.Printf("[chatgpt] 프로필 삭제 뒤 원격 폐기를 확인하지 못했습니다 profile=%d: %s", p.ID, siwc.CodeOf(err))
	}
}

// dropChatGPTCredentials 는 구독 프로필이 API Key 방식으로 바뀔 때 남은 OAuth 자격 증명을
// 원격 폐기하고 지운다(client_id 포함 — 더는 이 프로필의 것이 아니다).
func (s *Server) dropChatGPTCredentials(ctx context.Context, p *db.LLMProfile) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := s.chatgptSession(p).Logout(ctx); err != nil {
		log.Printf("[chatgpt] 인증 방식 변경 중 로그아웃 실패 profile=%d: %s", p.ID, siwc.CodeOf(err))
	}
	if err := s.m.pg.DeleteProfileOAuth(ctx, p.ID); err != nil {
		log.Printf("[chatgpt] OAuth 자격 증명 삭제 실패 profile=%d: %v", p.ID, err)
	}
	s.cgpt.forget(p.ID)
}

// testChatGPT 는 저장된 구독 프로필로 실제 최소 호출을 보내 연결을 확인한다. 폼의 모델·추론 강도를
// 그대로 써서 "테스트는 통과했는데 실행은 거부"되는 차이를 없앤다.
func (s *Server) testChatGPT(w http.ResponseWriter, r *http.Request, profileID *int64, model, thinking, effort string) {
	fail := func(msg string) { writeJSON(w, 200, map[string]any{"ok": false, "error": trMsg(msg)}) }
	if s.m == nil || s.m.pg == nil || profileID == nil {
		fail(errChatGPTNoProfile)
		return
	}
	p, err := s.m.pg.ProfileByID(*profileID)
	if err != nil || p == nil || p.AuthMode != db.AuthModeChatGPT {
		fail(errChatGPTNoProfile)
		return
	}
	sess := s.chatgptSession(p)
	if !chatgptReady(sess) {
		fail(errChatGPTLoginRequired)
		return
	}
	if strings.TrimSpace(model) == "" {
		model = p.Model
	}
	cfg := agent.NewChatGPTConfig(model, p.Proxy, sess)
	cfg.ThinkingType, cfg.ReasoningEffort = thinking, effort
	lat, reply, err := agent.TestConnection(r.Context(), cfg)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "latency_ms": lat.Milliseconds(), "model": cfg.Model, "reply": truncateReply(reply)})
}
