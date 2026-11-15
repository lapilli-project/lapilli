package serve

import (
	"context"
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
// keeps deploys, how many came during one. A replay that failed, or ran out
// of the digest's time, leaves what it would have said unknown, and err
// says why; the rest is kept.
func (s *Server) unrecordedOf(ctx context.Context, in replayInput, rep *verdict.Report, from, to time.Time) ([]verdict.Unrecorded, error) {
	if !rep.NoRecord || len(in.versions) == 0 {
		return nil, nil
	}
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	fired := func(rule string) ([]firing.Firing, error) {
		fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: from, To: to, Rule: rule})
		var out []firing.Firing
		for _, f := range fs {
			if !f.Excused { // as the rule's stones count them
				out = append(out, f.Firing)
			}
		}
		return out, err
	}
	us, failed := source.Unrecorded(ctx, s.h, in.versions, rep, fired, own.labelTeam, s.cfg.Step)
	deploys, err := s.l.HasDeploys(ctx)
	if err != nil {
		return nil, err
	}
	during := map[string]int{}
	for _, r := range rep.Rules {
		during[r.Rule] = r.DuringDeploys
	}
	for i := range us {
		us[i].Team = own.teamOf(us[i].Rule)
		if deploys {
			n := during[us[i].Rule]
			us[i].DuringDeploys = &n
		}
	}
	return us, failed
}

// unrecordedWeek is what the last digest said of the rules that fired often
// with no record, by rule, and the week it said it of; none when its record
// is kept since. t's weeks say whether it is, when they hold the week.
func (s *Server) unrecordedWeek(ctx context.Context, t *tallies) (map[string]verdict.Unrecorded, time.Time, error) {
	rs, err := s.l.Results(ctx, "unrecorded", 1)
	if err != nil || len(rs) == 0 {
		return nil, time.Time{}, err
	}
	var us []verdict.Unrecorded
	if err := rs[0].Decode(&us); err != nil || len(us) == 0 {
		return nil, time.Time{}, err
	}
	kept, ok := t.kept[ledger.Week(s.l.Day(rs[0].From))]
	if !ok {
		if kept, err = s.l.RecordKept(ctx, rs[0].From, rs[0].To); err != nil {
			return nil, time.Time{}, err
		}
	}
	if kept {
		return nil, time.Time{}, nil
	}
	out := make(map[string]verdict.Unrecorded, len(us))
	for _, u := range us {
		out[u.Rule] = u
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
