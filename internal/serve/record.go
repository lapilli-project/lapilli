package serve

import (
	"context"
	"log/slog"
	"maps"
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
// every one when nil: each week, and, of each rule's firings in it, those
// judged (jby). With named signals (docs/design/signals.md), a week's
// firings are judged by the signals tied to them, so part of a rule's
// week may be judged and part not: kept says only that an incident
// written down keeps the week's record.
func (t *tallies) keep(keeps func(ledger.Incident) bool) {
	var plain, lapilli []incident.Incident
	for _, in := range t.recs {
		if keeps == nil || keeps(in) {
			plain = append(plain, in.Incident)
			if !in.Real {
				lapilli = append(lapilli, in.Incident)
			}
		}
	}
	t.rec, t.keeps = verdict.RecordOf(plain, t.o), keeps
	t.recL = t.rec
	if len(lapilli) < len(plain) {
		t.recL = verdict.RecordOf(lapilli, t.o)
	}
	t.kept = make(map[int64]bool, len(t.weeks))
	for _, w := range t.weeks {
		t.kept[w] = t.rec.Kept(t.mid(w), t.mid(w+7))
	}
	t.jby = make(map[int64]map[int64]ledger.Tally, len(t.weeks))
	t.ub = make(map[int64]map[int64]ledger.Tally, len(t.weeks))
	for _, w := range t.weeks {
		t.jby[w] = map[int64]ledger.Tally{}
		t.ub[w] = map[int64]ledger.Tally{}
	}
	if t.parts == nil {
		for w, rows := range t.by {
			for id, x := range rows {
				t.countUseful(w, id, x, t.kept[w] && !t.o.NotJudged(t.rules[id].Name))
			}
		}
		// Read whole, as before tallies were kept by signals: a week's
		// all judged, or none; a rule an entry judges by nothing, none.
		for w, rows := range t.by {
			if t.kept[w] {
				t.jby[w] = maps.Clone(rows)
				maps.DeleteFunc(t.jby[w], func(id int64, _ ledger.Tally) bool { return t.o.NotJudged(t.rules[id].Name) })
			}
		}
		return
	}
	for _, x := range t.parts {
		m := t.jby[x.At]
		if m == nil {
			continue
		}
		t.countUseful(x.At, x.Rule, x, t.judgedPart(x.At, x.Rule, x.Signals))
		judged := x
		if !t.judgedPart(x.At, x.Rule, x.Signals) {
			if !t.o.Signals.Named() {
				continue // a week with no record: none of its firings, as before
			}
			// A firing that caught something is judged whatever its
			// signals, as the report has it (verdict.RuleResult.Judged):
			// of a part with no record, its white stones alone.
			judged = ledger.Tally{Firings: x.White, White: x.White, Deploy: x.DeployWhite, DeployWhite: x.DeployWhite,
				Episodes: x.EpisodesWhite, EpisodesWhite: x.EpisodesWhite}
		}
		j := m[x.Rule].Add(judged)
		j.At, j.Rule, j.Signals = x.At, x.Rule, 0
		m[x.Rule] = j
	}
}

// countUseful adds to the week's useful and not useful firings of a rule
// those of x, a tally of its firings that week (a part, or the whole): of
// unknown use, black, and judged by the record, not useful.
func (t *tallies) countUseful(w, id int64, x ledger.Tally, judged bool) {
	if t.ub[w] == nil {
		return
	}
	u := t.ub[w][id]
	u.Useful += x.Useful
	u.NotUseful += x.NotUseful
	if judged {
		u.NotUseful += x.Firings - x.Useful - x.NotUseful
	}
	t.ub[w][id] = u
}

// usefulOf are the firings of rule id in week w that were useful and not
// useful.
func (t *tallies) usefulOf(w, id int64) (useful, not int) {
	u := t.ub[w][id]
	return u.Useful, u.NotUseful
}

// judgedPart reports whether the firings of rule id in week w, their
// alerts tied to signals (ledger.Tally.Signals), are judged: by the week
// with no named signal; else by the signals tied to them, the rule's
// entry's or else its alerts' (verdict.Record.JudgesTied).
func (t *tallies) judgedPart(w, id int64, signals int64) bool {
	return t.judgedBy(t.rec, w, id, signals)
}

// judgedBy is judgedPart by the record rec: a rule an entry judges by
// nothing (judged: false) is never judged.
func (t *tallies) judgedBy(rec verdict.Record, w, id int64, signals int64) bool {
	if t.o.NotJudged(t.rules[id].Name) {
		return false
	}
	if !t.o.Signals.Named() {
		return rec.Kept(t.mid(w), t.mid(w+7))
	}
	return rec.JudgesTied(t.mid(w), t.o.Signals.TiedOf(t.rules[id].Name, signals))
}

// byLapilli is, of the firings of rule id in week w tied to signals, a
// part as tallied, those Lapilli's own judging would count, catch and
// judge (docs/design/judging.md, "How reports count them"): every
// "unrelated" white, those excused for it counted again; the incidents
// "This was real" wrote left out, their firings black, or excused where a
// quiet window matches them, or not judged where only such an incident
// kept the record.
func (t *tallies) byLapilli(x ledger.Tally) (firings, white, judged int) {
	firings = x.Firings + x.UnrelatedExcused - x.RealExcused
	white = x.White + x.Unrelated - x.RealWhite
	switch {
	case t.judgedBy(t.recL, x.At, x.Rule, x.Signals):
		judged = firings
	case t.o.Signals.Named() && !t.o.NotJudged(t.rules[x.Rule].Name):
		judged = white // what caught something is judged, whatever its signals
	}
	return firings, white, judged
}

// judged are, of rule id's firings in week w, those judged; unjudged, the
// rest (counted, not judged), as t has them after only kept one team's.
func (t *tallies) judged(w, id int64) ledger.Tally { return t.jby[w][id] }

func (t *tallies) unjudged(w, id int64) ledger.Tally { return minus(t.by[w][id], t.jby[w][id]) }

// unjudgedAll is unjudged, as everyone's rules were tallied, before only
// kept one team's.
func (t *tallies) unjudgedAll(w, id int64) ledger.Tally {
	if t.everyone == nil {
		return t.unjudged(w, id)
	}
	return minus(t.everyone[w][id], t.jeveryone[w][id])
}

// firingJudge says, of a firing of rule that started at start, its alert
// tied to signals, whether it is judged should it catch nothing, or only
// counted, as the report says it (ledger.Ledger.FiringJudged). A page that
// lists or draws firings one by one, or by day or hour, asks it, so that
// none of them calls black what the weeks and the summary count unjudged.
type firingJudge func(rule string, start time.Time, signals int64) bool

// judgeOver is what judges the firings from from to to, by the incidents
// keeps keeps (every one when nil), with o.
func (s *Server) judgeOver(ctx context.Context, from, to time.Time, o verdict.Options, keeps func(ledger.Incident) bool) (firingJudge, error) {
	rec, err := s.l.RecordOf(ctx, from, to, o, keeps)
	if err != nil {
		return nil, err
	}
	return func(rule string, start time.Time, signals int64) bool {
		return s.l.FiringJudged(rec, o, rule, start, signals)
	}, nil
}

// spansOf is j as RuleSpansJudged asks it, of one rule.
func (j firingJudge) spansOf(rule string) func(time.Time, int64) bool {
	return func(start time.Time, signals int64) bool { return j(rule, start, signals) }
}

// minus is a's counts less b's.
func minus(a, b ledger.Tally) ledger.Tally {
	a.Firings -= b.Firings
	a.White -= b.White
	a.Refired -= b.Refired
	a.Excused -= b.Excused
	a.Deploy -= b.Deploy
	a.DeployWhite -= b.DeployWhite
	a.Episodes -= b.Episodes
	a.EpisodesWhite -= b.EpisodesWhite
	return a
}

// keptAny reports whether any firing of week w would be judged: an
// incident written down keeps its record, or, with named signals, a
// window of one does, or one that keeps the record had started by its
// start. With no named signal, kept.
func (t *tallies) keptAny(w int64) bool {
	if t.kept[w] || !t.o.Signals.Named() {
		return t.kept[w]
	}
	for i := range t.o.Signals.List {
		if t.rec.JudgesTied(t.mid(w), []int{i}) {
			return true
		}
	}
	return false
}

// someUnjudged reports whether a firing of week w, of the rules t holds,
// was counted and not judged.
func (t *tallies) someUnjudged(w int64) bool {
	for id := range t.by[w] {
		if t.unjudged(w, id).Firings > 0 {
			return true
		}
	}
	return false
}

// fillTallies reads into t each rule's stones in each of its weeks, from
// w1 to w2: by, summed over the signals the firings' alerts are tied to,
// and parts, apart by them, which judge them (keep).
func (s *Server) fillTallies(ctx context.Context, t *tallies, w1, w2 int64) error {
	rows, err := s.l.WeeksBy(ctx, w1, w2)
	if err != nil {
		return err
	}
	if t.epFrom, t.epSome, err = s.l.EpisodesFrom(ctx); err != nil {
		return err
	}
	if t.inputs, err = s.l.ResponseInputs(ctx); err != nil {
		return err
	}
	t.parts = rows
	for _, r := range rows {
		m := t.by[r.At]
		if m == nil {
			continue
		}
		x := m[r.Rule].Add(r)
		x.At, x.Rule, x.Signals = r.At, r.Rule, 0
		m[r.Rule] = x
	}
	return nil
}

// keptWeek reports whether the record of the week of from is kept, as the
// tallies have it when they hold that week, or else as the ledger does,
// for everyone or the team t's are kept to.
func (s *Server) keptWeek(ctx context.Context, t *tallies, from, to time.Time) (bool, error) {
	if _, ok := t.kept[ledger.Week(s.l.Day(from))]; ok {
		return t.keptAny(ledger.Week(s.l.Day(from))), nil
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
