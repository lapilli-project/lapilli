package serve

import (
	"context"
	"html"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
)

// TestIncidentFirstNotNoisy: an incident a noisy rule caught first, and a
// rule that isn't noisy a little after, is listed by the one that isn't,
// the rule its lead is of, as its state is; the noisy one that came first
// is said apart, and on the incident's page each rule that caught it says
// whether it was noisy that week. The list used to name the noisy rule
// beside the other's lead, as if the noisy rule had made the catch.
func TestIncidentFirstNotNoisy(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	week := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	start := week.Add(58 * time.Hour)
	pay := map[string]string{"service": "pay"}
	var fs []firing.Firing
	for k := range 12 { // noisy: twelve firings that catch nothing
		at := week.Add(time.Duration(k) * 3 * time.Hour)
		fs = append(fs, firing.Firing{Rule: "Chatty", Start: at, End: at.Add(4 * time.Minute), Labels: pay})
	}
	fs = append(fs,
		firing.Firing{Rule: "Chatty", Start: start.Add(2 * time.Minute), End: start.Add(6 * time.Minute), Labels: pay},
		firing.Firing{Rule: "Steady", Start: start.Add(5 * time.Minute), End: start.Add(20 * time.Minute), Labels: pay})
	if err := s.l.Record(ctx, ledger.Found{From: week.Add(-14 * 24 * time.Hour), To: now, Firings: fs, Run: ledger.Run{Kind: "backfill", Started: week, Finished: week}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "steady", Rules: "^Steady"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.Put(ctx, incident.Incident{ID: "I-1", Start: start, End: start.Add(time.Hour), Labels: incident.Labels{"service": {"pay"}}}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}

	iv := viewOf(t, s, "incidents", "/incidents?weeks=4", s.incidentsPage).(incidentsView)
	var line *incidentLine
	for i := range iv.Lines {
		if iv.Lines[i].ID == "I-1" {
			line = &iv.Lines[i]
		}
	}
	if line == nil {
		t.Fatalf("I-1 isn't listed: %+v", iv.Lines)
	}
	if len(line.CaughtBy) != 2 || line.CaughtBy[0].Rule != "Chatty" {
		t.Fatalf("I-1 should be caught by Chatty, then Steady: %+v", line.CaughtBy)
	}
	if line.Status != "caught" || !strings.HasPrefix(line.First, "Steady ") || !strings.HasPrefix(line.Earlier, "Chatty ") {
		t.Errorf("I-1: %s by %q, earlier %q; want caught by Steady, Chatty earlier", line.Status, line.First, line.Earlier)
	}
	if want := leadText("en", -5*time.Minute); line.Lead != want || iv.Lead != want {
		t.Errorf("I-1's lead %q, the median %q; want Steady's, %q", line.Lead, iv.Lead, want)
	}
	_, body, _ := get(t, s.Handler(), "/incidents?weeks=4")
	if !strings.Contains(html.UnescapeString(body), line.Earlier+" caught it first, noisy that week") {
		t.Error("the list doesn't say the noisy rule apart")
	}

	if len(line.Catches) != 2 || !line.Catches[0].Noisy || line.Catches[1].Noisy {
		t.Errorf("I-1's catches: %+v", line.Catches)
	}
	code, body, _ := get(t, s.Handler(), "/incidents/I-1")
	_, caught, _ := strings.Cut(html.UnescapeString(body), ">Caught by</h2>")
	if code != 200 || strings.Count(body, "(noisy that week)") != 1 || !strings.Contains(caught, "Chatty</bdi></a> +2m <span class=\"quiet\">(noisy that week)</span>") {
		t.Errorf("I-1's page (%d) doesn't mark Chatty, and only Chatty, noisy that week", code)
	}
	// A rule's page lists it by the same rule, a team picked or not (its
	// tallies kept to the team's rules know nothing of Chatty's noise),
	// and says the noisy one apart, on the noisy rule's own page too.
	for _, path := range []string{"/rules/Steady", "/rules/Steady?team=steady", "/rules/Chatty"} {
		_, body, _ := get(t, s.Handler(), path)
		_, caught, _ := strings.Cut(html.UnescapeString(body), ">First caught by</th>")
		if !strings.Contains(caught, "<td>Steady +5m<br><span class=\"quiet\">Chatty +2m caught it first, noisy that week</span></td>") {
			t.Errorf("%s doesn't list I-1 by Steady, Chatty apart", path)
		}
	}
}
