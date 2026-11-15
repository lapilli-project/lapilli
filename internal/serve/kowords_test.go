package serve

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// whiteBlack is a white or a black stone, as Korean would say it.
var whiteBlack = regexp.MustCompile(`(?:흰|검은)\s*돌`)

// stoneEnds are what may follow a stone (돌) as a noun: its plural, a
// particle, a copula and its forms, longest first, so that 돌이면 is 이면
// and not 이.
var stoneEnds = []string{"이라고", "이에요", "입니다", "이면", "이고", "이라", "이나", "이든", "이다", "까지", "처럼", "으로", "부터",
	"보다", "마다", "에서", "에게", "예요", "들", "이", "을", "은", "는", "도", "과", "와", "의", "로", "만", "에", "인"}

// verbAfter are the words after 돌 and a space that make it the verb 돌다
// (to run): 돌 수 있어요, 돌 때, 돌 거예요, 돌 줄.
var verbAfter = []string{"수", "때", "거", "줄", "지", "뿐"}

// stoneWord is the first stone a Korean text names, as a noun, alone or
// with what may follow it (stoneEnds), and then the text's end, a space or
// a sign; "" for none. 되돌리기, 돌아가기, 돌려, 돌린, 돌고, 돌면, and 돌
// before 수, 때 and the like, are the verbs that hold the syllable.
func stoneWord(s string) string {
	if m := whiteBlack.FindString(s); m != "" {
		return m
	}
	for i := 0; ; {
		j := strings.Index(s[i:], "돌")
		if j < 0 {
			return ""
		}
		at := i + j
		i = at + len("돌")
		if strings.HasSuffix(s[:at], "되") {
			continue
		}
		rest := s[i:]
		for range 3 { // 돌들이에요: a plural, a particle, a copula
			for _, e := range stoneEnds {
				if strings.HasPrefix(rest, e) {
					rest = rest[len(e):]
					break
				}
			}
		}
		r, _ := utf8.DecodeRuneInString(rest)
		switch {
		case rest == "", strings.ContainsRune(".,:;!?)(\"'·", r):
			return s[at : len(s)-len(rest)]
		case r == ' ':
			after := strings.TrimLeft(rest, " ")
			verb := false
			for _, v := range verbAfter {
				verb = verb || strings.HasPrefix(after, v) && rest == s[i:]
			}
			if !verb {
				return s[at : len(s)-len(rest)]
			}
		}
	}
}

// TestKoreanSaysNoStones: the Korean UI speaks as on-call engineers do
// (docs/design/ui.md): a firing that caught something is 정탐 (○), one
// that caught nothing 오탐 (●), one a quiet window excused 면제 (◌); never
// the stones the English names them by, which read as a game to them.
func TestKoreanSaysNoStones(t *testing.T) {
	for _, c := range []struct {
		text  string
		stone bool
	}{
		{"돌", true}, {"규칙별 돌이에요.", true}, {"발화마다 돌을 줘요.", true}, {"흰 돌도 검은 돌도 아니라서", true},
		{"그러면 돌이 바뀌어요.", true}, {"검은돌", true}, {"돌들이 쌓여요", true}, {"돌인 발화", true}, {"돌이면 알려요", true},
		{"돌이고 아니고", true}, {"돌까지 세요", true}, {"돌처럼 보여요", true}, {"돌(○)", true}, {"돌로 표시해요", true},
		{"이전으로 되돌려요", false}, {"되돌리기", false}, {"돌아가기", false}, {"백테스트를 돌린 결과", false}, {"되돌림", false},
		{"정탐(○)과 오탐(●)이에요.", false}, {"매시간 돌 수 있어요", false}, {"백테스트가 돌 때", false}, {"다시 돌 거예요", false},
		{"돌고 나서", false}, {"돌면 알려요", false},
	} {
		if got := stoneWord(c.text) != ""; got != c.stone {
			t.Errorf("%q: stone %v, want %v", c.text, got, c.stone)
		}
	}
	for en, ko := range catalogs["ko"] {
		if m := stoneWord(ko); m != "" {
			t.Errorf("ko: %q says %q for a stone: say 정탐 (○), 오탐 (●) or 면제 (◌)\n%s", en, m, ko)
		}
	}
}
