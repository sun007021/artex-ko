package agent

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ChatGPT 구독 경로의 도구 규칙.
//
// 공식 문서(siwc/token-sharing-open-source/preview-limitations, "Supported tools")는 구독 직접 경로에서
// function/custom 도구를 "namespace 로 묶거나 additional_tools 입력 항목으로 전달"하라고 한다. 최상위
// tools 에 {"type":"function"} 을 그대로 두면 거부될 수 있으므로, 전송 직전에 모든 function 도구를 하나의
// namespace 로 감싼다(문서의 namespace 모양: {"type":"namespace","name":..,"description":..,"tools":[..]}).
// defer_loading 과 tool_search 는 쓰지 않는다 — tool_search 는 이 경로에서 지원되지 않는다.
//
// 왕복 규칙:
//   - 요청: tools 의 function 항목을 namespace 안으로 옮기고, 입력 이력의 function_call 항목에도
//     같은 namespace 를 붙여 준다(응답의 function_call 항목이 "namespace" 필드를 갖는 모양과 같다).
//   - 응답: function_call 항목의 name 은 도구 이름 그대로이고 namespace 는 별도 필드다(norma 는 name 만 읽는다).
//     방어적으로, name 이 "<namespace>.<이름>" 처럼 접두사가 붙어 오면 접두사를 떼어 norma 와 에이전트가
//     아는 ARTEX 도구 이름으로 되돌린다.
const (
	chatGPTToolNamespace = "artex"
	chatGPTNamespaceDesc = "ARTEX penetration-testing agent tools."
)

// chatGPTNamespaceSeparators are the joiners a namespaced function name could use.
var chatGPTNamespaceSeparators = []string{".", "/", ":", "__"}

// groupChatGPTTools moves every top-level function tool of the request into the ARTEX namespace and tags
// the function_call items of the input history with it. items is the decoded input array (may be nil); the
// returned changed reports whether items was modified.
func groupChatGPTTools(m map[string]json.RawMessage, items []map[string]json.RawMessage) (changed bool) {
	raw, ok := m["tools"]
	if !ok {
		return false
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return false
	}
	var grouped, rest []json.RawMessage
	names := map[string]bool{}
	for _, t := range tools {
		var h struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(t, &h)
		if h.Type == "function" && h.Name != "" {
			grouped = append(grouped, t)
			names[h.Name] = true
			continue
		}
		rest = append(rest, t)
	}
	if len(grouped) == 0 {
		return false
	}
	ns, err := json.Marshal(map[string]any{
		"type": "namespace", "name": chatGPTToolNamespace, "description": chatGPTNamespaceDesc, "tools": grouped,
	})
	if err != nil {
		return false
	}
	out, err := json.Marshal(append([]json.RawMessage{ns}, rest...))
	if err != nil {
		return false
	}
	m["tools"] = out
	for _, it := range items {
		if string(it["type"]) != `"function_call"` {
			continue
		}
		var name string
		_ = json.Unmarshal(it["name"], &name)
		if _, has := it["namespace"]; !has && names[name] {
			it["namespace"] = json.RawMessage(`"` + chatGPTToolNamespace + `"`)
			changed = true
		}
	}
	return changed
}

// stripToolNamespace turns a namespaced function_call name back into the plain ARTEX tool name. The
// documented shape (plain name + a separate "namespace" field) is left untouched.
func stripToolNamespace(name string) (string, bool) {
	for _, sep := range chatGPTNamespaceSeparators {
		if rest, ok := strings.CutPrefix(name, chatGPTToolNamespace+sep); ok && rest != "" {
			return rest, true
		}
	}
	return name, false
}

// plainFunctionCall rewrites one output item in place. It reports whether the item changed.
func plainFunctionCall(item map[string]json.RawMessage) bool {
	if string(item["type"]) != `"function_call"` {
		return false
	}
	if raw, ok := item["namespace"]; ok {
		var ns string
		if json.Unmarshal(raw, &ns) != nil || ns != chatGPTToolNamespace {
			return false // some other namespace: not ours to touch
		}
	}
	var name string
	if json.Unmarshal(item["name"], &name) != nil {
		return false
	}
	plain, ok := stripToolNamespace(name)
	if !ok {
		return false
	}
	b, _ := json.Marshal(plain)
	item["name"] = b
	return true
}

// plainResponseTools rewrites the function_call items of a response object ({"output":[..]}).
func plainResponseTools(resp json.RawMessage) (json.RawMessage, bool) {
	if !bytes.Contains(resp, []byte(`"function_call"`)) {
		return resp, false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(resp, &obj) != nil {
		return resp, false
	}
	var out []map[string]json.RawMessage
	if json.Unmarshal(obj["output"], &out) != nil {
		return resp, false
	}
	changed := false
	for _, it := range out {
		if plainFunctionCall(it) {
			changed = true
		}
	}
	if !changed {
		return resp, false
	}
	ob, err := json.Marshal(out)
	if err != nil {
		return resp, false
	}
	obj["output"] = ob
	nb, err := json.Marshal(obj)
	if err != nil {
		return resp, false
	}
	return nb, true
}

// plainEventTools rewrites the function names of an SSE event body: output_item.added / output_item.done
// carry "item", response.completed carries "response". Events without a namespaced name come back as-is.
func plainEventTools(data []byte) ([]byte, bool) {
	if !bytes.Contains(data, []byte(`"function_call"`)) {
		return data, false
	}
	var ev map[string]json.RawMessage
	if json.Unmarshal(data, &ev) != nil {
		return data, false
	}
	changed := false
	if raw, ok := ev["item"]; ok {
		var it map[string]json.RawMessage
		if json.Unmarshal(raw, &it) == nil && plainFunctionCall(it) {
			if b, err := json.Marshal(it); err == nil {
				ev["item"], changed = b, true
			}
		}
	}
	if raw, ok := ev["response"]; ok {
		if nb, ok := plainResponseTools(raw); ok {
			ev["response"], changed = nb, true
		}
	}
	if !changed {
		return data, false
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return data, false
	}
	return b, true
}
