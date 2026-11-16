package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// --expect holds the rules a change touches to what they are expected to
// do: the report says, before → after, what the change broke; with
// --fail-expectations the run exits 1 for it, and with --expect-required
// for a rule added that nothing holds. A file it can't read fails the run
// before anything is replayed.
func TestCompareExpectations(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	base := `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: errors > 5
        labels: {severity: page}
`
	inPlace := write(t, "base.yaml", base)
	// Twelve firings in the last ten hours, where the rule in place fired once.
	flappy := write(t, "flappy.yaml", strings.Replace(base, "expr: errors > 5", "expr: flappy", 1))
	added := write(t, "added.yaml", base+"      - alert: New\n        expr: flappy\n")
	expect := write(t, "expect.yaml", "expectations:\n  - rule: CheckoutErrors\n    budget: {week: 5}\n")
	loose := write(t, "loose.yaml", "expectations:\n  - rule: CheckoutErrors\n    budget: {week: 50}\n")
	lapilli := func(args ...string) (int, string, string) {
		var out, errs strings.Builder
		code := run(append([]string{"verdict", "--prometheus", "http://p", "--tz", "UTC"}, args...), &out, &errs)
		return code, out.String(), errs.String()
	}

	code, out, errs := lapilli("--rules", flappy, "--compare", inPlace, "--expect", expect, "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	for _, want := range []string{`"expected": true`, `"expectations_broken": 1`, `"what": "budget week"`, `"before": "1"`, `"before_state": "met"`, `"after_state": "broken"`} {
		if !strings.Contains(out, want) {
			t.Errorf("--json: no %s in\n%s", want, out)
		}
	}
	if code, out, _ := lapilli("--rules", flappy, "--compare", inPlace, "--expect", expect); code != 0 ||
		!strings.Contains(out, "Expectations: 1 broken by this change, 0 broken before too, 0 met") || !strings.Contains(out, "✗ broken by this change") {
		t.Errorf("text: exit %d\n%s", code, out)
	}
	if code, out, _ := lapilli("--rules", flappy, "--compare", inPlace, "--expect", expect, "--changed-only", "--markdown"); code != 0 ||
		!strings.Contains(out, `"expectations_broken":1`) || !strings.Contains(out, "#### Expectations") || !strings.Contains(out, "| `CheckoutErrors` | at most 5 a week |") ||
		!strings.Contains(out, "**✗ broken by this change**") {
		t.Errorf("--markdown: exit %d\n%s", code, out)
	}
	if code, _, errs := lapilli("--rules", flappy, "--compare", inPlace, "--expect", expect, "--fail-expectations"); code != 1 ||
		!strings.Contains(errs, "the change breaks 1 expectations the rules in place meet (--fail-expectations)") {
		t.Errorf("--fail-expectations: exit %d\n%s", code, errs)
	}
	// Raised in the pull request: met after, and the report says it was 5.
	if code, out, errs := lapilli("--rules", flappy, "--compare", inPlace, "--expect", loose, "--expect-before", expect, "--fail-expectations"); code != 0 ||
		!strings.Contains(out, "(was 5, in this change)") {
		t.Errorf("raised: exit %d\n%s\n%s", code, out, errs)
	}
	// A rule added that nothing holds.
	if code, out, errs := lapilli("--rules", added, "--compare", inPlace, "--expect", expect, "--expect-required"); code != 1 ||
		!strings.Contains(out, "Rules this change adds that no expectation holds: New") || !strings.Contains(errs, "adds 1 rules no expectation holds") {
		t.Errorf("--expect-required: exit %d\n%s\n%s", code, out, errs)
	}

	bad := write(t, "bad.yaml", "expectations:\n  - rule: CheckoutErrors\n    budjet: {week: 5}\n")
	if code, _, errs := lapilli("--rules", flappy, "--compare", inPlace, "--expect", bad); code != 1 || !strings.Contains(errs, "--expect "+bad+": ") ||
		!strings.Contains(errs, "budjet") {
		t.Errorf("a bad file: exit %d\n%s", code, errs)
	}
	for _, args := range [][]string{
		{"--rules", flappy, "--expect", expect},
		{"--rules", flappy, "--compare", inPlace, "--fail-expectations"},
		{"--rules", flappy, "--compare", inPlace, "--expect-before", expect},
	} {
		if code, _, errs := lapilli(args...); code != 2 || !strings.Contains(errs, "--expect") {
			t.Errorf("%v: exit %d\n%s", args, code, errs)
		}
	}
	var out2, errs2 strings.Builder
	if code := run([]string{"serve", "--rules", inPlace, "--prometheus", "http://p", "--expect", expect}, &out2, &errs2); code != 2 ||
		!strings.Contains(errs2.String(), "lapilli serve doesn't take --expect") {
		t.Errorf("serve --expect: exit %d\n%s", code, errs2.String())
	}
}
