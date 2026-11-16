package serve

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// What the summary, reports and pages read of marked windows over weeks:
// the windows with an occurrence meeting them, and their kinds' weekly
// tallies, read once (docs/design/marked.md, "How they are counted").

// markedOcc is an occurrence of a marked window.
type markedOcc struct {
	ledger.Occurrence
	M *ledger.Marked
}

// Key is what the occurrence is known by: its window's id and its start.
func (o markedOcc) Key() string { return ledger.OccurrenceKey(o.M.ID, o.Start) }

// markedSpan is the marked windows meeting weeks, and their tallies by
// week, rule and kind.
type markedSpan struct {
	s     *Server
	own   *owners
	ms    []ledger.Marked
	tally map[int64]map[int64]map[string]ledger.MarkedTally
}

// markedWeeks reads the marked windows meeting the weeks from Monday w1 to
// Monday w2, and their tallies.
func (s *Server) markedWeeks(ctx context.Context, w1, w2 int64, own *owners) (*markedSpan, error) {
	x := &markedSpan{s: s, own: own, tally: map[int64]map[int64]map[string]ledger.MarkedTally{}}
	ms, err := s.l.MarkedMeeting(ctx, s.l.Midnight(w1), s.l.Midnight(w2+7).Add(-time.Second))
	if err != nil || len(ms) == 0 {
		return x, err
	}
	x.ms = ms
	ts, err := s.l.MarkedWeeks(ctx, w1, w2)
	if err != nil {
		return nil, err
	}
	for _, m := range ts {
		if x.tally[m.At] == nil {
			x.tally[m.At] = map[int64]map[string]ledger.MarkedTally{}
		}
		if x.tally[m.At][m.Rule] == nil {
			x.tally[m.At][m.Rule] = map[string]ledger.MarkedTally{}
		}
		x.tally[m.At][m.Rule][m.Kind] = m
	}
	return x, nil
}

// applies reports whether a window is everyone's or team's: for everyone
// (nil), any window but a removed team's.
func applies(m ledger.Marked, team *ledger.Team) bool {
	return m.TeamID == 0 || m.TeamID > 0 && (team == nil || m.TeamID == team.ID)
}

// occurrences are, by kind, the occurrences of the windows that apply to
// team meeting [from, to), the earliest first.
func (x *markedSpan) occurrences(from, to time.Time, team *ledger.Team) map[string][]markedOcc {
	out := map[string][]markedOcc{}
	for i := range x.ms {
		m := &x.ms[i]
		if !applies(*m, team) {
			continue
		}
		os, _ := m.Occurrences(from, to.Add(-time.Second), ledger.MarkedKindWeek*53)
		for _, o := range os {
			out[m.Kind] = append(out[m.Kind], markedOcc{o, m})
		}
	}
	for k := range out {
		slices.SortStableFunc(out[k], func(a, b markedOcc) int { return a.Start.Compare(b.Start) })
	}
	return out
}

// during reports whether an incident started during one of the
// occurrences and agrees with its window, as a firing does: a team's
// window, of an incident the team owns.
func (x *markedSpan) during(in ledger.Incident, os []markedOcc) bool {
	for _, o := range os {
		if in.Start.Before(o.Start) || in.Start.After(o.End) || !o.M.Agrees(in.Labels) {
			continue
		}
		if o.M.TeamID > 0 && !x.own.ownsIncident(x.own.byID[o.M.TeamID], in) {
			continue
		}
		return true
	}
	return false
}

// row is a summary row's marked: each kind with an occurrence in the week
// w, for team (nil for everyone), whose rules mine says. row is the row as
// worked out, for precision outside each kind; incs, the week's incidents;
// judged, a share caught or nil with no record.
func (x *markedSpan) row(w int64, team *ledger.Team, mine func(string) bool, row summaryRow, t *tallies, incs []ledger.Incident,
	judged func(n, of int) *float64) map[string]summaryMarked {
	out := map[string]summaryMarked{}
	if len(x.ms) == 0 {
		return out
	}
	occs := x.occurrences(x.s.l.Midnight(w), x.s.l.Midnight(w+7), team)
	for kind, os := range occs {
		c := summaryMarked{Windows: len(os)}
		for id, byKind := range x.tally[w] {
			if !mine(t.rules[id].Name) {
				continue
			}
			k := byKind[kind]
			c.Firings += k.Firings
			c.Caught += k.White
			c.Refired += k.Refired
		}
		c.Precision = judged(c.Caught, c.Firings)
		c.PrecisionOutside = judged(row.Caught-c.Caught, row.Firings-c.Firings)
		var leads, impactLeads []time.Duration
		for _, in := range incs {
			if team != nil && !x.own.ownsIncident(*team, in) || !x.during(in, os) {
				continue
			}
			sum, ls := &c.Incidents, &leads
			if in.Origin == "impact" {
				sum, ls = &c.Impacts, &impactLeads
			}
			sum.Total++
			switch t.state(in, w) {
			case "caught":
				sum.Caught++
				for _, cb := range in.CaughtBy {
					if !t.noisy(w, t.id(cb.Rule)) {
						*ls = append(*ls, -cb.Delay)
						break
					}
				}
			case "noisy":
				sum.NoisyOnly++
			default:
				sum.Missed++
			}
		}
		c.Incidents.MedianLead, c.Impacts.MedianLead = medianSeconds(leads), medianSeconds(impactLeads)
		out[kind] = c
	}
	return out
}

// markedAsked are the kind of marked window a rules page is asked for,
// inside it (during=) or outside it (outside=): one at most, the first.
func markedAsked(q interface{ Get(string) string }) (during, outside string) {
	if v := q.Get("during"); v != "" && markedKind(v, true) {
		return v, ""
	}
	if v := q.Get("outside"); v != "" && markedKind(v, false) {
		return "", v
	}
	return "", ""
}

// markedOnly makes t's tallies, of the weeks it holds, those of the
// firings during the kind in, or outside the kind out: by rule and week,
// from marked_weeks, whose rows are of firings judged, so excused nothing.
// during deploy keeps the rules with firings during deploys, as they are:
// their column as a filter.
func (s *Server) markedOnly(ctx context.Context, t *tallies, in, out string) error {
	if in == "" && out == "" {
		return nil
	}
	if in == "deploy" {
		for _, w := range t.weeks {
			for id, x := range t.by[w] {
				if x.Deploy == 0 {
					delete(t.by[w], id)
				}
			}
		}
		return nil
	}
	ms, err := s.l.MarkedWeeks(ctx, t.weeks[0], t.weeks[len(t.weeks)-1])
	if err != nil {
		return err
	}
	kind := in + out
	marked := map[int64]map[int64]ledger.MarkedTally{}
	for _, m := range ms {
		if m.Kind != kind {
			continue
		}
		if marked[m.At] == nil {
			marked[m.At] = map[int64]ledger.MarkedTally{}
		}
		marked[m.At][m.Rule] = m
	}
	for _, w := range t.weeks {
		for id, x := range t.by[w] {
			m := marked[w][id]
			if in != "" {
				if m.Firings == 0 {
					delete(t.by[w], id)
					continue
				}
				t.by[w][id] = ledger.Tally{At: w, Rule: id, Firings: m.Firings, White: m.White, Refired: m.Refired}
				continue
			}
			x.Firings, x.White, x.Refired = x.Firings-m.Firings, x.White-m.White, x.Refired-m.Refired
			if x.Firings+x.Excused == 0 {
				delete(t.by[w], id)
				continue
			}
			t.by[w][id] = x
		}
	}
	return nil
}

// markedReport is what a report's marked section, and the digest, say of
// marked windows over a span: each kind's firings inside it beside outside,
// and each occurrence on in the span, inside its kind and the four weeks
// before outside it.
type markedReport struct {
	Kinds       map[string]markedKindCount `json:"kinds"`
	Occurrences []markedOccCount           `json:"occurrences"`
	lines       []string
}

type markedKindCount struct {
	Firings          int      `json:"firings"`
	Caught           int      `json:"caught"`
	Precision        *float64 `json:"precision"`
	PrecisionOutside *float64 `json:"precision_outside"`
}

type markedOccCount struct {
	Key    string           `json:"key"`
	Kind   string           `json:"kind"`
	Title  string           `json:"title"`
	Start  time.Time        `json:"start"`
	End    time.Time        `json:"end"`
	Inside markedCountShown `json:"inside"`
	Before markedCountShown `json:"before"`
}

// markedCountShown is a markedCount as numbers: precision of those judged,
// null with none.
type markedCountShown struct {
	Firings   int              `json:"firings"`
	Caught    int              `json:"caught"`
	Refired   int              `json:"refired"`
	Noisy     int              `json:"noisy_rules"`
	Precision *float64         `json:"precision"`
	Incidents summaryIncidents `json:"incidents"`
	Impacts   summaryIncidents `json:"impacts"`
}

func shownCount(c markedCount) markedCountShown {
	in := func(x markedIncidents) summaryIncidents {
		return summaryIncidents{Total: x.Total, Caught: x.Caught, NoisyOnly: x.NoisyOnly, Missed: x.Missed}
	}
	return markedCountShown{Firings: c.Firings, Caught: c.Caught, Refired: c.Refired, Noisy: c.Noisy, Precision: share(c.Caught, c.judged),
		Incidents: in(c.Incidents), Impacts: in(c.Impacts)}
}

// markedOfReport works out what is said of marked windows over [from, to)
// for team (nil for everyone): from rep's rules, by kind, and the
// occurrences on in the span. nil when none was.
func (s *Server) markedOfReport(ctx context.Context, rep *verdict.Report, from, to time.Time, team *ledger.Team, loc *time.Location) (*markedReport, error) {
	ms, err := s.l.MarkedMeeting(ctx, from, to.Add(-time.Second))
	if err != nil || len(ms) == 0 {
		return nil, err
	}
	l := s.cfg.Lang
	lines, _, err := s.markedLines(ctx, ms, from, to.Add(-time.Second), team, l)
	if err != nil || len(lines) == 0 {
		return nil, err
	}
	x := &markedReport{Kinds: map[string]markedKindCount{}}
	white, judged := 0, 0
	type kc struct{ firings, caught, judged int }
	by := map[string]kc{}
	for _, r := range rep.Rules {
		white += r.White
		judged += r.Firings - r.Unjudged
		for k, c := range r.Marked {
			x := by[k]
			by[k] = kc{x.firings + c.Firings, x.caught + c.Caught, x.judged + c.Firings - c.Unjudged}
		}
	}
	at := func(t time.Time) string { return t.In(loc).Format("2006-01-02 15:04") }
	for _, o := range lines {
		if _, ok := by[o.Kind]; !ok {
			by[o.Kind] = kc{}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(by)) {
		c := by[k]
		x.Kinds[k] = markedKindCount{Firings: c.firings, Caught: c.caught, Precision: share(c.caught, c.judged), PrecisionOutside: share(white-c.caught, judged-c.judged)}
		x.lines = append(x.lines, tr(l, "During %s: %d firings, %d of them catching something; precision %s inside, %s outside.",
			verdict.Clean(k), c.firings, c.caught, pct(c.caught, c.judged), pct(white-c.caught, judged-c.judged)))
	}
	slices.Reverse(lines) // the earliest first, as a report reads
	for _, o := range lines {
		if o.Ahead {
			continue
		}
		x.Occurrences = append(x.Occurrences, markedOccCount{Key: o.Key(), Kind: o.Kind, Title: o.Title, Start: o.Start.UTC(), End: o.End.UTC(),
			Inside: shownCount(o.Inside), Before: shownCount(o.Before)})
		x.lines = append(x.lines, tr(l, "%s %s, %s → %s: %d firings, precision %s, the four weeks before %s; incidents %d, %d of them missed.",
			verdict.Clean(o.Kind), verdict.Clean(o.Title), at(o.Start), at(o.End), o.Inside.Firings, o.Inside.Precision, o.Before.Precision,
			o.Inside.Incidents.Total+o.Inside.Impacts.Total, o.Inside.Incidents.Missed+o.Inside.Impacts.Missed))
	}
	if len(x.Occurrences) == 0 {
		return nil, nil
	}
	return x, nil
}

// text is what a report or the digest says of marked windows: "" for none.
func (x *markedReport) text() string {
	if x == nil {
		return ""
	}
	return strings.Join(x.lines, "\n")
}

// markedTile is the overview's marked window: the latest occurrence that
// is on, or ended, in the weeks t shows, for team (nil for everyone); nil
// for none.
func (s *Server) markedTile(ctx context.Context, t *tallies, team *ledger.Team, l string) (*occLine, error) {
	from, to := s.l.Midnight(t.weeks[0]), s.now()
	ms, err := s.l.MarkedMeeting(ctx, from, to)
	if err != nil || len(ms) == 0 {
		return nil, err
	}
	lines, _, err := s.markedLines(ctx, ms, from, to, team, l)
	for _, o := range lines {
		if !o.Ahead {
			return &o, err
		}
	}
	return nil, err
}

// ruleKind is a rule's firings during a kind of marked window over the
// weeks shown: those that caught something, and precision inside the kind
// and outside it.
type ruleKind struct {
	Kind                     string
	Firings, Caught, Refired int
	Inside, Outside          string
}

// ruleKinds are the kinds of marked window rule id fired in over the last
// n weeks of t, each inside beside outside, l the rule's line over them.
func (s *Server) ruleKinds(ctx context.Context, t *tallies, id int64, n int, l ruleLine) ([]ruleKind, error) {
	if id == 0 {
		return nil, nil
	}
	recent := t.weeks[len(t.weeks)-n:]
	ms, err := s.l.MarkedWeeks(ctx, recent[0], recent[len(recent)-1])
	if err != nil {
		return nil, err
	}
	by := map[string]*ruleKind{}
	judged := map[string]int{}
	for _, m := range ms {
		if m.Rule != id {
			continue
		}
		k := by[m.Kind]
		if k == nil {
			k = &ruleKind{Kind: m.Kind}
			by[m.Kind] = k
		}
		k.Firings += m.Firings
		k.Caught += m.White
		k.Refired += m.Refired
		if t.kept[m.At] {
			judged[m.Kind] += m.Firings
		} else {
			judged[m.Kind] += m.White
		}
	}
	all := l.Firings - l.Unjudged
	var out []ruleKind
	for _, kind := range slices.Sorted(maps.Keys(by)) {
		k := by[kind]
		k.Inside, k.Outside = pct(k.Caught, judged[kind]), pct(l.White-k.Caught, all-judged[kind])
		out = append(out, *k)
	}
	return out, nil
}

// incidentKinds gives each incident the kinds of marked window it started
// during, read once for all of them.
func (s *Server) incidentKinds(ctx context.Context, ls []incidentLine) error {
	if len(ls) == 0 {
		return nil
	}
	from, to := ls[0].Start, ls[0].Start
	for _, l := range ls {
		from, to = minTime(from, l.Start), maxTime(to, l.Start)
	}
	ms, err := s.l.MarkedMeeting(ctx, from, to)
	if err != nil || len(ms) == 0 {
		return err
	}
	own, err := s.owners(ctx)
	if err != nil {
		return err
	}
	x := &markedSpan{s: s, own: own, ms: ms}
	occs := x.occurrences(from, to.Add(time.Second), nil)
	for i := range ls {
		for _, k := range slices.Sorted(maps.Keys(occs)) {
			if x.during(ls[i].Incident, occs[k]) {
				ls[i].Kinds = append(ls[i].Kinds, k)
			}
		}
	}
	return nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
