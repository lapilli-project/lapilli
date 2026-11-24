package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/prom"
)

// jobsOfHistory is Prometheus 3.5.0's configuration and targets, and
// answers count by (job) of batch_runs_total with job batch; refused
// while refuse is set.
type jobsOfHistory struct {
	*targetsHistory
	refuse    bool
	jobsAsked int
}

func (h *jobsOfHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if !strings.HasPrefix(expr, "count by (job) (") {
		return h.targetsHistory.QueryRange(ctx, expr, start, end, step)
	}
	h.jobsAsked++
	if h.refuse {
		return nil, errors.New("refused")
	}
	if expr != "count by (job) (batch_runs_total)" {
		return nil, nil
	}
	return []prom.Series{{Labels: map[string]string{"job": "batch"}, Samples: []prom.Sample{{T: end, V: 3}}}}, nil
}

// A range over a selector that names no job counts with the interval of
// the jobs its series come from, asked once a day; refused, as before,
// and asked again an hour later.
func TestContentJobsOf(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, dir := server(t, func(c *Config) { c.CheckContent = true })
	h := &jobsOfHistory{targetsHistory: &targetsHistory{history: &history{}}, refuse: true}
	s.h = h
	os.WriteFile(filepath.Join(dir, "jobs.yaml"), []byte(`groups:
  - name: jobs
    rules:
      - alert: BatchStalled
        expr: rate(batch_runs_total[3m]) == 0
        for: 10m
        labels: {severity: page}
        annotations: {summary: "Batch stalled", runbook_url: "https://runbooks.example/batch"}
`), 0o644)
	short := func() string {
		cf, err := s.contentFindings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range cf["BatchStalled"] {
			if f.Kind == content.ShortRange {
				return f.Detail
			}
		}
		return ""
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	// Refused: counted with the global 30s, as before.
	if got := short(); got != "" || h.jobsAsked != 1 {
		t.Fatalf("refused: %q, asked %d", got, h.jobsAsked)
	}
	h.refuse = false
	if err := s.look(ctx, &raw); err != nil || h.jobsAsked != 1 {
		t.Fatalf("asked again within the hour: %d %v", h.jobsAsked, err)
	}
	s.now = func() time.Time { return now.Add(time.Hour) }
	if err := s.look(ctx, &raw); err != nil || h.jobsAsked != 2 {
		t.Fatalf("an hour later: %d %v", h.jobsAsked, err)
	}
	if got := short(); got != `rate[3m] < 2 × 2m (job "batch")` {
		t.Errorf("by its series' job: %q", got)
	}
	// Known now: not asked again within the day.
	s.now = func() time.Time { return now.Add(3 * time.Hour) }
	if err := s.look(ctx, &raw); err != nil || h.jobsAsked != 2 {
		t.Errorf("asked again within the day: %d %v", h.jobsAsked, err)
	}
}
