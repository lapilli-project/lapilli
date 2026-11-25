package serve

import (
	"context"
	"fmt"
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
func (s *Server) unrecordedOf(ctx context.Context, in replayInput, rep *verdict.Report, from, to time.Time, scope string, spans [][2]time.Time) ([]verdict.Unrecorded, error) {
	if rep.Unjudged == 0 || len(in.versions) == 0 {
		return nil, nil
	}
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	us, failed := source.Unrecorded(ctx, s.h, in.versions, rep, weekUnjudged{s, own, from, to, spans}, s.cfg.Step)
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
	// spans are the stretches of the week whose record isn't kept, when
	// the week's days are judged apart: only the firings in them are with
	// no record. None, the whole week.
	spans [][2]time.Time
}

// stretches are where the firings with no record are: spans, or the week.
func (w weekUnjudged) stretches() [][2]time.Time {
	if w.spans != nil {
		return w.spans
	}
	return [][2]time.Time{{w.from, w.to}}
}

func (w weekUnjudged) Firings(ctx context.Context, rule string, most int) ([]firing.Firing, bool, error) {
	var out []firing.Firing
	more := false
	for _, sp := range w.stretches() {
		fs, err := w.s.l.Firings(ctx, ledger.FiringQuery{From: sp[0], To: sp[1], Rule: rule, Black: true, Limit: most + 1})
		if err != nil {
			return nil, false, err
		}
		for _, f := range fs {
			if len(out) >= most {
				more = true
				break
			}
			out = append(out, f.Firing)
		}
	}
	return out, more, nil
}

func (w weekUnjudged) Alerts(ctx context.Context, rule string, most int) ([]verdict.AlertCount, int, error) {
	// The alerts of each stretch put together by their labels.
	by := map[string]*verdict.AlertCount{}
	var order []string
	for _, sp := range w.stretches() {
		as, _, err := w.s.l.RuleAlerts(ctx, rule, sp[0], sp[1], 1<<30)
		if err != nil {
			return nil, 0, err
		}
		for _, a := range as {
			k := fmt.Sprint(a.Labels)
			if by[k] == nil {
				by[k] = &verdict.AlertCount{Labels: a.Labels, Team: w.own.labelTeam(a.Labels)}
				order = append(order, k)
			}
			by[k].Firings += a.Firings
		}
	}
	out := make([]verdict.AlertCount, 0, len(order))
	for _, k := range order {
		out = append(out, *by[k])
	}
	slices.SortStableFunc(out, func(a, b verdict.AlertCount) int { return b.Firings - a.Firings })
	n := len(out)
	return out[:min(len(out), most)], n, nil
}

// unrecordedWeek is what the last digest said of the rules that fired often
// with no record, by rule, and the week it said it of: those said for
// everyone, or, of t's kept to a team, for it. One that doesn't decode is
// none.
func (s *Server) unrecordedWeek(ctx context.Context, t *tallies) (map[string]verdict.Unrecorded, time.Time, error) {
	rs, err := s.l.Results(ctx, "unrecorded", 1)
	if err != nil || len(rs) == 0 {
		return nil, time.Time{}, err
	}
	var us []verdict.Unrecorded
	if !s.decodeKept(rs[0], &us) || len(us) == 0 {
		return nil, time.Time{}, nil
	}
	// Shown while the rule's firings are unjudged (ruleLines): by the days
	// of the week some of which may be kept.
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

// unrecordedTeams is unrecordedOf of the week's report, for everyone: of the
// rules none of whose firings were judged, those of the days with no
// record; and, of each team's rules whose firings its own record, by what
// it owns, judges less than the ledger's does, the same, scoped to it.
// Reads and replays have rctx.
func (s *Server) unrecordedTeams(ctx, rctx context.Context, in replayInput, rep *verdict.Report, o verdict.Options, from, to time.Time) ([]verdict.Unrecorded, error) {
	rec, err := s.l.RecordOf(ctx, from, to, o, nil)
	if err != nil {
		return nil, err
	}
	out, err := s.unrecordedOf(rctx, in, rep, from, to, "", s.l.UnkeptSpans(rec, o, from, to))
	if err != nil || rep.NoRecord {
		return out, err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	everyone := map[string]int{}
	for _, r := range rep.Rules {
		everyone[r.Rule] = r.Unjudged
	}
	for _, t := range own.teams {
		if len(own.rulesOf(t.Name)) == 0 {
			continue
		}
		keeps := teamKeeps(own, &t)
		if keeps == nil {
			continue // judged by everyone's
		}
		part, err := s.l.ReportFor(rctx, from, to, o, false, keeps)
		if err != nil {
			return out, err
		}
		// Of its own rules that its record judges less than everyone's.
		part.Rules = slices.DeleteFunc(part.Rules, func(r verdict.RuleResult) bool {
			return own.rule[r.Rule] != t.ID || r.Unjudged <= everyone[r.Rule]
		})
		if len(part.Rules) == 0 {
			continue
		}
		trec, err := s.l.RecordOf(ctx, from, to, o, keeps)
		if err != nil {
			return out, err
		}
		us, err := s.unrecordedOf(rctx, in, part, from, to, t.Name, s.l.UnkeptSpans(trec, o, from, to))
		out = append(out, us...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}
