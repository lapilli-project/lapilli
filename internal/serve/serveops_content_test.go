package serve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lapilli.dev/internal/content"
)

// targetsHistory serves what a real Prometheus 3.5.0 said of its
// configuration and its targets (internal/content/testdata).
type targetsHistory struct {
	*history
	asked int
}

func (h *targetsHistory) StatusConfig(context.Context) (string, error) {
	b, err := os.ReadFile("../content/testdata/prometheus-3.5.0/status-config.json")
	if err != nil {
		return "", err
	}
	var a struct {
		Data struct{ YAML string } `json:"data"`
	}
	return a.Data.YAML, json.Unmarshal(b, &a)
}

func (h *targetsHistory) Targets(context.Context) ([]byte, error) {
	h.asked++
	return os.ReadFile("../content/testdata/prometheus-3.5.0/targets-active.json")
}

const jobsFile = `groups:
  - name: jobs
    rules:
      - alert: BatchStalled
        expr: rate(batch_runs_total{job="batch"}[3m]) == 0
        for: 10m
        labels: {severity: page}
        annotations: {summary: "Batch stalled", runbook_url: "https://runbooks.example/batch"}
      - alert: CheckoutErrors
        expr: rate(http_requests_total{job="checkout", code=~"5.."}[90s]) > 1
        for: 5m
        labels: {severity: page}
        annotations: {summary: "Checkout errors", runbook_url: "https://runbooks.example/checkout"}
      - alert: PrometheusBusy
        expr: rate(prometheus_http_requests_total{job="prometheus"}[45s]) > 100
        for: 5m
        labels: {severity: page}
        annotations: {summary: "Busy", runbook_url: "https://runbooks.example/busy"}
`

// The range check counts each rule's range with its job's own interval, as
// Prometheus's configuration and targets give them, and says whose.
func TestContentByJob(t *testing.T) {
	ctx := context.Background()
	s, dir := server(t, func(c *Config) { c.CheckContent = true })
	th := &targetsHistory{history: &history{}}
	s.h = th
	os.WriteFile(filepath.Join(dir, "jobs.yaml"), []byte(jobsFile), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if sc := s.scrapes(); sc.Jobs["checkout"] == 0 || th.asked != 1 {
		t.Fatalf("the jobs read: %v, targets asked %d", sc, th.asked)
	}
	cf, err := s.contentFindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	short := func(rule string) string {
		for _, f := range cf[rule] {
			if f.Kind == content.ShortRange {
				return f.Detail
			}
		}
		return ""
	}
	for rule, want := range map[string]string{
		"BatchStalled":   `rate[3m] < 2 × 2m (job "batch")`,
		"CheckoutErrors": `rate[90s] < 2 × 1m (job "checkout")`,
		"PrometheusBusy": "", // 45s holds three of its 15s scrapes, though not two of the global 30s
	} {
		if got := short(rule); got != want {
			t.Errorf("%s: %q, want %q", rule, got, want)
		}
	}
	_, body, _ := get(t, s.Handler(), "/rules/CheckoutErrors")
	if !strings.Contains(body, `rate[90s] &lt; 2 × 1m (job &#34;checkout&#34;)`) {
		t.Errorf("the rule's page:\n%s", body)
	}
}
