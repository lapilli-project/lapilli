package serve

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// The overview is a dashboard of the last twelve weeks, read from the weekly
// tallies and the weeks' incidents: what a person opens on Monday.

// weekView is one week: its stones, and its incidents by what caught them.
type weekView struct {
	Start                                time.Time
	Firings, White, Black, Noisy         int
	Excused                              int
	Incidents, Caught, NoisyOnly, Missed int
	Delays                               []time.Duration // to the first catch by a rule that isn't noisy
	Asleep                               int             // firings that caught nothing while people slept; notifications for them, once there are any
	Uncounted                            bool            // Asleep isn't counted: a week before the first notification
	Changed, Current                     bool
	// Refired are its firings that re-fired; Deploys those that came
	// during a deploy, DeploysCaught those of them that caught something.
	Refired, Deploys, DeploysCaught int
}

// Without is the week's precision leaving out the firings during deploys.
func (w weekView) Without() string { return pct(w.White-w.DeploysCaught, w.Firings-w.Deploys) }

func (w weekView) precision() (float64, bool) {
	if w.Firings == 0 {
		return 0, false
	}
	return float64(w.White) / float64(w.Firings), true
}

// Precision is the week's precision for the table, as the precision chart
// draws it; "–" with no firings.
func (w weekView) Precision() string {
	p, ok := w.precision()
	if !ok {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", 100*p)
}

// median is the week's median time to catch, ok false with none caught.
func (w weekView) median() (time.Duration, bool) {
	if len(w.Delays) == 0 {
		return 0, false
	}
	d := slices.Sorted(slices.Values(w.Delays))
	if n := len(d); n%2 == 1 {
		return d[n/2], true
	} else {
		return (d[n/2-1] + d[n/2]) / 2, true
	}
}

// tile is a headline number for the last whole week, beside the week
// before, with its twelve weeks drawn small.
type tile struct {
	Label, Value, Of, Delta string
	// Better and Worse color the delta: whether the change is good news.
	Better, Worse bool
	Spark         string // an SVG path, 96 by 24
	DotX, DotY    float64
	Has           bool   // the sparkline has a point to end on
	Values        string // the sparkline's, the oldest first, for those who can't see it
}

// geometry of the column charts
const (
	chartW, chartH       = 560.0, 200.0
	plotL, plotR         = 44.0, 10.0
	plotT, plotB         = 12.0, 28.0
	gap                  = 2.0 // between stacked segments
	maxColumn            = 24.0
	sparkW, sparkH       = 96.0, 24.0
	heatCells            = 12
	heatRows             = 12
	actionsShown         = 8
	missedShownInActions = 3
)

type tick struct {
	Y     float64
	Label string
}

type segment struct {
	Class, Path, Title string
}

type column struct {
	X, W, Mid float64
	Segments  []segment
	Label     string
	Changed   bool
	Current   bool
	Alt       bool // every other week, left unlabelled on a narrow screen
}

type columnChart struct {
	Columns []column
	Ticks   []tick
	Base    float64
}

type point struct {
	X, Y  float64
	Title string
}

type lineChart struct {
	Path      string
	Points    []point
	Ticks     []tick
	Labels    []column // for the week labels only
	End       point
	EndLabel  string
	Base      float64
	HasPoints bool
}

type heatCell struct {
	Class, Title string
	Black        int // what the cell's shade shows, for those who can't see it
}

type heatRow struct {
	Name  string
	Cells []heatCell
	Black int
}

// action is something to look at, and the task it would make, or the
// one open about it.
type action struct {
	Text, Detail, Href          string
	Kind, Rule, Incident, Title string
	Task                        int64
}

type dashboard struct {
	Empty, Backlog bool
	Cursor         time.Time
	Run            *ledger.Run
	From, To       time.Time // the last whole week
	Tiles          []tile
	Stones         columnChart
	Precision      lineChart
	Incidents      columnChart
	Heat           []heatRow
	HeatWeeks      []string
	Actions        []action
	Weeks          []weekView // the latest first, for the table
	Review         *reviewView
	Team           teamFilter
	// Check is the replay beside what really fired over the last whole
	// week, with the rules that differ; nil when that isn't checked.
	Check       *weekCheck
	Tasks       []taskLine // open, the latest changed first
	Outcomes    []outcomeLine
	OutcomesAll bool // a team is picked, and the outcomes are everyone's
	ReadOnly    bool
	// Deploys says the ledger keeps deploys.
	Deploys bool
}

// outcomeLine is a line of the outcomes card, Bad when it asks for a look.
type outcomeLine struct {
	Text string
	Bad  bool
}

func (s *Server) overview(r *http.Request) (any, error) {
	ctx := r.Context()
	l := lang(r)
	t, err := s.tallies(ctx, weeks)
	if err != nil {
		return nil, err
	}
	v := dashboard{}
	if v.Cursor, err = s.l.Cursor(ctx); err != nil {
		return nil, err
	}
	v.Empty = v.Cursor.IsZero()
	v.Backlog = !v.Empty && s.now().Sub(v.Cursor) > 2*s.cfg.Every+s.cfg.Settle
	if runs, err := s.l.Runs(ctx, 1); err != nil {
		return nil, err
	} else if len(runs) > 0 {
		v.Run = &runs[0]
	}
	if v.Empty {
		return v, nil
	}
	v.ReadOnly = s.cfg.ReadOnly
	o, err := s.owners(ctx)
	if err != nil {
		return nil, err
	}
	var team *ledger.Team
	v.Team, team = s.teamFilter(o, r.URL.Query().Get("team"))
	if team != nil {
		t.only(o, *team)
	}

	from := s.l.Midnight(t.weeks[0])
	incs, err := s.l.Incidents(ctx, from, s.now().Add(24*time.Hour), true)
	if err != nil {
		return nil, err
	}
	vs, err := s.l.Versions(ctx, from, s.now(), false)
	if err != nil {
		return nil, err
	}
	first, _, err := s.l.Bounds(ctx)
	if err != nil {
		return nil, err
	}

	byWeek := map[int64]*weekView{}
	var ws []*weekView
	for _, w := range t.weeks {
		wv := &weekView{Start: s.l.Midnight(w), Current: w == t.weeks[len(t.weeks)-1]}
		for id, x := range t.by[w] {
			wv.Firings += x.Firings
			wv.White += x.White
			wv.Excused += x.Excused
			wv.Refired += x.Refired
			wv.Deploys += x.Deploy
			wv.DeploysCaught += x.DeployWhite
			if t.noisy(w, id) {
				wv.Noisy++
			}
		}
		wv.Black = wv.Firings - wv.White
		byWeek[w] = wv
		ws = append(ws, wv)
	}
	for _, ver := range vs {
		if !ver.Since.IsZero() {
			if wv := byWeek[ledger.Week(s.l.Day(ver.Since))]; wv != nil {
				wv.Changed = true
			}
		}
	}
	hours, err := s.byHours(ctx, t)
	if err != nil {
		return nil, err
	}
	for w, by := range hours {
		if wv := byWeek[w]; wv != nil {
			for id, x := range by {
				if _, ok := t.by[w][id]; ok { // the team's rules, when a team is picked
					wv.Asleep += x.AsleepBlack
				}
			}
		}
	}
	recent := t.weeks[len(t.weeks)-4]
	var missed []ledger.Incident
	undecided := 0
	for _, in := range incs {
		w := ledger.Week(s.l.Day(in.Start))
		if in.Origin == "impact" && in.State == "" && in.Counts && w >= recent {
			undecided++
		}
		if !in.Counts || (team != nil && !o.ownsIncident(*team, in)) {
			continue
		}
		wv := byWeek[w]
		if wv == nil {
			continue
		}
		wv.Incidents++
		switch t.state(in, w) {
		case "caught":
			wv.Caught++
			for _, c := range in.CaughtBy {
				if !t.noisy(w, t.id(c.Rule)) {
					wv.Delays = append(wv.Delays, c.Delay)
					break
				}
			}
		case "noisy":
			wv.NoisyOnly++
		default:
			wv.Missed++
			if w >= recent {
				missed = append(missed, in)
			}
		}
	}

	// The last whole week, beside the week before.
	last, before := ws[len(ws)-2], ws[len(ws)-3]
	v.From, v.To = last.Start, ws[len(ws)-1].Start
	asleep := o.schedules(t.o.Hours).asleepNote(l, t, team, s.cfg.Loc)
	// Once notifications come, woken for nothing counts them: a
	// notification, unlike a firing, is a person woken.
	if first, ok, err := s.woken(ctx, t, o, team, byWeek); err != nil {
		return nil, err
	} else if ok && asleep != "" {
		asleep = tr(l, "notifications for firings that caught nothing, while people slept, since %s", first.In(s.cfg.Loc).Format("2006-01-02"))
	}
	if v.Deploys, err = s.l.HasDeploys(ctx); err != nil {
		return nil, err
	}
	v.Tiles = s.tiles(l, ws, last, before, asleep, v.Deploys, func() (int, int) {
		return t.refireWeeks(t.weeks, func(int64) bool { return true }) // the team's, when one is picked
	})
	if s.checking() {
		// Only a week checked to its end: one checked in part says less than
		// it seems to.
		to, err := s.l.CheckedTo(ctx)
		if err != nil {
			return nil, err
		}
		if !to.Before(v.To) {
			in := func(rule string) bool { return team == nil || o.rule[rule] == team.ID }
			all, err := s.l.CheckTotal(ctx, v.From, v.To, in)
			if err != nil {
				return nil, err
			}
			cs, err := s.l.Checks(ctx, v.From, v.To)
			if err != nil {
				return nil, err
			}
			v.Check = weekCheckOf(all, cs, s.differs(), in)
		}
	}
	v.Stones = stonesChart(l, ws, s.cfg.Loc)
	v.Precision = precisionChart(l, ws, s.cfg.Loc)
	v.Incidents = incidentsChart(l, ws, s.cfg.Loc)
	for i := len(ws) - 1; i >= 0; i-- {
		if !first.IsZero() && !ws[i].Start.Before(s.l.Midnight(ledger.Week(s.l.Day(first)))) {
			v.Weeks = append(v.Weeks, *ws[i])
		}
	}

	lines, silent, err := s.ruleLines(ctx, l, t, 4)
	if err != nil {
		return nil, err
	}
	v.Heat, v.HeatWeeks = s.heat(l, t, lines)
	reviews, err := s.reviews(ctx, "", 1)
	if err != nil {
		return nil, err
	}
	if len(reviews) > 0 {
		v.Review = &reviews[0]
	}
	if team != nil {
		silent = slices.DeleteFunc(silent, func(name string) bool { return o.rule[name] != team.ID })
	}
	tq := ledger.TaskQuery{Status: []string{"open", "doing"}, Limit: 200}
	if team != nil {
		tq.TeamID = team.ID
	}
	open, err := s.l.Tasks(ctx, tq)
	if err != nil {
		return nil, err
	}
	v.Tasks = s.taskLines(o, open)
	v.Actions = s.actions(l, lines, silent, missed, undecided)
	// What is already being seen to links to its task.
	for i, a := range v.Actions {
		for _, x := range open {
			// A task to tune a rule doesn't see to whether to retire it.
			if (a.Rule != "" && x.Rule == a.Rule && (a.Kind != "retire" || x.Kind == "retire")) || (a.Incident != "" && x.Incident == a.Incident) {
				v.Actions[i].Task = x.ID
				break
			}
		}
	}
	if len(v.Tasks) > 6 {
		v.Tasks = v.Tasks[:6]
	}
	out, err := s.outcomes(ctx)
	if err != nil {
		return nil, err
	}
	v.Outcomes, v.OutcomesAll = outcomeLines(l, out), team != nil
	return v, nil
}

// outcomeLines are the outcomes in short, for the overview.
func outcomeLines(l string, out *outcomes) []outcomeLine {
	var lines []outcomeLine
	e, u := out.Effect, out.Use
	switch {
	case e.Fewer > 0:
		lines = append(lines, outcomeLine{Text: tr(l, "%d fewer firings since the rule changes than at their rates before", e.Fewer)})
	case e.Fewer < 0:
		lines = append(lines, outcomeLine{Text: tr(l, "%d more firings since the rule changes than at their rates before", -e.Fewer)})
	}
	if e.Measured > 0 {
		lines = append(lines, outcomeLine{Text: tr(l, "%d of %d changes measured came out as predicted", e.AsPredicted, e.Measured)})
	}
	if out.Record.Quiet {
		lines = append(lines, outcomeLine{Text: tr(l, "No incident written down in two weeks or more: are they still being written down?"), Bad: true})
	}
	if u.Suggested > 0 {
		lines = append(lines, outcomeLine{Text: tr(l, "%d of %d suggestions acted on", u.ActedOn, u.Suggested)})
	}
	if u.People > 0 {
		lines = append(lines, outcomeLine{Text: tr(l, "%d edits in four weeks, by %d people", u.Edits, u.People)})
	} else {
		lines = append(lines, outcomeLine{Text: tr(l, "%d edits in four weeks", u.Edits)})
	}
	return lines
}

// tiles are the headline numbers.
func (s *Server) tiles(l string, ws []*weekView, last, before *weekView, asleep string, deploys bool, refired func() (int, int)) []tile {
	var out []tile
	add := func(label string, val func(*weekView) (float64, bool), show func(float64) string, delta func(a, b float64) string, better int, of string) {
		x := tile{Label: tr(l, label), Value: "–", Of: of}
		if a, ok := val(last); ok {
			x.Value = show(a)
			if b, ok := val(before); ok && a != b {
				x.Delta = tr(l, "%s on the week before", delta(a, b))
				x.Better = better != 0 && (a-b)*float64(better) > 0
				x.Worse = better != 0 && (a-b)*float64(better) < 0
			} else if ok {
				x.Delta = tr(l, "as the week before")
			}
		}
		var ys []float64
		var has []bool
		var vals []string
		for _, w := range ws[:len(ws)-1] { // whole weeks
			y, ok := val(w)
			ys = append(ys, y)
			has = append(has, ok)
			if ok {
				vals = append(vals, show(y))
			} else {
				vals = append(vals, "–")
			}
		}
		x.Spark, x.DotX, x.DotY, x.Has = sparkline(ys, has)
		x.Values = strings.Join(vals, ", ")
		out = append(out, x)
	}
	pct := func(f float64) string { return fmt.Sprintf("%.0f%%", 100*f) }
	count := func(f float64) string { return fmt.Sprintf("%d", int(f)) }
	add("Precision", func(w *weekView) (float64, bool) { return w.precision() }, pct,
		func(a, b float64) string { return fmt.Sprintf("%+.0f pt", 100*(a-b)) }, +1, tr(l, "of firings caught something"))
	excused := ""
	if last.Excused > 0 {
		excused = tr(l, "%d more excused by quiet windows", last.Excused)
	}
	add("Firings", func(w *weekView) (float64, bool) { return float64(w.Firings), true }, count,
		func(a, b float64) string {
			if b == 0 {
				return fmt.Sprintf("%+d", int(a-b))
			}
			return fmt.Sprintf("%+.0f%%", 100*(a-b)/b)
		}, 0, excused)
	add("Incidents missed", func(w *weekView) (float64, bool) { return float64(w.Missed), true }, count,
		func(a, b float64) string { return fmt.Sprintf("%+d", int(a-b)) }, -1, tr(l, "of %d", last.Incidents))
	add("Time to catch", func(w *weekView) (float64, bool) {
		d, ok := w.median()
		return d.Seconds(), ok
	}, func(f float64) string { return verdict.Delay(time.Duration(f) * time.Second) },
		func(a, b float64) string {
			return verdict.Delay(time.Duration(a-b) * time.Second)
		}, -1, tr(l, "median, by a rule that isn't noisy"))
	add("Noisy rules", func(w *weekView) (float64, bool) { return float64(w.Noisy), true }, count,
		func(a, b float64) string { return fmt.Sprintf("%+d", int(a-b)) }, -1, "")
	if asleep != "" {
		add("Woken for nothing", func(w *weekView) (float64, bool) { return float64(w.Asleep), !w.Uncounted }, count,
			func(a, b float64) string { return fmt.Sprintf("%+d", int(a-b)) }, -1, asleep)
	}
	// Re-fires: how many, and in how many of the whole weeks shown, for a
	// team that keeps to none a week.
	weeksOf := ""
	if with, of := refired(); of > 0 {
		weeksOf = tr(l, "%d of the last %d whole weeks had one", with, of)
	}
	add("Refired", func(w *weekView) (float64, bool) { return float64(w.Refired), true }, count,
		func(a, b float64) string { return fmt.Sprintf("%+d", int(a-b)) }, -1, weeksOf)
	if deploys {
		add("During deploys", func(w *weekView) (float64, bool) { return float64(w.Deploys), true }, count,
			func(a, b float64) string { return fmt.Sprintf("%+d", int(a-b)) }, 0, tr(l, "firings; precision %s without them", last.Without()))
	}
	return out
}

// sparkline draws the values that are there as a line, and where it ends.
func sparkline(ys []float64, has []bool) (string, float64, float64, bool) {
	lo, hi := math.Inf(1), math.Inf(-1)
	for i, y := range ys {
		if has[i] {
			lo, hi = min(lo, y), max(hi, y)
		}
	}
	if math.IsInf(lo, 1) {
		return "", 0, 0, false
	}
	if hi == lo {
		hi, lo = hi+1, lo-1
	}
	var b strings.Builder
	var x, y float64
	pen := "M"
	for i, v := range ys {
		if !has[i] {
			pen = "M"
			continue
		}
		x = 2 + float64(i)*(sparkW-4)/float64(max(len(ys)-1, 1))
		y = 2 + (sparkH-4)*(1-(v-lo)/(hi-lo))
		fmt.Fprintf(&b, "%s%.1f %.1f ", pen, x, y)
		pen = "L"
	}
	return strings.TrimSpace(b.String()), x, y, has[len(has)-1]
}

// niceTicks are round ticks from 0 up to at least top.
func niceTicks(top float64) (float64, []float64) {
	if top <= 0 {
		top = 1
	}
	step := math.Pow(10, math.Floor(math.Log10(top/4)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if top/(step*m) <= 4 {
			step *= m
			break
		}
	}
	end := math.Ceil(top/step) * step
	var out []float64
	for v := 0.0; v <= end+step/2; v += step {
		out = append(out, v)
	}
	return end, out
}

// columnPath is a column from the baseline up, its data end rounded.
func columnPath(x, w, bottom, top float64, round bool) string {
	if bottom-top < 0.5 {
		return ""
	}
	r := min(4.0, w/2, bottom-top)
	if !round {
		return fmt.Sprintf("M%.1f %.1fV%.1fH%.1fV%.1fZ", x, bottom, top, x+w, bottom)
	}
	return fmt.Sprintf("M%.1f %.1fV%.1fQ%.1f %.1f %.1f %.1fH%.1fQ%.1f %.1f %.1f %.1fV%.1fZ",
		x, bottom, top+r, x, top, x+r, top, x+w-r, x+w, top, x+w, top+r, bottom)
}

// stacked lays out a column chart of stacked parts, the first at the
// baseline.
func stacked(l string, ws []*weekView, loc *time.Location, parts func(*weekView) []int, classes, names []string) columnChart {
	most := 0
	for _, w := range ws {
		n := 0
		for _, v := range parts(w) {
			n += v
		}
		most = max(most, n)
	}
	end, ticks := niceTicks(float64(most))
	if most < 4 {
		// Counts this small are ticked one by one, not by fractions.
		end, ticks = float64(max(most, 1)), nil
		for v := 0; v <= max(most, 1); v++ {
			ticks = append(ticks, float64(v))
		}
	}
	plotW, plotH := chartW-plotL-plotR, chartH-plotT-plotB
	base := plotT + plotH
	band := plotW / float64(len(ws))
	cw := min(maxColumn, band*0.6)
	c := columnChart{Base: base}
	for _, v := range ticks {
		c.Ticks = append(c.Ticks, tick{Y: base - plotH*v/end, Label: compact(v)})
	}
	for i, w := range ws {
		x := plotL + band*float64(i) + (band-cw)/2
		col := column{X: x, W: cw, Mid: x + cw/2, Label: w.Start.In(loc).Format("01-02"), Changed: w.Changed, Current: w.Current,
			Alt: (len(ws)-1-i)%2 == 1}
		ps := parts(w)
		top := len(ps) - 1
		for top >= 0 && ps[top] == 0 {
			top--
		}
		y := base
		for j, n := range ps {
			if n == 0 {
				continue
			}
			h := plotH * float64(n) / end
			lo := y
			if y < base {
				lo -= gap // the surface between two parts
			}
			hi := y - h
			if p := columnPath(x, cw, lo, hi, j == top); p != "" {
				col.Segments = append(col.Segments, segment{classes[j], p,
					tr(l, "week of %s: %d %s", w.Start.In(loc).Format("2006-01-02"), n, tr(l, names[j]))})
			}
			y = hi
		}
		c.Columns = append(c.Columns, col)
	}
	return c
}

func compact(v float64) string {
	switch {
	case v >= 10000:
		return fmt.Sprintf("%.0fK", v/1000)
	case v == math.Trunc(v):
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

func stonesChart(l string, ws []*weekView, loc *time.Location) columnChart {
	return stacked(l, ws, loc, func(w *weekView) []int { return []int{w.White, w.Black} },
		[]string{"caught", "nothing"}, []string{"caught something", "caught nothing"})
}

func incidentsChart(l string, ws []*weekView, loc *time.Location) columnChart {
	return stacked(l, ws, loc, func(w *weekView) []int { return []int{w.Caught, w.NoisyOnly, w.Missed} },
		[]string{"good", "warn", "bad"}, []string{"caught", "caught only by noisy rules", "missed"})
}

func precisionChart(l string, ws []*weekView, loc *time.Location) lineChart {
	top := 0.0
	for _, w := range ws {
		if p, ok := w.precision(); ok {
			top = max(top, p)
		}
	}
	end, ticks := niceTicks(100 * top)
	if end < 10 {
		end, ticks = 10, []float64{0, 5, 10}
	}
	plotW, plotH := chartW-plotL-plotR, chartH-plotT-plotB
	base := plotT + plotH
	band := plotW / float64(len(ws))
	c := lineChart{Base: base}
	for _, v := range ticks {
		c.Ticks = append(c.Ticks, tick{Y: base - plotH*v/end, Label: compact(v) + "%"})
	}
	var b strings.Builder
	pen := "M"
	for i, w := range ws {
		x := plotL + band*float64(i) + band/2
		c.Labels = append(c.Labels, column{X: x, Mid: x, Label: w.Start.In(loc).Format("01-02"), Current: w.Current, Alt: (len(ws)-1-i)%2 == 1})
		p, ok := w.precision()
		if !ok {
			pen = "M"
			continue
		}
		y := base - plotH*100*p/end
		fmt.Fprintf(&b, "%s%.1f %.1f ", pen, x, y)
		pen = "L"
		title := tr(l, "week of %s: %.0f%% of %d firings caught something", w.Start.In(loc).Format("2006-01-02"), 100*p, w.Firings)
		if w.Current {
			title += " " + tr(l, "(so far)")
		}
		c.Points = append(c.Points, point{x, y, title})
		c.End, c.EndLabel = point{x, y, title}, fmt.Sprintf("%.0f%%", 100*p)
		c.HasPoints = true
	}
	c.Path = strings.TrimSpace(b.String())
	return c
}

// heat is the noisiest rules of the last four weeks, their black stones
// week by week, in five steps of the one hue.
func (s *Server) heat(l string, t *tallies, lines []ruleLine) ([]heatRow, []string) {
	var labels []string
	for _, w := range t.weeks {
		labels = append(labels, s.l.Midnight(w).In(s.cfg.Loc).Format("01-02"))
	}
	var rows []heatRow
	var ids []int64
	most := 1
	for _, l := range lines {
		if l.Black == 0 || len(rows) == heatRows {
			continue
		}
		id := t.id(l.Name)
		for _, w := range t.weeks {
			most = max(most, t.by[w][id].Black())
		}
		rows = append(rows, heatRow{Name: l.Name, Black: l.Black})
		ids = append(ids, id)
	}
	for i := range rows {
		for j, w := range t.weeks {
			x := t.by[w][ids[i]]
			class := "h0"
			if n := x.Black(); n > 0 {
				// Five steps, by the square root: a few noisy weeks
				// shouldn't wash out the rest.
				class = fmt.Sprintf("h%d", 1+min(4, int(5*math.Sqrt(float64(n)/float64(most))-1e-9)))
			}
			rows[i].Cells = append(rows[i].Cells, heatCell{class,
				tr(l, "%s, week of %s: %d caught nothing, %d caught something", rows[i].Name, labels[j], x.Black(), x.White), x.Black()})
		}
	}
	return rows, labels
}

// actions are what to look at this week, the most telling first.
func (s *Server) actions(l string, lines []ruleLine, silent []string, missed []ledger.Incident, undecided int) []action {
	var out []action
	path := func(name string) string { return "/rules/" + url.PathEscape(name) }
	// What speaks of four weeks links to the rule's page over them.
	four := func(name string) string { return path(name) + "?weeks=4" }
	for _, x := range lines {
		if x.Suggestion != "" {
			out = append(out, action{Text: tr(l, "Quiet %s", x.Name), Detail: x.Suggestion, Href: path(x.Name),
				Kind: "tune", Rule: x.Name, Title: tr(l, "Quiet %s", x.Name) + ": " + x.Suggestion})
		}
	}
	for _, x := range lines {
		if x.Noisy && x.Suggestion == "" {
			d := tr(l, "fired %d times in four weeks, %d caught something", x.Firings, x.White)
			if x.Streak > 1 {
				d += tr(l, "; noisy %d weeks running", x.Streak)
			}
			out = append(out, action{Text: tr(l, "Look at %s", x.Name), Detail: d, Href: four(x.Name),
				Kind: "tune", Rule: x.Name, Title: tr(l, "Tune %s", x.Name)})
		}
	}
	sort.SliceStable(missed, func(a, b int) bool { return missed[a].Start.After(missed[b].Start) })
	for i, in := range missed {
		if i == missedShownInActions {
			out = append(out, action{Text: tr(l, "%d more incidents no alert caught", len(missed)-i), Detail: tr(l, "in the last four weeks"), Href: "/incidents"})
			break
		}
		in = shown(l, in)
		name := in.ID
		if in.Title != "" {
			name += " " + in.Title
		}
		out = append(out, action{Text: tr(l, "No alert caught %s", name), Detail: tr(l, "%s: a rule for it, or a lower threshold?", in.Start.In(s.cfg.Loc).Format("2006-01-02 15:04")),
			Href: "/incidents/" + url.PathEscape(in.ID), Kind: "write", Incident: in.ID, Title: tr(l, "A rule for %s", in.ID)})
	}
	if undecided > 0 {
		out = append(out, action{Text: tr(l, "Decide on %d impact windows", undecided), Detail: tr(l, "confirm the real ones, dismiss the rest: the verdict sharpens either way"), Href: "/incidents"})
	}
	for _, x := range lines {
		if x.Spare {
			out = append(out, action{Text: tr(l, "Does %s still earn its place?", x.Name),
				Detail: tr(l, "caught %d incidents in four weeks; another rule caught each as soon, at the same severity or above", x.Incidents),
				Href:   four(x.Name), Kind: "retire", Rule: x.Name, Title: tr(l, "Retire %s?", x.Name)})
		}
	}
	if len(silent) > 0 {
		out = append(out, action{Text: tr(l, "%d rules never fired in twelve weeks", len(silent)), Detail: tr(l, "still needed, or do they watch something gone?"), Href: "/rules?weeks=12"})
	}
	if len(out) > actionsShown {
		out = out[:actionsShown]
	}
	return out
}

// weekCheck is the replay beside what really fired over a week, as the
// dashboard says it.
type weekCheck struct {
	Share, Either int
	Differ        []string
}

// weekCheckOf is a week's check of the rules in, all told, with those that
// differ; nil when none was checked.
func weekCheckOf(all ledger.Check, cs map[string]ledger.Check, d Differs, in func(string) bool) *weekCheck {
	if all.Either() == 0 {
		return nil
	}
	return &weekCheck{Share: int(math.Round(all.Share() * 100)), Either: all.Either(), Differ: ledger.Differing(cs, in, d.N, d.Ratio)}
}
