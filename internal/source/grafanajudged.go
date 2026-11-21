package source

import (
	"context"
	"maps"
	"slices"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/rules"
)

// What a Grafana's own alert state history says its rules fired, for the
// rules Lapilli can't replay (rules.Rule.FromHistory,
// docs/design/grafanajudged.md): read as --check-replay reads it, within
// the same bounds, and each firing given the name its rule is judged
// under.

// JudgedLead is how long before a period the history is read from, as
// Grafana keeps changes of state, not states: an alert on since before
// the period, which changed state within a day of its start, is seen with
// its start.
const JudgedLead = 24 * time.Hour

// FromHistory is what the history says the rules judged by it fired.
type FromHistory struct {
	// Firings are the rules' firings over what was read, each under the
	// name its rule is judged under: one on since before it starting at
	// its start, as firing.FromTransitions has it.
	Firings []firing.Firing
	// Unsure are the rules, by name, the history judges none of: it
	// passed over changes of them, doesn't tell their alerts apart, or
	// names another of Grafana's rules alike.
	Unsure []string
	// Passed is how many changes the read passed over, past what is read
	// of one (HistoryRead.Passed).
	Passed int
}

// Judge reads the history kept where over [from, to] and gives the firings
// of rs, the rules to judge by it, each under its Name. An annotation's
// tags name a rule as AnnotationName writes its alert name, Loki's as it
// is; a name the history gives two of Grafana's rules among all, which
// holds rs, is neither's (rs alone, when all is nil). A read past its
// bounds fails, as ErrTooManyChanges, and one whose answers can't be
// used, as ErrUnusable: none of rs is judged then.
func (g GrafanaHistory) Judge(ctx context.Context, where string, rs, all []rules.Rule, from, to time.Time) (FromHistory, error) {
	ts, read, err := g.Read(ctx, where, from, to)
	if err != nil {
		return FromHistory{}, err
	}
	j := FromHistory{Passed: read.Passed}
	key := func(r rules.Rule) string {
		if where == HistoryAnnotations {
			return AnnotationName(r.AlertName())
		}
		return r.AlertName()
	}
	of := all
	if len(of) == 0 {
		of = rs
	}
	shared := map[string]int{}
	for _, r := range of {
		if r.UID != "" {
			shared[key(r)]++
		}
	}
	byKey := map[string]string{}
	unsure := map[string]bool{}
	for _, r := range rs {
		k := key(r)
		if read.UnsureMore || shared[k] > 1 {
			unsure[r.Name] = true
			continue
		}
		byKey[k] = r.Name
	}
	fs, odd := firing.FromTransitions(ts, from)
	for _, n := range slices.Concat(read.Unsure, odd) {
		if name, ok := byKey[n]; ok {
			unsure[name] = true
			delete(byKey, n)
		}
	}
	for _, f := range fs {
		if name, ok := byKey[f.Rule]; ok {
			f.Rule = name
			j.Firings = append(j.Firings, f)
		}
	}
	j.Unsure = slices.Sorted(maps.Keys(unsure))
	return j, nil
}
