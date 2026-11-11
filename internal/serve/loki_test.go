package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/sse"
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

// TestLokiIdentity: a Loki ruler's rule and a Mimir rule of one namespace,
// group, name and labels are two rules from one version to the next; a
// Mimir rule's identity, which names in an existing ledger hang on, is as
// it was.
func TestLokiIdentity(t *testing.T) {
	mimir := rules.Rule{Name: "HighErrors", Group: "api", Namespace: "prod", Labels: map[string]string{"severity": "page"}}
	loki := mimir
	loki.Logs = rules.LokiRuler
	if identity(mimir) == identity(loki) {
		t.Errorf("one identity: %s", identity(mimir))
	}
	if identity(mimir) != fmt.Sprintf("%q %q %q %q %q", "api", "HighErrors", sse.LabelKey(mimir.Labels), "", "prod") {
		t.Errorf("a Mimir rule's identity changed: %s", identity(mimir))
	}
}

// TestLokiProposalForAPerson: serve reads a Mimir's rules with --ruler,
// changes in its namespace shop allowed, and a Loki's with
// --loki-loaded-rules, one of them in a namespace also called shop. A
// longer for proposed for the Loki rule is backtested against Loki, and is
// for a person: nothing is written to the Mimir, whose namespace has the
// same name, nor to Loki.
func TestLokiProposalForAPerson(t *testing.T) {
	ctx := context.Background()
	lokiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loki/api/v1/rules":
			fmt.Fprint(w, "shop:\n    - name: api\n      rules:\n        - alert: CheckoutLogErrors\n          expr: sum by (service) (count_over_time({app=\"checkout\"} |= \"error\" [5m])) > 5\n          for: 2m\n")
		case "/prometheus/api/v1/rules":
			fmt.Fprint(w, `{"status":"success","data":{"groups":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer lokiSrv.Close()
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	s := mimirServer(t, m, func(c *Config) { c.Loki, c.LokiLoaded = lokiSrv.URL, true })
	logs := &lokiLogs{}
	s.h = replay.WithLogs(s.h, func(rules.Rule) (replay.History, error) { return logs, nil })
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	before := len(m.written())
	id := proposeRule(t, h, "CheckoutLogErrors", "for", url.Values{"for": {"15m"}})
	p := state(t, s, id)
	ev := proposalEvidence(p)
	if p.Path != ledger.PathNone || ev.NoPath != noPathLoki || ev.Backtest == nil {
		t.Errorf("path %s, why %q, backtest %v", p.Path, ev.NoPath, ev.Backtest != nil)
	}
	if len(m.written()) != before {
		t.Errorf("the Mimir was written to: %v", m.written()[before:])
	}
	// Nor is it taken for the Mimir's anywhere else, its namespace alike.
	f := found{rule: rules.Rule{Name: "CheckoutLogErrors", Namespace: "shop", Logs: rules.LokiRuler},
		doc: rules.Doc{Namespace: "shop", Logs: rules.LokiRuler}}
	if s.rulerKept(f) {
		t.Error("a Loki ruler's rule is taken as --ruler's")
	}
	logs.Lock()
	asked := len(logs.asked)
	logs.Unlock()
	if asked == 0 {
		t.Error("the backtest didn't ask Loki")
	}
}
