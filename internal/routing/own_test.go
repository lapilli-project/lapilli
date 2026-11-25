package routing

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// A rule that names its contact point goes to it, past the policies, with
// its own grouping and timings; the root's timings fill what it leaves out.
func TestOwnRoute(t *testing.T) {
	root := quick("default", &Route{Receiver: "oncall", Matchers: []Matcher{mt(t, "team", "=", "db")}})
	c := build(t, root, nil, nil, recv("default", "slack"), recv("oncall", "pagerduty"), recv("mine", "pagerduty", "slack"))
	own := &OwnRoute{Receiver: "mine", GroupInterval: dur(2 * time.Minute)}
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
	// Grafana puts grafana_folder and alertname first in any group_by
	// (NormalizedGroupBy), so naming only the folder still splits by name; a
	// list with ... is a group for each label set, whatever else it names.
	own2 := &OwnRoute{Receiver: "mine", GroupBy: []string{"grafana_folder"}}
	res, err = c.Run([]Alert{mk("A", "f", own2), mk("B", "f", own2)}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Groups != 2 {
		t.Fatalf("grouped by folder, normalized: %d groups, want 2 (alertname is always in it)", res.Groups)
	}
	own3 := &OwnRoute{Receiver: "mine", GroupBy: []string{"severity", "...", "grafana_folder"}}
	a, b := mk("A", "f", own3), mk("A", "f", own3)
	a.Labels["x"], b.Labels["x"] = "1", "2"
	if res, err = c.Run([]Alert{a, b}, Options{}); err != nil || res.Groups != 2 {
		t.Fatalf("a group_by with ...: %d groups, %v; want 2", res.Groups, err)
	}
}

func TestNormalizedGroupBy(t *testing.T) {
	for _, tc := range []struct {
		in, want []string
	}{
		{nil, []string{"alertname", "grafana_folder"}},
		{[]string{"cluster"}, []string{"alertname", "cluster", "grafana_folder"}},
		{[]string{"b", "a", "b", "alertname"}, []string{"a", "alertname", "b", "grafana_folder"}},
		{[]string{"a", "...", "b"}, []string{"..."}},
	} {
		if got := NormalizedGroupBy(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("NormalizedGroupBy(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// An explicit 0s group_wait is no wait, not the root's; a timing under a
// second is read as a second, and said.
func TestOwnRouteTimings(t *testing.T) {
	c := build(t, quick("default"), nil, nil, recv("default", "slack"), recv("mine", "slack"))
	a := Alert{Labels: map[string]string{"alertname": "A"}, Start: t0, End: t0.Add(time.Hour), Own: &OwnRoute{Receiver: "mine", GroupWait: dur(0)}}
	res, err := c.Run([]Alert{a}, Options{})
	if err != nil || len(res.Notifications) == 0 || !res.Notifications[0].At.Equal(t0) {
		t.Fatalf("a group_wait of 0s: %v %+v", err, res.Notifications)
	}
	a.Own = &OwnRoute{Receiver: "mine", GroupInterval: dur(time.Nanosecond), RepeatInterval: dur(0)}
	res, err = c.Run([]Alert{a}, Options{})
	if err != nil || len(res.Warnings) != 2 || !strings.Contains(res.Warnings[0], "1s") {
		t.Fatalf("timings under a second: %v warnings %v", err, res.Warnings)
	}
	if len(res.Notifications) > 3601 {
		t.Fatalf("a one-second floor makes at most one a second, made %d notifications", len(res.Notifications))
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
