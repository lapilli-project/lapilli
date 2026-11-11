package ledger

import "unicode"

// nameHidden reports whether a name has a character that doesn't show, and
// so could make it look like another's: a control character; a format
// character (a zero-width space, a soft hyphen, a word joiner, a byte
// order mark, those that turn text around), but the zero-width joiner and
// non-joiner between two letters of one script, or two emoji, as names
// written in Persian or with emoji use them; a letter or mark that shows
// nothing on its own; or a variation selector but after a symbol. It is
// the rule v0.25.0 gives names (Hidden), which takes its place once this
// is on it.
func nameHidden(name string) bool {
	rs := []rune(name)
	for i, r := range rs {
		switch {
		case unicode.IsControl(r), blankRune[r]:
			return true
		case r == 0x200C || r == 0x200D:
			p := i - 1
			for p >= 0 && (rs[p] >= 0xFE00 && rs[p] <= 0xFE0F) {
				p--
			}
			if p < 0 || i+1 >= len(rs) || !joinsRunes(rs[p], rs[i+1]) {
				return true
			}
		case unicode.Is(unicode.Cf, r):
			return true
		case r >= 0xFE00 && r <= 0xFE0F:
			if i == 0 || !emojiRune(rs[i-1]) {
				return true
			}
		}
	}
	return false
}

// blankRune are letters and marks that show nothing on their own.
var blankRune = map[rune]bool{0x034F: true, 0x115F: true, 0x1160: true, 0x17B4: true, 0x17B5: true, 0x2800: true, 0x3164: true, 0xFFA0: true}

// joinsRunes reports whether a joiner may stand between two characters:
// two letters of one script beyond ASCII, or two emoji.
func joinsRunes(a, b rune) bool {
	if emojiRune(a) && emojiRune(b) {
		return true
	}
	if a < 0x80 || b < 0x80 || !unicode.IsLetter(a) || !unicode.IsLetter(b) {
		return false
	}
	for _, script := range unicode.Scripts {
		if unicode.Is(script, a) {
			return unicode.Is(script, b)
		}
	}
	return false
}

// emojiRune is a symbol beyond ASCII, or a skin tone, as emoji are made of.
func emojiRune(r rune) bool {
	return r >= 0x80 && unicode.Is(unicode.So, r) || r >= 0x1F3FB && r <= 0x1F3FF
}
