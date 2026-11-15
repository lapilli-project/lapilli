package source

import (
	"context"
	"errors"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// How much Unrecorded looks at: the rules, the most firings with no record
// first, each replayed once more over the period when its condition doesn't
// count events; and of each, the firings read to tell how many went well
// past its bound, past which that isn't told.
const (
	unrecordedMost  = 10
	unrecordedFired = 5000
)

// UnrecordedFrom is where Unrecorded reads a rule's firings with no record
// in the period.
type UnrecordedFrom interface {
	// Firings are a rule's firings with no record, most of them at most;
	// more says there were more.
	Firings(ctx context.Context, rule string, most int) (fs []firing.Firing, more bool, err error)
	// Alerts are its alerts with no record that fired most, most of them at
	// most, each with all its labels, how many times, and the team whose
	// labels it has, when that is known; all is how many there were.
	Alerts(ctx context.Context, rule string, most int) (top []verdict.AlertCount, all int, err error)
}

// Unrecorded works out, for each rule of rep that fired rep.NoisyAt.Firings
// times or more with no record (verdict.RuleResult.Unrecorded), and was
// replayed, what those firings were (verdict.Unrecorded): the most first,
// unrecordedMost at most.
//
// Of a rule whose condition counts events, in every version it is in
// (rules.Rule.EventWindow), each firing is an event the condition counted:
// all happened. A Grafana rule's firing on no data (noData) is none: with
// one among them, or more than are read to tell, that isn't said. Of
// another with one bound that decides it (deciding), it is replayed with
// the bound rules.NearBy further, and each of its firings one of those
// overlaps, alike in its labels, went well past it; one on no data didn't.
// A replay that fails, or runs out of time, leaves that unknown, and err
// says why; the rest is kept.
func Unrecorded(ctx context.Context, h replay.History, versions []replay.Version, rep *verdict.Report, from UnrecordedFrom,
	step time.Duration) ([]verdict.Unrecorded, error) {
	var rrs []verdict.RuleResult
	for _, rr := range rep.Rules {
		if rr.Replayed && rr.Unsupported == "" && rr.Unrecorded(rep.NoisyAt) {
			rrs = append(rrs, rr)
		}
	}
	slices.SortStableFunc(rrs, func(a, b verdict.RuleResult) int {
		if a.Unjudged != b.Unjudged {
			return b.Unjudged - a.Unjudged
		}
		return strings.Compare(a.Rule, b.Rule)
	})
	var out []verdict.Unrecorded
	var failed error
	for _, rr := range rrs[:min(len(rrs), unrecordedMost)] {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		u := verdict.Unrecorded{Rule: rr.Rule, Firings: rr.Firings - rr.Judged(), Top: []verdict.AlertCount{}}
		top, all, err := from.Alerts(ctx, rr.Rule, verdict.TopRead)
		if err != nil {
			return out, err
		}
		u.Alerts, u.Top = all, verdict.TopAlerts(top)
		switch counts := countsEvents(versions, rr.Rule); {
		case counts:
			if firesOnNoData(versions, rr.Rule) {
				fs, more, err := from.Firings(ctx, rr.Rule, unrecordedFired)
				if err != nil {
					return out, err
				}
				if more || slices.ContainsFunc(fs, noData) {
					break // not all events, or not known to be
				}
			}
			all := u.Firings
			u.Events, u.Clear = true, &all
		case failed == nil:
			fs, more, err := from.Firings(ctx, rr.Rule, unrecordedFired)
			if err != nil {
				return out, err
			}
			if more {
				break // more than is read: how many went past isn't told
			}
			n, bound, err := clearOf(ctx, h, versions, rr.Rule, fs, rep.From, rep.To, step)
			if err != nil {
				failed = err
			} else if bound != "" {
				u.Clear, u.Bound = &n, bound
			}
		}
		out = append(out, u)
	}
	if failed == nil {
		failed = ctx.Err()
	}
	return out, failed
}

// countsEvents reports whether a rule counts events in every version it is
// in, and is in one.
func countsEvents(versions []replay.Version, rule string) bool {
	in := false
	for _, ver := range versions {
		for _, r := range ver.Rules {
			if r.Name != rule {
				continue
			}
			if _, ok := r.EventWindow(); !ok {
				return false
			}
			in = true
		}
	}
	return in
}

// firesOnNoData reports whether a rule fires when its query returns
// nothing, in a version it is in: a Grafana rule whose noDataState isn't
// OK. Its alerts then are NoData's (noData), not its condition's.
func firesOnNoData(versions []replay.Version, rule string) bool {
	for _, ver := range versions {
		for _, r := range ver.Rules {
			if r.Name == rule && r.Graph != nil && r.Graph.NoData != rules.StateOK {
				return true
			}
		}
	}
	return false
}

// noData reports whether a firing is a Grafana rule's on no data, as the
// replay labels it.
func noData(f firing.Firing) bool { return f.Labels[replay.StateLabel] == replay.NoDataState }

// Judged are, as an UnrecordedFrom, a report's firings with no record.
type Judged []verdict.Judged

func (js Judged) Firings(_ context.Context, rule string, most int) ([]firing.Firing, bool, error) {
	var out []firing.Firing
	for _, j := range js {
		if j.Rule == rule && j.NoRecord {
			if len(out) == most {
				return out, true, nil
			}
			out = append(out, j.Firing)
		}
	}
	return out, false, nil
}

func (js Judged) Alerts(_ context.Context, rule string, most int) ([]verdict.AlertCount, int, error) {
	by := map[string]*verdict.AlertCount{}
	for _, j := range js {
		if j.Rule != rule || !j.NoRecord {
			continue
		}
		k := labelKey(j.Labels)
		if by[k] == nil {
			by[k] = &verdict.AlertCount{Labels: j.Labels}
		}
		by[k].Firings++
	}
	all := make([]verdict.AlertCount, 0, len(by))
	for _, k := range slices.Sorted(maps.Keys(by)) {
		all = append(all, *by[k])
	}
	slices.SortStableFunc(all, func(a, b verdict.AlertCount) int { return b.Firings - a.Firings })
	return all[:min(len(all), most)], len(by), nil
}

// deciding is the one bound that decides a rule's condition: its only
// one, not nothing, not a range's end, not on the right of unless, and the
// whole condition, not an operand of and, or or unless (a Grafana classic
// condition names its one by its reducer and query). ok is false for any
// other.
func deciding(r rules.Rule) (rules.Bound, bool) {
	bs := r.Bounds()
	if len(bs) != 1 {
		return rules.Bound{}, false
	}
	b := bs[0]
	if b.Value == 0 || b.Rest != nil || b.Unless || (b.Of != "" && r.Graph == nil) {
		return rules.Bound{}, false
	}
	return b, true
}

// past is the number rules.NearBy past a bound, the way it holds more.
func past(b rules.Bound) float64 {
	if b.Above() {
		return b.Value + rules.NearBy*math.Abs(b.Value)
	}
	return b.Value - rules.NearBy*math.Abs(b.Value)
}

// clearOf is how many of a rule's firings fs went well past its bound over
// [from, to]: those a replay of it with the bound rules.NearBy further
// fires within, alike in their labels, as the ledger keeps them. bound is
// the bound as compared, "" when the rule has no one bound that decides it
// in every version it is in, and then n is nothing.
func clearOf(ctx context.Context, h replay.History, versions []replay.Version, rule string, fs []firing.Firing, from, to time.Time,
	step time.Duration) (n int, bound string, err error) {
	vs := make([]replay.Version, len(versions))
	for i, ver := range versions {
		vs[i].From = ver.From
		var rs []rules.Rule
		for _, r := range ver.Rules {
			if r.Name == rule {
				rs = append(rs, r)
			}
		}
		if len(rs) == 0 {
			continue
		}
		if len(rs) > 1 {
			return 0, "", nil
		}
		b, ok := deciding(rs[0])
		if !ok {
			return 0, "", nil
		}
		at := b.Op + " " + verdict.Number(b.Value)
		if bound != "" && bound != at {
			return 0, "", nil // its bound changed: well past which?
		}
		bound = at
		vs[i].Rules = []rules.Rule{rs[0].WithBound(b, past(b))}
	}
	if bound == "" {
		return 0, "", nil
	}
	ran, clear := replay.Run(ctx, h, vs, from, to, step, 1)
	if i := slices.IndexFunc(ran, func(r rules.Rule) bool { return r.Unsupported != "" }); i >= 0 {
		if u := ran[i].Unsupported; u == replay.Unfinished || strings.HasPrefix(u, "query failed") {
			return 0, "", errors.New(u)
		}
		return 0, "", nil
	}
	byKey := map[string][]firing.Firing{}
	for _, c := range clear {
		k := labelKey(verdict.KeptLabels(c.Labels))
		byKey[k] = append(byKey[k], c)
	}
	for _, f := range fs {
		if noData(f) {
			continue // fired on no data, at no value
		}
		if slices.ContainsFunc(byKey[labelKey(verdict.KeptLabels(f.Labels))], func(c firing.Firing) bool { return overlap(f, c, to) }) {
			n++
		}
	}
	return n, bound, nil
}

// overlap reports whether two firings were on at once, one still on at to
// taken as on until then.
func overlap(a, b firing.Firing, to time.Time) bool {
	end := func(f firing.Firing) time.Time {
		if f.End.IsZero() {
			return to
		}
		return f.End
	}
	return a.Start.Before(end(b)) && b.Start.Before(end(a)) || a.Start.Equal(b.Start)
}

// labelKey tells label sets apart.
func labelKey(l map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(l)) {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(l[k])
		b.WriteByte(0)
	}
	return b.String()
}
