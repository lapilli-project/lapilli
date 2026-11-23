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
	// Unsure are the rules, by name, the history judges none of over
	// what was read: it passed over changes of them, or doesn't tell their
	// alerts apart. Shared are those whose alert name, as the history
	// names it, another of Grafana's rules has too: it never says which of
	// them fired.
	Unsure, Shared []string
	// Passed is how many changes the read passed over, past what is read
	// of one (HistoryRead.Passed).
	Passed int
}

// historyName is the name the history kept where gives a rule's alerts:
// an annotation's tags as AnnotationName writes its alert name, Loki's as
// it is.
func historyName(where string, r rules.Rule) string {
	if where == HistoryAnnotations {
		return AnnotationName(r.AlertName())
	}
	return r.AlertName()
}

// SharedNames are those of rs, each by the name it is judged under, whose
// alert name, as the history kept where names it, another of Grafana's
// rules among all has too (rs alone, when all is nil): known before the
// history is read, as Judge gives them in Shared.
func SharedNames(where string, rs, all []rules.Rule) map[string]bool {
	of := all
	if len(of) == 0 {
		of = rs
	}
	n := map[string]int{}
	for _, r := range of {
		if r.UID != "" {
			n[historyName(where, r)]++
		}
	}
	twice := map[string]bool{}
	for _, r := range rs {
		if n[historyName(where, r)] > 1 {
			twice[r.Name] = true
		}
	}
	return twice
}

// Judge reads the history kept where over [from, to] and gives the firings
// of rs, the rules to judge by it, each under its Name. An annotation's
// tags name a rule as AnnotationName writes its alert name, Loki's as it
// is; a name the history gives two of Grafana's rules among all, every
// rule read from the Grafana, before any --rule, is neither's (rs alone,
// when all is nil). A read past its
// bounds fails, as ErrTooManyChanges, and one whose answers can't be
// used, as ErrUnusable: none of rs is judged then.
func (g GrafanaHistory) Judge(ctx context.Context, where string, rs, all []rules.Rule, from, to time.Time) (FromHistory, error) {
	ts, read, err := g.Read(ctx, where, from, to)
	if err != nil {
		return FromHistory{}, err
	}
	j := FromHistory{Passed: read.Passed}
	twice := SharedNames(where, rs, all)
	byKey := map[string]string{}
	unsure := map[string]bool{}
	for _, r := range rs {
		switch {
		case twice[r.Name]:
		case read.UnsureMore:
			unsure[r.Name] = true
		default:
			byKey[historyName(where, r)] = r.Name
		}
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
	j.Unsure, j.Shared = slices.Sorted(maps.Keys(unsure)), slices.Sorted(maps.Keys(twice))
	return j, nil
}
