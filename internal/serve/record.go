package serve

import (
	"context"
	"log/slog"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// lapilli serve judges a firing by its week (verdict.Record): a week's
// firings are judged when an incident its firings are judged against was on
// in it or in the two weeks before it, or the impact signal was read over
// all of it (verdict.Options.ImpactFrom); else they are counted, not judged.
// A span of weeks is judged by its weeks, each by its own: what the rules
// list and a rule's page say of four weeks is the sum of what the overview
// says of each. What it replays is judged so too (options sets
// verdict.Options.RecordWeek), so that it compares like with like. With a
// team picked, by the incidents it owns; a team with no labels owns none
// but those its rules caught, and is judged by everyone's (teamKeeps). The
// tallies read what keeps the record once, with them, never the firings.

// readRecord reads, for t's weeks, what keeps their record: the incidents on
// from two weeks before the first to the end of the last.
func (s *Server) readRecord(ctx context.Context, t *tallies) error {
	first, last := t.weeks[0], t.weeks[len(t.weeks)-1]
	incs, err := s.l.RecordIncidents(ctx, verdict.RecordFrom(s.l.Midnight(first), s.l.Midnight(last+7)), s.l.Midnight(last+7), t.o.UnknownLen)
	if err != nil {
		return err
	}
	t.recs, t.mid, t.at = incs, s.l.Midnight, s.now()
	t.keep(nil)
	return nil
}

// keep judges t's weeks by the incidents keeps says keep their record,
// every one when nil.
func (t *tallies) keep(keeps func(ledger.Incident) bool) {
	var plain []incident.Incident
	for _, in := range t.recs {
		if keeps == nil || keeps(in) {
			plain = append(plain, in.Incident)
		}
	}
	t.rec = verdict.RecordOf(plain, t.o)
	t.kept = make(map[int64]bool, len(t.weeks))
	for _, w := range t.weeks {
		t.kept[w] = t.rec.Kept(t.mid(w), t.mid(w+7))
	}
}

// keptWeek reports whether the record of the week of from is kept, as the
// tallies have it when they hold that week, or else as the ledger does,
// for everyone or the team t's are kept to.
func (s *Server) keptWeek(ctx context.Context, t *tallies, from, to time.Time) (bool, error) {
	if kept, ok := t.kept[ledger.Week(s.l.Day(from))]; ok {
		return kept, nil
	}
	return s.spanKept(ctx, from, to, t.team)
}

// spanKept reports whether the record of one of [from, to)'s weeks is kept:
// by every incident, or, with team, by those it owns (teamKeeps).
func (s *Server) spanKept(ctx context.Context, from, to time.Time, team *ledger.Team) (bool, error) {
	o, err := s.options(ctx)
	if err != nil {
		return false, err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return false, err
	}
	keeps := teamKeeps(own, team)
	if keeps == nil {
		return s.l.RecordKept(ctx, from, to, o)
	}
	rec, err := s.l.RecordOf(ctx, from, to, o, keeps)
	if err != nil {
		return false, err
	}
	for _, w := range s.l.SpanWeeks(from, to) {
		if s.l.WeekKept(rec, w) {
			return true, nil
		}
	}
	return false, nil
}

// teamKeeps is what keeps a team's record: the incidents it owns; nil, for
// none picked, every one. A team with no labels owns no incident by them,
// only those its rules caught, so its rules' firings that caught nothing
// would never have a record: it is judged by everyone's.
func teamKeeps(own *owners, team *ledger.Team) func(ledger.Incident) bool {
	if team == nil || len(team.Labels) == 0 {
		return nil
	}
	return func(in ledger.Incident) bool { return own.ownsIncident(*team, in) }
}

// suggestionsNow are the last digest's suggestions and the week they were
// worked out for: none once that week's record isn't kept, for everyone or,
// with team, by what it owns: the firings they were worked out from are no
// longer judged, after an upgrade or an incident taken away.
func (s *Server) suggestionsNow(ctx context.Context, team *ledger.Team) ([]verdict.Suggestion, time.Time, error) {
	rs, err := s.l.Results(ctx, "suggestions", 1)
	if err != nil || len(rs) == 0 {
		return nil, time.Time{}, err
	}
	kept, err := s.spanKept(ctx, rs[0].From, rs[0].To, team)
	if err != nil || !kept {
		return nil, time.Time{}, err
	}
	var ss []verdict.Suggestion
	if !s.decodeKept(rs[0], &ss) {
		return nil, time.Time{}, nil
	}
	return ss, rs[0].From, nil
}

// suggestionsIn are suggestionsNow, of t's weeks as they are kept: everyone's,
// or the team's they are kept to.
func (s *Server) suggestionsIn(ctx context.Context, t *tallies) ([]verdict.Suggestion, error) {
	rs, err := s.l.Results(ctx, "suggestions", 1)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	kept, err := s.keptWeek(ctx, t, rs[0].From, rs[0].To)
	if err != nil || !kept {
		return nil, err
	}
	var ss []verdict.Suggestion
	if !s.decodeKept(rs[0], &ss) {
		return nil, nil
	}
	return ss, nil
}

// recordAround are the incidents that keep the record of [from, to)'s
// weeks and didn't start in it: what verdict.Judge is given beside the
// period's own, so that, judging by the same weeks (o.RecordWeek), it says
// what the ledger's report says. It judges no firing against them.
func (s *Server) recordAround(ctx context.Context, from, to time.Time, o verdict.Options) ([]incident.Incident, error) {
	wf, _ := s.l.WeekOf(from)
	_, wt := s.l.WeekOf(to.Add(-time.Nanosecond))
	if !wt.After(wf) {
		_, wt = s.l.WeekOf(from)
	}
	around, err := s.l.RecordIncidents(ctx, verdict.RecordFrom(wf, wt), wt, o.UnknownLen)
	out := make([]incident.Incident, 0, len(around))
	for _, in := range around {
		if in.Start.Before(from) || !in.Start.Before(to) {
			out = append(out, in.Incident)
		}
	}
	return out, err
}

// noisyAt is when a rule counts as noisy, by o.
func noisyAt(o verdict.Options) verdict.NoisyAt {
	n := verdict.NoisyAt{Firings: o.NoisyFirings, Precision: o.NoisyPrecision}
	if n.Firings == 0 {
		n.Firings = verdict.NoisyFirings
	}
	if n.Precision == 0 {
		n.Precision = verdict.NoisyPrecision
	}
	return n
}

// decodeKept decodes a result the digest kept; one that doesn't decode is
// logged, and taken as none, until the next digest keeps another.
func (s *Server) decodeKept(r ledger.Result, v any) bool {
	if err := r.Decode(v); err != nil {
		s.log.Warn("a result the digest kept doesn't decode: taken as none", slog.String("kind", r.Kind), slog.Any("err", err))
		return false
	}
	return true
}
