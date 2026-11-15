package source

import (
	"context"
	"errors"
	"fmt"
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

// unrecordedMost is how many rules Unrecorded looks at, the most firings
// first: each is replayed once more, over the whole period.
const unrecordedMost = 10

// Unrecorded works out, for each rule of rep that fired rep.NoisyAt.Firings
// times or more in a period with no record (Report.NoRecord), and was
// replayed, what its firings were (verdict.Unrecorded): the most firings
// first, unrecordedMost at most. fired gives a rule's firings in the
// period, as they were judged, those a quiet window excused aside; teamOf,
// when not nil, the team whose labels an alert has.
//
// How many went well past the bound is known only of a rule whose
// condition has one bound that decides it (deciding), in each version it
// is in: it is replayed with the bound rules.NearBy further, and each of
// its firings that one of those overlaps, alike in its labels, went well
// past it. A replay that fails, or runs out of time, leaves it unknown, and
// err says why; the rest is kept.
func Unrecorded(ctx context.Context, h replay.History, versions []replay.Version, rep *verdict.Report,
	fired func(rule string) ([]firing.Firing, error), teamOf func(map[string]string) string, step time.Duration) ([]verdict.Unrecorded, error) {
	if !rep.NoRecord {
		return nil, nil
	}
	var rrs []verdict.RuleResult
	for _, rr := range rep.Rules {
		if rr.Replayed && rr.Unsupported == "" && rr.Firings >= rep.NoisyAt.Firings {
			rrs = append(rrs, rr)
		}
	}
	slices.SortStableFunc(rrs, func(a, b verdict.RuleResult) int {
		if a.Firings != b.Firings {
			return b.Firings - a.Firings
		}
		return strings.Compare(a.Rule, b.Rule)
	})
	var out []verdict.Unrecorded
	var failed error
	for _, rr := range rrs[:min(len(rrs), unrecordedMost)] {
		fs, err := fired(rr.Rule)
		if err != nil {
			return out, err
		}
		u := verdict.Unrecorded{Rule: rr.Rule, Firings: rr.Firings, Top: []verdict.AlertCount{}}
		byKey := map[string]*verdict.AlertCount{}
		var keys []string
		for _, f := range fs {
			k := labelKey(f.Labels)
			if byKey[k] == nil {
				a := &verdict.AlertCount{Labels: f.Labels}
				if teamOf != nil {
					a.Team = teamOf(f.Labels)
				}
				byKey[k] = a
				keys = append(keys, k)
			}
			byKey[k].Firings++
		}
		all := make([]verdict.AlertCount, 0, len(keys))
		for _, k := range keys {
			all = append(all, *byKey[k])
		}
		if len(all) > 0 {
			u.Alerts, u.Top = verdict.TopAlerts(all)
		}
		if failed == nil && ctx.Err() == nil {
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
		fmt.Fprintf(&b, "%q=%q,", k, l[k])
	}
	return b.String()
}
