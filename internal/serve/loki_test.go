package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		if d.Logs == rules.LokiFile {
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

// TestServeLokiRulesAlone: lapilli serve with a Loki ruler's rules as its
// only rules (and --prometheus, for what really fired) reads them and
// replays them; replaying() didn't count them, so they were never read.
func TestServeLokiRulesAlone(t *testing.T) {
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
	s, _ := server(t, func(c *Config) { c.RulesPath, c.Impact = "", ""; c.Loki, c.LokiRules = "http://loki:3100", dir })
	s.h = replay.WithLogs(s.h, func(r rules.Rule) (replay.History, error) { return logs, nil })
	if !s.replaying() {
		t.Error("lapilli serve --loki URL --loki-rules DIR: replaying() is false, so the rules are never read")
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if _, ok, _ := s.l.Latest(ctx); !ok {
		t.Error("no version of the rules kept")
	}
}

// TestCompareKeepsLoki: compare (MCP) reads the rules given as a Prometheus
// rule file; a Loki ruler's rule given back changed is still Loki's, in its
// namespace, its LogQL asked of Loki, not of Prometheus, and run without
// the keep_firing_for Loki doesn't run.
func TestCompareKeepsLoki(t *testing.T) {
	expr := `sum(count_over_time({app="shop"} |= "error" [1m])) > 10`
	current := []rules.Rule{{Name: "ShopLogErrors", Group: "shop", Expr: expr, Logs: rules.LokiRuler, Source: "loki/fake", Namespace: "fake"}}
	given, err := rules.Parse([]byte("groups:\n  - name: shop\n    rules:\n      - alert: ShopLogErrors\n        expr: sum(count_over_time({app=\"shop\"} |= \"error\" [1m])) > 5\n        keep_firing_for: 5m\n"))
	if err != nil {
		t.Fatal(err)
	}
	is, err := proposed(current, given, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(is) != 1 || is[0].Logs != rules.LokiRuler || is[0].Namespace != "fake" || is[0].KeepFiringFor != 0 {
		t.Errorf("the proposed rule in place of a Loki ruler's: %+v: its LogQL goes to Prometheus", is[0])
	}
}

// TestLokiLoadedProposal: a rule read with --loki-loaded-rules comes from a
// ruler's
// configuration API, a document a namespace (API false, Namespace set).
// With --ruler, rulerKept takes it as --ruler's: its proposal reads the
// Mimir's group of that namespace and name and is refused, "the rule
// changed since it was read: try again in a minute", for ever. Without
// --ruler, it falls through to the rule-file path: a git change to a
// "file" that is Loki's API. docs/design/loki.md says a proposal for a
// Loki rule is for a person.
func TestLokiLoadedProposal(t *testing.T) {
	loki := http.NewServeMux()
	loki.HandleFunc("/loki/api/v1/rules", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "shop:\n  - name: api\n    interval: 1m\n    rules:\n      - alert: LogErrors\n        expr: sum(count_over_time({app=\"shop\"} |= \"error\" [5m])) > 9\n        for: 5m\n")
	})
	loki.HandleFunc("/prometheus/api/v1/rules", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"success","data":{"groups":[]}}`)
	})
	lts := httptest.NewServer(loki)
	defer lts.Close()
	ctx := context.Background()
	propose := func(t *testing.T, s *Server) {
		t.Helper()
		s.h = replay.WithLogs(s.h, func(r rules.Rule) (replay.History, error) { return &lokiLogs{}, nil })
		if err := s.look(ctx, new(string)); err != nil {
			t.Fatal(err)
		}
		s.jobs(ctx, true)
		f, err := s.current(ctx, "LogErrors", true)
		if err != nil || f.rule.Logs != rules.LokiRuler {
			t.Fatalf("%v %+v", err, f.rule)
		}
		code, body, hd := ui(t, s.Handler(), http.MethodPost, "/proposals", bo, "same-origin",
			url.Values{"rule": {"LogErrors"}, "kind": {"for"}, "for": {"15m"}})
		if code != http.StatusSeeOther {
			t.Errorf("proposing a for for a Loki ruler's rule: %d %s", code, zzMsg(body))
			return
		}
		if p := state(t, s, locID(hd)); p.Path != ledger.PathNone {
			t.Errorf("a Loki ruler's rule's proposal: path %q, source %s, want %q (for a person)", p.Path, p.Source, ledger.PathNone)
		}
	}
	t.Run("with --ruler", func(t *testing.T) {
		m := mimirRuler(t, "/prometheus/config/v1/rules")
		s := mimirServer(t, m, func(c *Config) { c.Loki, c.LokiLoaded = lts.URL, true })
		f, err := s.current(ctx, "LogErrors", false)
		if err == nil && s.rulerKept(f) {
			t.Errorf("rulerKept: a Loki ruler's rule is taken as --ruler's (%s)", f.doc.Source)
		}
		propose(t, s)
	})
	t.Run("without --ruler", func(t *testing.T) {
		s, _ := changesServer(t, func(c *Config) { c.Loki, c.LokiLoaded = lts.URL, true })
		propose(t, s)
	})
}
