package serve

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

type knownReport struct {
	Notifications struct {
		Groups  int    `json:"groups"`
		Known   bool   `json:"known"`
		Stopped string `json:"stopped"`
	} `json:"notifications"`
	Rules []struct {
		Rule          string `json:"rule"`
		Notifications struct {
			Known   bool   `json:"known"`
			Stopped string `json:"stopped"`
		} `json:"notifications"`
	} `json:"rules"`
	Firings []struct {
		Rule  string    `json:"rule"`
		Start time.Time `json:"start"`
		End   time.Time `json:"end"`
	} `json:"firings"`
}

func knownReportOf(t *testing.T, s *Server, from, to time.Time) knownReport {
	t.Helper()
	code, body, _ := get(t, s.Handler(), "/api/v1/report?from="+from.UTC().Format(time.RFC3339)+"&to="+to.UTC().Format(time.RFC3339))
	var rep knownReport
	if err := json.Unmarshal([]byte(body), &rep); err != nil || code != 200 {
		t.Fatalf("report: %d %v\n%s", code, err, body)
	}
	return rep
}

// A rule's notifications say not known when some hour of its firings wasn't
// worked out, with why, and a rule whose firings all lie in hours that were
// is known, though the report's is not.
func TestReportRuleNotificationsKnown(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, nil)
	ctx := context.Background()
	c, _ := s.l.RoutedCover(ctx)
	from, ok := c.First()
	last, ok2 := c.Last()
	if !ok || !ok2 {
		t.Fatalf("cover %+v", c)
	}
	rep := knownReportOf(t, s, from, last)
	for _, r := range rep.Rules {
		if !r.Notifications.Known || r.Notifications.Stopped != "" {
			t.Errorf("%s: %+v before any hour is marked", r.Rule, r.Notifications)
		}
	}
	if len(rep.Firings) == 0 {
		t.Fatal("no firings")
	}
	// Mark the hour the first firing started in as not worked out.
	f := rep.Firings[0]
	h0 := f.Start.Truncate(time.Hour)
	why := "more than 1 flushes: nothing of them is known"
	if err := s.l.MarkRoutedUnknown(ctx, h0, h0.Add(time.Hour), why); err != nil {
		t.Fatal(err)
	}
	rep = knownReportOf(t, s, from, last)
	if rep.Notifications.Known || rep.Notifications.Stopped == "" {
		t.Errorf("the report: %+v", rep.Notifications)
	}
	end := func(e time.Time, st time.Time) time.Time {
		if e.IsZero() || !e.After(st) {
			return st.Add(time.Nanosecond)
		}
		return e
	}
	touched := map[string]bool{}
	for _, x := range rep.Firings {
		if x.Start.Before(h0.Add(time.Hour)) && end(x.End, x.Start).After(h0) {
			touched[x.Rule] = true
		}
	}
	for _, r := range rep.Rules {
		if touched[r.Rule] != !r.Notifications.Known || touched[r.Rule] != (r.Notifications.Stopped == why) {
			t.Errorf("%s: touched %v, but %+v", r.Rule, touched[r.Rule], r.Notifications)
		}
	}
	if !touched[f.Rule] {
		t.Errorf("%s: its firing is in the marked hour", f.Rule)
	}
}

// A group that goes on across two stretches of worked-out hours is one
// group in the report's total, not one for each stretch.
func TestReportGroupsDistinct(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, nil)
	ctx := context.Background()
	c, _ := s.l.RoutedCover(ctx)
	base, ok := c.First()
	if !ok {
		t.Fatal("no cover")
	}
	base = base.Truncate(time.Hour)
	h := func(n int) time.Time { return base.Add(time.Duration(n) * time.Hour) }
	last, _ := c.Last()
	if last.Before(h(6)) {
		t.Skipf("cover too short: %+v", c)
	}
	// Clear what was worked out, leave an hour between two stretches, and
	// put the same two groups in both.
	if err := s.l.KeepRouted(ctx, h(0), last, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.l.MarkRoutedUnknown(ctx, h(2), h(3), "stopped"); err != nil {
		t.Fatal(err)
	}
	n := func(at time.Time, g string) ledger.RoutedNotification {
		return ledger.RoutedNotification{At: at, Receiver: "oncall", Kind: "pagerduty", Page: true, Reason: "first", Group: g, Firings: 1}
	}
	if err := s.l.KeepRouted(ctx, h(0), h(2), "", []ledger.RoutedNotification{n(h(0), "g1"), n(h(1), "g2")}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.l.KeepRouted(ctx, h(3), h(5), "", []ledger.RoutedNotification{n(h(3), "g1"), n(h(4), "g1")}, nil); err != nil {
		t.Fatal(err)
	}
	rep := knownReportOf(t, s, h(0), h(5))
	if rep.Notifications.Groups != 2 {
		t.Errorf("groups %d, want 2 (g1 is in both stretches)", rep.Notifications.Groups)
	}
}
