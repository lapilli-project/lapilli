package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/source"
)

// grafanaRule is a Grafana rule replayed against grafanaHistory: errors
// above 5, every minute.
const grafanaRule = `apiVersion: 1
groups:
  - orgId: 1
    name: shop
    folder: Ops
    interval: 1m
    rules:
      - uid: gerrors
        title: GErrors
        condition: B
        noDataState: OK
        for: 0s
        data:
          - {refId: A, datasourceUid: prom, relativeTimeRange: {from: 600, to: 0}, model: {refId: A, expr: errors, instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {refId: B, type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [5]}}]}
`

// grafanaHistory answers the query of grafanaRule with errors at 10 while
// history has "errors > 5" true: checkout for an hour 30h ago, search ten
// minutes in each of the twenty hours before. GRAFANA_ALERTS it keeps
// empty.
type grafanaHistory struct{ history }

func (h *grafanaHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if !strings.Contains(expr, "errors") {
		return nil, nil
	}
	ss, err := h.history.QueryRange(ctx, "errors > 5", start, end, step)
	for i := range ss {
		for j := range ss[i].Samples {
			ss[i].Samples[j].V = 10
		}
	}
	return ss, err
}

// stateHistory is a Grafana keeping its alert state history in
// annotations, as Grafana 12.4 and 13.3 answer for it: GErrors as it really
// fired, the checkout alert as replayed, fifteen of the twenty search
// alerts, one for cart the replay doesn't have; a Watchdog no rule given
// is named, and a name that doesn't show.
type stateHistory struct {
	mu       sync.Mutex
	settings string
	none     bool
	asked    []string
}

func (g *stateHistory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, r.URL.Path+" "+r.Header.Get("Authorization"))
	if r.URL.Path == "/api/frontend/settings" {
		fmt.Fprint(w, g.settings)
		return
	}
	var from, to int64
	fmt.Sscan(r.URL.Query().Get("from"), &from)
	fmt.Sscan(r.URL.Query().Get("to"), &to)
	type note struct {
		ID        int64    `json:"id"`
		AlertName string   `json:"alertName"`
		PrevState string   `json:"prevState"`
		NewState  string   `json:"newState"`
		Time      int64    `json:"time"`
		Tags      []string `json:"tags"`
	}
	var all []note
	fired := func(rule string, tags []string, a, b time.Time) {
		tags = append([]string{"alertname:" + rule, "grafana_folder:Ops"}, tags...)
		all = append(all, note{int64(len(all) + 1), rule, "Normal", "Alerting", a.UnixMilli(), tags},
			note{int64(len(all) + 2), rule, "Alerting", "Normal", b.UnixMilli(), tags})
	}
	if !g.none {
		fired("GErrors", []string{"service:checkout"}, now.Add(-30*time.Hour), now.Add(-29*time.Hour+time.Minute))
		for k := 0; k < 15; k++ {
			at := now.Add(-50*time.Hour + time.Duration(k)*time.Hour)
			fired("GErrors", []string{"service:search"}, at, at.Add(11*time.Minute))
		}
		fired("GErrors", []string{"service:cart", "note:" + strings.Repeat("\u202e", 2) + strings.Repeat("x", 300)}, now.Add(-40*time.Hour), now.Add(-39*time.Hour))
		fired("Watchdog", nil, now.Add(-45*time.Hour), now.Add(-45*time.Hour+5*time.Minute))
		fired("Watch‮dog", nil, now.Add(-44*time.Hour), now.Add(-44*time.Hour+5*time.Minute))
	}
	var out []note
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Time >= from && all[i].Time <= to {
			out = append(out, all[i])
		}
	}
	// Newest first, as Grafana answers.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Time > out[j-1].Time; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	json.NewEncoder(w).Encode(out)
}

func historyServer(t *testing.T, g *stateHistory) *Server {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("glsa_viewer"), 0o600)
	s, rules := server(t, func(c *Config) {
		c.CheckReplay = true
		c.History = &source.GrafanaHistory{URL: srv.URL, TokenFile: tok, Agent: "test"}
	})
	os.Remove(filepath.Join(rules, "shop.yaml"))
	if err := os.WriteFile(filepath.Join(rules, "grafana.yaml"), []byte(grafanaRule), 0o644); err != nil {
		t.Fatal(err)
	}
	s.h = &grafanaHistory{}
	raw := ""
	if err := s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	return s
}

// Grafana's rules are set beside what Grafana's own state history says
// they fired, where it keeps one it serves, in place of GRAFANA_ALERTS,
// which this Grafana doesn't write: each firing once, what differs, alerts
// no rule is named; the runs page says so.
func TestCheckReplayGrafanaHistory(t *testing.T) {
	g := &stateHistory{settings: `{"unifiedAlerting":{"alertStateHistoryBackend":"annotations"}}`}
	s := historyServer(t, g)
	ctx := context.Background()
	s.jobs(ctx, true)
	if to, err := s.l.CheckedTo(ctx); err != nil || !to.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("checked to %s %v", to, err)
	}
	cs, err := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if c := cs["GErrors"]; c.Replayed != 21 || c.Real != 17 || c.Alike != 16 || c.Off != 0 {
		t.Errorf("GErrors: %+v; want 21 replayed, 17 real, 16 alike, no time apart", c)
	}
	if el, _ := s.l.CheckElsewhere(ctx, now.Add(-7*24*time.Hour), now); el["Watchdog"] != 1 || len(el) != 1 {
		t.Errorf("elsewhere: %v", el)
	}
	// A miss of what really fired is kept as a replayed alert's labels
	// are: a value cut to 256 bytes, with nothing that turns text around.
	misses, err := s.l.CheckMisses(ctx, "GErrors")
	if err != nil {
		t.Fatal(err)
	}
	cart := 0
	for _, m := range misses {
		if m.Way == "real" && m.Labels["service"] == "cart" {
			cart++
			if n := m.Labels["note"]; len(n) > 256 || strings.Contains(n, "\u202e") {
				t.Errorf("kept %q", n)
			}
		}
	}
	if cart != 1 {
		t.Errorf("misses: %+v", misses)
	}
	for _, a := range g.asked {
		if !strings.HasSuffix(a, " Bearer glsa_viewer") {
			t.Errorf("asked %s", a)
		}
	}
	_, body, _ := get(t, s.Handler(), "/runs")
	for _, w := range []string{"Set beside ALERTS{alertstate=&#34;firing&#34;} up to", "its own alert state history, kept in annotations, stands in for GRAFANA_ALERTS"} {
		if !strings.Contains(body, w) {
			t.Errorf("/runs doesn't say %q", w)
		}
	}
	if strings.Contains(body, "GRAFANA_ALERTS{") {
		t.Error("/runs says GRAFANA_ALERTS is read")
	}
}

// A Grafana whose state history holds nothing over a day doesn't check
// its rules that day, rather than every replayed firing looking as if it
// never really happened; one that keeps it in Prometheus alone is checked
// against GRAFANA_ALERTS, as before.
func TestCheckReplayGrafanaHistoryNone(t *testing.T) {
	g := &stateHistory{settings: `{"unifiedAlerting":{"alertStateHistoryBackend":"annotations"}}`, none: true}
	s := historyServer(t, g)
	ctx := context.Background()
	s.jobs(ctx, true)
	if cs, _ := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now); cs["GErrors"].Either() != 0 {
		t.Errorf("checked against nothing: %+v", cs)
	}
	_, body, _ := get(t, s.Handler(), "/runs")
	if !strings.Contains(body, "Grafana&#39;s alert state history held no change of state over 2026-11-02") {
		t.Error("/runs doesn't say the history held nothing")
	}

	g = &stateHistory{settings: `{"unifiedAlerting":{"alertStateHistoryBackend":"prometheus"}}`}
	s = historyServer(t, g)
	s.jobs(ctx, true)
	s.status.Lock()
	empty := s.status.checkEmpty
	s.status.Unlock()
	if len(empty) != 1 || empty[0] != grafanaAlertsSeries {
		t.Errorf("GRAFANA_ALERTS not read: %v", empty)
	}
	for _, a := range g.asked {
		if strings.HasPrefix(a, "/api/annotations") {
			t.Errorf("asked %s of a Grafana keeping its history in Prometheus", a)
		}
	}
}

// More changes of state than are read at once don't check Grafana's rules
// that day, and the check goes on to the next: a busy Grafana can't hold
// the check up for good. The runs page says so, in each language.
func TestCheckReplayGrafanaHistoryTooMany(t *testing.T) {
	defer func(n int) { source.MaxChanges = n }(source.MaxChanges)
	source.MaxChanges = 3
	g := &stateHistory{settings: `{"unifiedAlerting":{"alertStateHistoryBackend":"annotations"}}`}
	s := historyServer(t, g)
	ctx := context.Background()
	s.jobs(ctx, true)
	if to, err := s.l.CheckedTo(ctx); err != nil || !to.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("checked to %s %v", to, err)
	}
	if cs, _ := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now); cs["GErrors"].Either() != 0 {
		t.Errorf("checked against part of the history: %+v", cs)
	}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	h := s.Handler()
	for _, n := range names {
		r := httptest.NewRequest(http.MethodGet, "/runs", nil)
		r.Header.Set("Accept-Language", n.Code)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if n.Code == "en" && !strings.Contains(w.Body.String(), "more changes of state over 2026-11-02 than are read at once") {
			t.Error("/runs doesn't say there were too many")
		}
	}
	// The history's lines when it held nothing, in each language too.
	s.status.Lock()
	s.status.historyOff = historyNothing
	s.status.Unlock()
	for _, n := range names {
		r := httptest.NewRequest(http.MethodGet, "/runs", nil)
		r.Header.Set("Accept-Language", n.Code)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
}
