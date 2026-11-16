package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/source"
)

// grafanaRule is a Grafana rule replayed against grafanaHistory: errors
// above 5, every minute, its title with a colon, as an annotation's tags
// write as an underscore.
const grafanaRule = `apiVersion: 1
groups:
  - orgId: 1
    name: shop
    folder: Ops
    interval: 1m
    rules:
      - uid: gerrors
        title: "GErrors: shop"
        condition: B
        noDataState: OK
        for: 0s
        data:
          - {refId: A, datasourceUid: prom, relativeTimeRange: {from: 600, to: 0}, model: {refId: A, expr: errors, instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {refId: B, type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [5]}}]}
`

// gErrors is the rule's name, and its name in an annotation's tags.
const (
	gErrors       = "GErrors: shop"
	gErrorsTagged = "GErrors_ shop"
)

// grafanaHistory answers the query of grafanaRule with errors at 10 while
// history has "errors > 5" true: checkout for an hour 30h ago, search ten
// minutes in each of the twenty hours before, each with an instance and
// its port; and ALERTS, as alertsHistory keeps it for Errors, the
// Prometheus rule beside it. GRAFANA_ALERTS it keeps empty, unless
// grafanaAlerts.
type grafanaHistory struct {
	history
	alerts        alertsHistory
	grafanaAlerts bool
}

func (h *grafanaHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	switch {
	case expr == alertsSeries:
		return h.alerts.QueryRange(ctx, expr, start, end, step)
	case expr == grafanaAlertsSeries && h.grafanaAlerts:
		ss, err := h.history.QueryRange(ctx, "errors > 5", start, end, step)
		for i := range ss {
			ss[i].Labels["alertname"], ss[i].Labels["alertstate"], ss[i].Labels["instance"] = gErrors, "firing", "10.0.0.1:9100"
		}
		return ss, err
	case expr == "errors > 5":
		return h.history.QueryRange(ctx, expr, start, end, step)
	case !strings.Contains(expr, "errors"):
		return nil, nil
	}
	ss, err := h.history.QueryRange(ctx, "errors > 5", start, end, step)
	for i := range ss {
		ss[i].Labels["instance"] = "10.0.0.1:9100"
		for j := range ss[i].Samples {
			ss[i].Samples[j].V = 10
		}
	}
	return ss, err
}

// stateHistory is a Grafana 12.4 keeping its alert state history in
// annotations, as it answers for it, its labels in their tags as Grafana
// writes them, an instance's port with an underscore: GErrors as it really
// fired, the checkout alert as replayed, fifteen of the twenty search
// alerts, one for cart the replay doesn't have; a Watchdog no rule given
// is named, and a name that doesn't show. With none, nothing; with odd, a
// state Grafana has no name for among GErrors's changes; with refuse, a
// 403 to every annotation; with sameTime, every page full of changes at
// the time asked to, as a big Grafana's one tick can give.
type stateHistory struct {
	mu                          sync.Mutex
	settings                    string
	none, odd, refuse, sameTime bool
	asked                       []string
}

func (g *stateHistory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, r.URL.Path+" "+r.Header.Get("Authorization"))
	if r.URL.Path == "/api/frontend/settings" {
		fmt.Fprint(w, g.settings)
		return
	}
	if g.refuse {
		http.Error(w, `{"message":"You'll need additional permissions to perform this action. Permissions needed: annotations:read"}`, http.StatusForbidden)
		return
	}
	var from, to, limit int64
	fmt.Sscan(r.URL.Query().Get("from"), &from)
	fmt.Sscan(r.URL.Query().Get("to"), &to)
	fmt.Sscan(r.URL.Query().Get("limit"), &limit)
	type note struct {
		ID        int64    `json:"id"`
		AlertName string   `json:"alertName"`
		PrevState string   `json:"prevState"`
		NewState  string   `json:"newState"`
		Time      int64    `json:"time"`
		Tags      []string `json:"tags"`
	}
	var all []note
	if g.sameTime && limit > 1 {
		for i := range limit {
			all = append(all, note{i + 1, gErrors, "Normal", "Alerting", to, []string{"alertname:" + gErrorsTagged, fmt.Sprintf("pod:p%d", i)}})
		}
		json.NewEncoder(w).Encode(all)
		return
	}
	fired := func(rule string, tags []string, a, b time.Time) {
		tags = append([]string{"alertname:" + rule, "grafana_folder:Ops"}, tags...)
		all = append(all, note{int64(len(all) + 1), rule, "Normal", "Alerting", a.UnixMilli(), tags},
			note{int64(len(all) + 2), rule, "Alerting", "Normal", b.UnixMilli(), tags})
	}
	if !g.none {
		inst := "instance:10.0.0.1_9100"
		fired(gErrorsTagged, []string{inst, "service:checkout"}, now.Add(-30*time.Hour), now.Add(-29*time.Hour+time.Minute))
		for k := 0; k < 15; k++ {
			at := now.Add(-50*time.Hour + time.Duration(k)*time.Hour)
			fired(gErrorsTagged, []string{inst, "service:search"}, at, at.Add(11*time.Minute))
		}
		fired(gErrorsTagged, []string{inst, "service:cart", "note:" + strings.Repeat("‮", 2) + strings.Repeat("x", 80)}, now.Add(-40*time.Hour), now.Add(-39*time.Hour))
		fired("Watchdog", nil, now.Add(-45*time.Hour), now.Add(-45*time.Hour+5*time.Minute))
		fired("Watch‮dog", nil, now.Add(-44*time.Hour), now.Add(-44*time.Hour+5*time.Minute))
		// A change long before, so the history reaches back past each day.
		all = append(all, note{int64(len(all) + 1), "Old", "Normal", "Pending", now.Add(-100 * time.Hour).UnixMilli(), []string{"alertname:Old"}})
		if g.odd {
			all = append(all, note{int64(len(all) + 1), gErrors, "Normal", "Firing", now.Add(-72 * time.Hour).UnixMilli(), []string{"alertname:" + gErrorsTagged}})
		}
	}
	var out []note
	for _, n := range all {
		if n.Time >= from && n.Time <= to {
			out = append(out, n)
		}
	}
	// Newest first, as Grafana answers, as many as asked.
	slices.SortStableFunc(out, func(a, b note) int { return int(b.Time - a.Time) })
	if limit > 0 {
		out = out[:min(len(out), int(limit))]
	}
	json.NewEncoder(w).Encode(out)
}

const annotationsSettings = `{"buildInfo":{"version":"12.4.14"},"unifiedAlerting":{"alertStateHistoryBackend":"annotations"}}`

// historyServer is a server checking grafanaRule, beside the Prometheus
// rule Errors when prometheus, against the Grafana g.
func historyServer(t *testing.T, g *stateHistory, prometheus bool, change func(*Config)) (*Server, *grafanaHistory) {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("glsa_viewer"), 0o600)
	s, rules := server(t, func(c *Config) {
		c.CheckReplay = true
		c.History = &source.GrafanaHistory{URL: srv.URL, TokenFile: tok, Agent: "test"}
		if change != nil {
			change(c)
		}
	})
	if !prometheus {
		os.Remove(filepath.Join(rules, "shop.yaml"))
	}
	if err := os.WriteFile(filepath.Join(rules, "grafana.yaml"), []byte(grafanaRule), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &grafanaHistory{}
	s.h = h
	raw := ""
	if err := s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	return s, h
}

// checkedToToday reports the check got to the start of today.
func checkedToToday(t *testing.T, s *Server) {
	t.Helper()
	if to, err := s.l.CheckedTo(context.Background()); err != nil || !to.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("checked to %s %v", to, err)
	}
}

// Grafana's rules are set beside what Grafana's own state history says
// they fired, where it keeps one it serves, in place of GRAFANA_ALERTS,
// which this Grafana doesn't write: each firing once, the replay's labels
// as the annotations' tags hold them (an instance's port, a title's colon),
// what differs, alerts no rule is named; the runs page says so.
func TestCheckReplayGrafanaHistory(t *testing.T) {
	g := &stateHistory{settings: annotationsSettings}
	s, _ := historyServer(t, g, false, nil)
	ctx := context.Background()
	s.jobs(ctx, true)
	checkedToToday(t, s)
	cs, err := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if c := cs[gErrors]; c.Replayed != 21 || c.Real != 17 || c.Alike != 16 || c.Off != 0 {
		t.Errorf("GErrors: %+v; want 21 replayed, 17 real, 16 alike, no time apart", c)
	}
	if el, _ := s.l.CheckElsewhere(ctx, now.Add(-7*24*time.Hour), now); el["Watchdog"] != 1 || len(el) != 1 {
		t.Errorf("elsewhere: %v", el)
	}
	// A miss of what really fired is kept as a replayed alert's labels
	// are: nothing that turns text around.
	misses, err := s.l.CheckMisses(ctx, gErrors)
	if err != nil {
		t.Fatal(err)
	}
	cart := 0
	for _, m := range misses {
		if m.Way == "real" && m.Labels["service"] == "cart" {
			cart++
			if n := m.Labels["note"]; len(n) > 256 || strings.Contains(n, "‮") {
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

// Where Grafana keeps its history is asked once an hour, however many
// days a round checks; --rule leaves out what really fired of the alerts
// it doesn't match, as it does of the series.
func TestCheckReplayGrafanaHistoryAsked(t *testing.T) {
	g := &stateHistory{settings: annotationsSettings}
	s, _ := historyServer(t, g, false, func(c *Config) { c.RuleRE = regexp.MustCompile(`^GErrors`) })
	ctx := context.Background()
	settings := func() int {
		g.mu.Lock()
		defer g.mu.Unlock()
		n := 0
		for _, a := range g.asked {
			if strings.HasPrefix(a, "/api/frontend/settings") {
				n++
			}
		}
		return n
	}
	s.jobs(ctx, true)
	checkedToToday(t, s)
	if el, _ := s.l.CheckElsewhere(ctx, now.Add(-7*24*time.Hour), now); len(el) != 0 {
		t.Errorf("--rule let in: %v", el)
	}
	if n := settings(); n != 1 {
		t.Errorf("settings asked %d times over a round", n)
	}
	// Within the hour, as it said; past it, asked again.
	for _, c := range []struct {
		later time.Duration
		want  int
	}{{30 * time.Minute, 1}, {2 * time.Hour, 2}} {
		s.now = func() time.Time { return now.Add(c.later) }
		if w, err := s.historyWhere(ctx); err != nil || w.Serves != source.HistoryAnnotations {
			t.Fatalf("%+v %v", w, err)
		}
		if n := settings(); n != c.want {
			t.Errorf("%s later: settings asked %d times, want %d", c.later, n, c.want)
		}
	}
}

// A history that holds no change from before a day doesn't check
// Grafana's rules that day, rather than every replayed firing looking as
// if it never really happened; one that keeps it in Prometheus alone is
// checked against GRAFANA_ALERTS, as before.
func TestCheckReplayGrafanaHistoryNone(t *testing.T) {
	g := &stateHistory{settings: annotationsSettings, none: true}
	s, _ := historyServer(t, g, false, nil)
	ctx := context.Background()
	s.jobs(ctx, true)
	checkedToToday(t, s)
	if cs, _ := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now); cs[gErrors].Either() != 0 {
		t.Errorf("checked against nothing: %+v", cs)
	}
	_, body, _ := get(t, s.Handler(), "/runs")
	if !strings.Contains(body, "Grafana&#39;s alert state history holds no change of state from before 2026-11-02") {
		t.Error("/runs doesn't say the history doesn't reach the day")
	}

	g = &stateHistory{settings: `{"unifiedAlerting":{"alertStateHistoryBackend":"prometheus"}}`}
	s, _ = historyServer(t, g, false, nil)
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

// A Grafana keeping its history in several places may keep it in
// Prometheus too: while GRAFANA_ALERTS holds anything, it is read, as it
// was before; where it holds nothing, the history.
func TestCheckReplayGrafanaHistoryMultiple(t *testing.T) {
	multiple := `{"buildInfo":{"version":"12.4.14"},"unifiedAlerting":{"alertStateHistoryBackend":"multiple","alertStateHistoryPrimary":"annotations"}}`
	g := &stateHistory{settings: multiple}
	s, h := historyServer(t, g, false, nil)
	h.grafanaAlerts = true
	ctx := context.Background()
	s.jobs(ctx, true)
	checkedToToday(t, s)
	for _, a := range g.asked {
		if strings.HasPrefix(a, "/api/annotations") {
			t.Fatalf("asked %s while GRAFANA_ALERTS held what really fired", a)
		}
	}
	// Twenty, as GRAFANA_ALERTS has them here, every one replayed.
	if cs, _ := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now); cs[gErrors].Real != 21 || cs[gErrors].Alike != 21 {
		t.Errorf("GErrors: %+v", cs[gErrors])
	}

	g = &stateHistory{settings: multiple}
	s, _ = historyServer(t, g, false, nil)
	s.jobs(ctx, true)
	if cs, _ := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now); cs[gErrors].Real != 17 || cs[gErrors].Alike != 16 {
		t.Errorf("GErrors from the history: %+v", cs[gErrors])
	}
}

// What can't be used, as Grafana answered, doesn't hold the check up, nor
// fail the rules beside: a token that may not read annotations (403), a
// page of changes at one time past what a page grows to, a change of a
// rule passed over. Grafana's rules aren't checked that day; the
// Prometheus rule beside is, and the check goes on, each time.
func TestCheckReplayGrafanaHistoryUnusable(t *testing.T) {
	for name, g := range map[string]*stateHistory{
		"403":       {settings: annotationsSettings, refuse: true},
		"same time": {settings: annotationsSettings, sameTime: true},
		"odd":       {settings: annotationsSettings, odd: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := historyServer(t, g, true, nil)
			ctx := context.Background()
			for range 3 {
				s.jobs(ctx, true)
			}
			checkedToToday(t, s)
			cs, err := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now)
			if err != nil {
				t.Fatal(err)
			}
			if c := cs["Errors"]; c.Replayed != 21 || c.Alike != 16 {
				t.Errorf("the Prometheus rule beside: %+v", c)
			}
			if name != "odd" {
				if c := cs[gErrors]; c.Either() != 0 {
					t.Errorf("GErrors checked: %+v", c)
				}
				_, body, _ := get(t, s.Handler(), "/runs")
				if !strings.Contains(body, "couldn&#39;t be used, as it answered") {
					t.Error("/runs doesn't say the history couldn't be used")
				}
				return
			}
			// The day whose read held the change passed over, at noon on
			// 31 October, isn't checked for it; the day after is.
			first, _ := s.l.Checks(ctx, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
			if c := first[gErrors]; c.Either() != 0 {
				t.Errorf("GErrors checked the day a change was passed over: %+v", c)
			}
			if c := cs[gErrors]; c.Either() == 0 {
				t.Errorf("GErrors checked no day: %+v", c)
			}
		})
	}
}

// More changes of state than are read at once don't check Grafana's rules
// that day, and the check goes on to the next: a busy Grafana can't hold
// the check up for good. The runs page says so, in each language, and
// what it says otherwise.
func TestCheckReplayGrafanaHistoryTooMany(t *testing.T) {
	defer func(n int) { source.MaxChanges = n }(source.MaxChanges)
	source.MaxChanges = 3
	g := &stateHistory{settings: annotationsSettings}
	s, _ := historyServer(t, g, false, nil)
	ctx := context.Background()
	s.jobs(ctx, true)
	checkedToToday(t, s)
	if cs, _ := s.l.Checks(ctx, now.Add(-7*24*time.Hour), now); cs[gErrors].Either() != 0 {
		t.Errorf("checked against part of the history: %+v", cs)
	}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	h := s.Handler()
	for _, off := range []string{historyTooMany, historyFrom, historyUnusable} {
		s.status.Lock()
		s.status.historyOff = off
		s.status.Unlock()
		for _, n := range names {
			r := httptest.NewRequest(http.MethodGet, "/runs", nil)
			r.Header.Set("Accept-Language", n.Code)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if n.Code == "en" && off == historyTooMany && !strings.Contains(w.Body.String(), "more changes of state over 2026-11-02 than are read at once") {
				t.Error("/runs doesn't say there were too many")
			}
		}
	}
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
}
