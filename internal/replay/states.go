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

// grafanaStates holds a Grafana rule's alerts, evaluation by evaluation,
// as Grafana's state manager does (pkg/services/ngalert/state in Grafana
// 13): which of them are held, pending or firing, at each. "for" and
// keep_firing_for are left to firing.FromSeries, as for any rule.
type grafanaStates struct {
	noData   string
	forDur   time.Duration
	interval time.Duration
	missing  int
	alerts   map[string]*alert // Grafana's cache, by the labels its results have
}

// An alert is one of a Grafana rule's alert instances.
type alert struct {
	labels map[string]string
	held   bool      // pending or firing
	since  time.Time // held since
	last   time.Time // last evaluated
	// noData is that the last result it had was NoData; nodata, that it is
	// held for one in noDataState NoData.
	noData, nodata bool
}

func newStates(r rules.Rule, every time.Duration) *grafanaStates {
	g := r.Graph
	s := &grafanaStates{noData: g.NoData, forDur: r.For, interval: r.Interval, missing: g.MissingToResolve, alerts: map[string]*alert{}}
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
// a stretch of no data to have had data before it.
func (s *grafanaStates) warmup() time.Duration {
	w := time.Duration(s.missing+1) * s.interval
	if s.noData != rules.StateNoData {
		w = max(w, time.Hour)
	}
	return w
}

// heldAt is an alert held at an evaluation, and its labels as replayed.
type heldAt struct {
	key    string
	labels map[string]string
}

// eval takes one evaluation at t: the condition's result, or, when noData
// isn't empty, the labels of Grafana's NoData results. A result of
// nothing at all is NoData too, with no labels. It returns the alerts held
// after it.
func (s *grafanaStates) eval(t time.Time, cond sse.Set, noData []map[string]string) []heldAt {
	if len(noData) == 0 && len(cond) == 0 {
		noData = []map[string]string{{}}
	}
	seen := map[string]bool{}
	set := func(l map[string]string, held, noData, nodata bool) {
		if l == nil {
			l = map[string]string{}
		}
		k := key(l)
		a := s.alerts[k]
		if a == nil {
			a = &alert{labels: l}
			s.alerts[k] = a
		}
		if held && (!a.held || a.nodata != nodata) {
			a.since = t
		}
		a.held, a.last, a.noData, a.nodata = held, t, noData, nodata
		seen[k] = true
	}
	switch {
	case len(noData) == 0:
		for _, it := range cond {
			// Anything but 0 is Alerting: NaN too.
			set(it.Labels, it.V != 0, false, false)
		}
	case s.noData == rules.StateNoData:
		// The NoData results are alerts of their own, held in the NoData
		// state; the rule's other alerts are missing.
		for _, l := range noData {
			set(l, true, true, true)
		}
	case len(s.alerts) > 0:
		// Every alert the rule has takes the NoData result: Alerting holds
		// them, OK lets them go, KeepLast keeps each as it was.
		for _, a := range s.alerts {
			held := a.held
			switch s.noData {
			case rules.StateAlerting:
				held = true
			case rules.StateOK:
				held = false
			}
			set(a.labels, held, true, false)
		}
	default:
		// With no alerts to take it, the NoData results become alerts: held
		// for Alerting; for OK and KeepLast, not.
		for _, l := range noData {
			set(l, s.noData == rules.StateAlerting, true, false)
		}
	}
	// Alerts missing from the evaluation stay as they were until they have
	// been missing long enough, then resolve; one in the NoData state, or
	// pending for no data, after one evaluation.
	for k, a := range s.alerts {
		if seen[k] {
			continue
		}
		n := s.missing
		if a.noData && (a.nodata || s.noData == rules.StateAlerting && a.held && a.last.Sub(a.since) < s.forDur) {
			n = 1
		}
		if t.Sub(a.last) >= time.Duration(n)*s.interval {
			delete(s.alerts, k)
		}
	}
	var out []heldAt
	for _, k := range slices.Sorted(maps.Keys(s.alerts)) {
		a := s.alerts[k]
		if !a.held {
			continue
		}
		if a.nodata {
			l := maps.Clone(a.labels)
			l[StateLabel] = NoDataState
			out = append(out, heldAt{key(l), l})
			continue
		}
		out = append(out, heldAt{k, a.labels})
	}
	return out
}
