package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// TestFailNothingReplayed: a run in which the replay of every rule failed as
// the history couldn't be read judges nothing, and exits 1 saying so;
// --fail-nothing-replayed=false exits 0; one rule replayed keeps the exit 0,
// and --compare is left as it was.
func TestFailNothingReplayed(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	none := write(t, "none.yaml", `groups:
  - name: shop
    rules:
      - alert: Turned
        expr: turned > 1
      - alert: AlsoTurned
        expr: turned > 2
`)
	some := write(t, "some.yaml", `groups:
  - name: shop
    rules:
      - alert: Turned
        expr: turned > 1
      - alert: CheckoutErrors
        expr: errors > 5
        for: 5m
`)
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-7, title: Checkout down, start: \"2026-11-01T21:00:00Z\", end: \"2026-11-01T22:00:00Z\"}\n")
	verdictOf := func(args ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"verdict", "--prometheus", "http://p", "--incidents", incidents, "--tz", "UTC"}, args...), &stdout, &stderr)
		return code, stderr.String()
	}
	const said = "lapilli verdict: no rule was replayed: the replays of 2 rules failed, as the history couldn't be read, so the report judges nothing (--fail-nothing-replayed=false to exit 0)\n"
	if code, errs := verdictOf("--rules", none); code != 1 || !strings.Contains(errs, said) {
		t.Errorf("every replay failed: exit %d\n%s", code, errs)
	}
	for _, off := range []string{"--fail-nothing-replayed=false"} {
		if code, errs := verdictOf("--rules", none, off); code != 0 || strings.Contains(errs, "no rule was replayed") {
			t.Errorf("%s: exit %d\n%s", off, code, errs)
		}
	}
	if code, errs := verdictOf("--rules", some); code != 0 || strings.Contains(errs, "no rule was replayed") {
		t.Errorf("one rule replayed: exit %d\n%s", code, errs)
	}
	if code, errs := verdictOf("--rules", none, "--compare", none); code != 0 || strings.Contains(errs, "no rule was replayed") {
		t.Errorf("--compare: exit %d\n%s", code, errs)
	}
}
