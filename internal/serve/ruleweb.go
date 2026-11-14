package serve

import (
	"context"
	"fmt"
	"maps"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

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

// readsOf are, for the pages that read only some of their query, the
// query as each reads it: what else a page was asked is nothing to it, and
// mustn't make it worked out again, nor be said back on it.
var readsOf = map[string]func(url.Values) string{"rule": ruleQuery}

// ruleQuery is a rule's page's query as the page reads it: its tab, its
// weeks, its team (all teams, empty) and edit=note, in one order.
func ruleQuery(q url.Values) string {
	out := url.Values{}
	if t := ruleTabOf(q); t != "overview" {
		out.Set("tab", t)
	}
	switch w := q.Get("weeks"); w {
	case "1", "4", "12":
		out.Set("weeks", w)
	}
	if vs, ok := q["team"]; ok {
		out.Set("team", vs[0])
	}
	if q.Get("edit") == "note" {
		out.Set("edit", "note")
	}
	return out.Encode()
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

// condsIn is what a rule's conditions are worked out from, as the rules
// list and its page each have it: the problems of fact in what it says;
// whether its replay differs from what really fired; why it isn't
// replayed, and how it is replayed only in part; its alarm's tie, for a
// cloud's alarm; where it is recorded, for one copied in; and how many of
// its proposals not finished with were made against a version of it no
// longer in effect.
type condsIn struct {
	Facts       int
	Differs     bool
	Unsupported string
	Unseen      []string
	Tie         *tieView
	Source      string
	Stale       int
}

// condsOf are what to check about a rule, in the order its header lists
// them; the rules list's badge, "n to check", counts them.
func condsOf(c condsIn) []ruleCond {
	var out []ruleCond
	if c.Facts > 0 {
		out = append(out, ruleCond{Kind: "facts", N: c.Facts})
	}
	if c.Differs {
		out = append(out, ruleCond{Kind: "differs"})
	}
	if c.Unsupported != "" {
		out = append(out, ruleCond{Kind: "unreplayed", Text: c.Unsupported})
	}
	for _, u := range c.Unseen {
		out = append(out, ruleCond{Kind: "unseen", Text: u})
	}
	switch {
	case c.Tie != nil && c.Tie.Source != "":
		out = append(out, ruleCond{Kind: "alarm"})
	case c.Tie != nil:
		out = append(out, ruleCond{Kind: "untied", Text: c.Tie.Why})
	case c.Source != "":
		out = append(out, ruleCond{Kind: "recorded", Text: c.Source})
	}
	if c.Stale > 0 {
		out = append(out, ruleCond{Kind: "stale", N: c.Stale})
	}
	return out
}

// condsIn is what its page has of a rule's conditions.
func (v *ruleView) condsIn() condsIn {
	stale := 0
	for _, p := range v.Proposals {
		if p.Stale && slices.Contains(ledger.Unfinished(), p.State) {
			stale++
		}
	}
	return condsIn{Facts: v.Content.Facts, Differs: v.Differs, Unsupported: v.Unsupported, Unseen: v.Unseen, Tie: v.Tie, Source: v.Source, Stale: stale}
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
// rule changed, how many of its firings came during a deploy of what it is
// about, and whether a quiet window was over it. X is where it is drawn.
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
// versions of the rules over the strip's days, the earliest first; own,
// its labels, as far as they are known.
func (s *Server) glance(ctx context.Context, v *ruleView, t *tallies, vs []version, own map[string]string) error {
	name := v.Name
	now := s.now()
	today := s.l.Day(now)
	first := today - stripDays + 1
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
	return s.marks(ctx, v, vs, own, first)
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

// marks marks the strip's days, from the day first on: those the rule
// changed on (vs, the versions of the rules over them); those on which it
// fired during a deploy of what it is about, as the ledger judged each of
// its firings; and those a quiet window was over it: each day a window
// excused one of its firings, the ledger's own say, and each a window
// covers that applies to it by its own name, team and labels, own.
func (s *Server) marks(ctx context.Context, v *ruleView, vs []version, own map[string]string, first int64) error {
	days := v.Strip.Days
	from := s.l.Midnight(first)
	var last *rules.Rule
	for k, ver := range vs {
		var cur *rules.Rule
		if i := slices.IndexFunc(ver.Rules, func(x rules.Rule) bool { return x.Name == v.Name }); i >= 0 {
			cur = &ver.Rules[i]
		}
		if k > 0 && (cur == nil) != (last == nil) || cur != nil && last != nil && cur.Whole() != last.Whole() {
			if i := int(s.l.Day(ver.Since) - first); i >= 0 && i < len(days) && !ver.Since.Before(from) {
				days[i].Changed = true
			}
		}
		last = cur
	}
	deployed, err := s.l.RuleDeploys(ctx, v.Name, first, first+int64(len(days))-1)
	if err != nil {
		return err
	}
	for d, n := range deployed {
		days[d-first].Deploys = n
	}
	quiet := 0
	for i := range days {
		if days[i].Excused > 0 {
			days[i].Quiet = true
			quiet++
		}
	}
	ws, err := s.l.Windows(ctx, from, s.l.Midnight(first+int64(len(days))))
	if err != nil {
		return err
	}
	for _, w := range ws {
		if quiet == len(days) {
			break
		}
		if w.TeamID < 0 || !w.Applies(v.Name, own) {
			continue // before working out when it falls due
		}
		windowDays(w, s.l, first, len(days), func(i int) {
			if !days[i].Quiet {
				days[i].Quiet = true
				quiet++
			}
		})
	}
	for _, d := range days {
		v.Strip.Changed = v.Strip.Changed || d.Changed
		v.Strip.Deploys = v.Strip.Deploys || d.Deploys > 0
		v.Strip.Quiet = v.Strip.Quiet || d.Quiet
	}
	return nil
}

// dayLedger is what windowDays reads a day by: the ledger's zone, and its
// days and when each starts.
type dayLedger interface {
	Location() *time.Location
	Day(time.Time) int64
	Midnight(int64) time.Time
}

// windowDays marks each of the n days from first that a quiet window covers
// some time of, as the ledger's own Covers says: a day is i, from 0. One
// that doesn't repeat covers its own time. One every day or week covers,
// of each repeat, the part on its day and the period after it, as Covers
// sets a time beside the repeats of the periods it falls in. One by a
// schedule covers each repeat, from when it falls due to as long after:
// they are walked back from the last day's end, so what it costs is how
// many fall due over the days and one lookup back; past stripMost of them,
// each day asks for the last due by its end instead, a short way back for
// a schedule due so often.
func windowDays(w ledger.Window, l dayLedger, first int64, n int, mark func(i int)) {
	from, to := l.Midnight(first), l.Midnight(first+int64(n))
	long := w.End.Sub(w.Start)
	span := func(lo, hi time.Time) {
		if lo.Before(from) {
			lo = from
		}
		if !hi.Before(to) {
			hi = to.Add(-time.Nanosecond)
		}
		for d := l.Day(lo); !lo.After(hi) && d <= l.Day(hi); d++ {
			mark(int(d - first))
		}
	}
	loc := l.Location()
	switch w.Every {
	case "":
		span(w.Start, w.End)
	case "day", "week":
		per := int64(1)
		if w.Every == "week" {
			per = 7
		}
		s := w.Start.In(loc)
		c0 := civilDay(s)
		// Covers sets a time beside the repeats of its period and the one
		// before: repeat k covers what it lasts over of its period and the
		// next, from its period's first day's start, and from its start,
		// which a skipped midnight can put in the day before.
		lo := max(0, (l.Day(from)-c0)/per-1)
		for k := lo; k <= (l.Day(to)-c0)/per; k++ {
			at := s.AddDate(0, 0, int(k*per))
			if !w.Until.IsZero() && civilDay(at) > civilDay(w.Until.In(loc)) {
				break
			}
			span(latest(at, w.Start, l.Midnight(c0+k*per)), earliest(at.Add(long), l.Midnight(c0+(k+2)*per).Add(-time.Nanosecond)))
		}
	case "month", "cron":
		sch, ok := windowSchedule(w, loc)
		if !ok {
			return
		}
		// The last repeat starts by the end of Until's day, as Covers has it.
		upTo := func(t time.Time) time.Time {
			if !w.Until.IsZero() {
				u := w.Until.In(loc)
				if end := time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, loc).Add(-time.Nanosecond); t.After(end) {
					return end
				}
			}
			return t
		}
		floor := from.Add(-long)
		k := 0
		for at := sch.Last(upTo(to.Add(-time.Nanosecond)), loc); !at.IsZero() && !at.Before(w.Start) && !at.Before(floor); at = sch.Prev(at, loc) {
			if k++; k > stripMost {
				// Due more often than that: a day at a time.
				for d := first; d < first+int64(n); d++ {
					start := l.Midnight(d)
					if at := sch.Last(upTo(l.Midnight(d+1).Add(-time.Nanosecond)), loc); !at.IsZero() && !at.Before(w.Start) && !at.Add(long).Before(start) {
						mark(int(d - first))
					}
				}
				return
			}
			span(at, at.Add(long))
		}
	}
}

// stripMost is how many repeats of a quiet window by a schedule a rule's
// strip walks, four a day; past that, it looks a day at a time.
const stripMost = 4 * stripDays

func latest(ts ...time.Time) time.Time {
	out := ts[0]
	for _, t := range ts[1:] {
		if t.After(out) {
			out = t
		}
	}
	return out
}

func earliest(ts ...time.Time) time.Time {
	out := ts[0]
	for _, t := range ts[1:] {
		if t.Before(out) {
			out = t
		}
	}
	return out
}

// windowSchedule is when a window repeating every month, or by a cron
// expression, falls due, read as the ledger reads it: one every month on
// a day past the 28th, with no day given, has none.
func windowSchedule(w ledger.Window, loc *time.Location) (schedule.Schedule, bool) {
	on := strings.ToLower(strings.Join(strings.Fields(w.On), " "))
	if w.Every == "month" {
		s := w.Start.In(loc)
		if on == "" {
			if s.Day() > 28 {
				return schedule.Schedule{}, false
			}
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

// diffLine is a line of a version beside the one before: Op is "-" for
// one taken out, "+" for one put in, " " for one the same.
type diffLine struct {
	Op, Text string
}

// versionText is a version of a rule as its diff compares it, a line each:
// its expression, for, keep_firing_for, and each of its labels, its value
// quoted, so that a value with spaces or an = in it is one label.
func versionText(c change) []string {
	var out []string
	for i, l := range strings.Split(c.Expr, "\n") {
		if i == 0 {
			out = append(out, "expr: "+l)
		} else {
			out = append(out, "      "+l)
		}
	}
	out = append(out, "for: "+c.For)
	if c.Keep != "" {
		out = append(out, "keep_firing_for: "+c.Keep)
	}
	for _, k := range slices.Sorted(maps.Keys(c.labels)) {
		name := k
		if !labelName.MatchString(k) {
			name = strconv.Quote(k)
		}
		out = append(out, "label: "+name+"="+strconv.Quote(c.labels[k]))
	}
	return out
}

// labelName is a label's name as Prometheus has them, written as it is.
var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// diffMost is how many lines a side a version is set beside the one
// before line by line; past it, it is shown after the one before, whole.
const diffMost = 400

// versionDiff is a version beside the one before it, line by line: the
// lines the same in both, and those taken out and put in, by their longest
// common run. One taken away is all taken out, one written again after,
// all put in. Whole says one side was past diffMost lines: the one before,
// taken out, then this one, put in.
func versionDiff(before, after change) (out []diffLine, whole bool) {
	var a, b []string
	if !before.Gone {
		a = versionText(before)
	}
	if !after.Gone {
		b = versionText(after)
	}
	if len(a) > diffMost || len(b) > diffMost {
		for _, l := range a {
			out = append(out, diffLine{"-", l})
		}
		for _, l := range b {
			out = append(out, diffLine{"+", l})
		}
		return out, true
	}
	// lcs[i][j] is the longest common run of a[i:] and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			out = append(out, diffLine{" ", a[i]})
			i, j = i+1, j+1
		case j == len(b) || i < len(a) && lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, diffLine{"-", a[i]})
			i++
		default:
			out = append(out, diffLine{"+", b[j]})
			j++
		}
	}
	return out, false
}
