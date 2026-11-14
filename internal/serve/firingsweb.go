package serve

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
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
	// Black are those that caught nothing; where its parts are drawn, from
	// the foot up: caught nothing, excused, caught something.
	Black          int
	BlackY, BlackH float64
	ExcusedY       float64
	ExcusedH       float64
	WhiteY, WhiteH float64
}

// dayStrip is a day's firings an hour at a time, as the page lists them.
type dayStrip struct {
	Hours                []hourCell
	Width                int
	Excused, Odd, Capped bool
}

type firingsView struct {
	Day, Rule string
	Black     bool
	Lines     []firingRow
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
	if v := q.Get("day"); v != "" {
		day, err := dateDay(v)
		if err != nil {
			return nil, bad("day %q: want a date like 2026-11-02", v)
		}
		a.day = day
	}
	a.before, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	var more bool
	a.labels, more = labelsOf(q["label"])
	dl := s.l
	v := firingsView{Day: s.l.Midnight(a.day).In(s.cfg.Loc).Format("2006-01-02"), Rule: a.rule, Black: a.black, More: more,
		PrevDay: s.l.Midnight(a.day - 1).In(s.cfg.Loc).Format("2006-01-02"), ThenDay: s.l.Midnight(a.day + 1).In(s.cfg.Loc).Format("2006-01-02")}
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
	fq := ledger.FiringQuery{From: from, To: to, Rule: a.rule, Black: a.black}

	// The firings listed, and those the strip counts: the day's whole.
	var fs []ledger.Firing
	var starts []ledger.StartCount
	if len(a.labels) == 0 {
		var err error
		list := fq
		list.Before, list.Limit = a.before, firingsPage
		if fs, err = s.l.Firings(ctx, list); err != nil {
			return nil, err
		}
		if starts, err = s.l.FiringStarts(ctx, fq); err != nil {
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
			x := ledger.StartCount{Start: f.Start, Firings: 1}
			switch {
			case f.White:
				x.White = 1
			case f.Excused:
				x.Excused = 1
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

	var lines []firingLine
	for _, f := range fs {
		lines = append(lines, s.firingLine(f))
	}
	if err := s.notifiedFirings(ctx, lines); err != nil {
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
func (s *Server) hourStrip(day int64, starts []ledger.StartCount, capped bool) dayStrip {
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
		st.Excused = st.Excused || x.Excused > 0
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
		c.BlackY, c.BlackH = part(c.Black)
		c.ExcusedY, c.ExcusedH = part(c.Excused)
		c.WhiteY, c.WhiteH = part(c.White)
	}
	return st
}

// Cell is where an hour's bar is drawn.
func (c hourCell) Cell() int { return c.X + (hourStep-hourBar)/2 }
