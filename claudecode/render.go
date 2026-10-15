package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

// ---- 메시지 서명 ----
//
// ARTEX(agentcore)는 매 호출마다 전체 대화(req.Messages)를 다시 준다. claude 세션은 이미 그 앞부분을
// 자기 컨텍스트에 갖고 있으므로, 이번 요청이 "지난번까지 흡수한 대화 + 새 꼬리"인지 확인해 꼬리만 보낸다.
// 서명은 구조(역할·텍스트·도구 ID)만 본다 — 도구 결과의 본문은 제외한다. ARTEX 의 MicroCompact 가 오래된
// 도구 결과를 임시로 비우지만 그건 ARTEX 쪽 작업 사본의 변화일 뿐 claude 세션과 어긋남이 아니기 때문이다.
// 반면 AutoCompact(앞부분을 요약으로 교체)는 서명이 달라지므로 재동기화(새 프로세스 + 전사 재생)로 이어진다.

// msgSig 는 메시지 하나의 서명이다. thinking 블록은 제외한다.
func msgSig(m llm.Message) string {
	var text strings.Builder
	var ids []string
	for _, b := range m.Content {
		switch b.Type {
		case llm.BlockText:
			text.WriteString(b.Text)
		case llm.BlockToolUse:
			ids = append(ids, "u:"+b.ID)
		case llm.BlockToolResult:
			ids = append(ids, "r:"+b.ToolUseID)
		}
	}
	h := sha256.Sum256([]byte(text.String()))
	return string(m.Role) + ":" + hex.EncodeToString(h[:8]) + ":" + strings.Join(ids, ",")
}

func sigs(msgs []llm.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = msgSig(m)
	}
	return out
}

func hasPrefix(all, prefix []string) bool {
	if len(prefix) > len(all) {
		return false
	}
	for i := range prefix {
		if all[i] != prefix[i] {
			return false
		}
	}
	return true
}

// reminderPrefix 는 agentcore 가 매 모델 호출 맨 앞에 끼우는 <system-reminder>(오늘 날짜 등)의 시작이다.
// 이 메시지는 대화 기록에 저장되지 않고 Prompt 마다 새로 만들어지므로 서명 비교에서 뺀다.
const reminderPrefix = "<system-reminder>"

// splitReminder 는 맨 앞 system-reminder 메시지를 분리한다.
func splitReminder(msgs []llm.Message) (reminder *llm.Message, rest []llm.Message) {
	if len(msgs) > 0 && msgs[0].Role == llm.RoleUser && len(msgs[0].Content) == 1 &&
		msgs[0].Content[0].Type == llm.BlockText && strings.HasPrefix(msgs[0].Content[0].Text, reminderPrefix) {
		return &msgs[0], msgs[1:]
	}
	return nil, msgs
}

// ---- 전사 렌더링(재동기화용) ----

// renderTranscript 는 claude 세션에 이어 붙일 수 없는 대화(새 프로세스, 압축으로 바뀐 앞부분)를 한 개의
// 사용자 메시지 본문으로 만든다. 구조화된 tool_use/tool_result 를 외부에서 주입할 방법이 없으므로 텍스트
// 전사로 옮긴다. API 경로의 모델이 보는 것(ARTEX 의 현재 작업 사본)과 같은 내용이고, 형식만 다르다.
// 이 경로는 프로세스 재시작·압축·TTL 만료 때만 탄다(정상 진행은 꼬리만 보낸다).
func renderTranscript(reminder *llm.Message, msgs []llm.Message) string {
	var b strings.Builder
	if reminder != nil {
		for _, c := range reminder.Content {
			if c.Type == llm.BlockText {
				b.WriteString(c.Text)
				b.WriteString("\n\n")
			}
		}
	}
	if len(msgs) == 1 && msgs[0].Role == llm.RoleUser && onlyText(msgs[0]) {
		b.WriteString(msgs[0].Text())
		return b.String()
	}
	b.WriteString("<restored-conversation>\n")
	b.WriteString("This session was restored: the conversation so far is reproduced below as a transcript. ")
	b.WriteString("The tool calls shown were really executed by you earlier and their results are real. ")
	b.WriteString("Continue from the final entry exactly as if these were your own earlier turns; do not repeat finished work.\n\n")
	for _, m := range msgs {
		writeTranscriptMessage(&b, m)
	}
	b.WriteString("</restored-conversation>")
	return b.String()
}

func onlyText(m llm.Message) bool {
	for _, c := range m.Content {
		if c.Type != llm.BlockText {
			return false
		}
	}
	return true
}

func writeTranscriptMessage(b *strings.Builder, m llm.Message) {
	role := "user"
	if m.Role == llm.RoleAssistant {
		role = "assistant"
	}
	for _, c := range m.Content {
		switch c.Type {
		case llm.BlockText:
			if strings.TrimSpace(c.Text) != "" {
				fmt.Fprintf(b, "[%s]\n%s\n\n", role, c.Text)
			}
		case llm.BlockToolUse:
			in := strings.TrimSpace(string(c.Input))
			if in == "" {
				in = "{}"
			}
			fmt.Fprintf(b, "[assistant tool_use id=%s name=%s]\n%s\n\n", c.ID, c.Name, in)
		case llm.BlockToolResult:
			status := "ok"
			if c.IsError {
				status = "error"
			}
			fmt.Fprintf(b, "[tool_result id=%s status=%s]\n%s\n\n", c.ToolUseID, status, resultText(c))
		}
	}
}

// resultText 는 tool_result 의 텍스트 블록을 이어 붙인다.
func resultText(c llm.ContentBlock) string {
	var parts []string
	for _, x := range c.Content {
		if x.Type == llm.BlockText {
			parts = append(parts, x.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ---- 입력 메시지(stdin stream-json) ----

type inputBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type inputMessage struct {
	Type    string `json:"type"`
	Message struct {
		Role    string       `json:"role"`
		Content []inputBlock `json:"content"`
	} `json:"message"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	SessionID       string  `json:"session_id"`
}

// userInputLine 은 stdin 으로 보낼 stream-json 사용자 메시지 한 줄(개행 포함)이다.
func userInputLine(texts ...string) []byte {
	var im inputMessage
	im.Type = "user"
	im.Message.Role = "user"
	for _, t := range texts {
		if strings.TrimSpace(t) == "" {
			continue
		}
		im.Message.Content = append(im.Message.Content, inputBlock{Type: "text", Text: t})
	}
	raw, _ := json.Marshal(im)
	return append(raw, '\n')
}
