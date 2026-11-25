package routing

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A group whose first flush is at From or after isn't notified yet, however
// early its alert began; one whose first flush came before From was; and an
// alert that is Carried is taken as notified whatever its group_wait is.
func TestFromIsExact(t *testing.T) {
	c := build(t, quick("r"), nil, nil, recv("r", "pagerduty"))
	first := func(a Alert) []string {
		return seq(run(t, c, []Alert{a}, Options{From: t0, Until: t0.Add(time.Hour)}), "r", "")
	}
	// group_wait is 10s: began 5s before From, its first flush is 5s after it.
	if got := first(al(-5*time.Second, time.Hour, "alertname", "A")); len(got) == 0 || got[0] != "5s first" {
		t.Errorf("began 5s before From: %v", got)
	}
	// Began 20s before: its first flush was 10s before From.
	for _, s := range first(al(-20*time.Second, time.Hour, "alertname", "A")) {
		if strings.HasSuffix(s, " first") {
			t.Errorf("began 20s before From, notified a first: %v", s)
		}
	}
	// Carried, though its start is From and its group_wait 10s.
	a := al(0, time.Hour, "alertname", "A")
	a.Carried = true
	for _, s := range first(a) {
		if strings.HasSuffix(s, " first") {
			t.Errorf("a carried alert notified a first: %v", s)
		}
	}
}

// A group taken as notified before From has its repeats counted from when
// Notified says it last notified, not from From.
func TestNotifiedSeedsRepeats(t *testing.T) {
	c := build(t, quick("r"), nil, nil, recv("r", "pushover"))
	a := al(0, 3*time.Hour, "alertname", "A")
	a.Carried = true
	probe := run(t, c, []Alert{a}, Options{From: t0, Until: t0.Add(time.Hour)})
	if len(probe.Notifications) == 0 {
		t.Fatal("no repeat")
	}
	group := probe.Notifications[0].Group
	// Last notified 4 minutes before From: the next repeat is a minute after
	// it (plus a flush), not five.
	res := run(t, c, []Alert{a}, Options{From: t0, Until: t0.Add(time.Hour),
		Notified: []NotifiedAt{{Group: group, Receiver: "r", Kind: "pushover", At: t0.Add(-4 * time.Minute)}}})
	if got := res.Notifications[0].At.Sub(t0); got >= probe.Notifications[0].At.Sub(t0) {
		t.Errorf("repeat at %v with the last notification 4 minutes before From, %v without", got, probe.Notifications[0].At.Sub(t0))
	}
	// One after From is not a seed.
	res = run(t, c, []Alert{a}, Options{From: t0, Until: t0.Add(time.Hour),
		Notified: []NotifiedAt{{Group: group, Receiver: "r", Kind: "pushover", At: t0.Add(time.Minute)}}})
	if !res.Notifications[0].At.Equal(probe.Notifications[0].At) {
		t.Errorf("a notification after From moved the repeat: %v", res.Notifications[0].At)
	}
}

// PageRule is the rule the model pages by, and a notification that tells of
// resolved alerts alone is no page.
func TestPageRuleResolvedIsNone(t *testing.T) {
	for _, kind := range []string{"pagerduty", "pushover", "slack"} {
		if _, page := PageRule(kind, false, true, false, ReasonResolved); page {
			t.Errorf("%s: a page for resolved alerts alone", kind)
		}
	}
	if _, page := PageRule("pagerduty", false, true, true, ReasonRepeat); !page {
		t.Error("a repeat on a dedup kind of a receiver --paging-receivers matched is a page")
	}
	if _, page := PageRule("pagerduty", false, false, true, ReasonRepeat); page {
		t.Error("a repeat on a dedup kind is a page")
	}
	// The model: Pushover tells of the alert resolving, which rings no one.
	c := build(t, quick("r"), nil, nil, recv("r", "pushover"))
	res := run(t, c, []Alert{al(0, 20*time.Second, "alertname", "A")}, Options{})
	for _, n := range res.Notifications {
		if n.Reason == ReasonResolved && n.Page {
			t.Errorf("a resolved notification is a page: %+v", n)
		}
	}
}

// Silences up to MaxSilences are taken, and matching them is charged: a
// run over silences each of which matches every alert, asked about at every
// flush, stops at its budget instead of going on without one.
func TestSilenceMatchingIsCharged(t *testing.T) {
	c := build(t, quick("r"), nil, nil, recv("r", "slack"))
	re := mt(t, "alertname", "=~", ".*")
	var sils []Silence
	for i := range 2000 {
		sils = append(sils, Silence{ID: fmt.Sprint("s", i), Matchers: []Matcher{re}, Start: t0.Add(24 * time.Hour), End: t0.Add(25 * time.Hour)})
	}
	// One alert, on for hours, flushed every minute: each flush asks which
	// silence mutes it and when that changes, of 2,000 that match it.
	alerts := []Alert{al(0, 3*time.Hour, "alertname", "A")}
	full := run(t, c, alerts, Options{Silences: sils, Budget: 50_000_000})
	if full.Stopped != "" {
		t.Fatalf("stopped with a large budget: %s", full.Stopped)
	}
	small := run(t, c, alerts, Options{Silences: sils, Budget: 100_000})
	if !strings.Contains(small.Stopped, "work units") {
		t.Errorf("going over 2,000 matching silences at every flush wasn't charged: %q", small.Stopped)
	}
}
