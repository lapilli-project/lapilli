package rules

import (
	"strconv"
	"strings"
	"testing"
)

// TestScopeNumbers: a namespace or a cluster put into an expression of
// many numbers takes time and memory of its length: the rest of the
// expression was lowered at every number, 431 MB for 20,000 numbers with a
// capital letter after them, 6 s for 100,000 without.
func TestScopeNumbers(t *testing.T) {
	expr := strings.TrimSuffix(strings.Repeat("1+", 20000), "+") + "+A"
	doc := "apiVersion: monitoring.googleapis.com/v1\nkind: Rules\nmetadata: {name: r, namespace: n}\nspec:\n  groups:\n  - name: g\n    rules:\n" +
		"    - alert: X\n      expr: " + strconv.Quote(expr) + "\n"
	var rs []Rule
	var err error
	bytes, took := allocated(func() { rs, err = Parse([]byte(doc)) })
	t.Logf("20,000 numbers: %d MB allocated, %v", bytes>>20, took)
	if err != nil || len(rs) != 1 || !strings.HasSuffix(rs[0].Expr, `+A{namespace="n"}`) {
		t.Fatalf("%v", err)
	}
	if bytes > 16<<20 {
		t.Errorf("%d MB allocated", bytes>>20)
	}
	for in, want := range map[string]string{"0x1F + a": `0x1F + a{c="x"}`, "0X1f+a": `0X1f+a{c="x"}`, "1e-3 + a": `1e-3 + a{c="x"}`, "0": "0"} {
		if got, err := Scope(in, map[string]string{"c": "x"}); err != nil || got != want {
			t.Errorf("%q: %q, %v", in, got, err)
		}
	}
}
