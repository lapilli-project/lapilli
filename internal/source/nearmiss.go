package source

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

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
)

// NearMisses looks, for each incident in rep that nothing caught, or only
// noisy rules did, at the rules that nearly caught it: those whose condition
// held about then, but not for their "for". For each, it keeps the longest
// shorter "for" that would have caught it, and replays the rule with that
// over the period, to say what it would cost.
//
// versions are the rules as they stood, in order; an incident is looked at
// against the version in effect when it started. A rule is passed over that
// is noisy, couldn't be replayed, has no "for", shares its name with
// another, or whose own labels disagree with the incident's.
func NearMisses(ctx context.Context, h replay.History, versions []replay.Version, rep *verdict.Report, incs []incident.Incident,
	o verdict.Options, step time.Duration, parallel int) []verdict.NearMiss {
	results := map[string]verdict.RuleResult{}
	for _, rr := range rep.Rules {
		results[rr.Rule] = rr
	}
	// The work: each rule, with the incidents to look at it for.
	type job struct {
		rule rules.Rule
		incs []incident.Incident
	}
	jobs := map[[2]int]*job{} // version and rule
	looked := 0
	for _, ir := range rep.Incidents {
		if !ir.Missed() && !ir.OnlyNoisy() {
			continue
		}
		if looked++; looked > nearIncidents {
			break
		}
		if len(versions) == 0 {
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
		for i, r := range rs {
			rr, ok := results[r.Name]
			switch {
			case r.For <= 0 || r.Unsupported != "" || !ok || rr.Noisy || rr.Unsupported != "" || named[r.Name] > 1,
				slices.ContainsFunc(ir.CaughtBy, func(c verdict.Catch) bool { return c.Rule == r.Name }),
				!ir.Labels.Agree(r.Labels, o.Match):
				continue
			}
			k := [2]int{v, i}
			if jobs[k] == nil {
				jobs[k] = &job{rule: r}
			}
			jobs[k].incs = append(jobs[k].incs, ir.Incident)
		}
	}
	keys := make([][2]int, 0, len(jobs))
	for k := range jobs {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b [2]int) int {
		if a[0] != b[0] {
			return a[0] - b[0]
		}
		return a[1] - b[1]
	})

	// Ask when each rule's condition held about its incidents, a few rules
	// at a time.
	type find struct {
		verdict.NearMiss
		rule rules.Rule
		v    int
	}
	var mu sync.Mutex
	var found []find
	work := make(chan [2]int)
	var wg sync.WaitGroup
	for range max(1, parallel) {
		wg.Go(func() {
			for k := range work {
				j := jobs[k]
				every := replay.Every(j.rule, step)
				for _, w := range windows(j.incs, j.rule.For+every, o) {
					if ctx.Err() != nil {
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
						found = append(found, find{nm, j.rule, k[0]})
						mu.Unlock()
					}
				}
			}
		})
	}
	for _, k := range keys {
		if ctx.Err() != nil {
			break
		}
		work <- k
	}
	close(work)
	wg.Wait()

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
	type cost struct {
		v    int
		rule string
		d    time.Duration
	}
	costs := map[cost]*verdict.RuleResult{}
	out := make([]verdict.NearMiss, len(found))
	for i, f := range found {
		out[i] = f.NearMiss
		nm := &out[i]
		k := cost{f.v, nm.Rule, nm.NewFor}
		if after, ok := costs[k]; ok {
			nm.After = after
			continue
		}
		if len(costs) >= nearCosts || ctx.Err() != nil {
			continue
		}
		alts, err := replay.WithFor(ctx, h, f.rule, []time.Duration{nm.NewFor}, o.From, o.To, step)
		if err != nil {
			continue
		}
		after := &verdict.RuleResult{Rule: nm.Rule}
		alt := verdict.Judge(nil, alts[nm.NewFor], incs, alone)
		if j := slices.IndexFunc(alt.Rules, func(r verdict.RuleResult) bool { return r.Rule == nm.Rule }); j >= 0 {
			*after = alt.Rules[j]
		}
		costs[k] = after
		nm.After = after
	}
	return out
}

// A window is a stretch of time to ask a rule's condition over, and the
// incidents in it.
type window struct {
	from, to time.Time
	incs     []incident.Incident
}

// windows are where to look for each incident: from long enough before it
// that an alert pending then would have fired before it, to as long after
// it, to see how long the condition held. Those that overlap are asked for
// together.
func windows(incs []incident.Incident, margin time.Duration, o verdict.Options) []window {
	var ws []window
	for _, in := range incs {
		ws = append(ws, window{in.Start.Add(-o.Lead - margin), in.EndOr(o.UnknownLen).Add(margin), []incident.Incident{in}})
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
