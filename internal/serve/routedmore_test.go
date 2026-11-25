package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// pagesBudget is the checks of a rule held to a budget of pages, in a
// server that works the notifications out.
func pagesBudget(t *testing.T, budget string) (*Server, map[string]ledger.ExpectCheck) {
	t.Helper()
	ctx := context.Background()
	am := routedFile(t, routedAM)
	s, rulesDir := server(t, func(c *Config) {
		keepRecord(c)
		c.Notifications, c.AlertmanagerConfig = true, am
	})
	os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte(`groups:
  - name: shop
    rules:
      - alert: Errors
        expr: errors > 5
        for: 2m
        labels: {severity: page}
        annotations: {lapilli.dev/budget: "`+budget+`"}
`), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	checkAgain(t, s)
	out := map[string]ledger.ExpectCheck{}
	for key, c := range checksOf(t, s, "Errors") {
		if strings.HasPrefix(key, "budget pages") {
			out[key] = c
		}
	}
	if len(out) == 0 {
		t.Fatalf("no pages check: %v", slices.Sorted(mapsKeys(checksOf(t, s, "Errors"))))
	}
	return s, out
}

// A budget of pages is checked by the pages worked out of the firings that
// started in each week: over, broken; within, so far in the week under way;
// for a week the notifications weren't all worked out for, not known.
func TestRoutedPagesBudget(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, held := pagesBudget(t, "100 pages")
	week := ledger.Week(s.l.Day(now.Add(-time.Hour)))
	p, _, err := s.l.RoutedPagesByWeek(ctx, s.l.Midnight(week), s.l.Midnight(week+7))
	if err != nil || p[week]["Errors"] == 0 {
		t.Fatalf("no page that week: %v %v", p, err)
	}
	this := held["budget pages "+s.l.Midnight(week).Format("2006-01-02")]
	if this.Result != ledger.ExpectOpen || this.Why != whySoFar || this.N == nil || *this.N != p[week]["Errors"] {
		t.Errorf("the week under way: %+v, pages %d", this, p[week]["Errors"])
	}
	for key, c := range held {
		if c.Why == whyPagesNotYet || c.Result == ledger.ExpectBroken {
			t.Errorf("%s: %+v", key, c)
		}
	}
	// The weeks before the first hour worked out aren't known.
	before := held["budget pages "+s.l.Midnight(week-7).Format("2006-01-02")]
	if before.Result != ledger.ExpectUnknown || before.Why != whyPagesNotCovered {
		t.Errorf("the week before the hours worked out: %+v", before)
	}
	// One more page than the budget holds is broken.
	_, tight := pagesBudget(t, "1 pages")
	if c := tight["budget pages "+s.l.Midnight(week).Format("2006-01-02")]; c.Result != ledger.ExpectBroken || c.N == nil || *c.N <= 1 {
		t.Errorf("a budget of 1 page: %+v", c)
	}
	_, body, _ := get(t, s.Handler(), "/rules/Errors?tab=expectations")
	if strings.Contains(body, "not yet by lapilli serve") || !strings.Contains(body, "pages") {
		t.Errorf("the tab: pages not shown, or still not worked out by serve")
	}
}

// With no routing configuration to work them out from, a pages budget says
// so, as it said it was not yet worked out.
func TestRoutedPagesBudgetNotWorkedOut(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, rulesDir := server(t, keepRecord)
	os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte(`groups:
  - name: shop
    rules:
      - alert: Errors
        expr: errors > 5
        annotations: {lapilli.dev/budget: "5 pages"}
`), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	checkAgain(t, s)
	got := false
	for key, c := range checksOf(t, s, "Errors") {
		if strings.HasPrefix(key, "budget pages") {
			got = true
			if c.Result != ledger.ExpectUnknown || c.Why != whyPagesNotYet {
				t.Errorf("%s: %+v", key, c)
			}
		}
	}
	if !got {
		t.Error("no pages check")
	}
}

// A goal on the pages that went in a week (pages, pages.asleep,
// pages.nothing) takes the pages as the summary gives them, and is not
// known where they aren't worked out.
func TestRoutedGoalMeasures(t *testing.T) {
	parallel(t)
	for _, m := range []string{"pages", "pages.asleep", "pages.nothing"} {
		if _, ok := ledger.GoalMeasures[m]; !ok || !slices.Contains(ledger.GoalMeasureNames, m) {
			t.Errorf("%s isn't a goal measure", m)
		}
		if goalMeasureNames[m] == "" {
			t.Errorf("%s has no name", m)
		}
	}
	row := &summaryRow{Paged: &summaryPaged{Pages: 12, Asleep: 3, ForNothing: 5}}
	for m, want := range map[string]float64{"pages": 12, "pages.asleep": 3, "pages.nothing": 5} {
		v := goalValue(ledger.Goal{Measure: m}, row)
		if v == nil || *v != want {
			t.Errorf("%s: %v, want %v", m, v, want)
		}
	}
	if v := goalValue(ledger.Goal{Measure: "pages"}, &summaryRow{}); v != nil {
		t.Errorf("pages with none worked out: %v", *v)
	}
}

// MCP's rule and firing tools give the pages and what a firing is worked
// out to have reached, marked worked out.
func TestRoutedMCP(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, nil)
	ctx := context.Background()
	l := ruleLine{Name: "Errors"}
	p, err := s.notified(ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := p.pageLines(p.from, now.Add(time.Hour), func(string) *verdict.Hours { return nil })
	l.pagesLine = *by["Errors"]
	out := ruleOutOf(l)
	if out.Pages == nil || out.Pages.Pages == 0 || out.Pages.WorkedOut != out.Pages.Pages || out.Pages.Basis != "worked out" {
		t.Errorf("rule out: %+v", out.Pages)
	}
	if b, _ := json.Marshal(out); !strings.Contains(string(b), `"basis":"worked out"`) {
		t.Errorf("json: %s", b)
	}
	if ruleOutOf(ruleLine{Name: "x"}).Pages != nil {
		t.Error("pages with none counted")
	}
	lines := []firingLine{{Firing: ledger.Firing{ID: 1}}}
	fs, _, _ := s.l.FiringsOverlapping(ctx, now.Add(-3*24*time.Hour), now, now.Add(-3*24*time.Hour), 10)
	lines[0].Firing.ID = fs[0].ID
	if err := s.routedFirings(ctx, lines); err != nil {
		t.Fatal(err)
	}
	w := workedOutOutOf(lines[0].Routed)
	if w == nil || w.Notified == 0 || len(w.Receivers) != 1 || w.Receivers[0] != "oncall" {
		t.Errorf("firing out: %+v", w)
	}
}

// A Grafana rule that names its own contact point goes there, past the
// policies, with its own timings, in what serve works out: the rules' file
// says it (notification_settings), the configuration's tree would send it
// to oncall.
func TestRoutedOwnContactPoint(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	am := routedFile(t, routedAM+`  - name: pager
    pagerduty_configs: [{routing_key: k}]
`)
	s, rulesDir := server(t, func(c *Config) { c.Notifications, c.AlertmanagerConfig = true, am })
	os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte(`apiVersion: 1
groups:
  - orgId: 1
    name: shop
    folder: Ops
    interval: 1m
    rules:
      - uid: disk
        title: Disk
        condition: B
        data:
          - refId: A
            relativeTimeRange: {from: 600, to: 0}
            datasourceUid: prom
            model: {expr: up, instant: true, refId: A}
          - refId: B
            datasourceUid: __expr__
            model:
              conditions: [{evaluator: {params: [0], type: gt}}]
              expression: A
              refId: B
              type: threshold
        noDataState: NoData
        execErrState: Alerting
        for: 1m
        labels: {severity: page}
        isPaused: false
        notification_settings:
          receiver: pager
          group_by: [alertname]
          group_wait: 5s
          group_interval: 1m
          repeat_interval: 30m
`), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	at := now.Add(-2 * time.Hour)
	from := now.Add(-3 * time.Hour)
	if err := s.l.Record(ctx, ledger.Found{From: from, To: now, Run: ledger.Run{Kind: "test", Started: at, Finished: at, From: from, To: now},
		Firings: []firing.Firing{{Rule: "Disk", Labels: map[string]string{"severity": "page", "instance": "db-1"}, Start: at, End: at.Add(20 * time.Minute)}}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	s.workOutRouted(ctx, from, now)
	r, ok := workedOf(t, s, "Disk")[at]
	if !ok {
		t.Fatal("the firing wasn't worked out")
	}
	if len(r.Receivers) != 1 || r.Receivers[0].Receiver != "pager" || r.Receivers[0].Kind != "pagerduty" || r.Paged == 0 {
		t.Errorf("went to %+v", r)
	}
	if !r.FirstPage.Equal(at.Add(5 * time.Second)) {
		t.Errorf("its own group_wait: first page at %v, started %v", r.FirstPage, at)
	}
}

// With --grafana the policies, contact points and mute timings are read: a
// firing Grafana's policies send to a contact point of type oncall pages, as
// Grafana's oncall integration does.
func TestRoutedGrafana(t *testing.T) {
	was := grafanaNotifications
	t.Cleanup(func() { grafanaNotifications = was })
	grafanaNotifications = func(ctx context.Context, base, tokenFile, agent string) ([]byte, []byte, []byte, error) {
		return []byte(`[]`), []byte(`[{"uid":"a","name":"oncall-team","type":"oncall"},{"uid":"b","name":"email","type":"email"}]`),
			[]byte(`{"receiver":"email","group_by":["alertname"],"group_wait":"30s","group_interval":"5m","repeat_interval":"1h",
			  "routes":[{"receiver":"oncall-team","object_matchers":[["severity","=","page"]]}]}`), nil
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mute timings, and the rules Grafana has: none, the file's give them.
		if strings.HasPrefix(r.URL.Path, "/api/v1/provisioning/") {
			w.Write([]byte(`[]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()
	// Firings in the ledger, and then Grafana's policies given: they are
	// read, as an hour's run reads them (rules from Grafana's API would be
	// read too, which a stand-in for the policies alone doesn't give).
	s, _ := server(t, nil)
	raw := ""
	if err := s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(context.Background(), true)
	s.cfg.Notifications, s.cfg.Grafana = true, api.URL
	from, to := now.Add(-4*24*time.Hour), now
	s.workOutRouted(context.Background(), from, to)
	got := workedOf(t, s, "Errors")
	for start, r := range got {
		if r.Paged == 0 || len(r.Receivers) != 1 || r.Receivers[0].Receiver != "oncall-team" || r.Receivers[0].Kind != "oncall" {
			t.Errorf("firing at %v: %+v", start, r)
		}
	}
	// And the policies unreadable: nothing is worked out, and the page says
	// so.
	grafanaNotifications = func(context.Context, string, string, string) ([]byte, []byte, []byte, error) {
		return nil, nil, nil, errors.New("HTTP 403")
	}
	s2, _ := server(t, nil)
	s2.jobs(context.Background(), true)
	s2.cfg.Notifications, s2.cfg.Grafana = true, api.URL
	s2.workOutRouted(context.Background(), from, to)
	if _, _, ok, _ := routedSpan(s2.l, context.Background()); ok {
		t.Error("worked out with policies that couldn't be read")
	}
	_, body, _ := get(t, s2.Handler(), "/notifications")
	if !strings.Contains(body, "What was worked out before stays") {
		t.Errorf("the page doesn't say the configuration can't be read")
	}
}

// The overview's "Woken for nothing" counts pages, and
// the rules list's rows count the pages for nothing while people slept, not
// every notification.
func TestRoutedWoken(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, func(c *Config) { c.Options.Hours = verdict.DefaultHours(time.UTC) })
	h := s.Handler()
	_, body, _ := get(t, h, "/")
	// With no record in the week, which firings were for nothing isn't known:
	// the tile says how many pages came while people slept, not notifications.
	if !strings.Contains(body, "Woken for nothing") || !strings.Contains(body, "pages came while people slept that week (counted since 2026-10-31)") {
		t.Errorf("the overview: %s", grepLines(body, "Woken", "counted since"))
	}
	if strings.Contains(body, "notifications, counted since") {
		t.Error("the overview counts notifications")
	}
	_, body, _ = get(t, h, "/rules?weeks=12")
	if !strings.Contains(body, "pages for nothing, while people slept") && !strings.Contains(body, "pages, worked out") {
		t.Errorf("the rules list: %s", grepLines(body, "pages"))
	}
	if strings.Contains(body, "notifications for nothing, while people slept") {
		t.Error("the rules list counts notifications for nothing")
	}
	// The digest's line says it too.
	rep, err := s.ledgerReport(context.Background(), now.Add(-7*24*time.Hour), now, s.cfg.Options, false)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := s.sent(context.Background(), rep, now.Add(-7*24*time.Hour), now)
	if err != nil || sent == nil || sent.Pages == nil || sent.Pages.Basis != "worked out" || sent.Pages.Pages == 0 || sent.Pages.Asleep == 0 {
		t.Fatalf("the digest: %+v %v", sent, err)
	}
	if line := sent.text("en"); !strings.Contains(line, "Pages, worked out:") {
		t.Errorf("the digest's line: %s", line)
	}
}

// With a record to judge by, the tile says its pages are worked out (or
// recorded, or both) and counts those for nothing while people slept.
func TestRoutedWokenJudged(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, func(c *Config) {
		keepRecord(c)
		c.Options.Hours = verdict.DefaultHours(time.UTC)
	})
	_, body, _ := get(t, s.Handler(), "/")
	if !strings.Contains(body, "pages, worked out, counted since 2026-10-31") {
		t.Errorf("the overview: %s", grepLines(body, "Woken", "counted since", "pages"))
	}
	s2 := workedAndRecorded(t)
	p, err := s2.notified(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := p.basisOverall(); got != "recorded and worked out" {
		t.Errorf("basis %q, with pages of both kinds", got)
	}
}
