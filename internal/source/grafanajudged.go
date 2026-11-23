package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	// alerts apart, or Grafana lists them no more as they were read
	// (ruleIDs). Shared are those whose alert name, as the history
	// names it, another of Grafana's rules has too, where it doesn't say
	// which of them fired.
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

// sharedNames are those of rs, each by the name it is judged under, whose
// alert name, as the history kept where names it, another of Grafana's
// rules among all has too, of another uid (rs alone, when all is nil).
func sharedNames(where string, rs, all []rules.Rule) map[string]bool {
	of := all
	if len(of) == 0 {
		of = rs
	}
	uids := map[string]map[string]bool{}
	for _, r := range of {
		if r.UID == "" {
			continue
		}
		n := historyName(where, r)
		if uids[n] == nil {
			uids[n] = map[string]bool{}
		}
		uids[n][r.UID] = true
	}
	twice := map[string]bool{}
	for _, r := range rs {
		if len(uids[historyName(where, r)]) > 1 {
			twice[r.Name] = true
		}
	}
	return twice
}

// Judge reads the history kept where over [from, to] and gives the firings
// of rs, the rules to judge by it, each under its Name. A change is taken
// for a rule of rs only where the history says it is that rule's: kept in
// Loki, by the rule's uid each change carries; in annotations, by the
// rule's id in Grafana's database each carries (alertId), as Grafana lists
// the rule of its uid now (ruleIDs). A change of another rule of the name
// is passed over: its rule isn't the one judged, and the name isn't
// shared. A rule Grafana lists no more, or under another title, is unsure:
// what it fired under its title now isn't read.
//
// Where the history doesn't say which rule a change of a rule's name is of
// (with no uid or id), the rule is judged by its name, as the history
// names it; and one whose name another rule has, among all, every rule
// read from the Grafana now, before any --rule (rs alone, when all is
// nil), and ever, every rule of a version in effect across the read, is
// neither's. A name the read holds no change of is its rule's, which fired
// nothing of it, whoever else has it. A read past its bounds fails, as
// ErrTooManyChanges, and one whose answers can't be used, as ErrUnusable:
// none of rs is judged then.
func (g GrafanaHistory) Judge(ctx context.Context, where string, rs, all, ever []rules.Rule, from, to time.Time) (FromHistory, error) {
	ts, read, err := g.read(ctx, where, from, to, true)
	if err != nil {
		return FromHistory{}, err
	}
	j := FromHistory{Passed: read.Passed}
	byName := map[string][]rules.Rule{}
	unsure := map[string]bool{}
	for _, r := range rs {
		if read.UnsureMore {
			unsure[r.Name] = true
			continue
		}
		n := historyName(where, r)
		byName[n] = append(byName[n], r)
	}
	// The changes of each name, and whether each says which rule it is of.
	of := map[string][]int{}
	for i, t := range ts {
		if _, ok := byName[t.Rule]; ok {
			of[t.Rule] = append(of[t.Rule], i)
		}
	}
	says := func(t firing.Transition) bool {
		if where == HistoryLoki {
			return t.Of.UID != ""
		}
		return t.Of.ID != 0
	}
	everShared := sharedNames(where, rs, slices.Concat(all, ever))
	shared := map[string]bool{}
	whose := make([]string, len(ts)) // the name of the rule judged each change is of
	// The ids of the rules judged, read once, when a rule needs its own.
	var ids map[string]int64
	for _, n := range slices.Sorted(maps.Keys(byName)) {
		if len(of[n]) == 0 {
			continue // no change of it: nothing to say whose
		}
		told := !slices.ContainsFunc(of[n], func(i int) bool { return !says(ts[i]) })
		for _, r := range byName[n] {
			var mine func(firing.RuleOf) bool
			switch {
			case !told && everShared[r.Name]:
				shared[r.Name] = true
				continue
			case !told:
				mine = func(firing.RuleOf) bool { return true }
			case where == HistoryLoki:
				mine = func(o firing.RuleOf) bool { return o.UID == r.UID }
			default:
				if ids == nil {
					if ids, err = g.ruleIDs(ctx, rs); err != nil {
						return FromHistory{}, err
					}
				}
				id := ids[r.UID]
				if id == 0 {
					unsure[r.Name] = true
					continue
				}
				mine = func(o firing.RuleOf) bool { return o.ID == id }
			}
			for _, i := range of[n] {
				if whose[i] == "" && mine(ts[i].Of) {
					whose[i] = r.Name
				}
			}
		}
	}
	var keep []firing.Transition
	for i, t := range ts {
		if whose[i] != "" {
			t.Rule = whose[i]
			keep = append(keep, t)
		}
	}
	// A name passed over changes of: each rule of it is unsure.
	for _, n := range read.Unsure {
		for _, r := range byName[n] {
			if !shared[r.Name] {
				unsure[r.Name] = true
			}
		}
	}
	fs, odd := firing.FromTransitions(keep, from)
	for _, n := range odd {
		unsure[n] = true
	}
	for _, f := range fs {
		if !unsure[f.Rule] && !shared[f.Rule] {
			j.Firings = append(j.Firings, f)
		}
	}
	j.Unsure, j.Shared = slices.Sorted(maps.Keys(unsure)), slices.Sorted(maps.Keys(shared))
	return j, nil
}

// ruleIDs are the ids in Grafana's database of the rules of rs, by uid, as
// Grafana's provisioning API lists its rules now
// (/api/v1/provisioning/alert-rules): what an annotation names its rule
// by (alertId). Grafana asks of a token the same to list them as to
// export them, which Lapilli reads its rules from, and the export holds no
// id. A rule it doesn't list, lists twice, or lists under another title
// than rs has (renamed since they were read) has none. A list past
// ListLimit or MaxRules can't be used.
func (g GrafanaHistory) ruleIDs(ctx context.Context, rs []rules.Rule) (map[string]int64, error) {
	const what = "Grafana's alert rules"
	body, err := g.client().Get(ctx, "/api/v1/provisioning/alert-rules", ListLimit)
	if err != nil {
		return nil, unusable(what, err)
	}
	title := make(map[string]string, len(rs))
	for _, r := range rs {
		if r.UID != "" {
			title[r.UID] = r.AlertName()
		}
	}
	type listed struct {
		id    int64
		title string
		n     int
	}
	got := map[string]*listed{}
	d := json.NewDecoder(bytes.NewReader(body))
	if t, err := d.Token(); err != nil || t != json.Delim('[') {
		return nil, fmt.Errorf("%w: reading %s: not a list of rules", ErrUnusable, what)
	}
	for n := 0; d.More(); n++ {
		if n >= MaxRules {
			return nil, fmt.Errorf("%w: reading %s: more than %d rules", ErrUnusable, what, MaxRules)
		}
		var x struct {
			ID    int64  `json:"id"`
			UID   string `json:"uid"`
			Title string `json:"title"`
		}
		if err := d.Decode(&x); err != nil {
			return nil, fmt.Errorf("%w: reading %s: %v", ErrUnusable, what, err)
		}
		if _, ok := title[x.UID]; !ok {
			continue
		}
		l := got[x.UID]
		if l == nil {
			l = &listed{}
			got[x.UID] = l
		}
		l.id, l.title, l.n = x.ID, x.Title, l.n+1
	}
	ids := make(map[string]int64, len(got))
	for uid, l := range got {
		if l.n == 1 && l.id > 0 && l.title == title[uid] {
			ids[uid] = l.id
		}
	}
	return ids, nil
}
