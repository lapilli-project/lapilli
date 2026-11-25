package routing

import (
	"testing"
	"time"
)

// A rule that names its contact point goes to it, past the policies, with
// its own grouping and timings; the root's timings fill what it leaves out.
func TestOwnRoute(t *testing.T) {
	root := quick("default", &Route{Receiver: "oncall", Matchers: []Matcher{mt(t, "team", "=", "db")}})
	c := build(t, root, nil, nil, recv("default", "slack"), recv("oncall", "pagerduty"), recv("mine", "pagerduty", "slack"))
	own := &OwnRoute{Receiver: "mine", GroupInterval: 2 * time.Minute}
	alerts := []Alert{
		{Labels: map[string]string{"alertname": "A", "team": "db"}, Start: t0, End: t0.Add(30 * time.Minute), Own: own},
		{Labels: map[string]string{"alertname": "B", "team": "db"}, Start: t0, End: t0.Add(30 * time.Minute)},
	}
	res, err := c.Run(alerts, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Alerts[0].Receivers; len(got) != 1 || got[0] != "mine" {
		t.Fatalf("own alert reached %v, want [mine]", got)
	}
	if got := res.Alerts[1].Receivers; len(got) != 1 || got[0] != "oncall" {
		t.Fatalf("tree alert reached %v, want [oncall]", got)
	}
	// Two integrations: a page and a message for the first notification; the root's
	// group_wait (10s) holds it, then its own group_interval (2m) sets the flushes.
	var first time.Time
	kinds := map[string]int{}
	for _, nt := range res.Notifications {
		if nt.Receiver != "mine" {
			continue
		}
		kinds[nt.Kind]++
		if first.IsZero() {
			first = nt.At
		}
	}
	if !first.Equal(t0.Add(10*time.Second)) || kinds["pagerduty"] == 0 || kinds["slack"] == 0 {
		t.Fatalf("own notifications first at %v, kinds %v", first, kinds)
	}
	if res.Alerts[0].Paged == 0 {
		t.Fatal("the own route's pager didn't page")
	}
}

// Rules with the same settings share a group (a group_by of alertname and
// folder by default); a different group_by splits them; an unknown contact
// point falls back to the policies.
func TestOwnRouteGroups(t *testing.T) {
	c := build(t, quick("default"), nil, nil, recv("default", "slack"), recv("mine", "slack"))
	own := &OwnRoute{Receiver: "mine"}
	mk := func(name, folder string, o *OwnRoute) Alert {
		return Alert{Labels: map[string]string{"alertname": name, "grafana_folder": folder}, Start: t0, End: t0.Add(time.Hour), Own: o}
	}
	res, err := c.Run([]Alert{mk("A", "f", own), mk("B", "f", own), mk("A", "g", &OwnRoute{Receiver: "gone"})}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Groups != 3 { // A/f and B/f differ by alertname; the fallback is the tree's
		t.Fatalf("groups = %d, want 3", res.Groups)
	}
	if got := res.Alerts[2].Receivers; len(got) != 1 || got[0] != "default" {
		t.Fatalf("unknown contact point reached %v", got)
	}
	own2 := &OwnRoute{Receiver: "mine", GroupBy: []string{"grafana_folder"}}
	res, err = c.Run([]Alert{mk("A", "f", own2), mk("B", "f", own2)}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Groups != 1 || res.Alerts[0].Notifications[0] != res.Alerts[1].Notifications[0] {
		t.Fatalf("grouped by folder: %d groups", res.Groups)
	}
}

// A mute timing on the rule mutes its alert without muting the tree's.
func TestOwnRouteMuteTiming(t *testing.T) {
	always := map[string][]TimeInterval{"always": {{}}}
	c, err := NewConfig(quick("default"), []Receiver{recv("default", "slack"), recv("mine", "slack")}, always, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	a := Alert{Labels: map[string]string{"alertname": "A"}, Start: t0, End: t0.Add(time.Hour), Own: &OwnRoute{Receiver: "mine", MuteTimeIntervals: []string{"always"}}}
	b := Alert{Labels: map[string]string{"alertname": "B"}, Start: t0, End: t0.Add(time.Hour)}
	res, err := c.Run([]Alert{a, b}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Alerts[0].Notified != 0 || len(res.Alerts[0].Muted) == 0 || res.Alerts[0].Muted[0].Kind != MutedMuteTime {
		t.Fatalf("own alert: %+v", res.Alerts[0])
	}
	if res.Alerts[1].Notified == 0 {
		t.Fatal("the tree's alert was muted")
	}
}
