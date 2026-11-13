package verdict

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

// A label set as kept: within 4 KB as JSON, each name and value within
// its bounds and as UTF-8, the short labels kept and the longest folded,
// the fold's label reserved, and alerts apart kept apart.
func TestKeptLabels(t *testing.T) {
	check := func(name string, in map[string]string) map[string]string {
		t.Helper()
		got := KeptLabels(in)
		b, _ := json.Marshal(got)
		if len(b) > maxKeptBytes {
			t.Errorf("%s: kept as %d bytes", name, len(b))
		}
		for k, v := range got {
			if len(k) > maxKeptName || len(v) > maxKeptValue && k != CutLabel || !utf8.ValidString(k) || !utf8.ValidString(v) {
				t.Errorf("%s: %q=%q", name, k, v)
			}
		}
		// Kept again, the same, but for a fold: the fold's label is the
		// API's to give no more than a set kept twice is.
		if again := KeptLabels(got); got[CutLabel] == "" && fmt.Sprint(again) != fmt.Sprint(got) {
			t.Errorf("%s: kept again, another set", name)
		}
		return got
	}
	// Multi-byte runes cut at a rune.
	got := check("runes", map[string]string{"x": strings.Repeat("가", 100)})
	if !strings.HasPrefix(got["x"], "가가") || !strings.Contains(got["x"], "…") {
		t.Errorf("runes: %q", got["x"])
	}
	// Many small labels, folded a few bytes at a time: the fold's own
	// label fits in what is left, to the byte.
	small := map[string]string{}
	for i := range 400 {
		small[fmt.Sprintf("l%03d", i)] = fmt.Sprintf("v%03d", i)
	}
	if got := check("many small", small); got[CutLabel] == "" {
		t.Errorf("many small: not folded")
	}
	for extra := range 40 {
		m := map[string]string{"pad": strings.Repeat("p", extra)}
		for i := range 300 {
			m[fmt.Sprintf("k%03d", i)] = "v"
		}
		check(fmt.Sprintf("many small, %d more", extra), m)
	}
	// Folded to what is left whatever the short labels kept come to: two
	// of 0 to 15 bytes beside 200 longer ones, so that what is kept ends
	// at each byte, the fold's own label after it.
	for p := range 16 {
		for q := range 16 {
			m := map[string]string{"p": strings.Repeat("p", p), "q": strings.Repeat("q", q)}
			for i := range 200 {
				m[fmt.Sprintf("k%03d", i)] = strings.Repeat("v", 20)
			}
			check(fmt.Sprintf("folded, %d and %d kept", p, q), m)
		}
	}
	// The longest folded first: every short label stays.
	mixed := map[string]string{}
	for i := range 30 {
		mixed[fmt.Sprintf("long%02d", i)] = strings.Repeat("x", 200)
	}
	for i := range 10 {
		mixed[fmt.Sprintf("s%d", i)] = "checkout"
	}
	got = check("mixed", mixed)
	for i := range 10 {
		if got[fmt.Sprintf("s%d", i)] != "checkout" {
			t.Errorf("mixed: a short label folded: %v", got)
			break
		}
	}
	// An API's label named as the fold's is folded itself.
	a, b := KeptLabels(map[string]string{CutLabel: "a", "x": "1"}), KeptLabels(map[string]string{CutLabel: "b", "x": "1"})
	if a[CutLabel] == "a" || a[CutLabel] == b[CutLabel] {
		t.Errorf("the fold's label taken from the API: %v %v", a, b)
	}
	// A name the API gives as another label's is kept (a long name's,
	// cut): both kept apart, the same each time.
	long := strings.Repeat("n", 200)
	kept := KeptName(long)
	x, y := KeptLabels(map[string]string{kept: "a", long: "b"}), KeptLabels(map[string]string{kept: "a", long: "c"})
	if fmt.Sprint(x) == fmt.Sprint(y) || x[CutLabel] == "" {
		t.Errorf("a name as another's is kept, taken for it: %v %v", x, y)
	}
	for range 20 {
		if again := KeptLabels(map[string]string{kept: "a", long: "b"}); fmt.Sprint(again) != fmt.Sprint(x) {
			t.Fatalf("kept as %v, then %v", x, again)
		}
	}
	// Apart, kept apart: a value that turns text around, and one cut.
	if KeptValue("disk ‮full") == KeptValue("disk ‭full") || KeptValue(strings.Repeat("a", 300)) == KeptValue(strings.Repeat("a", 301)) {
		t.Error("two values apart kept as one")
	}
}

// jsonLen is how long encoding/json writes a string.
func TestJSONLen(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	alphabet := []string{"a", "\"", "\\", "\n", "\b", "\f", "\t", "\r", "\x01", "\x1f", "<", ">", "&", "가", " ", " ", "\xff", "\xe2\x80", "😀", "‮", "\x7f"}
	for range 5000 {
		var b strings.Builder
		for range r.IntN(20) {
			b.WriteString(alphabet[r.IntN(len(alphabet))])
		}
		s := b.String()
		want, _ := json.Marshal(s)
		if got := jsonLen(s); got != len(want) {
			t.Fatalf("%q: %d, want %d", s, got, len(want))
		}
	}
}

// A replayed alert's labels, as kept, agree with those of the one that
// really fired, read as the API gives them: a long name and a long value
// taken as they would be kept, and compared so. One that differs in a
// long name's value doesn't agree.
func TestLabelsAgreeKept(t *testing.T) {
	long, url := strings.Repeat("n", 200), "https://example.com/?"+strings.Repeat("q", 300)
	real := map[string]string{"alertname": "Errors", long: "a", "url": url}
	if !labelsAgree(KeptLabels(real), real) {
		t.Errorf("kept %v, beside what fired, doesn't agree", KeptLabels(real))
	}
	other := map[string]string{"alertname": "Errors", long: "b", "url": url}
	if labelsAgree(KeptLabels(real), other) {
		t.Error("a long name's value apart, taken as alike")
	}
	if labelsAgree(KeptLabels(real), map[string]string{"alertname": "Errors", long: "a", "url": url + "x"}) {
		t.Error("a long value apart, taken as alike")
	}
}
