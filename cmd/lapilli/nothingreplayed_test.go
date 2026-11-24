package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

// TestFailNothingReplayed: a run in which the replay of every rule failed as
// the history couldn't be read judges nothing, and exits 1 saying so;
// --fail-nothing-replayed=false exits 0; one rule replayed keeps the exit 0,
// and --compare is left as it was.
func TestFailNothingReplayed(t *testing.T) {
	alone(t)
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
	const said = "lapilli verdict: no rule was replayed: the replays of 2 rules failed, as the history couldn't be read or their labels on their series were too many, so the report judges nothing (--fail-nothing-replayed=false to exit 0)\n"
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

// questionCounter counts the questions put to the history.
type questionCounter struct {
	fakeHistory
	n *atomic.Int64
}

func (c questionCounter) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	c.n.Add(1)
	return c.fakeHistory.QueryRange(ctx, expr, start, end, step)
}

// TestCheckNoData: --check-no-data=false skips the question, for each rule
// that never fired, of whether it read any series, and no other step.
func TestCheckNoData(t *testing.T) {
	alone(t)
	var n atomic.Int64
	newHistory = func(context.Context, *verdictFlags) (history, error) { return questionCounter{n: &n}, nil }
	now = func() time.Time { return t0 }
	rs := write(t, "rules.yaml", `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: errors > 5
        for: 5m
      - alert: Quiet
        expr: never > 1
`)
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-7, title: Checkout down, start: \"2026-11-01T21:00:00Z\", end: \"2026-11-01T22:00:00Z\"}\n")
	asked := func(more ...string) int64 {
		n.Store(0)
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"verdict", "--prometheus", "http://p", "--incidents", incidents, "--tz", "UTC", "--rules", rs}, more...), &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit %d\n%s", more, code, stderr.String())
		}
		return n.Load()
	}
	on, off := asked(), asked("--check-no-data=false")
	if on <= off {
		t.Errorf("%d questions with the step, %d without: want fewer", on, off)
	}
	// What the step finds, Quiet's one expression with no series, is said
	// with it and not without.
	said := func(more ...string) bool {
		var stdout, stderr bytes.Buffer
		run(append([]string{"verdict", "--prometheus", "http://p", "--incidents", incidents, "--tz", "UTC", "--rules", rs, "--json"}, more...), &stdout, &stderr)
		var rep verdict.Report
		if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
			t.Fatal(err)
		}
		for _, r := range rep.Rules {
			if r.Rule == "Quiet" && len(r.NoData) > 0 {
				return true
			}
		}
		return false
	}
	if !said() || said("--check-no-data=false") {
		t.Errorf("no_data said with the step %v, without %v", said(), said("--check-no-data=false"))
	}
}
