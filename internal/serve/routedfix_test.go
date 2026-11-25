package serve

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/routing"
)

// workedServerWith is workedServer with the work-out's bounds lowered before
// the runs.
func workedServerWith(t *testing.T, lim routedLimits) *Server {
	t.Helper()
	am := routedFile(t, routedAM)
	s, _ := server(t, func(c *Config) {
		c.Notifications, c.AlertmanagerConfig = true, am
		c.Options.Hours = nil
	})
	s.routed.limits = lim
	raw := ""
	if err := s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(context.Background(), true)
	return s
}

func weeksPaged(t *testing.T, s *Server) (unknown, counted int) {
	t.Helper()
	_, body, _ := get(t, s.Handler(), "/api/v1/summary")
	var sum struct {
		Rows []struct {
			Paged *struct {
				Pages   int  `json:"pages"`
				Unknown bool `json:"unknown"`
			} `json:"paged"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(body), &sum); err != nil {
		t.Fatal(err)
	}
	for _, r := range sum.Rows {
		if r.Paged != nil && r.Paged.Unknown {
			unknown++
		} else if r.Paged != nil {
			counted++
		}
	}
	return
}

// A run that takes more work units than its budget stops, and its hours are
// recorded as not worked out with the text kept whole: nothing is kept of
// them, and every page says not known, never none.
func TestRoutedBudgetStops(t *testing.T) {
	parallel(t)
	s := workedServerWith(t, routedLimits{budget: 1})
	ctx := context.Background()
	c, err := s.l.RoutedCover(ctx)
	if err != nil || len(c) == 0 {
		t.Fatalf("cover: %+v %v", c, err)
	}
	stopped := 0
	for _, x := range c {
		if !x.Worked && strings.Contains(x.Why, "spent its budget of 1 work units") {
			stopped++
		} else if !x.Worked || x.Why != "" {
			t.Errorf("stretch: %+v", x)
		}
	}
	if stopped == 0 {
		t.Errorf("no stretch stopped: %+v", c)
	}
	if by, _ := s.l.RoutedByReceiver(ctx, now.Add(-30*24*time.Hour), now.Add(time.Hour)); len(by) != 0 {
		t.Errorf("something kept of hours that weren't worked out: %v", by)
	}
	if unknown, _ := weeksPaged(t, s); unknown == 0 {
		t.Errorf("the summary: no week not known")
	}
	if _, body, _ := get(t, s.Handler(), "/notifications"); !strings.Contains(body, "Not known:") || !strings.Contains(body, "spent its budget of 1 work units") {
		t.Errorf("/notifications doesn't say: %.300s", body)
	}
}

// Firings past the cap in a run's own hours leave the hours not worked out.
func TestRoutedFiringCap(t *testing.T) {
	parallel(t)
	s := workedServerWith(t, routedLimits{firings: 1})
	c, _ := s.l.RoutedCover(context.Background())
	if !strings.Contains(c.Unknown(now.Add(-30*24*time.Hour), now.Add(time.Hour)), "more than 1 firings") {
		t.Errorf("cover: %+v", c)
	}
}

// A firing that has been on for a month is taken as beginning at the lead-in,
// and as notified before it: the work-out of a day takes the work units of the
// day and the lead-in (about 2,500 here, among the other firings'), not the
// 8,600 flushes of the month; and it makes no first notification at the
// lead-in's start.
func TestRoutedLeadClamp(t *testing.T) {
	parallel(t)
	s := workedServerWith(t, routedLimits{budget: 6000})
	ctx := context.Background()
	to := now
	from := to.Add(-24 * time.Hour)
	start := to.Add(-30 * 24 * time.Hour)
	if err := s.l.Record(ctx, ledger.Found{From: from, To: to, Run: ledger.Run{Kind: "test", Started: from, Finished: from, From: from, To: to},
		Firings: []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "old", "severity": "page"}, Start: start}}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	s.workOutRouted(ctx, from, to)
	c, _ := s.l.RoutedCover(ctx)
	if !c.Covers(from, to) {
		t.Errorf("a day with a month-old firing stopped: %+v", c)
	}
	if by, _ := s.l.RoutedByReceiver(ctx, from, to); by["oncall"].Notifications == 0 {
		t.Errorf("nothing worked out: %v", by)
	}
	// Its group was notified a month ago: none of what was worked out is a
	// first notification, and none of its pages is a PagerDuty incident's.
	for _, f := range []string{"old"} {
		fs, _, _ := s.l.FiringsOverlapping(ctx, from, to, from, 1000)
		for _, x := range fs {
			if x.Labels["service"] != f {
				continue
			}
			if got, _ := s.l.RoutedOf(ctx, []int64{x.ID}, now); got[x.ID].Paged != 0 || !got[x.ID].FirstPage.IsZero() {
				t.Errorf("a firing on for a month got a first page: %+v", got[x.ID])
			}
		}
	}
}

// A gap in what was worked out reads as not known on the summary, until the
// catch-up works it out again.
func TestRoutedGapNotKnown(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, nil)
	ctx := context.Background()
	from, to, _, _ := routedSpan(s.l, ctx)
	if unknown, counted := weeksPaged(t, s); unknown != 0 || counted == 0 {
		t.Fatalf("before: %d not known, %d counted", unknown, counted)
	}
	mid := from.Truncate(24 * time.Hour).Add(36 * time.Hour)
	if err := s.l.MarkRoutedUnknown(ctx, mid, mid.Add(2*time.Hour), "a test's failure"); err != nil {
		t.Fatal(err)
	}
	if unknown, _ := weeksPaged(t, s); unknown == 0 {
		t.Error("a week touching a gap isn't not known")
	}
	if c, _ := s.l.RoutedCover(ctx); c.Covers(from, to) || c.Unknown(mid, mid.Add(time.Hour)) != "a test's failure" {
		t.Errorf("cover: %+v", c)
	}
	s.catchUpRouted(ctx, to)
	if c, _ := s.l.RoutedCover(ctx); !c.Covers(mid, mid.Add(2*time.Hour)) {
		t.Errorf("the catch-up didn't work the gap out: %+v", c)
	}
	if unknown, _ := weeksPaged(t, s); unknown != 0 {
		t.Errorf("%d weeks not known after the catch-up", unknown)
	}
}

// A recorded notification to a PagerDuty integration is a page only when it
// is a firing notification after none or after all resolved (reason first);
// a Slack one on a paging pattern is, as ever.
func TestRecordedPagesDedup(t *testing.T) {
	parallel(t)
	cfg, err := content.ReadAlertmanagerRouting([]byte(routedAM))
	if err != nil {
		t.Fatal(err)
	}
	pages := pagingOfRecorded(cfg, nil, false)
	for _, c := range []struct {
		name, reason   string
		resolved, want bool
	}{
		{"oncall/pagerduty", ledger.ReasonFirst, false, true},
		{"oncall/pagerduty", ledger.ReasonRepeat, false, false},
		{"oncall/pagerduty", ledger.ReasonNew, false, false},
		{"oncall/pagerduty", ledger.ReasonResolved, true, false},
		{"oncall/slack", ledger.ReasonFirst, false, false},
		{"oncall", ledger.ReasonFirst, false, true},   // a name alone: its pager dedups
		{"oncall", ledger.ReasonRepeat, false, false}, // and its repeat isn't a page
	} {
		if got := pages(c.name, c.reason, c.resolved); got != c.want {
			t.Errorf("%s %s resolved %v: %v, want %v", c.name, c.reason, c.resolved, got, c.want)
		}
	}
}

// The inhibiting firing's rule is kept with the mute.
func TestRoutedSourceRule(t *testing.T) {
	parallel(t)
	a := routing.AlertResult{Muted: []routing.Muting{{Kind: routing.MutedInhibition, By: "down", Source: 1}, {Kind: routing.MutedSilence, By: "s1", Source: -1}}}
	x := routedOf(7, a, nil, time.Time{}, func(i int) string { return []string{"A", "NodeDown"}[i] })
	if len(x.Muted) != 2 || x.Muted[0].SourceRule != "NodeDown" || x.Muted[1].SourceRule != "" {
		t.Errorf("muted: %+v", x.Muted)
	}
}

// A silence the recorder told of is tied to the ledger's firing, and matches
// on its labels and alertname, not on the recorded alert's.
func TestRoutedSilencesFromMutes(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, nil)
	ctx := context.Background()
	at := now.Add(-2 * time.Hour)
	fs := []ledger.Firing{{ID: 1, Firing: firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "a"}, Start: at.Add(-time.Minute), End: at.Add(time.Hour)}}}
	if _, errs, err := s.l.KeepMutes(ctx, []ledger.Mute{
		{At: at, Last: at.Add(10 * time.Minute), Rule: "Errors", Kind: ledger.MutedSilence, Ref: "sil",
			Labels: map[string]string{"alertname": "Errors", "service": "a", "cluster": "ext"}},
		{At: at, Last: at.Add(10 * time.Minute), Rule: "Errors", Kind: ledger.MutedSilence, Ref: "other",
			Labels: map[string]string{"alertname": "Errors", "service": "b"}},
	}); err != nil || slices.ContainsFunc(errs, func(e error) bool { return e != nil }) {
		t.Fatal(errs, err)
	}
	alerts := []routing.Alert{{Labels: map[string]string{"alertname": "Errors", "service": "a"}, Start: fs[0].Start}}
	got, over, err := s.mutedSilences(ctx, at.Add(-time.Hour), now, fs, alerts, 100)
	if err != nil || over || len(got) != 1 || got[0].ID != "sil" {
		t.Fatalf("silences: %+v %v", got, err)
	}
	names := map[string]string{}
	for _, m := range got[0].Matchers {
		names[m.Name] = m.Value
	}
	if len(names) != 2 || names["alertname"] != "Errors" || names["service"] != "a" {
		t.Errorf("matchers: %v (the external label cluster is the alert's, not the firing's)", names)
	}
}

// A server that has run nothing, as a replica that isn't the leader, reads
// the configuration for its pages: they are the leader's.
func TestRoutedReplicaReadsConfig(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, nil)
	leader, err := s.notified(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	am := routedFile(t, routedAM)
	r, _ := server(t, func(c *Config) { c.Notifications, c.AlertmanagerConfig = true, am })
	if r.routingKept() != nil {
		t.Fatal("a configuration before any read")
	}
	// The replica shares the ledger the leader worked out.
	r.l = s.l
	p, err := r.notified(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.routingKept() == nil {
		t.Error("the configuration wasn't read for the pages")
	}
	if len(p.events) != len(leader.events) || len(p.events) == 0 {
		t.Errorf("the replica counts %d pages, the leader %d", len(p.events), len(leader.events))
	}
}

// Receivers recorded by name alone (a webhook port) are compared with the
// webhook integrations worked out, and one recorded that nothing was worked
// out for is counted apart, not in the agreement.
func TestNotificationsAgreementWebhook(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, nil)
	ctx := context.Background()
	fs, _, _ := s.l.FiringsOverlapping(ctx, now.Add(-4*24*time.Hour), now, now.Add(-4*24*time.Hour), 1000)
	n := 0
	for _, f := range fs {
		if f.Rule != "Errors" || n == 3 {
			continue
		}
		n++
		if _, err := s.l.Notify(ctx, ledger.Notification{At: f.Start.Add(31 * time.Second), Source: "webhook", Receiver: "oncall", Group: "g", Reason: ledger.ReasonFirst,
			Alerts: []ledger.Alert{{Rule: "Errors", Labels: map[string]string{"alertname": "Errors", "service": f.Labels["service"], "severity": "page"}, Start: f.Start}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.l.Notify(ctx, ledger.Notification{At: fs[0].Start.Add(time.Minute), Source: "webhook", Receiver: "null", Group: "g", Reason: ledger.ReasonFirst,
		Alerts: []ledger.Alert{{Rule: "Watchdog", Labels: map[string]string{"alertname": "Watchdog"}, Start: fs[0].Start}}}); err != nil {
		t.Fatal(err)
	}
	v, err := s.notificationsNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]receiverRow{}
	for _, r := range v.Rows {
		rows[r.Receiver] = r
	}
	// oncall has a PagerDuty and a Slack, and no webhook: nothing to compare
	// the recorded ones with, which the agreement says.
	if r := rows["oncall"]; !r.Compared || !r.Webhook || r.WorkedOut != 0 || r.Recorded == 0 || r.Agreement() != 0 {
		t.Errorf("oncall: %+v", r)
	}
	if r := rows["null"]; !r.RecordedOnly || r.Compared || r.Recorded != 1 {
		t.Errorf("null: %+v", r)
	}
	if v.RecordedOnly != 1 || v.Compared != 1 {
		t.Errorf("recorded only %d, compared %d", v.RecordedOnly, v.Compared)
	}
}
