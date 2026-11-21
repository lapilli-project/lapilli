package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/source"
)

// ordersGrafana stands in for a Grafana 12.4 holding Orders slow, whose
// condition reads a MySQL data source, which Lapilli can't replay, and
// keeping its state history in annotations: Orders slow fired for the
// checkout from 06:00 to 06:30 the day before now, which caught INC-1,
// and has been on for the orders since 07:00; once ended, it ended ten
// minutes after now.
type ordersGrafana struct {
	mu    sync.Mutex
	ended bool
	asked []string
	// title is the rule's, Orders slow when "".
	title string
}

func (g *ordersGrafana) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, r.URL.Path+" "+r.Header.Get("Authorization"))
	switch r.URL.Path {
	case "/api/v1/provisioning/alert-rules/export":
		title := g.title
		if title == "" {
			title = "Orders slow"
		}
		rule := map[string]any{"uid": "orders", "title": title, "condition": "B", "for": "0s", "labels": map[string]any{"severity": "page"},
			"data": []any{
				map[string]any{"refId": "A", "datasourceUid": "orders", "relativeTimeRange": map[string]any{"from": 600, "to": 0}, "model": map[string]any{"refId": "A", "rawSql": "SELECT 1"}},
				map[string]any{"refId": "B", "datasourceUid": "__expr__", "model": map[string]any{"refId": "B", "type": "threshold", "expression": "A",
					"conditions": []any{map[string]any{"evaluator": map[string]any{"type": "gt", "params": []any{5}}}}}}}}
		json.NewEncoder(w).Encode(map[string]any{"apiVersion": 1, "groups": []any{map[string]any{"orgId": 1, "name": "shop", "folder": "Ops", "interval": "1m", "rules": []any{rule}}}})
	case "/api/datasources/uid/orders":
		io.WriteString(w, `{"uid": "orders", "type": "mysql", "jsonData": {}}`)
	case "/api/frontend/settings":
		io.WriteString(w, annotationsSettings)
	case "/api/annotations":
		var from, to, limit int64
		fmt.Sscan(r.URL.Query().Get("from"), &from)
		fmt.Sscan(r.URL.Query().Get("to"), &to)
		fmt.Sscan(r.URL.Query().Get("limit"), &limit)
		note := func(id int, prev, next, service string, at time.Time) map[string]any {
			return map[string]any{"id": id, "alertName": "Orders slow", "prevState": prev, "newState": next, "time": at.UnixMilli(),
				"tags": []string{"alertname:Orders slow", "grafana_folder:Ops", "service:" + service, "severity:page"}}
		}
		all := []map[string]any{ // newest first
			note(3, "Normal", "Alerting", "orders", now.Add(-5*time.Hour)),
			note(2, "Alerting", "Normal", "checkout", now.Add(-30*time.Hour+30*time.Minute)),
			note(1, "Normal", "Alerting", "checkout", now.Add(-30*time.Hour)),
		}
		if g.ended {
			all = append([]map[string]any{note(4, "Alerting", "Normal", "orders", now.Add(10*time.Minute))}, all...)
		}
		out := []map[string]any{}
		for _, n := range all {
			if ms := n["time"].(int64); ms >= from && ms <= to && int64(len(out)) < limit {
				out = append(out, n)
			}
		}
		json.NewEncoder(w).Encode(out)
	default:
		http.NotFound(w, r)
	}
}

// ordersServer is a server reading the rules of g, a Grafana, and judging
// those it can't replay from its history.
func ordersServer(t *testing.T, g *ordersGrafana) *Server {
	t.Helper()
	ts := httptest.NewServer(g)
	t.Cleanup(ts.Close)
	tok := tokenFile(t, "glsa_viewer")
	s, _ := server(t, func(c *Config) {
		c.RulesPath, c.Grafana, c.GrafanaToken = "", ts.URL, tok
		c.JudgedHistory = &source.GrafanaHistory{URL: ts.URL, TokenFile: tok, Agent: "test"}
		c.IncidentsListen, c.FiringsToken = "127.0.0.1:0", tokenFile(t, firingsTok)
	})
	raw := ""
	if err := s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	return s
}

// A rule --grafana holds that Lapilli can't replay is judged from what
// Grafana's own history says it fired, copied in each round under the
// source grafana, as one rule with its history (docs/design/grafanajudged.md):
// its stones, the incident it caught, its page saying so. One on since
// before a copy that the history then says ended is ended. No job may
// write to the source.
func TestServeGrafanaJudged(t *testing.T) {
	ctx := context.Background()
	g := &ordersGrafana{}
	s := ordersServer(t, g)
	s.jobs(ctx, true)
	if s.status.errWhat != "" {
		t.Fatalf("a round failed: %s: %s", s.status.errWhat, s.status.err)
	}
	table, err := s.l.RuleTable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range table {
		names = append(names, r.Name+" <"+r.Source+">")
	}
	if !slices.Equal(names, []string{"Orders slow <grafana>"}) {
		t.Errorf("rules: %q", names)
	}
	open, err := s.l.OpenFirings(ctx, ledger.GrafanaSource, now.Add(time.Hour))
	if err != nil || len(open) != 1 || !open[0].Start.Equal(now.Add(-5*time.Hour)) {
		t.Errorf("open: %+v %v", open, err)
	}
	o, _ := s.options(ctx)
	rep, err := s.l.Report(ctx, now.Add(-7*24*time.Hour), now.Add(time.Hour), o, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rules) != 1 || rep.Rules[0].Firings != 2 || rep.Rules[0].White != 1 || !rep.Rules[0].ByHistory() {
		t.Errorf("report: %+v", rep.Rules)
	}
	for _, a := range g.asked {
		if !strings.HasSuffix(a, " Bearer glsa_viewer") && !strings.HasPrefix(a, "/api/v1/provisioning/") {
			t.Errorf("asked %s", a)
		}
	}
	s.ready.Store(true)
	_, body, _ := get(t, s.Handler(), "/rules/"+url.PathEscape("Orders slow"))
	if !strings.Contains(body, "A Grafana rule Lapilli can&#39;t replay: what Grafana&#39;s own alert state history says it fired, copied in from") {
		t.Errorf("the rule's page doesn't say it is judged from Grafana's history")
	}

	// It ends, ten minutes after now: the next round ends the firing on
	// since 07:00, written again from its start.
	g.mu.Lock()
	g.ended = true
	g.mu.Unlock()
	s.now = func() time.Time { return now.Add(2 * time.Hour) }
	s.jobs(ctx, true)
	if open, err := s.l.OpenFirings(ctx, ledger.GrafanaSource, now.Add(3*time.Hour)); err != nil || len(open) != 0 {
		t.Errorf("still open: %+v %v", open, err)
	}
	fs, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, now.Add(-6*time.Hour), now.Add(3*time.Hour))
	if err != nil || len(fs) != 1 || !fs[0].End.Equal(now.Add(10*time.Minute)) {
		t.Errorf("ended: %+v %v", fs, err)
	}

	// No job writes over it.
	r := httptest.NewRequest(http.MethodPut, "/api/v1/firings/grafana?from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer "+firingsTok)
	w := httptest.NewRecorder()
	s.IncidentsHandler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "copies Grafana's own alert state history in under") {
		t.Errorf("a job wrote to grafana: %d %s", w.Code, w.Body.String())
	}
}

// An alert of a rule Lapilli can't replay flipping between Alerting and
// NoData every 500ms for two hours, with 128 labels: thousands of firings,
// within every bound of a read, none of which a write holds. Each is
// passed over before anything is made of its labels, and the round goes
// on.
func TestServeGrafanaJudgedFlipping(t *testing.T) {
	ctx := context.Background()
	g := &ordersGrafana{title: gErrors}
	s := ordersServer(t, g)
	f := httptest.NewServer(&flipping{settings: annotationsSettings})
	defer f.Close()
	h := *s.cfg.JudgedHistory
	h.URL = f.URL
	s.cfg.JudgedHistory = &h
	var keyed, seen atomic.Int64
	defer func(k func(firing.Firing) [32]byte) { alertOf = k }(alertOf)
	alertOf = func(f firing.Firing) [32]byte {
		keyed.Add(1)
		return [32]byte{}
	}
	defer func(r func(firing.Firing) error) { recordable = r }(recordable)
	recordable = func(f firing.Firing) error {
		seen.Add(1)
		return ledger.Recordable(f)
	}
	s.jobs(ctx, true)
	if s.status.errWhat != "" {
		t.Fatalf("a round failed: %s: %s", s.status.errWhat, s.status.err)
	}
	if n, k := seen.Load(), keyed.Load(); n < 14000 || k != 0 {
		t.Errorf("%d firings read, %d of them made into keys", n, k)
	}
	if fs, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, now.Add(-7*24*time.Hour), now.Add(time.Hour)); err != nil || len(fs) != 0 {
		t.Errorf("copied in: %d %v", len(fs), err)
	}
	if _, to, _ := s.l.Coverage(ctx, ledger.GrafanaSource); !to.Equal(now) {
		t.Errorf("copied up to %s", to)
	}
}

// A day the history holds more of than is read at once is passed over,
// counted by its changes (one here), its firings left out and logged, and
// the copy goes on from the day after; the round doesn't fail for it.
func TestServeGrafanaJudgedTooMany(t *testing.T) {
	ctx := context.Background()
	was := source.MaxChanges
	t.Cleanup(func() { source.MaxChanges = was })
	source.MaxChanges = 1
	s := ordersServer(t, &ordersGrafana{})
	s.jobs(ctx, true)
	if s.status.errWhat != "" {
		t.Fatalf("a round failed: %s: %s", s.status.errWhat, s.status.err)
	}
	if fs, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, now.Add(-7*24*time.Hour), now.Add(time.Hour)); err != nil || len(fs) != 0 {
		t.Errorf("copied in past the bounds: %+v %v", fs, err)
	}
	s.status.Lock()
	from := s.status.copyFrom
	s.status.Unlock()
	if !from.Equal(now) {
		t.Errorf("goes on from %s", from)
	}
	// Within the bounds again, it goes on from there: the alert on since
	// 07:00 is seen from then.
	source.MaxChanges = was
	s.now = func() time.Time { return now.Add(time.Hour) }
	s.jobs(ctx, true)
	fs, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, now.Add(-7*24*time.Hour), now.Add(2*time.Hour))
	if err != nil || len(fs) != 1 || !fs[0].Start.Equal(now) || !fs[0].End.IsZero() || fs[0].Labels["service"] != "orders" {
		t.Errorf("copied in: %+v %v", fs, err)
	}
}
