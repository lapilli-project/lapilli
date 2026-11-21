package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// --fail-missed exits 1 when the change leaves an incident a rule caught
// before it caught by no rule after it, by --incidents or --impact; the
// report says which, as text, Markdown and JSON. With --changed-only, the
// rules left alone are replayed when one might still catch it: one that
// does keeps it caught. It goes with --compare and the incidents, and
// with --fail-expectations, either failing fails the run.
func TestCompareFailMissed(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	rulesWith := func(forDur, more string) string {
		return `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: errors > 5
        for: ` + forDur + `
        labels: {severity: page}
      - alert: ImpactSeen
        expr: impact
        for: 1m
      - alert: Quiet
        expr: never
` + more
	}
	inPlace := write(t, "base.yaml", rulesWith("5m", ""))
	// Held 2h, CheckoutErrors never fires: errors > 5 lasts an hour.
	slower := write(t, "slower.yaml", rulesWith("2h", ""))
	// A rule left alone that catches the incident on both sides.
	alsoBase := write(t, "also-base.yaml", rulesWith("5m", "      - alert: CheckoutDown\n        expr: errors > 5\n        for: 1m\n"))
	alsoSlower := write(t, "also-slower.yaml", rulesWith("2h", "      - alert: CheckoutDown\n        expr: errors > 5\n        for: 1m\n"))
	incidents := write(t, "incidents.yaml", fmt.Sprintf("incidents:\n  - id: INC-7\n    title: Checkout down\n    start: %s\n    end: %s\n",
		t0.Add(-3*time.Hour).Format(time.RFC3339), t0.Add(-2*time.Hour).Format(time.RFC3339)))
	lapilli := func(args ...string) (int, string, string) {
		var out, errs strings.Builder
		code := run(append([]string{"verdict", "--prometheus", "http://p", "--tz", "UTC", "--since", "1d"}, args...), &out, &errs)
		return code, out.String(), errs.String()
	}
	said := "lapilli verdict: the change leaves 1 incidents a rule caught before it caught by no rule (--fail-missed)\n"

	// Every rule replayed: missed, said in each report, the run fails.
	code, out, errs := lapilli("--rules", slower, "--compare", inPlace, "--incidents", incidents, "--fail-missed")
	if code != 1 || !strings.HasSuffix(errs, said) ||
		!strings.Contains(out, "The run fails (--fail-missed): 1 incidents a rule caught before the change are caught by no rule after it.\n") ||
		!strings.Contains(out, "INC-7 Checkout down  2026-11-01 21:00  CheckoutErrors ") {
		t.Errorf("text: exit %d\n%s\n%s", code, out, errs)
	}
	code, out, _ = lapilli("--rules", slower, "--compare", inPlace, "--incidents", incidents, "--fail-missed", "--json")
	for _, want := range []string{`"missed_checked": true`, `"incidents_missed": 1`, `"missed": [`, `"id": "INC-7"`} {
		if code != 1 || !strings.Contains(out, want) {
			t.Errorf("--json: exit %d, no %s in\n%s", code, want, out)
		}
	}
	// --changed-only: the rules left alone (Quiet, ImpactSeen) replayed
	// to see, and none catches it.
	code, out, errs = lapilli("--rules", slower, "--compare", inPlace, "--incidents", incidents, "--fail-missed", "--changed-only", "--markdown")
	if code != 1 || !strings.Contains(out, `"missed_checked":true,"incidents_missed":1,"missed_not_known":0}`) || !strings.Contains(out, "#### Caught before, by no rule after\n") ||
		!strings.Contains(out, "**This run fails** (`--fail-missed`): 1 incidents a rule caught before the change are caught by no rule after it, the rules left alone among them.") ||
		!strings.Contains(out, "| `INC-7` `Checkout down` | 2026-11-01 21:00 | `CheckoutErrors` ") || !strings.HasSuffix(errs, said) {
		t.Errorf("--changed-only --markdown: exit %d\n%s\n%s", code, out, errs)
	}
	// A rule left alone still catches it: not missed, with --changed-only
	// as without.
	for _, only := range []string{"--changed-only", "--json"} {
		code, out, errs = lapilli("--rules", alsoSlower, "--compare", alsoBase, "--incidents", incidents, "--fail-missed", only)
		if code != 0 || strings.Contains(errs, "--fail-missed") {
			t.Errorf("caught by a rule left alone, %s: exit %d\n%s\n%s", only, code, out, errs)
		}
	}
	if code, out, _ = lapilli("--rules", alsoSlower, "--compare", alsoBase, "--incidents", incidents, "--fail-missed", "--changed-only"); code != 0 ||
		!strings.Contains(out, "Every incident a rule caught before the change is caught after it too (--fail-missed).") {
		t.Errorf("caught by a rule left alone, as text: exit %d\n%s", code, out)
	}
	// Without the flag: the report as it was, and 0.
	if code, out, _ = lapilli("--rules", slower, "--compare", inPlace, "--incidents", incidents, "--changed-only", "--markdown"); code != 0 ||
		!strings.Contains(out, `"missed_checked":false,"incidents_missed":0,"missed_not_known":0}`) || strings.Contains(out, "fails") {
		t.Errorf("without --fail-missed: exit %d\n%s", code, out)
	}
	// By --impact: ImpactSeen held an hour misses the 10m window.
	impactSlower := write(t, "impact.yaml", strings.Replace(rulesWith("5m", ""), "for: 1m", "for: 1h", 1))
	if code, out, errs = lapilli("--rules", impactSlower, "--compare", inPlace, "--impact", "impact", "--fail-missed", "--changed-only", "--json"); code != 1 ||
		!strings.Contains(out, `"incidents_missed": 1`) || !strings.HasSuffix(errs, said) {
		t.Errorf("--impact: exit %d\n%s\n%s", code, out, errs)
	}
	// With --fail-expectations, both fail it, each said.
	expect := write(t, "expect.yaml", "expectations:\n  - rule: CheckoutErrors\n    catches:\n      - incident: INC-7\n")
	if code, _, errs = lapilli("--rules", slower, "--compare", inPlace, "--incidents", incidents, "--expect", expect, "--fail-expectations", "--fail-missed"); code != 1 ||
		!strings.Contains(errs, "(--fail-expectations)") || !strings.Contains(errs, said) {
		t.Errorf("both: exit %d\n%s", code, errs)
	}
	// Nothing changed: nothing missed.
	if code, out, errs = lapilli("--rules", inPlace, "--compare", inPlace, "--incidents", incidents, "--fail-missed", "--markdown"); code != 0 ||
		!strings.Contains(out, `"missed_checked":true,"incidents_missed":0,"missed_not_known":0}`) {
		t.Errorf("nothing changed: exit %d\n%s\n%s", code, out, errs)
	}
	// Redacted, the incident and its rules by their aliases.
	if code, out, _ = lapilli("--rules", slower, "--compare", inPlace, "--incidents", incidents, "--fail-missed", "--json", "--redact"); code != 1 ||
		strings.Contains(out, "INC-7") || strings.Contains(out, "CheckoutErrors") || !strings.Contains(out, `"missed": [`) {
		t.Errorf("--redact: exit %d\n%s", code, out)
	}

	for _, c := range []struct {
		args []string
		said string
	}{
		{[]string{"--rules", slower, "--compare", inPlace, "--fail-missed"}, "--fail-missed fails the run when the change leaves an incident caught by no rule: give the incidents, --incidents FILE or --impact PROMQL"},
		{[]string{"--rules", slower, "--incidents", incidents, "--fail-missed"}, "--fail-missed holds a change to the rules to the incidents those in place catch: it goes with --compare"},
	} {
		if code, _, errs := lapilli(c.args...); code != 2 || !strings.Contains(errs, c.said) {
			t.Errorf("%v: exit %d\n%s", c.args, code, errs)
		}
	}
	var o, e strings.Builder
	if code := run([]string{"serve", "--rules", inPlace, "--prometheus", "http://p", "--fail-missed"}, &o, &e); code != 2 ||
		!strings.Contains(e.String(), "--fail-missed") {
		t.Errorf("serve --fail-missed: exit %d\n%s", code, e.String())
	}
}

// A rule after the change that couldn't be replayed may catch an incident
// the rules replayed don't: it is not known to be missed, said so in each
// report and counted on the summary, and fails nothing without
// --fail-unreplayed, with --changed-only (the rules left alone replayed
// apart) as without.
func TestCompareFailMissedNotKnown(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	rulesWith := func(forDur string) string {
		return `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: errors > 5
        for: ` + forDur + `
      - alert: TurnedAway
        expr: turned_total > 0
`
	}
	inPlace, slower := write(t, "base.yaml", rulesWith("5m")), write(t, "slower.yaml", rulesWith("2h"))
	incidents := write(t, "incidents.yaml", fmt.Sprintf("incidents:\n  - id: INC-7\n    start: %s\n    end: %s\n",
		t0.Add(-3*time.Hour).Format(time.RFC3339), t0.Add(-2*time.Hour).Format(time.RFC3339)))
	lapilli := func(args ...string) (int, string, string) {
		var out, errs strings.Builder
		code := run(append([]string{"verdict", "--prometheus", "http://p", "--tz", "UTC", "--since", "1d", "--rules", slower, "--compare", inPlace,
			"--incidents", incidents, "--fail-missed"}, args...), &out, &errs)
		return code, out.String(), errs.String()
	}
	said := "1 incidents a rule caught before the change are caught by no rule replayed after it, but a rule after it couldn't be replayed: whether they are missed isn't known, and they fail nothing"
	for _, only := range [][]string{{"--changed-only"}, nil} {
		code, out, errs := lapilli(only...)
		if code != 0 || !strings.Contains(out, said) || strings.Contains(out, "The run fails") || strings.Contains(out, "Every incident") {
			t.Errorf("%v, text: exit %d\n%s\n%s", only, code, out, errs)
		}
		code, out, _ = lapilli(append(only, "--markdown")...)
		if code != 0 || !strings.Contains(out, `"missed_checked":true,"incidents_missed":0,"missed_not_known":1}`) || !strings.Contains(out, said+" (`--fail-unreplayed` fails") {
			t.Errorf("%v, --markdown: exit %d\n%s", only, code, out)
		}
		if code, out, _ = lapilli(append(only, "--json")...); code != 0 || !strings.Contains(out, `"missed_not_known": 1`) {
			t.Errorf("%v, --json: exit %d\n%s", only, code, out)
		}
		// The history couldn't be read for it: --fail-unreplayed fails it.
		if code, _, errs := lapilli(append(only, "--fail-unreplayed")...); code != 1 || !strings.Contains(errs, "(--fail-unreplayed)") {
			t.Errorf("%v, --fail-unreplayed: exit %d\n%s", only, code, errs)
		}
	}
}
