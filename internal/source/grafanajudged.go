package source

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
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

// maxLookups is how many rules a read of the annotations asks the id of
// (ruleID), at most: one past it is judged as shared.
const maxLookups = 100

// Judge reads the history kept where over [from, to] and gives the firings
// of rs, the rules to judge by it, each under its Name. A change is taken
// for a rule of rs only where the history says it is that rule's: kept in
// Loki, by the rule's uid each change carries; in annotations, by the
// rule's id in Grafana's database each carries (alertId): the rule's
// where every change of its name is of one id, its title as each carries
// it now (alertName) the rule's, and no other rule among all, every rule
// read from the Grafana now, before any --rule, has its title (rs alone,
// when all is nil); else as the newest annotation of its uid says
// (ruleID). A change of another rule of the name is passed over: its rule
// isn't the one judged, and the name isn't shared.
//
// Where the history doesn't say which rule a change of a rule's name is of
// (with no uid or id), the rule is judged by its name, as the history
// names it; and one whose name another rule has, among all and ever, every
// rule of a version in effect across the read, is neither's. A name the
// read holds no change of is its rule's, which fired nothing of it,
// whoever else has it. A read past its bounds fails, as ErrTooManyChanges,
// and one whose answers can't be used, as ErrUnusable: none of rs is
// judged then.
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
	nowShared := sharedNames(where, rs, all)
	everShared := sharedNames(where, rs, slices.Concat(all, ever))
	shared := map[string]bool{}
	whose := make([]string, len(ts)) // the name of the rule judged each change is of
	lookups := 0
	for n, named := range byName {
		if len(of[n]) == 0 {
			continue // no change of it: nothing to say whose
		}
		told := !slices.ContainsFunc(of[n], func(i int) bool { return !says(ts[i]) })
		for _, r := range named {
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
				id, got, err := g.idOf(ctx, r, of[n], ts, nowShared[r.Name], &lookups, to)
				switch {
				case err != nil:
					return FromHistory{}, err
				case got == idPast:
					shared[r.Name] = true
					continue
				case got == idOther:
					unsure[r.Name] = true
					continue
				case got == idNone:
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

// What idOf found of a rule's id.
const (
	idFound = iota
	// idNone: none of its changes is of it.
	idNone
	// idPast: past maxLookups, it wasn't asked.
	idPast
	// idOther: asked, the answer is of a rule of another title now, and
	// what it says can't be taken.
	idOther
)

// idOf is the id in Grafana's database of r, judged from annotations,
// its changes of its name those of at (in ts): where every one of them is
// of one id, and of r's title now, and no other rule has that title
// (shared is false), that id; else as the newest annotation of r's uid
// says (ruleID), lookups counting the rules asked of, maxLookups at most.
func (g GrafanaHistory) idOf(ctx context.Context, r rules.Rule, at []int, ts []firing.Transition, shared bool, lookups *int, to time.Time) (int64, int, error) {
	if len(at) == 0 {
		return 0, idNone, nil
	}
	ids := map[int64]bool{}
	titled := true
	for _, i := range at {
		ids[ts[i].Of.ID] = true
		titled = titled && ts[i].Of.Title == r.AlertName()
	}
	if titled && len(ids) == 1 && !shared {
		for id := range ids {
			return id, idFound, nil
		}
	}
	if *lookups >= maxLookups {
		return 0, idPast, nil
	}
	*lookups++
	id, title, ok, err := g.ruleID(ctx, r.UID, to)
	switch {
	case err != nil:
		return 0, idNone, err
	case !ok:
		return 0, idNone, nil
	case title != r.AlertName():
		return 0, idOther, nil
	}
	return id, idFound, nil
}

// ruleID is the id in Grafana's database of the rule of uid, and its
// title now, as its newest alert annotation up to to says: alertUID keeps
// one rule's. ok is false for a rule with none.
func (g GrafanaHistory) ruleID(ctx context.Context, uid string, to time.Time) (id int64, title string, ok bool, err error) {
	const what = "Grafana's alert annotations"
	q := url.Values{"type": {"alert"}, "alertUID": {uid}, "from": {"1"}, "to": {strconv.FormatInt(to.UnixMilli(), 10)}, "limit": {"1"}}
	body, err := g.client().Get(ctx, "/api/annotations?"+q.Encode(), historyBody)
	if err != nil {
		return 0, "", false, unusable(what, err)
	}
	var items []annotation
	if err := json.Unmarshal(body, &items); err != nil {
		return 0, "", false, fmt.Errorf("%w: reading %s: %v", ErrUnusable, what, err)
	}
	if len(items) == 0 {
		return 0, "", false, nil
	}
	return items[0].AlertID, items[0].AlertName, true, nil
}
