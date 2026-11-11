package sse

import "testing"

// A Grafana math expression comes from a file or, over MCP, from an agent:
// parsed and worked out, it never panics.
func FuzzParseMath(f *testing.F) {
	for _, s := range []string{`$A + $B`, `abs($A) > 2 && ($B - 1) * -3 <= 4 || !$C`, `log($A) / ($B % 2)`, `${A} == 1`, `-(-(-1))`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		e, err := ParseMath(s)
		if err != nil {
			return
		}
		e.Refs()
		e.Eval(func(ref string) (Set, bool) {
			return Set{{Labels: map[string]string{"a": ref}, V: 2}, {V: -1}}, true
		})
	})
}

// What near misses do with a condition read from a file: write it back,
// find its comparisons, set one's number, and work out what one decides.
// None of it panics, and what is written back parses again to the same.
func FuzzComparisons(f *testing.F) {
	for _, s := range []string{`$A > 0.9 && $B < 10`, `($A > 1 || 2 <= $B) && !($C >= 3)`, `abs($A) > 2 || $B ^ 2 <= -1`, `$A > $B && ${C D} < 1e3`} {
		f.Add(s, 1)
	}
	f.Fuzz(func(t *testing.T, s string, i int) {
		e, err := ParseMath(s)
		if err != nil {
			return
		}
		w := Format(e)
		again, err := ParseMath(w)
		if err != nil {
			t.Fatalf("%q wrote %q: %v", s, w, err)
		}
		if Format(again) != w {
			t.Fatalf("%q wrote %q, then %q", s, w, Format(again))
		}
		refs := func(ref string) (Set, bool) {
			return Set{{Labels: map[string]string{"a": ref}, V: 2}, {V: -1}}, true
		}
		cs := Comparisons(e)
		if len(cs) == 0 {
			return
		}
		i = (i%len(cs) + len(cs)) % len(cs)
		SetNumber(e, i, 0.5).Eval(refs)
		if d, ok := Decides(e, i); ok {
			d.Eval(refs)
			d.Refs()
		}
	})
}
