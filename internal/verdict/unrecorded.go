package verdict

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// A rule that fired as often as a noisy one with no record (Record) can't be
// called noisy: its firings caught nothing written down, which isn't
// nothing. What can be said without a record is what they were. Of a rule
// whose condition counts events (rules.Rule.EventWindow), each firing is an
// event it counted, a count that went up: they happened. Of another with one
// bound that decides it, those whose value went well past the bound
// happened, and those that stayed within a fifth of it may have flickered at
// its edge. And which alerts fired most. Whether they mattered is for a
// person to write down. A longer "for" would only hide them.

// Unrecorded is such a rule, as lapilli verdict, replaying it, and lapilli
// serve's weekly digest work it out.
type Unrecorded struct {
	Rule string `json:"rule"`
	// Firings are its firings with no record (RuleResult.Unjudged).
	Firings int `json:"firings"`
	// Events says its condition counts events: each firing is one, and
	// Clear is all of them. Not said of a Grafana rule with a firing on no
	// data among them, which is none.
	Events bool `json:"events,omitempty"`
	// Clear is how many of its firings happened: of a condition that counts
	// events, all; of another, those whose value went past its bound by
	// more than rules.NearBy of it, for all of its "for". Nil when that
	// can't be told: its condition has no one bound that decides it (a
	// range, and, or, unless), its bound is nothing, it couldn't be
	// replayed with the bound further, or it fired more than is read. Bound
	// is the bound, as compared: "> 0.5".
	Clear *int   `json:"clear,omitempty"`
	Bound string `json:"bound,omitempty"`
	// TooMany says Clear is nil for there were more firings than are read
	// to tell.
	TooMany bool `json:"too_many,omitempty"`
	// Alerts is how many alerts (label sets) fired; Top, those that fired
	// most, TopShown at most, each by the labels that tell them apart.
	Alerts int          `json:"alerts"`
	Top    []AlertCount `json:"top"`
	// DuringDeploys is, when deploys are known (lapilli serve), how many
	// came during a deploy of what they are about.
	DuringDeploys *int `json:"during_deploys,omitempty"`
	// Team is, in lapilli serve, the team that owns the rule; Scope, the
	// team whose record it had none of, "" for the ledger's: what is said in
	// that team's views, and not in others'.
	Team  string `json:"team,omitempty"`
	Scope string `json:"scope,omitempty"`
}

// Happened says its firings happened, as far as is known.
func (u Unrecorded) Happened() bool { return u.Clear != nil && *u.Clear > 0 }

// AlertCount is an alert, by its labels, and how many times it fired. Team
// is, in lapilli serve, the team whose labels it has.
type AlertCount struct {
	Labels  map[string]string `json:"labels"`
	Firings int               `json:"firings"`
	Team    string            `json:"team,omitempty"`
}

// LabelsText is an alert's labels as k=v, in order.
func (a AlertCount) LabelsText() string {
	var ls []string
	for _, k := range slices.Sorted(maps.Keys(a.Labels)) {
		ls = append(ls, k+"="+a.Labels[k])
	}
	return strings.Join(ls, " ")
}

// TopShown is how many of a rule's alerts are named; TopRead, how many of
// those that fired most are read to name them by; topLabels, how many of
// the labels that tell them apart each is named by; topValue, how much of a
// label's value.
const (
	TopShown  = 5
	TopRead   = 1000
	topLabels = 4
	topValue  = 80
)

// TopAlerts are, of a rule's alerts, each with all its labels, those that
// fired most, TopShown at most, the most first, each by the labels whose
// values aren't the same in all of them: the labels that tell them apart.
// One alert alone is named by its own, but alertname and severity. It reads
// TopRead of them at most, those that fired most, in one pass.
func TopAlerts(all []AlertCount) []AlertCount {
	if len(all) == 0 {
		return []AlertCount{}
	}
	type alert struct {
		AlertCount
		text string
	}
	as := make([]alert, len(all))
	for i, a := range all {
		as[i] = alert{a, ""}
	}
	slices.SortStableFunc(as, func(a, b alert) int { return b.Firings - a.Firings })
	as = as[:min(len(as), TopRead)]
	for i := range as {
		as[i].text = as[i].LabelsText()
	}
	slices.SortStableFunc(as, func(a, b alert) int {
		if a.Firings != b.Firings {
			return b.Firings - a.Firings
		}
		return strings.Compare(a.text, b.text)
	})
	// Each label: in how many, its first value, and whether another
	// differs.
	type seen struct {
		n       int
		first   string
		differs bool
	}
	keys := map[string]*seen{}
	for _, a := range as {
		for k, v := range a.Labels {
			if x := keys[k]; x == nil {
				keys[k] = &seen{n: 1, first: v}
			} else {
				x.n++
				x.differs = x.differs || v != x.first
			}
		}
	}
	var apart []string
	for k, x := range keys {
		if x.differs || x.n != len(as) || len(as) == 1 && k != "alertname" && k != "severity" {
			apart = append(apart, k)
		}
	}
	slices.Sort(apart)
	apart = apart[:min(len(apart), topLabels)]
	top := make([]AlertCount, 0, TopShown)
	for _, a := range as[:min(len(as), TopShown)] {
		x := AlertCount{Labels: map[string]string{}, Firings: a.Firings, Team: a.Team}
		for _, k := range apart {
			if v, ok := a.Labels[k]; ok {
				if r := []rune(v); len(r) > topValue {
					v = string(r[:topValue-1]) + "…"
				}
				x.Labels[k] = v
			}
		}
		top = append(top, x)
	}
	return top
}

// Text says, in English, what is known of the rule's firings, and what to
// do with them.
func (u Unrecorded) Text() string { return u.text(true) }

// Prose is Text without the alerts' labels, which are their sources' words:
// for where they are given apart (MCP's top).
func (u Unrecorded) Prose() string { return u.text(false) }

func (u Unrecorded) text(labels bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fired %d times with no incident written down to judge them by.", u.Firings)
	switch {
	case u.Events:
		fmt.Fprintf(&b, " All %d are events its condition counts (a count that went up): these happened.", u.Firings)
	case u.Clear == nil && u.TooMany:
		b.WriteString(" Too many firings to tell how many happened.")
	case u.Clear == nil:
	case *u.Clear == u.Firings:
		fmt.Fprintf(&b, " All %d went well past its bound (%s), not a flicker at its edge: these happened.", *u.Clear, Clean(u.Bound))
	case *u.Clear > 0:
		fmt.Fprintf(&b, " %d of them went well past its bound (%s), not a flicker at its edge: these happened.", *u.Clear, Clean(u.Bound))
	default:
		fmt.Fprintf(&b, " None stayed more than a fifth past its bound (%s) for all of its for.", Clean(u.Bound))
	}
	if u.DuringDeploys != nil && *u.DuringDeploys > 0 {
		fmt.Fprintf(&b, " %d came during a deploy.", *u.DuringDeploys)
	}
	switch {
	case !labels && len(u.Top) > 0:
		fmt.Fprintf(&b, " They came from %d alerts.", u.Alerts)
	case u.Alerts == 1 && len(u.Top) == 1:
		fmt.Fprintf(&b, " All came from one alert: %s.", alertText(u.Top[0], false))
	case len(u.Top) > 0:
		var parts []string
		for _, a := range u.Top {
			parts = append(parts, alertText(a, true))
		}
		fmt.Fprintf(&b, " Most came from: %s", strings.Join(parts, ", "))
		if u.Alerts > len(u.Top) {
			fmt.Fprintf(&b, ", of %d alerts", u.Alerts)
		}
		b.WriteString(".")
	}
	if u.Happened() {
		b.WriteString(` A longer "for" would hide them: write down whether they mattered, or fix what fires them.`)
	} else {
		b.WriteString(" Write down whether they mattered, or fix what fires them.")
	}
	return b.String()
}

// alertText is an alert as Text names it: its labels, how many times it
// fired, and the team whose labels it has.
func alertText(a AlertCount, count bool) string {
	s := Clean(a.LabelsText())
	if s == "" {
		s = "no labels of its own"
	}
	if count {
		s += fmt.Sprintf(" %d", a.Firings)
	}
	if a.Team != "" {
		s += " (" + Clean(a.Team) + ")"
	}
	return s
}
