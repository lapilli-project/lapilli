package content

import (
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
)

// The selector a range over a selector that names no job is taken of,
// written again as one plain selector, for the jobs its series come from
// to be asked by; nothing that isn't one.
func TestRangeSelector(t *testing.T) {
	for expr, want := range map[string]string{
		`rate(http_requests_total[5m]) > 1`:                 `http_requests_total`,
		`sum(rate(http_requests_total{code=~'5..'}[1m]))`:   `http_requests_total{code=~"5.."}`,
		`increase({__name__="x", env!="dev",}[10m])`:        `{__name__="x",env!="dev"}`,
		`rate(x{job="batch"}[1m])`:                          ``, // names its job
		`rate(x{a=b}[1m])`:                                  ``,
		`rate((x)[5m:1m])`:                                  ``,
		`rate(x{a="\") or vector(1) or (\""}[1m])`:          `x{a="\") or vector(1) or (\""}`,
		`rate(x{a="1"} offset 5m [1m])`:                     ``,
		`delta(ns:cpu:sum{ns="shop"}[2m]) and rate(y[30s])`: `ns:cpu:sum{ns="shop"}`,
	} {
		us := Ranges(expr)
		got := ""
		if len(us) > 0 {
			got = us[0].Selector
		}
		if got != want {
			t.Errorf("%s: %q, want %q", expr, got, want)
		}
	}
}

// Asked of the selectors whose range could be shorter than two scrapes of
// some job, each once; of none when every job has one interval, or with
// no job's known.
func TestToAsk(t *testing.T) {
	s := Scrapes{Global: 30 * time.Second, Jobs: map[string]time.Duration{"batch": 2 * time.Minute, "api": 15 * time.Second}}
	rs := []rules.Rule{
		{Name: "a", Expr: `rate(batch_runs_total[3m]) == 0 or rate(batch_runs_total[3m]) > 9`},
		{Name: "b", Expr: `rate(api_requests_total[10m]) > 1`}, // holds two of the longest
		{Name: "c", Expr: `rate(x{job="api"}[1m]) > 1`},
		{Name: "d", Expr: `rate(y[1m]) > 1`, Unsupported: rules.Disabled},
		{Name: "e", Expr: `rate({app="x"}[1m])`, Logs: rules.LokiRuler},
	}
	if got := ToAsk(rs, s); !slices.Equal(got, []string{"batch_runs_total"}) {
		t.Errorf("%q", got)
	}
	for name, s := range map[string]Scrapes{
		"one interval": {Global: 30 * time.Second, Jobs: map[string]time.Duration{"batch": time.Minute, "api": time.Minute}},
		"no jobs":      {Global: 30 * time.Second},
	} {
		if got := ToAsk(rs, s); got != nil {
			t.Errorf("%s: %q", name, got)
		}
	}
}

// A range over a selector that names no job counts with the longest
// interval of the jobs its series come from, and says whose; one not
// asked, or whose jobs aren't known, as before.
func TestShortRangeByJobsOf(t *testing.T) {
	jobs := map[string]time.Duration{"batch": 2 * time.Minute, "api": 15 * time.Second}
	r := good("Stalled")
	r.Expr = `rate(batch_runs_total[3m]) == 0`
	o := Options{Scrape: 30 * time.Second, Jobs: jobs}
	one(t, r, o)
	o.JobsOf = map[string][]string{"batch_runs_total": {"api", "batch"}}
	one(t, r, o, ShortRange+`:rate[3m] < 2 × 2m (job "batch")`)
	// A job Prometheus doesn't have is the global interval's; none known,
	// as before.
	o.JobsOf = map[string][]string{"batch_runs_total": {"elsewhere"}}
	one(t, r, o)
	if d, job, ok := (Scrapes{Global: time.Minute, Jobs: jobs}).ForJobs([]string{"elsewhere", "api"}); d != time.Minute || job != "elsewhere" || !ok {
		t.Errorf("%s %q %v", d, job, ok)
	}
}
