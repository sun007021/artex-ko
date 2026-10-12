package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Autumn-27/norma/llm"
)

// toolInfo 는 claude 가 부르는 도구 별칭(접두사 제외)과 ARTEX 도구 이름을 잇는다.
type toolInfo struct {
	artexName string
	tool      mcpTool
}

// toolSet 은 프로세스가 시작될 때 보여 준 도구 집합의 지문이다(별칭 → 설명+스키마 해시).
// claude 는 tools/list 를 프로세스 시작 때 한 번만 읽으므로(listChanged:false), 실행 중인 프로세스는
// 이 집합에 없던 도구나 바뀐 스키마를 모른다.
type toolSet map[string]string

// covers 는 want 의 모든 도구를 s 가 같은 정의로 이미 갖고 있는지 알려 준다. 요청에서 빠진 도구는
// 상관없다: 그 도구의 호출은 callPairer 가 "지금은 쓸 수 없음" 으로 거절한다(재동기화보다 싸다).
func (s toolSet) covers(want toolSet) bool {
	for name, h := range want {
		if s[name] != h {
			return false
		}
	}
	return true
}

// missing 은 s 가 want 를 덮지 못하는 이유로 쓸 도구 이름들(정렬)을 돌려준다.
func (s toolSet) missing(want toolSet) []string {
	var out []string
	for name, h := range want {
		if s[name] != h {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// toolBox 는 세션이 이번 요청에서 노출하는 도구 목록이다. 요청마다 통째로 교체된다.
type toolBox struct {
	mu     sync.RWMutex
	byName map[string]toolInfo // claude 가 부르는 이름(접두사 제외) → 정보
	list   []mcpTool
	set    toolSet
}

// replace 는 이번 요청의 도구 스키마를 MCP 이름으로 바꿔 둔다. claude 가 이름을 접두사 없이 보내므로
// ARTEX 이름과 1:1 인 안전한 별칭을 만든다(허용 문자 밖의 문자는 '_' 로, 충돌 시 번호).
func (b *toolBox) replace(schemas []llm.ToolSchema) {
	by := map[string]toolInfo{}
	list := make([]mcpTool, 0, len(schemas))
	set := toolSet{}
	for _, sc := range schemas {
		name := safeToolName(sc.Name, by)
		schema := sc.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if _, ok := schema["type"]; !ok {
			cp := make(map[string]any, len(schema)+1)
			for k, v := range schema {
				cp[k] = v
			}
			cp["type"] = "object"
			schema = cp
		}
		t := mcpTool{
			Name: name, Description: sc.Description, InputSchema: schema,
			// 결과가 길어도 파일로 빼 두지 않고 그대로 모델에 주도록 한도를 올린다(ARTEX 가 이미 자체 상한을 건다).
			Meta: map[string]any{"anthropic/maxResultSizeChars": 500000},
		}
		by[name] = toolInfo{artexName: sc.Name, tool: t}
		list = append(list, t)
		set[name] = toolHash(t)
	}
	b.mu.Lock()
	b.byName, b.list, b.set = by, list, set
	b.mu.Unlock()
}

func toolHash(t mcpTool) string {
	schema, _ := json.Marshal(t.InputSchema) // map 키는 정렬되어 직렬화된다
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s", t.Name, t.Description, schema)
	return hex.EncodeToString(h.Sum(nil)[:8])
}

func (b *toolBox) lookup(name string) (toolInfo, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	ti, ok := b.byName[name]
	return ti, ok
}

func (b *toolBox) snapshot() []mcpTool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]mcpTool(nil), b.list...)
}

// fingerprint 는 지금 도구 집합의 지문이다(읽기 전용으로 공유한다).
func (b *toolBox) fingerprint() toolSet {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.set
}

// safeToolName 은 MCP/API 도구 이름 규칙([A-Za-z0-9_-], 최대 52자: 접두사 12자 + 52 = 64)에 맞춘다.
func safeToolName(name string, taken map[string]toolInfo) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	n := b.String()
	if n == "" {
		n = "tool"
	}
	if len(n) > 52 {
		n = n[:52]
	}
	base := n
	for i := 2; ; i++ {
		if _, dup := taken[n]; !dup {
			return n
		}
		suffix := fmt.Sprintf("_%d", i)
		if len(base)+len(suffix) > 52 {
			n = base[:52-len(suffix)] + suffix
		} else {
			n = base + suffix
		}
	}
}
