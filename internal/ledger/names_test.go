package ledger

import "testing"

func TestPlain(t *testing.T) {
	for s, want := range map[string]bool{
		"HighErrorRate":           true,
		"checkout-5xx [aws, sev]": true,
		"디스크 사용률":                 true,
		"HighErrorRate​":          false, // zero-width space
		"‮gnp.exe":                false, // right-to-left override
		"Disk\nTOTAL 0 missed":    false,
		"a\rb":                    false,
		"\x1b[31mred":             false,
		"line separator":          false,
		"\xff":                    false,
	} {
		if got := plain(s); got != want {
			t.Errorf("plain(%q) = %v, want %v", s, got, want)
		}
	}
}
