package serve

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// An incident's timeline (docs/design/ui.md): its span on a time axis,
// from its lead before it to its lead after, and a row for each rule that
// caught it, nearly caught it, or fired around it, with its firings drawn:
// what caught something hollow (○), what caught nothing solid (●), what a
// quiet window excused dashed, and when a rule that nearly caught it would
// have fired, a diamond (◇).

// The timeline's bounds: rules, and firings drawn of each; firings read.
const (
	timelineRows    = 20
	timelineFirings = 50
	timelineW       = 400.0
)

var timelineRead = 5000

// timelineMark is a firing drawn on a rule's row, or when one would have
// been: Kind caught, nothing, excused or near; X where it starts, To where
// it ends (as far as the axis goes).
type timelineMark struct {
	Kind    string
	X, To   float64
	Start   time.Time
	End     time.Time
	Title   string
	Ongoing bool
	// Diamond is a near one's shape; Line, that it lasted long enough
	// to draw as a line from its start.
	Diamond   string
	Line      bool
	startText string
}

// timelineRow is a rule's row: Kind caught, near or fired, and what it
// did, in words; its firings drawn, and how many more weren't.
type timelineRow struct {
	Rule, Kind, Did                string
	Marks                          []timelineMark
	More                           int
	Firings, White, Excused, Black int
	// Starts are when each firing drawn started, and its stone, as the
	// table lists them.
	Starts string
	first  time.Time
	delay  time.Duration
}

// timelineView is an incident's timeline: the axis, from From to To, the
// incident's span on it, its rows, and how many more rules fired around it
// than are drawn.
type timelineView struct {
	From, To     time.Time
	Lead         string
	SpanX, SpanW float64
	Rows         []timelineRow
	More         int
	// Capped says more firings were on than were read, Read of them; Near
	// and Excused, that a row has a mark of that kind, which the legend
	// then names.
	Capped, Near, Excused bool
	Read                  int
	Width                 float64
}

// timeline works out an incident's timeline: the firings read once, those
// that were on in the span from its lead before it to its lead after; t
// the tallies, to say whether a rule that caught it was noisy that week;
// near, the rules that nearly caught it.
func (s *Server) timeline(ctx context.Context, lg string, in ledger.Incident, t *tallies, near []verdict.NearMiss) (*timelineView, error) {
	// The span the verdict judges it over, as verdict.Judge has it.
	lead, end := t.o.Lead, in.EndOr(t.o.UnknownLen)
	if end.Before(in.Start) {
		end = in.Start
	}
	v := &timelineView{From: in.Start.Add(-lead), To: end.Add(lead), Lead: verdict.Duration(lead), Width: timelineW}
	span := v.To.Sub(v.From).Seconds()
	x := func(at time.Time) float64 {
		f := at.Sub(v.From).Seconds() / span
		return math.Round(10*(2+(timelineW-4)*min(1, max(0, f)))) / 10
	}
	v.SpanX, v.SpanW = x(in.Start), math.Round(10*(x(end)-x(in.Start)))/10

	// The firings on in the span: started in it, or before it and not ended
	// by its start, a day back at most; the earliest first, so that those
	// that caught it are read before any that came after them.
	fs, err := s.l.FiringsOn(ctx, v.From, v.To, 24*time.Hour, timelineRead+1)
	if err != nil {
		return nil, err
	}
	if len(fs) > timelineRead {
		fs, v.Capped, v.Read = fs[:timelineRead], true, timelineRead
	}
	clock := "15:04"
	rows := map[string]*timelineRow{}
	row := func(rule string) *timelineRow {
		r := rows[rule]
		if r == nil {
			r = &timelineRow{Rule: rule, Kind: "fired"}
			rows[rule] = r
		}
		return r
	}
	week := ledger.Week(s.l.Day(in.Start))
	for _, c := range in.CaughtBy {
		r := row(c.Rule)
		r.Kind, r.delay = "caught", c.Delay
		r.Did = tr(lg, "caught it, %s", leadText(lg, -c.Delay))
		if t.noisy(week, t.id(c.Rule)) {
			r.Did = tr(lg, "caught it, %s, and noisy that week", leadText(lg, -c.Delay))
		}
	}
	for _, nm := range near {
		r := row(nm.Rule)
		if r.Kind == "caught" {
			continue
		}
		at := in.Start.Add(nm.Delay)
		r.Kind, r.Did, r.delay = "near", tr(lg, `nearly caught it: with "for" %s, it would have fired %s`, verdict.Duration(nm.NewFor), leadText(lg, -nm.Delay)), nm.Delay
		r.Marks = append(r.Marks, timelineMark{Kind: "near", X: x(at), Start: at, Title: tr(lg, "it would have fired at %s", at.In(s.cfg.Loc).Format(clock))})
		v.Near = true
	}
	for _, f := range fs {
		r := row(f.Rule)
		if r.Firings == 0 {
			r.first = f.Start
		}
		r.Firings++
		kind := "nothing"
		switch {
		case f.White:
			kind = "caught"
			r.White++
		case f.Excused:
			kind = "excused"
			r.Excused++
			v.Excused = true
		default:
			r.Black++
		}
		if r.Firings > timelineFirings {
			r.More++
			continue
		}
		m := timelineMark{Kind: kind, X: x(f.Start), Start: f.Start, End: f.End, Ongoing: f.End.IsZero()}
		if m.Ongoing || f.End.After(v.To) {
			m.To = x(v.To)
		} else {
			m.To = x(f.End)
		}
		m.Line = m.To-m.X > 1
		m.startText = f.Start.In(s.cfg.Loc).Format(clock)
		m.Title = m.startText + " → "
		if m.Ongoing {
			m.Title += tr(lg, "firing")
		} else {
			m.Title += f.End.In(s.cfg.Loc).Format(clock)
		}
		r.Marks = append(r.Marks, m)
	}
	for _, r := range rows {
		var ts []string
		for _, m := range r.Marks {
			if m.Kind != "near" {
				ts = append(ts, m.startText+" "+map[string]string{"caught": "○", "nothing": "●", "excused": "◌"}[m.Kind])
			}
		}
		r.Starts = strings.Join(ts, ", ")
		if r.Kind == "fired" {
			r.Did = tr(lg, "fired around it")
		}
		for i := range r.Marks {
			if r.Marks[i].Kind == "near" {
				r.Marks[i].Diamond = diamond(r.Marks[i].X)
			}
		}
	}
	// Those that caught it first, then those that nearly did, then the rest
	// by when they first fired.
	all := make([]*timelineRow, 0, len(rows))
	for _, r := range rows {
		all = append(all, r)
	}
	rank := map[string]int{"caught": 0, "near": 1, "fired": 2}
	sort.Slice(all, func(a, b int) bool {
		x, y := all[a], all[b]
		if rank[x.Kind] != rank[y.Kind] {
			return rank[x.Kind] < rank[y.Kind]
		}
		if x.Kind != "fired" && x.delay != y.delay {
			return x.delay < y.delay
		}
		if !x.first.Equal(y.first) {
			return x.first.Before(y.first)
		}
		return x.Rule < y.Rule
	})
	if len(all) > timelineRows {
		v.More = len(all) - timelineRows
		all = all[:timelineRows]
	}
	for _, r := range all {
		v.Rows = append(v.Rows, *r)
	}
	return v, nil
}

// diamond is a diamond at x on a row 16 high.
func diamond(x float64) string {
	return fmt.Sprintf("M%.1f 2L%.1f 8L%.1f 14L%.1f 8Z", x, x+5, x, x-5)
}
