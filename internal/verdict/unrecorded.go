package verdict

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// A rule that fired as often as a noisy one in a period with no record
// (Report.NoRecord) can't be called noisy: its firings caught nothing
// written down, which isn't nothing. What can be said without a record is
// what they were: whether the value its condition compares went well past
// its bound, or flickered at its edge, and which alerts fired most. Those
// that went well past it happened; whether they mattered is for a person to
// write down. A longer "for" would only hide them.

// Unrecorded is such a rule, as lapilli verdict, replaying it, and lapilli
// serve's weekly digest work it out.
type Unrecorded struct {
	Rule    string `json:"rule"`
	Firings int    `json:"firings"`
	// Clear is how many of its firings had the value its condition
	// compares go past its bound by more than rules.NearBy of it, for as
	// long as its "for": not a flicker at its edge. Nil when that can't be
	// told: its condition has no one bound that decides it (a range, and,
	// or, unless), its bound is nothing, or it couldn't be replayed with
	// the bound further. Bound is the bound, as compared: "> 0.5".
	Clear *int   `json:"clear,omitempty"`
	Bound string `json:"bound,omitempty"`
	// Alerts is how many alerts (label sets) fired; Top, those that fired
	// most, TopShown at most, each by the labels that tell them apart.
	Alerts int          `json:"alerts"`
	Top    []AlertCount `json:"top"`
	// DuringDeploys is, when deploys are known (lapilli serve), how many
	// came during a deploy of what they are about.
	DuringDeploys *int `json:"during_deploys,omitempty"`
	// Team is, in lapilli serve, the team that owns the rule.
	Team string `json:"team,omitempty"`
}

// Happened says the firings went well past the bound, as far as is known.
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

// TopShown is how many of a rule's alerts are named; topLabels, how many
// of the labels that tell them apart each is named by; topValue, how much
// of a label's value.
const (
	TopShown  = 5
	topLabels = 4
	topValue  = 80
)

// TopAlerts are, of a rule's alerts, each with all its labels, how many
// there are and those that fired most, TopShown at most, the most first,
// each by the labels whose values aren't the same in all of them: the
// labels that tell them apart. One alert alone is named by its own, but
// alertname and severity.
func TopAlerts(all []AlertCount) (int, []AlertCount) {
	all = slices.Clone(all)
	slices.SortStableFunc(all, func(a, b AlertCount) int {
		if a.Firings != b.Firings {
			return b.Firings - a.Firings
		}
		return strings.Compare(a.LabelsText(), b.LabelsText())
	})
	keys := map[string]bool{}
	for _, a := range all {
		for k := range a.Labels {
			keys[k] = true
		}
	}
	var apart []string
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		v0, ok0 := all[0].Labels[k]
		for _, a := range all[1:] {
			if v, ok := a.Labels[k]; v != v0 || ok != ok0 {
				apart = append(apart, k)
				break
			}
		}
	}
	if len(all) == 1 {
		for _, k := range slices.Sorted(maps.Keys(all[0].Labels)) {
			if k != "alertname" && k != "severity" {
				apart = append(apart, k)
			}
		}
	}
	apart = apart[:min(len(apart), topLabels)]
	var top []AlertCount
	for _, a := range all[:min(len(all), TopShown)] {
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
	return len(all), top
}

// Text says, in English, what is known of the rule's firings, and what to
// do with them.
func (u Unrecorded) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fired %d times with no incident written down to judge them by.", u.Firings)
	if u.Clear != nil {
		switch c := *u.Clear; {
		case c == u.Firings:
			fmt.Fprintf(&b, " All %d went well past its bound (%s), not a flicker at its edge: these happened.", c, Clean(u.Bound))
		case c > 0:
			fmt.Fprintf(&b, " %d of them went well past its bound (%s), not a flicker at its edge: these happened.", c, Clean(u.Bound))
		default:
			fmt.Fprintf(&b, " None went well past its bound (%s): each came within a fifth of it.", Clean(u.Bound))
		}
	}
	if u.DuringDeploys != nil && *u.DuringDeploys > 0 {
		fmt.Fprintf(&b, " %d came during a deploy.", *u.DuringDeploys)
	}
	switch {
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
