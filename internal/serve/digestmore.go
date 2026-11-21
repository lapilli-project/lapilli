package serve

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// What the weekly digest says of firings copied in from elsewhere and of
// notifications as they were sent, and the notice when a source goes
// quiet: so what Slack and the webhook get covers them too.

// sourceQuiet is how long a source may go without a write before it is
// told of, when --source-quiet doesn't say.
const sourceQuiet = 26 * time.Hour

// copiedIn is a source as the digest tells of it: its firings in the
// week, and whether it has gone quiet.
type copiedIn struct {
	Name    string    `json:"name"`
	Firings int       `json:"firings"`
	Written time.Time `json:"written"`
	Quiet   bool      `json:"quiet"`
}

// copied are the sources firings are copied in from, with their rules'
// firings in the report.
func (s *Server) copied(ctx context.Context, rep *verdict.Report) ([]copiedIn, error) {
	srcs, err := s.l.Sources(ctx)
	if err != nil || len(srcs) == 0 {
		return nil, err
	}
	out := make([]copiedIn, 0, len(srcs))
	for _, src := range srcs {
		c := copiedIn{Name: src.Name, Written: src.Written, Quiet: s.quiet(src)}
		for _, r := range rep.Rules {
			if r.Source == src.Name {
				c.Firings += r.Firings + r.Excused
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// quiet reports whether nothing has been copied in from a source for
// longer than --source-quiet.
func (s *Server) quiet(src ledger.Source) bool {
	return s.now().Sub(src.Written) > s.quietAfter()
}

// sourcesLine says where firings were copied in from, and which source has
// gone quiet; "" for none.
func (s *Server) sourcesLine(cs []copiedIn) string {
	if len(cs) == 0 {
		return ""
	}
	l := s.cfg.Lang
	var items, quiet []string
	for _, c := range cs {
		items = append(items, tr(l, "%s, %d firings", c.Name, c.Firings))
		if c.Quiet {
			quiet = append(quiet, tr(l, "Nothing has been copied in from %s since %s.", c.Name, c.Written.In(s.cfg.Loc).Format("2006-01-02 15:04")))
		}
	}
	line := tr(l, "Firings copied in from elsewhere, judged with the rest: %s.", strings.Join(items, "; "))
	if len(quiet) > 0 {
		line += " " + strings.Join(quiet, " ")
	}
	return line
}

// sentLine is what the notifications sent in the week came to, for the
// rules in the report.
type sentLine struct {
	Notifications int            `json:"notifications"`
	Rules         int            `json:"rules"`
	Repeats       int            `json:"repeats"`
	Asleep        int            `json:"asleep"`
	ForNothing    int            `json:"for_nothing"`
	Untied        int            `json:"untied"`
	ByReceiver    map[string]int `json:"by_receiver"`
	// Most are the rules that sent the most for firings that caught
	// nothing, the most first, five at most.
	Most []sentFor `json:"most_for_nothing"`
	// NoRecord says no incident was written down to judge the span's
	// firings by: those that caught nothing aren't known to be for nothing;
	// Unjudged, that some of them had none, as over a span that isn't the
	// ledger's weeks.
	NoRecord bool `json:"no_record,omitempty"`
	Unjudged bool `json:"unjudged,omitempty"`
	// ByNothing says some of them are of rules an entry judges by
	// nothing (judged: false), which Unjudged counts too, as it always
	// has: the line says so, not that they had no incident (bundle 67).
	ByNothing bool `json:"by_nothing,omitempty"`
	// noIncident is Unjudged less those: some had no incident to judge
	// them by. The line says why from it; the numbers keep Unjudged's
	// meaning.
	noIncident bool
	// PagedNoOne are the span's firings no notification told of, muted
	// by a silence, an inhibition or either, or not (docs/design/mutes.md):
	// given once the ledger keeps a mute.
	PagedNoOne *ledger.Unpaged `json:"paged_no_one,omitempty"`
}

type sentFor struct {
	Rule string `json:"rule"`
	N    int    `json:"notifications"`
}

// sent is what the notifications that came in [from, to) came to, for the
// rules in the report, counted asleep by each rule's team's hours; nil
// when none had come by to.
func (s *Server) sent(ctx context.Context, rep *verdict.Report, from, to time.Time) (*sentLine, error) {
	p, err := s.notified(ctx)
	if err != nil || p.first.IsZero() || !p.first.Before(to) {
		return nil, err
	}
	o, err := s.options(ctx)
	if err != nil {
		return nil, err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	sc := own.schedules(o.Hours)
	in := make(map[string]bool, len(rep.Rules))
	for _, r := range rep.Rules {
		in[r.Rule] = true
	}
	out := &sentLine{ByReceiver: map[string]int{}, NoRecord: rep.NoRecord}
	out.Unjudged = slices.ContainsFunc(rep.Rules, func(r verdict.RuleResult) bool { return r.Unjudged > 0 })
	out.noIncident, out.ByNothing = unjudgedWhy(rep.Rules)
	for rule, x := range p.lines(from, to, sc.of) {
		if !in[rule] {
			continue
		}
		out.Rules++
		out.Notifications += x.Notifications
		out.Repeats += x.Repeats
		out.Asleep += x.NotifiedAsleep
		out.ForNothing += x.ForNothing
		out.Untied += x.Untied
		for k, v := range x.ByReceiver {
			out.ByReceiver[k] += v
		}
		if x.ForNothing > 0 {
			out.Most = append(out.Most, sentFor{rule, x.ForNothing})
		}
	}
	slices.SortFunc(out.Most, func(a, b sentFor) int { return cmp.Or(cmp.Compare(b.N, a.N), cmp.Compare(a.Rule, b.Rule)) })
	out.Most = out.Most[:min(len(out.Most), 5)]
	// The firings that paged no one, of the weeks the notifications read
	// tell of (docs/design/mutes.md).
	if from := later(from, p.from); p.mutes && from.Before(to) {
		u, err := s.l.Unpaged(ctx, from, to, func(rule string) bool { return in[rule] }, p.toldOf, p.counted, p.within, s.now())
		if err != nil {
			return nil, err
		}
		out.PagedNoOne = &u
	}
	return out, nil
}

// text says it in a line, in --lang's language: nothing for none.
func (x *sentLine) text(l string) string {
	if x == nil {
		return ""
	}
	var parts []string
	if x.Notifications > 0 {
		parts = append(parts, x.sentText(l))
	}
	if u := x.PagedNoOne; u != nil && u.Total() > 0 {
		line := tr(l, "Firings that paged no one: %d muted by a silence, %d inhibited by another alert, %d with no mute told of.", u.Silence, u.Inhibition, u.Nothing)
		if u.Either > 0 {
			line = tr(l, "Firings that paged no one: %d muted by a silence, %d inhibited by another alert, %d muted by a silence or an inhibition, %d with no mute told of.",
				u.Silence, u.Inhibition, u.Either, u.Nothing)
		}
		if u.Partial {
			line += " " + tr(l, "Mutes were too many to look through for every firing: those of some rules weren't counted.")
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, " ")
}

// sentText says what the notifications sent came to.
func (x *sentLine) sentText(l string) string {
	line := tr(l, "Notifications sent, as the alert managers sent them: %d, for %d rules; %d repeats, %d while people slept, %d for firings that caught nothing.",
		x.Notifications, x.Rules, x.Repeats, x.Asleep, x.ForNothing)
	switch {
	case x.NoRecord && x.ByNothing:
		line = tr(l, "Notifications sent, as the alert managers sent them: %d, for %d rules; %d repeats, %d while people slept, %d for firings with no incident written down to judge them by, or judged by nothing.",
			x.Notifications, x.Rules, x.Repeats, x.Asleep, x.ForNothing)
	case x.NoRecord:
		line = tr(l, "Notifications sent, as the alert managers sent them: %d, for %d rules; %d repeats, %d while people slept, %d for firings with no incident written down to judge them by.",
			x.Notifications, x.Rules, x.Repeats, x.Asleep, x.ForNothing)
	case x.noIncident && x.ByNothing:
		line = tr(l, "Notifications sent, as the alert managers sent them: %d, for %d rules; %d repeats, %d while people slept, %d for firings that caught nothing, had no incident written down to judge them by, or were judged by nothing.",
			x.Notifications, x.Rules, x.Repeats, x.Asleep, x.ForNothing)
	case x.ByNothing:
		line = tr(l, "Notifications sent, as the alert managers sent them: %d, for %d rules; %d repeats, %d while people slept, %d for firings that caught nothing, or were judged by nothing.",
			x.Notifications, x.Rules, x.Repeats, x.Asleep, x.ForNothing)
	case x.noIncident:
		line = tr(l, "Notifications sent, as the alert managers sent them: %d, for %d rules; %d repeats, %d while people slept, %d for firings that caught nothing, or had no incident written down to judge them by.",
			x.Notifications, x.Rules, x.Repeats, x.Asleep, x.ForNothing)
	}
	if len(x.Most) > 0 {
		var names []string
		for _, m := range x.Most {
			names = append(names, fmt.Sprintf("%s (%d)", verdict.Clean(m.Rule), m.N))
		}
		if x.NoRecord || x.Unjudged || x.ByNothing {
			line += " " + tr(l, "The most for those firings: %s.", strings.Join(names, ", "))
		} else {
			line += " " + tr(l, "The most for nothing: %s.", strings.Join(names, ", "))
		}
	}
	return line
}

// tellQuiet tells, once each time it happens, of a source nothing has been
// copied in from for longer than --source-quiet: the job copying from it
// may have stopped.
func (s *Server) tellQuiet(ctx context.Context) error {
	dests := s.toldTo()
	if len(dests) == 0 || s.cfg.FiringsToken == "" {
		return nil
	}
	srcs, err := s.l.Sources(ctx)
	if err != nil {
		return err
	}
	for _, src := range srcs {
		if !s.quiet(src) {
			continue
		}
		told := make([]ledger.Told, 0, len(dests))
		for _, d := range dests {
			told = append(told, ledger.Told{Dest: d, About: map[string]any{"source": src}})
		}
		// Once for each last write: written again and quiet again, told
		// again.
		if err := s.l.TellOnce(ctx, "quiet-told."+src.Name, fmt.Sprint(src.Written.Unix()), "source.quiet", told); err != nil {
			return err
		}
	}
	return nil
}

// aboutMore adds to what is told of a digest, everyone's (team nil) or a
// team's, the sources firings were copied in from (for everyone's), what
// the notifications sent came to, the re-fires, and the marked windows on
// in the week.
func (s *Server) aboutMore(ctx context.Context, about map[string]any, rep *verdict.Report, from, to time.Time, team *ledger.Team) error {
	if team == nil {
		cs, err := s.copied(ctx, rep)
		if err != nil {
			return err
		}
		if cs != nil {
			about["sources"] = cs
		}
		if s.checking() {
			checks, err := s.l.Checks(ctx, from, to)
			if err != nil {
				return err
			}
			if len(checks) > 0 {
				about["replay_check"] = checks
			}
		}
	}
	sent, err := s.sent(ctx, rep, from, to)
	if err != nil {
		return err
	}
	if sent != nil {
		about["notifications"] = sent
	}
	rf, err := s.refiresOf(ctx, rep, from, to)
	if err != nil {
		return err
	}
	about["refired"] = rf
	// The marked windows on in the week, when one was.
	if mk, err := s.markedOfReport(ctx, rep, from, to, team, s.cfg.Loc); err != nil {
		return err
	} else if mk != nil {
		about["marked"] = mk
	}
	return nil
}

// copiedTo is how far the ledger has firings copied in up to, with no runs:
// now, or the end of the last period written by the source that is
// furthest behind, of those not gone quiet.
func (s *Server) copiedTo(ctx context.Context) (time.Time, error) {
	srcs, err := s.l.Sources(ctx)
	if err != nil {
		return time.Time{}, err
	}
	to := s.now()
	for _, src := range srcs {
		if !s.quiet(src) && src.To.Before(to) {
			to = src.To
		}
	}
	return to, nil
}

// unjudgedWhy says whether some of rules' firings weren't judged for want
// of an incident (noRecord), and whether some were of rules an entry
// judges by nothing (byNothing): a rule's Unjudged counts both, its
// NotJudged the second alone.
func unjudgedWhy(rules []verdict.RuleResult) (noRecord, byNothing bool) {
	for _, r := range rules {
		noRecord = noRecord || r.Unjudged > r.NotJudged
		byNothing = byNothing || r.NotJudged > 0
	}
	return noRecord, byNothing
}
