package routing

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// t0 is a Monday at ten in the morning, UTC.
var t0 = time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)

func mt(t testing.TB, name, op, value string) Matcher {
	t.Helper()
	m, err := NewMatcher(name, op, value)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func dur(d time.Duration) *time.Duration { return &d }

// recv is a receiver with an integration of each kind, sending resolves as
// the kind's default does.
func recv(name string, kinds ...string) Receiver {
	r := Receiver{Name: name}
	count := map[string]int{}
	for _, k := range kinds {
		r.Integrations = append(r.Integrations, Integration{Kind: k, Index: count[k], SendResolved: DefaultSendResolved(k)})
		count[k]++
	}
	return r
}

func build(t testing.TB, root *Route, intervals map[string][]TimeInterval, inhibit []InhibitRule, receivers ...Receiver) *Config {
	t.Helper()
	c, err := NewConfig(root, receivers, intervals, inhibit, false)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// quick is a route whose timings are short and exact: no wait, a minute
// between flushes, five minutes before a repeat.
func quick(receiver string, children ...*Route) *Route {
	return &Route{Receiver: receiver, GroupWait: dur(10 * time.Second), GroupInterval: dur(time.Minute), RepeatInterval: dur(5 * time.Minute), Routes: children}
}

func al(start time.Duration, end time.Duration, kv ...string) Alert {
	a := Alert{Labels: map[string]string{}, Start: t0.Add(start)}
	if end != 0 {
		a.End = t0.Add(end)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		a.Labels[kv[i]] = kv[i+1]
	}
	return a
}

func run(t testing.TB, c *Config, alerts []Alert, o Options) *Result {
	t.Helper()
	if o.Until.IsZero() {
		o.Until = t0.Add(48 * time.Hour)
	}
	r, err := c.Run(alerts, o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// seq writes a result's notifications on one receiver and kind as
// "offset reason", offset from t0.
func seq(r *Result, receiver, kind string) []string {
	var out []string
	for _, n := range r.Notifications {
		if n.Receiver == receiver && (kind == "" || n.Kind == kind) {
			out = append(out, n.At.Sub(t0).String()+" "+string(n.Reason))
		}
	}
	return out
}

func eq(t testing.TB, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got %s\nwant %s", what, strings.Join(got, ", "), strings.Join(want, ", "))
	}
}
