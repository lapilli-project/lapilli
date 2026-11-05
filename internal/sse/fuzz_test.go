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
