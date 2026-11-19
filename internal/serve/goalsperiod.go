package serve

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// Goals over a period (docs/design/goals.md, "Over a period"): a goal of
// each: KIND is checked for each occurrence of that kind of marked window
// (docs/design/marked.md) that ended, and for the one on now as it
// stands, not each week. Its value is the measure inside the occurrence,
// as the Marked windows page counts it: the firings marked with the kind
// on the days it covers, and the incidents during it. A goal of
// no_worse_than: before sets it beside the same measure over the four
// whole weeks before the occurrence's first week, outside the kind, with
// by as slack. An occurrence is in the summary's row of the week it ended
// in, or, on now, of this week, and is worked out wherever it is read, as
// a week is.

// whyNoBefore is why an occurrence's goal of no_worse_than isn't known:
// the four whole weeks before have nothing to set it beside, no firing
// judged for precision, no incident caught for the median lead.
const whyNoBefore = "no_before"

// goalHair is how far a value may be past its bound and be no worse: a
// share, as a quotient, and a bound less a slack, as a difference, are
// that far off what they mean (0.8 - 0.05 is not quite 0.75).
const goalHair = 1e-9

// goalOccsShown is how many occurrences of a goal over a period the Goals
// page shows, the latest, as it shows a weekly goal's twelve whole weeks
// and this one.
const goalOccsShown = 13

// goalOf reports whether a goal is of whom: everyone's (team nil), or a
// team's own or each team's, weekly or over a period.
func goalOf(g ledger.Goal, team *ledger.Team) bool {
	if team == nil {
		return g.TeamID == 0 && !g.EveryTeam
	}
	return g.EveryTeam || g.TeamID == team.ID
}

// teamGoals are the goals of whom, each week's (rowGoals) and over a
// period.
func teamGoals(gs []ledger.Goal, team *ledger.Team) []ledger.Goal {
	var out []ledger.Goal
	for _, g := range gs {
		if (g.Each == "week" || g.OverPeriod()) && goalOf(g, team) {
			out = append(out, g)
		}
	}
	return out
}

// hasPeriodGoals reports whether any goal of whom is over a period.
func hasPeriodGoals(gs []ledger.Goal, who []*ledger.Team) bool {
	for _, g := range gs {
		if g.OverPeriod() && slices.ContainsFunc(who, func(t *ledger.Team) bool { return goalOf(g, t) }) {
			return true
		}
	}
	return false
}

// goalBy is a goal's slack, 0 when left out.
func goalBy(g ledger.Goal) float64 {
	n, _ := strconv.ParseFloat(strings.TrimSpace(g.By), 64)
	return n
}

// periodValue is an occurrence's value of a measure, from its count
// inside it, or over the weeks before: nil when there is none (precision
// with no firing judged, a median with nothing caught).
func periodValue(measure string, c markedCount) *float64 {
	n := func(v int) *float64 {
		f := float64(v)
		return &f
	}
	switch measure {
	case "firings":
		return n(c.Firings)
	case "precision":
		return share(c.Caught, c.judged)
	case "refired.firings":
		return n(c.Refired)
	case "incidents.missed":
		return n(c.Incidents.Missed)
	case "impacts.missed":
		return n(c.Impacts.Missed)
	case "incidents.noisy_only":
		return n(c.Incidents.NoisyOnly)
	case "incidents.median_lead_seconds":
		if m := medianSeconds(c.Incidents.leads); m != nil {
			f := float64(*m)
			return &f
		}
	case "episodes.total":
		if c.episodesCounted {
			return n(c.Episodes)
		}
	case "episodes.precision":
		if c.episodesCounted {
			return share(c.EpisodesCaught, c.episodesJudged)
		}
	}
	return nil
}

// noWorse reports whether v is no worse than before by more than by: for a
// measure higher is better, at least before less by; else at most before
// and by.
func noWorse(higher bool, v, before, by float64) bool {
	if higher {
		return v >= before-by-goalHair
	}
	return v <= before+by+goalHair
}

// occWhen is what an occurrence's goal is worked out by besides its
// counts: whether the ledger has passed its end (open, not), whether the
// ledger holds it whole (after the week its first firing fell in), and
// whether its firings had no record, as a week's (verdict.NoRecordOf).
type occWhen struct {
	open, whole, noRecord bool
}

// periodOn works a goal over a period out for an occurrence, for a team
// (byCatching, one with no labels): as a week is (goalOn), not known with
// why in the same order, before before has nothing to divide.
func periodOn(g ledger.Goal, byCatching bool, o markedOcc, in, before markedCount, when occWhen) summaryGoal {
	m := ledger.GoalMeasures[g.Measure]
	from, to := o.Start.UTC(), o.End.UTC()
	out := summaryGoal{Name: g.Name, Measure: g.Measure, Each: g.Each, Occurrence: o.Key(), Title: o.M.Title, Start: &from, End: &to, held: occurrenceHeld(o)}
	kind, target := g.Bound()
	by := goalBy(g)
	switch kind {
	case "at_least":
		out.AtLeast = &target
	case "at_most":
		out.AtMost = &target
	default:
		out.NoWorseThan, out.By = g.NoWorseThan, &by
	}
	v := periodValue(g.Measure, in)
	out.Value = v
	var b *float64
	if kind == "no_worse_than" {
		b = periodValue(g.Measure, before)
		out.Before = b
	}
	unknown := func(why string) {
		out.Result, out.Why, out.Value = goalUnknown, why, nil
	}
	switch {
	case when.open:
		out.Result = goalOpen
		out.Over = kind == "at_most" && !m.Share && !m.Seconds && !m.Higher && v != nil && *v > target
	case !when.whole:
		unknown(whyNotWhole)
	case m.Incidents && byCatching:
		unknown(whyByCatching)
	case m.Record && when.noRecord:
		unknown(whyNoRecord)
	case episodesMeasure(g.Measure) && (!in.episodesCounted || kind == "no_worse_than" && !before.episodesCounted):
		unknown(whyNoEpisodes)
	case v == nil:
		unknown(whyNoValue)
	case kind == "no_worse_than" && b == nil:
		unknown(whyNoBefore)
	case kind == "no_worse_than" && noWorse(m.Higher, *v, *b, by),
		kind == "at_least" && *v >= target, kind == "at_most" && *v <= target:
		out.Result = goalMet
	default:
		out.Result = goalMissed
	}
	return out
}

// occurrenceHeld is what is kept of an occurrence's goal (what was told of
// it, the task made for it, what was said on it) is keyed by: its content,
// its kind, its window's team and labels, its start and its end, hashed,
// not its window's row (OccurrenceKey). A window an expression finds again
// is kept as a row anew, with another id, but is the same occurrence, and
// isn't told again; one whose start or end a person changes is another,
// and is. Two windows alike in all of it are one occurrence here, as they
// mark the same firings.
func occurrenceHeld(o markedOcc) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00", o.M.Kind, o.M.TeamID)
	for _, k := range slices.Sorted(maps.Keys(o.M.Labels)) {
		fmt.Fprintf(h, "%s=%s\x00", k, o.M.Labels[k])
	}
	fmt.Fprintf(h, "\x00%d\x00%d", o.Start.Unix(), o.End.Unix())
	return "h" + hex.EncodeToString(h.Sum(nil)[:8])
}

// periodAt is where an occurrence's goal is in the summary: the week of
// the row (a Monday, as a day) and its team's id, 0 for everyone.
type periodAt struct {
	week, team int64
}

// periodRead, when set, is told the days of the occurrences periodGoals
// reads, from the first's start to the last's end: their days of
// marked_days, and the weeks of tallies from the four before the first's
// to the last's (readMarked). For the tests, which hold what a goal over
// a period reads.
var periodRead func(d1, d2 int64)

// occPlaced is when an occurrence is placed in the summary's weeks: its
// end, or now while it is on, as it stands.
func occPlaced(o ledger.Occurrence, now time.Time) time.Time {
	if now.Before(o.End) {
		return now
	}
	return o.End
}

// periodGoals works out the goals over a period of who (each a team, or
// nil for everyone) for the occurrences placed in the weeks from Monday w1
// to Monday w2: those that ended in one of them, and the one on now, in
// this week. For each week and team, the goals in their order, each
// occurrence of its kind that applies to the team (everyone's windows and
// the team's own) by when it ended. Read once for every team and goal,
// from #90's counts of each occurrence: its days of marked_days, four
// weeks of weekly tallies before it, and the incidents of those weeks.
// A team's is judged by everyone's record, as the summary's row of a team
// is (the design's decision on a team's record); cursor says which have
// ended, as far as the runs have gone less what a span waits for before
// it is told (wholeAfter), first which the ledger holds whole.
func (s *Server) periodGoals(ctx context.Context, w1, w2 int64, who []*ledger.Team, gs []ledger.Goal, own *owners, cursor time.Time,
	firstWeek int64, firstOK bool) (map[periodAt][]summaryGoal, error) {
	if !hasPeriodGoals(gs, who) {
		return nil, nil
	}
	kinds := map[string]bool{}
	for _, g := range gs {
		if g.OverPeriod() {
			kinds[g.Each] = true
		}
	}
	now := s.now()
	from, to := s.l.Midnight(w1), s.l.Midnight(w2+7)
	if !now.After(from) {
		return nil, nil
	}
	ms, err := s.l.MarkedMeeting(ctx, from, to.Add(-time.Second))
	if err != nil {
		return nil, err
	}
	var keep []ledger.Marked
	for _, m := range ms {
		if kinds[m.Kind] {
			keep = append(keep, m)
		}
	}
	if len(keep) == 0 {
		return nil, nil
	}
	x := &markedSpan{s: s, own: own, ms: keep}
	var occs []markedOcc
	d1, d2 := int64(math.MaxInt64), int64(math.MinInt64)
	for _, os := range x.occurrences(from, to, nil) {
		for _, o := range os {
			if p := occPlaced(o.Occurrence, now); o.Start.After(now) || p.Before(from) || !p.Before(to) {
				continue
			}
			occs = append(occs, o)
			d1, d2 = min(d1, s.l.Day(o.Start)), max(d2, s.l.Day(o.End))
		}
	}
	if len(occs) == 0 {
		return nil, nil
	}
	slices.SortStableFunc(occs, func(a, b markedOcc) int {
		return cmp.Or(a.End.Compare(b.End), a.Start.Compare(b.Start), cmp.Compare(a.M.ID, b.M.ID))
	})
	if periodRead != nil {
		periodRead(d1, d2)
	}
	d, err := s.readMarked(ctx, x, d1, d2, nil)
	if err != nil {
		return nil, err
	}
	type key struct {
		occ  string
		team int64
	}
	type counted struct {
		in, before markedCount
	}
	memo := map[key]counted{}
	out := map[periodAt][]summaryGoal{}
	for _, team := range who {
		byCatching := team != nil && len(team.Labels) == 0
		for _, g := range gs {
			if !g.OverPeriod() || !goalOf(g, team) {
				continue
			}
			for _, o := range occs {
				if o.M.Kind != g.Each || !applies(*o.M, team) {
					continue
				}
				k := key{o.Key(), teamIDOf(team)}
				c, ok := memo[k]
				if !ok {
					c.in, c.before, _ = d.count(o, team)
					memo[k] = c
				}
				first, last := ledger.Week(s.l.Day(o.Start)), ledger.Week(s.l.Day(o.End))
				when := occWhen{open: cursor.Before(o.End), whole: firstOK && first > firstWeek}
				when.noRecord = verdict.NoRecordOf(c.in.Firings, c.in.Firings-c.in.judged, func() bool {
					for w := first; w <= last; w += 7 {
						if d.t.keptAny(w) {
							return false
						}
					}
					return true
				})
				at := periodAt{ledger.Week(s.l.Day(occPlaced(o.Occurrence, now))), teamIDOf(team)}
				out[at] = append(out[at], periodOn(g, byCatching, o, c.in, c.before, when))
			}
		}
	}
	if periodCounted != nil {
		periodCounted(d.rows, int((d.w2-d.w1)/7+1), len(occs), len(memo))
	}
	return out, nil
}

// periodCounted, when set, is told what periodGoals read and worked out:
// the rows of tallies and incidents, the weeks of tallies, the occurrences
// placed in the span, and the counts of an occurrence for a team. For
// TestPagesScale, which holds them.
var periodCounted func(rows, weeks, occs, counted int)

// The texts of a goal over a period, as the pages, the digest, reports,
// tasks and what is told say them.

// occText names an occurrence: its kind, its title, and when, in loc.
func occText(l string, w summaryGoal, loc *time.Location) string {
	at := func(t *time.Time) string {
		if t == nil {
			return "–"
		}
		return t.In(loc).Format("2006-01-02 15:04")
	}
	return tr(l, "%s %s, %s → %s", w.Each, verdict.Clean(w.Title), at(w.Start), at(w.End))
}

// periodTarget is a goal over a period's bound for an occurrence: of
// no_worse_than, with the weeks before's value it was set beside.
func periodTarget(l string, g ledger.Goal, before *float64) string {
	if kind, _ := g.Bound(); kind != "no_worse_than" {
		return goalTarget(l, g)
	}
	b := goalValueText(g.Measure, before)
	if by := goalBy(g); by != 0 {
		return tr(l, "no worse than the four weeks before, %s, by %s", b, goalSlack(g.Measure, by))
	}
	return tr(l, "no worse than the four weeks before, %s", b)
}

// goalSlack is a goal's by as a target says it: an amount, unsigned, as a
// lead's time would read "+1m".
func goalSlack(measure string, by float64) string {
	return strings.TrimPrefix(goalNumber(measure, by), "+")
}

// periodMissedText says a goal over a period was missed during an
// occurrence: its value and its target, of a team's or everyone's.
func periodMissedText(l string, g ledger.Goal, team string, w summaryGoal, loc *time.Location) string {
	if team != "" {
		return tr(l, "Goal %s of %s missed during %s: %s was %s; the goal is %s.", g.Name, verdict.Clean(team), occText(l, w, loc),
			goalMeasureText(l, g.Measure), goalMissedValue(g, w.Value), periodTarget(l, g, w.Before))
	}
	return tr(l, "Goal %s missed during %s: %s was %s; the goal is %s.", g.Name, occText(l, w, loc),
		goalMeasureText(l, g.Measure), goalMissedValue(g, w.Value), periodTarget(l, g, w.Before))
}

// periodMissedTeamsText says a goal of each team's over a period was
// missed during an occurrence by teams: how many, the first ten with their
// values, and its target.
func periodMissedTeamsText(l string, g ledger.Goal, w summaryGoal, teams []goalNoticeTeam, more int, loc *time.Location) string {
	var named []string
	for _, x := range teams[:min(len(teams), 10)] {
		named = append(named, tr(l, "%s (%s)", verdict.Clean(x.Team), goalMissedValue(g, x.Value)))
	}
	list := strings.Join(named, ", ")
	if n := len(teams) + more - len(named); n > 0 {
		list += " " + tr(l, "and %d more", n)
	}
	return tr(l, "Goal %s missed during %s by %d teams: %s; the goal is %s.", g.Name, occText(l, w, loc), len(teams)+more, list, goalTarget(l, g))
}

// periodCellOf is an occurrence of a goal as a cell: as a week's
// (goalCellOf), named by the occurrence, with the weeks before's value of
// a goal of no_worse_than.
func periodCellOf(l string, w goalWeek, loc *time.Location) goalCell {
	c := goalCellOf(l, w, loc)
	c.Text = tr(l, "%s: %s, %s", occText(l, w.summaryGoal, loc), goalResultText(l, w.summaryGoal), c.Value)
	if w.NoWorseThan != "" {
		c.Text += "; " + tr(l, "the four weeks before %s", goalValueText(w.Measure, w.Before))
	}
	return c
}

// lastEnded is a goal over a period's latest occurrence that ended, ok
// false for none.
func (g goalLine) lastEnded() (goalWeek, bool) {
	for i := len(g.Occs) - 1; i >= 0; i-- {
		if g.Occs[i].Result != goalOpen {
			return g.Occs[i], true
		}
	}
	return goalWeek{}, false
}

// endedSince are a goal over a period's occurrences that ended at or after
// since, the earliest first.
func (g goalLine) endedSince(since time.Time) []goalWeek {
	var out []goalWeek
	for _, w := range g.Occs {
		if w.Result != goalOpen && !w.To.Before(since) {
			out = append(out, w)
		}
	}
	return out
}

// goalOccEnd is what notices and tasks of an occurrence's goal are kept
// by, besides its goal and team: when it ended, in Unix seconds.
func goalOccEnd(w goalWeek) int64 { return w.To.Unix() }

// cutText is s cut to n characters at most, with … where it was cut.
func cutText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// goalToldARun is how many goal.missed notices of goals over a period a
// run tells at most, as it makes 20 tasks and writes 20 comments: the
// earliest ended first, the rest told by the runs after. A goal of each
// week tells one a week, 50 at most, so it needs none. A variable, for
// tests.
var goalToldARun = 20

// tellPeriodGoals tells goal.missed of the goals over a period, once for
// each goal, team and occurrence that ended since the last whole week
// began and was missed, when it is first found missed: an occurrence that
// turns missed late (an incident written down a day after) is told then,
// whatever was told of the others. A goal of each team's is one notice an
// occurrence, of the teams that missed it and weren't told of it. Each
// team is marked told of it by its own key (ledger.TellPeriodGoalMissed),
// so two occurrences ending at once are told apart. goalToldARun a run at
// most.
func (s *Server) tellPeriodGoals(ctx context.Context, lines []goalLine, last int64) error {
	dests := s.dests()
	if len(dests) == 0 || !slices.ContainsFunc(lines, func(gl goalLine) bool { return gl.Goal.OverPeriod() }) {
		return nil
	}
	told0, _, err := s.l.PeriodGoalMarks(ctx)
	if err != nil {
		return err
	}
	type notice struct {
		about goalNotice
		teams []int64
		end   int64
		held  string
	}
	since := s.l.Midnight(last)
	var order []*notice
	byOcc := map[string]*notice{}
	for _, gl := range lines {
		if !gl.Goal.OverPeriod() {
			continue
		}
		team := teamIDOf(gl.Team)
		for _, w := range gl.endedSince(since) {
			if w.Result != goalMissed || told0[ledger.GoalOccKey(gl.Goal.Name, team, w.held)] != 0 {
				continue
			}
			k := gl.Goal.Name + "\x00" + w.held
			n := byOcc[k]
			if n == nil {
				n = &notice{end: goalOccEnd(w), held: w.held, about: goalNotice{Name: gl.Goal.Name, Team: gl.TeamName(), Measure: gl.Goal.Measure, AtLeast: w.AtLeast, AtMost: w.AtMost,
					Value: w.Value, From: w.From.UTC(), To: w.To.UTC(), Each: w.Each, Occurrence: w.Occurrence, Title: w.Title, NoWorseThan: w.NoWorseThan,
					By: w.By, Before: w.Before}}
				if gl.Goal.EveryTeam {
					n.about.Team, n.about.Value, n.about.Before = "*", nil, nil
				}
				byOcc[k] = n
				order = append(order, n)
			}
			n.teams = append(n.teams, team)
			if gl.Goal.EveryTeam {
				if len(n.about.Teams) < goalTeamsNamed {
					n.about.Teams = append(n.about.Teams, goalNoticeTeam{gl.TeamName(), w.Value})
				} else {
					n.about.More++
				}
			}
		}
	}
	// The earliest ended first: they are the first to pass the last whole
	// week's start, after which none is told.
	slices.SortStableFunc(order, func(a, b *notice) int {
		return cmp.Or(cmp.Compare(a.end, b.end), strings.Compare(a.about.Name, b.about.Name), strings.Compare(a.held, b.held))
	})
	if n := len(order) - goalToldARun; n > 0 {
		s.log.Info("goal.missed of occurrences of goals over a period wait for the runs after", "waiting", n, "a run", goalToldARun)
		order = order[:goalToldARun]
	}
	var errs []error
	for _, n := range order {
		told := make([]ledger.Told, 0, len(dests))
		for _, d := range dests {
			told = append(told, ledger.Told{Dest: d, About: map[string]any{"goal": n.about}})
		}
		errs = append(errs, s.l.TellPeriodGoalMissed(ctx, n.about.Name, n.teams, n.held, n.end, told))
	}
	return errors.Join(errs...)
}

// periodTaskIn is the task a goal over a period missed during an
// occurrence becomes, in a language: as a week's (goalTaskIn), titled with
// the goal and the occurrence, the window's title cut to fit.
func (s *Server) periodTaskIn(l string, g ledger.Goal, team *ledger.Team, w goalWeek) ledger.Task {
	day := w.From.In(s.cfg.Loc).Format("2006-01-02")
	title := cutText(verdict.Clean(w.Title), 80)
	link := "/goals"
	t := tr(l, "Goal %s missed during %s %s, %s", g.Name, w.Each, title, day)
	if team != nil {
		t = tr(l, "Goal %s of %s missed during %s %s, %s", g.Name, team.Name, w.Each, title, day)
		link += "?team=" + url.QueryEscape(team.Name)
	}
	link += "#goal-" + g.Name
	if s.cfg.ExternalURL != "" {
		link = strings.TrimRight(s.cfg.ExternalURL, "/") + link
	}
	body := periodMissedText(l, g, "", w.summaryGoal, s.cfg.Loc) + "\n\n" +
		tr(l, "Tune the rules, write the incidents down, or change the goal in values. The goal: %s", link)
	return ledger.Task{Title: cutText(t, 200), Kind: "decide", TeamID: teamIDOf(team), Goal: g.Name, Body: body}
}

// sayOnPeriodTask says on a goal over a period's open task what became of
// its occurrences, once each, by each occurrence's key, as a week's task
// is said on (sayOnGoalTasks): of those that ended since the last whole
// week began as it was made (Since), another missed ("missed again"), one
// it was made for or said missed that no longer is ("no longer missed"),
// whenever each turns so, an occurrence that ended before the one it was
// made for among them; and one met that ended after it ("met"). most at
// most; it returns how many it said.
func (s *Server) sayOnPeriodTask(ctx context.Context, id int64, gl goalLine, st ledger.GoalTaskState, most int) (int, error) {
	l := s.cfg.Lang
	oldest := s.l.Midnight(ledger.Week(s.l.Day(s.now())) - 12*7).Unix()
	if st.Occs == nil {
		st.Occs = map[string]ledger.GoalOccSaid{}
	}
	for k, x := range st.Occs {
		if x.End < oldest {
			delete(st.Occs, k) // long past every occurrence worked out
		}
	}
	said := 0
	for _, w := range gl.Occs {
		if w.Result == goalOpen || w.Why == whyNotWhole {
			continue
		}
		at := goalOccEnd(w)
		if at < st.Since || said >= most {
			continue
		}
		occ := occText(l, w.summaryGoal, s.cfg.Loc)
		var body, now string
		switch was := st.Occs[w.held].Said; {
		case w.Result == goalMissed:
			// Once an occurrence: the one it was made for is said already.
			if was == goalMissed || was == goalNoLonger {
				continue
			}
			body, now = tr(l, "Missed again, %s: %s.", occ, goalMissedValue(gl.Goal, w.Value)), goalMissed
		case was == goalMissed && w.Result == goalMet:
			body, now = tr(l, "No longer missed, %s: %s.", occ, goalValueText(gl.Goal.Measure, w.Value)), goalNoLonger
		case was == goalMissed:
			body, now = tr(l, "No longer missed, %s: %s.", occ, goalResultText(l, w.summaryGoal)), goalNoLonger
		case w.Result == goalMet && was == "" && at > st.Week:
			body, now = tr(l, "Met, %s: %s.", occ, goalValueText(gl.Goal.Measure, w.Value)), goalMet
		default:
			continue
		}
		st.Occs[w.held] = ledger.GoalOccSaid{End: at, Said: now}
		if err := s.l.SayOnGoalTask(ctx, id, body, st, ledger.GoalTasksBy); err != nil {
			return said, err
		}
		said++
	}
	return said, nil
}
