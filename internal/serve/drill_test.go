package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// drillServer is a server with last week (from Monday 2026-10-26) of two
// teams' rules: a noisy one of each, one of payments that caught an
// incident, one of search that did, an incident caught only by search's
// noisy rule, and one of both teams' too, one nothing caught; firings that re-fired, some while people
// slept, some during a deploy; and the weeks before and after with some
// of their own. With notify, the firings were notified too, a minute after
// each started: one, at 23:59:30 the Sunday before, notified in the week;
// and one began the week before, still firing, was notified again in it;
// but one, two weeks before, that came before any notification was kept.
func drillServer(t *testing.T, notify bool) *Server {
	t.Helper()
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Options.Hours = verdict.DefaultHours(time.UTC) })
	raw := ""
	if err := s.look(ctx, &raw); err != nil { // Errors, in effect, never fired
		t.Fatal(err)
	}
	week := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	var fs []firing.Firing
	add := func(rule string, at time.Time, labels ...string) {
		ls := map[string]string{}
		for i := 0; i+1 < len(labels); i += 2 {
			ls[labels[i]] = labels[i+1]
		}
		fs = append(fs, firing.Firing{Rule: rule, Start: at, End: at.Add(4 * time.Minute), Labels: ls})
	}
	for k := 0; k < 12; k++ { // noisy, at night: re-fires, the same alert again within the hour
		add("PayErrors", week.Add(25*time.Hour+time.Duration(k)*10*time.Minute), "service", "pay")
	}
	add("PayLatency", week.Add(57*time.Hour+55*time.Minute), "service", "pay") // catches P-1
	add("PayLatency", week.Add(80*time.Hour), "service", "pay", "pod", "a")
	add("PayLatency", week.Add(81*time.Hour), "service", "pay", "pod", "b")
	for k := 0; k < 15; k++ { // noisy, the first during a deploy, the last catching N-1
		add("SearchNoise", week.Add(75*time.Hour+time.Duration(k)*30*time.Minute), "service", "search", "k", fmt.Sprint(k))
	}
	add("SearchSlow", week.Add(100*time.Hour+50*time.Minute), "service", "search") // catches S-1
	add("SearchSlow", week.Add(140*time.Hour), "service", "search", "pod", "c")
	// The weeks around it.
	add("PayErrors", week.Add(-3*24*time.Hour), "service", "pay")
	add("PayErrors", week.Add(8*24*time.Hour+time.Hour), "service", "pay")
	add("SearchSlow", week.Add(8*24*time.Hour), "service", "search") // catches S-2
	if notify {
		add("PayOld", week.Add(-12*24*time.Hour), "service", "pay") // before the first notification
		add("PayNight", week.Add(-30*time.Second), "service", "pay")
		fs = append(fs, firing.Firing{Rule: "PayLong", Start: week.Add(-6*24*time.Hour + 3*time.Hour), End: week.Add(2 * 24 * time.Hour),
			Labels: map[string]string{"service": "pay"}})
	}
	if err := s.l.Record(ctx, ledger.Found{From: week.Add(-14 * 24 * time.Hour), To: now, Firings: fs, Run: ledger.Run{Kind: "backfill", Started: week, Finished: week}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	for _, tm := range []ledger.Team{{Name: "payments", Rules: "^Pay", Labels: incident.Labels{"service": {"pay"}}},
		{Name: "search", Rules: "^Search", Labels: incident.Labels{"service": {"search"}}}} {
		if _, err := s.l.PutTeam(ctx, tm, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	o := s.cfg.Options
	if _, err := s.l.SyncDeploys(ctx, []ledger.Deploy{{Start: week.Add(74*time.Hour + 58*time.Minute), End: week.Add(75*time.Hour + 5*time.Minute),
		Labels: map[string]string{"service": "search"}}}, o, ledger.DeploysRead{}); err != nil {
		t.Fatal(err)
	}
	for _, in := range []incident.Incident{
		{ID: "P-1", Start: week.Add(58 * time.Hour), End: week.Add(59 * time.Hour), Labels: incident.Labels{"service": {"pay"}}},
		{ID: "S-1", Start: week.Add(101 * time.Hour), End: week.Add(102 * time.Hour), Labels: incident.Labels{"service": {"search"}}},
		{ID: "N-1", Start: week.Add(82*time.Hour + 5*time.Minute), End: week.Add(83 * time.Hour), Labels: incident.Labels{"service": {"search"}}},
		{ID: "M-1", Start: week.Add(120 * time.Hour), End: week.Add(121 * time.Hour), Labels: incident.Labels{"service": {"pay"}}},
		{ID: "M-2", Start: week.Add(121 * time.Hour), End: week.Add(122 * time.Hour), Labels: incident.Labels{"service": {"search"}}},
		// Both teams', caught only by search's noisy rule.
		{ID: "B-1", Start: week.Add(76*time.Hour + 5*time.Minute), End: week.Add(76*time.Hour + 20*time.Minute), Labels: incident.Labels{"service": {"pay", "search"}}},
		// This week.
		{ID: "M-3", Start: week.Add(7*24*time.Hour + 10*time.Hour), End: week.Add(7*24*time.Hour + 11*time.Hour), Labels: incident.Labels{"service": {"pay"}}},
		{ID: "S-2", Start: week.Add(8*24*time.Hour + 5*time.Minute), End: week.Add(8*24*time.Hour + time.Hour), Labels: incident.Labels{"service": {"search"}}},
	} {
		if _, err := s.l.Put(ctx, in, "ana", o); err != nil {
			t.Fatal(err)
		}
	}
	if notify {
		for _, f := range fs {
			if f.Rule == "PayOld" {
				continue
			}
			if _, err := s.l.Notify(ctx, ledger.Notification{Source: "alertmanager", Receiver: "oncall", Group: fmt.Sprint(f.Rule, f.Start.Unix()), At: f.Start.Add(time.Minute),
				Alerts: []ledger.Alert{{Rule: f.Rule, Labels: f.Labels, Start: f.Start}}}); err != nil {
				t.Fatal(err)
			}
		}
		long := fs[len(fs)-1]
		if _, err := s.l.Notify(ctx, ledger.Notification{Source: "alertmanager", Receiver: "oncall", Group: fmt.Sprint(long.Rule, long.Start.Unix()),
			At: week.Add(27 * time.Hour), Alerts: []ledger.Alert{{Rule: long.Rule, Labels: long.Labels, Start: long.Start}}}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// viewOf is a page's view for a URL, as the page works it out.
func viewOf(t *testing.T, s *Server, name, target string, view func(*http.Request) (any, error)) any {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	_, vr := s.withContext(r, name)
	v, err := view(vr)
	if err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	return v
}

// TestDrillDown: each tile of the overview links to the list that makes
// its number, with the same team and over the same week, and that list,
// all told, comes to the tile's number: for every team.
func TestDrillDown(t *testing.T) {
	for _, notify := range []bool{false, true} {
		drillDown(t, drillServer(t, notify), notify)
	}
}

func drillDown(t *testing.T, s *Server, notify bool) {
	for _, team := range []string{"", "payments", "search"} {
		q := ""
		if team != "" {
			q = "team=" + team
		}
		d := viewOf(t, s, "overview", "/?"+q, s.overview).(dashboard)
		last := d.Weeks[1] // the latest first: this week, then last
		if got := last.Start.Format("2006-01-02"); got != "2026-10-26" {
			t.Fatalf("last week is %s", got)
		}
		seen := map[string]bool{}
		for _, x := range d.Tiles {
			seen[x.key] = true
			if x.Href == "" {
				t.Errorf("%s %q: no link", team, x.key)
				continue
			}
			u, _ := url.Parse(x.Href)
			dq := u.Query()
			if dq.Get("week") != "2026-10-26" {
				t.Errorf("%s %q: to %s, not last week", team, x.key, x.Href)
			}
			if team != "" {
				dq.Set("team", team)
			}
			target := u.Path + "?" + dq.Encode()
			what := fmt.Sprintf("%s %q → %s", cmpOr(team, "all"), x.key, target)
			switch u.Path {
			case "/rules":
				rl := viewOf(t, s, "rules", target, s.rulesList).(rulesList)
				if rl.Week != "2026-10-26" {
					t.Errorf("%s: over %q", what, rl.Week)
				}
				woken := fmt.Sprint(rl.Sum.AsleepBlack)
				if notify {
					// Notifications, once they are kept, as the tile says:
					// what the rows shown come to.
					woken = fmt.Sprint(rl.Sum.Woken)
					rows := 0
					for _, r := range rl.Rows {
						rows += r.Woken
					}
					if rows != rl.Sum.Woken {
						t.Errorf("%s: the rows come to %d woken, the list %d", what, rows, rl.Sum.Woken)
					}
				}
				got := map[string]string{"Precision": rl.Sum.Precision, "Firings": fmt.Sprint(rl.Sum.Firings), "Noisy rules": fmt.Sprint(rl.Shown),
					"Refired": fmt.Sprint(rl.Sum.Refired), "During deploys": fmt.Sprint(rl.Sum.Deploys), "Woken for nothing": woken}[x.key]
				if got != x.Value {
					t.Errorf("%s: the list comes to %s, the tile %s", what, got, x.Value)
				}
			case "/incidents":
				iv := viewOf(t, s, "incidents", target, s.incidentsPage).(incidentsView)
				if iv.Week != "2026-10-26" || iv.Judged.Total != last.Incidents {
					t.Errorf("%s: over %q, %d incidents, the week %d", what, iv.Week, iv.Judged.Total, last.Incidents)
				}
				switch x.key {
				case "Incidents missed":
					if fmt.Sprint(len(iv.Lines)) != x.Value || iv.Judged.Missed != last.Missed {
						t.Errorf("%s: %d listed, %d missed, the tile %s", what, len(iv.Lines), iv.Judged.Missed, x.Value)
					}
				case "Time to catch":
					// The median of when the first alert of a rule that isn't
					// noisy came, of those listed.
					if len(iv.Lines) != last.Caught || len(iv.Lines) != len(last.Delays) {
						t.Errorf("%s: %d listed, the week %d caught, %d timed", what, len(iv.Lines), last.Caught, len(last.Delays))
					}
					m, _ := last.median()
					if want := leadText("en", -m); iv.Lead != want || x.Value != verdict.Delay(m) {
						t.Errorf("%s: the median %q, the tile %s (%q)", what, iv.Lead, x.Value, want)
					}
				}
				for _, l := range iv.Lines {
					if want := map[string]string{"Incidents missed": "missed", "Time to catch": "caught"}[x.key]; l.Status != want {
						t.Errorf("%s: %s is %s", what, l.ID, l.Status)
					}
				}
			default:
				t.Errorf("%s: a link to %s", what, u.Path)
			}
		}
		for _, k := range []string{"Precision", "Firings", "Incidents missed", "Time to catch", "Noisy rules", "Woken for nothing", "Refired", "During deploys"} {
			if !seen[k] {
				t.Errorf("%s: no %q tile", team, k)
			}
		}
		// Something to count, each way.
		if team == "" && (last.Firings < 30 || last.Noisy != 2 || last.Missed != 2 || last.Caught != 2 || last.NoisyOnly != 2 || last.Refired == 0 || last.Deploys == 0 || last.Asleep == 0) {
			t.Errorf("last week: %+v", last)
		}
	}

	// Each rule over the week, and nothing of the week after: what it
	// caught, its notifications, whether it re-fired in the whole week,
	// how many weeks running it has been noisy.
	rl := viewOf(t, s, "rules", "/rules?week=2026-10-26", s.rulesList).(rulesList)
	rows := map[string]ruleRow{}
	for _, r := range rl.Rows {
		rows[r.Name] = r
	}
	pe, ss := rows["PayErrors"], rows["SearchSlow"]
	if ss.Incidents != 1 || pe.RefiredOf != 1 || pe.RefiredWeeks != 1 || pe.Streak != 1 || notify && (pe.Notifications != 12 || !rl.Sum.Notified) {
		t.Errorf("PayErrors %d of %d weeks re-fired, %d noisy, %d notified; SearchSlow caught %d", pe.RefiredWeeks, pe.RefiredOf, pe.Streak, pe.Notifications, ss.Incidents)
	}
	if iv := viewOf(t, s, "incidents", "/incidents?week=2026-10-26&state=missed", s.incidentsPage).(incidentsView); len(iv.Lines) != 2 {
		t.Errorf("%d missed that week", len(iv.Lines))
	}
	if notify {
		// Notified that week, at night, for nothing: the one begun Sunday
		// at 23:59:30, and the long one, notified again, neither of them a
		// firing that started in it, are listed, each woken once.
		for _, name := range []string{"PayNight", "PayLong"} {
			if r, ok := rows[name]; !ok || r.Woken != 1 || r.Firings != 0 {
				t.Errorf("%s: listed %v, %d woken, %d fired", name, ok, r.Woken, r.Firings)
			}
		}
		// A week before notifications were kept counts none: "–", as the
		// overview's week does, not 0.
		d := viewOf(t, s, "overview", "/", s.overview).(dashboard)
		var before *weekView
		for i := range d.Weeks {
			if d.Weeks[i].Start.Format("2006-01-02") == "2026-10-12" {
				before = &d.Weeks[i]
			}
		}
		old := viewOf(t, s, "rules", "/rules?week=2026-10-12", s.rulesList).(rulesList)
		if before == nil || !before.Uncounted || !old.Sum.Uncounted || !rl.Sum.Notified || rl.Sum.Uncounted {
			t.Errorf("the week of 10-12 uncounted: overview %v, list %v; last week %v", before != nil && before.Uncounted, old.Sum.Uncounted, rl.Sum.Uncounted)
		}
		_, body, _ := get(t, s.Handler(), "/rules?week=2026-10-12&weeks=12")
		if !strings.Contains(body, "Notifications for nothing, while people slept: – (none were kept yet)") || strings.Contains(body, "notifications for nothing, while people slept</span>") {
			t.Error("the week before notifications were kept says a number")
		}
	}

	// The page links each tile, with the team.
	_, body, _ := get(t, s.Handler(), "/?team=payments")
	for _, want := range []string{`<a class="tile-more" href="/rules?perf=noisy&amp;team=payments&amp;week=2026-10-26">The noisy rules<span class="vh">: Noisy rules</span>`,
		`<a class="tile-more" href="/incidents?state=missed&amp;team=payments&amp;week=2026-10-26">The incidents missed<span class="vh">: Incidents missed</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s", want)
		}
	}
}

// TestWeekAsked: week= is a whole week of the twelve kept, over: a date
// in it is its Monday; another, or this week, is no week, and the list is
// over its period. The week is a chip, and the lists' links keep it.
func TestWeekAsked(t *testing.T) {
	s := drillServer(t, false)
	for q, want := range map[string]string{"week=2026-10-26": "2026-10-26", "week=2026-10-28": "2026-10-26", "week=2026-11-01": "2026-10-26",
		"week=2026-11-02": "", "week=2026-11-03": "", "week=2026-08-17": "2026-08-17", "week=2026-08-16": "", "week=2026-13-01": "", "week=x": "",
		"week=2026-10-26T00:00": "", "week=": ""} {
		if rl := viewOf(t, s, "rules", "/rules?"+q, s.rulesList).(rulesList); rl.Week != want {
			t.Errorf("rules %s: %q, not %q", q, rl.Week, want)
		}
		if iv := viewOf(t, s, "incidents", "/incidents?"+q, s.incidentsPage).(incidentsView); iv.Week != want {
			t.Errorf("incidents %s: %q, not %q", q, iv.Week, want)
		}
	}
	// Over the week alone: what fired the week before or after isn't in it.
	all := viewOf(t, s, "rules", "/rules?weeks=12", s.rulesList).(rulesList)
	wk := viewOf(t, s, "rules", "/rules?week=2026-10-26&weeks=12", s.rulesList).(rulesList)
	if all.Sum.Firings != wk.Sum.Firings+3 || wk.Shown != 4 || wk.Weeks != 1 {
		t.Errorf("12 weeks %d, the week %d (%d shown, over %d weeks)", all.Sum.Firings, wk.Sum.Firings, wk.Shown, wk.Weeks)
	}
	h := s.Handler()
	_, body, _ := get(t, h, "/rules?week=2026-10-26&perf=noisy&sort=refired")
	for _, want := range []string{"Each rule&#39;s stones in the week of 2026-10-26, the noisiest first.",
		`<a class="chip" href="/rules?perf=noisy&amp;sort=refired"><span class="vh">Remove filter: </span>Week of: <bdi>2026-10-26</bdi>`,
		`<a href="/rules?perf=noisy&amp;week=2026-10-26">caught nothing, the most first</a>`,
		`<input type="hidden" name="back" value="/rules?perf=noisy&amp;sort=refired&amp;week=2026-10-26">`} {
		if !strings.Contains(body, want) {
			t.Errorf("rules: no %s", want)
		}
	}
	if _, body, _ := get(t, h, "/rules?week=2026-10-26"); !strings.Contains(body, "rules didn&#39;t fire in the week of 2026-10-26</summary>") {
		t.Error("rules: the quiet ones aren't said to be of the week")
	}
	// The filters' form keeps the week, in both its places, and what it
	// sends, or a search box sent to its parameters, is over the week.
	if _, body, _ := get(t, h, "/rules?week=2026-10-26&sort=refired"); strings.Count(body, `class="facets"><input type="hidden" name="sort" value="refired"><input type="hidden" name="week" value="2026-10-26">`) != 2 {
		t.Error("rules: the filters' form drops the week")
	}
	if rl := viewOf(t, s, "rules", "/rules?week=2026-10-26&sort=refired&perf=noisy&q=", s.rulesList).(rulesList); rl.Week != "2026-10-26" || rl.Weeks != 1 {
		t.Errorf("rules: the form sent, over %q, %d weeks", rl.Week, rl.Weeks)
	}
	if _, _, hd := get(t, h, "/rules?week=2026-10-26&q=is%3Anoisy"); !strings.Contains(hd.Get("Location"), "week=2026-10-26") {
		t.Errorf("rules: the search box sent to %q", hd.Get("Location"))
	}
	_, body, _ = get(t, h, "/incidents?week=2026-10-26&state=missed")
	for _, want := range []string{"What went wrong in the week of 2026-10-26, and what caught it.",
		`<a href="/incidents?state=missed&amp;weeks=4">4 weeks</a>`,
		`<a href="/incidents?week=2026-10-26">all <span class="quiet">(6)</span></a>`,
		`<a href="/incidents?state=missed&amp;week=2026-10-26" aria-current="page">✗ missed <span class="quiet">(2)</span></a>`,
		`<a class="chip" href="/incidents?state=missed"><span class="vh">Remove filter: </span>Week of: <bdi>2026-10-26</bdi>`} {
		if !strings.Contains(body, want) {
			t.Errorf("incidents: no %s", want)
		}
	}
	if strings.Count(body, `<tr class="missed">`) != 2 || strings.Contains(body, `<tr class="caught">`) {
		t.Error("incidents: not the missed alone")
	}
}
