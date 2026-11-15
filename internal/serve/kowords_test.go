package serve

import (
	"regexp"
	"testing"
)

// stoneWords are the stones the English names a firing's verdict by, as
// Korean would say them: a stone (돌) as a noun, alone or with a particle,
// and a white or a black one. Verbs that hold the same syllable (돌아가기,
// 되돌리기, 돌려, 돌린) aren't stones.
var stoneWords = regexp.MustCompile(`(?:흰|검은)\s*돌|(?:^|[^되])돌(?:들)?(?:이|을|은|는|도|과|와|의|로|만|에|에서)?(?:에요|이에요|예요|입니다)?(?:[\s.,:;!?)"]|$)`)

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
		{"그러면 돌이 바뀌어요.", true}, {"검은돌", true}, {"돌들이 쌓여요", true},
		{"이전으로 되돌려요", false}, {"되돌리기", false}, {"돌아가기", false}, {"백테스트를 돌린 결과", false}, {"되돌림", false},
		{"정탐(○)과 오탐(●)이에요.", false},
	} {
		if got := stoneWords.MatchString(c.text); got != c.stone {
			t.Errorf("%q: stone %v, want %v", c.text, got, c.stone)
		}
	}
	for en, ko := range catalogs["ko"] {
		if m := stoneWords.FindString(ko); m != "" {
			t.Errorf("ko: %q says %q for a stone: say 정탐 (○), 오탐 (●) or 면제 (◌)\n%s", en, m, ko)
		}
	}
}
