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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
)

// shopGrafana stands in for a Grafana 12.4 holding rules whose conditions
// read a MySQL data source, which Lapilli can't replay, and keeping its
// state history in annotations.
type shopGrafana struct {
	mu    sync.Mutex
	rules []shopRule
	notes []shopNote
}

// shopRule is a rule of a title in a folder, paused or not.
type shopRule struct {
	uid, title, folder string
	paused             bool
}

// shopNote is a change of state of the alert of a rule's title for a
// service, with as many labels besides as extra.
type shopNote struct {
	title, prev, next, service string
	at                         time.Time
	extra                      int
}

// fired adds a firing of title's alert for service from start to end, as
// long past now; still on, with no end.
func (g *shopGrafana) fired(title, service string, start, end time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.notes = append(g.notes, shopNote{title: title, prev: "Normal", next: "Alerting", service: service, at: now.Add(start)})
	if end != 0 {
		g.notes = append(g.notes, shopNote{title: title, prev: "Alerting", next: "Normal", service: service, at: now.Add(end)})
	}
}

// note adds a change as it is.
func (g *shopGrafana) note(n shopNote) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.notes = append(g.notes, n)
}

// hold sets the rules it holds.
func (g *shopGrafana) hold(rules ...shopRule) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rules = rules
}

func (g *shopGrafana) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch r.URL.Path {
	case "/api/v1/provisioning/alert-rules/export":
		folders := map[string][]any{}
		var order []string
		for _, x := range g.rules {
			if _, ok := folders[x.folder]; !ok {
				order = append(order, x.folder)
			}
			folders[x.folder] = append(folders[x.folder], map[string]any{"uid": x.uid, "title": x.title, "condition": "B", "for": "0s", "isPaused": x.paused,
				"labels": map[string]any{"severity": "page"},
				"data": []any{
					map[string]any{"refId": "A", "datasourceUid": "orders", "relativeTimeRange": map[string]any{"from": 600, "to": 0}, "model": map[string]any{"refId": "A", "rawSql": "SELECT 1"}},
					map[string]any{"refId": "B", "datasourceUid": "__expr__", "model": map[string]any{"refId": "B", "type": "threshold", "expression": "A",
						"conditions": []any{map[string]any{"evaluator": map[string]any{"type": "gt", "params": []any{5}}}}}}}})
		}
		var groups []any
		for _, f := range order {
			groups = append(groups, map[string]any{"orgId": 1, "name": "shop", "folder": f, "interval": "1m", "rules": folders[f]})
		}
		json.NewEncoder(w).Encode(map[string]any{"apiVersion": 1, "groups": groups})
	case "/api/datasources/uid/orders":
		io.WriteString(w, `{"uid": "orders", "type": "mysql", "jsonData": {}}`)
	case "/api/frontend/settings":
		io.WriteString(w, annotationsSettings)
	case "/api/annotations":
		var from, to, limit int64
		fmt.Sscan(r.URL.Query().Get("from"), &from)
		fmt.Sscan(r.URL.Query().Get("to"), &to)
		fmt.Sscan(r.URL.Query().Get("limit"), &limit)
		all := make([]map[string]any, 0, len(g.notes))
		for i, n := range g.notes {
			tags := []string{"alertname:" + n.title, "grafana_folder:Ops", "service:" + n.service, "severity:page"}
			for j := range n.extra {
				tags = append(tags, fmt.Sprintf("l%02d:v", j))
			}
			all = append(all, map[string]any{"id": i + 1, "alertName": n.title, "prevState": n.prev, "newState": n.next, "time": n.at.UnixMilli(), "tags": tags})
		}
		slices.SortStableFunc(all, func(a, b map[string]any) int { return int(b["time"].(int64) - a["time"].(int64)) }) // newest first
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

// shop is a lapilli serve reading the rules of a shopGrafana and judging
// them from its history, which can be started again on the same ledger.
type shop struct {
	t   *testing.T
	g   *shopGrafana
	url string
	tok string
	s   *Server
}

func shopServer(t *testing.T, g *shopGrafana) *shop {
	t.Helper()
	ts := httptest.NewServer(g)
	t.Cleanup(ts.Close)
	sh := &shop{t: t, g: g, url: ts.URL, tok: tokenFile(t, "glsa_viewer")}
	sh.s, _ = server(t, sh.config(nil))
	return sh
}

func (sh *shop) config(was *Server) func(*Config) {
	return func(c *Config) {
		c.RulesPath, c.Grafana, c.GrafanaToken = "", sh.url, sh.tok
		c.JudgedHistory = &source.GrafanaHistory{URL: sh.url, TokenFile: sh.tok, Agent: "test"}
		if was != nil {
			c.Ledger, c.LedgerURLFile = was.cfg.Ledger, was.cfg.LedgerURLFile
		}
	}
}

// restart is lapilli serve stopped and started again on its ledger: what
// it kept in memory of the copies is gone.
func (sh *shop) restart() {
	sh.t.Helper()
	was := sh.s
	if err := was.Close(); err != nil {
		sh.t.Fatal(err)
	}
	sh.s, _ = server(sh.t, sh.config(was))
}

// round reads the rules and runs a round, at as long past now.
func (sh *shop) round(at time.Duration) {
	sh.t.Helper()
	sh.s.now = func() time.Time { return now.Add(at) }
	raw := ""
	if err := sh.s.look(context.Background(), &raw); err != nil {
		sh.t.Fatal(err)
	}
	sh.s.jobs(context.Background(), true)
	if sh.s.status.errWhat != "" {
		sh.t.Fatalf("the round at %s failed: %s: %s", at, sh.s.status.errWhat, sh.s.status.err)
	}
}

// copied are the firings copied in from the history, as "rule/service
// start end" in time past now, in order; "on" for one with no end.
func (sh *shop) copied() []string {
	sh.t.Helper()
	fs, err := sh.s.l.RecordedFirings(context.Background(), ledger.GrafanaSource, now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour))
	if err != nil {
		sh.t.Fatal(err)
	}
	firing.Sort(fs)
	var out []string
	for _, f := range fs {
		end := "on"
		if !f.End.IsZero() {
			end = f.End.Sub(now).String()
		}
		out = append(out, fmt.Sprintf("%s/%s %s %s", f.Rule, f.Labels["service"], f.Start.Sub(now), end))
	}
	slices.Sort(out)
	return out
}

// covers are what the copies cover of a rule, as hours past now, from the
// second span on: the first begins where the backfill does.
func (sh *shop) covers(rule string) string {
	sh.t.Helper()
	spans, err := sh.s.l.SpansOf(context.Background(), ledger.GrafanaSource, rule)
	if err != nil {
		sh.t.Fatal(err)
	}
	var out []string
	for i, sp := range spans {
		from := sp[0].Sub(now).String()
		if i == 0 {
			from = ""
		}
		out = append(out, from+"→"+sp[1].Sub(now).String())
	}
	return strings.Join(out, " ")
}

func (sh *shop) want(what string, got, want any) {
	sh.t.Helper()
	if g, w := fmt.Sprint(got), fmt.Sprint(want); g != w {
		sh.t.Errorf("%s:\n got %s\nwant %s", what, g, w)
	}
}

const (
	ordersUID = "orders"
	stockUID  = "stock"
)

// ordersAndStock is a Grafana holding Orders slow and Stock low: Orders
// slow fired for the checkout from 06:00 to 06:30 the day before now, and
// has been on for the orders since 07:00; Stock low fired for the stock
// from 09:00 to 10:00.
func ordersAndStock() *shopGrafana {
	g := &shopGrafana{}
	g.hold(shopRule{ordersUID, "Orders slow", "Ops", false}, shopRule{stockUID, "Stock low", "Ops", false})
	g.fired("Orders slow", "checkout", -30*time.Hour, -30*time.Hour+30*time.Minute)
	g.fired("Orders slow", "orders", -5*time.Hour, 0)
	g.fired("Stock low", "stock", -3*time.Hour, -2*time.Hour)
	return g
}

// A second rule of a judged rule's title, even paused, in any folder
// Lapilli reads: the history never says which of the two fired. The judged
// rule is left out of each day while the title is shared, alone: nothing of
// it is written or taken away, its alert on stays on, and the days are no
// record of it, then and once the other rule is gone, whatever round reads
// them again and after a restart. The other rules' days are written as
// they are, and read as copied. Judged again, an alert of it that began
// meanwhile and is still on is written from there, in every round after.
func TestServeGrafanaJudgedShared(t *testing.T) {
	for _, paused := range []bool{false, true} {
		ctx := context.Background()
		g := ordersAndStock()
		sh := shopServer(t, g)
		sh.round(0)
		before := []string{"Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s on", "Stock low/stock -3h0m0s -2h0m0s"}
		sh.want("copied in", sh.copied(), before)

		// Two hours on, the other rule made. Meanwhile: under the title,
		// the payments fired, either rule's, and the orders' alert ended;
		// Stock low fired again.
		g.hold(shopRule{ordersUID, "Orders slow", "Ops", false}, shopRule{stockUID, "Stock low", "Ops", false}, shopRule{"elsewhere", "Orders slow", "Elsewhere", paused})
		g.fired("Orders slow", "payments", 30*time.Minute, 40*time.Minute)
		g.note(shopNote{title: "Orders slow", prev: "Alerting", next: "Normal", service: "orders", at: now.Add(10 * time.Minute)})
		g.fired("Stock low", "stock", time.Hour, 90*time.Minute)
		sh.round(2 * time.Hour)
		shared := append(slices.Clone(before), "Stock low/stock 1h0m0s 1h30m0s")
		sh.want("beside a rule of the same title", sh.copied(), shared)
		if _, to, _ := sh.s.l.Coverage(ctx, ledger.GrafanaSource); !to.Equal(now.Add(2 * time.Hour)) {
			t.Errorf("paused %v: the other rules' day copied up to %s", paused, to)
		}
		sh.want("Stock low's copies", sh.covers("Stock low"), "→2h0m0s")
		sh.want("Orders slow's copies, its title shared", sh.covers("Orders slow"), "→0s")
		// The other rule, judged too when it isn't paused, under a name
		// that tells it apart, is left out as well: its title is the
		// first's.
		out, err := sh.s.l.LeftOut(ctx, ledger.GrafanaSource)
		if want := map[bool]int{false: 2, true: 1}[paused]; err != nil || len(out) != want || len(out["Orders slow"]) != 1 {
			t.Errorf("paused %v: left out %v, want %d rules: %v", paused, out, want, err)
		}
		ties, err := sh.s.l.Ties(ctx)
		if o, st := ties["Orders slow"], ties["Stock low"]; err != nil || o.To != now.Unix() || o.Out != now.Unix() || st.To != now.Add(2*time.Hour).Unix() || st.Out != 0 {
			t.Errorf("paused %v: ties %+v %+v %v", paused, o, st, err)
		}
		sh.s.ready.Store(true)
		if _, body, _ := get(t, sh.s.Handler(), "/rules/"+url.PathEscape("Orders slow")); !strings.Contains(body, "left out of what is copied in of Grafana&#39;s own alert state history since") {
			t.Errorf("paused %v: its page doesn't say it is left out", paused)
		}
		if _, body, _ := get(t, sh.s.Handler(), "/rules/"+url.PathEscape("Stock low")); !strings.Contains(body, "what Grafana&#39;s own alert state history says it fired, copied in from") {
			t.Errorf("paused %v: Stock low's page doesn't say its history is its record", paused)
		}

		// Still shared two hours on: the search and the cart fire under
		// the title, and stay on; so does the basket, until five hours on.
		g.fired("Orders slow", "search", 3*time.Hour, 0)
		g.fired("Orders slow", "cart", 3*time.Hour+45*time.Minute, 0)
		g.fired("Orders slow", "basket", 3*time.Hour+10*time.Minute, 5*time.Hour)
		sh.round(4 * time.Hour)
		sh.want("still shared", sh.copied(), shared)
		sh.want("Orders slow's copies, still shared", sh.covers("Orders slow"), "→0s")

		// The other rule gone. A round shorter than --settle after: the
		// orders' alert is ended where the history says, and those on
		// since the title was shared are written from where it is the
		// rule's alone again. What fired and ended meanwhile is no one's.
		g.hold(shopRule{ordersUID, "Orders slow", "Ops", false}, shopRule{stockUID, "Stock low", "Ops", false})
		// The round after reads from after the search and the basket
		// began: the search, of which it reads no change, stays on as it
		// is, and the basket is ended from where it was written.
		after := []string{"Orders slow/basket 4h0m0s 5h0m0s", "Orders slow/cart 4h0m0s on", "Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s 10m0s",
			"Orders slow/search 4h0m0s on", "Stock low/stock -3h0m0s -2h0m0s", "Stock low/stock 1h0m0s 1h30m0s"}
		for _, at := range []time.Duration{4*time.Hour + 30*time.Minute, 6 * time.Hour, 8 * time.Hour} {
			sh.round(at)
			want := slices.Clone(after)
			if at < 5*time.Hour {
				want[0] = "Orders slow/basket 4h0m0s on"
			}
			sh.want(fmt.Sprintf("paused %v: the other rule gone, at %s", paused, at), sh.copied(), want)
			sh.want(fmt.Sprintf("paused %v: Orders slow's copies at %s", paused, at), sh.covers("Orders slow"), "→0s 4h0m0s→"+at.String())
		}
		// And lapilli serve started again.
		sh.restart()
		sh.round(10 * time.Hour)
		sh.want("after a restart", sh.copied(), after)
		sh.want("Orders slow's copies after a restart", sh.covers("Orders slow"), "→0s 4h0m0s→10h0m0s")
		sh.want("Stock low's copies after a restart", sh.covers("Stock low"), "→10h0m0s")
		ties, err = sh.s.l.Ties(ctx)
		if o := ties["Orders slow"]; err != nil || o.Since != now.Add(4*time.Hour).Unix() || o.To != now.Add(10*time.Hour).Unix() || o.Out != 0 {
			t.Errorf("paused %v: Orders slow's tie once judged again: %+v %v", paused, o, err)
		}
		// Its record is held from where it is judged again, not across
		// the days its title was shared.
		for _, c := range []struct {
			rule string
			at   time.Duration
			want bool
		}{{"Orders slow", -time.Hour, false}, {"Orders slow", 2 * time.Hour, false}, {"Orders slow", 5 * time.Hour, true}, {"Stock low", -time.Hour, true}} {
			v := ruleView{ruleLine: ruleLine{Name: c.rule}, Tie: tieViewOf(ties[c.rule])}
			if got, err := sh.s.holds(ctx, &v, now.Add(c.at)); err != nil || got != c.want {
				t.Errorf("paused %v: %s's record held from %s: %v %v", paused, c.rule, c.at, got, err)
			}
		}
	}
}

// The one judged rule's title shared: no rule is judged for the day, which
// is left a gap, as a day the history is of no use for is. The period is
// kept for the rule all the same: a restart, which reads the gap's days
// again, the other rule gone, writes nothing the history says of them
// under the title.
func TestServeGrafanaJudgedSharedAlone(t *testing.T) {
	ctx := context.Background()
	g := &shopGrafana{}
	g.hold(shopRule{ordersUID, "Orders slow", "Ops", false})
	g.fired("Orders slow", "orders", -5*time.Hour, -4*time.Hour)
	sh := shopServer(t, g)
	sh.round(0)
	g.hold(shopRule{ordersUID, "Orders slow", "Ops", false}, shopRule{"elsewhere", "Orders slow", "Elsewhere", true})
	g.fired("Orders slow", "payments", 30*time.Minute, 40*time.Minute)
	sh.round(2 * time.Hour)
	if _, to, _ := sh.s.l.Coverage(ctx, ledger.GrafanaSource); !to.Equal(now) {
		t.Errorf("no rule judged, yet copied up to %s", to)
	}
	out, err := sh.s.l.LeftOut(ctx, ledger.GrafanaSource)
	if err != nil || len(out["Orders slow"]) != 1 || !out["Orders slow"][0][0].Equal(now) || !out["Orders slow"][0][1].Equal(now.Add(2*time.Hour)) {
		t.Errorf("kept as left out: %v %v", out, err)
	}
	g.hold(shopRule{ordersUID, "Orders slow", "Ops", false})
	g.fired("Orders slow", "search", 2*time.Hour+30*time.Minute, 2*time.Hour+40*time.Minute)
	sh.restart()
	sh.round(3 * time.Hour)
	sh.want("after a restart, the other rule gone", sh.copied(), []string{"Orders slow/orders -5h0m0s -4h0m0s", "Orders slow/search 2h30m0s 2h40m0s"})
	sh.want("its copies", sh.covers("Orders slow"), "→0s 2h0m0s→3h0m0s")
}

// A rule the history can't say all it fired of (a state Grafana has no
// name for, here) is left out alone, while a read holds the change; the
// other rule's days are written. Past it, the rule is judged again.
func TestServeGrafanaJudgedUnsureAlone(t *testing.T) {
	g := ordersAndStock()
	sh := shopServer(t, g)
	sh.round(0)
	g.note(shopNote{title: "Stock low", prev: "Normal", next: "Firing", service: "stock", at: now.Add(30 * time.Minute)})
	g.fired("Orders slow", "payments", time.Hour, 90*time.Minute)
	sh.round(2 * time.Hour)
	sh.want("beside a rule the history can't say all of", sh.copied(), []string{"Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s on",
		"Orders slow/payments 1h0m0s 1h30m0s", "Stock low/stock -3h0m0s -2h0m0s"})
	sh.want("Stock low's copies", sh.covers("Stock low"), "→0s")
	sh.want("Orders slow's copies", sh.covers("Orders slow"), "→2h0m0s")
	// A day and three hours on: read from a day before each day copied,
	// the change is in the first's read, and not in the second's.
	g.fired("Stock low", "stock", 26*time.Hour+30*time.Minute, 26*time.Hour+40*time.Minute)
	sh.round(27 * time.Hour)
	sh.want("Stock low's copies, judged again", sh.covers("Stock low"), "→0s 26h0m0s→27h0m0s")
	sh.want("Orders slow's copies, a day on", sh.covers("Orders slow"), "→27h0m0s")
	if got := sh.copied(); !slices.Contains(got, "Stock low/stock 26h30m0s 26h40m0s") {
		t.Errorf("Stock low judged again: %q", got)
	}
}

// A rule with a firing no write holds (40 labels) is left out alone, and
// so is one whose name the ledger keeps no rule by, a zero-width space in
// it: that one with no period kept, as it never has a record.
func TestServeGrafanaJudgedUnheldAlone(t *testing.T) {
	ctx := context.Background()
	g := ordersAndStock()
	const hidden = "Cart\u200bslow"
	g.hold(shopRule{ordersUID, "Orders slow", "Ops", false}, shopRule{stockUID, "Stock low", "Ops", false}, shopRule{"cart", hidden, "Ops", false})
	sh := shopServer(t, g)
	sh.round(0)
	g.note(shopNote{title: "Stock low", prev: "Normal", next: "Alerting", service: "depot", at: now.Add(30 * time.Minute), extra: 40})
	g.fired(hidden, "cart", 20*time.Minute, 25*time.Minute)
	g.fired("Orders slow", "payments", time.Hour, 90*time.Minute)
	sh.round(2 * time.Hour)
	sh.want("beside rules with firings no write holds", sh.copied(), []string{"Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s on",
		"Orders slow/payments 1h0m0s 1h30m0s", "Stock low/stock -3h0m0s -2h0m0s"})
	sh.want("Stock low's copies", sh.covers("Stock low"), "→0s")
	sh.want("Orders slow's copies", sh.covers("Orders slow"), "→2h0m0s")
	out, err := sh.s.l.LeftOut(ctx, ledger.GrafanaSource)
	if err != nil || len(out) != 1 || len(out["Stock low"]) != 1 {
		t.Errorf("kept as left out: %v %v", out, err)
	}
}

// An alert of a rule left out, on since before, is neither ended nor
// written again while the rule is left out, whatever the history says
// under its title; judged again, it is ended where the history says. One
// the history says ended and fired again meanwhile, still on, is ended and
// written from where the rule is judged again.
func TestServeGrafanaJudgedLeftOutOn(t *testing.T) {
	g := ordersAndStock()
	g.fired("Orders slow", "search", -4*time.Hour, 0)
	sh := shopServer(t, g)
	sh.round(0)
	dup := shopRule{"elsewhere", "Orders slow", "Elsewhere", true}
	g.hold(shopRule{ordersUID, "Orders slow", "Ops", false}, shopRule{stockUID, "Stock low", "Ops", false}, dup)
	// The orders' alert ends; the search's ends and fires again.
	g.note(shopNote{title: "Orders slow", prev: "Alerting", next: "Normal", service: "orders", at: now.Add(10 * time.Minute)})
	g.note(shopNote{title: "Orders slow", prev: "Alerting", next: "Normal", service: "search", at: now.Add(20 * time.Minute)})
	g.fired("Orders slow", "search", 50*time.Minute, 0)
	sh.round(2 * time.Hour)
	sh.want("left out", sh.copied(), []string{"Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s on",
		"Orders slow/search -4h0m0s on", "Stock low/stock -3h0m0s -2h0m0s"})
	g.hold(shopRule{ordersUID, "Orders slow", "Ops", false}, shopRule{stockUID, "Stock low", "Ops", false})
	for _, at := range []time.Duration{4 * time.Hour, 4*time.Hour + 30*time.Minute, 6 * time.Hour} {
		sh.round(at)
		sh.want(fmt.Sprintf("judged again, at %s", at), sh.copied(), []string{"Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s 10m0s",
			"Orders slow/search -4h0m0s 20m0s", "Orders slow/search 2h0m0s on", "Stock low/stock -3h0m0s -2h0m0s"})
	}
}

// A rule left out twice: an alert of it on since the first period, written
// from where the rule was judged again, that ends in the second, is ended
// where the history says once the rule is judged again, read from before
// either: the history has it from its start, the ledger from later.
func TestServeGrafanaJudgedLeftOutTwice(t *testing.T) {
	g := ordersAndStock()
	sh := shopServer(t, g)
	sh.round(0)
	one := []shopRule{{ordersUID, "Orders slow", "Ops", false}, {stockUID, "Stock low", "Ops", false}}
	two := append(slices.Clone(one), shopRule{"elsewhere", "Orders slow", "Elsewhere", true})
	g.note(shopNote{title: "Orders slow", prev: "Alerting", next: "Normal", service: "orders", at: now.Add(10 * time.Minute)})
	g.fired("Orders slow", "search", time.Hour, 4*time.Hour+30*time.Minute)
	g.hold(two...)
	sh.round(2 * time.Hour)
	g.hold(one...)
	sh.round(4 * time.Hour)
	if got := sh.copied(); !slices.Contains(got, "Orders slow/search 2h0m0s on") {
		t.Errorf("judged again: %q", got)
	}
	g.hold(two...)
	sh.round(6 * time.Hour)
	g.hold(one...)
	for _, at := range []time.Duration{8 * time.Hour, 10 * time.Hour} {
		sh.round(at)
		sh.want(fmt.Sprintf("judged again a second time, at %s", at), sh.copied(), []string{"Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s 10m0s",
			"Orders slow/search 2h0m0s 4h30m0s", "Stock low/stock -3h0m0s -2h0m0s"})
		sh.want("its copies", sh.covers("Orders slow"), "→0s 2h0m0s→4h0m0s 6h0m0s→"+at.String())
	}
}

// The same of a gap of the source's, a day no rule is judged for (the one
// rule's title shared, here): an alert that ended and fired again in the
// gap, still on, is ended and written from where the copies cover again;
// one that began in it stays, in a round after that reads from after it
// began; and one written from a gap's end that ends in a later gap is
// ended where the history says, read from before either.
func TestServeGrafanaJudgedGapAlerts(t *testing.T) {
	g := &shopGrafana{}
	one := []shopRule{{ordersUID, "Orders slow", "Ops", false}}
	two := append(slices.Clone(one), shopRule{"elsewhere", "Orders slow", "Elsewhere", true})
	g.hold(one...)
	g.fired("Orders slow", "orders", -5*time.Hour, 20*time.Minute)
	sh := shopServer(t, g)
	sh.round(0)
	sh.want("copied in", sh.copied(), []string{"Orders slow/orders -5h0m0s on"})
	g.fired("Orders slow", "orders", 50*time.Minute, 0)
	g.fired("Orders slow", "search", time.Hour, 5*time.Hour)
	g.fired("Orders slow", "cart", 70*time.Minute, 0)
	g.hold(two...)
	sh.round(2 * time.Hour)
	g.hold(one...)
	// The first round after the gap, shorter than --settle; the next reads
	// from after the search and the cart began.
	on := []string{"Orders slow/cart 2h0m0s on", "Orders slow/orders -5h0m0s 20m0s", "Orders slow/orders 2h0m0s on", "Orders slow/search 2h0m0s on"}
	for _, at := range []time.Duration{2*time.Hour + 30*time.Minute, 4 * time.Hour} {
		sh.round(at)
		sh.want(fmt.Sprintf("after the gap, at %s", at), sh.copied(), on)
	}
	// A second gap, the search ending in it.
	g.hold(two...)
	sh.round(6 * time.Hour)
	g.hold(one...)
	for _, at := range []time.Duration{8 * time.Hour, 10 * time.Hour} {
		sh.round(at)
		sh.want(fmt.Sprintf("after a second gap, at %s", at), sh.copied(), []string{"Orders slow/cart 2h0m0s on", "Orders slow/orders -5h0m0s 20m0s",
			"Orders slow/orders 2h0m0s on", "Orders slow/search 2h0m0s 5h0m0s"})
	}
	spans, err := sh.s.l.Spans(context.Background(), ledger.GrafanaSource)
	if err != nil || len(spans) != 3 || !spans[0][1].Equal(now) || !spans[1][0].Equal(now.Add(2*time.Hour)) || !spans[1][1].Equal(now.Add(4*time.Hour)) || !spans[2][0].Equal(now.Add(6*time.Hour)) {
		t.Errorf("the gaps: %v %v", spans, err)
	}
}

// What a rule is held to isn't known over a week it was left out of the
// copies in, once it is a rule only copied in (renamed in Grafana, here):
// a week no firing was copied in for it isn't a week it didn't fire.
func TestServeGrafanaJudgedLeftOutExpect(t *testing.T) {
	ctx := context.Background()
	g := ordersAndStock()
	sh := shopServer(t, g)
	sh.round(0)
	g.hold(shopRule{ordersUID, "Orders slow", "Ops", false}, shopRule{stockUID, "Stock low", "Ops", false}, shopRule{"elsewhere", "Orders slow", "Elsewhere", true})
	sh.round(2 * time.Hour)
	// Both renamed: each judged under its new name, and the history copied
	// in for the old ones kept, as a source's rules, named apart.
	g.hold(shopRule{ordersUID, "Orders slower", "Ops", false}, shopRule{stockUID, "Stock lower", "Ops", false})
	file := filepath.Join(t.TempDir(), "expect.yaml")
	os.WriteFile(file, []byte("expectations:\n  - rule: \"Orders slow [grafana]\"\n    budget: {week: 5}\n  - rule: \"Stock low [grafana]\"\n    budget: {week: 5}\n"), 0o600)
	sh.s.cfg.Expect = file
	sh.round(4 * time.Hour)
	expectReadTwice(t, sh.s)
	checkAgain(t, sh.s)
	table, err := sh.s.l.RuleTable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var copied []string
	for _, r := range table {
		if r.Source == ledger.GrafanaSource {
			copied = append(copied, r.Name+" < "+r.Sent)
		}
	}
	slices.Sort(copied)
	sh.want("the rules copied in, and the names they were sent by", copied, []string{"Orders slow [grafana] < Orders slow", "Stock low [grafana] < Stock low"})
	week := "budget week " + sh.s.mondayOf(ledger.Week(sh.s.l.Day(now)))
	if c := checksOf(t, sh.s, "Orders slow [grafana]")[week]; c.Result != ledger.ExpectUnknown || c.Why != whyNotCopied {
		t.Errorf("Orders slow, left out in the week: %+v", c)
	}
	if c := checksOf(t, sh.s, "Stock low [grafana]")[week]; c.Result != ledger.ExpectOpen {
		t.Errorf("Stock low, copied through the week so far: %+v", c)
	}
	// Its page's numbers aren't set beside the span before, across the
	// period it was left out.
	for rule, want := range map[string]bool{"Orders slow [grafana]": false, "Stock low [grafana]": true} {
		v := ruleView{ruleLine: ruleLine{Name: rule, Source: ledger.GrafanaSource}}
		if got, err := sh.s.holds(ctx, &v, now.Add(-time.Hour)); err != nil || got != want {
			t.Errorf("%s's record held from an hour before: %v %v", rule, got, err)
		}
	}
}

// A Grafana rule judged under a name made to tell it apart from a rule of
// its name read elsewhere, a Loki ruler's here, is read from the history
// by its title, as Grafana names its alerts: not taken for a rule that
// never fired.
func TestServeGrafanaJudgedNamedApart(t *testing.T) {
	ctx := context.Background()
	g := &shopGrafana{}
	g.hold(shopRule{ordersUID, "Orders slow", "Ops", false})
	g.fired("Orders slow", "orders", -5*time.Hour, -4*time.Hour)
	ts := httptest.NewServer(g)
	t.Cleanup(ts.Close)
	tok := tokenFile(t, "glsa_viewer")
	dir := filepath.Join(t.TempDir(), "loki")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "logs.yaml"), []byte(`groups:
  - name: logs
    rules:
      - alert: Orders slow
        expr: sum by (service) (count_over_time({app="orders"} |= "slow" [5m])) > 5
`), 0o644)
	s, _ := server(t, func(c *Config) {
		c.RulesPath, c.Grafana, c.GrafanaToken = "", ts.URL, tok
		c.JudgedHistory = &source.GrafanaHistory{URL: ts.URL, TokenFile: tok, Agent: "test"}
		c.Loki, c.LokiRules = "http://loki.example:3100", dir
	})
	logs := &lokiLogs{}
	s.h = replay.WithLogs(s.h, func(rules.Rule) (replay.History, error) { return logs, nil })
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if s.status.errWhat != "" {
		t.Fatalf("a round failed: %s: %s", s.status.errWhat, s.status.err)
	}
	judged, err := s.l.FromHistory(ctx)
	if err != nil || len(judged) != 1 || judged[0] == "Orders slow" {
		t.Fatalf("the Grafana rule, named apart from the Loki ruler's: %q %v", judged, err)
	}
	fs, err := s.l.RecordedFirings(ctx, ledger.GrafanaSource, now.Add(-7*24*time.Hour), now.Add(time.Hour))
	if err != nil || len(fs) != 1 || fs[0].Rule != judged[0] || fs[0].Labels["service"] != "orders" {
		t.Errorf("its firing, under the name it is judged by, %q: %+v %v", judged[0], fs, err)
	}
}

// A firing on in a span of the copies the ledger let go, past as many as
// it keeps, that the history then says ended, is ended where it is: not
// left on, nor written a second time where the copies cover again, and
// the span stays let go.
func TestServeGrafanaJudgedEndedInSpanLetGo(t *testing.T) {
	ctx := context.Background()
	g := &shopGrafana{}
	g.hold(shopRule{ordersUID, "Orders slow", "Ops", false})
	g.fired("Orders slow", "orders", -5*time.Hour, 0)
	g.fired("Orders slow", "search", -5*time.Hour, -4*time.Hour)
	sh := shopServer(t, g)
	sh.round(0)
	// As many spans after it, a gap between each, as push the first out.
	n := 0
	for at := now.Add(time.Hour); ; at = at.Add(2 * time.Minute) {
		if _, err := sh.s.l.PutFirings(ctx, ledger.GrafanaSource, at, at.Add(time.Minute), nil, ledger.GrafanaCopier, sh.s.cfg.Options); err != nil {
			t.Fatal(err)
		}
		n++
		if spans, err := sh.s.l.Spans(ctx, ledger.GrafanaSource); err != nil {
			t.Fatal(err)
		} else if spans[0][0].After(now) {
			break
		}
		if n > 1000 {
			t.Fatal("the first span is never let go")
		}
	}
	_, to, _ := sh.s.l.Coverage(ctx, ledger.GrafanaSource)
	end := to.Add(20 * time.Minute)
	g.note(shopNote{title: "Orders slow", prev: "Alerting", next: "Normal", service: "orders", at: end})
	sh.round(end.Add(30 * time.Minute).Sub(now))
	sh.want("ended where it is", sh.copied(), []string{"Orders slow/orders -5h0m0s " + end.Sub(now).String(), "Orders slow/search -5h0m0s -4h0m0s"})
	if spans, err := sh.s.l.Spans(ctx, ledger.GrafanaSource); err != nil || !spans[0][0].After(now) {
		t.Errorf("the span let go is covered again: %v %v", spans[0], err)
	}
}
