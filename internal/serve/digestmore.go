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
	after := s.cfg.SourceQuiet
	if after <= 0 {
		after = sourceQuiet
	}
	return s.now().Sub(src.Written) > after
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
	out := &sentLine{ByReceiver: map[string]int{}}
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
	return out, nil
}

// text says it in a line, in --lang's language: nothing for none.
func (x *sentLine) text(l string) string {
	if x == nil || x.Notifications == 0 {
		return ""
	}
	line := tr(l, "Notifications sent, as the alert managers sent them: %d, for %d rules; %d repeats, %d while people slept, %d for firings that caught nothing.",
		x.Notifications, x.Rules, x.Repeats, x.Asleep, x.ForNothing)
	if len(x.Most) > 0 {
		var names []string
		for _, m := range x.Most {
			names = append(names, fmt.Sprintf("%s (%d)", verdict.Clean(m.Rule), m.N))
		}
		line += " " + tr(l, "The most for nothing: %s.", strings.Join(names, ", "))
	}
	return line
}

// tellQuiet tells, once each time it happens, of a source nothing has been
// copied in from for longer than --source-quiet: the job copying from it
// may have stopped.
func (s *Server) tellQuiet(ctx context.Context) error {
	dests := s.dests()
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

// aboutMore adds to what is told of a digest the sources firings were
// copied in from (for everyone's) and what the notifications sent came to.
func (s *Server) aboutMore(ctx context.Context, about map[string]any, rep *verdict.Report, from, to time.Time, everyone bool) error {
	if everyone {
		cs, err := s.copied(ctx, rep)
		if err != nil {
			return err
		}
		if cs != nil {
			about["sources"] = cs
		}
	}
	sent, err := s.sent(ctx, rep, from, to)
	if err != nil {
		return err
	}
	if sent != nil {
		about["notifications"] = sent
	}
	return nil
}
