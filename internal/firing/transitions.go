package firing

import (
	"maps"
	"slices"
	"strings"
	"time"
)

// Transition is one alert's change of state, as Grafana keeps them in its
// alert state history (docs/design/grafanahistory.md): its rule's alert
// name, its labels but alertname, when it changed, and the states before
// and after, as Grafana writes them: Normal, Pending, Alerting, Recovering,
// NoData or Error, with a reason after or not ("Normal (MissingSeries)").
// Key, when set, tells its alert apart, the same for every change of it;
// otherwise its rule and labels do. The changes of one alert may share
// one Labels, which isn't written to.
//
// Of, where the history was read with its alerts told apart by the rule
// that fired them, is what it says of that rule beside its alert name.
type Transition struct {
	Rule     string
	Labels   map[string]string
	At       time.Time
	From, To string
	Key      string
	Of       RuleOf
}

// RuleOf is what Grafana's alert state history says of the rule a change
// is of, beside the alert name its labels carry: kept in Loki, the rule's
// uid; in annotations, the rule's id in Grafana's database (no uid). Zero
// where the history says neither, or wasn't read for them.
type RuleOf struct {
	UID string
	ID  int64
}

// The label a firing in Grafana's NoData state carries, as its series of
// what fired (GRAFANA_ALERTS) has it, and the replay gives it: a firing of
// its own, apart from the alert's while alerting.
const (
	stateLabel  = "grafana_alertstate"
	noDataState = "nodata"
)

// InNoData reports whether a firing's labels say it is one of Grafana's
// NoData state.
func InNoData(labels map[string]string) bool { return labels[stateLabel] == noDataState }

// grafanaFiring is what a state of Grafana's sends: "" for none, "firing"
// for Alerting and Recovering (keep_firing_for), which its notifications
// take as one alert, and "nodata" for NoData, sent as DatasourceNoData. An
// Error isn't among them, as GRAFANA_ALERTS{grafana_alertstate!="error"}
// leaves it out: a replay can't tell when a query failed. A reason after
// the state ("Alerting (NoData)") doesn't change what it sends.
func grafanaFiring(state string) string {
	switch GrafanaBase(state) {
	case "Alerting", "Recovering":
		return "firing"
	case "NoData":
		return noDataState
	}
	return ""
}

// GrafanaBase is a state as Grafana writes it without its reason:
// "Normal" of "Normal (MissingSeries)".
func GrafanaBase(state string) string {
	base, _, _ := strings.Cut(state, " (")
	return strings.TrimSpace(base)
}

// GrafanaState reports whether state is one of Grafana's alert states, a
// reason after it or not.
func GrafanaState(state string) bool {
	switch GrafanaBase(state) {
	case "Normal", "Pending", "Alerting", "Recovering", "NoData", "Error":
		return true
	}
	return false
}

// FromTransitions works out when alerts fired from their changes of
// state, read from from on, as FromAlerts does from GRAFANA_ALERTS: a firing
// from a change into Alerting, Recovering or NoData until a change out of
// them, Alerting and Recovering one firing, NoData one of its own with
// grafana_alertstate="nodata". One whose first change seen is out of a
// firing state was firing at from; one with no change after its last into
// a firing state is still firing, and has no End. An alert that fired all
// through what was read changed state in none of it: it isn't seen.
//
// unsure are the rules an alert of which changed from a state it wasn't
// in, as the change before left it: a firing or not where it wasn't. Grafana
// writes every change of an alert, from the state the one before left
// (those it leaves out, Normal to Normal (NoData) and into KeepLast, don't
// change whether it fires): changes missing, or two alerts the history
// doesn't tell apart, would make firings that weren't. The firings of an
// alert share their labels, which aren't written to.
func FromTransitions(ts []Transition, from time.Time) (fs []Firing, unsure []string) {
	byAlert := map[string][]Transition{}
	var keys []string
	for _, t := range ts {
		k := t.Key
		if k == "" {
			k = "\x00" + t.Rule + "\x00" + labelKey(t.Labels)
		}
		if _, ok := byAlert[k]; !ok {
			keys = append(keys, k)
		}
		byAlert[k] = append(byAlert[k], t)
	}
	slices.Sort(keys)
	odd := map[string]bool{}
	for _, k := range keys {
		seq := byAlert[k]
		slices.SortStableFunc(seq, func(a, b Transition) int { return a.At.Compare(b.At) })
		base, nodata := seq[0].Labels, map[string]string(nil)
		if base == nil {
			base = map[string]string{}
		}
		var open *Firing
		kind := ""
		start := func(as string, at time.Time) {
			l := base
			if as == noDataState {
				if nodata == nil {
					nodata = maps.Clone(base)
					nodata[stateLabel] = noDataState
				}
				l = nodata
			}
			kind = as
			open = &Firing{Rule: seq[0].Rule, Labels: l, Start: at}
		}
		stop := func(at time.Time) {
			// One that changed at from was over by then.
			if open != nil && at.After(open.Start) {
				open.End = at
				fs = append(fs, *open)
			}
			open, kind = nil, ""
		}
		if was := grafanaFiring(seq[0].From); was != "" {
			start(was, from)
		}
		for i, t := range seq {
			if i > 0 && grafanaFiring(t.From) != kind {
				odd[t.Rule] = true
			}
			is := grafanaFiring(t.To)
			if is == kind {
				continue
			}
			stop(t.At)
			if is != "" {
				start(is, t.At)
			}
		}
		if open != nil {
			fs = append(fs, *open)
		}
	}
	Sort(fs)
	return fs, slices.Sorted(maps.Keys(odd))
}
