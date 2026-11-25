package serve

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// The firings of a day (docs/design/ui.md): each with its labels as chips,
// each a link to the same day's firings with that label; the labels picked
// so, as chips that take each away; the day's firings an hour at a time;
// and what caught nothing, picked, written down as real.

// maxLabels are the labels the firings are picked by, at most; a label's
// name and value are as long as the ledger keeps them (verdict.KeptLabels).
const (
	maxLabels     = 5
	maxLabelName  = 128
	maxLabelValue = 256
)

// firingsPage is how many firings the page lists at a time; dayScan, how
// many of a day's it looks through for those with the labels picked.
const (
	firingsPage = 100
	dayScan     = 20000
)

// firingsAsk is what the firings page was asked, read once: its links to
// itself are made from it, each with one thing changed.
type firingsAsk struct {
	day    int64
	rule   string
	black  bool
	labels [][2]string
	before int64
	// kind picks those marked with a kind of marked window.
	kind string
	// response picks those with a response (ledger.ResponseKinds), once
	// responses show.
	response string
}

// url is the firings page for what was asked.
func (a firingsAsk) url(loc dayLedger) string {
	q := url.Values{"day": {loc.Midnight(a.day).In(loc.Location()).Format("2006-01-02")}}
	if a.rule != "" {
		q.Set("rule", a.rule)
	}
	if a.black {
		q.Set("black", "1")
	}
	for _, kv := range a.labels {
		q.Add("label", kv[0]+"="+kv[1])
	}
	if a.before > 0 {
		q.Set("before", strconv.FormatInt(a.before, 10))
	}
	if a.kind != "" {
		q.Set("kind", a.kind)
	}
	if a.response != "" {
		q.Set("response", a.response)
	}
	return "/firings?" + q.Encode()
}

// has reports whether a label is among those picked.
func (a firingsAsk) has(k, v string) bool { return slices.Contains(a.labels, [2]string{k, v}) }

// labelsOf are the labels a firings page is picked by: each name=value,
// with a name and value as long as the ledger keeps, each once, maxLabels
// of them; more says some were left out.
func labelsOf(vs []string) (out [][2]string, more bool) {
	for _, v := range vs {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" || val == "" || len(k) > maxLabelName || len(val) > maxLabelValue {
			continue
		}
		if slices.Contains(out, [2]string{k, val}) {
			continue
		}
		if len(out) == maxLabels {
			return out, true
		}
		out = append(out, [2]string{k, val})
	}
	return out, false
}

// labelChip is a firing's label: a link to the day's firings with it too,
// or, picked already or with as many picked as may be, none.
type labelChip struct {
	Text, Href string
	On         bool
}

// chipParts are a label's name and value apart, so that a chip wider than
// the rules list's column breaks after its "=", not inside a word
// (docs/design/ui.md, "The table fits a laptop's screen").
type chipParts struct {
	Name, Value string
	Eq          bool
}

// Parts are the chip's label's name and value.
func (c labelChip) Parts() chipParts {
	k, v, ok := strings.Cut(c.Text, "=")
	return chipParts{k, v, ok}
}

// firingRow is a firing as the page lists it, its labels as chips.
type firingRow struct {
	firingLine
	Chips []labelChip
}

// hourCell is an hour of the day's strip: the firings that started in it,
// by their stones; Clock how many times the hour came that day, 0 where
// the clocks skipped it and 2 where they went back over it.
type hourCell struct {
	Hour                    string
	Firings, White, Excused int
	Clock                   int
	X                       int
	// Black are those that caught nothing, Unjudged of them those with no
	// record to judge them by; where its parts are drawn, from the foot
	// up: caught nothing (judged), not judged, excused, caught something.
	Black, Unjudged      int
	BlackY, BlackH       float64
	UnjudgedY, UnjudgedH float64
	ExcusedY             float64
	ExcusedH             float64
	WhiteY, WhiteH       float64
}

// dayStrip is a day's firings an hour at a time, as the page lists them.
type dayStrip struct {
	Hours                          []hourCell
	Width                          int
	Excused, Odd, Capped, Unjudged bool
	// ByNothing says some rule entry judges its rules by nothing, so that
	// the legend says those not judged may be so for that (bundle 66).
	ByNothing bool
}

type firingsView struct {
	Day, Rule string
	Black     bool
	Lines     []firingRow
	// Responses says what was done about the firings is shown: the column,
	// and the form's choice of Response, one of Kinds, "" for any.
	Responses bool
	Response  string
	Kinds     []string
	// Labels are those picked, each a chip that takes it away; More says
	// some were left out, past maxLabels.
	Labels []chip
	More   bool
	// Hidden are the labels picked, as the form's fields.
	Hidden           []field
	Next, Prev, Then string
	PrevDay, ThenDay string
	Strip            dayStrip
	Bulk             bulkView
	// Said is, asked for no day, which it shows: the last with firings.
	Said firingsSaid
}

// The strip's geometry: an hour every hourStep, its bar hourBar wide, in
// a drawing hourTop high.
const (
	hourStep = 20
	hourBar  = 14
	hourTop  = 44.0
)

func (s *Server) firingsPage(r *http.Request) (any, error) {
	ctx := r.Context()
	q := r.URL.Query()
	a := firingsAsk{day: s.l.Day(s.now()), rule: q.Get("rule"), black: q.Get("black") != ""}
	if k := q.Get("kind"); k != "" && ledger.CheckKind(k) == nil {
		a.kind = k
	}
	a.response = responseAsked(q)
	o, err := s.options(ctx)
	if err != nil {
		return nil, err
	}
	shown, err := s.responsesShown(ctx, o)
	if err != nil {
		return nil, err
	}
	if !shown {
		a.response = "" // nothing to pick by
	}
	if v := q.Get("day"); v != "" {
		day, err := dateDay(v)
		if err != nil {
			return nil, bad("day %q: want a date like 2026-11-02", v)
		}
		a.day = day
	}
	// Without a day, the last day with firings, not an empty today.
	var said firingsSaid
	if q.Get("day") == "" {
		var err error
		if a.day, said, err = s.lastFiringDay(r, a.rule, a.black, a.response); err != nil {
			return nil, err
		}
	}
	a.before, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	var more bool
	a.labels, more = labelsOf(q["label"])
	dl := s.l
	v := firingsView{Responses: shown, Response: a.response, Kinds: ledger.ResponseKinds, Said: said, Day: s.l.Midnight(a.day).In(s.cfg.Loc).Format("2006-01-02"), Rule: a.rule, Black: a.black, More: more,
		PrevDay: s.l.Midnight(a.day - 1).In(s.cfg.Loc).Format("2006-01-02"), ThenDay: s.l.Midnight(a.day + 1).In(s.cfg.Loc).Format("2006-01-02")}
	if a.kind != "" {
		v.Hidden = append(v.Hidden, field{"kind", a.kind})
		b := a
		b.before, b.kind = 0, ""
		v.Labels = append(v.Labels, chip{Group: "Marked", Value: a.kind, Raw: true, Href: b.url(dl)})
	}
	if a.response != "" {
		b := a
		b.before, b.response = 0, ""
		v.Labels = append(v.Labels, chip{Group: "Response", Value: responseLabel[a.response], Href: b.url(dl)})
	}
	for _, kv := range a.labels {
		v.Hidden = append(v.Hidden, field{"label", kv[0] + "=" + kv[1]})
		b := a
		b.before = 0
		b.labels = slices.DeleteFunc(slices.Clone(a.labels), func(x [2]string) bool { return x == kv })
		v.Labels = append(v.Labels, chip{Group: "Label", Value: kv[0] + "=" + kv[1], Raw: true, Href: b.url(dl)})
	}
	for _, d := range []struct {
		to   *string
		days int64
	}{{&v.Prev, -1}, {&v.Then, 1}} {
		b := a
		b.day, b.before = a.day+d.days, 0
		*d.to = b.url(dl)
	}
	from, to := s.l.Midnight(a.day), s.l.Midnight(a.day+1)
	fq := ledger.FiringQuery{From: from, To: to, Rule: a.rule, Black: a.black, Kind: a.kind, Response: a.response}
	// A firing that caught nothing is judged, or only counted, as the
	// report says it: its week's record or the signals tied to it, by
	// everyone's incidents; or, one rule's firings come to from its page
	// seen from its team's (?team=), by the incidents the team owns, as
	// that page judged them.
	var keeps func(ledger.Incident) bool
	if a.rule != "" && q.Get("team") != "" {
		own, err := s.owners(ctx)
		if err != nil {
			return nil, err
		}
		if team, ok := own.team(q.Get("team")); ok && own.rule[a.rule] == team.ID {
			keeps = teamKeeps(own, &team)
		}
	}
	judge, err := s.judgeOver(ctx, from, to, o, keeps)
	if err != nil {
		return nil, err
	}

	// The firings listed, and those the strip counts: the day's whole.
	var fs []ledger.Firing
	var starts []startCount
	if len(a.labels) == 0 {
		var err error
		list := fq
		list.Before, list.Limit = a.before, firingsPage
		if fs, err = s.l.Firings(ctx, list); err != nil {
			return nil, err
		}
		if starts, err = s.startsJudged(ctx, fq, o, judge); err != nil {
			return nil, err
		}
	} else {
		// Those with the labels picked, of the latest dayScan of the day.
		all := fq
		all.Limit = dayScan + 1
		day, err := s.l.Firings(ctx, all)
		if err != nil {
			return nil, err
		}
		if len(day) > dayScan {
			day, v.Strip.Capped = day[:dayScan], true
		}
		var after *ledger.Firing
		if a.before > 0 {
			if f, err := s.l.FiringByID(ctx, a.before); err == nil {
				after = &f
			}
		}
		for _, f := range day {
			if !hasLabels(f.Labels, a.labels) {
				continue
			}
			x := startCount{StartCount: ledger.StartCount{Start: f.Start, Firings: 1}}
			switch {
			case f.White:
				x.White = 1
			case f.Excused:
				x.Excused = 1
			case !judge(f.Rule, f.Start, f.Signals):
				x.Unjudged = 1
			}
			starts = append(starts, x)
			if after != nil && !(f.Start.Before(after.Start) || f.Start.Equal(after.Start) && f.ID < after.ID) {
				continue
			}
			if len(fs) < firingsPage {
				fs = append(fs, f)
			}
		}
	}
	v.Strip = s.hourStrip(a.day, starts, v.Strip.Capped)
	v.Strip.ByNothing = v.Strip.Unjudged && o.Signals.NotJudgedRules()

	var lines []firingLine
	for _, f := range fs {
		lines = append(lines, s.firingLine(f))
	}
	judged(lines, judge, o)
	if err := s.respondedOn(ctx, lines, judge, o); err != nil {
		return nil, err
	}
	if err := s.notifiedFirings(ctx, lines); err != nil {
		return nil, err
	}
	if err := s.saidOn(r, lines); err != nil {
		return nil, err
	}
	for _, l := range lines {
		row := firingRow{firingLine: l}
		for _, k := range slices.Sorted(maps.Keys(l.Firing.Labels)) {
			val := l.Firing.Labels[k]
			c := labelChip{Text: k + "=" + val}
			switch {
			case a.has(k, val):
				c.On = true
			case len(a.labels) < maxLabels && len(k) <= maxLabelName && len(val) <= maxLabelValue && val != "":
				b := a
				b.before = 0
				b.labels = append(slices.Clone(a.labels), [2]string{k, val})
				c.Href = b.url(dl)
			}
			row.Chips = append(row.Chips, c)
		}
		v.Lines = append(v.Lines, row)
	}
	if len(fs) == firingsPage {
		b := a
		b.before = fs[len(fs)-1].ID
		v.Next = b.url(dl)
	}
	v.Bulk = s.bulkFor(r, "editor")
	v.Bulk.Back = a.url(dl)
	return v, nil
}

// hasLabels reports whether a firing has each label picked.
func hasLabels(ls map[string]string, want [][2]string) bool {
	for _, kv := range want {
		if v, ok := ls[kv[0]]; !ok || v != kv[1] {
			return false
		}
	}
	return true
}

// hourStrip counts a day's firings by the hour of the clock each started
// in, the day as the ledger's days start (Midnight: on a day whose midnight
// the clocks skip, from 01:00), each hour as often as the clocks came to
// it that day: from starts, counted by a quarter of an hour, or a firing
// at a time, which each fall in one hour of the clock.
// startCount is a count of firings by when they started, and, of those
// that caught nothing, Unjudged: those with no record to judge them by.
type startCount struct {
	ledger.StartCount
	Unjudged int
}

// startsJudged are the firings fq picks counted by the quarter hour, as
// FiringStarts counts them, each count's Unjudged by judge: with no named
// signal, a day's firings are judged alike, by its week; with named
// signals, by rule and by the signals tied to them, counted apart.
func (s *Server) startsJudged(ctx context.Context, fq ledger.FiringQuery, o verdict.Options, judge firingJudge) ([]startCount, error) {
	// Apart by rule where a rule's own entry may decide its judging:
	// named signals, or an entry that judges its rules by nothing, whose
	// firings the strip would count as caught nothing (bundle 66).
	read := s.l.FiringStarts
	if o.Signals.Named() || o.Signals.NotJudgedRules() {
		read = s.l.FiringStartsBy
	}
	xs, err := read(ctx, fq)
	if err != nil {
		return nil, err
	}
	out := make([]startCount, len(xs))
	for i, x := range xs {
		out[i].StartCount = x
		if black := x.Firings - x.White - x.Excused; black > 0 && !judge(x.Rule, x.Start, x.Signals) {
			out[i].Unjudged = black
		}
	}
	return out, nil
}

func (s *Server) hourStrip(day int64, starts []startCount, capped bool) dayStrip {
	st := dayStrip{Width: 24 * hourStep, Capped: capped}
	loc := s.cfg.Loc
	for h := range 24 {
		st.Hours = append(st.Hours, hourCell{Hour: fmt.Sprintf("%02d:00", h), X: h * hourStep})
	}
	from, to := s.l.Midnight(day), s.l.Midnight(day+1)
	for t := from; t.Before(to); t = t.Add(time.Hour) {
		st.Hours[t.In(loc).Hour()].Clock++
	}
	for _, x := range starts {
		c := &st.Hours[x.Start.In(loc).Hour()]
		c.Firings += x.Firings
		c.White += x.White
		c.Excused += x.Excused
		c.Unjudged += x.Unjudged
		st.Excused = st.Excused || x.Excused > 0
		st.Unjudged = st.Unjudged || x.Unjudged > 0
	}
	most := 1
	for i := range st.Hours {
		c := &st.Hours[i]
		c.Black = c.Firings - c.White - c.Excused
		most = max(most, c.Firings)
		st.Odd = st.Odd || c.Clock != 1
	}
	for i := range st.Hours {
		c := &st.Hours[i]
		y := hourTop
		part := func(n int) (float64, float64) {
			if n == 0 {
				return 0, 0
			}
			h := max(2, (hourTop-8)*float64(n)/float64(most))
			y -= h
			top := y
			y--
			return top, h
		}
		c.BlackY, c.BlackH = part(c.Black - c.Unjudged)
		c.UnjudgedY, c.UnjudgedH = part(c.Unjudged)
		c.ExcusedY, c.ExcusedH = part(c.Excused)
		c.WhiteY, c.WhiteH = part(c.White)
	}
	return st
}

// Nothing are, of Black, those judged: black stones.
func (c hourCell) Nothing() int { return c.Black - c.Unjudged }

// Cell is where an hour's bar is drawn.
func (c hourCell) Cell() int { return c.X + (hourStep-hourBar)/2 }
