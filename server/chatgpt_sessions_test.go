package server

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 프로필 id 하나에는 프록시가 바뀌어도 같은 Session 이 쓰인다(회전하는 refresh token 의 기록 주체가 하나).
func TestChatGPTSessionIsOnePerProfileAcrossProxyChanges(t *testing.T) {
	var reg chatgptSessions
	p := &db.LLMProfile{ID: 7, Proxy: ""}
	first := reg.session(nil, p)
	if again := reg.session(nil, p); again != first {
		t.Fatal("같은 프로필은 같은 세션이어야 합니다")
	}
	p2 := &db.LLMProfile{ID: 7, Proxy: "http://proxy.example:3128"}
	if got := reg.session(nil, p2); got != first {
		t.Fatal("프록시가 바뀌어도 세션은 하나여야 합니다(낡은 refresh token 으로 갱신하는 두 번째 세션 금지)")
	}
	if other := reg.session(nil, &db.LLMProfile{ID: 8}); other == first {
		t.Fatal("다른 프로필은 다른 세션이어야 합니다")
	}
	reg.forget(7)
	if after := reg.session(nil, p); after == first {
		t.Fatal("forget 뒤에는 새 세션이어야 합니다")
	}
}

// 쓸 수 없는 프록시 설정이면 인증 서버로 직접 연결하지 않고 요청이 실패하며, 로그에 자격 증명이 없다.
func TestChatGPTInvalidProxyFailsClosedAndIsRedacted(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	reg := chatgptSessions{issuer: srv.URL}
	for _, proxy := range []string{"ftp://user:topsecret@proxy.example:21", "user:topsecret@proxy.example:3128", "http://user:topsecret@"} {
		c := reg.client(proxy)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := c.Refresh(ctx, "oaiapp_x", "rt", nil)
		cancel()
		if err == nil {
			t.Fatalf("%q: 잘못된 프록시로 인증 서버 호출이 성공했습니다", proxy)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("잘못된 프록시인데 인증 서버에 직접 연결했습니다: %d회", hits.Load())
	}
	if strings.Contains(logs.String(), "topsecret") {
		t.Fatalf("로그에 프록시 자격 증명이 있습니다: %s", logs.String())
	}
}
