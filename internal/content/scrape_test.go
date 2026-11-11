package content

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
)

// realScrapes are what a real Prometheus 3.5.0 said of
// testdata/prometheus-3.5.0/prometheus.yml: global 30s; prometheus 15s,
// batch 2m, node the global's, and serviceMonitor/shop/checkout/0 at 1m,
// relabelled to job="checkout" as the Prometheus Operator does.
func realScrapes(t *testing.T) Scrapes {
	t.Helper()
	b, err := os.ReadFile("testdata/prometheus-3.5.0/status-config.json")
	if err != nil {
		t.Fatal(err)
	}
	var a struct {
		Data struct {
			YAML string `json:"yaml"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	s, ok := ScrapeConfig(a.Data.YAML)
	if !ok {
		t.Fatal("the configuration didn't read")
	}
	targets, err := os.ReadFile("testdata/prometheus-3.5.0/targets-active.json")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Targets(targets) {
		t.Fatal("the targets didn't read")
	}
	return s
}

// Each job's interval, by its job label: its name, or, as its targets say,
// what relabelling made it.
func TestScrapeConfig(t *testing.T) {
	s := realScrapes(t)
	want := map[string]time.Duration{"prometheus": 15 * time.Second, "batch": 2 * time.Minute, "node": 30 * time.Second,
		"serviceMonitor/shop/checkout/0": time.Minute, "checkout": time.Minute}
	if s.Global != 30*time.Second || len(s.Jobs) != len(want) {
		t.Fatalf("global %s, jobs %v", s.Global, s.Jobs)
	}
	for job, d := range want {
		if s.Jobs[job] != d {
			t.Errorf("%s: %s, want %s", job, s.Jobs[job], d)
		}
	}
	// Without its targets, the relabelled job is known by its name only.
	b, _ := os.ReadFile("testdata/prometheus-3.5.0/status-config.json")
	var a struct {
		Data struct{ YAML string } `json:"data"`
	}
	json.Unmarshal(b, &a)
	if c, _ := ScrapeConfig(a.Data.YAML); c.Jobs["checkout"] != 0 || c.Jobs["serviceMonitor/shop/checkout/0"] != time.Minute {
		t.Errorf("from the configuration alone: %v", c.Jobs)
	}
	for _, bad := range []string{"global: [", "global:\n  scrape_interval: soon\n"} {
		if _, ok := ScrapeConfig(bad); ok {
			t.Errorf("%q read", bad)
		}
	}
	if c, ok := ScrapeConfig("scrape_configs:\n- job_name: a\n"); !ok || c.Global != time.Minute || c.Jobs["a"] != time.Minute {
		t.Errorf("Prometheus's default: %+v", c)
	}
	if (&Scrapes{}).Targets([]byte(`{"status": "error"}`)) {
		t.Error("an error read as targets")
	}
}

// A range is counted with the interval of the job its selector names (the
// longest of those a regular expression can be), or the global one.
func TestShortRangeByJob(t *testing.T) {
	s := realScrapes(t)
	o := Options{Scrape: s.Global, Jobs: s.Jobs}
	for _, c := range []struct {
		expr string
		want []string
	}{
		// 1m holds four of prometheus's 15s scrapes.
		{`rate(prometheus_http_requests_total{job="prometheus"}[1m]) > 1`, nil},
		{`rate(x{job="prometheus"}[25s]) > 1`, []string{ShortRange + `:rate[25s] < 2 × 15s (job "prometheus")`}},
		// batch is scraped every 2m: 3m holds one point.
		{`rate(batch_runs_total{job="batch"}[3m]) > 0`, []string{ShortRange + `:rate[3m] < 2 × 2m (job "batch")`}},
		{`increase(batch_runs_total{instance="b", job='batch'}[4m]) > 0`, nil},
		// The Prometheus Operator's job, by its label.
		{`rate(http_requests_total{job="checkout",code=~"5.."}[90s]) > 1`, []string{ShortRange + `:rate[90s] < 2 × 1m (job "checkout")`}},
		// A regular expression: the longest of the jobs it can be.
		{`rate(x{job=~"node|batch"}[2m]) > 0`, []string{ShortRange + `:rate[2m] < 2 × 2m (job "batch")`}},
		{`rate(x{job=~"pro.*"}[40s]) > 0`, nil},
		// No job, or one Prometheus hasn't, or only what it isn't: the global.
		{`rate(x[50s]) > 0`, []string{ShortRange + `:rate[50s] < 2 × 30s`}},
		{`rate(x{job="elsewhere"}[50s]) > 0`, []string{ShortRange + `:rate[50s] < 2 × 30s`}},
		{`rate(x{job!="batch"}[1m]) > 0`, nil},
		// A label called something like job isn't.
		{`rate(x{job_name="batch"}[1m]) > 0`, nil},
		{`rate(x{other="job", z="batch"}[1m]) > 0`, nil},
	} {
		r := good("R")
		r.Expr = c.expr
		one(t, r, o, c.want...)
	}
	// Every job scraped alike: that interval, whatever the global says.
	r := good("R")
	r.Expr = `rate(x[40s]) > 0`
	one(t, r, Options{Scrape: 30 * time.Second, Jobs: map[string]time.Duration{"a": 15 * time.Second, "b": 15 * time.Second}})
	one(t, r, Options{Scrape: 30 * time.Second, Jobs: map[string]time.Duration{"a": 15 * time.Second, "b": time.Minute}}, ShortRange+":rate[40s] < 2 × 30s")
}

// A job's regular expression is compiled once, not once for each job
// Prometheus has, and the jobs are sorted once: 2,000 rules of two ranges
// each, over 2,000 jobs, are checked within a second (0.3 s on a laptop).
func TestShortRangeByJobCost(t *testing.T) {
	jobs := map[string]time.Duration{}
	for i := range 1000 {
		jobs[fmt.Sprintf("serviceMonitor/ns%d/app%d/0", i, i)] = 30 * time.Second
		jobs[fmt.Sprintf("app%d", i)] = 30 * time.Second
	}
	var rs []rules.Rule
	for i := range 2000 {
		rs = append(rs, rules.Rule{Name: fmt.Sprintf("r%d", i), Group: "g",
			Expr: fmt.Sprintf(`sum(rate(x{job=~"app%d|app%d-canary"}[5m])) / sum(rate(y{job=~"app%d.*"}[5m])) > 1`, i%1000, i%1000, i%1000)})
	}
	began := time.Now()
	Check(rs, Options{Scrape: time.Minute, Jobs: jobs})
	if took := time.Since(began); took > time.Second*time.Duration(slowFactor()) {
		t.Errorf("2,000 rules over 2,000 jobs: %s", took)
	}
	// A regular expression that doesn't compile names no job.
	s := Scrapes{Global: time.Minute, Jobs: map[string]time.Duration{"a": 2 * time.Minute}}
	if d, job := s.For([]Matcher{{Re: true, Value: "a("}}); d != 2*time.Minute || job != "" {
		t.Errorf("%s %q", d, job)
	}
}
