package routing

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"
)

const (
	min = time.Minute
	hr  = time.Hour
)

// A route and its children: the first matching child ends the search among
// its siblings unless it continues; a route none of whose children match
// is the match itself.
func TestRouteContinue(t *testing.T) {
	root := quick("root",
		&Route{Receiver: "a", Matchers: []Matcher{mt(t, "team", "=", "a")}, Continue: true},
		&Route{Receiver: "b", Matchers: []Matcher{mt(t, "team", "=~", "a|b")}},
		&Route{Receiver: "c", Matchers: []Matcher{mt(t, "severity", "=", "page")}},
		&Route{Receiver: "d", Matchers: []Matcher{mt(t, "team", "=", "n")},
			Routes: []*Route{{Matchers: []Matcher{mt(t, "env", "=", "prod")}, Receiver: "e"}}})
	c := build(t, root, nil, nil, recv("root", "webhook"), recv("a", "webhook"), recv("b", "webhook"), recv("c", "webhook"), recv("d", "webhook"), recv("e", "webhook"))
	receivers := func(labels ...string) []string {
		var out []string
		for _, i := range c.Match(al(0, 0, labels...).Labels) {
			out = append(out, c.Routes()[i].Receiver)
		}
		return out
	}
	for _, tc := range []struct {
		labels []string
		want   []string
	}{
		{[]string{"team", "a", "severity", "page"}, []string{"a", "b"}}, // a continues, b stops
		{[]string{"team", "b", "severity", "page"}, []string{"b"}},
		{[]string{"severity", "page"}, []string{"c"}},
		{[]string{"severity", "info"}, []string{"root"}},
		{[]string{"team", "n"}, []string{"d"}},                                    // no child of d matches: d itself
		{[]string{"team", "n", "env", "prod"}, []string{"e"}},                     // its child does
		{[]string{"team", "n", "env", "prod", "severity", "page"}, []string{"c"}}, // c comes first and stops
	} {
		if got := receivers(tc.labels...); !slices.Equal(got, tc.want) {
			t.Errorf("%v: %v, want %v", tc.labels, got, tc.want)
		}
	}
	// A label an alert lacks is the empty text: != and a regular expression
	// that matches it do.
	for _, tc := range []struct {
		m    Matcher
		want bool
	}{
		{mt(t, "x", "!=", "y"), true}, {mt(t, "x", "=", ""), true}, {mt(t, "x", "=~", ".*"), true},
		{mt(t, "x", "=~", "y"), false}, {mt(t, "x", "!~", "y"), true}, {mt(t, "x", "=", "y"), false},
	} {
		if got := tc.m.Matches(map[string]string{}); got != tc.want {
			t.Errorf("%v on an alert without x: %v", tc.m, got)
		}
	}
	// Regular expressions are anchored.
	if mt(t, "x", "=~", "ab").Matches(map[string]string{"x": "abc"}) {
		t.Error("an unanchored regular expression")
	}
	if _, err := NewMatcher("x", "=~", "("); err == nil {
		t.Error("a regular expression that doesn't compile")
	}
}

// What a route inherits, and what it doesn't: receiver, group_by (an
// explicit one, `...`, or an empty list that clears), the three timings,
// with Alertmanager's defaults; not the time intervals.
func TestInheritance(t *testing.T) {
	root := &Route{Receiver: "r", GroupBy: []string{"b", "a"}, GroupBySet: true, MuteTimeIntervals: []string{"night"},
		Routes: []*Route{
			{Matchers: []Matcher{mt(t, "i", "=", "1")}},
			{Matchers: []Matcher{mt(t, "i", "=", "2")}, GroupByAll: true, GroupWait: dur(time.Second), Receiver: "s",
				Routes: []*Route{
					{Matchers: []Matcher{mt(t, "j", "=", "1")}},
					{Matchers: []Matcher{mt(t, "j", "=", "2")}, GroupBy: []string{"c"}, GroupBySet: true, RepeatInterval: dur(hr)},
				}},
			{Matchers: []Matcher{mt(t, "i", "=", "3")}, GroupBy: []string{}, GroupBySet: true, ActiveTimeIntervals: []string{"night"}},
		}}
	night := []TimeInterval{{Times: []TimeRange{{0, 360}}}}
	c := build(t, root, map[string][]TimeInterval{"night": night}, nil, recv("r", "slack"), recv("s", "slack"))
	rs := c.Routes()
	type want struct {
		recv    string
		by      []string
		all     bool
		wait    time.Duration
		repeat  time.Duration
		mute    []string
		active  []string
		keyPart string
	}
	for i, w := range []want{
		{"r", []string{"a", "b"}, false, 30 * time.Second, 4 * hr, []string{"night"}, nil, "{}"},
		{"r", []string{"a", "b"}, false, 30 * time.Second, 4 * hr, nil, nil, `{i="1"}`}, // mute isn't inherited
		{"s", []string{"a", "b"}, true, time.Second, 4 * hr, nil, nil, `{i="2"}`},
		{"s", []string{"a", "b"}, true, time.Second, 4 * hr, nil, nil, `{}/{i="2"}/{j="1"}`},
		{"s", []string{"c"}, false, time.Second, hr, nil, nil, `{}/{i="2"}/{j="2"}`},
		{"r", []string{}, false, 30 * time.Second, 4 * hr, nil, []string{"night"}, `{i="3"}`},
	} {
		r := rs[i]
		if r.Receiver != w.recv || !slices.Equal(r.GroupBy, w.by) || r.GroupByAll != w.all && !(i == 3 && r.GroupByAll) || r.GroupWait != w.wait || r.RepeatInterval != w.repeat ||
			!slices.Equal(r.MuteTimeIntervals, w.mute) || !slices.Equal(r.ActiveTimeIntervals, w.active) || r.GroupInterval != 5*min {
			t.Errorf("route %d: %+v, want %+v", i, r, w)
		}
	}
	// Group by every label, inherited by the child that doesn't say; and
	// cleared by one that lists labels.
	if !rs[3].GroupByAll || rs[4].GroupByAll {
		t.Errorf("group_by ...: %v %v", rs[3].GroupByAll, rs[4].GroupByAll)
	}
	if rs[2].Key != `{}/{i="2"}` || rs[0].Key != "{}" {
		t.Errorf("keys %q %q", rs[0].Key, rs[2].Key)
	}
	// What can't be: an undefined receiver or time interval, a zero interval.
	for _, bad := range []*Route{
		{Receiver: "nobody"},
		{Receiver: "r", MuteTimeIntervals: []string{"nope"}},
		{Receiver: "r", GroupInterval: dur(0)},
		{Receiver: "r", Routes: []*Route{{Receiver: "nobody"}}},
		{},
	} {
		if _, err := NewConfig(bad, []Receiver{recv("r", "slack")}, nil, nil, false); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// group_by: alerts in a group are one notification; `...` makes a group of
// each; an empty list, one of all.
func TestGrouping(t *testing.T) {
	alerts := []Alert{
		al(0, 0, "alertname", "Disk", "instance", "a", "dc", "x"),
		al(0, 0, "alertname", "Disk", "instance", "b", "dc", "x"),
		al(0, 0, "alertname", "Disk", "instance", "c", "dc", "y"),
		al(0, 0, "alertname", "Mem", "instance", "a", "dc", "x"),
	}
	for _, tc := range []struct {
		name   string
		route  func(*Route)
		groups int
		first  int // alerts in the first notification of the biggest group
	}{
		{"by alertname", func(r *Route) { r.GroupBy, r.GroupBySet = []string{"alertname"}, true }, 2, 3},
		{"by alertname and dc", func(r *Route) { r.GroupBy, r.GroupBySet = []string{"alertname", "dc"}, true }, 3, 2},
		{"by ...", func(r *Route) { r.GroupByAll = true }, 4, 1},
		{"by nothing", func(r *Route) { r.GroupBy, r.GroupBySet = []string{}, true }, 1, 4},
		{"by a label none has", func(r *Route) { r.GroupBy, r.GroupBySet = []string{"zone"}, true }, 1, 4},
	} {
		root := quick("r")
		tc.route(root)
		res := run(t, build(t, root, nil, nil, recv("r", "webhook")), alerts, Options{Until: t0.Add(30 * time.Second)})
		if res.Groups != tc.groups {
			t.Errorf("%s: %d groups, want %d", tc.name, res.Groups, tc.groups)
		}
		most := 0
		for _, n := range res.Notifications {
			most = max(most, len(n.Firing))
		}
		if most != tc.first || len(res.Notifications) != tc.groups {
			t.Errorf("%s: %d notifications, the biggest of %d alerts; want %d, %d", tc.name, len(res.Notifications), most, tc.groups, tc.first)
		}
	}
}

// group_wait holds the first notification, a group's flushes come every
// group_interval after it, an alert that comes between is told of at the
// next, and repeat_interval repeats only once it has passed (strictly).
func TestTiming(t *testing.T) {
	c := build(t, quick("r"), nil, nil, recv("r", "pagerduty"))
	alerts := []Alert{
		al(0, 20*min, "alertname", "A", "i", "1"),
		al(2*min+30*time.Second, 0, "alertname", "A", "i", "2"),
	}
	res := run(t, c, alerts, Options{Until: t0.Add(25 * min)})
	eq(t, "pagerduty", seq(res, "r", ""), []string{
		"10s first",
		"3m10s new",       // the second came at 2m30s: told at the flush after
		"9m10s repeat",    // 3m10s and 5m is 8m10s exactly, not yet passed: the flush after
		"15m10s repeat",   // and 6 minutes on
		"20m10s resolved", // the first ended at 20m: told at the flush after
	})
}

// An integration that doesn't send resolves isn't told of them, but its log
// has the group's resolution, so the alert firing again is a first
// notification, as it is to an integration that does send them. Only the
// ones that do are sent a notification of resolved alerts.
func TestSendResolved(t *testing.T) {
	c := build(t, quick("r"), nil, nil, recv("r", "webhook", "slack"))
	alerts := []Alert{
		al(0, 5*min, "alertname", "A"),
		al(30*min, 40*min, "alertname", "A"),       // the same alert again
		al(0, 5*time.Second, "alertname", "Quick"), // resolved before group_wait: told of to no one
	}
	res := run(t, c, alerts, Options{})
	eq(t, "webhook", seq(res, "r", "webhook"), []string{"10s first", "5m10s resolved", "30m10s first", "36m10s repeat", "40m10s resolved"})
	eq(t, "slack", seq(res, "r", "slack"), []string{"10s first", "30m10s first", "36m10s repeat"})
	if len(res.Alerts[2].Notifications) != 0 {
		t.Errorf("an alert that resolved within group_wait was told of: %v", res.Alerts[2])
	}

	// One of two resolves: told to the integration that sends resolves,
	// not to the other.
	res = run(t, c, []Alert{al(0, 0, "alertname", "A", "i", "1"), al(0, 3*min, "alertname", "A", "i", "2")}, Options{Until: t0.Add(4 * min)})
	eq(t, "webhook", seq(res, "r", "webhook"), []string{"10s first", "3m10s resolved"})
	eq(t, "slack", seq(res, "r", "slack"), []string{"10s first"})
	last := res.Notifications[len(res.Notifications)-1]
	if len(last.Firing) != 1 || len(last.Resolved) != 1 || last.Resolved[0] != 1 {
		t.Errorf("the resolved notification: %+v", last)
	}
}

// An inhibition rule mutes an alert that matches its target while one that
// matches its source fires and agrees on the equal labels; absent agrees
// with absent; an alert never inhibits itself.
func TestInhibition(t *testing.T) {
	rule := InhibitRule{SourceMatchers: []Matcher{mt(t, "severity", "=", "critical")}, TargetMatchers: []Matcher{mt(t, "severity", "=", "warning")}, Equal: []string{"cluster"}}
	root := quick("r")
	root.GroupByAll = true
	c := build(t, root, nil, []InhibitRule{rule}, recv("r", "pagerduty"))
	alerts := []Alert{
		al(0, 20*min, "severity", "critical", "cluster", "a"),
		al(0, 30*min, "severity", "warning", "cluster", "a"), // inhibited until the critical one resolves
		al(0, 30*min, "severity", "warning", "cluster", "b"), // another cluster: not
		al(0, 30*min, "severity", "warning"),                 // no cluster: absent isn't "a"
	}
	res := run(t, c, alerts, Options{})
	notified := func(i int) []string { return notifiedAt(res, i) }
	eq(t, "critical", notified(0), []string{"10s first", "6m10s repeat", "12m10s repeat", "18m10s repeat", "20m10s resolved"})
	eq(t, "inhibited", notified(1), []string{"20m10s first", "26m10s repeat", "30m10s resolved"})
	eq(t, "other cluster", notified(2)[:1], []string{"10s first"})
	eq(t, "no cluster", notified(3)[:1], []string{"10s first"})
	m := res.Alerts[1].Muted
	if len(m) != 1 || m[0].Kind != MutedInhibition || m[0].Rule != 0 || m[0].Source != 0 || m[0].By != "#1" || !m[0].First.Equal(t0.Add(10*time.Second)) {
		t.Errorf("muted: %+v", m)
	}
	if len(res.Alerts[0].Muted)+len(res.Alerts[2].Muted)+len(res.Alerts[3].Muted) != 0 {
		t.Error("an alert muted that no rule mutes")
	}

	// Absent agrees with absent: neither has a cluster.
	res = run(t, c, []Alert{al(0, 20*min, "severity", "critical"), al(0, 30*min, "severity", "warning")}, Options{})
	if got := res.Alerts[1].Notifications; len(got) == 0 || !res.Notifications[got[0]].At.Equal(t0.Add(20*min+10*time.Second)) {
		t.Errorf("absent labels: %v", got)
	}
	// An alert that matches both sides doesn't inhibit itself, nor another
	// that does.
	both := InhibitRule{Name: "any", SourceMatchers: []Matcher{mt(t, "severity", "=~", ".+")}, TargetMatchers: []Matcher{mt(t, "severity", "=~", ".+")}}
	c = build(t, root, nil, []InhibitRule{both}, recv("r", "pagerduty"))
	res = run(t, c, []Alert{al(0, 10*min, "severity", "a"), al(0, 10*min, "severity", "b")}, Options{Until: t0.Add(time.Minute)})
	if len(res.Alerts[0].Muted)+len(res.Alerts[1].Muted) != 0 || res.Alerts[0].Notified != 1 || res.Alerts[1].Notified != 1 {
		t.Errorf("alerts that match both sides: %+v", res.Alerts)
	}
	// But one that only matches the target is inhibited by one that matches
	// both: the first rule that does it is named.
	rule2 := InhibitRule{Name: "page", SourceMatchers: []Matcher{mt(t, "severity", "=~", ".+")}, TargetMatchers: []Matcher{mt(t, "kind", "=", "t")}}
	c = build(t, root, nil, []InhibitRule{rule, rule2}, recv("r", "pagerduty"))
	res = run(t, c, []Alert{al(0, 10*min, "severity", "a"), al(0, 10*min, "kind", "t")}, Options{Until: t0.Add(time.Minute)})
	if m := res.Alerts[1].Muted; len(m) != 1 || m[0].By != "page" || m[0].Source != 0 {
		t.Errorf("by a source that matches both: %+v", m)
	}
}

// A silence mutes the alerts it matches while it is in force: an alert is
// told of at the first flush after it ends, which can be a repeat; one before
// it starts is no different.
func TestSilences(t *testing.T) {
	root := quick("r")
	root.GroupBy, root.GroupBySet = []string{"alertname"}, true
	c := build(t, root, nil, nil, recv("r", "webhook"))
	sil := Silence{ID: "s1", Matchers: []Matcher{mt(t, "alertname", "=", "A")}, Start: t0.Add(5 * min), End: t0.Add(25 * min)}
	res := run(t, c, []Alert{al(0, 40*min, "alertname", "A"), al(0, 40*min, "alertname", "B")}, Options{Silences: []Silence{sil}})
	eq(t, "starts after the first", notifiedAt(res, 0), []string{"10s first", "25m10s repeat", "31m10s repeat", "37m10s repeat", "40m10s resolved"})
	if n := len(res.Alerts[1].Notifications); n < 4 || len(res.Alerts[1].Muted) != 0 {
		t.Errorf("an alert no silence matches: %+v", res.Alerts[1])
	}
	if m := res.Alerts[0].Muted; len(m) != 1 || m[0].Kind != MutedSilence || m[0].By != "s1" || !m[0].First.Equal(t0.Add(5*min+10*time.Second)) || !m[0].Last.Equal(t0.Add(6*min+10*time.Second)) {
		t.Errorf("muted: %+v", m)
	}
	// In force from before the alert: nothing until it ends.
	sil.Start = t0.Add(-hr)
	res = run(t, c, []Alert{al(0, 40*min, "alertname", "A")}, Options{Silences: []Silence{sil}})
	eq(t, "silenced from the start", notifiedAt(res, 0), []string{"25m10s first", "31m10s repeat", "37m10s repeat", "40m10s resolved"})
	// A silence that ends before the alert, or never matches it, mutes nothing.
	sil.End = t0.Add(-min)
	res = run(t, c, []Alert{al(0, 20*min, "alertname", "A")}, Options{Silences: []Silence{sil}})
	if len(res.Alerts[0].Muted) != 0 {
		t.Error("an expired silence muted an alert")
	}
	// An alert the silence mutes when it resolves isn't told of resolving,
	// and the log keeps what it had: the alert firing again is a repeat only.
	sil = Silence{ID: "s2", Matchers: []Matcher{mt(t, "alertname", "=", "A")}, Start: t0.Add(5 * min), End: t0.Add(hr)}
	res = run(t, c, []Alert{al(0, 20*min, "alertname", "A"), al(30*min, 50*min, "alertname", "A")}, Options{Silences: []Silence{sil}})
	eq(t, "silenced through its resolution", notifiedAt(res, 0), []string{"10s first"})
}

// notifiedAt are the notifications that told of alert i, firing or resolved.
func notifiedAt(r *Result, i int) []string {
	var out []string
	for _, n := range r.Notifications {
		if slices.Contains(n.Firing, i) || slices.Contains(n.Resolved, i) {
			out = append(out, n.At.Sub(t0).String()+" "+string(n.Reason))
		}
	}
	return out
}

// Mute time intervals mute a route's group while one holds, active ones while
// none does; a notification waits for the first flush after it ends.
func TestTimeIntervals(t *testing.T) {
	lunch := []TimeInterval{{Times: []TimeRange{{12 * 60, 13 * 60}}}}
	work := []TimeInterval{{Times: []TimeRange{{9 * 60, 17 * 60}}, Weekdays: []Range{{1, 5}}}}
	root := quick("r")
	root.MuteTimeIntervals = []string{"lunch"}
	c := build(t, root, map[string][]TimeInterval{"lunch": lunch}, nil, recv("r", "webhook"))
	res := run(t, c, []Alert{al(115*min, 210*min, "alertname", "A")}, Options{}) // 11:55 to 13:30
	got := notifiedAt(res, 0)
	if len(got) < 4 || got[0] != "1h55m10s first" || got[1] != "3h0m10s repeat" || got[2] != "3h6m10s repeat" || got[len(got)-1] != "3h30m10s resolved" {
		t.Errorf("a mute time interval: %v", got)
	}
	if m := res.Alerts[0].Muted; len(m) != 1 || m[0].Kind != MutedMuteTime || m[0].By != "lunch" || !m[0].First.Equal(t0.Add(2*hr+10*time.Second)) {
		t.Errorf("muted: %+v", m)
	}

	// Outside the active intervals: from 18:00 on Monday until 09:00 on Tuesday.
	root = quick("r")
	root.ActiveTimeIntervals = []string{"work"}
	c = build(t, root, map[string][]TimeInterval{"work": work}, nil, recv("r", "webhook"))
	res = run(t, c, []Alert{al(8*hr, 30*hr, "alertname", "A")}, Options{})
	got = notifiedAt(res, 0)
	if len(got) == 0 || got[0] != "23h0m10s first" {
		t.Errorf("an active time interval: %v", got)
	}
	if m := res.Alerts[0].Muted; len(m) != 1 || m[0].Kind != MutedActiveTime || m[0].By != "work" {
		t.Errorf("muted: %+v", m)
	}
	// A weekend: nothing until Monday.
	res = run(t, c, []Alert{{Labels: map[string]string{"alertname": "A"}, Start: time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC), End: time.Date(2026, 1, 13, 0, 0, 0, 0, time.UTC)}},
		Options{Until: time.Date(2026, 1, 14, 0, 0, 0, 0, time.UTC)})
	if got = notifiedAt(res, 0); len(got) == 0 || res.Notifications[0].At != time.Date(2026, 1, 12, 9, 0, 10, 0, time.UTC) {
		t.Errorf("a weekend: %v %v", got, res.Notifications)
	}
	// Muted at its end, an alert's resolution isn't told: the mute stage
	// stops the flush, which then sends nothing.
	root = quick("r")
	root.MuteTimeIntervals = []string{"lunch"}
	c = build(t, root, map[string][]TimeInterval{"lunch": lunch}, nil, recv("r", "webhook"))
	res = run(t, c, []Alert{al(115*min, 130*min, "alertname", "A")}, Options{}) // resolves at 12:10
	eq(t, "resolved while muted", notifiedAt(res, 0), []string{"1h55m10s first"})
}

// A receiver with several integrations sends one notification on each, in
// Alertmanager's order of kinds; paging by kind, or by receiver.
func TestIntegrations(t *testing.T) {
	root := quick("r", &Route{Receiver: "quiet", Matchers: []Matcher{mt(t, "team", "=", "q")}})
	c := build(t, root, nil, nil, recv("r", "webhook", "pagerduty", "slack", "pagerduty"), recv("quiet"))
	res := run(t, c, []Alert{al(0, 0, "alertname", "A"), al(0, 0, "alertname", "B", "team", "q")}, Options{Until: t0.Add(time.Minute)})
	var got []string
	for _, n := range res.Notifications {
		got = append(got, fmt.Sprintf("%s[%d] page=%v %v", n.Kind, n.Index, n.Page, n.Reason))
	}
	eq(t, "integrations", got, []string{"pagerduty[0] page=true first", "pagerduty[1] page=true first", "slack[0] page=false first", "webhook[0] page=false first"})
	a := res.Alerts[0]
	if a.Notified != 4 || a.Paged != 2 || !a.FirstPage.Equal(t0.Add(10*time.Second)) || len(a.Receivers) != 1 || len(a.Empty) != 0 {
		t.Errorf("alert A: %+v", a)
	}
	// B reached a receiver that sends nothing.
	if b := res.Alerts[1]; b.Notified != 0 || !slices.Equal(b.Empty, []string{"quiet"}) || !slices.Equal(b.Receivers, []string{"quiet"}) {
		t.Errorf("alert B: %+v", b)
	}
	if !reflect.DeepEqual(res.Paging, []Paging{{Receiver: "r", Kinds: []string{"pagerduty"}}}) {
		t.Errorf("paging: %+v", res.Paging)
	}
	// --paging-receivers makes a webhook's notifications pages.
	res = run(t, c, []Alert{al(0, 0, "alertname", "A")}, Options{Until: t0.Add(time.Minute), PagingReceivers: regexp.MustCompile("^r$")})
	if res.Alerts[0].Paged != 4 || !res.Paging[0].ByPattern {
		t.Errorf("by pattern: %+v %+v", res.Alerts[0], res.Paging)
	}
	// Only Grafana's oncall pages as a kind.
	if PagingKind("oncall", false) || !PagingKind("oncall", true) || PagingKind("slack", true) || !PagingKind("victorops", false) || !PagingKind("pushover", false) || !PagingKind("opsgenie", false) {
		t.Error("kinds that page")
	}
}

// What the labels are: the external labels are added where an alert lacks
// them, so a route can match on them.
func TestExternalLabels(t *testing.T) {
	root := quick("r", &Route{Receiver: "prod", Matchers: []Matcher{mt(t, "cluster", "=", "prod")}})
	c := build(t, root, nil, nil, recv("r", "webhook"), recv("prod", "webhook"))
	res := run(t, c, []Alert{al(0, 0, "alertname", "A"), al(0, 0, "alertname", "B", "cluster", "dev")},
		Options{ExternalLabels: map[string]string{"cluster": "prod"}, Until: t0.Add(time.Minute)})
	if !slices.Equal(res.Alerts[0].Receivers, []string{"prod"}) || !slices.Equal(res.Alerts[1].Receivers, []string{"r"}) {
		t.Errorf("%v %v", res.Alerts[0].Receivers, res.Alerts[1].Receivers)
	}
}

// A run is worked out by events, not by ticks: a year of an alert firing is
// its repeats; it is deterministic; it stops at the groups it may have.
func TestBounds(t *testing.T) {
	root := &Route{Receiver: "r"}
	c := build(t, root, nil, nil, recv("r", "webhook"))
	year := 365 * 24 * hr
	res := run(t, c, []Alert{al(0, 0, "alertname", "A")}, Options{Until: t0.Add(year)})
	// A repeat is due strictly after four hours, and flushes come every five
	// minutes: every 4h5m.
	n := res.Notifications
	if len(n) < 2000 || len(n) > 2200 {
		t.Fatalf("%d notifications in a year", len(n))
	}
	for i := 2; i < len(n); i++ {
		if n[i].At.Sub(n[i-1].At) != 4*hr+5*min {
			t.Fatalf("repeat %d after %v", i, n[i].At.Sub(n[i-1].At))
		}
	}
	again := run(t, c, []Alert{al(0, 0, "alertname", "A")}, Options{Until: t0.Add(year)})
	if !reflect.DeepEqual(res, again) {
		t.Error("two runs differ")
	}
	// Nothing is worked out after Until; an alert that starts after it is
	// routed and nothing else.
	res = run(t, c, []Alert{al(2*hr, 0, "alertname", "A")}, Options{Until: t0.Add(hr)})
	if len(res.Notifications) != 0 || len(res.Alerts[0].Receivers) != 1 {
		t.Errorf("an alert after the period: %+v", res)
	}

	root = quick("r")
	root.GroupByAll = true
	c = build(t, root, nil, nil, recv("r", "webhook"))
	var many []Alert
	for i := range MaxGroups + 5 {
		many = append(many, al(0, 0, "alertname", "A", "i", fmt.Sprint(i)))
	}
	res = run(t, c, many, Options{Until: t0.Add(time.Minute)})
	if res.Stopped == "" || res.Groups != MaxGroups || len(res.Notifications) != MaxGroups {
		t.Errorf("stopped %q, %d groups, %d notifications", res.Stopped, res.Groups, len(res.Notifications))
	}
	if _, err := c.Run([]Alert{{Labels: map[string]string{"a": "b"}}}, Options{}); err == nil {
		t.Error("an alert without a start")
	}
}
