package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// jobbed is a Prometheus whose jobs have intervals of their own, and whose
// batch_runs_total comes from job batch.
type jobbed struct{ fakeHistory }

func (jobbed) StatusConfig(context.Context) (string, error) {
	return "global:\n  scrape_interval: 30s\nscrape_configs:\n  - job_name: batch\n    scrape_interval: 2m\n  - job_name: api\n    scrape_interval: 15s\n", nil
}

func (h jobbed) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if expr == "count by (job) (batch_runs_total)" {
		return []prom.Series{{Labels: map[string]string{"job": "batch"}, Samples: []prom.Sample{{T: end, V: 1}}}}, nil
	}
	return h.fakeHistory.QueryRange(ctx, expr, start, end, step)
}

// lapilli verdict --check-content counts a range over a selector that names
// no job with the interval of the jobs its series come from, as asked.
func TestCheckContentJobsOf(t *testing.T) {
	alone(t)
	newHistory = func(context.Context, *verdictFlags) (history, error) { return jobbed{}, nil }
	now = func() time.Time { return t0 }
	rulesFile := write(t, "rules.yaml", `groups:
  - name: batch
    rules:
      - alert: BatchStalled
        expr: rate(batch_runs_total[3m]) == 0
        for: 10m
        labels: {severity: page}
        annotations: {summary: "Batch stalled", runbook_url: "https://runbooks.example/batch"}
`)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verdict", "--rules", rulesFile, "--check-content", "--prometheus", "http://x"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `rate[3m] < 2 × 2m (job "batch")`) {
		t.Errorf("not counted by its series' job:\n%s", stdout.String())
	}
}
