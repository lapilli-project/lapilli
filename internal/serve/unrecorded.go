package serve

import (
	"context"
	"slices"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// A week with no record calls no rule noisy (record.go). For each rule
// replayed that fired as often as a noisy rule, the weekly digest works out
// instead what its firings were (verdict.Unrecorded), and keeps it as the
// week's "unrecorded" result: the pages read it from there, never from the
// firings. A week whose record is kept since, by an incident written down
// later, no longer shows it.

// unrecordedOf is what the firings of the rules that fired often in a week
// with no record were: by source.Unrecorded, each rule's team, and each
// alert's, the team whose labels it has, beside them, and, once the ledger
// keeps deploys, how many came during one. scope is the team whose record
// rep was judged by, "" for the ledger's. Its reads and replays have ctx,
// the digest's --timeout: what didn't fit in it is left unknown, and err
// says why; the rest is kept.
func (s *Server) unrecordedOf(ctx context.Context, in replayInput, rep *verdict.Report, from, to time.Time, scope string) ([]verdict.Unrecorded, error) {
	if rep.Unjudged == 0 || len(in.versions) == 0 {
		return nil, nil
	}
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	us, failed := source.Unrecorded(ctx, s.h, in.versions, rep, weekUnjudged{s, own, from, to}, s.cfg.Step)
	deploys, err := s.l.HasDeploys(ctx)
	if err != nil {
		return us, err
	}
	during := map[string]int{}
	for _, r := range rep.Rules {
		during[r.Rule] = r.DuringDeploys - r.DeploysCaught
	}
	for i := range us {
		us[i].Team, us[i].Scope = own.teamOf(us[i].Rule), scope
		if deploys {
			n := during[us[i].Rule]
			us[i].DuringDeploys = &n
		}
	}
	return us, failed
}

// weekUnjudged are, as a source.UnrecordedFrom, a week's firings with no
// record: those that caught nothing, as the week's record isn't kept, read
// from the ledger as many as asked, counted by alert there.
type weekUnjudged struct {
	s        *Server
	own      *owners
	from, to time.Time
}

func (w weekUnjudged) Firings(ctx context.Context, rule string, most int) ([]firing.Firing, bool, error) {
	fs, err := w.s.l.Firings(ctx, ledger.FiringQuery{From: w.from, To: w.to, Rule: rule, Black: true, Limit: most + 1})
	if err != nil {
		return nil, false, err
	}
	out := make([]firing.Firing, 0, min(len(fs), most))
	for _, f := range fs[:min(len(fs), most)] {
		out = append(out, f.Firing)
	}
	return out, len(fs) > most, nil
}

func (w weekUnjudged) Alerts(ctx context.Context, rule string, most int) ([]verdict.AlertCount, int, error) {
	as, n, err := w.s.l.RuleAlerts(ctx, rule, w.from, w.to, most)
	out := make([]verdict.AlertCount, len(as))
	for i, a := range as {
		out[i] = verdict.AlertCount{Labels: a.Labels, Firings: a.Firings, Team: w.own.labelTeam(a.Labels)}
	}
	return out, n, err
}

// unrecordedWeek is what the last digest said of the rules that fired often
// with no record, by rule, and the week it said it of: those said for
// everyone, or, of t's kept to a team, for it; none when the week's record
// is kept since, as t's weeks are judged. One that doesn't decode is none.
func (s *Server) unrecordedWeek(ctx context.Context, t *tallies) (map[string]verdict.Unrecorded, time.Time, error) {
	rs, err := s.l.Results(ctx, "unrecorded", 1)
	if err != nil || len(rs) == 0 {
		return nil, time.Time{}, err
	}
	var us []verdict.Unrecorded
	if !s.decodeKept(rs[0], &us) || len(us) == 0 {
		return nil, time.Time{}, nil
	}
	kept, err := s.keptWeek(ctx, t, rs[0].From, rs[0].To)
	if err != nil || kept {
		return nil, time.Time{}, err
	}
	scope := ""
	if t.team != nil {
		scope = t.team.Name
	}
	out := make(map[string]verdict.Unrecorded, len(us))
	for _, u := range us {
		if u.Scope == "" || u.Scope == scope {
			out[u.Rule] = u
		}
	}
	return out, rs[0].From, nil
}

// unrecordedView is what the last digest said of a rule that fired often
// with no record, as its page and the rules list show it: in the week of
// Week; Known says how many went well past its bound, Clear, is known.
type unrecordedView struct {
	verdict.Unrecorded
	Week         time.Time
	Known        bool
	Clear        int
	Deploys      int
	DeploysKnown bool
}

func unrecordedViewOf(u verdict.Unrecorded, week time.Time) *unrecordedView {
	v := &unrecordedView{Unrecorded: u, Week: week}
	if u.Clear != nil {
		v.Known, v.Clear = true, *u.Clear
	}
	if u.DuringDeploys != nil {
		v.DeploysKnown, v.Deploys = true, *u.DuringDeploys
	}
	return v
}

// unrecordedTeams is unrecordedOf of the week's report, for everyone; and,
// when the ledger's record of it is kept, of each team's rules whose team's
// own isn't, by what it owns, scoped to it. Reads and replays have rctx.
func (s *Server) unrecordedTeams(ctx, rctx context.Context, in replayInput, rep *verdict.Report, o verdict.Options, from, to time.Time) ([]verdict.Unrecorded, error) {
	if rep.NoRecord {
		return s.unrecordedOf(rctx, in, rep, from, to, "")
	}
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	var out []verdict.Unrecorded
	for _, t := range own.teams {
		if len(own.rulesOf(t.Name)) == 0 {
			continue
		}
		part, err := s.l.ReportFor(rctx, from, to, o, false, teamKeeps(own, &t))
		if err != nil {
			return out, err
		}
		if !part.NoRecord {
			continue
		}
		part.Rules = slices.DeleteFunc(part.Rules, func(r verdict.RuleResult) bool { return own.rule[r.Rule] != t.ID })
		us, err := s.unrecordedOf(rctx, in, part, from, to, t.Name)
		out = append(out, us...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}
