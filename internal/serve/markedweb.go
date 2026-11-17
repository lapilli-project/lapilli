package serve

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// The marked windows' page, under Monitoring beside Incidents, and API
// (docs/design/marked.md, "Where it shows"): what was happening, read with
// the incidents, not a change to the verdict. Each occurrence, the latest
// first, with its firings and incidents inside its kind, beside the four
// weeks before it, outside its kind.

func (s *Server) markedRoutes(mux *http.ServeMux, page func(string, func(*http.Request) (any, error)) http.HandlerFunc) {
	mux.HandleFunc("GET /marked", page("marked", s.markedPage))
	mux.HandleFunc("GET /marked/{id}", page("markedone", s.markedOnePage))
	mux.HandleFunc("POST /marked", s.teamWrite(s.postMarked))
	mux.HandleFunc("POST /marked/{id}/delete", s.teamWrite(s.deleteMarked))
	mux.HandleFunc("GET /api/v1/marked", s.cached(s.apiMarked))
	mux.HandleFunc("GET /api/v1/marked/{id}", s.cached(s.apiMarkedOne))
	mux.HandleFunc("POST /api/v1/marked", s.teamWrite(s.apiPutMarked))
	mux.HandleFunc("PUT /api/v1/marked/{id}", s.teamWrite(s.apiPutMarked))
	mux.HandleFunc("DELETE /api/v1/marked/{id}", s.teamWrite(s.apiDelete(func(r *http.Request, id int64) error {
		o, err := s.options(r.Context())
		if err != nil {
			return err
		}
		defer s.poke() // to carry on marking what is left
		return s.l.DeleteMarked(r.Context(), id, s.user(r), o)
	})))
}

// markedShown are how many occurrences a page lists at most, the latest
// first, and the weeks back and ahead it lists them over.
const (
	markedShown  = 100
	markedBack   = 12 * 7 * 24 * time.Hour
	markedAhead  = 4 * 7 * 24 * time.Hour
	markedBefore = 4 // the whole weeks before an occurrence it is set beside
)

// markedCount is an occurrence's firings inside its kind, or the weeks
// before outside it: those that caught something, re-fired, and the rules
// noisy there; Precision of those judged, "–" with none; and the incidents
// and impact windows, caught, caught only by noisy rules, or missed.
type markedCount struct {
	Firings, Caught, Refired, Noisy int
	Precision                       string
	Incidents, Impacts              markedIncidents
	// judged are the firings judged, of Firings.
	judged int
}

type markedIncidents struct {
	Total, Caught, NoisyOnly, Missed int
	// Lead is the median lead, as a page says it: "" for none caught.
	Lead  string
	leads []time.Duration
}

// occLine is an occurrence as the pages list it.
type occLine struct {
	markedOcc
	Kind, Title, Team, Labels string
	// On says it is on now; Ahead, that it hasn't started.
	On, Ahead bool
	// Inside are its own, Before the four whole weeks before its first,
	// outside its kind; Shared says another occurrence of its kind is on
	// one of its days, counted with it.
	Inside, Before markedCount
	Shared         bool
	// Firings is Alert history of its first day, its kind's.
	Firings string
}

// markedRows, when set, is told how many rows of tallies and incidents
// markedLines read: for TestMarkedPagesScale, which holds them.
var markedRows func(n int)

// markedLines are the occurrences of ms meeting [from, to] for team (nil
// for everyone), the latest first, markedShown at most (cut says more),
// each with its firings and incidents inside its kind and the weeks
// before outside it: read from the days and weeks of the kinds and of
// everything, and the incidents, once for all of them.
func (s *Server) markedLines(ctx context.Context, ms []ledger.Marked, from, to time.Time, team *ledger.Team, lg string) ([]occLine, bool, error) {
	own, err := s.owners(ctx)
	if err != nil {
		return nil, false, err
	}
	x := &markedSpan{s: s, own: own, ms: ms}
	var occs []markedOcc
	for _, os := range x.occurrences(from, to, team) {
		occs = append(occs, os...)
	}
	slices.SortFunc(occs, func(a, b markedOcc) int { return b.Start.Compare(a.Start) })
	cut := len(occs) > markedShown
	occs = occs[:min(len(occs), markedShown)]
	if len(occs) == 0 {
		return nil, false, nil
	}
	now := s.now()
	d1, d2 := s.l.Day(occs[len(occs)-1].Start), s.l.Day(now)
	for _, o := range occs {
		d2 = max(d2, s.l.Day(o.End))
	}
	w1, w2 := ledger.Week(d1)-7*markedBefore, ledger.Week(d2)
	t, err := s.talliesOver(ctx, w1, w2)
	if err != nil {
		return nil, false, err
	}
	if team != nil {
		t.only(own, *team)
	}
	mine := func(id int64) bool { return team == nil || own.rule[t.rules[id].Name] == team.ID }
	days, err := s.l.MarkedDaysBy(ctx, d1, d2)
	if err != nil {
		return nil, false, err
	}
	byDay := map[int64][]ledger.MarkedTally{}
	for _, d := range days {
		byDay[d.At] = append(byDay[d.At], d)
	}
	weeks, err := s.l.MarkedWeeksBy(ctx, w1, w2)
	if err != nil {
		return nil, false, err
	}
	// Each rule's firings of a kind in a week, and, of them, those that
	// caught nothing and had no record by the signals tied to them.
	byWeek := map[int64]map[int64]map[string]ledger.MarkedTally{}
	unjudged := map[int64]map[int64]map[string]int{}
	for _, m := range weeks {
		if byWeek[m.At] == nil {
			byWeek[m.At], unjudged[m.At] = map[int64]map[string]ledger.MarkedTally{}, map[int64]map[string]int{}
		}
		if byWeek[m.At][m.Rule] == nil {
			byWeek[m.At][m.Rule], unjudged[m.At][m.Rule] = map[string]ledger.MarkedTally{}, map[string]int{}
		}
		k := byWeek[m.At][m.Rule][m.Kind]
		k.At, k.Rule, k.Kind = m.At, m.Rule, m.Kind
		k.Firings += m.Firings
		k.White += m.White
		k.Refired += m.Refired
		byWeek[m.At][m.Rule][m.Kind] = k
		if !t.judgedPart(m.At, m.Rule, m.Signals) {
			unjudged[m.At][m.Rule][m.Kind] += m.Firings - m.White
		}
	}
	incs, err := s.l.Incidents(ctx, s.l.Midnight(w1), s.l.Midnight(w2+7), false)
	if err != nil {
		return nil, false, err
	}
	if markedRows != nil {
		n := len(days) + len(weeks) + len(incs)
		for _, w := range t.weeks {
			n += len(t.by[w])
		}
		markedRows(n)
	}
	all := x.occurrences(s.l.Midnight(w1), s.l.Midnight(w2+7), team)
	at := noisyAt(t.o)
	var out []occLine
	for _, o := range occs {
		l := occLine{markedOcc: o, Kind: o.M.Kind, Title: o.M.Title, Labels: ledger.MarkedText(*o.M), On: !now.Before(o.Start) && !now.After(o.End),
			Ahead: now.Before(o.Start)}
		if o.M.TeamID > 0 {
			l.Team = own.byID[o.M.TeamID].Name
		}
		l.Firings = (firingsAsk{day: s.l.Day(o.Start), kind: o.M.Kind}).url(s.l)
		// Inside: the kind's firings on the days it covers, by rule.
		inRules := map[int64][3]int{}
		judged := 0
		for d := s.l.Day(o.Start); d <= s.l.Day(o.End); d++ {
			for _, m := range byDay[d] {
				if m.Kind != o.M.Kind || !mine(m.Rule) {
					continue
				}
				c := inRules[m.Rule]
				inRules[m.Rule] = [3]int{c[0] + m.Firings, c[1] + m.White, c[2] + m.Refired}
				if t.judgedPart(ledger.Week(d), m.Rule, m.Signals) {
					judged += m.Firings
				} else {
					judged += m.White
				}
			}
		}
		l.Inside = markedTotals(inRules, judged, at)
		// Another of its kind on one of its days is counted with it.
		for _, p := range all[o.M.Kind] {
			if p != o && s.l.Day(p.Start) <= s.l.Day(o.End) && s.l.Day(p.End) >= s.l.Day(o.Start) {
				l.Shared = true
			}
		}
		// Before: the four whole weeks before its first, outside its kind.
		first := ledger.Week(s.l.Day(o.Start))
		beRules := map[int64][3]int{}
		judged = 0
		for w := first - 7*markedBefore; w < first; w += 7 {
			for id, x := range t.by[w] {
				if !mine(id) {
					continue
				}
				m := byWeek[w][id][o.M.Kind]
				n, white := x.Firings-m.Firings, x.White-m.White
				c := beRules[id]
				beRules[id] = [3]int{c[0] + n, c[1] + white, c[2] + x.Refired - m.Refired}
				// Outside its kind: the rule's firings judged, less those
				// of the kind.
				u := t.unjudged(w, id)
				judged += n - (u.Firings - u.White - unjudged[w][id][o.M.Kind])
			}
		}
		l.Before = markedTotals(beRules, judged, at)
		// The incidents: those during it, and those of the weeks before
		// during none of its kind.
		bFrom, bTo := s.l.Midnight(first-7*markedBefore), s.l.Midnight(first)
		for _, in := range incs {
			if team != nil && !own.ownsIncident(*team, in) {
				continue
			}
			w := ledger.Week(s.l.Day(in.Start))
			switch {
			case x.during(in, []markedOcc{o}):
				countIncident(&l.Inside, in, t, w)
			case !in.Start.Before(bFrom) && in.Start.Before(bTo) && !x.during(in, all[o.M.Kind]):
				countIncident(&l.Before, in, t, w)
			}
		}
		finishLeads(lg, &l.Inside, &l.Before)
		out = append(out, l)
	}
	return out, cut, nil
}

// markedTotals sums each rule's firings, those that caught something and
// re-fired, of which judged were judged; the noisy are those of judged
// firings enough, and too few caught.
func markedTotals(rules map[int64][3]int, judged int, at verdict.NoisyAt) markedCount {
	var c markedCount
	for _, r := range rules {
		c.Firings += r[0]
		c.Caught += r[1]
		c.Refired += r[2]
		if verdict.IsNoisy(r[0], r[1], at) {
			c.Noisy++
		}
	}
	c.Precision, c.judged = pct(c.Caught, judged), judged
	return c
}

// countIncident counts an incident in c, by its state in its week.
func countIncident(c *markedCount, in ledger.Incident, t *tallies, w int64) {
	sum := &c.Incidents
	if in.Origin == "impact" {
		sum = &c.Impacts
	}
	sum.Total++
	switch t.state(in, w) {
	case "caught":
		sum.Caught++
		for _, cb := range in.CaughtBy {
			if !t.noisy(w, t.id(cb.Rule)) {
				sum.leads = append(sum.leads, -cb.Delay)
				break
			}
		}
	case "noisy":
		sum.NoisyOnly++
	default:
		sum.Missed++
	}
}

// finishLeads says each count's median lead.
func finishLeads(lg string, cs ...*markedCount) {
	for _, c := range cs {
		for _, sum := range []*markedIncidents{&c.Incidents, &c.Impacts} {
			if m := medianSeconds(sum.leads); m != nil {
				sum.Lead = leadText(lg, time.Duration(*m)*time.Second)
			}
			sum.leads = nil
		}
	}
}

// talliesOver are the tallies of the weeks from Monday w1 to Monday w2,
// with their record, as the summary reads them.
func (s *Server) talliesOver(ctx context.Context, w1, w2 int64) (*tallies, error) {
	o, err := s.options(ctx)
	if err != nil {
		return nil, err
	}
	t := &tallies{by: map[int64]map[int64]ledger.Tally{}, o: o}
	for w := w1; w <= w2; w += 7 {
		t.weeks = append(t.weeks, w)
		t.by[w] = map[int64]ledger.Tally{}
	}
	if err := s.fillTallies(ctx, t, w1, w2); err != nil {
		return nil, err
	}
	if t.rules, err = s.l.RuleTable(ctx); err != nil {
		return nil, err
	}
	return t, s.readRecord(ctx, t)
}

// markedWindowLine is a window as the page lists it.
type markedWindowLine struct {
	ledger.Marked
	Team   string
	Labels string
}

type markedView struct {
	// Kinds are the kinds, each with its occurrences, the latest first.
	Kinds []markedKindView
	Cut   bool
	// Windows are the windows kept with an occurrence in the weeks shown,
	// or ahead.
	Windows []markedWindowLine
	Team    string
	// Draft is the form to keep one.
	Draft markedDraft
	// Has says the ledger keeps any.
	Has bool
}

type markedKindView struct {
	Kind  string
	Lines []occLine
}

// markedDraft is a marked window's form.
type markedDraft struct {
	ID                                                int64
	Kind, Title, Start, End, Every, On, Until, Labels string
	Team                                              int64
	Pick                                              teamPick
	Kinds                                             []string
}

func (s *Server) markedPage(r *http.Request) (any, error) {
	ctx := r.Context()
	now := s.now()
	o, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	var team *ledger.Team
	if c := s.pageContext(r); c.Team != "" {
		if t, ok := o.team(c.Team); ok {
			team = &t
		}
	}
	ms, err := s.l.MarkedMeeting(ctx, now.Add(-markedBack), now.Add(markedAhead))
	if err != nil {
		return nil, err
	}
	lines, cut, err := s.markedLines(ctx, ms, now.Add(-markedBack), now.Add(markedAhead), team, lang(r))
	if err != nil {
		return nil, err
	}
	v := markedView{Cut: cut, Team: o.teamName(team)}
	by := map[string][]occLine{}
	for _, l := range lines {
		by[l.Kind] = append(by[l.Kind], l)
	}
	for _, k := range slices.Sorted(maps.Keys(by)) {
		v.Kinds = append(v.Kinds, markedKindView{k, by[k]})
	}
	for _, m := range ms {
		if !applies(m, team) && !(team == nil && m.TeamID < 0) {
			continue
		}
		l := markedWindowLine{Marked: m, Labels: ledger.MarkedText(m)}
		if m.TeamID > 0 {
			l.Team = o.byID[m.TeamID].Name
		}
		v.Windows = append(v.Windows, l)
	}
	slices.SortStableFunc(v.Windows, func(a, b markedWindowLine) int { return b.Start.Compare(a.Start) })
	if v.Has, err = s.l.HasMarked(ctx); err != nil {
		return nil, err
	}
	kinds, err := s.l.MarkedKinds(ctx)
	if err != nil {
		return nil, err
	}
	v.Draft = markedDraft{Pick: s.teamPick(r, o), Kinds: kinds}
	return v, nil
}

// teamName is a team's name, "" for everyone.
func (o *owners) teamName(t *ledger.Team) string {
	if t == nil {
		return ""
	}
	return t.Name
}

type markedOneView struct {
	ledger.Marked
	Team, Labels, UntilText string
	Lines                   []occLine
	Cut                     bool
	// Incidents are those that started during one of its occurrences.
	Incidents []ledger.Incident
	Draft     markedDraft
	// Theirs says the marked file or an expression keeps it: changed
	// there.
	Theirs bool
}

func (s *Server) markedOnePage(r *http.Request) (any, error) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, ledger.ErrNotFound
	}
	m, err := s.l.MarkedByID(ctx, id)
	if err != nil {
		return nil, err
	}
	o, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	v := markedOneView{Marked: m, Labels: ledger.MarkedText(m), Theirs: m.Origin == ledger.OriginFile || m.Origin == ledger.OriginExpr}
	if m.TeamID > 0 {
		v.Team = o.byID[m.TeamID].Name
	}
	if !m.Until.IsZero() {
		v.UntilText = m.Until.In(s.cfg.Loc).Format("2006-01-02")
	}
	// Its occurrences, from its start (or the weeks shown) to ahead.
	now := s.now()
	from := now.Add(-markedBack)
	if m.Start.After(from) {
		from = m.Start
	}
	if v.Lines, v.Cut, err = s.markedLines(ctx, []ledger.Marked{m}, from, now.Add(markedAhead), nil, lang(r)); err != nil {
		return nil, err
	}
	if len(v.Lines) > 0 {
		x := &markedSpan{s: s, own: o, ms: []ledger.Marked{m}}
		first := v.Lines[len(v.Lines)-1].Start
		incs, err := s.l.Incidents(ctx, first, v.Lines[0].End.Add(time.Second), false)
		if err != nil {
			return nil, err
		}
		var os []markedOcc
		for _, l := range v.Lines {
			os = append(os, l.markedOcc)
		}
		for _, in := range incs {
			if x.during(in, os) {
				v.Incidents = append(v.Incidents, in)
			}
		}
	}
	kinds, err := s.l.MarkedKinds(ctx)
	if err != nil {
		return nil, err
	}
	v.Draft = markedDraftOf(m, s.cfg.Loc, s.teamPick(r, o), kinds)
	return v, nil
}

// markedDraftOf is a window's form, as kept.
func markedDraftOf(m ledger.Marked, loc *time.Location, pick teamPick, kinds []string) markedDraft {
	d := markedDraft{ID: m.ID, Kind: m.Kind, Title: m.Title, Every: m.Every, On: m.On, Team: m.TeamID, Pick: pick, Kinds: kinds}
	if len(m.Labels) > 0 {
		var ls []string
		for _, k := range slices.Sorted(maps.Keys(m.Labels)) {
			ls = append(ls, k+"="+m.Labels[k])
		}
		d.Labels = strings.Join(ls, " ")
	}
	if !m.Start.IsZero() {
		d.Start, d.End = m.Start.In(loc).Format("2006-01-02T15:04"), m.End.In(loc).Format("2006-01-02T15:04")
	}
	if !m.Until.IsZero() {
		d.Until = m.Until.In(loc).Format("2006-01-02")
	}
	return d
}

// markedLabels reads a marked window's labels from a form: service=search,
// one value each.
func markedLabels(v string) (map[string]string, error) {
	ls, err := parseLabels(v)
	if err != nil {
		return nil, err
	}
	var out map[string]string
	for k, vs := range ls {
		if len(vs) > 1 {
			return nil, bad("labels %q: a marked window's label has one value", v)
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = vs[0]
	}
	return out, nil
}

func (s *Server) postMarked(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	m, err := s.markedForm(r)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	id, err := s.l.PutMarked(r.Context(), m, s.user(r), o)
	s.poke() // to carry on marking what is left
	s.written(w, r, err, fmt.Sprintf("/marked/%d", id))
}

// markedForm is the window a form sends.
func (s *Server) markedForm(r *http.Request) (ledger.Marked, error) {
	start, err := s.parseTime(r.PostFormValue("start"))
	if err != nil {
		return ledger.Marked{}, err
	}
	end, err := s.parseTime(r.PostFormValue("end"))
	if err != nil {
		return ledger.Marked{}, err
	}
	labels, err := markedLabels(r.PostFormValue("labels"))
	if err != nil {
		return ledger.Marked{}, err
	}
	m := ledger.Marked{ID: formInt(r, "id"), Kind: strings.TrimSpace(r.PostFormValue("kind")), Title: r.PostFormValue("title"), Start: start, End: end,
		Every: r.PostFormValue("every"), On: r.PostFormValue("on"), TeamID: formInt(r, "team"), Labels: labels}
	if v := r.PostFormValue("until"); v != "" {
		day, err := dateDay(v)
		if err != nil {
			return m, bad("until %q: want a date like 2026-12-31", v)
		}
		m.Until = s.l.Midnight(day)
	}
	return m, nil
}

func (s *Server) deleteMarked(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	o, err := s.options(r.Context())
	if err == nil {
		err = s.l.DeleteMarked(r.Context(), id, s.user(r), o)
		s.poke()
	}
	s.written(w, r, err, "/marked")
}

// apiMarked is GET /api/v1/marked?from=&to=: the marked windows with an
// occurrence meeting the period, 400 days at most, the last seven days by
// default, each with its occurrences in it.
func (s *Server) apiMarked(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.period(r, 400*24*time.Hour)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	ms, err := s.l.MarkedMeeting(r.Context(), from, to)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	out := []markedAPI{}
	for _, m := range ms {
		os, _ := m.Occurrences(from, to, ledger.MarkedKindWeek*60)
		if len(os) == 0 {
			continue
		}
		out = append(out, markedAPI{Marked: m, Occurrences: os})
	}
	writeJSON(w, out)
}

// markedAPI is a marked window as the API gives it: with its occurrences
// in the period asked.
type markedAPI struct {
	ledger.Marked
	Occurrences []ledger.Occurrence `json:"occurrences,omitempty"`
}

func (s *Server) apiMarkedOne(w http.ResponseWriter, r *http.Request) {
	m, err := s.l.MarkedByID(r.Context(), pathID(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, m)
}

func (s *Server) apiPutMarked(w http.ResponseWriter, r *http.Request) {
	var m ledger.Marked
	if err := readJSON(r, &m); err != nil {
		s.oops(w, r, err)
		return
	}
	m.ID = 0
	if r.Method == http.MethodPut {
		var err error
		if m.ID, err = putID(r); err != nil {
			s.oops(w, r, err)
			return
		}
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	id, err := s.l.PutMarked(r.Context(), m, s.user(r), o)
	s.poke()
	if err != nil {
		s.oops(w, r, err)
		return
	}
	got, err := s.l.MarkedByID(r.Context(), id)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	answer(w, r, fmt.Sprintf("/api/v1/marked/%d", id), got)
}
