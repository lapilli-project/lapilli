package serve

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// TestChartMarks: the overview's charts mark the weeks with firings during
// deploys, and the weeks a quiet window of the team's, or everyone's, was
// over, as the ledger's own Covers says, besides the rules' changes; each
// can be turned off and on by a link, and the table of the weeks says the
// same.
func TestChartMarks(t *testing.T) {
	ctx := context.Background()
	s := drillServer(t, false)
	o := s.cfg.Options
	teams, err := s.l.Teams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := map[string]int64{}
	for _, tm := range teams {
		id[tm.Name] = tm.ID
	}
	at := func(m time.Month, d, h int) time.Time { return time.Date(2026, m, d, h, 0, 0, 0, time.UTC) }
	for _, w := range []ledger.Window{
		{Title: "everyone's", Start: at(10, 13, 2), End: at(10, 13, 3)},
		{Title: "everyone's, a Sunday night", Start: at(10, 4, 22), End: at(10, 4, 23)},
		{Title: "payments'", Start: at(10, 20, 2), End: at(10, 20, 3), TeamID: id["payments"]},
		{Title: "search's, weekly, three of them", Start: at(9, 7, 23), End: at(9, 8, 1), Every: "week", Until: at(9, 21, 0), TeamID: id["search"]},
		{Title: "before the twelve weeks", Start: at(8, 3, 2), End: at(8, 3, 3)},
	} {
		if _, err := s.l.PutWindow(ctx, w, "ana", o); err != nil {
			t.Fatal(err)
		}
	}
	// A team taken away, its window quieting nothing.
	gone, err := s.l.PutTeam(ctx, ledger.Team{Name: "gone", Rules: "^Gone"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutWindow(ctx, ledger.Window{Title: "gone's", Start: at(9, 30, 2), End: at(9, 30, 3), TeamID: gone}, "ana", o); err != nil {
		t.Fatal(err)
	}
	if err := s.l.DeleteTeam(ctx, gone, "ana"); err != nil {
		t.Fatal(err)
	}
	ws, err := s.l.Windows(ctx, at(1, 1, 0), now.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, team := range []string{"", "payments", "search"} {
		q := url.Values{}
		if team != "" {
			q.Set("team", team)
		}
		d := viewOf(t, s, "overview", "/?"+q.Encode(), s.overview).(dashboard)
		// What the ledger says: a week some time of which a window of the
		// team's, or everyone's, covers, or with a firing excused.
		for i, c := range d.Stones.Columns {
			from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 7*(i-len(d.Stones.Columns)+1))
			quiet := false
			for _, w := range ws {
				if w.TeamID < 0 || team != "" && w.TeamID != 0 && w.TeamID != id[team] {
					continue
				}
				for x := from; x.Before(from.Add(7 * 24 * time.Hour)); x = x.Add(5 * time.Minute) {
					quiet = quiet || w.Covers(x)
				}
			}
			if c.Quiet != quiet || d.Incidents.Columns[i].Quiet != quiet {
				t.Errorf("%s, week of %s: quiet %v, the ledger %v", cmpOr(team, "all"), from.Format("01-02"), c.Quiet, quiet)
			}
			deploys := from.Format("01-02") == "10-26" && team != "payments"
			if (c.Deploys > 0) != deploys {
				t.Errorf("%s, week of %s: %d during deploys", cmpOr(team, "all"), from.Format("01-02"), c.Deploys)
			}
		}
		n := 0
		for _, c := range d.Stones.Columns {
			if c.Quiet {
				n++
			}
		}
		if want := map[string]int{"": 6, "payments": 3, "search": 5}[team]; n != want {
			t.Errorf("%s: %d weeks quiet, not %d", cmpOr(team, "all"), n, want)
		}
	}

	// Turned off and on: the marks, their legend, and the table's tags.
	h := s.Handler()
	for _, c := range []struct {
		query            string
		deploys, windows bool
		links            []string
	}{
		{"", true, true, []string{`<a href="/?marks=windows">Hide deploys</a>`, `<a href="/?marks=deploys">Hide quiet windows</a>`}},
		{"marks=deploys,windows", true, true, []string{`<a href="/?marks=windows">Hide deploys</a>`}},
		{"marks=deploys", true, false, []string{`<a href="/?marks=none">Hide deploys</a>`, `<a href="/">Show quiet windows</a>`}},
		{"marks=windows", false, true, []string{`<a href="/">Show deploys</a>`, `<a href="/?marks=none">Hide quiet windows</a>`}},
		{"marks=none", false, false, []string{`<a href="/?marks=deploys">Show deploys</a>`, `<a href="/?marks=windows">Show quiet windows</a>`}},
		{"marks=bogus&team=search", false, false, []string{`<a href="/?marks=deploys&amp;team=search">Show deploys</a>`}},
		{"marks=deploys&marks=windows", true, true, nil},
	} {
		_, body, _ := get(t, h, "/?"+c.query)
		for what, want := range map[string]bool{
			`<path class="deploy"`: c.deploys, "firings during deploys</li>": c.deploys, `<span class="tag">deploys</span>`: c.deploys,
			`<rect class="quiet"`: c.windows, "a quiet window</li>": c.windows, `<span class="tag">a quiet window</span>`: c.windows,
		} {
			if strings.Contains(body, what) != want {
				t.Errorf("%s: %s %v, want %v", c.query, what, !want, want)
			}
		}
		for _, l := range c.links {
			if !strings.Contains(body, l) {
				t.Errorf("%s: no %s", c.query, l)
			}
		}
		// The rules' changes are marked, whatever is asked.
		if !strings.Contains(body, `<circle class="changed"`) == slices.ContainsFunc(s.mustVersions(t), func(v time.Time) bool { return v.After(time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)) }) {
			t.Errorf("%s: the rules' change isn't marked", c.query)
		}
	}
}

// mustVersions are when each version of the rules came into effect.
func (s *Server) mustVersions(t *testing.T) []time.Time {
	t.Helper()
	vs, err := s.l.Versions(context.Background(), time.Time{}, now, false)
	if err != nil {
		t.Fatal(err)
	}
	var out []time.Time
	for _, v := range vs {
		if !v.Since.IsZero() {
			out = append(out, v.Since)
		}
	}
	return out
}
