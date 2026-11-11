package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// fakeGrafana answers as a real Grafana 13.3.1 did
// (internal/content/testdata/grafana-13.3.1), counting what it is asked,
// and refuses the notifications while refuse is set.
func fakeGrafana(t *testing.T, refuse *bool) (*httptest.Server, *int) {
	t.Helper()
	asked := new(int)
	dir := "../content/testdata/grafana-13.3.1/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file := map[string]string{"/api/v1/provisioning/alert-rules/export": "alert-rules.yaml", "/api/v1/provisioning/templates": "templates.json",
			"/api/v1/provisioning/contact-points": "contact-points.json", "/api/v1/provisioning/policies": "policies.json"}[r.URL.Path]
		if r.URL.Path == "/api/v1/provisioning/templates" {
			*asked++
			if refuse != nil && *refuse {
				http.Error(w, `{"message": "Access denied"}`, http.StatusForbidden)
				return
			}
		}
		b, err := os.ReadFile(dir + file)
		if file == "" || err != nil {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, asked
}

// With --grafana, the templates a Grafana rule's notifications are
// written with are checked beside it, read once an hour; refused, the
// check goes on without them.
func TestContentNotifyTemplates(t *testing.T) {
	ctx := context.Background()
	refuse := true
	g, asked := fakeGrafana(t, &refuse)
	s, _ := server(t, func(c *Config) { c.RulesPath, c.Grafana, c.CheckContent = "", g.URL, true })
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	has := func(rule string) bool {
		cf, err := s.contentFindings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range cf[rule] {
			if f.Kind == content.NotifyNoText && f.Detail == `"shop.message": summary` {
				return true
			}
		}
		return false
	}
	if has("PayLatency") || *asked != 1 {
		t.Fatalf("refused: asked %d", *asked)
	}
	refuse = false
	if err := s.look(ctx, &raw); err != nil || *asked != 1 {
		t.Fatalf("asked again within the hour: %d %v", *asked, err)
	}
	s.now = func() time.Time { return now.Add(time.Hour) }
	if err := s.look(ctx, &raw); err != nil || *asked != 2 {
		t.Fatalf("an hour later: %d %v", *asked, err)
	}
	if !has("PayLatency") || has("ShopErrors") {
		t.Error("PayLatency's notifications show a summary it hasn't; ShopErrors has one")
	}
	_, body, _ := get(t, s.Handler(), "/rules/PayLatency")
	if !strings.Contains(body, "a notification template shows an annotation the rule hasn&#39;t") || !strings.Contains(body, `&#34;shop.message&#34;: summary`) {
		t.Errorf("the rule's page:\n%s", body)
	}
}

// A rule whose notification templates weren't checked, its routing given
// up on, says so on its page, and why; the log says how many, once a
// check.
func TestContentNotifyUnchecked(t *testing.T) {
	ctx := context.Background()
	dir := "../content/testdata/grafana-13.3.1/"
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file := map[string]string{"/api/v1/provisioning/alert-rules/export": "alert-rules.yaml", "/api/v1/provisioning/templates": "templates.json",
			"/api/v1/provisioning/contact-points": "review/contact-points-1697.json", "/api/v1/provisioning/policies": "review/policies-1697.json"}[r.URL.Path]
		b, err := os.ReadFile(dir + file)
		if file == "" || err != nil {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer g.Close()
	s, _ := server(t, func(c *Config) { c.RulesPath, c.Grafana, c.CheckContent = "", g.URL, true })
	var logged bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.contentFindings(ctx); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logged.String(), "notification templates weren't checked"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, logged.String())
	}
	_, body, _ := get(t, s.Handler(), "/rules/ShopErrors")
	if !strings.Contains(body, "Its notification templates weren&#39;t checked: routing it would take more work than a rule may.") {
		t.Errorf("the rule's page:\n%s", body)
	}
}
