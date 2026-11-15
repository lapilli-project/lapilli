package rules

import "testing"

// An escape past the last code point is no character: it reads as the
// letter and digits written, not as a rune its number wraps into.
func TestUnescapedPastMaxRune(t *testing.T) {
	for in, want := range map[string]string{
		`\UFFFFFFFF`: "UFFFFFFFF",
		`\U80000067`: "U80000067",
		`\U0010FFFF`: "\U0010FFFF",
		`gr\x6fups`:  "groups",
		`groups`:     "groups",
	} {
		if got := string(unescaped([]byte(in))); got != want {
			t.Errorf("unescaped(%q) = %q, want %q", in, got, want)
		}
	}
}
