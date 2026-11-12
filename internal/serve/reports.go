package serve

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/report"
	"lapilli.dev/internal/schedule"
	"lapilli.dev/internal/verdict"
)

// Reports people schedule (docs/design/reports.md). Each falls due by its
// schedule, waits for the ledger to have its span, and goes through the
// outbox once for each span: its text to the Slack-style webhooks it names,
// report.ready to the webhook. It reads the ledger and replays nothing.

// reportDest is the outbox's place for a report's text, to a destination
// named in it: each notice its own receiver, so one down holds up no other.
const reportDest = "report"

// reportWait is how long a report waits for a ledger behind, at most,
// before it is sent with what there is; half the time since it last fell
// due, when that is less, so an hourly report isn't overtaken by the next,
// whatever span it covers.
const reportWait = 6 * time.Hour

func waitFor(gap time.Duration) time.Duration { return min(reportWait, gap/2) }

// reportGap is how long before due the report fell due last: what a late
// report is measured by, from when it fell due, not from its span's end,
// which may be long before (a month's last day; a span given longer than
// the time between two).
func reportGap(sch schedule.Schedule, due time.Time, loc *time.Location) time.Duration {
	if prev := sch.Prev(due, loc); !prev.IsZero() && prev.Before(due) {
		return due.Sub(prev)
	}
	return 24 * time.Hour
}

// destFile is the file holding a named destination's webhook, and the flag
// that named it; ok is false for a name nothing names now.
func (s *Server) destFile(name string) (file, flag string, ok bool) {
	switch {
	case name == "slack":
		return s.cfg.Slack, "--slack-webhook-file", s.cfg.Slack != ""
	case name == "webhook":
		return "", "", false // report.ready, as JSON
	case strings.HasPrefix(name, "team:"):
		team := strings.TrimPrefix(name, "team:")
		file, ok = s.cfg.TeamDigests[team]
		return file, "--team-digest " + team, ok
	}
	file, ok = s.cfg.ReportDests[name]
	return file, "--report-dest " + name, ok
}

// reportDests are the names of the destinations a report can go to now:
// those --report-dest names, then slack, webhook and each team:NAME.
func (s *Server) reportDests() []string {
	out := slices.Sorted(maps.Keys(s.cfg.ReportDests))
	if s.cfg.Slack != "" {
		out = append(out, "slack")
	}
	if s.cfg.NotifyWebhook != "" {
		out = append(out, "webhook")
	}
	for _, team := range slices.Sorted(maps.Keys(s.cfg.TeamDigests)) {
		if ledger.ValidDest("team:" + team) {
			out = append(out, "team:"+team)
		}
	}
	return out
}

// knowsDests refuses a report naming a destination nothing names: the UI
// and the API pick among names, never take a URL.
func (s *Server) knowsDests(r ledger.Scheduled) error {
	known := s.reportDests()
	for _, d := range r.Dests {
		if !slices.Contains(known, d) {
			return bad("there is no destination called %q: --report-dest names them", d)
		}
	}
	return nil
}

// sendsReports reports whether a report has anywhere to go.
func (s *Server) sendsReports() bool { return len(s.reportDests()) > 0 }

// reportTeam is the team a report is for: nil for everyone; ok is false
// when its team was removed, and it stops: the ledger negates the id of a
// team removed, so a team made later with its id isn't taken for it.
func reportTeam(r ledger.Scheduled, own *owners) (*ledger.Team, bool) {
	switch {
	case r.TeamID == 0:
		return nil, true
	case r.TeamID < 0:
		return nil, false
	}
	t, ok := own.byID[r.TeamID]
	return &t, ok
}

// reportLoc is the zone a report's times are counted in: its team's, when
// it says so and the team has one; the ledger's otherwise.
func (s *Server) reportLoc(r ledger.Scheduled, t *ledger.Team) *time.Location {
	if r.Zone == "team" && t != nil && t.TZ != "" {
		if loc, err := time.LoadLocation(t.TZ); err == nil {
			return loc
		}
	}
	return s.cfg.Loc
}

// reports sends each report that fell due and hasn't been sent for its
// span.
func (s *Server) reports(ctx context.Context, o verdict.Options) error {
	if !s.sendsReports() {
		return nil
	}
	rs, err := s.l.Scheduleds(ctx)
	if err != nil || len(rs) == 0 {
		return err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rs {
		team, ok := reportTeam(r, own)
		if !r.Enabled || !ok {
			continue // stopped; the reports and runs pages say why
		}
		if err := s.reportDue(ctx, r, team, o); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
		}
	}
	return errors.Join(errs...)
}

// reportDue sends a report, if it is due: for the last time it fell due,
// once, if that was no longer ago than the time between it and the one
// before (after a restart, the last only) and since the report was last
// changed.
func (s *Server) reportDue(ctx context.Context, r ledger.Scheduled, team *ledger.Team, o verdict.Options) error {
	sch, err := r.When()
	if err != nil {
		return err
	}
	now := s.now()
	loc := s.reportLoc(r, team)
	due := sch.Last(now, loc)
	if due.IsZero() || due.Before(r.Updated) {
		return nil
	}
	from, to := sch.Span(due, loc)
	gap := reportGap(sch, due, loc)
	if now.Sub(due) > gap {
		return nil // after a restart: too late, by its own schedule
	}
	key, span := ledger.ReportSentKey(r.ID), fmt.Sprint(to.Unix())
	if sent, err := s.l.Told(ctx, key); err != nil || sent == span {
		return err
	}
	firings, checked, err := s.ledgerTo(ctx)
	if err != nil {
		return err
	}
	ready := !firings.Before(to) && (!slices.Contains(r.Sections, "replay") || !s.checking() || !checked.Before(to))
	if !ready && now.Sub(due) < waitFor(gap) {
		return nil // waits for the ledger
	}
	b, err := s.buildReport(ctx, r, team, sch, loc, due, from, to, o, firings, checked)
	if err != nil {
		return err
	}
	var told []ledger.Told
	for _, d := range r.Dests {
		if d == "webhook" {
			if s.cfg.NotifyWebhook != "" {
				told = append(told, ledger.Told{Dest: "webhook", About: b.about()})
			}
			continue
		}
		if _, _, ok := s.destFile(d); !ok {
			s.log.Warn("a report names a destination nothing names now", "report", r.Name, "dest", d)
			continue
		}
		told = append(told, ledger.Told{Dest: reportDest, About: map[string]any{"to": d, "report": r.Name, "report_id": r.ID, "text": b.Text}})
	}
	return s.l.TellOnce(ctx, key, span, "report.ready", told)
}

// ledgerTo is how far the ledger has firings (the runs' cursor, or with no
// runs, what was copied in, as the digest has it) and has set the replay
// beside what really fired.
func (s *Server) ledgerTo(ctx context.Context) (firings, checked time.Time, err error) {
	firings, err = s.l.Cursor(ctx)
	if !s.running() && err == nil {
		firings, err = s.copiedTo(ctx)
	}
	if err != nil || !s.checking() {
		return firings, checked, err
	}
	checked, err = s.l.CheckedTo(ctx)
	return firings, checked, err
}

// built is a report worked out for a span.
type built struct {
	ID       int64
	Name     string
	Team     string
	Due      time.Time
	From, To time.Time
	Zone     string
	Sections []string
	// Numbers are each section's, by its name.
	Numbers map[string]any
	Text    string
	// FiringsTo is how far the ledger had firings, when not to the span's
	// end.
	FiringsTo time.Time
}

// about is what the webhook is told of a report: report.ready.
func (b *built) about() map[string]any {
	about := map[string]any{"report": b.Name, "report_id": b.ID, "due": b.Due.UTC(), "from": b.From.UTC(), "to": b.To.UTC(), "zone": b.Zone,
		"sections": b.Sections, "numbers": b.Numbers,
		// text is the report as written, from rule names and incident
		// titles others wrote: escape it for where it goes. slack_text is
		// it as it is posted to a Slack-style webhook.
		"text": b.Text, "slack_text": report.SlackText(b.Text)}
	if b.Team != "" {
		about["team"] = b.Team
	}
	if !b.FiringsTo.IsZero() {
		about["ledger_to"] = b.FiringsTo.UTC()
	}
	return about
}

// reportMax is how long a report's text is, at most.
const reportMax = 256 << 10

// buildReport works a report out for the span [from, to): its sections'
// lines, in --lang's language, and their numbers.
func (s *Server) buildReport(ctx context.Context, r ledger.Scheduled, team *ledger.Team, sch schedule.Schedule, loc *time.Location,
	due, from, to time.Time, o verdict.Options, firingsTo, checkedTo time.Time) (*built, error) {
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	in := func(string) bool { return true }
	var teamID int64
	if team != nil {
		teamID = team.ID
		in = func(rule string) bool { return own.rule[rule] == team.ID }
	}
	// A report of the ledger over a span, for whom the report is for.
	of := func(from, to time.Time) (*verdict.Report, error) {
		rep, err := s.ledgerReport(ctx, from, to, o, false)
		if err != nil || team == nil {
			return rep, err
		}
		return teamReport(rep, own, *team, o.Hours), nil
	}
	rep, err := of(from, to)
	if err != nil {
		return nil, err
	}
	b := &built{ID: r.ID, Name: r.Name, Due: due, From: from, To: to, Zone: loc.String(), Sections: r.Sections, Numbers: map[string]any{}}
	l := s.cfg.Lang
	at := func(t time.Time) string { return t.In(loc).Format("2006-01-02 15:04") }
	head := tr(l, "%s: %s → %s (%s)", r.Name, at(from), at(to), loc.String())
	if team != nil {
		b.Team = team.Name
		head = tr(l, "%s, for %s: %s → %s (%s)", r.Name, team.Name, at(from), at(to), loc.String())
	}
	lines := []string{verdict.Clean(head)}
	if firingsTo.Before(to) {
		b.FiringsTo = firingsTo
		if firingsTo.IsZero() {
			lines = append(lines, tr(l, "The ledger has no firings yet."))
		} else {
			lines = append(lines, tr(l, "The ledger had firings up to %s: what came later isn't in this report.", at(firingsTo)))
		}
	}
	// The span before, for the summary: never overlapping this one.
	before := func() (*verdict.Report, error) {
		return of(sch.Before(due, loc))
	}
	for _, sec := range r.Sections {
		var line string
		var nums any
		var err error
		switch sec {
		case "summary":
			line, nums, err = s.summarySection(rep, before)
		case "noise":
			line, nums, err = s.noiseSection(rep, from, to, loc, of, before)
		case "missed":
			line, nums, err = s.missedSection(ctx, rep, from, to, loc)
		case "woken":
			line, nums, err = s.wokenSection(ctx, rep, from, to, o, in)
		case "refired", "deploys":
			var rf *refires
			if rf, err = s.refiresOf(ctx, rep, from, to); err == nil {
				nums = rf
				if line = rf.refireLine(l); sec == "deploys" {
					if line = rf.deployLine(l); line == "" {
						line = tr(l, "No deploys are kept: --deploys names where they are read from.")
					}
				}
			}
		case "notifications":
			var sent *sentLine
			if sent, err = s.sent(ctx, rep, from, to); err == nil {
				if line = sent.text(l); line == "" {
					line = tr(l, "No notifications were taken for the span.")
				}
				if sent != nil {
					nums = sent
				}
			}
		case "replay":
			line, nums, err = s.replaySection(ctx, from, to, checkedTo, loc, in)
		case "sources":
			var cs []copiedIn
			if cs, err = s.copied(ctx, rep); err == nil {
				if line = s.sourcesLine(cs); line == "" {
					line = tr(l, "No firings are copied in from elsewhere.")
				}
				nums = orNoneOf(cs)
			}
		case "tasks":
			var open, late, done int
			if open, late, done, err = s.l.TeamTaskCounts(ctx, from, to, teamID); err == nil {
				line = tr(l, "Tasks: %d open, %d overdue, %d done in the span.", open, late, done)
				nums = map[string]int{"open": open, "overdue": late, "done": done}
			}
		case "content":
			var sum *contentSum
			if sum, err = s.contentSummary(ctx, l, in); err == nil {
				nums = sum
				if line = sum.Text; line == "" {
					line = tr(l, "The alert content check found nothing.")
				}
			}
		case "suggestions":
			line, nums, err = s.suggestionsSection(ctx, from, to, in)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sec, err)
		}
		b.Numbers[sec] = nums
		if line != "" {
			lines = append(lines, line)
		}
	}
	text := strings.Join(lines, "\n\n") + "\n"
	b.Text = strings.ToValidUTF8(text[:min(len(text), reportMax)], "")
	return b, nil
}

// counted are a report's firings, those that caught something and those
// that caught nothing, and its precision, nil with no firings.
type counted struct {
	Firings   int      `json:"firings"`
	Caught    int      `json:"caught"`
	Nothing   int      `json:"nothing"`
	Precision *float64 `json:"precision"`
}

func count(rep *verdict.Report) counted {
	var c counted
	for _, r := range rep.Rules {
		c.Firings += r.Firings
		c.Caught += r.White
	}
	c.Nothing = c.Firings - c.Caught
	if c.Firings > 0 {
		p := float64(c.Caught) / float64(c.Firings)
		c.Precision = &p
	}
	return c
}

func (c counted) pct() string {
	if c.Precision == nil {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", math.Round(*c.Precision*100))
}

// summarySection: firings, caught and caught nothing, and precision,
// beside the span before.
func (s *Server) summarySection(rep *verdict.Report, before func() (*verdict.Report, error)) (string, any, error) {
	prev, err := before()
	if err != nil {
		return "", nil, err
	}
	now, was := count(rep), count(prev)
	line := tr(s.cfg.Lang, "Firings: %d; %d caught something, %d caught nothing; precision %s. The span before: %d; %d, %d; %s.",
		now.Firings, now.Caught, now.Nothing, now.pct(), was.Firings, was.Caught, was.Nothing, was.pct())
	return line, map[string]any{"now": now, "before": was, "before_from": prev.From.UTC(), "before_to": prev.To.UTC()}, nil
}

// noisyRule is a noisy rule, as a report tells of it.
type noisyRule struct {
	Rule    string `json:"rule"`
	Firings int    `json:"firings"`
	Nothing int    `json:"nothing"`
}

// noiseSection: the noisiest rules, and those newly noisy since the span
// before. Over a span shorter than a week, the seven days to its end beside
// the seven before them, so one quiet day doesn't make a rule noisy or not.
func (s *Server) noiseSection(rep *verdict.Report, from, to time.Time, loc *time.Location, of func(from, to time.Time) (*verdict.Report, error),
	before func() (*verdict.Report, error)) (string, any, error) {
	l := s.cfg.Lang
	week := to.In(loc).AddDate(0, 0, -7)
	cur := rep
	var prev *verdict.Report
	var err error
	if from.After(week) {
		if cur, err = of(week, to); err == nil {
			prev, err = of(week.AddDate(0, 0, -7), week)
		}
	} else {
		prev, err = before()
	}
	if err != nil {
		return "", nil, err
	}
	was := map[string]bool{}
	for _, r := range prev.Rules {
		was[r.Rule] = r.Noisy
	}
	var noisy []noisyRule
	newly := []string{}
	for _, r := range cur.Rules {
		if !r.Noisy {
			continue
		}
		noisy = append(noisy, noisyRule{r.Rule, r.Firings, r.Firings - r.White})
		if !was[r.Rule] {
			newly = append(newly, r.Rule)
		}
	}
	slices.SortFunc(noisy, func(a, b noisyRule) int {
		return cmp.Or(cmp.Compare(b.Nothing, a.Nothing), cmp.Compare(a.Rule, b.Rule))
	})
	slices.Sort(newly)
	nums := map[string]any{"from": cur.From.UTC(), "to": cur.To.UTC(), "noisy": orNoneOf(noisy), "newly_noisy": newly}
	if len(noisy) == 0 {
		return tr(l, "No rule was noisy."), nums, nil
	}
	items := make([]string, 0, 10)
	for _, n := range noisy[:min(len(noisy), 10)] {
		items = append(items, tr(l, "%s (%d firings, %d caught nothing)", verdict.Clean(n.Rule), n.Firings, n.Nothing))
	}
	line := tr(l, "Noisy rules, from %s: %s.", cur.From.In(loc).Format("2006-01-02"), strings.Join(items, "; "))
	if n := len(noisy) - len(items); n > 0 {
		line += " " + tr(l, "And %d more.", n)
	}
	if len(newly) > 0 {
		names := make([]string, 0, len(newly))
		for _, n := range newly[:min(len(newly), 20)] {
			names = append(names, verdict.Clean(n))
		}
		line += " " + tr(l, "Newly noisy: %s.", strings.Join(names, ", "))
	}
	return line, nums, nil
}

// missedIncident is an incident no alert caught, as a report tells of it.
type missedIncident struct {
	ID    string    `json:"id"`
	Title string    `json:"title,omitempty"`
	Start time.Time `json:"start"`
	// Nearly are the rules that nearly caught it, as last looked for.
	Nearly []string `json:"nearly"`
}

// missedSection: the incidents that started in the span with no alert, and
// the rules that nearly caught each, as the near misses kept have them.
func (s *Server) missedSection(ctx context.Context, rep *verdict.Report, from, to time.Time, loc *time.Location) (string, any, error) {
	l := s.cfg.Lang
	near, err := s.nearKept(ctx, from, to)
	if err != nil {
		return "", nil, err
	}
	var missed []missedIncident
	n, onlyNoisy := 0, 0
	for _, in := range rep.Incidents {
		if in.Start.Before(from) || !in.Start.Before(to) {
			continue
		}
		n++
		if in.OnlyNoisy() {
			onlyNoisy++
		}
		if !in.Missed() {
			continue
		}
		m := missedIncident{ID: in.ID, Title: in.Title, Start: in.Start.UTC(), Nearly: []string{}}
		for _, nm := range near {
			if nm.Incident == in.ID && !slices.Contains(m.Nearly, nm.Rule) {
				m.Nearly = append(m.Nearly, nm.Rule)
			}
		}
		missed = append(missed, m)
	}
	nums := map[string]any{"incidents": n, "missed": orNoneOf(missed), "only_noisy": onlyNoisy}
	switch {
	case n == 0:
		return tr(l, "No incidents."), nums, nil
	case len(missed) == 0:
		return tr(l, "Incidents: %d, every one caught.", n), nums, nil
	}
	items := make([]string, 0, 10)
	for _, m := range missed[:min(len(missed), 10)] {
		item := verdict.Clean(cmp.Or(m.Title, m.ID)) + " (" + m.Start.In(loc).Format("2006-01-02 15:04") + ")"
		if len(m.Nearly) > 0 {
			names := make([]string, 0, len(m.Nearly))
			for _, r := range m.Nearly[:min(len(m.Nearly), 3)] {
				names = append(names, verdict.Clean(r))
			}
			item += " " + tr(l, "nearly caught by %s", strings.Join(names, ", "))
		}
		items = append(items, item)
	}
	line := tr(l, "Incidents: %d; no alert caught %d: %s.", n, len(missed), strings.Join(items, "; "))
	if k := len(missed) - len(items); k > 0 {
		line += " " + tr(l, "And %d more.", k)
	}
	return line, nums, nil
}

// nearKept are the near misses kept for the weeks that overlap [from, to).
func (s *Server) nearKept(ctx context.Context, from, to time.Time) ([]verdict.NearMiss, error) {
	rs, err := s.l.ByPeriod(ctx, "near_misses", from.AddDate(0, 0, -7))
	if err != nil {
		return nil, err
	}
	var out []verdict.NearMiss
	for _, r := range rs {
		if !r.From.Before(to) || !r.To.After(from) {
			continue
		}
		var nms []verdict.NearMiss
		if err := r.Decode(&nms); err != nil {
			return nil, err
		}
		out = append(out, nms...)
	}
	return out, nil
}

// woke is how many times a rule woke people for nothing.
type woke struct {
	Rule string `json:"rule"`
	N    int    `json:"n"`
}

// wokenSection: what came while people slept and caught nothing, by rule:
// the notifications, once they are taken for the whole span; the firings
// before that.
func (s *Server) wokenSection(ctx context.Context, rep *verdict.Report, from, to time.Time, o verdict.Options, in func(string) bool) (string, any, error) {
	l := s.cfg.Lang
	own, err := s.owners(ctx)
	if err != nil {
		return "", nil, err
	}
	sc := own.schedules(o.Hours)
	if sc.none() {
		return tr(l, "No one's hours say when people sleep, so nothing is counted as waking them: --sleep-hours, or a team's."), nil, nil
	}
	p, err := s.notified(ctx)
	if err != nil {
		return "", nil, err
	}
	scope := map[string]bool{}
	for _, r := range rep.Rules {
		scope[r.Rule] = in(r.Rule)
	}
	by := map[string]int{}
	what := "firings"
	if !p.first.IsZero() && !p.first.After(from) {
		what = "notifications"
		for _, pg := range p.pages {
			if pg.At.Before(from) || !pg.At.Before(to) || !scope[pg.Rule] || pg.Stone != ledger.PageBlack {
				continue
			}
			if h := sc.of(pg.Rule); h != nil && h.Of(pg.At) == verdict.Asleep {
				by[pg.Rule]++
			}
		}
	} else {
		for _, r := range rep.Rules {
			if scope[r.Rule] && r.AsleepBlack > 0 {
				by[r.Rule] = r.AsleepBlack
			}
		}
	}
	var ws []woke
	total := 0
	for rule, n := range by {
		ws = append(ws, woke{rule, n})
		total += n
	}
	slices.SortFunc(ws, func(a, b woke) int { return cmp.Or(cmp.Compare(b.N, a.N), cmp.Compare(a.Rule, b.Rule)) })
	nums := map[string]any{"counted": what, "total": total, "rules": orNoneOf(ws)}
	if total == 0 {
		return tr(l, "Nothing woke people for nothing."), nums, nil
	}
	items := make([]string, 0, 10)
	for _, w := range ws[:min(len(ws), 10)] {
		items = append(items, fmt.Sprintf("%s (%d)", verdict.Clean(w.Rule), w.N))
	}
	line := tr(l, "Firings that woke people for nothing: %d. By rule: %s.", total, strings.Join(items, ", "))
	if what == "notifications" {
		line = tr(l, "Notifications that woke people for nothing: %d. By rule: %s.", total, strings.Join(items, ", "))
	}
	if k := len(ws) - len(items); k > 0 {
		line += " " + tr(l, "And %d more.", k)
	}
	return line, nums, nil
}

// replaySection: the replay beside what really fired over the span, by the
// ledger's days, and the rules that differ.
func (s *Server) replaySection(ctx context.Context, from, to, checkedTo time.Time, loc *time.Location, in func(string) bool) (string, any, error) {
	l := s.cfg.Lang
	if !s.checking() {
		return tr(l, "The replay isn't set beside what really fired: --check-replay."), nil, nil
	}
	upTo := to
	if checkedTo.Before(to) {
		upTo = checkedTo
	}
	all, err := s.l.CheckTotal(ctx, from, upTo, in)
	if err != nil {
		return "", nil, err
	}
	cs, err := s.l.Checks(ctx, from, upTo)
	if err != nil {
		return "", nil, err
	}
	d := s.differs()
	names := orNoneOf(ledger.Differing(cs, in, d.N, d.Ratio))
	nums := map[string]any{"checked_to": checkedTo.UTC(), "firings": all.Either(), "alike": all.Share(), "differ": names}
	var line string
	if all.Either() == 0 {
		line = tr(l, "Nothing was set beside what really fired in the span.")
	} else {
		line = tr(l, "The replay beside what really fired: %d%% alike, of %d firings.", int(math.Round(all.Share()*100)), all.Either())
		if len(names) > 0 {
			differ := make([]string, 0, len(names))
			for _, n := range names {
				differ = append(differ, verdict.Clean(n))
			}
			line += " " + tr(l, "Replayed differently from how they really fired: %s.", strings.Join(differ, ", "))
		}
	}
	if checkedTo.Before(to) {
		line += " " + tr(l, "Set beside what really fired up to %s.", checkedTo.In(loc).Format("2006-01-02 15:04"))
	}
	return line, nums, nil
}

// suggestionsSection: the longer fors the weekly digest worked out last,
// and the spare rules over twelve weeks; for a week or longer only, as a
// shorter span is too short to tell.
func (s *Server) suggestionsSection(ctx context.Context, from, to time.Time, in func(string) bool) (string, any, error) {
	if to.Sub(from) < 7*24*time.Hour-time.Hour { // a week across a change of the clocks
		return "", nil, nil
	}
	l := s.cfg.Lang
	var sugg []verdict.Suggestion
	rs, err := s.l.Results(ctx, "suggestions", 1)
	if err != nil {
		return "", nil, err
	}
	if len(rs) > 0 {
		if err := rs[0].Decode(&sugg); err != nil {
			return "", nil, err
		}
	}
	sugg = slices.DeleteFunc(sugg, func(x verdict.Suggestion) bool { return !in(x.Rule) })
	spare, err := s.spareRules(ctx)
	if err != nil {
		return "", nil, err
	}
	spare = slices.DeleteFunc(spare, func(name string) bool { return !in(name) })
	type suggestion struct {
		Rule   string `json:"rule"`
		For    string `json:"for"`
		NewFor string `json:"new_for"`
		Fired  int    `json:"fired"`
		Would  int    `json:"would_fire"`
	}
	out := []suggestion{}
	var items []string
	for _, x := range sugg {
		out = append(out, suggestion{x.Rule, verdict.Duration(x.For), verdict.Duration(x.NewFor), x.Before.Firings, x.After.Firings})
		if len(items) < 10 {
			items = append(items, tr(l, "%s, for %s → %s (%d → %d firings)", verdict.Clean(x.Rule), verdict.Duration(x.For), verdict.Duration(x.NewFor), x.Before.Firings, x.After.Firings))
		}
	}
	nums := map[string]any{"suggestions": out, "spare_rules": orNoneOf(spare)}
	var lines []string
	if len(items) > 0 {
		lines = append(lines, tr(l, "Longer fors that would have caught the same with fewer firings: %s.", strings.Join(items, "; ")))
	}
	if len(spare) > 0 {
		names := make([]string, 0, 20)
		for _, name := range spare[:min(len(spare), 20)] {
			names = append(names, verdict.Clean(name))
		}
		line := tr(l, "Over twelve weeks, another rule caught every incident these caught, as soon, at the same severity or above: %s.", strings.Join(names, ", "))
		if n := len(spare) - len(names); n > 0 {
			line += " " + tr(l, "And %d more.", n)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return tr(l, "No suggestions."), nums, nil
	}
	return strings.Join(lines, " "), nums, nil
}

// preview is a report as it would be sent now, for the last span it fell
// due for, sent nowhere.
func (s *Server) preview(ctx context.Context, r ledger.Scheduled) (*built, error) {
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	team, ok := reportTeam(r, own)
	if !ok {
		return nil, bad("its team was removed: the report stops until it names another")
	}
	sch, err := r.When()
	if err != nil {
		return nil, err
	}
	o, err := s.options(ctx)
	if err != nil {
		return nil, err
	}
	loc := s.reportLoc(r, team)
	due := sch.Last(s.now(), loc)
	from, to := sch.Span(due, loc)
	firings, checked, err := s.ledgerTo(ctx)
	if err != nil {
		return nil, err
	}
	// Up to 31 days, the firings are read again to count them by the
	// hours they came in, for this span and the one before: one such
	// preview at a time, as /api/v1/report?firings=false takes its turn.
	if to.Sub(from) <= ledger.HoursLongest && s.countsHours(ctx) {
		done, err := s.busy(ctx)
		if err != nil {
			return nil, err
		}
		defer done()
	}
	return s.buildReport(ctx, r, team, sch, loc, due, from, to, o, firings, checked)
}
