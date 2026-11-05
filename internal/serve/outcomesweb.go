package serve

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The outcomes page, its JSON, and its line in the digest.

type outcomeRow struct {
	ruleOutcome
	Predicted, Measured, Caught, FewerText string
	StateText, Class                       string
}

type kindCount struct {
	Name string
	N    int
}

type outcomesView struct {
	*outcomes
	Empty bool
	// ThenNow are then and now side by side, ThenText says which weeks
	// they are, or NotYet when they can't be set side by side yet.
	ThenNow          []tile
	ThenText, NotYet string
	Summary          []string
	Rows             []outcomeRow
	Sources, Edits   columnChart
	Kinds            []kindCount
	QuietDays        int
	DoneIn           string
	Last             int        // the index of this week
	Sourced          sourceWeek // the twelve weeks' incidents, by where they came from
	// Earlier are the changes left out of Rows; MoreUnreplayed, the rules
	// left out of Unreplayed.
	Earlier        int
	Unreplayed     []string
	MoreUnreplayed int
}

// How many changes, and rules not replayed, the page shows; the API has
// them all.
const (
	rowsShown  = 50
	namesShown = 5
)

func (s *Server) outcomesPage(r *http.Request) (any, error) {
	l := lang(r)
	out, err := s.outcomes(r.Context())
	if err != nil {
		return nil, err
	}
	v := outcomesView{outcomes: out, Empty: out.Record.Cursor.IsZero()}
	if v.Empty {
		return v, nil
	}
	loc, e, rec := s.cfg.Loc, out.Effect, out.Record
	day := func(t time.Time) string { return t.In(loc).Format("2006-01-02") }

	switch {
	case e.Then != nil && e.ThenFirst:
		v.ThenNow = thenNow(l, *e.Then, *e.Now)
		v.ThenText = tr(l, "The first weeks lapilli serve kept, from %s, beside the latest, to %s.", day(e.Then.From), day(e.Now.To))
	case e.Then != nil:
		v.ThenNow = thenNow(l, *e.Then, *e.Now)
		v.ThenText = tr(l, "The weeks before lapilli serve began on %s, beside the latest, to %s.", day(e.Then.To), day(e.Now.To))
	case e.ThenFirst:
		v.NotYet = tr(l, "With little kept from before lapilli serve began on %s, its first four weeks stand for then: they are set beside the latest once there is a week more.", day(out.Began))
	default:
		v.NotYet = tr(l, "The weeks before lapilli serve began on %s are set beside the latest once it has kept a week since.", day(out.Began))
	}
	if e.Fewer != 0 {
		if e.Fewer > 0 {
			v.Summary = append(v.Summary, tr(l, "%d fewer firings since the changes than at each rule's rate before it, %d of them ●.", e.Fewer, max(e.FewerBlack, 0)))
		} else {
			v.Summary = append(v.Summary, tr(l, "%d more firings since the changes than at each rule's rate before it.", -e.Fewer))
		}
	}
	if e.Measured > 0 {
		v.Summary = append(v.Summary, tr(l, "%d of %d changes measured came out as predicted.", e.AsPredicted, e.Measured))
	}
	switch {
	case e.Added == 1:
		v.Summary = append(v.Summary, tr(l, "1 rule written; it has caught %d incidents since.", e.AddedCaught))
	case e.Added > 1:
		v.Summary = append(v.Summary, tr(l, "%d rules written; they have caught %d incidents since.", e.Added, e.AddedCaught))
	}
	for i, x := range e.Changes {
		if i == rowsShown {
			v.Earlier = len(e.Changes) - i
			break
		}
		v.Rows = append(v.Rows, s.outcomeRow(l, x))
	}
	v.Unreplayed = rec.Unreplayed
	if n := len(v.Unreplayed); n > namesShown {
		v.Unreplayed, v.MoreUnreplayed = v.Unreplayed[:namesShown], n-namesShown
	}

	// The charts are laid out as the overview's, a week to a column.
	ws, at := make([]*weekView, len(rec.Weeks)), map[*weekView]int{}
	for i, w := range rec.Weeks {
		ws[i] = &weekView{Start: w.Week, Current: i == len(rec.Weeks)-1}
		at[ws[i]] = i
	}
	v.Sources = stacked(l, ws, loc, func(w *weekView) []int {
		x := rec.Weeks[at[w]]
		return []int{x.File, x.UI, x.Impact}
	}, []string{"src-file", "src-ui", "src-impact"}, []string{"from the incidents file", "written in Lapilli", "impact windows to decide"})
	v.Edits = stacked(l, ws, loc, func(w *weekView) []int { return []int{out.Use.Weeks[at[w]].Edits} },
		[]string{"edits"}, []string{"edits"})
	v.Last = len(rec.Weeks) - 1
	for _, w := range rec.Weeks {
		v.Sourced.File += w.File
		v.Sourced.UI += w.UI
		v.Sourced.Impact += w.Impact
	}
	for _, k := range append(editKinds, struct{ prefix, kind, label string }{"", "other", "other"}) {
		if n := out.Use.ByKind[k.kind]; n > 0 {
			v.Kinds = append(v.Kinds, kindCount{k.label, n})
		}
	}
	if rec.Quiet {
		if rec.LastIncident.IsZero() {
			v.QuietDays = int(out.At.Sub(rec.Since) / (24 * time.Hour))
		} else {
			v.QuietDays = int(out.At.Sub(rec.LastIncident) / (24 * time.Hour))
		}
	}
	if out.Use.TasksDone > 0 {
		v.DoneIn = inDays(l, out.Use.DoneIn)
	}
	return v, nil
}

// inDays is a time in days, or in hours under two days.
func inDays(l string, d time.Duration) string {
	if d < 48*time.Hour {
		return tr(l, "%d hours", int(math.Round(d.Hours())))
	}
	return tr(l, "%d days", int(math.Round(d.Hours()/24)))
}

func (s *Server) outcomeRow(l string, x ruleOutcome) outcomeRow {
	row := outcomeRow{ruleOutcome: x, Predicted: "–", Measured: "–", Caught: "–", FewerText: "–"}
	side := func(n int) string {
		if n < 0 {
			return "–"
		}
		return strconv.Itoa(n)
	}
	if x.PredBefore >= 0 || x.PredAfter >= 0 {
		row.Predicted = side(x.PredBefore) + " → " + side(x.PredAfter)
	}
	measured := x.AfterTo.After(x.At)
	before, after := "–", "–"
	if x.Kind != "added" && x.At.After(x.BeforeFrom) {
		before = fmt.Sprintf("%.0f", rate(x.Before.Firings, x.BeforeFrom, x.At))
	}
	if measured {
		after = fmt.Sprintf("%.0f", rate(x.After.Firings, x.At, x.AfterTo))
	}
	row.Measured = before + " → " + after
	cb, ca := "–", "–"
	if x.Kind != "added" {
		cb = strconv.Itoa(x.Before.Caught)
	}
	if measured {
		ca = strconv.Itoa(x.After.Caught)
	}
	row.Caught = cb + " → " + ca
	if x.Kind != "added" && x.At.Sub(x.BeforeFrom) >= leastFor {
		row.FewerText = strconv.Itoa(x.Fewer)
	}
	switch x.State {
	case "as-predicted":
		row.StateText, row.Class = tr(l, "as predicted"), "done"
	case "otherwise":
		row.StateText = tr(l, "not as predicted")
	case "measuring":
		row.StateText, row.Class = tr(l, "measuring, %s so far", inDays(l, x.AfterTo.Sub(x.At))), "doing"
	case "superseded":
		row.StateText = tr(l, "changed again within the week")
	case "no-baseline":
		row.StateText = tr(l, "under a week before it to measure against")
	case "added":
		row.StateText, row.Class = tr(l, "rule written"), "open"
	case "removed":
		row.StateText = tr(l, "rule taken away")
	}
	return row
}

// thenNow are the headline numbers, then and now.
func thenNow(l string, then, now period) []tile {
	pct := func(p period) (float64, bool) {
		if p.Firings == 0 {
			return 0, false
		}
		return float64(p.White) / float64(p.Firings), true
	}
	var out []tile
	x := tile{Label: tr(l, "Precision"), Value: "–"}
	if a, ok := pct(now); ok {
		x.Value = fmt.Sprintf("%.0f%%", 100*a)
		if b, ok := pct(then); ok {
			x.Of = tr(l, "then %s", fmt.Sprintf("%.0f%%", 100*b))
			x.Delta, x.Better, x.Worse = fmt.Sprintf("%+.0f pt", 100*(a-b)), a > b, a < b
		}
	}
	out = append(out, x)
	weekly := func(label string, n func(period) int, better int) {
		a, b := now.perWeek(n(now)), then.perWeek(n(then))
		x := tile{Label: tr(l, label), Value: fmt.Sprintf("%.0f", a), Of: tr(l, "then %s", fmt.Sprintf("%.0f", b))}
		if b > 0 && math.Round(a) != math.Round(b) {
			x.Delta = fmt.Sprintf("%+.0f%%", 100*(a-b)/b)
			x.Better, x.Worse = better != 0 && (a-b)*float64(better) > 0, better != 0 && (a-b)*float64(better) < 0
		}
		out = append(out, x)
	}
	weekly("Firings a week", func(p period) int { return p.Firings }, 0)
	weekly("● a week", func(p period) int { return p.Firings - p.White }, -1)
	x = tile{Label: tr(l, "Incidents missed"), Value: "–"}
	if now.Incidents > 0 {
		x.Value = tr(l, "%d of %d", now.Missed, now.Incidents)
	}
	if then.Incidents > 0 {
		x.Of = tr(l, "then %s", tr(l, "%d of %d", then.Missed, then.Incidents))
		if now.Incidents > 0 {
			a, b := float64(now.Missed)/float64(now.Incidents), float64(then.Missed)/float64(then.Incidents)
			if math.Round(100*a) != math.Round(100*b) {
				x.Delta, x.Better, x.Worse = fmt.Sprintf("%+.0f pt", 100*(a-b)), a < b, a > b
			}
		}
	}
	out = append(out, x)
	return out
}

func (s *Server) apiOutcomes(w http.ResponseWriter, r *http.Request) {
	out, err := s.outcomes(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, out)
}

// outcomesLine says, for the digest, what the changes did and whether the
// record has gone quiet.
func (s *Server) outcomesLine(ctx context.Context) (string, error) {
	out, err := s.outcomes(ctx)
	if err != nil {
		return "", err
	}
	var parts []string
	e, l := out.Effect, s.cfg.Lang
	switch {
	case e.Fewer > 0:
		parts = append(parts, tr(l, "Since the rule changes: %d fewer firings than at each rule's rate before.", e.Fewer))
	case e.Fewer < 0:
		parts = append(parts, tr(l, "Since the rule changes: %d more firings than at each rule's rate before.", -e.Fewer))
	}
	if e.Measured > 0 {
		parts = append(parts, tr(l, "%d of %d changes measured came out as predicted.", e.AsPredicted, e.Measured))
	}
	if out.Record.Quiet {
		parts = append(parts, tr(l, "No incident in two weeks or more: are they still being written down?"))
	}
	return strings.Join(parts, " "), nil
}
