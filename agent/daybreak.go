package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// OpenAI Daybreak(Trusted Access for Cyber) 는 Responses API 요청의 access_programs.cyber 로 켠다.
// 승인된 계정(워크스페이스·프로젝트)에서만 효과가 있고, 끄면 이 필드를 보내지 않는다. Blue 만 지원한다
// (Red 는 승인된 기업·조직 전용이다).

// chatGPTDaybreakPrograms 는 Daybreak 를 켠 요청에 넣는 access_programs 값이다.
const chatGPTDaybreakPrograms = `{"cyber":"daybreak_blue"}`

// setDaybreakProgram 은 요청 본문 최상위에 Daybreak 프로그램을 정한다(호출자가 넣어 둔 값이 있어도 덮어쓴다).
func setDaybreakProgram(m map[string]json.RawMessage) {
	m["access_programs"] = json.RawMessage(chatGPTDaybreakPrograms)
}

// daybreakTransport 는 API Key 프로필(openai-responses 형식)의 모든 POST 본문에 Daybreak 프로그램을 더한다.
// ChatGPT 구독 경로는 자기 전송 계층(chatgptTransport)에서 같은 일을 한다. JSON 객체가 아닌 본문은 그대로 보낸다.
type daybreakTransport struct{ base http.RoundTripper }

func (t daybreakTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || req.Body == nil {
		return t.base.RoundTrip(req)
	}
	raw, err := readRequestBody(req)
	if err != nil {
		return nil, err
	}
	body := raw
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil && m != nil {
		setDaybreakProgram(m)
		if nb, err := json.Marshal(m); err == nil {
			body = nb
		}
	}
	r2 := req.Clone(req.Context())
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.ContentLength = int64(len(body))
	r2.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return t.base.RoundTrip(r2)
}
