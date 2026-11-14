package serve

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/schedule"
	"lapilli.dev/internal/verdict"
)

// The rule's page (docs/design/ui.md): a header with its badges, what to
// check about it and what can be done; five numbers over the page's period,
// each beside as long a span before it when the ledger holds one; its last
// 84 days, a cell a day; and its sections in five tabs, ?tab=.

// ruleTabs are the tabs of a rule's page, in order, each named by a text
// to translate. The overview is the one a page shows without tab=, or with
// one it doesn't have.
var ruleTabs = []struct{ id, name string }{
	{"overview", "Overview"}, {"firings", "Firings"}, {"versions", "Versions"}, {"proposals", "Proposals"}, {"activity", "Activity"},
}

// ruleTabOf is the tab a rule's page is asked for: one it has, or else the
// overview. What was asked is compared, never shown.
func ruleTabOf(q url.Values) string {
	v := q.Get("tab")
	for _, t := range ruleTabs {
		if t.id == v {
			return v
		}
	}
	return "overview"
}

// ruleWeeksOf is how many weeks a rule's page shows: weeks= 1 or 4, or else
// twelve, as it always has.
func ruleWeeksOf(q url.Values) int {
	switch q.Get("weeks") {
	case "1":
		return 1
	case "4":
		return 4
	}
	return weeks
}

// ruleTab is a tab of a rule's page: Param its tab=, "" for the overview;
// Count, when Counted, how many it holds.
type ruleTab struct {
	ID, Name, Param  string
	Count            int
	Counted, Current bool
}

// tabsOf are a rule's page's tabs, the one shown current, each counted
// where a count means something: its firings over the period, its
// versions, its proposals not finished with, its tasks and comments.
func tabsOf(v *ruleView) []ruleTab {
	open := 0
	for _, p := range v.Proposals {
		if slices.Contains(ledger.Unfinished(), p.State) {
			open++
		}
	}
	counts := map[string]int{"firings": v.Firings, "versions": len(v.Changes), "proposals": open, "activity": len(v.Tasks) + len(v.Comments.List)}
	var out []ruleTab
	for _, t := range ruleTabs {
		x := ruleTab{ID: t.id, Name: t.name, Current: t.id == v.Tab}
		if t.id != "overview" {
			x.Param = t.id
			x.Count, x.Counted = counts[t.id], true
		}
		out = append(out, x)
	}
	return out
}

// ruleActions are a rule's page's actions, in its header: propose a change
// (the proposals tab's forms), add a task (about it, for its team), edit
// its note (the overview's form, open). Those the reader may not use are
// disabled, and those disabled for the same reason share the one saying it.
func ruleActions(p page, v ruleView) []pageAction {
	edits := !p.Status.ReadOnly || v.TeamID > 0 && slices.Contains(p.Status.Teams, v.TeamID)
	as := []pageAction{mainAction(p, "Propose a change", !p.Status.ReadOnly, "editor", ""),
		mainAction(p, "Add a task", edits, "editor", ""), mainAction(p, "Edit the note", edits, "editor", "")}
	to := []string{p.Link(v.Path+"#propose", "tab", "proposals"),
		p.Link("/tasks", "rule", v.Name, "kind", "tune", "title", tr(p.Lang, "Tune %s", v.Name), "team", v.Team),
		p.Link(v.Path+"#note", "edit", "note")}
	ids := map[string]string{}
	for i := range as {
		as[i].Quiet = i > 0
		switch id, ok := ids[as[i].Why]; {
		case as[i].Why == "":
			as[i].Href = to[i]
		case ok:
			as[i].ID = id
		default:
			as[i].ID, as[i].Said = fmt.Sprintf("rule-why-%d", len(ids)+1), true
			ids[as[i].Why] = as[i].ID
		}
	}
	return as
}

// ruleCond is something to check about a rule, as its header lists them:
// Kind says what, N and Text what that needs.
type ruleCond struct {
	Kind string // facts, differs, unreplayed, unseen, alarm, untied, recorded, stale
	N    int
	Text string
}

// condsOf are what to check about a rule: problems of fact in what it says;
// its replay differing from what really fired; why it isn't replayed, or
// only in part; that its record is copied in, from a cloud's alarm or from
// elsewhere, or that an alarm has none; and proposals made against a
// version of it no longer in effect.
func condsOf(v *ruleView) []ruleCond {
	var out []ruleCond
	if v.Content.Facts > 0 {
		out = append(out, ruleCond{Kind: "facts", N: v.Content.Facts})
	}
	if v.Differs {
		out = append(out, ruleCond{Kind: "differs"})
	}
	if v.Unsupported != "" {
		out = append(out, ruleCond{Kind: "unreplayed", Text: v.Unsupported})
	}
	for _, u := range v.Unseen {
		out = append(out, ruleCond{Kind: "unseen", Text: u})
	}
	switch {
	case v.Tie != nil && v.Tie.Source != "":
		out = append(out, ruleCond{Kind: "alarm"})
	case v.Tie != nil:
		out = append(out, ruleCond{Kind: "untied", Text: v.Tie.Why})
	case v.Source != "":
		out = append(out, ruleCond{Kind: "recorded", Text: v.Source})
	}
	stale := 0
	for _, p := range v.Proposals {
		if p.Stale && slices.Contains(ledger.Unfinished(), p.State) {
			stale++
		}
	}
	if stale > 0 {
		out = append(out, ruleCond{Kind: "stale", N: stale})
	}
	return out
}

// ruleFact is a line of what a rule's header says of it: its team,
// severity, for, group and runbook, each when known. Key says which.
type ruleFact struct {
	Key, Value string
}

// moved is how a number changed from as long a span before, when On. Up
// says more is better: the sign's colour follows it, beside its text.
type moved struct {
	On bool
	By int
	Up bool
}

// Text is the change, signed: +3, −2, ±0.
func (m moved) Text() string {
	switch {
	case m.By > 0:
		return "+" + strconv.Itoa(m.By)
	case m.By < 0:
		return "−" + strconv.Itoa(-m.By)
	}
	return "±0"
}

// Class is better or worse, or "" for no change.
func (m moved) Class() string {
	switch {
	case m.By == 0:
		return ""
	case (m.By > 0) == m.Up:
		return "better"
	}
	return "worse"
}

// sharePct is n of all as a whole percent; false with none.
func sharePct(n, all int) (int, bool) {
	if all == 0 {
		return 0, false
	}
	// Rounded as pct rounds it, so a change is that of the shares shown.
	return int(math.RoundToEven(100 * float64(n) / float64(all))), true
}

// rulePanel is a rule's five numbers over the page's period, each, in By,
// beside as long a span before it when the ledger holds that.
type rulePanel struct {
	Precision   string
	PrecisionBy moved // in points
	// Firings are those judged; Black of them caught nothing; Excused, more,
	// a quiet window excused.
	Firings, Black, Excused int
	FiringsBy               moved
	// Incidents it caught, and Near, those it nearly caught.
	Incidents, Near int
	IncidentsBy     moved
	// Notified says notifications are counted for it: Notifications sent,
	// Asleep and OffHours of them while people slept, and off hours.
	Notified                        bool
	Notifications, Asleep, OffHours int
	NotificationsBy                 moved
	// Backtest is the share of its firings, of Either, alike in its replay
	// and in what really fired; "–" when that isn't checked.
	Backtest   string
	Either     int
	BacktestBy moved // in points
}

// stripDays is how many days a rule's strip shows: twelve weeks.
const stripDays = 84

// stripDay is a day of a rule's strip: its stones, which Kind sums up
// (caught: one caught something; nothing: only some that caught nothing;
// excused: only some a quiet window excused; none: no firing), whether the
// rule changed, how many deploys of what it is about there were, and
// whether a quiet window covered it. X is where it is drawn.
type stripDay struct {
	Day  string
	Kind string
	ledger.Span
	Changed bool
	Deploys int
	Quiet   bool
	X       int
}

// dayKind sums a day's stones up.
func dayKind(sp ledger.Span) string {
	switch {
	case sp.White > 0:
		return "caught"
	case sp.Firings > 0:
		return "nothing"
	case sp.Excused > 0:
		return "excused"
	}
	return "none"
}

// The strip is drawn a day every stripStep units: the marks above, in
// rows (a version, a deploy, a quiet window), the day's cell below.
const stripStep = 10

// Cell is where the day's cell is drawn; Hole, the middle of it.
func (d stripDay) Cell() int { return d.X + 1 }
func (d stripDay) Hole() int { return d.X + 5 }

// Version is the mark of a version of the rule, a diamond.
func (d stripDay) Version() string {
	return fmt.Sprintf("M%d 0.5L%d 4L%d 7.5L%d 4Z", d.X+5, d.X+8, d.X+5, d.X+2)
}

// Deploy is the mark of a deploy, a triangle pointing down.
func (d stripDay) Deploy() string {
	return fmt.Sprintf("M%d 9L%d 9L%d 14Z", d.X+1, d.X+9, d.X+5)
}

// ruleStrip is a rule's last 84 days, the earliest first.
type ruleStrip struct {
	Days []stripDay
	// Width is the drawing's; Changed, Deploys and Quiet say some day has
	// that mark, which the legend then names.
	Width                   int
	Changed, Deploys, Quiet bool
}

// Latest are the days, the latest first, as the table lists them.
func (s *ruleStrip) Latest() []stripDay {
	out := slices.Clone(s.Days)
	slices.Reverse(out)
	return out
}

// glance works out a rule's panel and its strip. Its stones by day, over
// the strip and the span before the period, are read once
// (ledger.RuleSpans); the notifications are those the pages hold; its
// replay beside what really fired, before the period, the incidents it
// nearly caught, its deploys and quiet windows, each one read. vs are the
// versions of the rules over the strip's days, the earliest first;
// labelSets, the labels it fires with, as far as is known.
func (s *Server) glance(ctx context.Context, v *ruleView, t *tallies, vs []version, labelSets []map[string]string) error {
	name := v.Name
	now := s.now()
	today := s.l.Day(now)
	first := today - stripDays + 1
	end := s.l.Midnight(today + 1)
	from := s.l.Midnight(t.weeks[len(t.weeks)-v.Period])
	v.From = from
	// The span before the period is as long as the period, to now, and ends
	// where it starts. A number is set beside it only when what the number
	// comes from holds all of it, and none of it was pruned (--keep).
	before := from.Add(-now.Sub(from))
	kept := s.cfg.Keep <= 0 || !before.Before(now.Add(-s.cfg.Keep))
	held, err := s.holds(ctx, v, before)
	if err != nil {
		return err
	}
	if kept && held {
		v.Before = before
	}
	cuts := make([]time.Time, 0, stripDays+2)
	for d := first; d <= today+1; d++ {
		cuts = append(cuts, s.l.Midnight(d))
	}
	if !v.Before.IsZero() {
		if i, ok := slices.BinarySearchFunc(cuts, v.Before, time.Time.Compare); !ok {
			cuts = slices.Insert(cuts, i, v.Before)
		}
	}
	spans, err := s.l.RuleSpans(ctx, name, cuts)
	if err != nil {
		return err
	}
	if len(spans) < len(cuts)-1 {
		spans = append(spans, make([]ledger.Span, len(cuts)-1-len(spans))...) // a rule the ledger hasn't
	}
	sum := func(lo, hi time.Time) ledger.Span {
		var x ledger.Span
		for i, sp := range spans {
			if !cuts[i].Before(lo) && !cuts[i+1].After(hi) {
				x.Firings += sp.Firings
				x.White += sp.White
				x.Excused += sp.Excused
				x.Caught += sp.Caught
			}
		}
		return x
	}
	strip := &ruleStrip{Width: stripDays * stripStep}
	for i := 0; i < stripDays; i++ {
		day := first + int64(i)
		sp := sum(s.l.Midnight(day), s.l.Midnight(day+1))
		strip.Days = append(strip.Days, stripDay{Day: s.l.Midnight(day).In(s.cfg.Loc).Format("2006-01-02"),
			Kind: dayKind(sp), Span: sp, X: i * stripStep})
	}
	v.Strip = strip

	p := rulePanel{Precision: v.Precision, Firings: v.Firings, Black: v.Black, Excused: v.Excused, Incidents: v.Incidents,
		Notified: v.Notified, Notifications: v.Notifications, Asleep: v.NotifiedAsleep, OffHours: v.NotifiedOffHours, Backtest: "–"}
	if !v.Before.IsZero() {
		p.PrecisionBy, p.FiringsBy, p.IncidentsBy = besides(v.ruleLine, sum(v.Before, from))
	}
	if p.Near, err = s.nearOf(ctx, name, from); err != nil {
		return err
	}
	if v.Notified && kept {
		pg, err := s.notified(ctx)
		if err != nil {
			return err
		}
		// The notifications' own record: from the first kept, and the
		// pages hold those of the twelve weeks shown.
		if !pg.first.IsZero() && !pg.first.After(before) && !before.Before(s.shownFrom()) {
			o, err := s.owners(ctx)
			if err != nil {
				return err
			}
			n := 0
			if x := pg.lines(before, from, o.schedules(t.o.Hours).of)[name]; x != nil {
				n = x.Notifications
			}
			p.NotificationsBy = moved{On: true, By: v.Notifications - n}
		}
	}
	if c := v.Check; c != nil {
		p.Backtest, p.Either = fmt.Sprintf("%d%%", c.Share), c.Either()
		// A replay is checked a whole day at a time, to yesterday: as many
		// whole days before the period as it has.
		if days := today - s.l.Day(from); !v.Before.IsZero() && days > 0 {
			cs, err := s.l.Checks(ctx, s.l.Midnight(s.l.Day(from)-days), from)
			if err != nil {
				return err
			}
			if b, ok := cs[name]; ok && b.Either() > 0 {
				p.BacktestBy = moved{On: true, By: c.Share - int(math.Round(b.Share()*100)), Up: true}
			}
		}
	}
	v.Panel = p
	return s.marks(ctx, v, vs, labelSets, s.l.Midnight(first), end)
}

// besides are a rule's precision, firings and incidents caught over the
// period beside those of the span before it, was: precision only when it
// fired in both.
func besides(l ruleLine, was ledger.Span) (precision, firings, incidents moved) {
	now, okNow := sharePct(l.White, l.Firings)
	then, okThen := sharePct(was.White, was.Firings)
	return moved{On: okNow && okThen, By: now - then, Up: true}, moved{On: true, By: l.Firings - was.Firings},
		moved{On: true, By: l.Incidents - was.Caught, Up: true}
}

// holds reports whether the ledger holds a rule's firings over all of a
// span from at on: for one it replays, the replay's runs; for one copied
// in, its copies without a gap: a cloud's alarm's history from when its
// tie says, a rule recorded elsewhere from when its source's do.
func (s *Server) holds(ctx context.Context, v *ruleView, at time.Time) (bool, error) {
	switch {
	case v.Tie != nil:
		return v.Tie.Source != "" && !v.Tie.Since.IsZero() && !v.Tie.Since.After(at), nil
	case v.Source != "":
		from, _, err := s.l.Coverage(ctx, v.Source)
		return err == nil && !from.IsZero() && !from.After(at), err
	}
	began, err := s.l.RunsFrom(ctx)
	return err == nil && !began.IsZero() && !began.After(at), err
}

// Beside says some number is set beside the span before.
func (p rulePanel) Beside() bool {
	return p.PrecisionBy.On || p.FiringsBy.On || p.IncidentsBy.On || p.NotificationsBy.On || p.BacktestBy.On
}

// nearOf is how many incidents a rule nearly caught that started in the
// weeks from from on, as each week's near misses were last worked out.
func (s *Server) nearOf(ctx context.Context, rule string, from time.Time) (int, error) {
	rs, err := s.l.ByPeriod(ctx, "near_misses", from)
	if err != nil {
		return 0, err
	}
	seen, n := map[string]bool{}, 0
	for _, r := range rs { // the latest week first, as nearMisses reads them
		var nms []verdict.NearMiss
		if err := r.Decode(&nms); err != nil {
			return 0, err
		}
		got := map[string]bool{}
		for _, nm := range nms {
			if !seen[nm.Incident] {
				got[nm.Incident] = true
				if nm.Rule == rule {
					n++
				}
			}
		}
		for id := range got {
			seen[id] = true
		}
	}
	return n, nil
}

// marks marks the strip's days the rule changed on, those with a deploy of
// what it is about, and those a quiet window covered it on. A deploy is of
// what it is about, and a window covers it, as the ledger says a firing of
// it would come during one, or be excused by one (ledger.Deploy.During,
// ledger.Window.Excuses), with one of the sets of labels it fires with.
func (s *Server) marks(ctx context.Context, v *ruleView, vs []version, labelSets []map[string]string, from, to time.Time) error {
	days := v.Strip.Days
	first := s.l.Day(from)
	at := func(t time.Time) int {
		if i := int(s.l.Day(t) - first); i >= 0 && i < len(days) {
			return i
		}
		return -1
	}
	var last *rules.Rule
	for k, ver := range vs {
		var cur *rules.Rule
		if i := slices.IndexFunc(ver.Rules, func(x rules.Rule) bool { return x.Name == v.Name }); i >= 0 {
			cur = &ver.Rules[i]
		}
		if k > 0 && (cur == nil) != (last == nil) || cur != nil && last != nil && cur.Whole() != last.Whole() {
			if i := at(ver.Since); i >= 0 && !ver.Since.Before(from) {
				days[i].Changed = true
			}
		}
		last = cur
	}
	if len(labelSets) == 0 {
		labelSets = []map[string]string{{}}
	}
	has, err := s.l.HasDeploys(ctx)
	if err != nil {
		return err
	}
	if has {
		ds, err := s.l.Deploys(ctx, from, to)
		if err != nil {
			return err
		}
		for _, d := range ds {
			i := at(d.Start)
			if i < 0 {
				continue
			}
			for _, ls := range labelSets {
				if d.During(firing.Firing{Rule: v.Name, Labels: ls, Start: d.Start}, 0, 0) {
					days[i].Deploys++
					break
				}
			}
		}
	}
	ws, err := s.l.Windows(ctx, from, to)
	if err != nil {
		return err
	}
	loc := s.l.Location()
	for _, w := range ws {
		for _, rp := range windowRepeats(w, loc, from, to) {
			if !slices.ContainsFunc(labelSets, func(ls map[string]string) bool {
				return w.Excuses(firing.Firing{Rule: v.Name, Labels: ls, Start: rp[0]})
			}) {
				continue
			}
			for d := max(s.l.Day(rp[0]), first); d <= s.l.Day(rp[1]); d++ {
				if i := int(d - first); i >= 0 && i < len(days) {
					days[i].Quiet = true
				}
			}
		}
	}
	for _, d := range days {
		v.Strip.Changed = v.Strip.Changed || d.Changed
		v.Strip.Deploys = v.Strip.Deploys || d.Deploys > 0
		v.Strip.Quiet = v.Strip.Quiet || d.Quiet
	}
	return nil
}

// windowRepeats are, of a quiet window's repeats that meet [from, to], those
// a rule's strip marks, each its start and end, the earliest first: its own
// time, for one that doesn't repeat; every repeat, for one every day or
// week, worked out by the calendar of loc as the ledger works them out; and
// for one by a schedule, the last to start by each day's end, which is the
// one that may last into it. Each is held to Covers, the ledger's own say.
func windowRepeats(w ledger.Window, loc *time.Location, from, to time.Time) [][2]time.Time {
	long := w.End.Sub(w.Start)
	var starts []time.Time
	switch w.Every {
	case "":
		starts = []time.Time{w.Start}
	case "day", "week":
		// A repeat lasts less than a period: the first that may meet from
		// starts on its day, or in the period before it.
		n := 1
		if w.Every == "week" {
			n = 7
		}
		s := w.Start.In(loc)
		for k := max(0, int((civilDay(from.In(loc))-civilDay(s))/int64(n))); ; k++ {
			at := s.AddDate(0, 0, k*n)
			if at.After(to) || !w.Until.IsZero() && civilDay(at) > civilDay(w.Until.In(loc)) {
				break
			}
			starts = append(starts, at)
		}
	case "month", "cron":
		// Every repeat lasts as long: when the last to start by a day's end
		// doesn't last into the day, no other does. The last is on Until's
		// day at the latest. A day at a time, however often it falls due.
		sch, ok := windowSchedule(w, loc)
		if !ok {
			return nil
		}
		f := from.In(loc)
		for day := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, loc); day.Before(to); day = day.AddDate(0, 0, 1) {
			end := day.AddDate(0, 0, 1).Add(-time.Nanosecond)
			if !w.Until.IsZero() {
				u := w.Until.In(loc)
				if last := time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, loc).Add(-time.Nanosecond); last.Before(end) {
					end = last
				}
			}
			due := sch.Last(end, loc)
			if due.IsZero() || due.Before(w.Start) || due.Add(long).Before(day) || len(starts) > 0 && starts[len(starts)-1].Equal(due) {
				continue
			}
			starts = append(starts, due)
		}
	}
	var out [][2]time.Time
	for _, at := range starts {
		if at.Add(long).Before(from) || at.After(to) || !w.Covers(at) {
			continue
		}
		out = append(out, [2]time.Time{at, at.Add(long)})
	}
	return out
}

// windowSchedule is when a window repeating every month, or by a cron
// expression, falls due, read as the ledger reads it.
func windowSchedule(w ledger.Window, loc *time.Location) (schedule.Schedule, bool) {
	on := strings.ToLower(strings.Join(strings.Fields(w.On), " "))
	if w.Every == "month" {
		s := w.Start.In(loc)
		if on == "" {
			on = strconv.Itoa(s.Day())
		}
		on = fmt.Sprintf("monthly %s@%02d:%02d", on, s.Hour(), s.Minute())
	}
	sch, err := schedule.Parse(on, 0)
	return sch, err == nil && (w.Every == "month" || sch.Cron())
}

// civilDay is a time's date as a count of days, its clock left out.
func civilDay(t time.Time) int64 {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
}

// diffLine is a line of a version beside the one before: Op is "-", "+",
// " ", or "@" for where a hunk starts.
type diffLine struct {
	Op, Text string
}

// versionText is a version of a rule as its diff compares it: its
// expression, for, keep_firing_for and labels, a line each.
func versionText(c change) string {
	var b strings.Builder
	for i, l := range strings.Split(c.Expr, "\n") {
		if i == 0 {
			b.WriteString("expr: " + l + "\n")
		} else {
			b.WriteString("      " + l + "\n")
		}
	}
	b.WriteString("for: " + c.For + "\n")
	if c.Keep != "" {
		b.WriteString("keep_firing_for: " + c.Keep + "\n")
	}
	for _, l := range strings.Fields(c.Labels) {
		b.WriteString("label: " + l + "\n")
	}
	return b.String()
}

// versionDiff is a version beside the one before it, as a unified diff
// (rules.Diff), its header left out: one taken away is all taken out, one
// written again after, all put in.
func versionDiff(before, after change) []diffLine {
	a, b := "", ""
	if !before.Gone {
		a = versionText(before)
	}
	if !after.Gone {
		b = versionText(after)
	}
	var out []diffLine
	for i, l := range strings.Split(strings.TrimSuffix(rules.Diff("rule", []byte(a), []byte(b)), "\n"), "\n") {
		if i < 2 || l == "" {
			continue // --- a/rule, +++ b/rule
		}
		out = append(out, diffLine{Op: l[:1], Text: l[1:]})
	}
	return out
}
