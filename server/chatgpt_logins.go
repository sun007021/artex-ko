package server

import (
	"sync"
	"time"

	"github.com/Autumn-27/artex/siwc"
)

const (
	chatgptPendingTTL = 10 * time.Minute
	chatgptMaxPending = 32
	// chatgptResultTTL 은 마지막 로그인 결과를 상태 화면에 보여 주는 기간이다.
	chatgptResultTTL = 30 * time.Minute
)

type chatgptLogin struct {
	profileID int64
	p         *siwc.Pending
	expires   time.Time
	// authURL 은 id_token_hint 를 담은 재인가 URL 일 때만 채운다. 이 값은 UI 로 보내지 않고
	// GET /auth/start 가 브라우저를 302 로 보낼 때 딱 한 번만 쓴다(ID 토큰이 화면 스크립트에 노출되지 않게,
	// 그리고 주소창·히스토리에 남은 state URL 을 다시 열어도 재인가 URL 이 나오지 않게).
	authURL string
}

type chatgptLoginResult struct {
	State string // "done" | "error"
	Code  string
	At    time.Time
}

// chatgptLogins 는 진행 중인 로그인(state → 시도)과 프로필별 마지막 결과를 쥔 상태 기계다. 제로 값으로
// 쓸 수 있다.
type chatgptLogins struct {
	mu      sync.Mutex
	pending map[string]*chatgptLogin // state → 진행 중 로그인
	results map[int64]chatgptLoginResult
}

// begin 은 새 시도를 등록한다. 만료분과 같은 프로필의 이전 시도는 정리하고, 동시 시도가 상한이면
// ok=false 를 돌려준다. authURL 은 재인가일 때만 넘긴다.
func (l *chatgptLogins) begin(profileID int64, p *siwc.Pending, authURL string) (login *chatgptLogin, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.pending == nil {
		l.pending = map[string]*chatgptLogin{}
	}
	for st, pl := range l.pending {
		if now.After(pl.expires) || pl.profileID == profileID {
			delete(l.pending, st)
		}
	}
	if len(l.pending) >= chatgptMaxPending {
		return nil, false
	}
	login = &chatgptLogin{profileID: profileID, p: p, expires: now.Add(chatgptPendingTTL), authURL: authURL}
	l.pending[p.State] = login
	delete(l.results, profileID)
	return login, true
}

// find 는 state 로 진행 중 로그인을 찾는다(만료분은 정리). consume=true 면 제거한다.
func (l *chatgptLogins) find(state string, consume bool) *chatgptLogin {
	l.mu.Lock()
	defer l.mu.Unlock()
	pl := l.pending[state]
	if pl == nil {
		return nil
	}
	if time.Now().After(pl.expires) {
		delete(l.pending, state)
		return nil
	}
	if consume {
		delete(l.pending, state)
	}
	return pl
}

// takeStartURL 은 /auth/start 가 보낼 재인가 URL 을 한 번만 꺼내 준다. 두 번째부터는 빈 값이다.
func (l *chatgptLogins) takeStartURL(state string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	pl := l.pending[state]
	if pl == nil || time.Now().After(pl.expires) {
		return ""
	}
	u := pl.authURL
	pl.authURL = ""
	return u
}

// forProfile 은 프로필의 가장 늦게 만료되는 진행 중 로그인을 돌려준다.
func (l *chatgptLogins) forProfile(profileID int64) *chatgptLogin {
	l.mu.Lock()
	defer l.mu.Unlock()
	var best *chatgptLogin
	for st, pl := range l.pending {
		if time.Now().After(pl.expires) {
			delete(l.pending, st)
			continue
		}
		if pl.profileID == profileID && (best == nil || pl.expires.After(best.expires)) {
			best = pl
		}
	}
	return best
}

func (l *chatgptLogins) setResult(profileID int64, res chatgptLoginResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.results == nil {
		l.results = map[int64]chatgptLoginResult{}
	}
	l.results[profileID] = res
}

// result 는 최근 로그인 결과(없거나 오래됐으면 ok=false)를 돌려준다.
func (l *chatgptLogins) result(profileID int64) (chatgptLoginResult, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, ok := l.results[profileID]
	return res, ok && time.Since(res.At) < chatgptResultTTL
}

// forget 은 프로필의 진행 중 로그인과 결과를 버린다.
func (l *chatgptLogins) forget(profileID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.results, profileID)
	for st, pl := range l.pending {
		if pl.profileID == profileID {
			delete(l.pending, st)
		}
	}
}
