package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// API Key 프로필(openai-responses): Daybreak 를 켜면 본문에 access_programs.cyber=daybreak_blue 가 더해지고,
// 기존 필드는 그대로이며, 끄면 본문을 건드리지 않는다.
func TestDaybreakTransportAddsProgramOnlyWhenOn(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{true, false} {
		var got []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, _ = io.ReadAll(r.Body)
			w.WriteHeader(200)
		}))
		client, err := quotaAwareHTTPClientDaybreak("", "", on)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Post(srv.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"m","input":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		srv.Close()
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("%s: %v", got, err)
		}
		if m["model"] != "m" || m["input"] != "x" {
			t.Fatalf("on=%v 기존 필드가 유지돼야 한다: %s", on, got)
		}
		ap, present := m["access_programs"].(map[string]any)
		if on && (!present || ap["cyber"] != "daybreak_blue") {
			t.Fatalf("켜면 access_programs.cyber=daybreak_blue: %s", got)
		}
		if !on && m["access_programs"] != nil {
			t.Fatalf("꺼지면 보내지 않는다: %s", got)
		}
	}
}

// JSON 객체가 아닌 본문(그리고 GET)은 그대로 통과한다.
func TestDaybreakTransportPassesThroughNonObjectBodies(t *testing.T) {
	t.Parallel()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	}))
	defer srv.Close()
	client, err := quotaAwareHTTPClientDaybreak("", "", true)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Post(srv.URL, "application/json", strings.NewReader(`[1,2]`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != `[1,2]` {
		t.Fatalf("%q", got)
	}
	resp, err = client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
