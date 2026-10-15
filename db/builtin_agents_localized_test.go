package db

import (
	"testing"
	"unicode"
)

// assertBuiltinAgentLabelKorean 은 builtinAgents 의 표시 전용 라벨(name·desc·promptVar
// desc/example)이 한글을 포함하고 중국어 한자가 없음을 단언한다. 이 값들은 agentDTO 로
// system/agents UI 에만 렌더되고 실제 system 프롬프트 조립(agent/promptcatalog.go)에는
// 쓰이지 않는 순수 UI 라벨이라, 상류 재동기화가 다시 중국어 원문을 끌어와도 이 테스트가
// 잡아낸다(reporter_seed_localized_test.go 와 같은 패턴).
func assertBuiltinAgentLabelKorean(t *testing.T, label, s string) {
	t.Helper()
	if s == "" {
		t.Fatalf("%s: 빈 문자열", label)
	}
	hasHangul := false
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			t.Fatalf("%s: 중국어 한자가 남아 있습니다: %q", label, s)
		}
		if unicode.Is(unicode.Hangul, r) {
			hasHangul = true
		}
	}
	if !hasHangul {
		t.Fatalf("%s: 한글이 없습니다: %q", label, s)
	}
}

// assertNoHan 은 (IP·포맷 샘플처럼 한글이 없을 수도 있는) promptVar.example 같은 값에
// 중국어 한자만 없는지 단언한다. 한글 포함은 요구하지 않는다.
func assertNoHan(t *testing.T, label, s string) {
	t.Helper()
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			t.Fatalf("%s: 중국어 한자가 남아 있습니다: %q", label, s)
		}
	}
}

func TestBuiltinAgentLabelsLocalized(t *testing.T) {
	for _, a := range builtinAgents {
		if a.key == "auto" {
			// "Auto" 는 표시 이름 자체가 영문 고유명사라 한글 대신 영문으로 유지한다.
			if a.name != "Auto" {
				t.Fatalf("agent %s: name 이 예상과 다릅니다: %q", a.key, a.name)
			}
		} else {
			assertBuiltinAgentLabelKorean(t, "agent."+a.key+".name", a.name)
		}
		assertBuiltinAgentLabelKorean(t, "agent."+a.key+".desc", a.desc)
		for _, v := range a.vars {
			assertBuiltinAgentLabelKorean(t, "agent."+a.key+".var."+v.name+".desc", v.desc)
			assertNoHan(t, "agent."+a.key+".var."+v.name+".example", v.example)
		}
	}
}
