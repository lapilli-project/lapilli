package source

import (
	"context"
	"slices"
	"sort"
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
)

// NearMisses looks, for each incident in rep that nothing caught, or only
// noisy rules did, at the rules that nearly caught it: those whose condition
// held about then, but not for their "for", or only long enough to fire
// after the incident. For each, it keeps the longest shorter "for" that
// would have caught it, and replays the rule with that over the period, to
// say what it would cost. done is false when ctx ran out first.
//
// versions are the rules as they stood, in order; an incident is looked at
// against the version in effect when it started, and the cost is of each
// version with the shorter "for". A rule is passed over that is noisy,
// couldn't be replayed, has no "for", shares its name with another, or whose
// own labels disagree with the incident's. excuse, when not nil, says which
// firings that caught nothing a quiet window excuses, as the ledger does.
func NearMisses(ctx context.Context, h replay.History, versions []replay.Version, rep *verdict.Report, incs []incident.Incident,
	o verdict.Options, step time.Duration, parallel int, excuse func(firing.Firing) bool) (out []verdict.NearMiss, done bool) {
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
	looked := 0
	for _, ir := range rep.Incidents {
		if !ir.Missed() && !ir.OnlyNoisy() {
			continue
		}
		if looked++; looked > nearIncidents || len(versions) == 0 {
			break
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
			case r.For <= 0 || r.For > nearFor || r.Unsupported != "" || !ok || rr.Noisy || rr.Unsupported != "" || named[r.Name] > 1,
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
		return nil, true // nothing to look at, however little time is left
	}

	// Ask when each rule's condition held about its incidents, a few rules
	// at a time, up to nearAsks questions in all.
	type find struct {
		verdict.NearMiss
		rule rules.Rule
	}
	var mu sync.Mutex
	var found []find
	var asked atomic.Int64
	work := make(chan *job)
	var wg sync.WaitGroup
	for range max(1, parallel) {
		wg.Go(func() {
			for j := range work {
				every := replay.Every(j.rule, step)
				for _, w := range windows(j.incs, j.rule.For+every, step, o) {
					if ctx.Err() != nil || asked.Add(1) > nearAsks {
						break
					}
					held, err := replay.Held(ctx, h, j.rule, w.from, w.to, step)
					if err != nil {
						break
					}
					for _, in := range w.incs {
						nm, ok := verdict.Nearly(in, held, j.rule.For, every, o)
						if !ok {
							continue
						}
						nm.Rule, nm.Before = j.rule.Name, results[j.rule.Name]
						mu.Lock()
						found = append(found, find{nm, j.rule})
						mu.Unlock()
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
	done = ctx.Err() == nil && asked.Load() <= nearAsks

	// The incidents' order, then the rules that would have changed least.
	order := map[string]int{}
	for i, ir := range rep.Incidents {
		order[ir.ID] = i
	}
	kept := func(nm verdict.NearMiss) float64 { return float64(nm.NewFor) / float64(nm.For) }
	sort.Slice(found, func(a, b int) bool {
		x, y := found[a], found[b]
		if order[x.Incident] != order[y.Incident] {
			return order[x.Incident] < order[y.Incident]
		}
		if kept(x.NearMiss) != kept(y.NearMiss) {
			return kept(x.NearMiss) > kept(y.NearMiss) // the more of its "for" kept
		}
		return x.Rule < y.Rule
	})

	// What each shorter "for" would cost, for the first few.
	alone := o
	alone.SomeRules = true
	alone.Hours = nil
	type cost struct {
		rule string
		d    time.Duration
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
		k := cost{nm.Rule, nm.NewFor}
		if after, ok := costs[k]; ok {
			nm.After = after
			continue
		}
		if tried >= nearCosts || ctx.Err() != nil {
			continue
		}
		tried++
		// The rule as it stood in each version, none of them waiting longer
		// than the shorter "for".
		vs := make([]replay.Version, len(versions))
		for v, ver := range versions {
			vs[v].From = ver.From
			for _, r := range ver.Rules {
				if r.Name == nm.Rule {
					r.For = min(r.For, nm.NewFor)
					vs[v].Rules = append(vs[v].Rules, r)
				}
			}
		}
		ran, fs := replay.Run(ctx, h, vs, o.From, o.To, step, 1)
		if slices.ContainsFunc(ran, func(r rules.Rule) bool { return r.Unsupported != "" }) {
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
	return out, done && ctx.Err() == nil
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
