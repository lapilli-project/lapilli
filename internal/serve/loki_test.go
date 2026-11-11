package serve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// lokiLogs answers LogQL: the checkout's errors, counted in its logs, the
// hour INC-1 was.
type lokiLogs struct {
	sync.Mutex
	asked []string
}

func (l *lokiLogs) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	l.Lock()
	l.asked = append(l.asked, expr)
	l.Unlock()
	if expr != `sum by (service) (count_over_time({app="checkout"} |= "error" [5m])) > 5` {
		return nil, nil
	}
	s := prom.Series{Labels: map[string]string{"service": "checkout"}}
	for t := now.Add(-30 * time.Hour); !t.After(now.Add(-29 * time.Hour)); t = t.Add(step) {
		if !t.Before(start) && !t.After(end) {
			s.Samples = append(s.Samples, prom.Sample{T: t, V: 9})
		}
	}
	return []prom.Series{s}, nil
}

// TestServeLokiRules: a Loki ruler's rule files, beside the Prometheus
// rules, are recorded in a version as a Loki's, replayed against Loki, and
// judged as any rule: the errors in the checkout's logs caught INC-1.
func TestServeLokiRules(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "loki")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "logs.yaml"), []byte(`groups:
  - name: logs
    rules:
      - alert: CheckoutLogErrors
        expr: sum by (service) (count_over_time({app="checkout"} |= "error" [5m])) > 5
        for: 2m
`), 0o644)
	logs := &lokiLogs{}
	s, _ := server(t, func(c *Config) { c.Loki, c.LokiRules = "http://loki:3100", dir })
	base := s.h
	s.h = replay.WithLogs(base, func(r rules.Rule) (replay.History, error) { return logs, nil })
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	v, ok, err := s.l.Latest(ctx)
	if err != nil || !ok {
		t.Fatalf("%v %v %+v", ok, err, v)
	}
	full, err := s.l.VersionByID(ctx, v.ID)
	if err != nil || len(full.Docs) != 2 {
		t.Fatalf("%v %+v", err, full)
	}
	var lokiDocs int
	for _, d := range full.Docs {
		if d.Logs == rules.LokiRuler {
			lokiDocs++
		}
	}
	if lokiDocs != 1 {
		t.Errorf("the ledger kept %d documents as a Loki's, want 1: %+v", lokiDocs, full.Docs)
	}
	_, body, _ := get(t, s.Handler(), "/api/v1/report?from=2026-10-31T00:00:00Z&to=2026-11-03T12:00:00Z")
	var rep verdict.Report
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatal(err)
	}
	var got *verdict.RuleResult
	for i := range rep.Rules {
		if rep.Rules[i].Rule == "CheckoutLogErrors" {
			got = &rep.Rules[i]
		}
	}
	if got == nil || got.Firings != 1 || got.White != 1 || got.Unsupported != "" {
		t.Errorf("CheckoutLogErrors: %+v\n%s", got, body)
	}
	logs.Lock()
	n := len(logs.asked)
	logs.Unlock()
	if n == 0 {
		t.Error("Loki wasn't asked")
	}
	if strings.Contains(body, `"unsupported"`) {
		t.Errorf("a rule wasn't replayed:\n%s", body)
	}
	// Without --loki, the same rules are listed, not replayed.
	s.cfg.Loki = ""
	s.h = base
	rs, err := s.parse(ctx, full)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Name == "CheckoutLogErrors" && !strings.Contains(r.Unsupported, "give --loki") {
			t.Errorf("without --loki: %q", r.Unsupported)
		}
	}
}
