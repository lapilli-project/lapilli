package serve

import (
	"cmp"
	"context"
	"sort"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// schedules say by whose hours each rule's firings are counted asleep or off
// hours: the hours of the team that owns it, when the team has its own, or
// else the server's.
type schedules struct {
	server *verdict.Hours
	team   map[int64]*verdict.Hours // the teams with hours of their own
	o      *owners
}

// schedules are worked out once for the owners, which stand until the
// ledger changes, and the server's hours.
func (o *owners) schedules(server *verdict.Hours) schedules {
	o.schedMu.Lock()
	defer o.schedMu.Unlock()
	if o.sched != nil && o.schedFor == server {
		return *o.sched
	}
	sc := schedules{server: server, team: map[int64]*verdict.Hours{}, o: o}
	for _, t := range o.teams {
		// What doesn't parse was refused when it was kept: the server's. A
		// team's hours the same as the server's are the server's.
		if h, err := t.Hours(server); err == nil && h != nil && (server == nil || ledger.HoursKey(h) != ledger.HoursKey(server)) {
			sc.team[t.ID] = h
		}
	}
	o.sched, o.schedFor = &sc, server
	return sc
}

// of is the hours a rule's firings are counted by; nil for none.
func (sc schedules) of(rule string) *verdict.Hours {
	if h := sc.team[sc.o.rule[rule]]; h != nil {
		return h
	}
	return sc.server
}

// forTeam is the hours a team's rules are counted by; nil for none.
func (sc schedules) forTeam(t *ledger.Team) *verdict.Hours {
	if h := sc.team[t.ID]; h != nil {
		return h
	}
	return sc.server
}

// none reports whether no firing is counted by any hours.
func (sc schedules) none() bool { return sc.server == nil && len(sc.team) == 0 }

// used are the hours the rules named were counted by, for a reader: the
// server's, and the teams' own that any of them went by. For one team's
// rules, they are that team's.
func (sc schedules) used(rules []verdict.RuleResult, team *ledger.Team) *verdict.HoursUsed {
	if team != nil {
		return sc.forTeam(team).Used()
	}
	var u *verdict.HoursUsed
	if sc.server != nil {
		u = sc.server.Used()
	}
	ids := map[int64]bool{}
	for _, rr := range rules {
		if id := sc.o.rule[rr.Rule]; sc.team[id] != nil {
			ids[id] = true
		}
	}
	for id := range ids {
		if u == nil {
			u = &verdict.HoursUsed{}
		}
		h := sc.team[id].Used()
		u.Teams = append(u.Teams, verdict.TeamHours{Team: sc.o.byID[id].Name, Zone: h.Zone, Work: h.Work, Sleep: h.Sleep, Shifts: h.Shifts})
	}
	if u != nil {
		sort.Slice(u.Teams, func(a, b int) bool { return u.Teams[a].Team < u.Teams[b].Team })
	}
	return u
}

// ledgerReport is the ledger's report, each rule's firings counted asleep or
// off hours by its team's hours, for 31 days at most.
func (s *Server) ledgerReport(ctx context.Context, from, to time.Time, o verdict.Options, firings bool) (*verdict.Report, error) {
	return s.ledgerReportFor(ctx, from, to, o, firings, nil)
}

// ledgerReportFor is ledgerReport, its firings judged by the record of a
// team's when team isn't nil: by the incidents it owns.
func (s *Server) ledgerReportFor(ctx context.Context, from, to time.Time, o verdict.Options, firings bool, team *ledger.Team) (*verdict.Report, error) {
	own, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	sc := own.schedules(o.Hours)
	o.Hours = nil // counted below
	if o, err = s.withHeld(ctx, o, from, to); err != nil {
		return nil, err
	}
	rep, err := s.l.ReportFor(ctx, from, to, o, firings, teamKeeps(own, team))
	if err != nil {
		return nil, err
	}
	s.lateCut(rep.LateCut, from, to)
	// What judged it, for a reader counting it again (docs/numbers.md): by
	// the ledger's days, or weeks.
	rep.Settings = o.Settings()
	if d := rep.Settings.Depends; d != nil {
		// How big the graph it was judged by was, of the ledger's edges.
		if d.Edges, err = s.l.EdgesIn(ctx, from, to); err != nil {
			return nil, err
		}
	}
	if rep.Settings.RecordBy == "" {
		// Options with no unit are judged by the ledger's weeks.
		rep.Settings.ByWeek, rep.Settings.RecordBy = true, "week"
	}
	// With responses kept, what rules are found noisy by is said, as it is
	// when they are counted (docs/design/responses.md).
	if rep.Responses != nil && rep.Settings.NoisyBy == "" {
		rep.Settings.NoisyBy, rep.Settings.Routine = verdict.NoisyByCaught, cmp.Or(o.Routine, 0.5)
	}
	if sc.none() || to.Sub(from) > ledger.HoursLongest {
		return rep, nil
	}
	if err := s.l.CountHours(ctx, rep.Rules, from, to, sc.of); err != nil {
		return nil, err
	}
	rep.Hours = sc.used(rep.Rules, nil)
	return rep, nil
}

// withHeld is o with how long before its start each rule's condition held
// (verdict.Options.Held), the least of the versions in effect from the
// span's start to its end and as long after it as a rule's held at most
// (verdict.HeldFor): what the ledger says a firing came after an incident
// was over by (ledger.LateOf), as lapilli verdict says it of the rules it
// replays, every version having held. A firing after an incident of the
// span comes that long after its end at most, so every version it can be
// of has its say. Kept when o has it already.
func (s *Server) withHeld(ctx context.Context, o verdict.Options, from, to time.Time) (verdict.Options, error) {
	if o.Held != nil {
		return o, nil
	}
	held := func(to time.Time) (map[string]time.Duration, int, error) {
		vs, err := s.versions(ctx, from, to)
		if err != nil {
			return nil, 0, err
		}
		var rs []rules.Rule
		for _, v := range vs {
			rs = append(rs, v.Rules...)
		}
		return verdict.HeldFor(rs), len(vs), nil
	}
	h, n, err := held(to)
	if err != nil {
		return o, err
	}
	var most time.Duration
	for _, d := range h {
		most = max(most, d)
	}
	if most > 0 {
		// The versions as long after: what more they hold only lowers what
		// held, so the longest before is long enough.
		more, m, err := held(to.Add(most))
		if err != nil {
			return o, err
		}
		if m > n {
			h = more
		}
	}
	o.Held = h
	return o, nil
}

// lateCut logs, when cut, that more firings came near the end of incidents
// in [from, to) than one read looks at for those that came after one.
func (s *Server) lateCut(cut bool, from, to time.Time) {
	if cut {
		s.log.Warn("more firings came near the end of incidents than one read looks at: those past it aren't said to have come after one", "from", from, "to", to)
	}
}

// asleepNote says what the tile of firings while people slept counts, for
// the rules of t, or one team's: "" when nobody's hours have people asleep.
func (sc schedules) asleepNote(l string, t *tallies, team *ledger.Team, home *time.Location) string {
	return sc.asleepSaid(l, t, team, home, true)
}

// asleepHours is when people sleep, as that tile counts it, and not what
// it counts: the span, in its zone when not the server's; by each team's,
// or each shift's; "" when nobody's hours have people asleep. A tile that
// doesn't know whether its firings caught anything says this alone, not
// that they caught nothing.
func (sc schedules) asleepHours(l string, t *tallies, team *ledger.Team, home *time.Location) string {
	return sc.asleepSaid(l, t, team, home, false)
}

func (sc schedules) asleepSaid(l string, t *tallies, team *ledger.Team, home *time.Location, note bool) string {
	var in []*verdict.Hours
	if team != nil {
		in = append(in, sc.forTeam(team))
	} else {
		// Of the rules that fired in the weeks shown.
		fired := map[int64]bool{}
		for _, by := range t.by {
			for id := range by {
				fired[id] = true
			}
		}
		seen := map[string]bool{}
		for id, r := range t.rules {
			if !fired[id] {
				continue
			}
			if h := sc.of(r.Name); h != nil && !seen[ledger.HoursKey(h)] {
				seen[ledger.HoursKey(h)] = true
				in = append(in, h)
			}
		}
	}
	var asleep []*verdict.Hours
	for _, h := range in {
		if h != nil && h.Sleeps() {
			asleep = append(asleep, h)
		}
	}
	switch {
	case len(asleep) == 0:
		return ""
	case (len(asleep) > 1 || len(in) > 1) && note:
		return tr(l, "firings that caught nothing, by each team's hours asleep")
	case len(asleep) > 1 || len(in) > 1:
		return tr(l, "by each team's hours asleep")
	case asleep[0].Shifts != nil && note:
		return tr(l, "firings that caught nothing, by the hours asleep of each shift")
	case asleep[0].Shifts != nil:
		return tr(l, "by the hours asleep of each shift")
	}
	h := asleep[0]
	span := strings.ReplaceAll(h.SleepText(), "-", "–")
	switch {
	case h.Loc.String() != home.String() && note:
		return tr(l, "firings that caught nothing, %s in %s", span, h.Loc.String())
	case h.Loc.String() != home.String():
		return tr(l, "%s in %s", span, h.Loc.String())
	case note:
		return tr(l, "firings that caught nothing, %s", span)
	}
	return span
}

// countsHours reports whether a report counts firings by any hours: the
// server's, or a team's own.
func (s *Server) countsHours(ctx context.Context) bool {
	if s.cfg.Options.Hours != nil {
		return true
	}
	own, err := s.owners(ctx)
	return err != nil || !own.schedules(nil).none() // when unsure, take the turn
}

// hoursLine is hours as a person reads them: "" for none.
func hoursLine(l string, h *verdict.Hours) string {
	if h == nil {
		return ""
	}
	if h.Shifts != nil && h.Shifts.OnCall != nil {
		return tr(l, "on call by the schedule %s: each turn by its person's hours; between turns, and for someone with none, at work %s, asleep %s, in %s",
			h.Shifts.OnCall.Name, verdict.HoursText(h.WorkText()), h.SleepText(), h.Loc.String())
	}
	if h.Shifts != nil {
		return tr(l, "in shifts: %s; where a turn doesn't say, at work %s, asleep %s, in %s", h.Shifts.String(),
			verdict.HoursText(h.WorkText()), h.SleepText(), h.Loc.String())
	}
	return tr(l, "at work %s, asleep %s, in %s", verdict.HoursText(h.WorkText()), h.SleepText(), h.Loc.String())
}
