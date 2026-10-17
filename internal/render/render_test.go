package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lapilli-project/lapilli/internal/diagnose"
)

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"OpenSSH_9.6":           "OpenSSH_9.6",
		"a\x1b[2Jb":             `a\x1b[2Jb`,
		"title\x1b]0;owned\x07": `title\x1b]0;owned\x07`,
		"csi\u009b31m":          `csi\x9b31m`,
		"evil\u202etxt.exe":     `evil\u202etxt.exe`,
		"line\nnext\ttab":       "line\nnext\ttab",
		"cr\rover":              `cr\x0dover`,
		"\u00fcn\u00efc\u00f6d\u00e9 \u043a\u043b\u044e\u0447": "\u00fcn\u00efc\u00f6d\u00e9 \u043a\u043b\u044e\u0447",
		"bad\xffbyte": "bad\ufffdbyte",
		"c1\x9bbyte":  "c1\ufffdbyte",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

// Nothing from a report reaches the terminal raw, commands included.
func TestTextIsClean(t *testing.T) {
	r := &diagnose.Report{
		Headline: "lab\x1b[2J said hi",
		Facts:    []diagnose.Fact{{Label: "Server", Value: "OpenSSH\x1b]0;x\x07"}},
		Keys:     []diagnose.KeyRow{{Name: "key\u202e", Result: "rejected"}},
		Why:      []string{"why\x1b[1m"},
		Fix:      []diagnose.Step{{Text: "run:", Cmd: "chmod 600 '/tmp/a\x1b[8m'\nls"}},
		Also:     []diagnose.Note{{Level: "info", Text: "also\x07", Cmd: "x\x1b"}},
	}
	var b bytes.Buffer
	Text(&b, r, false)
	if out := b.String(); strings.ContainsAny(out, "\x1b\x07\u202e") {
		t.Errorf("raw control characters in:\n%q", out)
	}
	if !strings.Contains(b.String(), `chmod 600 '/tmp/a\x1b[8m'`) {
		t.Errorf("command not shown escaped:\n%s", b.String())
	}
}
