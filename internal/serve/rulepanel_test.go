package serve

// A rule's panel beside the span before its period: as long as the period,
// and set beside only where what each number comes from holds all of it.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// besideServer is a ledger replayed from June on, in which Errors fires
// every day at 03:00 and Fresh only from this Monday on; Disk, recorded
// elsewhere, copied in over the last ten days; and the notifications of
// Errors, one in May and each day's since September.
func besideServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	s, _ := server(t, nil)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	o := s.cfg.Options
	monday := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for w := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC); w.Before(now); w = w.Add(7 * 24 * time.Hour) {
		to := w.Add(7 * 24 * time.Hour)
		if to.After(now) {
			to = now
		}
		var fs []firing.Firing
		for d := w; d.Before(to); d = d.Add(24 * time.Hour) {
			fs = append(fs, firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "search"}, Start: d.Add(3 * time.Hour), End: d.Add(3*time.Hour + time.Minute)})
			if !d.Before(monday) {
				fs = append(fs, firing.Firing{Rule: "Fresh", Labels: map[string]string{"service": "search"}, Start: d.Add(5 * time.Hour), End: d.Add(5*time.Hour + time.Minute)})
			}
		}
		if err := s.l.Record(ctx, ledger.Found{From: w, To: to.Add(-time.Second), Firings: fs,
			Run: ledger.Run{Kind: "backfill", Started: w, Finished: w, From: w, To: to}}, o); err != nil {
			t.Fatal(err)
		}
	}
	var disk []firing.Firing
	for d := now.Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour); d.Before(now); d = d.Add(24 * time.Hour) {
		if at := d.Add(4 * time.Hour); at.After(now.Add(-10*24*time.Hour)) && at.Before(now) {
			disk = append(disk, firing.Firing{Rule: "Disk", Labels: map[string]string{"service": "db"}, Start: at, End: at.Add(time.Minute)})
		}
	}
	if _, err := s.l.PutFirings(ctx, "cw", now.Add(-10*24*time.Hour), now, disk, ledger.FiringsCopier, o); err != nil {
		t.Fatal(err)
	}
	notify := func(at time.Time, start time.Time) {
		t.Helper()
		if _, err := s.l.Notify(ctx, ledger.Notification{Source: "alertmanager", Receiver: "oncall", Group: fmt.Sprint("g", at.Unix()), At: at,
			Alerts: []ledger.Alert{{Rule: "Errors", Labels: map[string]string{"service": "search"}, Start: start}}}); err != nil {
			t.Fatal(err)
		}
	}
	notify(time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC), time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC))
	for d := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC); d.Before(now); d = d.Add(24 * time.Hour) {
		notify(d.Add(3*time.Hour+time.Minute), d.Add(3*time.Hour))
	}
	return s
}

// ruleViewOf is a rule's page's view, as its HTML page has it.
func ruleViewOf(t *testing.T, s *Server, name, q string) ruleView {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/rules/"+name+"?"+q, nil)
	r.SetPathValue("name", name)
	v, err := s.rulePage(r)
	if err != nil {
		t.Fatal(err)
	}
	return v.(ruleView)
}

// TestPanelBesideHeld: a number is set beside the span before only when
// what it comes from holds all of that span: the replay's runs for a rule
// replayed, the copies for one recorded elsewhere, a cloud's alarm's
// history from when its tie says, the notifications from the first kept
// and in the pages; and none of it pruned (--keep).
func TestPanelBesideHeld(t *testing.T) {
	s := besideServer(t)
	type on struct{ firings, notifications bool }
	for _, c := range []struct {
		rule, q string
		keep    time.Duration
		want    on
	}{
		// Replayed from June: the span before four weeks, and one week,
		// held; twelve weeks before, from the end of May, not.
		{"Errors", "weeks=1", 0, on{true, true}},
		{"Errors", "weeks=4", 0, on{true, true}},
		{"Errors", "weeks=12", 0, on{false, false}},
		// Copied in over the last ten days: the week before held, four
		// weeks before not, though runs were replayed from June.
		{"Disk", "weeks=1", 0, on{true, false}},
		{"Disk", "weeks=4", 0, on{false, false}},
		// Pruned past thirty days: four weeks before, which starts 44
		// days ago, not; a week before, held.
		{"Errors", "weeks=4", 30 * 24 * time.Hour, on{false, false}},
		{"Errors", "weeks=1", 30 * 24 * time.Hour, on{true, true}},
	} {
		s.cfg.Keep = c.keep
		v := ruleViewOf(t, s, c.rule, c.q)
		got := on{v.Panel.FiringsBy.On, v.Panel.NotificationsBy.On}
		if got != c.want || v.Panel.IncidentsBy.On != got.firings || v.Before.IsZero() == got.firings {
			t.Errorf("%s?%s, keep %s: beside %+v (before from %s), want %+v", c.rule, c.q, c.keep, got, v.Before, c.want)
		}
		if got.firings && v.Panel.FiringsBy.By != v.Firings-firedIn(t, s, c.rule, v.Before, v.From) {
			t.Errorf("%s?%s: firings changed %+v, %d now", c.rule, c.q, v.Panel.FiringsBy, v.Firings)
		}
	}
	s.cfg.Keep = 0
	// The notifications over four weeks, beside those of the span before.
	v := ruleViewOf(t, s, "Errors", "weeks=4")
	ns, err := s.l.Notifications(context.Background(), v.Before, v.From)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Panel.NotificationsBy; got.By != v.Notifications-len(ns) || len(ns) == 0 || v.Notifications == 0 {
		t.Errorf("weeks=4: notifications %d changed %+v, %d before", v.Notifications, got, len(ns))
	}

	// A cloud's alarm's history, from when its tie says.
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		tie  tieView
		want bool
	}{
		{tieView{Source: "cw", Since: at}, true}, {tieView{Source: "cw", Since: at.Add(time.Second)}, false},
		{tieView{Source: "cw"}, false}, {tieView{Why: "no history"}, false},
	} {
		tie := c.tie
		if got, err := s.holds(context.Background(), &ruleView{Tie: &tie}, at); err != nil || got != c.want {
			t.Errorf("%+v from %s: %v %v", c.tie, at, got, err)
		}
	}
}

// firedIn is how many of a rule's firings the ledger has judged that
// started in [from, to).
func firedIn(t *testing.T, s *Server, rule string, from, to time.Time) int {
	t.Helper()
	fs, err := s.l.Firings(context.Background(), ledger.FiringQuery{From: from, To: to, Rule: rule, Limit: 10000})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range fs {
		if !f.Excused {
			n++
		}
	}
	return n
}

// TestPanelBesideNone: precision is set beside the span before only when
// the rule fired in both; firings and incidents, whenever the span is
// held. The page shows what is set beside, and nothing else.
func TestPanelBesideNone(t *testing.T) {
	precision, firings, incidents := besides(ruleLine{Firings: 4, White: 1, Incidents: 1}, ledger.Span{})
	if precision.On || !firings.On || firings.By != 4 || !incidents.On || incidents.By != 1 {
		t.Errorf("none before: %+v %+v %+v", precision, firings, incidents)
	}
	if precision, _, _ = besides(ruleLine{}, ledger.Span{Firings: 3, White: 3}); precision.On {
		t.Errorf("none now: %+v", precision)
	}
	if precision, _, _ = besides(ruleLine{Firings: 4, White: 1}, ledger.Span{Firings: 2, White: 1}); !precision.On || precision.By != 25-50 {
		t.Errorf("25%% beside 50%%: %+v", precision)
	}
	s := besideServer(t)
	_, body, _ := get(t, s.Handler(), "/rules/Fresh?weeks=1")
	panel, _, _ := strings.Cut(body[strings.Index(body, `<dl class="panel">`):], "</dl>")
	if strings.Contains(panel, " pt") || !strings.Contains(panel, `<span class="delta worse">&#43;2`) || !strings.Contains(body, "Beside each number") {
		t.Errorf("/rules/Fresh?weeks=1: a precision beside none, or firings not beside:\n%s", panel)
	}
}

// TestSharePct: a share's change is that of the shares the page shows,
// rounded as they are.
func TestSharePct(t *testing.T) {
	for all := 1; all <= 200; all++ {
		for n := 0; n <= all; n++ {
			if got, _ := sharePct(n, all); fmt.Sprintf("%d%%", got) != pct(n, all) {
				t.Fatalf("%d of %d: %d%%, the page %s", n, all, got, pct(n, all))
			}
		}
	}
	if _, ok := sharePct(0, 0); ok {
		t.Error("a share of none")
	}
}

// TestRuleOnlyHTML: a rule's header, panel and strip are worked out for its
// page alone, and each version beside the one before only on its versions
// tab; MCP's rule tool, which reads the rest, has none of them.
func TestRuleOnlyHTML(t *testing.T) {
	s := glanceServer(t)
	r := httptest.NewRequest(http.MethodGet, "/rules/Errors", nil)
	r.SetPathValue("name", "Errors")
	v, err := s.rule(r, false)
	if err != nil {
		t.Fatal(err)
	}
	if v.Strip != nil || v.Panel != (rulePanel{}) || v.Conds != nil || v.Facts != nil || v.Tabs != nil || len(v.Changes) < 2 {
		t.Errorf("not for the page: strip %v, panel %+v, header %v %v %v, %d versions", v.Strip != nil, v.Panel, v.Conds, v.Facts, v.Tabs, len(v.Changes))
	}
	for tab, diffs := range map[string]bool{"": false, "tab=firings": false, "tab=versions": true} {
		v := ruleViewOf(t, s, "Errors", tab)
		if v.Strip == nil || len(v.Tabs) == 0 {
			t.Errorf("%s: the page has no strip or tabs", tab)
		}
		for _, c := range v.Changes {
			if (len(c.Diff) > 0) != (diffs && !c.First) {
				t.Errorf("%s: a version from %s with %d lines of diff", tab, c.Since, len(c.Diff))
			}
		}
	}
	// MCP's rule tool says the same as before: it has no strip to say.
	_, out, err := s.toolRule(context.Background(), nil, ruleIn{Name: "Errors"})
	if err != nil || out.Rule.Name != "Errors" || len(out.Versions) != len(v.Changes) {
		t.Errorf("the rule tool: %v %+v", err, out.Rule)
	}
}

// TestRulePageCached: a rule's page is worked out once for what it reads
// of its query, however it is asked: what else is in the URL, in any
// order, is nothing to it, and isn't said back on it.
func TestRulePageCached(t *testing.T) {
	s := glanceServer(t)
	h := s.Handler()
	var first string
	for i, q := range []string{"weeks=4&tab=firings", "tab=firings&weeks=4", "tab=firings&weeks=4&zz=1", "zz=2&weeks=4&tab=firings&tab=versions",
		"tab=firings&weeks=4&edit=x&weeks=12", "tab=firings&weeks=4&lang=ko&utm_source=x"} {
		code, body, hdr := get(t, h, "/rules/Errors?"+q)
		if code != http.StatusOK {
			t.Fatalf("%s: %d", q, code)
		}
		if strings.Contains(body, "zz") || strings.Contains(body, "utm_source") {
			t.Errorf("%s: the page says back what it doesn't read", q)
		}
		if i == 0 {
			first = hdr.Get("ETag")
		} else if hdr.Get("ETag") != first {
			t.Errorf("%s: ETag %s, not %s", q, hdr.Get("ETag"), first)
		}
	}
	s.cache.Lock()
	n := len(s.cache.body)
	s.cache.Unlock()
	if n != 1 {
		t.Errorf("%d pages cached, want 1", n)
	}
	// What it reads, it is cached by.
	for _, q := range []string{"tab=firings&weeks=1", "tab=firings&weeks=4&team=", "tab=firings&weeks=4&edit=note", "weeks=4"} {
		get(t, h, "/rules/Errors?"+q)
	}
	s.cache.Lock()
	n = len(s.cache.body)
	s.cache.Unlock()
	if n != 5 {
		t.Errorf("%d pages cached, want 5", n)
	}
}
