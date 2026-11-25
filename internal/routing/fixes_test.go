package routing

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A tool that keeps one open incident for a group (PagerDuty's dedup_key,
// Opsgenie's alias, VictorOps' entity_id) isn't paged by a repeat, nor by new
// alerts in the group: the page is the notification that opens it.
func TestDedupKindsPageOncePerIncident(t *testing.T) {
	for _, kind := range []string{"pagerduty", "opsgenie", "victorops", "incidentio"} {
		if !DedupsByGroup(kind) || !PagingKind(kind, false) {
			t.Fatalf("%s should page and dedup by group", kind)
		}
		c := build(t, quick("r"), nil, nil, recv("r", kind))
		// Firing an hour with a repeat every five minutes; a second alert of
		// the group joins; it resolves; the group fires again.
		alerts := []Alert{al(0, 20*time.Minute, "alertname", "A"), al(7*time.Minute, 20*time.Minute, "alertname", "A", "x", "1"),
			al(40*time.Minute, 60*time.Minute, "alertname", "A")}
		res, err := c.Run(alerts, Options{})
		if err != nil {
			t.Fatal(err)
		}
		pages, onPaging := 0, 0
		for _, nt := range res.Notifications {
			if nt.Paging {
				onPaging++
			}
			if nt.Page {
				pages++
				if nt.Reason != ReasonFirst || len(nt.Firing) == 0 {
					t.Fatalf("%s: a page that is %q with %d firing", kind, nt.Reason, len(nt.Firing))
				}
			}
		}
		if pages != 2 {
			t.Fatalf("%s: %d pages, want 2 (the first, and the first after all resolved)", kind, pages)
		}
		if onPaging <= pages+2 {
			t.Fatalf("%s: %d notifications on the paging integration, want repeats and updates beyond the %d pages", kind, onPaging, pages)
		}
		if a := res.Alerts[0]; a.Paged != 1 || a.OnPaging <= a.Paged {
			t.Fatalf("%s: first alert paged %d, on paging %d", kind, a.Paged, a.OnPaging)
		}
	}
}

// Pushover rings for each notification, and so does a receiver
// --paging-receivers matched, whatever its kinds.
func TestPageForEachNotification(t *testing.T) {
	c := build(t, quick("r"), nil, nil, recv("r", "pushover"), recv("w", "pagerduty"))
	res, err := c.Run([]Alert{al(0, 30*time.Minute, "alertname", "A")}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, nt := range res.Notifications {
		if nt.Page != nt.Paging || !nt.Page {
			t.Fatalf("pushover notification: Page %v Paging %v", nt.Page, nt.Paging)
		}
		n++
	}
	if n < 4 {
		t.Fatalf("only %d notifications", n)
	}
	c = build(t, quick("w"), nil, nil, recv("w", "pagerduty"))
	res, err = c.Run([]Alert{al(0, 30*time.Minute, "alertname", "A")}, Options{PagingReceivers: regexp.MustCompile("^w$")})
	if err != nil {
		t.Fatal(err)
	}
	for _, nt := range res.Notifications {
		if !nt.Page {
			t.Fatalf("a receiver --paging-receivers matched didn't page for %+v", nt)
		}
	}
}

// A group's key is short, and written whole as Alertmanager writes it.
func TestGroupKey(t *testing.T) {
	root := quick("r", &Route{Receiver: "r", Matchers: []Matcher{mt(t, "severity", "=", "page")}}, &Route{Receiver: "r", Matchers: []Matcher{mt(t, "team", "=~", "a|b")}})
	c := build(t, root, nil, nil, recv("r", "slack"))
	rs := c.Routes()
	if rs[0].ID != "0" || rs[1].ID != "0.0" || rs[2].ID != "0.1" {
		t.Fatalf("route IDs %q %q %q", rs[0].ID, rs[1].ID, rs[2].ID)
	}
	res, err := c.Run([]Alert{al(0, time.Hour, "alertname", "A", "team", "a")}, Options{})
	if err != nil || len(res.Notifications) == 0 {
		t.Fatal(err)
	}
	nt := res.Notifications[0]
	if nt.Group != `0.1:{}` || c.GroupKey(nt) != `{}/{team=~"a|b"}:{}` {
		t.Fatalf("group %q, key %q", nt.Group, c.GroupKey(nt))
	}
	// A group of a long label set is cut, with a hash that keeps it apart.
	long := strings.Repeat("x", 3000)
	root = quick("r")
	root.GroupByAll = true
	c = build(t, root, nil, nil, recv("r", "slack"))
	res, err = c.Run([]Alert{al(0, time.Hour, "alertname", "A", "l", long+"1"), al(0, time.Hour, "alertname", "A", "l", long+"2")}, Options{})
	if err != nil || res.Groups != 2 || len(res.Notifications[0].Group) > 1100 || res.Notifications[0].Group == res.Notifications[1].Group {
		t.Fatalf("long group keys: %v %d groups", err, res.Groups)
	}
}

func TestBudgetAndContext(t *testing.T) {
	c := build(t, quick("r"), nil, nil, recv("r", "slack"))
	var alerts []Alert
	for i := 0; i < 500; i++ {
		alerts = append(alerts, al(0, 2*time.Hour, "alertname", fmt.Sprint("A", i)))
	}
	res, err := c.Run(alerts, Options{Budget: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stopped, "budget of 2000") {
		t.Fatalf("Stopped = %q", res.Stopped)
	}
	// A notification costs 50: what is kept is bounded by the budget.
	if len(res.Notifications) > 2000/50 {
		t.Fatalf("%d notifications kept past a budget of 2000", len(res.Notifications))
	}
	if res, err = c.Run(alerts, Options{}); err != nil || res.Stopped != "" {
		t.Fatalf("the default budget stopped a small run: %v %q", err, res.Stopped)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err = c.RunContext(ctx, alerts, Options{})
	if err != nil || !strings.Contains(res.Stopped, "canceled") || len(res.Notifications) != 0 {
		t.Fatalf("a canceled run: %v %q, %d notifications", err, res.Stopped, len(res.Notifications))
	}
}

func TestSubSecondTimingsRefused(t *testing.T) {
	for _, r := range []*Route{
		{Receiver: "r", GroupInterval: dur(time.Millisecond), RepeatInterval: dur(time.Hour)},
		{Receiver: "r", RepeatInterval: dur(time.Nanosecond)},
		{Receiver: "r", GroupWait: dur(time.Millisecond)},
	} {
		_, err := NewConfig(r, []Receiver{recv("r", "slack")}, nil, nil, false)
		if err == nil || !strings.Contains(err.Error(), "doesn't model timings under") {
			t.Fatalf("%+v: %v", r, err)
		}
	}
	// An explicit group_wait of 0 is valid.
	if _, err := NewConfig(&Route{Receiver: "r", GroupWait: dur(0)}, []Receiver{recv("r", "slack")}, nil, nil, false); err != nil {
		t.Fatal(err)
	}
}

func TestRegexProgramBounds(t *testing.T) {
	if _, err := NewMatcher("a", "=~", "(ab|cd|ef|gh|ij|kl|mn|op|qr|st){1000}"); err == nil || !strings.Contains(err.Error(), "instructions") {
		t.Fatalf("a program of a million instructions: %v", err)
	}
	if _, err := NewMatcher("a", "=~", "foo|bar.*"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMatcher(strings.Repeat("n", MaxMatcherText+1), "=", "x"); err == nil {
		t.Fatal("a matcher's name of 4 KB and a byte")
	}
	if _, err := NewMatcher("n", "=", strings.Repeat("v", MaxMatcherText+1)); err == nil {
		t.Fatal("a matcher's value of 4 KB and a byte")
	}
	// A budget is spent by the matchers it is given to.
	b := NewRegexBudget()
	var err error
	for i := 0; i < 400 && err == nil; i++ {
		_, err = NewMatcherBudget(b, "a", "=~", "(ab|cd|ef|gh|ij|kl|mn|op|qr|st){90}"+fmt.Sprint(i))
	}
	if err == nil || !strings.Contains(err.Error(), "in all") {
		t.Fatalf("the budget of all: %v", err)
	}
	// And NewConfig holds a Config built any other way to it.
	var ms []Matcher
	for i := 0; i < 400; i++ {
		m, err := NewMatcher("a", "=~", "(ab|cd|ef|gh|ij|kl|mn|op|qr|st){90}"+fmt.Sprint(i))
		if err != nil {
			t.Fatal(err)
		}
		ms = append(ms, m)
	}
	if _, err := NewConfig(&Route{Receiver: "r", Routes: []*Route{{Matchers: ms}}}, []Receiver{recv("r", "slack")}, nil, nil, false); err == nil {
		t.Fatal("a configuration whose regular expressions add up to millions of instructions")
	}
}

func TestStringBounds(t *testing.T) {
	rc := []Receiver{recv("r", "slack")}
	if _, err := NewConfig(&Route{Receiver: "r"}, rc, map[string][]TimeInterval{strings.Repeat("n", MaxName+1): {{}}}, nil, false); err == nil {
		t.Fatal("a time interval's name over 1,024 bytes")
	}
	if _, err := NewConfig(&Route{Receiver: "r"}, rc, nil, []InhibitRule{{Name: strings.Repeat("n", MaxName+1)}}, false); err == nil {
		t.Fatal("an inhibition rule's name over 1,024 bytes")
	}
	// A route's key: its matchers and its ancestors'.
	var chain *Route
	for i := 0; i < 20; i++ {
		var ms []Matcher
		for j := 0; j < 5; j++ {
			ms = append(ms, mt(t, fmt.Sprint("l", j), "=", strings.Repeat("v", 1000)))
		}
		r := &Route{Matchers: ms}
		if chain != nil {
			r.Routes = []*Route{chain}
		}
		chain = r
	}
	if _, err := NewConfig(&Route{Receiver: "r", Routes: []*Route{chain}}, rc, nil, nil, false); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("a route key of 100 KB: %v", err)
	}
	// A route's interval lists lose their repeats.
	c, err := NewConfig(&Route{Receiver: "r", MuteTimeIntervals: []string{"n", "n", "n"}}, rc, map[string][]TimeInterval{"n": {{Weekdays: []Range{{0, 6}}}}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Routes()[0].MuteTimeIntervals; len(got) != 1 {
		t.Fatalf("mute intervals %v", got)
	}
	// What muted it, in a Muting, is cut.
	names := map[string][]TimeInterval{}
	var list []string
	for i := 0; i < 100; i++ {
		n := fmt.Sprintf("%s%d", strings.Repeat("m", 900), i)
		names[n] = []TimeInterval{{}}
		list = append(list, n)
	}
	c, err = NewConfig(&Route{Receiver: "r", MuteTimeIntervals: list}, rc, names, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Run([]Alert{al(0, time.Hour, "alertname", "A")}, Options{})
	if err != nil || len(res.Alerts[0].Muted) != 1 || len(res.Alerts[0].Muted[0].By) > MaxBy+len("…") {
		t.Fatalf("By: %v %d", err, len(res.Alerts[0].Muted[0].By))
	}
}

func TestTimeIntervalBounds(t *testing.T) {
	// Parts are counted for all of a configuration's time intervals.
	big := TimeInterval{Weekdays: make([]Range, 900)}
	intervals := map[string][]TimeInterval{}
	for i := 0; i < 30; i++ {
		intervals[fmt.Sprint("i", i)] = []TimeInterval{big, big}
	}
	if _, err := NewConfig(&Route{Receiver: "r"}, []Receiver{recv("r", "slack")}, intervals, nil, false); err == nil || !strings.Contains(err.Error(), "in all") {
		t.Fatalf("time intervals of 54,000 parts: %v", err)
	}
	// A zone loads once by its name, and two loads of one zone are one zone.
	for i := 0; i < 50; i++ {
		if _, err := ParseTimeInterval(map[string]any{"location": "Asia/Seoul"}); err != nil {
			t.Fatal(err)
		}
	}
	zones.Lock()
	n := len(zones.by)
	zones.Unlock()
	if n != 1 {
		t.Fatalf("%d zones cached for 50 reads of one", n)
	}
	var tis []TimeInterval
	for i := 0; i < 200; i++ {
		loc, _ := time.LoadLocation("Asia/Seoul")
		tis = append(tis, TimeInterval{Location: loc, Times: []TimeRange{{60 + i, 600 + i}}})
	}
	_, work, over := nextChangeWork(tis, t0, 30*24*time.Hour, 0)
	if over || work == 0 {
		t.Fatalf("work %d over %v", work, over)
	}
	// Looking is limited: past the limit it gives up, and says so.
	if _, _, over := nextChangeWork(tis, t0, 30*24*time.Hour, 500); !over {
		t.Fatal("a limit of 500 wasn't reached")
	}
	// And never looks further than MaxLookAhead.
	never := []TimeInterval{{Years: []Range{{3000, 3001}}}}
	if at := nextChange(never, t0, 100*365*24*time.Hour); !at.IsZero() {
		t.Fatalf("a change in the year 3000 at %v", at)
	}
}

// Silences are found by their = matchers, not by trying each of them for
// each label set: tens of thousands of them cost what the label sets have.
func TestSilencesIndexed(t *testing.T) {
	var sils []Silence
	for i := 0; i < MaxSilences; i++ {
		sils = append(sils, Silence{ID: fmt.Sprint("s", i), Matchers: []Matcher{mt(t, "alertname", "=", fmt.Sprint("X", i))}, Start: t0.Add(-time.Hour), End: t0.Add(24 * time.Hour)})
	}
	c := build(t, quick("r"), nil, nil, recv("r", "slack"))
	var alerts []Alert
	for i := 0; i < 200; i++ {
		alerts = append(alerts, al(0, time.Hour, "alertname", fmt.Sprint("X", i*50)))
	}
	res, err := c.Run(alerts, Options{Silences: sils, Budget: 100_000})
	if err != nil || res.Stopped != "" {
		t.Fatalf("%v %q", err, res.Stopped)
	}
	for i, a := range res.Alerts {
		if a.Notified != 0 || len(a.Muted) != 1 || a.Muted[0].By != fmt.Sprint("s", i*50) {
			t.Fatalf("alert %d: %+v", i, a)
		}
	}
	if _, err := c.Run(alerts, Options{Silences: append(sils, sils[0])}); err == nil {
		t.Fatal("more silences than a run takes")
	}
	// One with no = matcher is tried for each, and still silences.
	re, err := NewMatcher("alertname", "=~", "Y.*")
	if err != nil {
		t.Fatal(err)
	}
	neg := mt(t, "alertname", "!=", "Z")
	res, err = c.Run([]Alert{al(0, time.Hour, "alertname", "Y1")}, Options{Silences: []Silence{{ID: "re", Matchers: []Matcher{re, neg}, Start: t0.Add(-time.Hour), End: t0.Add(time.Hour)}}})
	if err != nil || res.Alerts[0].Notified != 0 {
		t.Fatalf("a regex silence: %v %+v", err, res.Alerts[0])
	}
}

// A group whose alert was on when the period began was notified before it:
// no first notification at the period's start plus group_wait.
func TestCarriedFirings(t *testing.T) {
	c := build(t, quick("r"), nil, nil, recv("r", "pagerduty"))
	a := al(-3*time.Hour, 3*time.Hour, "alertname", "A")
	a.Start = t0 // as the verdict clamps one on since before the period
	a.Carried = true
	res, err := c.Run([]Alert{a}, Options{From: t0, Until: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, nt := range res.Notifications {
		if nt.Reason == ReasonFirst || nt.Page {
			t.Fatalf("a carried firing's group notified a first at %v", nt.At)
		}
	}
	if len(res.Notifications) == 0 || !res.Notifications[0].At.Equal(t0.Add(5*time.Minute+10*time.Second)) || res.Notifications[0].Reason != ReasonRepeat {
		t.Fatalf("notifications %+v: want a repeat at the flush after the period's start plus repeat_interval", res.Notifications)
	}
	// A firing that starts after the period began is as it was.
	b := al(10*time.Minute, time.Hour, "alertname", "B")
	res, err = c.Run([]Alert{b}, Options{From: t0})
	if err != nil || res.Notifications[0].Reason != ReasonFirst {
		t.Fatalf("a firing after the start: %v %+v", err, res.Notifications)
	}
}
