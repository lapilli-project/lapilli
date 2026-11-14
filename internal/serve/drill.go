package serve

import (
	"net/url"
	"strings"

	"lapilli.dev/internal/ledger"
)

// The overview's drill-down (docs/design/ui.md): each tile links to the
// list that makes its number, with the same team, over the same week.

// weekOf is the week a list is asked for, week=DATE: the Monday of the
// week the date falls in, as the ledger counts its days, when that week is
// one of the twelve kept and over; else 0, and the list is over its period.
// The overview's tiles are last week's, and each links to the list that
// makes its number, over that week.
func (s *Server) weekOf(q url.Values) int64 {
	v := q.Get("week")
	if v == "" || len(v) > len("2006-01-02") {
		return 0
	}
	d, err := dateDay(v)
	if err != nil {
		return 0
	}
	w, now := ledger.Week(d), ledger.Week(s.l.Day(s.now()))
	if w >= now || w < now-7*(weeks-1) {
		return 0
	}
	return w
}

// drillsOf are, by tile, the list that makes its number, and what it is.
var drillsOf = map[string][2]string{
	"Precision":         {"/rules", "The rules that week"},
	"Firings":           {"/rules", "The rules that week"},
	"Incidents missed":  {"/incidents?state=missed", "The incidents missed"},
	"Time to catch":     {"/incidents?state=caught", "The incidents caught"},
	"Noisy rules":       {"/rules?perf=noisy", "The noisy rules"},
	"Woken for nothing": {"/rules", "The rules that week"},
	"Refired":           {"/rules?sort=refired", "The rules, the most re-fired first"},
	"During deploys":    {"/rules?sort=deploys", "The rules, the most during deploys first"},
}

// drill links each tile to the list that makes its number, over the week
// of week (a date), the tiles' last week: the page context, the team, is
// put on as the overview links to any page.
func drill(ts []tile, week string) {
	for i := range ts {
		d, ok := drillsOf[ts[i].key]
		if !ok {
			continue
		}
		p, raw, _ := strings.Cut(d[0], "?")
		q, _ := url.ParseQuery(raw)
		q.Set("week", week)
		ts[i].Href, ts[i].More = p+"?"+q.Encode(), d[1]
	}
}
