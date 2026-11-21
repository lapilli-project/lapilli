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
	// more are changes of Orders slow besides; gone are those before it,
	// as a history's retention drops them.
	more []alertChange
	gone time.Time
}

// alertChange is a change of Orders slow's alert for a service.
type alertChange struct {
	prev, next, service string
	at                  time.Time
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
		for i, c := range g.more {
			all = append(all, note(10+i, c.prev, c.next, c.service, c.at))
		}
		slices.SortStableFunc(all, func(a, b map[string]any) int { return int(b["time"].(int64) - a["time"].(int64)) })
		out := []map[string]any{}
		for _, n := range all {
			if ms := n["time"].(int64); ms >= from && ms <= to && ms >= g.gone.UnixMilli() && int64(len(out)) < limit {
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
	return judgingServer(t, g)
}

// judgingServer is ordersServer, of any stand-in for a Grafana.
func judgingServer(t *testing.T, g http.Handler) *Server {
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
// within every bound of a read, none of which a write holds. The days
// that read them are of no use, left a gap rather than taken for quiet
// ones, before anything is made of the firings' labels; the round goes on.
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
	if n, k := seen.Load(), keyed.Load(); n == 0 || k != 0 {
		t.Errorf("%d firings looked at, %d of them made into keys", n, k)
	}
	if fs, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, now.Add(-7*24*time.Hour), now.Add(time.Hour)); err != nil || len(fs) != 0 {
		t.Errorf("copied in: %d %v", len(fs), err)
	}
	if from, to, _ := s.l.Coverage(ctx, ledger.GrafanaSource); !from.IsZero() || !to.IsZero() {
		t.Errorf("taken as copied from %s to %s", from, to)
	}
}

// A rule the history can't say all it fired of (a state Grafana has no
// name for, here) makes the day of no use: nothing of it is written, so
// what is copied in already stays, and the copies reach no further.
func TestServeGrafanaJudgedUnsure(t *testing.T) {
	ctx := context.Background()
	g := &ordersGrafana{}
	s := ordersServer(t, g)
	s.jobs(ctx, true)
	before, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, now.Add(-7*24*time.Hour), now.Add(time.Hour))
	if err != nil || len(before) != 2 {
		t.Fatalf("copied in: %+v %v", before, err)
	}
	g.mu.Lock()
	g.more = []alertChange{{"Normal", "Firing", "orders", now.Add(30 * time.Minute)}}
	g.mu.Unlock()
	s.now = func() time.Time { return now.Add(2 * time.Hour) }
	s.jobs(ctx, true)
	if s.status.errWhat != "" {
		t.Fatalf("a round failed: %s: %s", s.status.errWhat, s.status.err)
	}
	after, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, now.Add(-7*24*time.Hour), now.Add(3*time.Hour))
	if err != nil || len(after) != 2 {
		t.Errorf("after a day of no use: %+v %v", after, err)
	}
	if _, to, _ := s.l.Coverage(ctx, ledger.GrafanaSource); !to.Equal(now) {
		t.Errorf("copied up to %s", to)
	}
}

// A copy behind the history's retention passes over the days it no
// longer holds, a gap, rather than taking them for quiet ones.
func TestServeGrafanaJudgedRetention(t *testing.T) {
	ctx := context.Background()
	g := &ordersGrafana{}
	s := ordersServer(t, g)
	s.jobs(ctx, true)
	// Three days on, the history keeps only the last day, an alert of the
	// search in it.
	later := now.Add(3 * 24 * time.Hour)
	g.mu.Lock()
	g.gone = later.Add(-24 * time.Hour)
	g.more = []alertChange{{"Normal", "Alerting", "search", later.Add(-20 * time.Hour)}}
	g.mu.Unlock()
	s.now = func() time.Time { return later }
	s.jobs(ctx, true)
	spans, err := s.l.Spans(ctx, ledger.GrafanaSource)
	if err != nil || len(spans) != 2 || !spans[0][1].Equal(now) || !spans[1][0].Equal(later.Add(-24*time.Hour)) || !spans[1][1].Equal(later) {
		t.Errorf("copies cover %v %v", spans, err)
	}
	fs, _ := s.l.RecordedFirings(ctx, ledger.GrafanaSource, now, later)
	if len(fs) != 1 || fs[0].Labels["service"] != "search" {
		t.Errorf("copied in: %+v", fs)
	}
}

// A day the history holds more of than is read at once is passed over,
// counted by its changes (one here), left a gap, and logged; the round
// doesn't fail for it. The day after is copied as a first one: an alert
// that started in the day passed over, and is still on, is seen from its
// start.
func TestServeGrafanaJudgedTooMany(t *testing.T) {
	ctx := context.Background()
	was := source.MaxChanges
	t.Cleanup(func() { source.MaxChanges = was })
	g := &ordersGrafana{}
	s := ordersServer(t, g)
	s.jobs(ctx, true)
	// The next day, the search fires two hours on, and is still on.
	g.mu.Lock()
	g.more = []alertChange{{"Normal", "Alerting", "search", now.Add(2 * time.Hour)}}
	g.ended = true
	g.mu.Unlock()
	source.MaxChanges = 1
	s.now = func() time.Time { return now.Add(24 * time.Hour) }
	s.jobs(ctx, true)
	if s.status.errWhat != "" {
		t.Fatalf("a round failed: %s: %s", s.status.errWhat, s.status.err)
	}
	if _, to, _ := s.l.Coverage(ctx, ledger.GrafanaSource); !to.Equal(now) {
		t.Errorf("copied up to %s past the bounds", to)
	}
	s.status.Lock()
	from := s.status.copyFrom
	s.status.Unlock()
	if !from.Equal(now.Add(24 * time.Hour)) {
		t.Errorf("goes on from %s", from)
	}
	source.MaxChanges = was
	s.now = func() time.Time { return now.Add(25 * time.Hour) }
	s.jobs(ctx, true)
	fs, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, now.Add(24*time.Hour), now.Add(26*time.Hour))
	if err != nil || len(fs) != 1 || fs[0].Labels["service"] != "search" || !fs[0].Start.Equal(now.Add(24*time.Hour)) || !fs[0].End.IsZero() {
		t.Errorf("the search, on since the day passed over: %+v %v", fs, err)
	}
}

// A source of its name a job copied firings in under, before 0.68, isn't
// written over: Grafana's history isn't copied in, and it is said once.
func TestServeGrafanaJudgedTheirs(t *testing.T) {
	ctx := context.Background()
	s := ordersServer(t, &ordersGrafana{})
	if _, err := s.l.PutFirings(ctx, ledger.GrafanaSource, now.Add(-2*time.Hour), now.Add(-time.Hour), nil, ledger.FiringsCopier, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if fs, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, now.Add(-7*24*time.Hour), now.Add(time.Hour)); err != nil || len(fs) != 0 {
		t.Errorf("copied in under a job's source: %+v %v", fs, err)
	}
	srcs, _ := s.l.Sources(ctx)
	if len(srcs) != 1 || srcs[0].By != ledger.FiringsCopier || !s.status.copyTheirs {
		t.Errorf("sources %+v", srcs)
	}
}

// dupGrafana is ordersGrafana with, once dup is set, a second rule titled
// Orders slow in another folder, paused or not: what anyone who may write
// a rule in a folder Lapilli reads can make, as Grafana keeps a title
// unique in a folder alone, and two teams may make by chance.
type dupGrafana struct {
	*ordersGrafana
	mu          sync.Mutex
	dup, paused bool
}

func (d *dupGrafana) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	dup, paused := d.dup, d.paused
	d.mu.Unlock()
	if r.URL.Path != "/api/v1/provisioning/alert-rules/export" || !dup {
		d.ordersGrafana.ServeHTTP(w, r)
		return
	}
	rule := func(uid string) map[string]any {
		return map[string]any{"uid": uid, "title": "Orders slow", "condition": "B", "for": "0s", "labels": map[string]any{"severity": "page"}, "isPaused": paused && uid != "orders",
			"data": []any{
				map[string]any{"refId": "A", "datasourceUid": "orders", "relativeTimeRange": map[string]any{"from": 600, "to": 0}, "model": map[string]any{"refId": "A", "rawSql": "SELECT 1"}},
				map[string]any{"refId": "B", "datasourceUid": "__expr__", "model": map[string]any{"refId": "B", "type": "threshold", "expression": "A",
					"conditions": []any{map[string]any{"evaluator": map[string]any{"type": "gt", "params": []any{5}}}}}}}}
	}
	json.NewEncoder(w).Encode(map[string]any{"apiVersion": 1, "groups": []any{
		map[string]any{"orgId": 1, "name": "shop", "folder": "Ops", "interval": "1m", "rules": []any{rule("orders")}},
		map[string]any{"orgId": 1, "name": "mine", "folder": "Elsewhere", "interval": "1m", "rules": []any{rule("elsewhere")}},
	}})
}

// A second rule of the same title, even paused, makes each day the history
// is read of no use: it never says which of the two fired. The judged
// rule's firings copied in already stay, nothing is written, and the
// copies reach no further, a gap rather than a quiet day. Once it is gone,
// the copy goes on, the firing that ended meanwhile ended, and the gap
// stays one.
func TestServeGrafanaJudgedShared(t *testing.T) {
	for _, paused := range []bool{false, true} {
		ctx := context.Background()
		// A firing of search's within the --settle the next round reads
		// again.
		g := &dupGrafana{ordersGrafana: &ordersGrafana{more: []alertChange{
			{"Normal", "Alerting", "search", now.Add(-40 * time.Minute)},
			{"Alerting", "Normal", "search", now.Add(-35 * time.Minute)},
		}}, paused: paused}
		s := judgingServer(t, g)
		s.jobs(ctx, true)
		before, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, now.Add(-7*24*time.Hour), now.Add(5*time.Hour))
		if err != nil || len(before) != 3 {
			t.Fatalf("paused %v: copied in: %+v %v", paused, before, err)
		}
		_, covered, _ := s.l.Coverage(ctx, ledger.GrafanaSource)
		// Two hours on, the other rule made: payments fires, orders ends.
		g.mu.Lock()
		g.dup = true
		g.mu.Unlock()
		g.ordersGrafana.mu.Lock()
		g.ended = true
		g.more = append(g.more, alertChange{"Normal", "Alerting", "payments", now.Add(30 * time.Minute)}, alertChange{"Alerting", "Normal", "payments", now.Add(40 * time.Minute)})
		g.ordersGrafana.mu.Unlock()
		s.now = func() time.Time { return now.Add(2 * time.Hour) }
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		s.jobs(ctx, true)
		if s.status.errWhat != "" {
			t.Fatalf("paused %v: a round failed: %s: %s", paused, s.status.errWhat, s.status.err)
		}
		after, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, now.Add(-7*24*time.Hour), now.Add(5*time.Hour))
		if err != nil || len(after) != 3 || slices.ContainsFunc(after, func(f firing.Firing) bool { return f.Labels["service"] == "payments" }) {
			t.Errorf("paused %v: beside a rule of the same title: %+v %v", paused, after, err)
		}
		if _, to, _ := s.l.Coverage(ctx, ledger.GrafanaSource); !to.Equal(covered) {
			t.Errorf("paused %v: copied up to %s beside a rule of the same title, not %s", paused, to, covered)
		}
		// The other rule gone, two hours on.
		g.mu.Lock()
		g.dup = false
		g.mu.Unlock()
		s.now = func() time.Time { return now.Add(4 * time.Hour) }
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		s.jobs(ctx, true)
		if open, err := s.l.OpenFirings(ctx, ledger.GrafanaSource, now.Add(5*time.Hour)); err != nil || len(open) != 0 {
			t.Errorf("paused %v: still open: %+v %v", paused, open, err)
		}
		spans, err := s.l.Spans(ctx, ledger.GrafanaSource)
		if err != nil || len(spans) < 2 || spans[len(spans)-2][1].After(covered) || !spans[len(spans)-1][0].After(covered) {
			t.Errorf("paused %v: the gap beside a rule of the same title isn't one: %v %v", paused, spans, err)
		}
	}
}

// A rule judged so no more (renamed, here; paused or deleted alike) keeps
// what was copied in for it within the --settle the next round reads
// again: the write is of the rules judged now alone.
func TestServeGrafanaJudgedLeftOut(t *testing.T) {
	ctx := context.Background()
	g := &ordersGrafana{more: []alertChange{
		{"Normal", "Alerting", "search", now.Add(-40 * time.Minute)},
		{"Alerting", "Normal", "search", now.Add(-35 * time.Minute)},
	}}
	s := ordersServer(t, g)
	s.jobs(ctx, true)
	g.mu.Lock()
	g.title = "Orders slower"
	g.mu.Unlock()
	s.now = func() time.Time { return now.Add(2 * time.Hour) }
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if s.status.errWhat != "" {
		t.Fatalf("a round failed: %s: %s", s.status.errWhat, s.status.err)
	}
	fs, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, now.Add(-time.Hour), now.Add(3*time.Hour))
	if err != nil || len(fs) != 1 || fs[0].Rule != "Orders slow" || fs[0].Labels["service"] != "search" {
		t.Errorf("the renamed rule's firing copied in: %+v %v", fs, err)
	}
}

// A firing on across a gap, or two, that the history says ended a round
// or more after the copies went on past them is written again from its
// start within the spans it lies across, ended: each gap stays one, never
// read as days with a record.
func TestServeGrafanaJudgedGapsStay(t *testing.T) {
	for name, c := range map[string]struct {
		rounds []bool // a round each two hours, a title shared or not
		ended  int    // hours past now the firing ends
	}{
		// The day the firing ends follows a gap, a round on.
		"a gap, ended a round after": {[]bool{false, true, false, false}, 5},
		// Two gaps, the firing ending in the second.
		"two gaps":              {[]bool{false, true, false, true, false}, 5},
		"two gaps, ended after": {[]bool{false, true, false, true, false}, 7},
	} {
		rounds := c.rounds
		ctx := context.Background()
		g := &dupGrafana{ordersGrafana: &ordersGrafana{}}
		s := judgingServer(t, g)
		raw := ""
		for i, dup := range rounds {
			if i == len(rounds)-1 {
				// orders, on since 07:00, ends before this round.
				g.ordersGrafana.mu.Lock()
				g.more = append(g.more, alertChange{"Alerting", "Normal", "orders", now.Add(time.Duration(c.ended) * time.Hour)})
				g.ordersGrafana.mu.Unlock()
			}
			g.mu.Lock()
			g.dup = dup
			g.mu.Unlock()
			at := now.Add(time.Duration(2*i) * time.Hour)
			s.now = func() time.Time { return at }
			if err := s.look(ctx, &raw); err != nil {
				t.Fatal(err)
			}
			s.jobs(ctx, true)
			if s.status.errWhat != "" {
				t.Fatalf("%s: round %d failed: %s: %s", name, i, s.status.errWhat, s.status.err)
			}
		}
		if open, err := s.l.OpenFirings(ctx, ledger.GrafanaSource, now.Add(24*time.Hour)); err != nil || len(open) != 0 {
			t.Errorf("%s: still open: %+v %v", name, open, err)
		}
		spans, err := s.l.Spans(ctx, ledger.GrafanaSource)
		if err != nil {
			t.Fatal(err)
		}
		for i, dup := range rounds {
			if !dup {
				continue
			}
			in := now.Add(time.Duration(2*i-1) * time.Hour) // inside the round's gap
			for _, sp := range spans {
				if !sp[0].After(in) && sp[1].After(in) {
					t.Errorf("%s: the gap of round %d (%s) is covered: %v", name, i, in, spans)
				}
			}
		}
	}
}
