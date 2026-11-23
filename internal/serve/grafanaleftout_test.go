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
// state history in annotations. Its annotations say no rule's id unless
// ids is set: then each carries its rule's id (alertId) and the title the
// rule has now (alertName), "" once it is deleted, as Grafana's do, and its
// provisioning API lists each rule's id. hidden are rules in a folder the
// token doesn't read: neither exported nor listed, their annotations are
// read all the same.
type shopGrafana struct {
	mu     sync.Mutex
	rules  []shopRule
	hidden []shopRule
	notes  []shopNote
	ids    bool
}

// shopRule is a rule of a title in a folder, paused or not.
type shopRule struct {
	uid, title, folder string
	paused             bool
}

// shopNote is a change of state of the alert of a rule's title for a
// service, with as many labels besides as extra; of the rule of uid, ""
// for the first of the title the Grafana holds when it is added.
type shopNote struct {
	title, prev, next, service string
	at                         time.Time
	extra                      int
	uid                        string
}

// fired adds a firing of title's alert for service from start to end, as
// long past now; still on, with no end. It is the first rule's of the
// title.
func (g *shopGrafana) fired(title, service string, start, end time.Duration) {
	g.firedBy("", title, service, start, end)
}

// firedBy is fired, of the rule of uid.
func (g *shopGrafana) firedBy(uid, title, service string, start, end time.Duration) {
	g.note(shopNote{uid: uid, title: title, prev: "Normal", next: "Alerting", service: service, at: now.Add(start)})
	if end != 0 {
		g.note(shopNote{uid: uid, title: title, prev: "Alerting", next: "Normal", service: service, at: now.Add(end)})
	}
}

// note adds a change as it is.
func (g *shopGrafana) note(n shopNote) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if i := slices.IndexFunc(g.rules, func(r shopRule) bool { return r.title == n.title }); n.uid == "" && i >= 0 {
		n.uid = g.rules[i].uid
	}
	g.notes = append(g.notes, n)
}

// idOf is the id in Grafana's database of the rule of uid.
func (g *shopGrafana) idOf(uid string) int64 {
	ids := map[string]int64{ordersUID: 1, stockUID: 2}
	if id, ok := ids[uid]; ok {
		return id
	}
	var n int64
	for _, c := range uid {
		n = n*31 + int64(c)
	}
	return 100 + n%100000
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
	case "/api/v1/provisioning/alert-rules":
		out := []map[string]any{}
		for _, x := range g.rules {
			out = append(out, map[string]any{"id": g.idOf(x.uid), "uid": x.uid, "title": x.title, "folderUID": x.folder, "isPaused": x.paused})
		}
		json.NewEncoder(w).Encode(out)
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
		held := slices.Concat(g.rules, g.hidden)
		for i, n := range g.notes {
			tags := []string{"alertname:" + n.title, "grafana_folder:Ops", "service:" + n.service, "severity:page"}
			for j := range n.extra {
				tags = append(tags, fmt.Sprintf("l%02d:v", j))
			}
			a := map[string]any{"id": i + 1, "alertName": n.title, "prevState": n.prev, "newState": n.next, "time": n.at.UnixMilli(), "tags": tags}
			if g.ids {
				a["alertId"], a["alertName"] = g.idOf(n.uid), ""
				if j := slices.IndexFunc(held, func(r shopRule) bool { return r.uid == n.uid }); j >= 0 {
					a["alertName"] = held[j].title
				}
			}
			all = append(all, a)
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
// start end" in time past now, in order; "on" for one with no end. Those
// of the beat, which keeps a rule's changes in every read, are left out.
func (sh *shop) copied() []string {
	sh.t.Helper()
	fs, err := sh.s.l.RecordedFirings(context.Background(), ledger.GrafanaSource, now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour))
	if err != nil {
		sh.t.Fatal(err)
	}
	firing.Sort(fs)
	var out []string
	for _, f := range fs {
		if f.Labels["service"] == "beat" {
			continue
		}
		end := "on"
		if !f.End.IsZero() {
			end = f.End.Sub(now).String()
		}
		out = append(out, fmt.Sprintf("%s/%s %s %s", f.Rule, f.Labels["service"], f.Start.Sub(now), end))
	}
	slices.Sort(out)
	return out
}

// beat adds a firing of title's alert for the beat, two minutes every half
// hour from from to to: every read of the history holds a change of the
// title, so that a title shared is one whose changes the history doesn't
// say the rule of.
func (g *shopGrafana) beat(title string, from, to time.Duration) {
	for at := from; at < to; at += 30 * time.Minute {
		g.fired(title, "beat", at+time.Minute, at+3*time.Minute)
	}
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
// has been on for the orders since 07:00, its beat on and off every half
// hour from 10:00 on, for two days; Stock low fired for the stock from
// 09:00 to 10:00.
func ordersAndStock() *shopGrafana {
	g := &shopGrafana{}
	g.hold(shopRule{ordersUID, "Orders slow", "Ops", false}, shopRule{stockUID, "Stock low", "Ops", false})
	g.beat("Orders slow", -2*time.Hour, 48*time.Hour)
	g.fired("Orders slow", "checkout", -30*time.Hour, -30*time.Hour+30*time.Minute)
	g.fired("Orders slow", "orders", -5*time.Hour, 0)
	g.fired("Stock low", "stock", -3*time.Hour, -2*time.Hour)
	return g
}

// A second rule of a judged rule's title, even paused, in any folder
// Lapilli reads, of a history that says no rule's uid or id: it never says
// which of the two fired. The judged rule is left out of each day while a
// read reaches a version of the rules that holds the other, alone: nothing
// of it is written or taken away, its alert on stays on, and the days are
// no record of it, then and once the other rule is gone, whatever round
// reads them again and after a restart. The other rules' days are written
// as they are, and read as copied. Judged again, an alert of it that began
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
		// the title, and stay on; so does the basket, until seven hours on.
		g.fired("Orders slow", "search", 3*time.Hour, 0)
		g.fired("Orders slow", "cart", 3*time.Hour+45*time.Minute, 0)
		g.fired("Orders slow", "basket", 3*time.Hour+10*time.Minute, 7*time.Hour)
		sh.round(4 * time.Hour)
		sh.want("still shared", sh.copied(), shared)
		sh.want("Orders slow's copies, still shared", sh.covers("Orders slow"), "→0s")

		// The other rule gone by four and a half hours on. The rounds whose
		// reads reach back to then, when the version of the rules read
		// four hours on held it, leave the rule out still.
		g.hold(shopRule{ordersUID, "Orders slow", "Ops", false}, shopRule{stockUID, "Stock low", "Ops", false})
		for _, at := range []time.Duration{4*time.Hour + 30*time.Minute, 6 * time.Hour} {
			sh.round(at)
			sh.want(fmt.Sprintf("paused %v: the other rule gone, read back to when it wasn't, at %s", paused, at), sh.copied(), shared)
			sh.want(fmt.Sprintf("paused %v: Orders slow's copies at %s", paused, at), sh.covers("Orders slow"), "→0s")
		}
		// A round shorter than --settle after: the orders' alert is ended
		// where the history says, and those on since the title was shared
		// are written from where it is the rule's alone again. What fired
		// and ended meanwhile is no one's. The round after reads from after
		// the search and the basket began: the search, of which it reads no
		// change, stays on as it is, and the basket is ended from where it
		// was written.
		after := []string{"Orders slow/basket 6h0m0s 7h0m0s", "Orders slow/cart 6h0m0s on", "Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s 10m0s",
			"Orders slow/search 6h0m0s on", "Stock low/stock -3h0m0s -2h0m0s", "Stock low/stock 1h0m0s 1h30m0s"}
		for _, at := range []time.Duration{6*time.Hour + 30*time.Minute, 8 * time.Hour} {
			sh.round(at)
			want := slices.Clone(after)
			if at < 7*time.Hour {
				want[0] = "Orders slow/basket 6h0m0s on"
			}
			sh.want(fmt.Sprintf("paused %v: the other rule gone, at %s", paused, at), sh.copied(), want)
			sh.want(fmt.Sprintf("paused %v: Orders slow's copies at %s", paused, at), sh.covers("Orders slow"), "→0s 6h0m0s→"+at.String())
		}
		// And lapilli serve started again.
		sh.restart()
		sh.round(10 * time.Hour)
		sh.want("after a restart", sh.copied(), after)
		sh.want("Orders slow's copies after a restart", sh.covers("Orders slow"), "→0s 6h0m0s→10h0m0s")
		sh.want("Stock low's copies after a restart", sh.covers("Stock low"), "→10h0m0s")
		ties, err = sh.s.l.Ties(ctx)
		if o := ties["Orders slow"]; err != nil || o.Since != now.Add(6*time.Hour).Unix() || o.To != now.Add(10*time.Hour).Unix() || o.Out != 0 {
			t.Errorf("paused %v: Orders slow's tie once judged again: %+v %v", paused, o, err)
		}
		// Its record is held from where it is judged again, not across
		// the days its title was shared.
		for _, c := range []struct {
			rule string
			at   time.Duration
			want bool
		}{{"Orders slow", -time.Hour, false}, {"Orders slow", 2 * time.Hour, false}, {"Orders slow", 5 * time.Hour, false}, {"Orders slow", 7 * time.Hour, true}, {"Stock low", -time.Hour, true}} {
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
// under the title; nor does the round after, read from where the copies
// got to, a first day.
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
	g.fired("Orders slow", "cart", 3*time.Hour+30*time.Minute, 3*time.Hour+40*time.Minute)
	// The round after a restart writes from where the copies got to,
	// across the version that held the other rule: a gap still.
	sh.restart()
	sh.round(3 * time.Hour)
	sh.want("after a restart, the other rule gone", sh.copied(), []string{"Orders slow/orders -5h0m0s -4h0m0s"})
	sh.want("its copies after a restart", sh.covers("Orders slow"), "→0s")
	sh.round(5 * time.Hour)
	sh.want("the round after", sh.copied(), []string{"Orders slow/cart 3h30m0s 3h40m0s", "Orders slow/orders -5h0m0s -4h0m0s"})
	sh.want("its copies", sh.covers("Orders slow"), "→0s 3h0m0s→5h0m0s")
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

// A rule left out for more than a day by a change passed over, judged again
// by a read from a day before the day copied (the read back from where it
// was left out holds the change): an alert of it on since before, on still
// in Grafana and changing state in none of that day, stays on, as it is
// within a week of where the rule was left out.
func TestServeGrafanaJudgedUnsureKeepsOn(t *testing.T) {
	g := ordersAndStock()
	g.fired("Stock low", "shelf", -4*time.Hour, 0)
	sh := shopServer(t, g)
	sh.round(0)
	g.note(shopNote{title: "Stock low", prev: "Normal", next: "Firing", service: "depot", at: now.Add(30 * time.Minute)})
	g.fired("Stock low", "stock", 28*time.Hour, 28*time.Hour+10*time.Minute)
	for _, at := range []time.Duration{2 * time.Hour, 27 * time.Hour, 30 * time.Hour} {
		sh.round(at)
	}
	sh.want("Stock low's copies", sh.covers("Stock low"), "→0s 26h0m0s→30h0m0s")
	got := slices.DeleteFunc(sh.copied(), func(s string) bool { return !strings.HasPrefix(s, "Stock low/") })
	sh.want("Stock low's firings", got, []string{"Stock low/shelf -4h0m0s on", "Stock low/stock -3h0m0s -2h0m0s", "Stock low/stock 28h0m0s 28h10m0s"})
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

// A firing no write holds leaves its rule out of the rounds that would
// write it alone: the round after, which reads the history back from
// where the rule was left out, holds it ended before what it writes, and
// judges the rule again.
func TestServeGrafanaJudgedUnheldOnce(t *testing.T) {
	g := ordersAndStock()
	sh := shopServer(t, g)
	sh.round(0)
	g.note(shopNote{title: "Stock low", prev: "Normal", next: "Alerting", service: "depot", at: now.Add(30 * time.Minute), extra: 40})
	g.note(shopNote{title: "Stock low", prev: "Alerting", next: "Normal", service: "depot", at: now.Add(40 * time.Minute), extra: 40})
	g.fired("Stock low", "stock", 3*time.Hour, 3*time.Hour+10*time.Minute)
	sh.round(2 * time.Hour)
	sh.want("Stock low's copies, left out", sh.covers("Stock low"), "→0s")
	for _, at := range []time.Duration{4 * time.Hour, 6 * time.Hour} {
		sh.round(at)
		sh.want(fmt.Sprintf("Stock low's copies at %s", at), sh.covers("Stock low"), "→0s 2h0m0s→"+at.String())
	}
	if got := sh.copied(); !slices.Contains(got, "Stock low/stock 3h0m0s 3h10m0s") {
		t.Errorf("Stock low judged again: %q", got)
	}
}

// An alert of a rule left out, on since before, is neither ended nor
// written again while the rule is left out, whatever the history says
// under its title; judged again, it is ended where the history says. One
// the history says ended and fired again meanwhile, still on, is ended and
// written from where the rule is judged again. The rule is left out as
// long as a round writes from when a version of the rules held the other
// rule: to six hours on.
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
	for _, at := range []time.Duration{4 * time.Hour, 6 * time.Hour} {
		sh.round(at)
		sh.want(fmt.Sprintf("left out still, at %s", at), sh.copied(), []string{"Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s on",
			"Orders slow/search -4h0m0s on", "Stock low/stock -3h0m0s -2h0m0s"})
	}
	for _, at := range []time.Duration{8 * time.Hour, 8*time.Hour + 30*time.Minute, 10 * time.Hour} {
		sh.round(at)
		sh.want(fmt.Sprintf("judged again, at %s", at), sh.copied(), []string{"Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s 10m0s",
			"Orders slow/search -4h0m0s 20m0s", "Orders slow/search 6h0m0s on", "Stock low/stock -3h0m0s -2h0m0s"})
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
	g.fired("Orders slow", "search", time.Hour, 9*time.Hour)
	// Left out two hours on, and the two rounds after, which write from
	// when a version of the rules held the other rule.
	g.hold(two...)
	sh.round(2 * time.Hour)
	g.hold(one...)
	sh.round(4 * time.Hour)
	sh.round(6 * time.Hour)
	sh.round(8 * time.Hour)
	if got := sh.copied(); !slices.Contains(got, "Orders slow/search 6h0m0s on") {
		t.Errorf("judged again: %q", got)
	}
	g.hold(two...)
	sh.round(10 * time.Hour)
	g.hold(one...)
	sh.round(12 * time.Hour)
	sh.round(14 * time.Hour)
	for _, at := range []time.Duration{16 * time.Hour, 18 * time.Hour} {
		sh.round(at)
		sh.want(fmt.Sprintf("judged again a second time, at %s", at), sh.copied(), []string{"Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s 10m0s",
			"Orders slow/search 6h0m0s 9h0m0s", "Stock low/stock -3h0m0s -2h0m0s"})
		sh.want("its copies", sh.covers("Orders slow"), "→0s 6h0m0s→8h0m0s 14h0m0s→"+at.String())
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
	// A gap two hours on, and in the round after, which writes from when a
	// version of the rules held the other rule.
	g.hold(two...)
	sh.round(2 * time.Hour)
	g.hold(one...)
	sh.round(2*time.Hour + 30*time.Minute)
	// The first round after the gap; the next reads from after the search
	// and the cart began.
	on := []string{"Orders slow/cart 2h30m0s on", "Orders slow/orders -5h0m0s 20m0s", "Orders slow/orders 2h30m0s on", "Orders slow/search 2h30m0s on"}
	for _, at := range []time.Duration{3*time.Hour + 30*time.Minute, 4*time.Hour + 30*time.Minute} {
		sh.round(at)
		sh.want(fmt.Sprintf("after the gap, at %s", at), sh.copied(), on)
	}
	// A second gap, the search ending in it.
	g.hold(two...)
	sh.round(6*time.Hour + 30*time.Minute)
	g.hold(one...)
	sh.round(7 * time.Hour)
	for _, at := range []time.Duration{8*time.Hour + 30*time.Minute, 10 * time.Hour} {
		sh.round(at)
		sh.want(fmt.Sprintf("after a second gap, at %s", at), sh.copied(), []string{"Orders slow/cart 2h30m0s on", "Orders slow/orders -5h0m0s 20m0s",
			"Orders slow/orders 2h30m0s on", "Orders slow/search 2h30m0s 5h0m0s"})
	}
	spans, err := sh.s.l.Spans(context.Background(), ledger.GrafanaSource)
	if err != nil || len(spans) != 3 || !spans[0][1].Equal(now) || !spans[1][0].Equal(now.Add(2*time.Hour+30*time.Minute)) || !spans[1][1].Equal(now.Add(4*time.Hour+30*time.Minute)) ||
		!spans[2][0].Equal(now.Add(7*time.Hour)) {
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
	os.WriteFile(file, []byte("expectations:\n  - rule: \"Orders slow [grafana]\"\n    budget: {week: 500}\n  - rule: \"Stock low [grafana]\"\n    budget: {week: 500}\n"), 0o600)
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

// An alert that began while its rule was left out (its title shared), on
// where the rule is judged again and ended soon after: the round the rule
// comes back in, four hours on (the rounds before write from when a
// version of the rules held the other rule), writes it from there, ended,
// and every round after, which reads the settle again from after it
// began, keeps it so, as one the ledger has from within the write.
// Whether it began before the settle the first round reads or within it;
// and as lapilli serve runs unless told otherwise, a round an hour, the
// hour before read again.
func TestServeGrafanaJudgedLeftOutEndedSoon(t *testing.T) {
	one := []shopRule{{ordersUID, "Orders slow", "Ops", false}, {stockUID, "Stock low", "Ops", false}}
	two := append(slices.Clone(one), shopRule{"elsewhere", "Orders slow", "Elsewhere", true})
	for _, began := range []time.Duration{30 * time.Minute, 3*time.Hour + 30*time.Minute} {
		g := ordersAndStock()
		sh := shopServer(t, g)
		sh.round(0)
		g.hold(two...)
		g.fired("Orders slow", "search", began, 4*time.Hour+10*time.Minute)
		sh.round(2 * time.Hour)
		g.hold(one...)
		sh.round(2*time.Hour + 30*time.Minute)
		sh.round(4 * time.Hour)
		want := []string{"Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s on", "Orders slow/search 4h0m0s 4h10m0s", "Stock low/stock -3h0m0s -2h0m0s"}
		for _, at := range []time.Duration{5 * time.Hour, 5*time.Hour + 30*time.Minute, 7 * time.Hour} {
			sh.round(at)
			sh.want(fmt.Sprintf("began %s: judged again, at %s", began, at), sh.copied(), want)
		}
		sh.want("its copies", sh.covers("Orders slow"), "→0s 4h0m0s→7h0m0s")
	}

	g := ordersAndStock()
	sh := shopServer(t, g)
	sh.round(0)
	sh.round(time.Hour)
	g.hold(two...)
	g.fired("Orders slow", "search", 2*time.Hour+30*time.Minute, 5*time.Hour+20*time.Minute)
	sh.round(2 * time.Hour)
	sh.round(3 * time.Hour)
	g.hold(one...)
	sh.round(4 * time.Hour)
	sh.round(5 * time.Hour)
	want := []string{"Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s on", "Orders slow/search 5h0m0s 5h20m0s", "Stock low/stock -3h0m0s -2h0m0s"}
	for h := 6; h <= 8; h++ {
		sh.round(time.Duration(h) * time.Hour)
		sh.want(fmt.Sprintf("hourly: judged again from 5h, at %dh", h), sh.copied(), want)
	}
	sh.want("hourly: its copies", sh.covers("Orders slow"), "→1h0m0s 5h0m0s→8h0m0s")
}

// The same of a gap of the source's (the one judged rule's title shared):
// an alert on at the gap's end that ended before the first round after it
// read.
func TestServeGrafanaJudgedGapEndedSoon(t *testing.T) {
	g := &shopGrafana{}
	one := []shopRule{{ordersUID, "Orders slow", "Ops", false}}
	two := append(slices.Clone(one), shopRule{"elsewhere", "Orders slow", "Elsewhere", true})
	g.hold(one...)
	g.fired("Orders slow", "orders", -5*time.Hour, -4*time.Hour)
	sh := shopServer(t, g)
	sh.round(0)
	g.fired("Orders slow", "search", 30*time.Minute, 2*time.Hour+40*time.Minute)
	g.hold(two...)
	sh.round(2 * time.Hour)
	g.hold(one...)
	sh.round(2*time.Hour + 30*time.Minute) // a gap still
	want := []string{"Orders slow/orders -5h0m0s -4h0m0s", "Orders slow/search 2h30m0s 2h40m0s"}
	// The first round after the gap, and the next, which writes from where
	// it has the alert begin, and reads from after it began.
	for _, at := range []time.Duration{3*time.Hour + 30*time.Minute, 4*time.Hour + 30*time.Minute, 6 * time.Hour} {
		sh.round(at)
		sh.want(fmt.Sprintf("after the gap, at %s", at), sh.copied(), want)
	}
}

// A rule of a judged rule's title in another folder, made between two
// rounds, firing, and deleted before the second, of a history that says
// each change's rule (alertId, alertName): what it fired is none of the
// judged rule's, whether a read of the rules saw it (--watch) or not, and
// the judged rule isn't left out. Nor does an alert of it with the labels
// of one the judged rule has on end that one.
func TestServeGrafanaJudgedSharedBetweenRounds(t *testing.T) {
	for _, seen := range []bool{false, true} {
		g := ordersAndStock()
		g.ids = true
		sh := shopServer(t, g)
		sh.round(0)
		before := sh.copied()
		one := []shopRule{{ordersUID, "Orders slow", "Ops", false}, {stockUID, "Stock low", "Ops", false}}
		g.hold(append(slices.Clone(one), shopRule{"elsewhere", "Orders slow", "Elsewhere", false})...)
		for i := range 12 {
			at := 20*time.Minute + time.Duration(i)*5*time.Minute
			g.firedBy("elsewhere", "Orders slow", "payments", at, at+2*time.Minute)
		}
		g.firedBy("elsewhere", "Orders slow", "orders", 30*time.Minute, 40*time.Minute)
		if seen {
			sh.s.now = func() time.Time { return now.Add(30 * time.Minute) }
			raw := ""
			if err := sh.s.look(context.Background(), &raw); err != nil {
				t.Fatal(err)
			}
		}
		g.hold(one...)
		for _, at := range []time.Duration{2 * time.Hour, 4 * time.Hour} {
			sh.round(at)
			sh.want(fmt.Sprintf("seen %v: at %s", seen, at), sh.copied(), before)
			sh.want(fmt.Sprintf("seen %v: its copies at %s", seen, at), sh.covers("Orders slow"), "→"+at.String())
		}
		if out, err := sh.s.l.LeftOut(context.Background(), ledger.GrafanaSource); err != nil || len(out) != 0 {
			t.Errorf("seen %v: left out %v %v", seen, out, err)
		}
	}
}

// Two rules of one title in two folders, both judged, of a history that
// says each change's rule: each has what it fired, and neither is left out.
func TestServeGrafanaJudgedSharedTold(t *testing.T) {
	ctx := context.Background()
	g := ordersAndStock()
	g.ids = true
	sh := shopServer(t, g)
	sh.round(0)
	g.hold(shopRule{ordersUID, "Orders slow", "Ops", false}, shopRule{stockUID, "Stock low", "Ops", false}, shopRule{"elsewhere", "Orders slow", "Elsewhere", false})
	g.firedBy("elsewhere", "Orders slow", "payments", 30*time.Minute, 40*time.Minute)
	g.fired("Orders slow", "search", time.Hour, 70*time.Minute)
	sh.round(2 * time.Hour)
	names, err := sh.s.l.FromHistory(ctx)
	if err != nil || len(names) != 3 {
		t.Fatalf("judged: %q %v", names, err)
	}
	other := names[slices.IndexFunc(names, func(n string) bool { return n != "Orders slow" && n != "Stock low" })]
	want := []string{"Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s on", "Orders slow/search 1h0m0s 1h10m0s",
		other + "/payments 30m0s 40m0s", "Stock low/stock -3h0m0s -2h0m0s"}
	slices.Sort(want)
	sh.want("each its own", sh.copied(), want)
	sh.want("its copies", sh.covers("Orders slow"), "→2h0m0s")
	if out, err := sh.s.l.LeftOut(ctx, ledger.GrafanaSource); err != nil || len(out) != 0 {
		t.Errorf("left out %v %v", out, err)
	}
}

// A rule of a judged rule's title in a folder the token doesn't read, of a
// history that says each change's rule: what it fires, while the judged
// rule changes state in none of the read, is none of the judged rule's,
// nor does an alert of it with the labels of one the judged rule has on
// end that one. The judged rule's id is the one Grafana lists its uid
// with, not one the read alone holds of its title.
func TestServeGrafanaJudgedHiddenFolder(t *testing.T) {
	g := &shopGrafana{ids: true}
	g.hold(shopRule{ordersUID, "Orders slow", "Ops", false}, shopRule{stockUID, "Stock low", "Ops", false})
	g.hidden = []shopRule{{"teamb", "Orders slow", "Team B", false}}
	g.fired("Orders slow", "orders", -5*time.Hour, 0)
	g.fired("Stock low", "stock", -3*time.Hour, -2*time.Hour)
	sh := shopServer(t, g)
	sh.round(0)
	sh.want("copied in", sh.copied(), []string{"Orders slow/orders -5h0m0s on", "Stock low/stock -3h0m0s -2h0m0s"})
	g.firedBy("teamb", "Orders slow", "payments", 30*time.Minute, 40*time.Minute)
	g.firedBy("teamb", "Orders slow", "orders", 50*time.Minute, 70*time.Minute)
	g.fired("Stock low", "stock", time.Hour, 90*time.Minute)
	sh.round(2 * time.Hour)
	sh.want("beside a rule of its title the token doesn't read", sh.copied(), []string{"Orders slow/orders -5h0m0s on",
		"Stock low/stock -3h0m0s -2h0m0s", "Stock low/stock 1h0m0s 1h30m0s"})
	sh.want("its copies", sh.covers("Orders slow"), "→2h0m0s")
}

// The same rule, of a history that says no rule's id: a read of the rules
// (--watch) saw it, and the rule is left out of what is written across the
// version that held it.
func TestServeGrafanaJudgedSharedBetweenRoundsUntold(t *testing.T) {
	g := ordersAndStock()
	sh := shopServer(t, g)
	sh.round(0)
	before := sh.copied()
	one := []shopRule{{ordersUID, "Orders slow", "Ops", false}, {stockUID, "Stock low", "Ops", false}}
	g.hold(append(slices.Clone(one), shopRule{"elsewhere", "Orders slow", "Elsewhere", false})...)
	for i := range 12 {
		at := 20*time.Minute + time.Duration(i)*5*time.Minute
		g.firedBy("elsewhere", "Orders slow", "payments", at, at+2*time.Minute)
	}
	sh.s.now = func() time.Time { return now.Add(30 * time.Minute) }
	raw := ""
	if err := sh.s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	g.hold(one...)
	sh.round(2 * time.Hour)
	sh.want("copied in", sh.copied(), before)
	sh.want("its copies", sh.covers("Orders slow"), "→0s")
}

// A rule left out for longer than a day (its title shared for 28 hours) is
// read back from where it was left out once it is judged again: an alert
// of it on since before, which ended in the first hours it was left out,
// is ended there; one that began then and is still on is written from
// where the rule is judged again. So it is beside another rule that read
// leaves out, of a change passed over then (Stock low's, a state Grafana
// has no name for), which a read from a day before judges.
func TestServeGrafanaJudgedLeftOutLong(t *testing.T) {
	g := ordersAndStock()
	sh := shopServer(t, g)
	sh.round(0)
	one := []shopRule{{ordersUID, "Orders slow", "Ops", false}, {stockUID, "Stock low", "Ops", false}}
	two := append(slices.Clone(one), shopRule{"elsewhere", "Orders slow", "Elsewhere", true})
	g.hold(two...)
	g.note(shopNote{title: "Orders slow", prev: "Alerting", next: "Normal", service: "orders", at: now.Add(10 * time.Minute)})
	g.fired("Orders slow", "search", time.Hour, 0)
	g.note(shopNote{title: "Stock low", prev: "Normal", next: "Firing", service: "depot", at: now.Add(30 * time.Minute)})
	// Stock low keeps the history reaching each day.
	for h := 2; h <= 40; h += 4 {
		g.fired("Stock low", "stock", time.Duration(h)*time.Hour+5*time.Minute, time.Duration(h)*time.Hour+15*time.Minute)
	}
	for _, at := range []time.Duration{2 * time.Hour, 10 * time.Hour, 20 * time.Hour, 28 * time.Hour} {
		sh.round(at)
	}
	g.hold(one...)
	// The two rounds after write from when a version of the rules held the
	// other rule; the one after those judges it again, from 32 hours on.
	for _, at := range []time.Duration{30 * time.Hour, 32 * time.Hour, 36 * time.Hour} {
		sh.round(at)
	}
	got := slices.DeleteFunc(sh.copied(), func(s string) bool { return !strings.HasPrefix(s, "Orders slow/") })
	sh.want("judged again after 32 hours", got, []string{"Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s 10m0s", "Orders slow/search 32h0m0s on"})
	sh.want("its copies", sh.covers("Orders slow"), "→0s 32h0m0s→36h0m0s")
	// Stock low itself, left out while a read a day back holds the change.
	sh.want("Stock low's copies", sh.covers("Stock low"), "→0s 28h0m0s→36h0m0s")
}

// Left out for longer than a week, a rule is read back a week alone: an
// alert of it on since before, that the read says nothing of, is ended
// where the rule was left out, not left on.
func TestServeGrafanaJudgedLeftOutPastAWeek(t *testing.T) {
	g := ordersAndStock()
	sh := shopServer(t, g)
	sh.round(0)
	one := []shopRule{{ordersUID, "Orders slow", "Ops", false}, {stockUID, "Stock low", "Ops", false}}
	two := append(slices.Clone(one), shopRule{"elsewhere", "Orders slow", "Elsewhere", true})
	g.hold(two...)
	g.note(shopNote{title: "Orders slow", prev: "Alerting", next: "Normal", service: "orders", at: now.Add(10 * time.Minute)})
	g.beat("Orders slow", 48*time.Hour, 200*time.Hour)
	for h := 2; h <= 200; h += 4 {
		g.fired("Stock low", "stock", time.Duration(h)*time.Hour+5*time.Minute, time.Duration(h)*time.Hour+15*time.Minute)
	}
	for h := 2; h <= 194; h += 24 {
		sh.round(time.Duration(h) * time.Hour)
	}
	g.hold(one...)
	for _, at := range []time.Duration{196 * time.Hour, 198 * time.Hour, 200 * time.Hour} {
		sh.round(at)
	}
	got := slices.DeleteFunc(sh.copied(), func(s string) bool { return !strings.HasPrefix(s, "Orders slow/") })
	sh.want("judged again after eight days", got, []string{"Orders slow/checkout -30h0m0s -29h30m0s", "Orders slow/orders -5h0m0s 0s"})
	sh.want("its copies", sh.covers("Orders slow"), "→0s 198h0m0s→200h0m0s")
}
