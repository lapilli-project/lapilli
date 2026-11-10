package ledger

import (
	"unicode"
	"unicode/utf8"
)

// plain reports whether s is UTF-8 with nothing in it that doesn't show:
// no control characters, no format characters (zero-width spaces, marks
// that turn text right to left) and no line or paragraph separators. A
// name from outside is shown on pages, in Slack and to agents; one with
// such a character could look like another rule's, or break a line where
// a reader doesn't see one.
func plain(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) || unicode.In(c, unicode.Cf, unicode.Zl, unicode.Zp) {
			return false
		}
	}
	return true
}

// Plain reports whether a name from outside has nothing in it that doesn't
// show, as plain does.
func Plain(s string) bool { return plain(s) }
