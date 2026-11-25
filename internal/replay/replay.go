// Package replay runs alert rules against history to find when they would
// have fired.
package replay

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/sse"
)

// History is the part of a Prometheus API replay uses.
type History interface {
	QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error)
}

// Unfinished marks a rule whose replay the deadline cut short.
const Unfinished = rules.Unfinished

// Logs gives the history a rule written in LogQL is replayed against: the
// Loki its Logs names (docs/design/loki.md).
type Logs func(r rules.Rule) (History, error)

// withLogs is a history with the Lokis beside it.
type withLogs struct {
	History
	logs Logs
}

// WithLogs is h, the history of the rules written in PromQL, with logs
// beside it, for the rules written in LogQL. Without it, such a rule
// fails.
func WithLogs(h History, logs Logs) History {
	if logs == nil {
		return h
	}
	return &withLogs{h, logs}
}

// Base is the history of the rules written in PromQL, without the Lokis
// WithLogs puts beside it: the Prometheus-compatible API itself, to ask
// for what only it has (its rules, its configuration).
func Base(h History) History {
	if w, ok := h.(*withLogs); ok {
		return w.History
	}
	return h
}

// LogsOf is the Logs WithLogs put beside h, nil for none: what a lane of
// its own, pull request backtests', asks its Lokis through, each by a
// Limiter of the lane's.
func LogsOf(h History) Logs {
	if w, ok := h.(*withLogs); ok {
		return w.logs
	}
	return nil
}

// historyFor is the history r is replayed against: h for PromQL; for LogQL,
// the Loki WithLogs gives for it. An asker already asks its rule's.
func historyFor(h History, r rules.Rule) (History, error) {
	if r.Logs == "" {
		return Base(h), nil
	}
	switch x := h.(type) {
	case *asker:
		return h, nil
	case *withLogs:
		return x.logs(r)
	}
	return nil, errors.New("it is LogQL, and there is no Loki to ask")
}

// asideFor is the history the queries of r's beside its condition, on the
// Loki data source uid, are asked of: the Loki WithLogs gives for it. An
// asker already asks its rule's, in the askers Run made for each.
func asideFor(h History, r rules.Rule, uid string) (History, error) {
	switch x := h.(type) {
	case *asker:
		if a := x.sides[uid]; a != nil {
			return a, nil
		}
	case *withLogs:
		r.Logs = uid
		return x.logs(r)
	}
	return nil, errors.New("its Loki query beside the condition has no Loki to ask")
}

// Version is a set of rules and when it took effect; it is in effect until
// the next version does. A zero From is from before the period.
type Version struct {
	From  time.Time
	Rules []rules.Rule
}

// Run replays the rules in effect over [from, to], a few at a time, and
// returns those rules and when each would have fired. versions are in order
// of From. A rule whose queries fail gets Unsupported set rather than
// failing the run: one bad expression shouldn't hide the verdict on the
// rest.
//
// When a version takes over, its rules pick up the previous version's
// alerts as the rule's own system does on a reload: Prometheus carries them
// over by group, name and labels, pending or firing; Grafana carries them
// over by uid, but starts afresh when the rule changed in what its
// fingerprint holds (rules.Rule.Fingerprint): not its annotations.
//
// Rules that need the same query over the same day share one request; up to
// parallel rules are replayed at once against each history: the
// Prometheus, each Loki, each Grafana data source.
func Run(ctx context.Context, h History, versions []Version, from, to time.Time, step time.Duration, parallel int) ([]rules.Rule, []firing.Firing) {
	ran, fs := RunEach(ctx, h, [][]Version{versions}, from, to, step, parallel)
	return ran[0], fs[0]
}

// RunEach replays each set of versions as Run does, over the same period,
// and the sets share their requests too: to set one set of rules beside
// another.
func RunEach(ctx context.Context, h History, sets [][]Version, from, to time.Time, step time.Duration, parallel int) ([][]rules.Rule, [][]firing.Firing) {
	var runs [][]stint
	var setOf []int
	for i, versions := range sets {
		for _, run := range lineUp(versions, from, to, step) {
			runs = append(runs, run)
			setOf = append(setOf, i)
		}
	}
	ran := make([]rules.Rule, len(runs))
	results := make([][]firing.Firing, len(runs))
	sh := newShared(Base(h))
	askers := make([]*asker, len(runs))
	order := make([]int, len(runs))
	asks := make([]string, len(runs))
	srcs := make([]int, len(runs)) // the history each is replayed against
	weight := map[string]int{}     // requests, by the queries a rule asks for
	for i, run := range runs {
		ran[i] = run[len(run)-1].Rule
		order[i] = i
		// The history the rule is replayed against: one it has none of
		// fails it, as a query that fails does.
		target, err := historyFor(h, ran[i])
		if err != nil {
			ran[i].Unsupported = err.Error()
			for j := range run {
				if run[j].Unsupported == "" {
					run[j].Unsupported = ran[i].Unsupported
				}
			}
			target = sh.h
		}
		src := 0
		if ran[i].Logs != "" && target != nil {
			name := "loki\x00" + ran[i].Logs
			if ran[i].Graph != nil {
				name = "grafana\x00" + ran[i].Logs
			}
			src = sh.srcOf(name, target)
		}
		srcs[i] = src
		// The rule's history in two places at once: its Loki queries
		// beside the condition are asked of the Lokis they are on, each
		// with an asker of its own, as the rule's own history has one.
		sides := map[string]History{}
		if ran[i].Unsupported == "" {
			for _, uid := range ran[i].Graph.AsideUIDs() {
				side, err := asideFor(h, ran[i], uid)
				if err != nil {
					ran[i].Unsupported = err.Error()
					for j := range run {
						if run[j].Unsupported == "" {
							run[j].Unsupported = ran[i].Unsupported
						}
					}
					break
				}
				sides[uid] = side
			}
		}
		var reqs []request
		byUID := map[string][]request{}
		exprs := map[string]bool{}
		for j, st := range run {
			if st.Unsupported != "" {
				break
			}
			lo, hi := st.span(step)
			for uid, rs := range queries(st.Rule, lo, hi, step, st.warm(j)) {
				if uid == "" {
					reqs = append(reqs, rs...)
					for _, r := range rs {
						exprs[r.expr] = true
					}
				} else {
					byUID[uid] = append(byUID[uid], rs...)
				}
			}
		}
		askers[i] = sh.expect(target, src, reqs)
		weighed := len(reqs)
		for _, uid := range slices.Sorted(maps.Keys(sides)) {
			if askers[i].sides == nil {
				askers[i].sides = map[string]*asker{}
			}
			askers[i].sides[uid] = sh.expect(sides[uid], sh.srcOf("grafana\x00"+uid, sides[uid]), byUID[uid])
			weighed += len(byUID[uid])
		}
		asks[i] = strconv.Itoa(src) + "\x00" + strings.Join(slices.Sorted(maps.Keys(exprs)), "\x00")
		weight[asks[i]] = max(weight[asks[i]], weighed)
	}
	// Rules that ask for the same queries go together, so they ask at about
	// the same time and an answer isn't held long; those that ask the most
	// go first, so they don't finish last.
	slices.SortStableFunc(order, func(a, b int) int {
		if wa, wb := weight[asks[a]], weight[asks[b]]; wa != wb {
			return wb - wa
		}
		return strings.Compare(asks[a], asks[b])
	})
	p := progressOf(ctx)
	p.add(len(runs))
	// Each history has parallel rules replayed at once of its own: one
	// that pushes back holds its rules waiting, as it says to, and those
	// waiting shouldn't keep the rules of another from theirs.
	bySrc := map[int][]int{}
	for _, i := range order {
		bySrc[srcs[i]] = append(bySrc[srcs[i]], i)
	}
	var wg sync.WaitGroup
	for _, these := range bySrc {
		sem := make(chan struct{}, max(parallel, 1))
		wg.Go(func() {
			var each sync.WaitGroup
			for _, i := range these {
				sem <- struct{}{}
				each.Go(func() {
					defer func() { <-sem }()
					results[i], ran[i].Unsupported, ran[i].Failed = replayRun(ctx, askers[i], runs[i], to, step)
					askers[i].done()
					p.ran()
				})
			}
			each.Wait()
		})
	}
	wg.Wait()
	// A rule paused for part of the period was replayed for the rest; it
	// just didn't fire while paused.
	type named struct {
		set  int
		name string
	}
	live := map[named]bool{}
	for i, r := range ran {
		k := named{setOf[i], r.Name}
		live[k] = live[k] || r.Unsupported == ""
	}
	outRan := make([][]rules.Rule, len(sets))
	outFs := make([][]firing.Firing, len(sets))
	for i := range ran {
		if ran[i].Unsupported == rules.Paused && live[named{setOf[i], ran[i].Name}] {
			ran[i].Unsupported = ""
		}
		outRan[setOf[i]] = append(outRan[setOf[i]], ran[i])
		outFs[setOf[i]] = append(outFs[setOf[i]], results[i]...)
	}
	for _, fs := range outFs {
		firing.Sort(fs)
	}
	return outRan, outFs
}

// WithFor replays a rule over [from, to] with each of several "for"s in
// place of its own, asking for its history once.
func WithFor(ctx context.Context, h History, r rules.Rule, fors []time.Duration, from, to time.Time, step time.Duration) (map[time.Duration][]firing.Firing, error) {
	if len(fors) == 0 || r.Unsupported != "" {
		return nil, nil
	}
	out := map[time.Duration][]firing.Firing{}
	if r.Graph != nil {
		// Grafana's "for" is in its states: each is replayed of its own.
		for _, d := range fors {
			rd := r
			rd.For = d
			series, err := Condition(ctx, h, rd, from, to, step)
			if err != nil {
				return nil, err
			}
			if err := wide(h, rd, series); err != nil {
				return nil, err
			}
			out[d] = fire(rd, series, step, firing.For(0), to)
		}
		return out, nil
	}
	st, early := asRun(r, from, to, step, slices.Max(fors))
	start := from.Add(-slices.Max(fors))
	if early {
		start, _ = st.span(step)
	}
	series, err := Condition(ctx, h, r, start, to, step)
	if err != nil {
		return nil, err
	}
	if err := wide(h, r, series); err != nil {
		return nil, err
	}
	for _, d := range fors {
		if early {
			// As Run does: from the last evaluation before the period.
			floor := st.floor(step)
			at := func(pending time.Time) time.Time {
				if t := pending.Add(d); t.After(floor) {
					return t
				}
				return floor
			}
			out[d] = st.trim(fire(r, series, step, at, to), step)
			continue
		}
		// As a replay does, an alert pending since before the period fires
		// at its start at the earliest.
		fireAt := func(pending time.Time) time.Time {
			if t := pending.Add(d); t.After(from) {
				return t
			}
			return from
		}
		out[d] = fire(r, series, step, fireAt, to)
	}
	return out, nil
}

// Held is when a rule's condition held over [from, to]: each stretch a label
// set held without a break is a firing with no "for" or keep_firing_for,
// from its first evaluation that held to after its last. One still holding
// at to ends there.
func Held(ctx context.Context, h History, r rules.Rule, from, to time.Time, step time.Duration) ([]firing.Firing, error) {
	if r.Unsupported != "" {
		return nil, nil
	}
	r.For, r.KeepFiringFor = 0, 0
	st, early := asRun(r, from, to, step, 0)
	start := from
	if early {
		start, _ = st.span(step)
	}
	series, err := Condition(ctx, h, r, start, to, step)
	if err != nil {
		return nil, err
	}
	if err := wide(h, r, series); err != nil {
		return nil, err
	}
	fs := fire(r, series, step, firing.For(0), to)
	for i := range fs {
		if fs[i].End.IsZero() {
			fs[i].End = to
		}
	}
	return st.trim(fs, step), nil
}

// Every is how often a replay at step evaluates a rule: as often as its
// group is when that is a whole number of steps, otherwise every step.
func Every(r rules.Rule, step time.Duration) time.Duration {
	if iv := r.GroupEvery(); iv > step && iv%step == 0 {
		return iv
	}
	return step
}

// placed is, for a Prometheus rule whose group's place in its interval is
// known (Phase), how far past the steps' grid its evaluations fall, when a
// replay at step evaluates it as often as its group is: its interval a
// whole number of steps. ok is false otherwise: its evaluations are on the
// steps' grid.
func placed(r rules.Rule, step time.Duration) (off time.Duration, ok bool) {
	if r.Graph != nil || r.Phase <= 0 || r.GroupEvery() < step || r.GroupEvery()%step != 0 || r.Phase >= r.GroupEvery() {
		return 0, false
	}
	return r.Phase % step, true
}

// evaluations are the first and last of a Prometheus rule's evaluations in a
// chunk of the period: the chunk's steps, or each that much later where
// Prometheus places the rule's group in its interval, when that is known,
// and up to to.
func evaluations(r rules.Rule, c [2]time.Time, step time.Duration, to time.Time) (time.Time, time.Time, bool) {
	off, _ := placed(r, step)
	a, b := c[0].Add(off), c[1].Add(off)
	if b.After(to) {
		b = b.Add(-step)
	}
	return a, b, !b.Before(a)
}

// A stint is one version of a rule while it was in effect: evaluated at the
// steps from lo up to, not including, hi.
type stint struct {
	rules.Rule
	lo, hi time.Time
	before bool // in effect before the period, so pending from before it too
	// since is when the version took effect, zero from before the period:
	// a rule new in the version has no evaluation before it.
	since time.Time
}

// warm reports whether a stint, the jth of its run, carries alerts in from
// before it: one in effect before the period, or that follows another of
// its rule. A rule new in the period, or a Grafana rule that changed,
// starts afresh.
func (st stint) warm(j int) bool { return j > 0 || st.before }

// span is what a stint is replayed over: from when it took effect, or, if
// that was before the period, from "for" and keep_firing_for earlier, so
// alerts already pending, or held on, carry in.
//
// A rule whose group is placed within its interval starts from its last
// evaluation before the period (early), whole steps back: what it decided
// there, a firing on when the period starts, is the state the period starts
// in, even where it ends before the group's first evaluation in the period.
func (st stint) span(step time.Duration) (time.Time, time.Time) {
	lo := st.lo
	if st.before {
		if _, ok := st.early(step); ok {
			lo = lo.Add(-st.back(step))
			if st.For+st.KeepFiringFor > 0 {
				// One evaluation more, so the alert pending, or held on,
				// for as long as it takes to be firing at that evaluation
				// has a sample at or before where that began.
				lo = lo.Add(-st.GroupEvery())
			}
		}
		lo = lo.Add(-st.For - st.KeepFiringFor)
	}
	return lo, st.hi.Add(-step)
}

// gap is how long after lo the placed rule's first evaluation in the
// period falls, 0 when it falls on lo or the rule isn't placed.
func (st stint) gap(step time.Duration) time.Duration {
	if _, ok := placed(st.Rule, step); !ok {
		return 0
	}
	iv := int64(st.GroupEvery())
	return time.Duration(((int64(st.Phase)-st.lo.UnixNano())%iv + iv) % iv)
}

// early is the last evaluation of a placed rule's group before the period,
// for a stint in effect before it, when the group's first evaluation in
// the period is later than its start. ok is false for any other.
func (st stint) early(step time.Duration) (time.Time, bool) {
	if !st.before {
		return time.Time{}, false
	}
	g := st.gap(step)
	if g == 0 {
		return time.Time{}, false
	}
	e := st.lo.Add(g - st.GroupEvery())
	if !st.since.IsZero() && st.since.After(e) {
		return time.Time{}, false // the rule wasn't there to evaluate
	}
	return e, true
}

// back is how many whole steps before lo a placed rule's replay starts, to
// reach its last evaluation before the period.
func (st stint) back(step time.Duration) time.Duration {
	return (st.GroupEvery() - st.gap(step) + step - 1) / step * step
}

// trim settles the firings of a stint replayed from the last evaluation
// before the period (early): one there that is on when the period starts is
// reported from its start, as one that held on into it is; one that ended
// before it isn't reported. Any other stint's are as they are.
func (st stint) trim(fs []firing.Firing, step time.Duration) []firing.Firing {
	if _, ok := st.early(step); !ok {
		return fs
	}
	fs = slices.DeleteFunc(fs, func(f firing.Firing) bool { return !f.End.IsZero() && !f.End.After(st.lo) })
	for i := range fs {
		if fs[i].Start.Before(st.lo) {
			fs[i].Start = st.lo
		}
	}
	return fs
}

// asRun is the stint a replay of one rule over [from, to] is, with the
// "for" and keep_firing_for to look back by, set as Run sets them for a
// rule in effect before the period: its replay starts where Run's does
// (early), and ok says whether that is before from at all (a placed group
// whose first evaluation isn't on from). Every replay of a placed group,
// a longer "for" tried, a shorter one, a number, a window of a near miss,
// is made from there, or what Run reports at the period's start is a
// firing the others never see.
func asRun(r rules.Rule, from, to time.Time, step time.Duration, forLook time.Duration) (st stint, ok bool) {
	r.For = forLook
	st = stint{Rule: r, lo: from, hi: to.Add(step), before: true}
	_, ok = st.early(step)
	return st, ok
}

// floor is the earliest a firing of the stint starts: its first step, or,
// where it is replayed from before the period (early), where that starts,
// so that an alert that fired before the evaluation it carries on at isn't
// started later than the samples that held it.
func (st stint) floor(step time.Duration) time.Time {
	if _, ok := st.early(step); ok {
		lo, _ := st.span(step)
		return lo
	}
	return st.lo
}

// lineUp puts each rule's stints in order, in runs whose alerts carry over
// from one stint to the next. A rule unchanged across versions is one stint.
func lineUp(versions []Version, from, to time.Time, step time.Duration) [][]stint {
	// Versions take over at the first step on or after their From.
	at := func(t time.Time) time.Time {
		if !t.After(from) {
			return from
		}
		return from.Add((t.Sub(from) + step - 1) / step * step)
	}
	var runs [][]stint
	open := map[string]int{} // a rule's identity to its run, while in effect
	for i, v := range versions {
		lo, hi := at(v.From), to.Add(step)
		if i+1 < len(versions) && at(versions[i+1].From).Before(hi) {
			hi = at(versions[i+1].From)
		}
		if !lo.Before(hi) {
			continue // replaced before it took effect
		}
		now := map[string]int{}
		dups := map[string]int{}
		for _, r := range v.Rules {
			id := identity(r, dups)
			j, ok := open[id]
			if _, twice := now[id]; twice {
				ok = false // two rules claiming one uid: keep them apart
			}
			switch {
			case !ok || runs[j][len(runs[j])-1].Fingerprint != r.Fingerprint:
				// new, or a Grafana rule changed in what Grafana starts its
				// alerts afresh on
				runs = append(runs, []stint{{Rule: r, lo: lo, hi: hi, before: v.From.Before(from), since: v.From}})
				j = len(runs) - 1
			case same(runs[j][len(runs[j])-1].Rule, r):
				runs[j][len(runs[j])-1].hi = hi
			default:
				runs[j] = append(runs[j], stint{Rule: r, lo: lo, hi: hi})
			}
			now[id] = j
		}
		open = now
	}
	return runs
}

// identity is what a reload carries alerts over by: a Grafana rule's uid;
// otherwise the group, name and labels, with duplicates matched in order.
func identity(r rules.Rule, dups map[string]int) string {
	if r.UID != "" {
		return "uid\x00" + r.UID
	}
	k := r.Group + "\x00" + r.Name + "\x00" + key(r.AlertLabels())
	dups[k]++
	return k + "\x00" + strconv.Itoa(dups[k])
}

func same(a, b rules.Rule) bool {
	return a.Expr == b.Expr && a.For == b.For && a.KeepFiringFor == b.KeepFiringFor && a.Interval == b.Interval && a.QueryOffset == b.QueryOffset &&
		a.Digest == b.Digest && a.Unsupported == b.Unsupported
}

// replayRun replays one run of stints and says why it couldn't, if it
// couldn't; and which of its queries outside its condition failed.
func replayRun(ctx context.Context, h History, run []stint, to time.Time, step time.Duration) ([]firing.Firing, string, []string) {
	merged := map[string]*prom.Series{}
	failed := &failedRefs{refs: map[string]bool{}}
	ctx = context.WithValue(ctx, failedKey{}, failed)
	for j, st := range run {
		if st.Unsupported != "" {
			return nil, st.Unsupported, nil
		}
		lo, hi := st.span(step)
		series, err := condition(ctx, h, st.Rule, lo, hi, step, false, st.warm(j))
		switch {
		case err != nil && (ctx.Err() != nil || errors.Is(err, prom.ErrPastDeadline)):
			return nil, Unfinished, nil
		case err != nil && prom.Refused(err):
			return nil, rules.QueryFailed + err.Error(), nil
		case err != nil:
			return nil, rules.HistoryUnread + err.Error(), nil
		}
		for _, s := range series {
			l := maps.Clone(s.Labels)
			delete(l, "__name__")
			k := key(l)
			if merged[k] == nil {
				merged[k] = &prom.Series{Labels: l}
			}
			merged[k].Samples = append(merged[k].Samples, s.Samples...)
		}
	}
	series := make([]prom.Series, 0, len(merged))
	for _, k := range slices.Sorted(maps.Keys(merged)) {
		series = append(series, *merged[k])
	}
	last := run[len(run)-1]
	if err := wide(h, last.Rule, series); err != nil {
		return nil, err.Error(), nil
	}
	fs := fire(last.Rule, series, step, fireAt(run, step), to)
	fs = run[0].trim(fs, step)
	if !last.hi.After(to) {
		// Taken away or started afresh: its alerts resolved then, whatever
		// keep_firing_for would have held.
		gone := last.hi.Add(step)
		for i := range fs {
			if fs[i].End.IsZero() || fs[i].End.After(gone) {
				fs[i].End = gone
			}
		}
	}
	return fs, "", failed.sorted()
}

// failedKey is where a replay's context keeps the queries outside a
// Grafana rule's condition that failed.
type failedKey struct{}

type failedRefs struct {
	mu   sync.Mutex
	refs map[string]bool
}

func (f *failedRefs) sorted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.refs) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(f.refs))
}

// noteFailed tells the replay's context that a query of a rule failed.
func noteFailed(ctx context.Context, ref string) {
	if f, ok := ctx.Value(failedKey{}).(*failedRefs); ok {
		f.mu.Lock()
		f.refs[ref] = true
		f.mu.Unlock()
	}
}

// fire works out a rule's firings from the steps its condition held,
// evaluated as often as its group is when that is a whole number of steps:
// a 5m group sees its condition every fifth step, and fires and resolves
// only then.
//
// A Grafana rule's series are already of the evaluations its alerts were
// firing at, Grafana's states with its "for" and keep_firing_for worked
// out: they fire as they are. Each series' firings carry a copy of the
// rule's labels with the series' own (wide).
func fire(r rules.Rule, series []prom.Series, step time.Duration, fireAt func(time.Time) time.Time, end time.Time) []firing.Firing {
	keep := r.KeepFiringFor
	if r.Graph != nil {
		fireAt, keep = firing.For(0), 0
	}
	every := step
	if Every(r, step) > step && len(series) > 0 && len(series[0].Samples) > 0 {
		every = Every(r, step)
		var phase int64
		series, phase = evaluated(r, series, step, every)
		at := fireAt
		fireAt = func(pending time.Time) time.Time {
			t := at(pending)
			if r := (t.UnixNano() - phase) % int64(every); r != 0 {
				t = t.Add(every - time.Duration(r))
			}
			return t
		}
	}
	return firing.FromSeries(r.Name, r.AlertLabels(), series, every, fireAt, keep, end)
}

// evaluated keeps of each series the steps a rule evaluated every every is
// evaluated at, and says where on the grid those fall.
func evaluated(r rules.Rule, series []prom.Series, step, every time.Duration) ([]prom.Series, int64) {
	if every <= step || len(series) == 0 || len(series[0].Samples) == 0 {
		return series, 0
	}
	// Evaluations fall on the steps' own grid: the Unix epoch's, unless a
	// period was given in odd seconds; or where the rule's group is placed,
	// when that is known.
	phase := series[0].Samples[0].T.UnixNano() % int64(step)
	if _, ok := placed(r, step); ok {
		phase = int64(r.Phase)
	}
	on := func(t time.Time) bool { return (t.UnixNano()-phase)%int64(every) == 0 }
	var thin []prom.Series
	for _, s := range series {
		var kept []prom.Sample
		for _, smp := range s.Samples {
			if on(smp.T) {
				kept = append(kept, smp)
			}
		}
		if len(kept) > 0 {
			thin = append(thin, prom.Series{Labels: s.Labels, Samples: kept})
		}
	}
	return thin, phase
}

// Values, and NoData below, are of the evaluations in [from, to] alone: a
// number past a round one, or a count of series, at each. An evaluation
// before from isn't one of theirs whatever the group's place, so they start
// at from where the replays of firings (Run, WithFor, Held, Fired) start
// from the last evaluation before it.
//
// Values is, of a rule whose condition compares a value with b, the value at
// each of its evaluations over [from, to] past the furthest round number
// within rules.NearBy of b: those a number that near would hold for. A
// Prometheus rule is asked as it is, with that number, so the history
// returns no more than that; a Grafana rule's value is worked out and the
// rest left out. A bound whose values can't be read has none.
func Values(ctx context.Context, h History, r rules.Rule, b rules.Bound, from, to time.Time, step time.Duration) ([]prom.Series, error) {
	ask, ok := r.ValueOf(b)
	if !ok || ask.Unsupported != "" {
		return nil, nil
	}
	edge := b.Furthest()
	series, err := condition(ctx, h, ask, from, to, step, true, false)
	if err != nil {
		return nil, err
	}
	var out []prom.Series
	for _, s := range series {
		l := maps.Clone(s.Labels)
		delete(l, "__name__")
		smp := slices.DeleteFunc(s.Samples, func(x prom.Sample) bool { return !b.Whole(edge, x.V) })
		if len(smp) > 0 {
			out = append(out, prom.Series{Labels: l, Samples: smp})
		}
	}
	out, _ = evaluated(r, out, step, Every(r, step))
	return out, nil
}

// NoData is, of a Prometheus rule whose condition compares values with
// numbers, the distinct expressions it compares (rules.Rule.Operand) that
// returned no series with a sample at all over [from, to]: a rule that
// reads them could not have fired. Each is asked as count((expr)), which
// returns one series at most, so the history sends back one sample a step
// and not every series of a wide metric. A Grafana rule, a rule of logs and
// one that can't be replayed are none: nil. A question that fails returns
// its error.
func NoData(ctx context.Context, h History, r rules.Rule, from, to time.Time, step time.Duration) ([]string, error) {
	if r.Graph != nil || r.Logs != "" || r.Unsupported != "" {
		return nil, nil
	}
	var out []string
	for _, b := range r.Bounds() {
		expr, ok := r.Operand(b)
		if !ok || slices.Contains(out, expr) {
			continue
		}
		ask := r
		ask.Expr = "count((" + expr + "))"
		series, err := condition(ctx, h, ask, from, to, step, true, false)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(series, func(s prom.Series) bool {
			return slices.ContainsFunc(s.Samples, func(x prom.Sample) bool { return !math.IsNaN(x.V) })
		}) {
			out = append(out, expr)
		}
	}
	return out, nil
}

// Fired is when a rule fired over [from, to], with its "for" and
// keep_firing_for, as a replay of it alone over that time fires it: all of
// its firings, a label set that held at from pending from then; and those
// known to have started when they did, without what held at from and what
// followed it within keep_firing_for (firing.After). Firings still on at to
// have no End.
func Fired(ctx context.Context, h History, r rules.Rule, from, to time.Time, step time.Duration) (all, known []firing.Firing, err error) {
	if r.Unsupported != "" {
		return nil, nil, nil
	}
	keepFor := r.KeepFiringFor
	if r.Graph != nil {
		keepFor = 0
	}
	st, early := asRun(r, from, to, step, r.For)
	start := from
	if early {
		start, _ = st.span(step)
	}
	series, err := Condition(ctx, h, r, start, to, step)
	if err != nil {
		return nil, nil, err
	}
	if err := wide(h, r, series); err != nil {
		return nil, nil, err
	}
	every := Every(r, step)
	var seen []firing.Firing
	if early {
		// All of its firings, as Run has them; what is known to have
		// started when it did is of the period alone, below.
		floor := st.floor(step)
		at := func(pending time.Time) time.Time {
			if t := pending.Add(r.For); t.After(floor) {
				return t
			}
			return floor
		}
		seen = st.trim(fire(r, series, step, at, to), step)
		var inPeriod []prom.Series
		for _, s := range series {
			// A series that held only before the period is no series of it:
			// evaluated reads the grid from the first.
			if kept := slices.DeleteFunc(slices.Clone(s.Samples), func(x prom.Sample) bool { return x.T.Before(from) }); len(kept) > 0 {
				inPeriod = append(inPeriod, prom.Series{Labels: s.Labels, Samples: kept})
			}
		}
		series = inPeriod
	}
	thin, _ := evaluated(r, series, step, every)
	if !early {
		seen = fire(r, series, step, firing.For(r.For), to)
	}
	return seen, fire(r, firing.After(thin, from, every, keepFor), step, firing.For(r.For), to), nil
}

// firingLabel is what a label of a replayed firing takes: an entry of the
// map each series' firings share, its text the rule's or the series'.
const firingLabel = 96

// wide refuses a rule's replay whose firings would carry its labels, its
// group's and its own, on each of its series past what the history's
// samples are held to (--max-samples, prom.SampleBytes each), saying so
// (rules.TooWide), which --fail-unreplayed counts: the rule's
// labels come from whoever writes rules, its series from the history, and
// each series' firings have a copy of them. A rule of 9,999 labels on
// 1,000 series made 1.3 GB; real ones, a dozen labels on 50,000 series,
// some 100 MB at most.
func wide(h History, r rules.Rule, series []prom.Series) error {
	most := prom.DefaultMaxSamples
	if a, ok := h.(*asker); ok {
		h = a.h
	}
	if m, ok := Base(h).(interface{ MaxSamples() int }); ok && m.MaxSamples() > 0 {
		most = m.MaxSamples()
	}
	room := most * prom.SampleBytes
	own := len(r.Labels) + len(r.GroupLabels)
	n := 0
	for _, s := range series {
		if n += (len(s.Labels) + own) * firingLabel; n > room {
			return fmt.Errorf("%sits alerts would carry its %d labels with each of its %d series' own, past the %d MB a range's samples are held to (--max-samples %d)",
				rules.TooWide, own, len(series), room>>20, most)
		}
	}
	return nil
}

// fireAt is when a label set pending since a time fires: once it has been
// pending for the "for" of the stint in effect, and not before that stint
// took over.
func fireAt(run []stint, step time.Duration) func(time.Time) time.Time {
	return func(pending time.Time) time.Time {
		i := 0
		for i+1 < len(run) && !pending.Before(run[i+1].lo) {
			i++
		}
		for ; ; i++ {
			t := pending.Add(run[i].For)
			if f := run[i].floor(step); t.Before(f) {
				t = f
			}
			if i+1 == len(run) || t.Before(run[i].hi) {
				return t
			}
		}
	}
}

// chunk is how many steps one request covers: a day at one minute, well
// under the 11,000 points per series Prometheus allows.
const chunk = 1440

// chunks splits [from, to] into requests on a grid that doesn't depend on
// from, so rules that look back different lengths still ask for the same
// days, and can share the answers.
func chunks(from, to time.Time, step time.Duration) [][2]time.Time {
	length := chunk * step
	// The grid runs from the Unix epoch when from is on a step there, as
	// it is unless a period was given in odd seconds; otherwise from from.
	base := time.Unix(0, 0).UTC()
	if from.Sub(base)%step != 0 {
		base = from
	}
	var out [][2]time.Time
	// From the first chunk on, each starts a length after the one before:
	// counting from base each time would overflow a Duration past 2262.
	next := base.Add((from.Sub(base)/length + 1) * length)
	for c := from; !c.After(to); c, next = next, next.Add(length) {
		end := next.Add(-step)
		if end.After(to) {
			end = to
		}
		out = append(out, [2]time.Time{c, end})
	}
	return out
}

// warmup is how far before from a Grafana rule's queries must start for a
// reduce over several steps to see a whole window, as Grafana's query would.
func warmup(g *rules.Graph, step time.Duration) time.Duration {
	var warm time.Duration
	for _, st := range g.Steps {
		if st.Reduce != nil {
			warm = max(warm, (st.Reduce.Window+step-1)/step*step)
		}
	}
	return warm
}

// queries lists what Condition asks for over [from, to], request by
// request: by the Loki data source the queries beside the rule's condition
// are asked of, "" for the history the rule is replayed against.
func queries(r rules.Rule, from, to time.Time, step time.Duration, warm bool) map[string][]request {
	out := map[string][]request{}
	for _, a := range ranges(r, from, to, step, warm) {
		if !a.reduce {
			out[a.side] = append(out[a.side], request{expr: a.expr, start: a.start.Unix(), end: a.end.Unix()})
		}
	}
	return out
}

// askedRange is one range of a query Condition asks the history for: expr
// over [start, end] at step. A LogQL reduce's (reduce) is asked at Grafana's
// step, by itself (logReduce), and shares nothing with the other rules.
type askedRange struct {
	expr       string
	start, end time.Time
	step       time.Duration
	reduce     bool
	// side is the Loki data source a query beside the rule's condition is
	// asked of, by uid; "" for the history the rule is replayed against.
	side string
}

// ranges lists what Condition asks for over [from, to], range by range:
// the ranges a history is asked for, each as the Prometheus API or Loki's
// client may split them (prom.Pieces), which Plan counts.
func ranges(r rules.Rule, from, to time.Time, step time.Duration, warm bool) []askedRange {
	var out []askedRange
	if r.Graph == nil {
		for _, c := range chunks(from, to, step) {
			if a, b, ok := evaluations(r, c, step, to); ok {
				out = append(out, askedRange{expr: r.Expr, start: a.Add(-r.Lag()), end: b.Add(-r.Lag()), step: step})
			}
		}
		return out
	}
	start := grafanaStart(r, from, step, warm)
	done := map[string]bool{}
	for _, ref := range asked(r.Graph, false) {
		lag := max(r.Graph.Shift[ref], r.Late)
		if a := r.Graph.Asks[ref]; a.Logs {
			// By evaluation chunk, each a batch at a time, as Condition
			// asks; reduces of one query's points, one range.
			if k := logSame(a, lag, r.Graph.Aside[ref]); done[k] {
				continue
			} else {
				done[k] = true
			}
			res, _, _ := logGrid(a)
			for _, c := range chunks(start, to, step) {
				for _, b := range logBatches(a, c[0], c[1], step, lag) {
					out = append(out, askedRange{expr: a.Expr, start: b.lo, end: b.hi, step: res, reduce: true, side: r.Graph.Aside[ref]})
				}
			}
			continue
		}
		expr, _ := r.Graph.Expr(ref, r.AskStep(ref))
		for _, c := range chunks(start, to, step) {
			out = append(out, askedRange{expr: expr, start: c[0].Add(-lag), end: c[1].Add(-lag), step: step, side: r.Graph.Aside[ref]})
		}
	}
	return out
}

// grafanaStart is where a Grafana rule's evaluation over a period from
// from starts: with warm, its alerts worked out from before it; and before
// that, as far back again as the widest reduce over several steps needs to
// see a whole window.
func grafanaStart(r rules.Rule, from time.Time, step time.Duration, warm bool) time.Time {
	if warm {
		from = from.Add(-newStates(r, Every(r, step)).warmup() / step * step)
	}
	return from.Add(-warmup(r.Graph, step))
}

// shifted asks h for expr over [start, end] as it stood lag earlier: at
// each step, the history up to lag before it, as a rule sees it when its
// queries are asked that much earlier.
func shifted(ctx context.Context, h History, expr string, start, end time.Time, step, lag time.Duration) ([]prom.Series, error) {
	series, err := h.QueryRange(ctx, expr, start.Add(-lag), end.Add(-lag), step)
	if err != nil || lag == 0 {
		return series, err
	}
	out := make([]prom.Series, len(series))
	for i, s := range series {
		smp := make([]prom.Sample, len(s.Samples))
		for j, x := range s.Samples {
			smp[j] = prom.Sample{T: x.T.Add(lag), V: x.V}
		}
		out[i] = prom.Series{Labels: s.Labels, Samples: smp}
	}
	return out, nil
}

// logReduce is, at each step of [from, to], a Grafana reduce of a LogQL
// range query's points over its window, as Grafana's Loki data source asks
// them: every a.Res, from a.Window before the evaluation, less lag, to lag
// before it, both ends in. Loki puts a range's points on whole multiples
// of its step since the Unix epoch, the first at or before the range's
// start and the last at or after its end (measured on Loki 3.7.9), so the
// points of every evaluation's window are those of one range over the
// stretch, asked for once and reduced here: LogQL has no subqueries.
// A series with no point in a window has no value there.
func logReduce(ctx context.Context, h History, a rules.Ask, from, to time.Time, step, lag time.Duration) ([]prom.Series, error) {
	got, err := logReduceMany(ctx, h, []rules.Ask{a}, from, to, step, lag)
	if err != nil {
		return nil, err
	}
	return got[0], nil
}

// logReduceMany is logReduce for several reduces of one query's points,
// which share its requests: as asks must be of the same expression, window
// and step (logSame), each is answered from the one range asked for. A
// classic condition's reducer and its series' presence are two of them.
func logReduceMany(ctx context.Context, h History, as []rules.Ask, from, to time.Time, step, lag time.Duration) ([][]prom.Series, error) {
	a0 := as[0]
	res, down, up := logGrid(a0)
	type kept struct {
		labels  map[string]string
		samples []prom.Sample
	}
	byKey := make([]map[string]*kept, len(as))
	keys := make([][]string, len(as))
	for i := range as {
		byKey[i] = map[string]*kept{}
	}
	for _, b := range logBatches(a0, from, to, step, lag) {
		first, last := b.first, b.last
		got, err := h.QueryRange(ctx, a0.Expr, b.lo, b.hi, res)
		if err != nil {
			return nil, err
		}
		for _, s := range got {
			smp := s.Samples
			k := key(s.Labels)
			for at := first; !at.After(last); at = at.Add(step) {
				lo, hi := down(at.Add(-lag-a0.Window)), up(at.Add(-lag))
				i := sort.Search(len(smp), func(i int) bool { return !smp[i].T.Before(lo) })
				var vs []float64
				for ; i < len(smp) && !smp[i].T.After(hi); i++ {
					vs = append(vs, smp[i].V)
				}
				if len(vs) == 0 {
					continue
				}
				for n, a := range as {
					v, ok := logReduced(a, vs)
					if !ok {
						continue
					}
					if byKey[n][k] == nil {
						byKey[n][k] = &kept{labels: s.Labels}
						keys[n] = append(keys[n], k)
					}
					byKey[n][k].samples = append(byKey[n][k].samples, prom.Sample{T: at, V: v})
				}
			}
		}
	}
	out := make([][]prom.Series, len(as))
	for n := range as {
		out[n] = make([]prom.Series, 0, len(keys[n]))
		for _, k := range keys[n] {
			out[n] = append(out[n], prom.Series{Labels: byKey[n][k].labels, Samples: byKey[n][k].samples})
		}
	}
	return out, nil
}

// logReduced is a window's points reduced as a: by Grafana's reduce
// expression, or, for a classic condition's ask, by its reducer
// (rules.ClassicReduce), which finds none in a series of NaN alone.
func logReduced(a rules.Ask, vs []float64) (float64, bool) {
	if a.Classic {
		return rules.ClassicReduce(a.Fn, vs)
	}
	return reduce(a.Fn, vs), true
}

// logSame is the key of the LogQL asks that read the same points of one
// query: its expression, window and step, how long before the evaluation
// the window ends, and the Loki it is asked of (side, "" for the rule's).
func logSame(a rules.Ask, lag time.Duration, side string) string {
	return fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%s", a.Expr, a.Window, a.Res, lag, side)
}

// logGrid is the step a LogQL reduce's range is asked at, and the times on
// its grid since the epoch at or before a time, and at or after it.
func logGrid(a rules.Ask) (res time.Duration, down, up func(time.Time) time.Time) {
	res = a.Res
	if res <= 0 {
		res = time.Second
	}
	down = func(t time.Time) time.Time {
		return time.UnixMilli(floorDiv(t.UnixMilli(), res.Milliseconds()) * res.Milliseconds())
	}
	up = func(t time.Time) time.Time {
		d := down(t)
		if d.Before(t) {
			d = d.Add(res)
		}
		return d
	}
	return res, down, up
}

// logBatch is the evaluations of a LogQL reduce read from one range of its
// query's points: those from first to last, the range from lo to hi.
type logBatch struct {
	first, last, lo, hi time.Time
}

// logBatches are the ranges logReduce asks for to reduce the evaluations
// from from to to, lag before them: the evaluations a batch at a time,
// each batch's points asked for and reduced before the next, so that what
// is held is a batch's points and a window's, not a day's. The requests
// of a replay are counted from them (Plan).
func logBatches(a rules.Ask, from, to time.Time, step, lag time.Duration) []logBatch {
	res, down, up := logGrid(a)
	every := max(int64(step/res), 1)
	batch := max(reducePoints/every, 1)
	var out []logBatch
	for first := from; !first.After(to); first = first.Add(time.Duration(batch) * step) {
		last := first.Add(time.Duration(batch-1) * step)
		if last.After(to) {
			last = to
		}
		out = append(out, logBatch{first, last, down(first.Add(-lag - a.Window)), up(last.Add(-lag))})
	}
	return out
}

// reducePoints is how many of a LogQL query's points, beyond a window's,
// logReduce asks for and holds at once, for each series.
const reducePoints = 10_000

// floorDiv is a / b rounded down, for a before the epoch too.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// Condition returns, for each label set, the steps in [from, to] at which the
// rule's condition held. It fetches and evaluates a day at a time, so a long
// period takes no more memory than a day.
//
// A Grafana rule is evaluated as often as its group is, and its alerts
// held as Grafana's state manager holds them: an evaluation at which a
// query returned nothing is NoData, and becomes what the rule's
// noDataState says; an alert whose series has gone stays as it was until
// it has been gone MissingToResolve evaluations. A query error can't be
// told from history, and isn't replayed.
func Condition(ctx context.Context, h History, r rules.Rule, from, to time.Time, step time.Duration) ([]prom.Series, error) {
	return condition(ctx, h, r, from, to, step, false, true)
}

// condition is Condition; with values, it is the value of a Grafana rule's
// condition at each of its evaluations, where that isn't 0 or NaN, with no
// states. Without warm, a Grafana rule's alerts start afresh at from, as
// Grafana starts them when the rule changes.
func condition(ctx context.Context, h History, r rules.Rule, from, to time.Time, step time.Duration, values, warm bool) ([]prom.Series, error) {
	both := h
	h, err := historyFor(h, r)
	if err != nil {
		return nil, err
	}
	out := map[string]*prom.Series{}
	add := func(k string, l map[string]string, smps ...prom.Sample) {
		s, ok := out[k]
		if !ok {
			s = &prom.Series{Labels: l}
			out[k] = s
		}
		s.Samples = append(s.Samples, smps...)
	}
	result := func() []prom.Series {
		res := make([]prom.Series, 0, len(out))
		for _, k := range slices.Sorted(maps.Keys(out)) {
			res = append(res, *out[k])
		}
		return res
	}
	if r.Graph == nil {
		for _, c := range chunks(from, to, step) {
			a, b, ok := evaluations(r, c, step, to)
			if !ok {
				continue
			}
			part, err := shifted(ctx, h, r.Expr, a, b, step, r.Lag())
			if err != nil {
				return nil, err
			}
			for _, s := range part {
				add(key(s.Labels), s.Labels, s.Samples...)
			}
		}
		return result(), nil
	}
	g := r.Graph
	refs := asked(g, values)
	// The rule's history in two places at once: a query beside its
	// condition, on a Loki, is asked of that Loki.
	sides := map[string]History{}
	for _, ref := range refs {
		if uid := g.Aside[ref]; uid != "" && sides[uid] == nil {
			side, err := asideFor(both, r, uid)
			if err != nil {
				return nil, err
			}
			sides[uid] = side
		}
	}
	historyOf := func(ref string) History {
		if side := sides[g.Aside[ref]]; side != nil {
			return side
		}
		return h
	}
	under := map[string]bool{}
	for _, ref := range queriesUnder(g, g.Cond) {
		under[ref] = true
	}
	// past keeps what each reduce saw, by series, for windows that span
	// several steps, and days.
	type point struct {
		t int64
		v float64
	}
	past := map[string]map[string][]point{}
	var reduces []string
	for _, ref := range slices.Sorted(maps.Keys(g.Steps)) {
		if g.Steps[ref].Reduce != nil {
			reduces = append(reduces, ref)
		}
	}
	every := Every(r, step)
	phase := from.UnixNano() % int64(step)
	// Grafana's alerts, worked out from before from, so the evaluations
	// from it find those Grafana would have had.
	start := grafanaStart(r, from, step, warm && !values).Add(warmup(g, step))
	var held *grafanaStates
	var known *dataSeen
	if !values {
		held = newStates(r, every)
		known = &dataSeen{probed: map[string]bool{}, seen: map[string]bool{}}
	}
	firsts := map[int]*windowFirsts{}
	var failed error
	for _, c := range chunks(start.Add(-warmup(g, step)), to, step) {
		// The queries of one day are asked for together.
		answers := make([][]prom.Series, len(refs))
		errs := make([]error, len(refs))
		var wg sync.WaitGroup
		// LogQL reduces of one query's points are answered from one range:
		// the first of each such group asks for all.
		sameLogs := map[string][]int{}
		for i, ref := range refs {
			if a := g.Asks[ref]; a.Logs {
				k := logSame(a, max(g.Shift[ref], r.Late), g.Aside[ref])
				sameLogs[k] = append(sameLogs[k], i)
			}
		}
		for i, ref := range refs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if a := g.Asks[ref]; a.Logs {
					lag := max(g.Shift[ref], r.Late)
					group := sameLogs[logSame(a, lag, g.Aside[ref])]
					if group[0] != i {
						return
					}
					as := make([]rules.Ask, len(group))
					for n, j := range group {
						as[n] = g.Asks[refs[j]]
					}
					got, err := logReduceMany(ctx, historyOf(ref), as, c[0], c[1], step, lag)
					for n, j := range group {
						if err != nil {
							errs[j] = err
						} else {
							answers[j] = got[n]
						}
					}
					return
				}
				expr, _ := g.Expr(ref, r.AskStep(ref))
				answers[i], errs[i] = shifted(ctx, historyOf(ref), expr, c[0], c[1], step, max(g.Shift[ref], r.Late))
			}()
		}
		wg.Wait()
		// A query the condition reads that fails fails the rule. One
		// outside it the server refuses as written (400) returned nothing
		// to Grafana every time, which takes it as no data, as Grafana
		// 13.3.1 was seen to; one that fails otherwise is taken as not
		// known to have returned anything or not.
		unasked, refused := map[string]bool{}, map[string]bool{}
		for i, err := range errs {
			var api *prom.APIError
			switch {
			case err == nil:
			case under[refs[i]] || ctx.Err() != nil:
				return nil, err
			case errors.As(err, &api) && api.Status == http.StatusBadRequest && g.Aside[refs[i]] == "":
				// (A Loki query beside the condition that is refused is an
				// error to Grafana, not no data: not known.)
				refused[refs[i]] = true
			default:
				unasked[refs[i]] = true
				for ref, d := range g.Data {
					if d.Present == refs[i] {
						noteFailed(ctx, ref)
					}
				}
			}
		}
		data := map[string]map[int64]sse.Set{}
		for i, ref := range refs {
			series := answers[i]
			byT := map[int64]sse.Set{}
			for _, s := range series {
				l := maps.Clone(s.Labels)
				delete(l, "__name__")
				k := key(l)
				for _, smp := range s.Samples {
					t := smp.T.Unix()
					byT[t] = append(byT[t], sse.Item{Labels: l, Key: k, V: smp.V})
				}
			}
			data[ref] = byT
		}
		for at := c[0]; !at.After(c[1]); at = at.Add(step) {
			t := at.Unix()
			memo := map[string]sse.Set{}
			var eval func(ref string) sse.Set
			eval = func(ref string) sse.Set {
				if s, ok := memo[ref]; ok {
					return s
				}
				var s sse.Set
				switch st, isStep := g.Steps[ref]; {
				case !isStep:
					s = data[ref][t]
				case st.Threshold != nil:
					for _, it := range eval(st.Threshold.Input) {
						v := 0.0
						if st.Threshold.Pass(it.V) {
							v = 1
						}
						s = append(s, sse.Item{Labels: it.Labels, Key: it.Key, V: v})
					}
				case st.Reduce != nil:
					rd := st.Reduce
					if past[ref] == nil {
						past[ref] = map[string][]point{}
					}
					for _, it := range eval(rd.Input) {
						k := it.Key
						if k == "" {
							k = key(it.Labels)
						}
						pts := append(past[ref][k], point{t, it.V})
						for len(pts) > 0 && pts[0].t <= t-int64(rd.Window/time.Second) && rd.Window > 0 {
							pts = pts[1:]
						}
						if rd.Window == 0 {
							pts = pts[len(pts)-1:]
						}
						past[ref][k] = pts
						vs := make([]float64, len(pts))
						for i, p := range pts {
							vs[i] = p.v
						}
						s = append(s, sse.Item{Labels: it.Labels, Key: it.Key, V: reduce(rd.Fn, vs)})
					}
				case st.Classic != nil:
					var err error
					if s, err = classicAt(ctx, h, r, st.Classic, at, eval, firsts); err != nil && failed == nil {
						failed = err
					}
				default:
					s = st.Math.Eval(func(r string) (sse.Set, bool) { return eval(r), true })
				}
				memo[ref] = s
				return s
			}
			// The steps before start are for reduces to see whole windows;
			// a rule is evaluated only every so often.
			if at.Before(start) || (at.UnixNano()-phase)%int64(every) != 0 {
				for _, ref := range reduces {
					eval(ref)
				}
				continue
			}
			cond := eval(g.Cond)
			if failed != nil {
				return nil, failed
			}
			if values {
				for _, it := range cond {
					if it.V == 0 || math.IsNaN(it.V) {
						continue
					}
					k := it.Key
					if k == "" {
						k = key(it.Labels)
					}
					add(k, it.Labels, prom.Sample{T: at, V: it.V})
				}
				continue
			}
			noData, unknown, err := known.noData(ctx, historyOf, r, data, unasked, refused, at)
			if err != nil {
				return nil, err
			}
			var now []heldAt
			if unknown && len(noData) == 0 && len(cond) == 0 {
				// Nothing to say: a query the condition reads has no
				// history yet. The alerts stay as they are.
				now = held.firing()
			} else {
				now = held.eval(at, cond, noData)
			}
			if !at.Before(from) {
				for _, s := range now {
					add(s.key, s.labels, prom.Sample{T: at, V: 1})
				}
			}
		}
	}
	return result(), nil
}

// dataSeen is, for one replay of a Grafana rule, which of its data source
// queries have returned something: in the evaluations so far, or in the
// day before the first that returned nothing.
type dataSeen struct {
	seen, probed map[string]bool
}

// noData is, when a data source query of r returned nothing at at, the
// labels of Grafana's NoData result: for each data source, its uid, and the
// refIds of its queries that returned nothing. A query is taken to have
// returned nothing only once it has returned something, in this replay or
// in the day before: before then, the history may simply not be there
// (before the server's retention, before the metric was, or on another
// data source than the one read), and Grafana's NoData can't be told from
// it. unknown says such a query returned nothing. A question about the day
// before that fails fails the replay, as a query does.
func (k *dataSeen) noData(ctx context.Context, historyOf func(string) History, r rules.Rule, data map[string]map[int64]sse.Set, failed, refused map[string]bool, at time.Time) (out []map[string]string, unknown bool, err error) {
	g := r.Graph
	t := at.Unix()
	byUID := map[string][]string{}
	for _, ref := range slices.Sorted(maps.Keys(g.Data)) {
		d := g.Data[ref]
		if failed[d.Present] {
			unknown = true
			continue
		}
		if refused[d.Present] {
			byUID[d.UID] = append(byUID[d.UID], ref)
			continue
		}
		if len(data[d.Present][t]) > 0 {
			k.seen[ref] = true
			continue
		}
		if !k.seen[ref] && !k.probed[ref] {
			k.probed[ref] = true
			if k.seen[ref], err = before(ctx, historyOf(d.Present), r, ref, at); err != nil {
				return nil, false, err
			}
		}
		if !k.seen[ref] {
			unknown = true
			continue
		}
		byUID[d.UID] = append(byUID[d.UID], ref)
	}
	for _, uid := range slices.Sorted(maps.Keys(byUID)) {
		out = append(out, map[string]string{"datasource_uid": uid, "ref_id": strings.Join(byUID[uid], ",")})
	}
	return out, unknown, nil
}

// ProbeStep is how finely the day before is looked through: as often as
// the rule is evaluated, a minute at most, so a query that returned
// something for only a minute, between two of a coarser grid, is seen.
// The backtest port holds a pull request's rule to the points it makes.
func ProbeStep(r rules.Rule) time.Duration {
	if r.Interval <= 0 || r.Interval > time.Minute {
		return time.Minute
	}
	// A second at the finest, whatever the rule says: a day is 86,400
	// points a series then, and a group's interval of a nanosecond is no
	// reason to ask for more.
	return max(r.Interval, time.Second)
}

// probe is the question before asks: when, in the day before, the query
// last returned anything; nothing when it didn't.
func probe(expr string, step time.Duration) string {
	return "max(max_over_time(timestamp((" + expr + "))[1d:" + promStep(step) + "]))"
}

// A seenMemo keeps, for each probe asked of one Prometheus, the last time
// its query returned anything, so a later replay, a serve run an hour on,
// needn't ask again while that is still within the day. It is let go whole
// when full.
type seenMemo struct {
	sync.Mutex
	m map[string]time.Time
}

const maxSeenMemo = 10_000

var memos sync.Map // *prom.Client to its *seenMemo

// memoOf is the memo of the Prometheus h asks, nil when h isn't one.
func memoOf(h History) *seenMemo {
	if a, ok := h.(*asker); ok {
		h = a.h
	}
	c, ok := h.(*prom.Client)
	if !ok {
		return nil
	}
	m, _ := memos.LoadOrStore(c, &seenMemo{m: map[string]time.Time{}})
	return m.(*seenMemo)
}

// before reports whether a data source query returned anything in the day
// before at.
func before(ctx context.Context, h History, r rules.Rule, ref string, at time.Time) (bool, error) {
	expr, ok := r.Graph.Queries[ref]
	if !ok {
		return false, nil
	}
	when := at.Add(-max(r.Graph.Shift[ref], r.Late))
	q := probe(expr, ProbeStep(r))
	from := when
	logs := r.Logs != "" || r.Graph.Aside[ref] != ""
	if logs {
		// LogQL has no subqueries: the query itself, over the day, at the
		// probe's step, is asked whether it returned anything.
		q, from = expr, when.Add(-24*time.Hour)
	}
	memo := memoOf(h)
	if memo != nil {
		memo.Lock()
		last, ok := memo.m[q]
		memo.Unlock()
		if ok && last.After(when.Add(-24*time.Hour)) && !last.After(when) {
			return true, nil
		}
	}
	var series []prom.Series
	var err error
	pstep := time.Minute
	if logs {
		pstep = ProbeStep(r)
	}
	if a, ok := h.(*asker); ok {
		series, err = a.probe(ctx, q, from, when, pstep)
	} else {
		series, err = h.QueryRange(ctx, q, from, when, pstep)
	}
	if err != nil {
		return false, err
	}
	if logs {
		// The last time the query returned anything, as the probe's
		// answer would be for PromQL.
		var last time.Time
		for _, s := range series {
			for _, smp := range s.Samples {
				if !math.IsNaN(smp.V) && smp.T.After(last) {
					last = smp.T
				}
			}
		}
		if last.IsZero() {
			return false, nil
		}
		series = []prom.Series{{Samples: []prom.Sample{{T: when, V: float64(last.UnixMilli()) / 1e3}}}}
	}
	for _, s := range series {
		for _, smp := range s.Samples {
			if math.IsNaN(smp.V) || math.IsInf(smp.V, 0) {
				continue
			}
			if memo != nil {
				// timestamp() is in seconds, to the millisecond.
				seen := time.UnixMilli(int64(math.Round(smp.V * 1000)))
				memo.Lock()
				if len(memo.m) >= maxSeenMemo {
					clear(memo.m)
				}
				if seen.After(memo.m[q]) {
					memo.m[q] = seen
				}
				memo.Unlock()
			}
			return true, nil
		}
	}
	return false, nil
}

// classicAt is what classic conditions come to at one evaluation: a series
// with no labels, 1 when they hold, 0 when not, none when Grafana would say
// there was no data; or, for c.Of, the value that condition compares.
func classicAt(ctx context.Context, h History, r rules.Rule, c *rules.Classic, at time.Time, eval func(string) sse.Set,
	firsts map[int]*windowFirsts) (sse.Set, error) {
	var failed error
	series := func(i int) []rules.Reduced {
		cd := c.Conds[i]
		byKey := func(ref string) map[string]float64 {
			m := map[string]float64{}
			if ref == "" {
				return m
			}
			for _, it := range eval(ref) {
				k := it.Key
				if k == "" {
					k = key(it.Labels)
				}
				m[k] = it.V
			}
			return m
		}
		values, oldest, points := byKey(cd.Value), byKey(cd.Oldest), byKey(cd.Points)
		var out []rules.Reduced
		for _, it := range eval(cd.Present) {
			k := it.Key
			if k == "" {
				k = key(it.Labels)
			}
			v, ok := values[k]
			o, has := oldest[k]
			if cd.Diff() && ok && !has {
				// No number at the window's start: Grafana takes the first
				// in the window, which only the window's points say.
				w := firsts[i]
				if w == nil {
					w = &windowFirsts{}
					firsts[i] = w
				}
				var err error
				if o, has, err = w.first(ctx, h, r, cd, at, k); err != nil && failed == nil {
					failed = err
				}
			}
			out = append(out, cd.Reduce(rules.Reduced{V: v, OK: ok}, rules.Reduced{V: o, OK: has}, points[k] == 1))
		}
		return out
	}
	// One alert, with no labels of its own.
	one := func(v float64) sse.Set { return sse.Set{{Labels: map[string]string{}, V: v}} }
	if c.Of > 0 {
		v := c.ValueOf(c.Of-1, series)
		if failed != nil || math.IsNaN(v) {
			return nil, failed
		}
		return one(v), nil
	}
	firing, noData := c.Eval(series)
	switch {
	case failed != nil:
		return nil, failed
	case noData:
		return nil, nil
	case firing:
		return one(1), nil
	}
	return one(0), nil
}

// windowFirsts are, for a classic condition, the first numbers of its
// series' windows, as last asked for: a series that appeared within the
// window keeps its first number until the window has passed it, so it is
// asked for again only for a series not asked for yet, or once that has.
type windowFirsts struct {
	byKey map[string]firstAt
}

// firstAt is a series' first number in a window, and when it was; when
// none was found, when it was asked.
type firstAt struct {
	t     time.Time
	v     float64
	found bool
}

// first is the first number in the window ending at at of the series with
// key k, as Grafana's range query has it, asking for the window's points
// when it isn't known.
func (w *windowFirsts) first(ctx context.Context, h History, r rules.Rule, cd rules.ClassicCond, at time.Time, k string) (float64, bool, error) {
	lag := max(r.Graph.Shift[cd.Value], r.Late)
	end := at.Add(-lag)
	if f, ok := w.byKey[k]; ok && !f.t.Before(end.Add(-cd.Window)) {
		return f.v, f.found, nil
	}
	series, err := h.QueryRange(ctx, "(("+cd.Expr+") >= -Inf)", end.Add(-cd.Window), end, cd.Res)
	if err != nil {
		return 0, false, fmt.Errorf("the first numbers of %s's window: %w", cd.Query, err)
	}
	if w.byKey == nil {
		w.byKey = map[string]firstAt{}
	}
	for _, s := range series {
		l := maps.Clone(s.Labels)
		delete(l, "__name__")
		for _, smp := range s.Samples {
			if !math.IsNaN(smp.V) && !smp.T.Before(end.Add(-cd.Window)) && !smp.T.After(end) {
				w.byKey[key(l)] = firstAt{t: smp.T, v: smp.V, found: true}
				break
			}
		}
	}
	f, ok := w.byKey[k]
	if !ok || f.t.Before(end.Add(-cd.Window)) {
		// None found: not asked for again until a window from now.
		f = firstAt{t: end}
		w.byKey[k] = f
	}
	return f.v, f.found, nil
}

// asked are the refs a Grafana rule's evaluation asks the history for: the
// queries its condition depends on and, unless only its values are wanted,
// what says whether each data source query returned anything.
func asked(g *rules.Graph, values bool) []string {
	refs := queriesUnder(g, g.Cond)
	if !values {
		for _, d := range g.Data {
			if !slices.Contains(refs, d.Present) {
				refs = append(refs, d.Present)
			}
		}
	}
	sort.Strings(refs)
	return refs
}

// A Query is an expression a rule's condition asks for, and where: the
// history that takes PromQL when Logs is "", else the Loki it is asked of,
// by its Grafana data source's uid, or, for a rule written in LogQL that is
// not Grafana's, the mark of its Logs.
type Query struct {
	Expr, Logs string
}

// Queries are the expressions a rule's condition asks for, each once, as
// Condition asks them (PromQL as the history takes it, LogQL as written), and
// of which history: a Grafana rule's Loki query beside a Prometheus
// condition (Graph.Aside) goes to its Loki, as asideFor routes it. None for
// a rule that can't be replayed.
func Queries(r rules.Rule) []Query {
	if r.Unsupported != "" {
		return nil
	}
	if r.Graph == nil {
		if r.Expr == "" {
			return nil
		}
		return []Query{{Expr: r.Expr, Logs: r.Logs}}
	}
	var out []Query
	for _, ref := range asked(r.Graph, false) {
		expr, ok := r.Graph.Expr(ref, r.AskStep(ref))
		if !ok || expr == "" {
			continue
		}
		q := Query{Expr: expr, Logs: r.Logs}
		if uid := r.Graph.Aside[ref]; uid != "" {
			q.Logs = uid
		}
		if !slices.Contains(out, q) {
			out = append(out, q)
		}
	}
	return out
}

// queriesUnder lists the queries a node depends on.
func queriesUnder(g *rules.Graph, ref string) []string {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(r string) {
		if seen[r] {
			return
		}
		seen[r] = true
		if st, ok := g.Steps[r]; ok {
			for _, u := range st.Uses() {
				walk(u)
			}
		}
	}
	walk(ref)
	var out []string
	for r := range seen {
		if _, ok := g.Expr(r, 0); ok {
			if _, isStep := g.Steps[r]; !isStep {
				out = append(out, r)
			}
		}
	}
	sort.Strings(out)
	return out
}

// reduce collapses values the way Grafana's reducers do.
func reduce(fn string, vs []float64) float64 {
	if len(vs) == 0 {
		return math.NaN()
	}
	switch fn {
	case "last":
		return vs[len(vs)-1]
	case "count":
		return float64(len(vs))
	}
	acc := vs[0]
	for _, v := range vs[1:] {
		switch fn {
		case "max":
			acc = max(acc, v)
		case "min":
			acc = min(acc, v)
		default: // sum, mean
			acc += v
		}
	}
	if fn == "mean" {
		acc /= float64(len(vs))
	}
	return acc
}

func key(l map[string]string) string { return sse.LabelKey(l) }

// promStep is a step as PromQL writes it: whole seconds, or milliseconds.
func promStep(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return strconv.FormatInt(int64(d/time.Millisecond), 10) + "ms"
}
