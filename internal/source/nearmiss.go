package source

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// How much near misses are looked for: the incidents looked at, and the
// rules replayed again over the whole period to say what a shorter "for"
// would cost.
const (
	nearIncidents = 20
	nearCosts     = 10
	// nearEach is the most rules kept for an incident, and nearAsks the
	// most questions asked of the history in all.
	nearEach = 5
	nearAsks = 2000
	// nearFor is the longest "for" looked at: one longer than a week is
	// beyond any period's near miss.
	nearFor = 7 * 24 * time.Hour
	// nearNumbers is the most of a rule's numbers looked at by value.
	nearNumbers = 4
)

// nearBounds are the numbers of a rule's condition looked at by value: the
// first few, but nothing, as nothing is within a share of nothing.
func nearBounds(r rules.Rule) []rules.Bound {
	bs := slices.DeleteFunc(r.Bounds(), func(b rules.Bound) bool { return b.Value == 0 })
	return bs[:min(len(bs), nearNumbers)]
}

// NearMisses looks, for each incident in rep that nothing caught, or only
// noisy rules did, at the rules that nearly caught it: those whose condition
// held about then, but not for their "for", or only long enough to fire
// after the incident; and, of the rest, those whose condition compares a
// value with a number the value came within verdict.NearBy of. For each, it
// keeps the longest shorter "for", or the round number short of the value,
// that would have caught it, and replays the rule with that over the
// period, to say what it would cost. Of a condition with several numbers
// (a Grafana range, PromQL's and, or and unless), each is looked at, the
// others as they are: by the values it compares where the history returns
// them, or else by replaying the rule with nearer numbers, the furthest
// within verdict.NearBy first, and halving the way back when that would
// have caught the incident. done is false when not every incident
// was looked at, for the bounds, ctx running out or a question failing; err
// says which of those two it was, when it was one.
//
// versions are the rules as they stood, in order; an incident is looked at
// against the version in effect when it started, and the cost is of each
// version with the shorter "for", or the new number. A rule is passed over
// that is noisy, couldn't be replayed, has neither a "for" nor such a
// number, has a "for" over a week, shares its name with another, or whose
// own labels disagree with the incident's. excuse, when not nil, says which
// firings that caught nothing a quiet window excuses, as the ledger does.
func NearMisses(ctx context.Context, h replay.History, versions []replay.Version, rep *verdict.Report, incs []incident.Incident,
	o verdict.Options, step time.Duration, parallel int, excuse func(firing.Firing) bool) (out []verdict.NearMiss, done bool, err error) {
	results := map[string]verdict.RuleResult{}
	for _, rr := range rep.Rules {
		results[rr.Rule] = rr
	}
	// The work: each rule, with the incidents to look at it for. A rule the
	// same in several versions is one piece of work.
	type job struct {
		rule rules.Rule
		incs []incident.Incident
	}
	jobs := map[string]*job{}
	var keys []string
	looking := LookedAt(rep)
	for _, ir := range rep.Incidents {
		if !looking[ir.ID] || len(versions) == 0 {
			continue
		}
		// The first version stands for the time before it too.
		v := 0
		for i, ver := range versions {
			if !ver.From.After(ir.Start) {
				v = i
			}
		}
		rs := versions[v].Rules
		named := map[string]int{}
		for _, r := range rs {
			named[r.Name]++
		}
		for _, r := range rs {
			rr, ok := results[r.Name]
			switch {
			case r.For <= 0 && len(nearBounds(r)) == 0, r.For < 0, r.For > nearFor,
				r.Unsupported != "" || !ok || rr.Noisy || rr.Unsupported != "" || named[r.Name] > 1,
				slices.ContainsFunc(ir.CaughtBy, func(c verdict.Catch) bool { return c.Rule == r.Name }),
				!ir.Labels.Agree(r.Labels, o.Match):
				continue
			}
			k := r.Name + "\x00" + r.Key()
			if jobs[k] == nil {
				jobs[k] = &job{rule: r}
				keys = append(keys, k)
			}
			jobs[k].incs = append(jobs[k].incs, ir.Incident)
		}
	}

	if len(keys) == 0 {
		return nil, true, nil // nothing to look at, however little time is left
	}

	// Ask when each rule's condition held about its incidents, a few rules
	// at a time, up to nearAsks questions in all.
	var mu sync.Mutex
	var found []find
	var failed error
	fail := func(err error) {
		mu.Lock()
		if failed == nil {
			failed = err
		}
		mu.Unlock()
	}
	var asked atomic.Int64
	work := make(chan *job)
	var wg sync.WaitGroup
	for range max(1, parallel) {
		wg.Go(func() {
			// A rule no check foresaw stops the looking, not the server.
			defer func() {
				if v := recover(); v != nil {
					fail(fmt.Errorf("looking for near misses: %v", v))
					for range work {
					}
				}
			}()
			for j := range work {
				every := replay.Every(j.rule, step)
				bounds := nearBounds(j.rule)
				keep := func(f find) {
					f.Rule, f.Before, f.rule = j.rule.Name, results[j.rule.Name], j.rule
					mu.Lock()
					found = append(found, f)
					mu.Unlock()
				}
				// From long enough before that a firing kept on through a dip,
				// by keep_firing_for, is seen to start then: up to a day.
				margin := j.rule.For + min(j.rule.KeepFiringFor, 24*time.Hour) + every
				for _, w := range windows(j.incs, margin, step, o) {
					// By its "for" first: its condition held, but not for long
					// enough.
					var left []incident.Incident
					if j.rule.For > 0 {
						if ctx.Err() != nil || asked.Add(1) > nearAsks {
							break
						}
						held, err := replay.Held(ctx, h, j.rule, w.from, w.to, step)
						if err != nil {
							fail(err)
							break
						}
						for _, in := range w.incs {
							if nm, ok := verdict.Nearly(in, held, j.rule.For, every, o); ok {
								keep(find{NearMiss: nm})
							} else {
								left = append(left, in)
							}
						}
					} else {
						left = w.incs
					}
					// Then, for the incidents left, by its value: it came close.
					if len(bounds) == 0 || len(left) == 0 {
						continue
					}
					stop := !byValue(ctx, h, j.rule, bounds, w, left, step, every, o, &asked, fail, keep)
					if stop {
						break
					}
				}
			}
		})
	}
	for _, k := range keys {
		if ctx.Err() != nil || asked.Load() >= nearAsks {
			break
		}
		work <- jobs[k]
	}
	close(work)
	wg.Wait()
	if failed == nil {
		failed = ctx.Err()
	}
	done = failed == nil && asked.Load() <= nearAsks

	// The incidents' order, then the rules that would have changed least.
	order := map[string]int{}
	for i, ir := range rep.Incidents {
		order[ir.ID] = i
	}
	sort.Slice(found, func(a, b int) bool {
		x, y := found[a], found[b]
		if order[x.Incident] != order[y.Incident] {
			return order[x.Incident] < order[y.Incident]
		}
		if x.Kept() != y.Kept() {
			return x.Kept() > y.Kept() // the more of its "for", or its number, kept
		}
		return x.Rule < y.Rule
	})

	// What each shorter "for" would cost, for the first few.
	alone := o
	alone.SomeRules = true
	alone.Hours = nil
	type cost struct {
		rule  string
		d     time.Duration
		value float64 // the new number, when by value
		by    bool
		at    int // which number
	}
	costs := map[cost]*verdict.RuleResult{}
	// The few that would have changed least, for each incident.
	each := map[string]int{}
	found = slices.DeleteFunc(found, func(f find) bool { each[f.Incident]++; return each[f.Incident] > nearEach })
	out = make([]verdict.NearMiss, len(found))
	tried := 0
	for i, f := range found {
		out[i] = f.NearMiss
		nm := &out[i]
		k := cost{rule: nm.Rule, d: nm.NewFor}
		if nm.Value != nil {
			k.value, k.by, k.at = nm.Value.New, true, f.bound.At
		}
		if after, ok := costs[k]; ok {
			nm.After = after
			continue
		}
		if tried >= nearCosts || ctx.Err() != nil {
			continue
		}
		tried++
		// The rule as it stood in each version, none of them waiting longer
		// than the shorter "for", or, by value, none with a number short of
		// the new one.
		vs := make([]replay.Version, len(versions))
		for v, ver := range versions {
			vs[v].From = ver.From
			for _, r := range ver.Rules {
				if r.Name != nm.Rule {
					continue
				}
				if nm.Value == nil {
					r.For = min(r.For, nm.NewFor)
				}
				for _, b := range r.Bounds() {
					if nm.Value == nil || b.At != f.bound.At || b.Op != f.bound.Op || b.Unless != f.bound.Unless || b.Of != f.bound.Of {
						continue
					}
					if b.Lowers() {
						r = r.WithBound(b, min(b.Value, nm.Value.New))
					} else {
						r = r.WithBound(b, max(b.Value, nm.Value.New))
					}
				}
				vs[v].Rules = append(vs[v].Rules, r)
			}
		}
		ran, fs := replay.Run(ctx, h, vs, o.From, o.To, step, 1)
		if i := slices.IndexFunc(ran, func(r rules.Rule) bool { return r.Unsupported != "" }); i >= 0 {
			// A question failed, or time ran out; or, paused or turned off
			// for part of the period, the rule has no cost to tell.
			if u := ran[i].Unsupported; failed == nil && (u == replay.Unfinished || strings.HasPrefix(u, "query failed")) {
				failed = errors.New(u)
			}
			continue
		}
		after := &verdict.RuleResult{Rule: nm.Rule}
		alt := verdict.Judge(nil, fs, incs, alone)
		if j := slices.IndexFunc(alt.Rules, func(r verdict.RuleResult) bool { return r.Rule == nm.Rule }); j >= 0 {
			*after = alt.Rules[j]
		}
		Excuse(after, alt.Firings, excuse)
		costs[k] = after
		nm.After = after
	}
	if failed == nil {
		failed = ctx.Err()
	}
	return out, done && failed == nil, failed
}

// LookedAt are the incidents of rep NearMisses looks at: the first twenty
// that nothing caught, or only noisy rules did.
func LookedAt(rep *verdict.Report) map[string]bool {
	out := map[string]bool{}
	for _, ir := range rep.Incidents {
		if (ir.Missed() || ir.OnlyNoisy()) && len(out) < nearIncidents {
			out[ir.ID] = true
		}
	}
	return out
}

// Excuse takes the firings that caught nothing and that excuse says a quiet
// window excuses out of a rule's stones, as the ledger counts them.
func Excuse(r *verdict.RuleResult, js []verdict.Judged, excuse func(firing.Firing) bool) {
	if excuse == nil {
		return
	}
	for _, j := range js {
		if j.Rule == r.Rule && !j.White && excuse(j.Firing) {
			r.Firings--
			r.Black--
			r.Excused++
		}
	}
}

// A window is a stretch of time to ask a rule's condition over, and the
// incidents in it.
type window struct {
	from, to time.Time
	incs     []incident.Incident
}

// windows are where to look for each incident: from long enough before it
// that an alert pending then would have fired before it, to as long after
// it, to see how long the condition held. Each starts on a step from the
// period's start, so the rule is evaluated when the replay evaluated it.
// Those that overlap are asked for together.
func windows(incs []incident.Incident, margin, step time.Duration, o verdict.Options) []window {
	onStep := func(t time.Time) time.Time {
		n := t.Sub(o.From) / step
		if t.Before(o.From.Add(n * step)) {
			n--
		}
		return o.From.Add(n * step)
	}
	var ws []window
	for _, in := range incs {
		// Within the period, whatever the incident's end: a firing after it
		// isn't judged. And from a day before the incident at most, whatever
		// the lead.
		w := window{onStep(in.Start.Add(-min(o.Lead, 24*time.Hour) - margin)), in.EndOr(o.UnknownLen).Add(margin), []incident.Incident{in}}
		if w.to.After(o.To) {
			w.to = o.To
		}
		if w.from.Before(w.to) {
			ws = append(ws, w)
		}
	}
	sort.Slice(ws, func(a, b int) bool { return ws[a].from.Before(ws[b].from) })
	var out []window
	for _, w := range ws {
		if n := len(out); n > 0 && !w.from.After(out[n-1].to) {
			out[n-1].to = later(out[n-1].to, w.to)
			out[n-1].incs = append(out[n-1].incs, w.incs...)
			continue
		}
		out = append(out, w)
	}
	return out
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// find is a near miss found, and the rule and, by value, its number that
// would have caught the incident.
type find struct {
	verdict.NearMiss
	rule  rules.Rule
	bound rules.Bound
}

// byValue looks, for the incidents left in a window, at each of a rule's
// numbers, and keeps for each incident the near miss that changes its
// number least. ok is false when the bounds or ctx ran out, or a question
// failed, which it passes to fail.
func byValue(ctx context.Context, h replay.History, r rules.Rule, bounds []rules.Bound, w window, left []incident.Incident,
	step, every time.Duration, o verdict.Options, asked *atomic.Int64, fail func(error), keep func(nm find)) (ok bool) {
	type kept struct {
		nm verdict.NearMiss
		b  rules.Bound
	}
	best := map[string]kept{}
	better := func(nm verdict.NearMiss, b rules.Bound) {
		if k, ok := best[nm.Incident]; ok && !(nm.Kept() > k.nm.Kept() || nm.Kept() == k.nm.Kept() && nm.Delay < k.nm.Delay) {
			return
		}
		best[nm.Incident] = kept{nm, b}
	}
	defer func() {
		for _, in := range left {
			if k, ok := best[in.ID]; ok {
				keep(find{NearMiss: k.nm, bound: k.b})
			}
		}
	}()
	ask := func() bool { return ctx.Err() == nil && asked.Add(1) <= nearAsks }
	for _, b := range bounds {
		if b.Values {
			if !ask() {
				return false
			}
			values, err := replay.Values(ctx, h, r, b, w.from, w.to, step)
			if err != nil {
				fail(err)
				return false
			}
			for _, in := range left {
				if nm, ok := verdict.NearlyBy(in, values, r, b, every, o); ok {
					better(nm, b)
				}
			}
			continue
		}
		// The rule replayed with b's number at the round numbers within a
		// fifth: a number nearer holds less, so if the furthest wouldn't have
		// caught an incident, none would.
		steps := b.Steps()
		fired := map[int][]firing.Firing{}
		probe := func(i int) ([]firing.Firing, bool) {
			if fs, ok := fired[i]; ok {
				return fs, true
			}
			if !ask() {
				return nil, false
			}
			fs, err := replay.Fired(ctx, h, r.WithBound(b, steps[i]), w.from, w.to, step)
			if err != nil {
				fail(err)
				return nil, false
			}
			fired[i] = fs
			return fs, true
		}
		for _, in := range left {
			if len(steps) == 0 {
				break
			}
			hi := len(steps) - 1
			fs, ok := probe(hi)
			if !ok {
				return false
			}
			nm, caught := verdict.NearlyAt(in, fs, r, b, steps[hi], every, o)
			if !caught {
				continue
			}
			for lo := 0; lo < hi; {
				mid := (lo + hi) / 2
				fs, ok := probe(mid)
				if !ok {
					return false
				}
				if at, caught := verdict.NearlyAt(in, fs, r, b, steps[mid], every, o); caught {
					hi, nm = mid, at
				} else {
					lo = mid + 1
				}
			}
			better(nm, b)
		}
	}
	return true
}
