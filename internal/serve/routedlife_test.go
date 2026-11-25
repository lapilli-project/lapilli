package serve

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/routing"
	"lapilli.dev/internal/verdict"
)

// A pager that rings for every notification (Pushover) and one that opens an
// incident for the group (PagerDuty), repeating every four hours.
const routedRepeatAM = `route:
  receiver: ring
  group_by: [alertname]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
receivers:
  - name: ring
    pagerduty_configs: [{routing_key: k}]
    pushover_configs: [{user_key: u, token: t}]
`

// A firing on for a month, worked out by hourly runs, each of which sees it
// for the hours of its own and the lead-in: what it reached is what one run
// over the whole month works out. Not a second first page every hour (a run
// takes the firing as carried and its group as notified before the lead-in),
// no repeat lost (a run keeps its own hours' notifications, and a firing's
// totals are those of all the runs), and no repeat moved (a group's repeats
// are counted from its last notification kept).
func TestRoutedFiringForAMonth(t *testing.T) {
	parallel(t)
	am := routedFile(t, routedRepeatAM)
	s, _ := server(t, func(c *Config) { c.Notifications, c.AlertmanagerConfig = true, am })
	ctx := context.Background()
	base := now.Add(-31 * 24 * time.Hour).Truncate(time.Hour)
	end := base.Add(30 * 24 * time.Hour)
	f := firing.Firing{Rule: "Errors", Labels: map[string]string{"service": "old"}, Start: base}
	if err := s.l.Record(ctx, ledger.Found{From: base, To: end, Run: ledger.Run{Kind: "test", Started: base, Finished: base, From: base, To: end}, Firings: []firing.Firing{f}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	cfg := s.routingConfig(ctx)
	if cfg == nil {
		t.Fatal("no configuration")
	}
	// What one run over the whole month works out.
	truth, err := cfg.Run(verdict.NotificationAlerts([]verdict.Judged{{Firing: f}}, nil), routing.Options{Until: end})
	if err != nil || truth.Stopped != "" || len(truth.Notifications) < 100 {
		t.Fatalf("the month at once: %d notifications %q %v", len(truth.Notifications), truth.Stopped, err)
	}
	for at := base; at.Before(end); at = at.Add(time.Hour) {
		s.workOutRouted(ctx, at, at.Add(time.Hour))
	}
	if c, _ := s.l.RoutedCover(ctx); !c.Covers(base, end) {
		t.Fatalf("cover: %+v", c)
	}
	want := truth.Alerts[0]
	got := workedOf(t, s, "Errors")[base]
	if got.Notified != want.Notified || got.Paged != want.Paged || !got.FirstPage.Equal(want.FirstPage) {
		t.Errorf("a month of hourly runs: %d notified, %d paged, first page %v; at once: %d, %d, %v",
			got.Notified, got.Paged, got.FirstPage, want.Notified, want.Paged, want.FirstPage)
	}
	// The one page of the PagerDuty incident is its first, and every
	// Pushover notification rings.
	pagerduty := 0
	for _, a := range truth.Notifications {
		if a.Kind == "pagerduty" && a.Page {
			pagerduty++
		}
	}
	if pagerduty != 1 {
		t.Fatalf("the month at once pages PagerDuty %d times", pagerduty)
	}
	by, err := s.l.RoutedByReceiverKind(ctx, base, end)
	if err != nil {
		t.Fatal(err)
	}
	if pd := by["ring"]["pagerduty"]; pd.Pages != 1 || pd.Notifications != len(truth.Notifications)/2 {
		t.Errorf("PagerDuty: %+v of %d notifications", pd, len(truth.Notifications)/2)
	}
	if po := by["ring"]["pushover"]; po.Pages != po.Notifications || po.Notifications != len(truth.Notifications)/2 {
		t.Errorf("Pushover: %+v of %d notifications", po, len(truth.Notifications)/2)
	}
	// Each repeat is where it is when worked out at once.
	rows, _, err := s.l.RoutedPages(ctx, base, end, 10)
	if err != nil || len(rows) != 1 || len(rows[0].PageAts) != min(ledger.MaxRoutedPages, want.Paged) {
		t.Fatalf("page times: %d firings %v", len(rows), err)
	}
	for i, pa := range rows[0].PageAts {
		var at time.Time
		n := 0
		for _, nt := range truth.Notifications {
			if nt.Page {
				if n == i {
					at = nt.At
				}
				n++
			}
		}
		if !pa.At.Equal(at) {
			t.Fatalf("page %d at %v, at once %v", i, pa.At, at)
		}
	}
}
