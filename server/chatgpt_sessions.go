package server

import (
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/siwc"
)

const (
	chatgptAuthHTTPTimeout = 30 * time.Second
	chatgptMaxClients      = 16
)

// chatgptSessions 는 구독 프로필의 토큰 세션 레지스트리다. 프로필 id 하나에 살아 있는 siwc.Session 은
// 항상 하나다: refresh token 은 회전하므로 같은 행을 두 세션이 갱신하면 한쪽이 낡은 토큰으로 갱신을
// 시도해 계정이 reauth_required 가 된다. 프록시가 바뀌어도 세션은 그대로 두고 HTTP 클라이언트만 바꾼다.
// 제로 값으로 쓸 수 있다.
type chatgptSessions struct {
	mu       sync.Mutex
	clients  map[string]*siwc.Client // proxy → client (discovery/JWKS 캐시 공유)
	sessions map[int64]*chatgptSessionEntry

	// issuer 는 테스트가 가짜 발급자로 바꾸는 자리다(운영에서는 비어 있어 siwc 의 기본 발급자를 쓴다).
	issuer string
}

type chatgptSessionEntry struct {
	proxy string
	sess  *siwc.Session
}

// failClosedTransport 는 쓸 수 없는 프록시 설정에서 인증 서버로 가는 모든 요청을 막는다. 프록시가
// 잘못됐다고 직접 연결로 돌아가면 사용자가 프록시로 감추려던 트래픽이 그대로 나가기 때문이다.
type failClosedTransport struct{ err error }

func (t failClosedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return nil, t.err
}

// client 는 proxy 설정으로 인증 서버에 접속하는 클라이언트를 돌려준다(설정별로 캐시).
// 프록시 설정이 올바르지 않으면 직접 연결로 우회하지 않고 모든 요청이 실패하는 클라이언트를 준다.
// 오류 문구에는 프록시 자격 증명이 들어가지 않는다(agent.ProxyHTTPClient 가 가린다).
func (r *chatgptSessions) client(proxy string) *siwc.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clientLocked(proxy)
}

func (r *chatgptSessions) clientLocked(proxy string) *siwc.Client {
	if c := r.clients[proxy]; c != nil {
		return c
	}
	hc, err := agent.ProxyHTTPClient(proxy, chatgptAuthHTTPTimeout)
	if err != nil {
		log.Printf("[chatgpt] 프록시 설정을 쓸 수 없어 인증 서버 접속을 막습니다(직접 연결로 우회하지 않음): %v", err)
		hc = &http.Client{Transport: failClosedTransport{err: errors.New("chatgpt: proxy setting is not usable")}, Timeout: chatgptAuthHTTPTimeout}
	}
	c := siwc.New(siwc.Config{AppName: chatgptAppName, Issuer: r.issuer, HTTPClient: hc, Logf: log.Printf})
	if r.clients == nil {
		r.clients = map[string]*siwc.Client{}
	}
	if len(r.clients) >= chatgptMaxClients { // 바뀌는 프록시 값이 쌓이지 않게(세션은 자기 클라이언트를 계속 쥔다)
		clear(r.clients)
	}
	r.clients[proxy] = c
	return c
}

// session 은 프로필의 토큰 세션을 돌려준다. 같은 프로필이면 프록시가 바뀌어도 항상 같은 포인터라서
// agent.Config 의 == 비교(캐시된 구성 판별)가 유지된다.
func (r *chatgptSessions) session(pg *db.DB, p *db.LLMProfile) *siwc.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.sessions[p.ID]; e != nil {
		if e.proxy != p.Proxy {
			e.sess.SetClient(r.clientLocked(p.Proxy))
			e.proxy = p.Proxy
		}
		return e.sess
	}
	sess := r.clientLocked(p.Proxy).NewSession(profileOAuthStore{pg: pg, id: p.ID})
	if r.sessions == nil {
		r.sessions = map[int64]*chatgptSessionEntry{}
	}
	r.sessions[p.ID] = &chatgptSessionEntry{proxy: p.Proxy, sess: sess}
	return sess
}

// forget 은 프로필 삭제·인증 방식 변경 뒤 세션을 버린다.
func (r *chatgptSessions) forget(profileID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, profileID)
}
