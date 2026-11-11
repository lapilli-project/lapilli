package replay

import (
	"maps"
	"slices"
	"time"

	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/sse"
)

// StateLabel is the label a replayed alert carries while Grafana holds it
// in its NoData state, with the value NoDataState, as Grafana's own series
// of what fired (GRAFANA_ALERTS) has it. Grafana notifies such an alert as
// DatasourceNoData; it is the rule's.
const (
	StateLabel  = "grafana_alertstate"
	NoDataState = "nodata"
)

// A gstate is one of Grafana's alert states, Error aside: a replay can't
// tell when a query failed.
type gstate int

const (
	normal gstate = iota
	pending
	alerting
	recovering
	nodata
)

// firing reports whether Grafana sends the alert as firing in a state:
// Alerting, Recovering (keep_firing_for), and NoData (as DatasourceNoData).
func (s gstate) firing() bool { return s == alerting || s == recovering || s == nodata }

// A result is what one evaluation says of an alert.
type result int

const (
	resNormal result = iota
	resAlerting
	resNoData
)

// maxWarmup is the longest Grafana's alerts are worked out from before a
// period, besides "for" and keep_firing_for: a day, however many
// evaluations missing_series_evals_to_resolve asks to wait.
const maxWarmup = 24 * time.Hour

// grafanaStates holds a Grafana rule's alerts, evaluation by evaluation,
// as Grafana's state manager does (pkg/services/ngalert/state in Grafana
// 13.3): Normal, Pending, Alerting, Recovering and NoData, moving only on
// an evaluation's results, and resolving an alert whose series has gone
// once it is stale, without Recovering. The replay fires an alert at the
// evaluations it is firing at, with no "for" or keep_firing_for of its own:
// those are here.
type grafanaStates struct {
	noData   string
	forDur   time.Duration
	keep     time.Duration
	interval time.Duration
	missing  int
	alerts   map[string]*alert // Grafana's cache, by the labels its results have
}

// An alert is one of a Grafana rule's alert instances.
type alert struct {
	labels   map[string]string
	state    gstate
	startsAt time.Time // when it came into its state
	last     time.Time // last evaluated
	// reason is that the last result it had was NoData, or, with
	// noDataState KeepLast, "NoData, KeepLast", as Grafana's StateReason.
	reason string
}

func newStates(r rules.Rule, every time.Duration) *grafanaStates {
	g := r.Graph
	s := &grafanaStates{noData: g.NoData, forDur: r.For, keep: r.KeepFiringFor, interval: r.Interval, missing: g.MissingToResolve,
		alerts: map[string]*alert{}}
	if s.noData == "" {
		s.noData = rules.StateNoData
	}
	if s.interval <= 0 {
		s.interval = every
	}
	if s.missing <= 0 {
		s.missing = 2
	}
	return s
}

// warmup is how long before a period the alerts are worked out from, to
// find those Grafana would have had at its start: long enough for one gone
// to have resolved and, where no data is put on the alerts there are, for
// a stretch of no data to have had data before it; a day at most; and the
// rule's "for" and keep_firing_for besides.
func (s *grafanaStates) warmup() time.Duration {
	w := maxWarmup
	if n := int64(maxWarmup / s.interval); int64(s.missing) < n {
		w = time.Duration(s.missing+1) * s.interval
	}
	if s.noData != rules.StateNoData {
		w = max(w, time.Hour)
	}
	return min(w, maxWarmup) + s.forDur + s.keep
}

// heldAt is an alert firing at an evaluation, and its labels as replayed.
type heldAt struct {
	key    string
	labels map[string]string
}

// eval takes one evaluation at t: the condition's result, or, when noData
// isn't empty, the labels of Grafana's NoData results. A result of
// nothing at all is NoData too, with no labels. It returns the alerts
// firing after it.
func (s *grafanaStates) eval(t time.Time, cond sse.Set, noData []map[string]string) []heldAt {
	if len(noData) == 0 && len(cond) == 0 {
		noData = []map[string]string{{}}
	}
	seen := map[string]bool{}
	get := func(l map[string]string) *alert {
		if l == nil {
			l = map[string]string{}
		}
		k := key(l)
		a := s.alerts[k]
		if a == nil {
			a = &alert{labels: l, state: normal, startsAt: t}
			s.alerts[k] = a
		}
		seen[k] = true
		return a
	}
	switch {
	case len(noData) == 0:
		for _, it := range cond {
			// Anything but 0 is Alerting: NaN too.
			res := resNormal
			if it.V != 0 {
				res = resAlerting
			}
			s.transition(get(it.Labels), res, t)
		}
	case s.noData != rules.StateNoData && len(s.alerts) > 0:
		// Every alert the rule has takes the NoData result.
		for k, a := range s.alerts {
			seen[k] = true
			s.transition(a, resNoData, t)
		}
	default:
		for _, l := range noData {
			s.transition(get(l), resNoData, t)
		}
	}
	// Alerts missing from the evaluation stay as they were, firing or
	// pending, until stale, and then are Normal; one in the NoData state,
	// or pending for no data, after one evaluation.
	for k, a := range s.alerts {
		if seen[k] {
			continue
		}
		n := s.missing
		if a.state == nodata || a.state == pending && a.reason == rules.StateNoData {
			n = 1
		}
		if t.Sub(a.last) >= time.Duration(n)*s.interval {
			delete(s.alerts, k)
		}
	}
	return s.firing()
}

// firing are the alerts firing now, NoData ones labelled so.
func (s *grafanaStates) firing() []heldAt {
	var out []heldAt
	for _, k := range slices.Sorted(maps.Keys(s.alerts)) {
		a := s.alerts[k]
		if !a.state.firing() {
			continue
		}
		if a.state == nodata {
			l := maps.Clone(a.labels)
			l[StateLabel] = NoDataState
			out = append(out, heldAt{key(l), l})
			continue
		}
		out = append(out, heldAt{k, a.labels})
	}
	return out
}

// transition moves an alert on one result at t, as Grafana's
// State.transition does.
func (s *grafanaStates) transition(a *alert, res result, t time.Time) {
	a.last = t
	switch res {
	case resNormal:
		s.normal(a, t)
	case resAlerting:
		s.alerting(a, t)
	default:
		switch s.noData {
		case rules.StateAlerting:
			s.alerting(a, t)
		case rules.StateOK:
			s.normal(a, t)
		case rules.StateKeepLast:
			switch a.state {
			case alerting:
				s.alerting(a, t)
			case pending:
				if t.Sub(a.startsAt) >= s.forDur {
					s.alerting(a, t)
				}
			default:
				s.normal(a, t)
			}
		default: // NoData
			switch a.state {
			case nodata:
			case pending:
				if t.Sub(a.startsAt) >= s.forDur {
					a.state, a.startsAt = nodata, t
				}
			default:
				if a.state != recovering && s.forDur > 0 {
					a.state, a.startsAt = pending, t
				} else {
					a.state, a.startsAt = nodata, t
				}
			}
		}
	}
	// Grafana says why an alert is in a state other than its result's.
	a.reason = ""
	if res == resNoData && a.state != nodata {
		a.reason = rules.StateNoData
		if s.noData == rules.StateKeepLast {
			a.reason = "NoData, KeepLast"
		}
	}
}

func (s *grafanaStates) normal(a *alert, t time.Time) {
	switch {
	case a.state == normal:
	case a.state == recovering:
		if t.Sub(a.startsAt) >= s.keep {
			a.state, a.startsAt = normal, t
		}
	case a.state == alerting && s.keep > 0:
		a.state, a.startsAt = recovering, t
	default:
		a.state, a.startsAt = normal, t
	}
}

func (s *grafanaStates) alerting(a *alert, t time.Time) {
	switch a.state {
	case alerting:
	case pending:
		if t.Sub(a.startsAt) >= s.forDur {
			a.state, a.startsAt = alerting, t
		}
	default:
		if a.state != recovering && s.forDur > 0 {
			a.state, a.startsAt = pending, t
		} else {
			a.state, a.startsAt = alerting, t
		}
	}
}
