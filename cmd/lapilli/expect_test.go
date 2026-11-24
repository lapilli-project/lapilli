package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/verdict"
)

// --expect holds the rules a change touches to what they are expected to
// do: the report says, before → after, what the change broke; with
// --fail-expectations the run exits 1 for it, and with --expect-required
// for a rule added that nothing holds. A file it can't read fails the run
// before anything is replayed.
func TestCompareExpectations(t *testing.T) {
	alone(t)
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

	// By labels, as --rules-select takes them: the rule's own labels.
	byLabels := write(t, "labels.yaml", "expectations:\n  - labels: severity=page\n    budget: {week: 5}\n  - labels: severity=ticket\n    budget: {week: 0}\n")
	if code, out, _ := lapilli("--rules", flappy, "--compare", inPlace, "--expect", byLabels, "--fail-expectations"); code != 1 ||
		!strings.Contains(out, "Expectations: 1 broken by this change, 0 broken before too, 0 met") {
		t.Errorf("by labels: exit %d\n%s", code, out)
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
	// serve takes --expect (docs/design/expectations.md, step 2), and
	// refuses at start a file it can't read as one.
	var out2, errs2 strings.Builder
	if code := run([]string{"serve", "--rules", inPlace, "--prometheus", "http://p", "--expect", bad}, &out2, &errs2); code != 2 ||
		!strings.Contains(errs2.String(), "--expect "+bad+": ") || !strings.Contains(errs2.String(), "budjet") {
		t.Errorf("serve --expect: exit %d\n%s", code, errs2.String())
	}
}

// --draft-expectations writes an expectations file drafted from the
// period judged: each rule's most in a whole week, rounded up, and the
// incidents it caught, read back as an expectations file is. Budgets need
// no incident; it drafts the rules judged, not a change, nor a redacted
// report.
func TestDraftExpectations(t *testing.T) {
	alone(t)
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	rulesFile := write(t, "rules.yaml", `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: errors > 5
      - alert: Flappy
        expr: flappy
`)
	incs := write(t, "incidents.yaml", "incidents:\n  - id: INC-1\n    start: 2026-11-01T21:10:00Z\n    end: 2026-11-01T21:40:00Z\n")
	out := filepath.Join(t.TempDir(), "expectations.yaml")
	lapilli := func(args ...string) (int, string) {
		var o, errs strings.Builder
		code := run(append([]string{"verdict", "--prometheus", "http://p", "--tz", "UTC", "--since", "14d"}, args...), &o, &errs)
		return code, errs.String()
	}
	if code, errs := lapilli("--rules", rulesFile, "--incidents", incs, "--draft-expectations", out); code != 0 || !strings.Contains(errs, "2 rules drafted") {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	e, err := verdict.ReadExpectations(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	got := map[string]string{}
	for _, x := range e.Expectations {
		var held []string
		for _, it := range x.Items() {
			held = append(held, it.What+" "+it.Held)
		}
		got[x.Rule] = strings.Join(held, "; ")
	}
	if got["CheckoutErrors"] != "budget week 1; budget asleep 1; budget refires 1; catches INC-1 within 1m" ||
		!strings.HasPrefix(got["Flappy"], "budget week 15; budget asleep 1;") {
		t.Errorf("drafted:\n%v\n%s", got, b)
	}
	if !strings.Contains(string(b), "Drafted by lapilli verdict from the period 2026-10-19 to 2026-11-02, by whole weeks counted back from its end") {
		t.Errorf("what it says of itself:\n%s", b)
	}
	// Budgets need no incident written down.
	if code, errs := lapilli("--rules", rulesFile, "--draft-expectations", out); code != 0 {
		t.Errorf("without incidents: exit %d\n%s", code, errs)
	}
	for _, args := range [][]string{
		{"--rules", rulesFile, "--compare", rulesFile, "--draft-expectations", out},
		{"--rules", rulesFile, "--incidents", incs, "--redact", "--draft-expectations", out},
	} {
		if code, errs := lapilli(args...); code != 2 || !strings.Contains(errs, "--draft-expectations") {
			t.Errorf("%v: exit %d\n%s", args, code, errs)
		}
	}
}
