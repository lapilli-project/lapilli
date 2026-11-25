package routing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// never stands for a time that doesn't come: when an alert that is still
// firing ends.
var never = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// An inst is an alert as the run holds it: its labels with the external
// ones added, the label set's number (alerts of one label set are one alert
// to Alertmanager), and when it started and ended.
type inst struct {
	labels     map[string]string
	fp         int
	start, end time.Time
}

type runner struct {
	c     *Config
	o     Options
	in    []inst
	until time.Time
	res   *Result

	fps    map[string]int // label sets, by their canonical text
	fpLab  []map[string]string
	fpOwn  []int // the extra route a label set goes along, -1 for the tree
	extra  []*node
	owns   map[string]int
	routes [][]int // by label set; nil until worked out
	sils   [][]int // the silences each label set matches
	silsOK []bool
	silIdx *silenceIndex
	inh    *inhibitor

	ctx      context.Context
	left     int // the work units left
	budget   int
	stopped  bool
	warnings []string

	raw    []Notification
	muting []map[muteKey]*Muting // by alert
	order  [][]muteKey
}

type muteKey struct {
	route int
	kind  MuteKind
	by    string
}

// Run works out what the alert manager does with the alerts: the routes they
// reach, the groups they make, when each group's integrations send, and
// what mutes which alert. The alerts and options aren't changed. An error is
// an alert without a start; whatever else is wrong with a configuration was
// said by NewConfig.
func (c *Config) Run(alerts []Alert, o Options) (*Result, error) {
	return c.RunContext(context.Background(), alerts, o)
}

// RunContext is Run that gives up when ctx ends, or when the run has spent
// Options.Budget: it returns what is worked out so far, with Result.Stopped
// saying why. An error is for what can't be run at all: no configuration, an
// alert without a start, more than MaxSilences silences.
func (c *Config) RunContext(ctx context.Context, alerts []Alert, o Options) (*Result, error) {
	if c == nil {
		return nil, errors.New("no configuration")
	}
	if len(o.Silences) > MaxSilences {
		return nil, fmt.Errorf("more than %d silences", MaxSilences)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	budget := o.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	r := &runner{c: c, o: o, ctx: ctx, left: budget, budget: budget, fps: map[string]int{}, owns: map[string]int{}, res: &Result{Alerts: make([]AlertResult, len(alerts))}}
	r.in = make([]inst, len(alerts))
	latest := time.Time{}
	for i, a := range alerts {
		if a.Start.IsZero() {
			return nil, fmt.Errorf("alert %d has no start", i)
		}
		if i%1024 == 0 && !r.alive() {
			break
		}
		labels := make(map[string]string, len(a.Labels)+len(o.ExternalLabels))
		maps.Copy(labels, o.ExternalLabels)
		maps.Copy(labels, a.Labels)
		x := inst{labels: labels, start: a.Start, end: never}
		if !a.End.IsZero() {
			x.end = a.End
			if x.end.Before(x.start) {
				x.end = x.start
			}
			if x.end.After(latest) {
				latest = x.end
			}
		}
		if a.Start.After(latest) {
			latest = a.Start
		}
		own := r.own(a.Own)
		key := canonical(labels)
		if own >= 0 {
			key += "\xfe" + strconv.Itoa(own)
		}
		id, ok := r.fps[key]
		if !ok {
			id = len(r.fpLab)
			r.fps[key] = id
			r.fpLab = append(r.fpLab, labels)
			r.fpOwn = append(r.fpOwn, own)
		}
		x.fp = id
		r.in[i] = x
	}
	r.until = o.Until
	if r.until.IsZero() {
		r.until = latest
	}
	r.routes = make([][]int, len(r.fpLab))
	r.sils = make([][]int, len(r.fpLab))
	r.silsOK = make([]bool, len(r.fpLab))
	r.muting = make([]map[muteKey]*Muting, len(alerts))
	r.order = make([][]muteKey, len(alerts))
	if len(c.inhibit) > 0 {
		r.inh = newInhibitor(r)
	}
	if !r.stopped {
		r.group()
	}
	r.finish()
	return r.res, nil
}

// alive reports whether the run goes on: it hasn't stopped, and its context
// hasn't ended.
func (r *runner) alive() bool {
	if r.stopped {
		return false
	}
	if err := r.ctx.Err(); err != nil {
		r.stop("canceled before it was done (" + err.Error() + "): what is worked out is of the groups before that")
		return false
	}
	return true
}

// spend takes n work units, and reports whether the run may go on; past its
// budget it stops, and says so.
func (r *runner) spend(n int) bool {
	if r.stopped {
		return false
	}
	if r.left -= n; r.left < 0 {
		r.stop(fmt.Sprintf("it spent its budget of %d work units (a notification is 50, a flush is 1 and each alert it holds, 1): what is worked out is of the groups before that", r.budget))
		return false
	}
	return true
}

// stop ends the run, saying why; the first reason is kept with what came
// before it.
func (r *runner) stop(why string) {
	if r.stopped {
		return
	}
	r.stopped = true
	if r.res.Stopped != "" {
		why = r.res.Stopped + "; " + why
	}
	r.res.Stopped = why
}

// warn notes what the model changed, once each, twenty at most.
func (r *runner) warn(msg string) {
	if len(r.warnings) < 20 && !slices.Contains(r.warnings, msg) {
		r.warnings = append(r.warnings, clipTo(msg, 300))
	}
}

// node is a route of the tree, or one of the routes made for rules that name
// their own contact point, numbered after the tree's.
func (r *runner) node(i int) *node {
	if i < len(r.c.nodes) {
		return r.c.nodes[i]
	}
	return r.extra[i-len(r.c.nodes)]
}

// own makes, once for each distinct set of settings, the route Grafana makes
// for a rule that names its own contact point, and gives its number; -1 when
// the alert has none, or names a contact point the configuration doesn't
// have (the policies then say where it goes).
func (r *runner) own(o *OwnRoute) int {
	if o == nil || o.Receiver == "" {
		return -1
	}
	if _, ok := r.c.receivers[o.Receiver]; !ok {
		return -1
	}
	groupBy := NormalizedGroupBy(o.GroupBy)
	root := r.c.root
	n := &node{parent: root, receiver: o.Receiver, groupBy: groupBy, groupWait: root.groupWait, groupInterval: root.groupInterval,
		repeatInterval: root.repeatInterval, mute: dedupe(o.MuteTimeIntervals), active: dedupe(o.ActiveTimeIntervals)}
	if len(groupBy) == 1 && groupBy[0] == "..." {
		n.groupBy, n.groupByAll = nil, true
	}
	// An explicit 0s is zero, a timing left out the root's, one under a
	// second (but zero) a second, as Config's are, said.
	timing := func(name string, p *time.Duration, inherited time.Duration, zeroOK bool) time.Duration {
		switch {
		case p == nil:
			return inherited
		case *p == 0 && zeroOK:
			return 0
		case *p < MinInterval:
			r.warn(fmt.Sprintf("a rule's %s of %s for contact point %q is read as %s: Lapilli doesn't model timings under %s", name, *p, clipText(o.Receiver), MinInterval, MinInterval))
			return MinInterval
		}
		return *p
	}
	n.groupWait = timing("group_wait", o.GroupWait, n.groupWait, true)
	n.groupInterval = timing("group_interval", o.GroupInterval, n.groupInterval, false)
	n.repeatInterval = timing("repeat_interval", o.RepeatInterval, n.repeatInterval, false)
	for _, name := range append(slices.Clone(n.mute), n.active...) {
		if _, ok := r.c.intervals[name]; !ok {
			return -1
		}
	}
	n.key = ownKey(o.Receiver)
	id := n.key + "|" + strings.Join(groupBy, ",") + "|" + n.groupWait.String() + "|" + n.groupInterval.String() + "|" +
		n.repeatInterval.String() + "|" + strings.Join(n.mute, ",") + "|" + strings.Join(n.active, ",")
	if i, ok := r.owns[id]; ok {
		return i
	}
	sum := sha256.Sum256([]byte(id))
	n.short = "g" + hex.EncodeToString(sum[:4])
	i := len(r.c.nodes) + len(r.extra)
	n.index = i
	r.extra = append(r.extra, n)
	r.owns[id] = i
	return i
}

// ownKey is the route Grafana makes for a rule's own contact point, as
// Alertmanager writes a route's key.
func ownKey(receiver string) string {
	return "{__grafana_autogenerated__=\"true\", __grafana_receiver__=" + strconv.Quote(receiver) + "}"
}

// NormalizedGroupBy is a Grafana rule's group_by as Grafana normalizes it
// (NotificationSettings.NormalizedGroupBy): grafana_folder and alertname
// always, any others after them, and a list with ... only that. Left out, it
// is those two, which is how Grafana groups a rule that names no labels.
func NormalizedGroupBy(by []string) []string {
	if len(by) == 0 {
		return slices.Clone(DefaultOwnGroupBy)
	}
	if slices.Contains(by, "...") {
		return []string{"..."}
	}
	seen := map[string]bool{}
	for _, l := range DefaultOwnGroupBy {
		seen[l] = true
	}
	custom := []string{}
	for _, l := range by {
		if !seen[l] {
			seen[l] = true
			custom = append(custom, l)
		}
	}
	// The model keeps group_by sorted, as Alertmanager's does.
	out := slices.Concat(DefaultOwnGroupBy, custom)
	sort.Strings(out)
	return out
}

// canonical writes a label set as text, the same for the same labels.
func canonical(labels map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		b.WriteString(k)
		b.WriteByte(0xff)
		b.WriteString(labels[k])
		b.WriteByte(0xff)
	}
	return b.String()
}

// labelSet writes labels as Alertmanager writes a label set in a group's
// key: {a="b", c="d"}.
func labelSet(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		parts = append(parts, k+"="+strconv.Quote(labels[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// maxGroupText is how many bytes of a group's labels its key keeps; a longer
// one is cut and ends in a hash of the whole, so two groups are still two.
const maxGroupText = 1000

func groupText(labels string) string {
	if len(labels) <= maxGroupText {
		return labels
	}
	sum := sha256.Sum256([]byte(labels))
	return strings.ToValidUTF8(labels[:maxGroupText], "") + "#" + hex.EncodeToString(sum[:8])
}

// routesOf are the routes a label set is sent to, worked out once.
func (r *runner) routesOf(fp int) []int {
	if r.routes[fp] == nil && r.fpOwn[fp] >= 0 {
		r.routes[fp] = []int{r.fpOwn[fp]}
	}
	if r.routes[fp] == nil {
		r.spend(len(r.c.nodes))
		r.routes[fp] = r.c.Match(r.fpLab[fp])
		if r.routes[fp] == nil {
			r.routes[fp] = []int{}
		}
	}
	return r.routes[fp]
}

type group struct {
	route   int
	labels  string
	members []int
}

// group makes the groups: each alert goes to each route it matches, into the
// group of its values of the route's group_by, and each group is worked out.
func (r *runner) group() {
	type gk struct {
		route  int
		labels string
	}
	groups := map[gk]*group{}
	var list []*group
	for i, x := range r.in {
		if i%1024 == 0 && !r.alive() {
			break
		}
		res := &r.res.Alerts[i]
		seen := map[string]bool{}
		if !r.spend(1 + len(r.routesOf(x.fp))) {
			break
		}
		for _, ri := range r.routesOf(x.fp) {
			n := r.node(ri)
			res.Routes = append(res.Routes, ri)
			if !seen[n.receiver] {
				seen[n.receiver] = true
				res.Receivers = append(res.Receivers, n.receiver)
				if len(r.c.receivers[n.receiver].Integrations) == 0 {
					res.Empty = append(res.Empty, n.receiver)
				}
			}
			if len(r.c.receivers[n.receiver].Integrations) == 0 {
				continue // it reached a receiver that sends nothing
			}
			gl := map[string]string{}
			if n.groupByAll {
				gl = x.labels
			} else {
				for _, ln := range n.groupBy {
					if v, ok := x.labels[ln]; ok {
						gl[ln] = v
					}
				}
			}
			key := gk{ri, groupText(labelSet(gl))}
			g := groups[key]
			if g == nil {
				if len(groups) >= MaxGroups {
					r.res.Stopped = fmt.Sprintf("more than %d groups: the notifications of those after the first %d aren't worked out", MaxGroups, MaxGroups)
					continue
				}
				g = &group{route: ri, labels: key.labels}
				groups[key] = g
				list = append(list, g)
			}
			g.members = append(g.members, i)
		}
	}
	slices.SortFunc(list, func(a, b *group) int {
		if a.route != b.route {
			return a.route - b.route
		}
		return strings.Compare(a.labels, b.labels)
	})
	r.res.Groups = len(list)
	for _, g := range list {
		if !r.alive() {
			return
		}
		sort.SliceStable(g.members, func(a, b int) bool {
			x, y := r.in[g.members[a]], r.in[g.members[b]]
			if !x.start.Equal(y.start) {
				return x.start.Before(y.start)
			}
			return g.members[a] < g.members[b]
		})
		r.simGroup(g)
	}
}

// A log is what the notification log holds for a group and an integration:
// the alerts the last notification had firing and resolved, and when it was
// logged.
type log struct {
	has              bool
	firing, resolved map[int]bool
	ts               time.Time
}

func subset(a, of map[int]bool) bool {
	for k := range a {
		if !of[k] {
			return false
		}
	}
	return true
}

// needsUpdate is notify.DedupStage.needsUpdate.
func needsUpdate(l log, firing, resolved map[int]bool, repeat time.Duration, sendResolved bool, now time.Time) (Reason, bool) {
	// Not notified of the group before: notify unless only resolved alerts.
	if !l.has {
		if len(firing) > 0 {
			return ReasonFirst, true
		}
		return "", false
	}
	// New alerts in the group.
	if !subset(firing, l.firing) {
		// If the one before had none firing it was a resolution, and this
		// is the group's first again.
		if len(l.firing) == 0 {
			return ReasonFirst, true
		}
		return ReasonNew, true
	}
	// All resolved: told whether or not the integration sends resolves, so
	// the firing alerts are cleared from the log.
	if len(firing) == 0 {
		if len(l.firing) > 0 {
			return ReasonResolved, true
		}
		return "", false
	}
	if sendResolved && !subset(resolved, l.resolved) {
		return ReasonResolved, true
	}
	// Nothing changed: only if the repeat interval has passed.
	if l.ts.Before(now.Add(-repeat)) {
		return ReasonRepeat, true
	}
	return "", false
}

// simGroup works out one group, by the events in its life: an alert coming,
// an alert resolving, something muting or unmuting one of them, a repeat
// falling due. Between them it jumps to the flush that comes next, which is
// the group's own timer's: group_wait after its first alert, then each
// group_interval after the one before (dispatch.aggrGroup.run).
func (r *runner) simGroup(g *group) {
	n := r.node(g.route)
	ints := r.c.receivers[n.receiver].Integrations
	step := n.groupInterval + r.o.TimerLate
	logs := make([]log, len(ints))
	gkey := n.short + ":" + g.labels
	cur := map[int]int{} // the alert held for each label set, by position
	next := 0
	live := false
	first := true
	var f, c, e time.Time // when the timer fired, when its flush began, when it passed the gossip's settling
	for {
		if !live {
			if next >= len(g.members) {
				return
			}
			s := r.in[g.members[next]].start
			if s.After(r.until) {
				return
			}
			live = true
			clear(cur)
			f = s.Add(n.groupWait)
			c = f
			// A group whose first alert was on before the period began was
			// notified before it: its flushes aren't firsts, and a repeat
			// is due a repeat_interval after the period's start.
			if first && !r.o.From.IsZero() && !s.After(r.o.From) {
				seed := map[int]bool{}
				for _, i := range g.members {
					if x := r.in[i]; !x.start.After(r.o.From) && x.end.After(r.o.From) {
						seed[x.fp] = true
					}
				}
				for k := range logs {
					logs[k] = log{has: true, firing: maps.Clone(seed), resolved: map[int]bool{}, ts: r.o.From}
				}
			}
			first = false
		}
		if c.After(r.until) {
			return
		}
		if !r.alive() {
			return
		}
		e = c
		if e.Before(r.o.Settled) {
			e = r.o.Settled
		}
		// The alerts the flush has: those that came, the latest of each
		// label set.
		for next < len(g.members) && !r.in[g.members[next]].start.After(c) {
			i := g.members[next]
			cur[r.in[i].fp] = i
			next++
		}
		held := make([]int, 0, len(cur))
		for _, i := range cur {
			held = append(held, i)
		}
		slices.Sort(held)
		if !r.spend(1 + len(held)) {
			return
		}
		resolved := func(i int) bool { return !r.in[i].end.After(c) }

		// The pipeline's mute stages, in its order: inhibition, active time
		// intervals, mute time intervals, silences.
		var remaining []int
		for _, i := range held {
			if rule, src, ok := r.inhibited(i, e); ok {
				by := r.c.inhibit[rule].Name
				if by == "" {
					by = "#" + strconv.Itoa(rule+1)
				}
				r.mute(i, g.route, MutedInhibition, by, rule, src, e)
				continue
			}
			remaining = append(remaining, i)
		}
		if len(remaining) > 0 && len(n.active) > 0 {
			in := false
			for _, name := range n.active {
				if containsAny(r.c.intervals[name], e) {
					in = true
					break
				}
			}
			if !in {
				for _, i := range remaining {
					r.mute(i, g.route, MutedActiveTime, strings.Join(n.active, ","), -1, -1, e)
				}
				remaining = nil
			}
		}
		if len(remaining) > 0 && len(n.mute) > 0 {
			var by []string
			for _, name := range n.mute {
				if containsAny(r.c.intervals[name], e) {
					by = append(by, name)
				}
			}
			if len(by) > 0 {
				for _, i := range remaining {
					r.mute(i, g.route, MutedMuteTime, strings.Join(by, ","), -1, -1, e)
				}
				remaining = nil
			}
		}
		if len(remaining) > 0 && len(r.o.Silences) > 0 {
			var left []int
			for _, i := range remaining {
				if id, ok := r.silenced(i, e); ok {
					r.mute(i, g.route, MutedSilence, id, -1, -1, e)
					continue
				}
				left = append(left, i)
			}
			remaining = left
		}

		// What is left goes to each integration, which tells when its log
		// says it should (notify.DedupStage), and logs it.
		if len(remaining) > 0 {
			firing, done := map[int]bool{}, map[int]bool{}
			var firingAt, doneAt []int
			for _, i := range remaining {
				if resolved(i) {
					done[r.in[i].fp] = true
					doneAt = append(doneAt, i)
				} else {
					firing[r.in[i].fp] = true
					firingAt = append(firingAt, i)
				}
			}
			for k, ig := range ints {
				reason, ok := needsUpdate(logs[k], firing, done, n.repeatInterval, ig.SendResolved, f)
				if !ok {
					continue
				}
				// An integration that doesn't send resolves isn't sent a
				// notification of resolved alerts alone, but the log has it as
				// sent (notify.RetryStage), so it won't be told again.
				if ig.SendResolved || len(firing) > 0 {
					if !r.spend(50 + len(firingAt) + len(doneAt)) {
						return
					}
					paging, page := r.pages(n.receiver, ig.Kind, reason)
					nt := Notification{At: e, Receiver: n.receiver, Kind: ig.Kind, Index: ig.Index,
						Paging: paging, Page: page,
						Reason: reason, Route: g.route, Group: gkey, Firing: slices.Clone(firingAt)}
					if ig.SendResolved {
						nt.Resolved = slices.Clone(doneAt)
					}
					r.raw = append(r.raw, nt)
				}
				logs[k] = log{has: true, firing: firing, resolved: done, ts: e}
			}
		}
		// The resolved alerts are dropped once the flush went through; a
		// group left with none is gone.
		for _, i := range held {
			if resolved(i) {
				delete(cur, r.in[i].fp)
			}
		}
		if len(cur) == 0 {
			live = false
			continue
		}

		// The next flush that could do anything: the first of the group's
		// own timer's at or after the next event that could change what
		// the flush finds.
		cand := never
		soon := func(t time.Time) {
			if t.Before(cand) {
				cand = t
			}
		}
		if next < len(g.members) {
			soon(r.in[g.members[next]].start)
		}
		for _, i := range cur {
			soon(r.in[i].end)
			soon(r.muteChange(i, n, e))
		}
		for k := range ints {
			// A repeat that is due already but wasn't sent is held back by a
			// mute, whose end is an event of its own.
			if due := logs[k].ts.Add(n.repeatInterval).Add(time.Nanosecond); logs[k].has && len(logs[k].firing) > 0 && due.After(c) {
				soon(due)
			}
		}
		for _, name := range slices.Concat(n.active, n.mute) {
			t, work, _ := nextChangeWork(r.c.intervals[name], e, r.until.Sub(e), max(r.left, 1))
			if !r.spend(work) {
				return
			}
			if !t.IsZero() {
				soon(t)
			}
		}
		if cand.After(r.until) {
			return
		}
		f = c.Add(step)
		c = f
		if e.After(c) {
			c = e
		}
		if c.Before(cand) {
			k := (cand.Sub(c) + step - 1) / step
			c = c.Add(k * step)
			f = c
		}
	}
}

// pages reports whether a notification to a receiver on an integration of a
// kind is on a paging integration, and whether it is a page: on a kind that
// keeps an open incident for the group (DedupsByGroup) only the notification
// that opens it is, unless --paging-receivers took the receiver as paging,
// which rings for every one.
func (r *runner) pages(receiver, kind string, reason Reason) (paging, page bool) {
	byName := r.o.PagingReceivers != nil && r.o.PagingReceivers.MatchString(receiver)
	paging = PagingKind(kind, r.c.grafana) || byName
	page = paging && (byName || !DedupsByGroup(kind) || reason == ReasonFirst)
	return paging, page
}

// mute notes that an alert was kept out of a flush.
func (r *runner) mute(i, route int, kind MuteKind, by string, rule, source int, at time.Time) {
	m := r.muting[i]
	if m == nil {
		m = map[muteKey]*Muting{}
		r.muting[i] = m
	}
	by = clipTo(by, MaxBy)
	k := muteKey{route, kind, by}
	x := m[k]
	if x == nil {
		x = &Muting{Kind: kind, Route: route, By: by, Rule: rule, Source: source, First: at}
		m[k] = x
		r.order[i] = append(r.order[i], k)
	}
	x.Last = at
}

// silenced reports the silence that mutes an alert at a time, the first of
// the silences given that does.
func (r *runner) silenced(i int, at time.Time) (string, bool) {
	for _, s := range r.silencesOf(r.in[i].fp) {
		if sl := r.o.Silences[s]; !at.Before(sl.Start) && at.Before(sl.End) {
			return sl.ID, true
		}
	}
	return "", false
}

// A silenceIndex finds the silences a label set could match without trying
// them all: each is kept under one of its = matchers (a name and a value that
// isn't empty), which a label set that has it must carry; those with none
// are tried against every label set.
type silenceIndex struct {
	by   map[[2]string][]int
	scan []int
}

func newSilenceIndex(sils []Silence) *silenceIndex {
	ix := &silenceIndex{by: map[[2]string][]int{}}
	for s, sl := range sils {
		at := -1
		for k, m := range sl.Matchers {
			if m.Op == OpEqual && m.Value != "" {
				at = k
				break
			}
		}
		if at < 0 {
			ix.scan = append(ix.scan, s)
			continue
		}
		key := [2]string{sl.Matchers[at].Name, sl.Matchers[at].Value}
		ix.by[key] = append(ix.by[key], s)
	}
	return ix
}

// silencesOf are the silences whose matchers a label set matches, by their
// position, whenever they are in force.
func (r *runner) silencesOf(fp int) []int {
	if !r.silsOK[fp] {
		r.silsOK[fp] = true
		if r.silIdx == nil {
			r.silIdx = newSilenceIndex(r.o.Silences)
		}
		labels := r.fpLab[fp]
		cand := slices.Clone(r.silIdx.scan)
		for k, v := range labels {
			cand = append(cand, r.silIdx.by[[2]string{k, v}]...)
		}
		r.spend(1 + len(labels) + len(cand))
		slices.Sort(cand)
		for _, s := range cand {
			if matchAll(r.o.Silences[s].Matchers, labels) {
				r.sils[fp] = append(r.sils[fp], s)
			}
		}
	}
	return r.sils[fp]
}

// muteChange is the first time after at when something that mutes an alert
// starts or stops: a silence that matches it, an alert that inhibits it.
// never if nothing does.
func (r *runner) muteChange(i int, n *node, at time.Time) time.Time {
	t := never
	for _, s := range r.silencesOf(r.in[i].fp) {
		sl := r.o.Silences[s]
		if sl.Start.After(at) && sl.Start.Before(t) {
			t = sl.Start
		}
		if sl.End.After(at) && sl.End.Before(t) {
			t = sl.End
		}
	}
	if r.inh != nil {
		if u := r.inh.change(i, at); u.Before(t) {
			t = u
		}
	}
	return t
}

// finish puts the run's notifications in order and gives each alert its
// part of them.
func (r *runner) finish() {
	sort.SliceStable(r.raw, func(a, b int) bool {
		x, y := r.raw[a], r.raw[b]
		if !x.At.Equal(y.At) {
			return x.At.Before(y.At)
		}
		if x.Receiver != y.Receiver {
			return x.Receiver < y.Receiver
		}
		if x.Kind != y.Kind {
			return KindOrder(x.Kind, y.Kind) < 0
		}
		if x.Index != y.Index {
			return x.Index < y.Index
		}
		return x.Group < y.Group
	})
	r.res.Notifications = r.raw
	r.res.Warnings = slices.Concat(r.c.warnings, r.warnings)
	for k, nt := range r.res.Notifications {
		for _, i := range nt.Firing {
			a := &r.res.Alerts[i]
			a.Notifications = append(a.Notifications, k)
			a.Notified++
			if a.FirstNotified.IsZero() {
				a.FirstNotified = nt.At
			}
			if nt.Paging {
				a.OnPaging++
			}
			if nt.Page {
				a.Paged++
				if a.FirstPage.IsZero() {
					a.FirstPage = nt.At
				}
			}
		}
	}
	for i := range r.res.Alerts {
		for _, k := range r.order[i] {
			r.res.Alerts[i].Muted = append(r.res.Alerts[i].Muted, *r.muting[i][k])
		}
	}
	// The receivers that page.
	for _, name := range r.c.names {
		p := Paging{Receiver: name}
		for _, ig := range r.c.receivers[name].Integrations {
			if PagingKind(ig.Kind, r.c.grafana) && !slices.Contains(p.Kinds, ig.Kind) {
				p.Kinds = append(p.Kinds, ig.Kind)
			}
		}
		slices.Sort(p.Kinds)
		p.ByPattern = r.o.PagingReceivers != nil && r.o.PagingReceivers.MatchString(name) && len(r.c.receivers[name].Integrations) > 0
		if len(p.Kinds) > 0 || p.ByPattern {
			r.res.Paging = append(r.res.Paging, p)
		}
	}
}
