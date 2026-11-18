package replay

import (
	"time"

	"lapilli.dev/internal/rules"
)

// Planned is what Run would ask the histories for a period, counted
// without asking: each request of the reads rules alike share, once, as
// Run asks it (TestPlanIsRun holds the two to each other).
type Planned struct {
	// By are the requests by the history asked: "" the one Run is given
	// (--prometheus), "loki" a Loki ruler's rules' Loki, and grafana/UID
	// the Loki data source a Grafana rule reads, by its uid.
	By map[string]int
	// Requests are By's, in all.
	Requests int
	// Unsure are the Grafana rules replayed, which may ask more than these
	// as what is answered needs: whether a query that returned nothing
	// returned anything in the day before (once a query a replay), the
	// first numbers of a classic condition's window, and a LogQL query a
	// reduce reads, which is asked a batch of evaluations at a time.
	Unsure int
}

// Plan counts what Run would ask h for to replay versions over [from,
// to] at step, asking it nothing: h says only which rules have a history
// to be asked (a rule in LogQL needs a Loki, WithLogs). A rule whose
// replay Run would fail before asking, as Unsupported, asks nothing.
func Plan(h History, versions []Version, from, to time.Time, step time.Duration) Planned {
	return PlanEach(h, [][]Version{versions}, from, to, step)
}

// PlanEach counts what RunEach would ask h for to replay each set of
// versions over [from, to] at step, the sets sharing their requests as
// RunEach shares them: a pull request's backtest counted before it is
// queued, its rules before and after it set side by side.
func PlanEach(h History, sets [][]Version, from, to time.Time, step time.Duration) Planned {
	p := Planned{By: map[string]int{}}
	seen := map[request]bool{}
	names := map[string]int{"": 0}
	var runs [][]stint
	for _, versions := range sets {
		runs = append(runs, lineUp(versions, from, to, step)...)
	}
	for _, run := range runs {
		last := run[len(run)-1].Rule
		if _, err := historyFor(h, last); err != nil {
			continue
		}
		name := ""
		if last.Logs != "" {
			name = rules.LokiRuler
			if last.Graph != nil {
				name = "grafana/" + last.Logs
			}
		}
		src, ok := names[name]
		if !ok {
			src = len(names)
			names[name] = src
		}
		asks := false
		for j, st := range run {
			if st.Unsupported != "" {
				break
			}
			lo, hi := st.span(step)
			for _, r := range queries(st.Rule, lo, hi, step, st.warm(j)) {
				asks = true
				r.src = src
				if seen[r] {
					continue
				}
				seen[r] = true
				p.By[name]++
				p.Requests++
			}
		}
		if asks && last.Graph != nil {
			p.Unsure++
		}
	}
	return p
}
