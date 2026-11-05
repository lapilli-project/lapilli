package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// keepOutcomes keeps eight weeks of six rules, five of them changed two
// weeks ago, one paused five days ago and one changed three days ago (its
// review kept twice), with an incident a week, a suggestion acted on, and
// two people's edits. The ledger notes edits as they happen, so today is
// today.
func keepOutcomes(t *testing.T, s *Server, today time.Time) {
	t.Helper()
	ctx := context.Background()
	o := s.cfg.Options
	const day = 24 * time.Hour
	start, changed, paused, recent := today.Add(-56*day), today.Add(-14*day), today.Add(-5*day), today.Add(-3*day)

	// An incident on checkout at ten, once a week: eight of them.
	var incs []incident.Incident
	for d := start; d.Before(today); d = d.Add(7 * day) {
		at := d.Add(10 * time.Hour)
		incs = append(incs, incident.Incident{ID: "INC-" + at.Format("0102"), Start: at, End: at.Add(45 * time.Minute),
			Labels: incident.Labels{"service": {"checkout"}}})
	}
	if _, err := s.l.SyncFile(ctx, incs, o); err != nil {
		t.Fatal(err)
	}
	// Noisy fires 20 times a day, one of them on checkout at ten, then 4;
	// Old 5 until it is taken away; New on checkout at ten once written;
	// Flappy 10 throughout, whatever its change said; Late 2; Cpu 20, then
	// 10, then none once paused.
	for d := start; d.Before(today); d = d.Add(day) {
		var fs []firing.Firing
		fire := func(rule, service string, h int) {
			at := d.Add(time.Duration(h) * time.Hour)
			fs = append(fs, firing.Firing{Rule: rule, Labels: map[string]string{"service": service}, Start: at, End: at.Add(5 * time.Minute)})
		}
		noisy := 20
		if !d.Before(changed) {
			noisy = 4
			fire("New", "checkout", 10)
		} else {
			for h := range 5 {
				fire("Old", "search", 11+h)
			}
		}
		fire("Noisy", "checkout", 10)
		for h := 1; h < noisy; h++ {
			fire("Noisy", "search", h)
		}
		for h := range 10 {
			fire("Flappy", "cart", 13+h)
		}
		fire("Late", "cart", 1)
		fire("Late", "cart", 2)
		cpu := 20
		if !d.Before(paused) {
			cpu = 0
		} else if !d.Before(changed) {
			cpu = 10
		}
		for h := range cpu {
			fire("Cpu", "db", h)
		}
		if err := s.l.Record(ctx, ledger.Found{From: d, To: d.Add(day - time.Second), Firings: fs,
			Run: ledger.Run{Kind: "backfill", Started: d, Finished: d}}, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.l.SetMeta(ctx, "began", fmt.Sprint(today.Add(-21*day).Unix())); err != nil {
		t.Fatal(err)
	}
	rr := func(n int) *verdict.RuleResult { return &verdict.RuleResult{Firings: n} }
	review := func(at time.Time, rcs ...verdict.RuleChange) {
		if err := s.l.AddResult(ctx, "review", at, at.Add(-7*day), at, verdict.Comparison{Rules: rcs}); err != nil {
			t.Fatal(err)
		}
	}
	review(changed,
		verdict.RuleChange{Rule: "Noisy", Before: rr(140), After: rr(28)},
		verdict.RuleChange{Rule: "Old", Before: rr(35)},
		verdict.RuleChange{Rule: "New", After: rr(7)},
		verdict.RuleChange{Rule: "Flappy", Before: rr(70), After: rr(7)},
		verdict.RuleChange{Rule: "Cpu", Before: rr(140), After: rr(70)},
		verdict.RuleChange{Rule: "Same", Before: rr(3), After: rr(3)}) // fires alike: no prediction
	review(paused, verdict.RuleChange{Rule: "Cpu", Before: rr(70), After: &verdict.RuleResult{Unsupported: "paused"}})
	review(recent, verdict.RuleChange{Rule: "Late", Before: rr(14), After: rr(0)})
	review(recent, verdict.RuleChange{Rule: "Late", Before: rr(14), After: rr(0)})
	// Someone acts on a suggestion; someone else comments.
	if err := s.l.AddResult(ctx, "suggestions", today.Add(-6*day), today.Add(-13*day), today.Add(-6*day),
		[]verdict.Suggestion{{Rule: "Noisy"}, {Rule: "Flappy"}}); err != nil {
		t.Fatal(err)
	}
	id, err := s.l.PutTask(ctx, ledger.Task{Title: "quiet Noisy", Kind: "tune", Rule: "Noisy", Status: "done"}, "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutComment(ctx, ledger.Comment{On: "task", Key: fmt.Sprint(id), Body: "done"}, "bo@example.com"); err != nil {
		t.Fatal(err)
	}
	// Due today: open, not yet overdue.
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "look at Cpu", Rule: "Cpu", Due: today}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
}

// TestOutcomes checks what the outcomes make of keepOutcomes' ledger
// against sums done by hand.
func TestOutcomes(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.UserHeader = "X-Forwarded-Email" })
	today := time.Now().UTC().Truncate(24 * time.Hour)
	s.now = func() time.Time { return today.Add(12 * time.Hour) }
	const day = 24 * time.Hour
	paused := today.Add(-5 * day)
	keepOutcomes(t, s, today)
	out, err := s.outcomes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ruleOutcome{}
	for _, c := range out.Effect.Changes {
		by[c.Rule] = c
	}
	// Noisy: 560 in the four weeks before, 56 in the two since; 280 at the
	// rate before, so 224 fewer, all of them ●.
	if x := by["Noisy"]; x.State != "as-predicted" || x.Before.Firings != 560 || x.After.Firings != 56 ||
		x.Before.Caught != 4 || x.After.Caught != 2 || x.Fewer != 224 || x.FewerBlack != 224 {
		t.Errorf("Noisy: %+v", x)
	}
	if x := by["Old"]; x.State != "removed" || x.Fewer != 70 {
		t.Errorf("Old: %+v", x)
	}
	// New: written, 14 firings since, as many more; 2 of them caught.
	if x := by["New"]; x.State != "added" || x.Since.Caught != 2 || x.Fewer != -14 || x.FewerBlack != -12 {
		t.Errorf("New: %+v", x)
	}
	if x := by["Flappy"]; x.State != "otherwise" || x.Fewer != 0 {
		t.Errorf("Flappy: %+v", x)
	}
	if x := by["Late"]; x.State != "measuring" {
		t.Errorf("Late: %+v", x)
	}
	// Cpu: measured until it was paused, nine days at 70 a week, as
	// predicted: 180 at the rate before, so 90 fewer; the pause itself has
	// nothing to measure.
	var cpu []ruleOutcome
	for _, c := range out.Effect.Changes {
		if c.Rule == "Cpu" {
			cpu = append(cpu, c)
		}
	}
	if len(cpu) != 2 || cpu[0].State != "unreplayed" || cpu[0].Fewer != 0 ||
		cpu[1].State != "as-predicted" || !cpu[1].Until.Equal(paused) || cpu[1].After.Firings != 90 || cpu[1].Fewer != 90 {
		t.Errorf("Cpu: %+v", cpu)
	}
	if _, ok := by["Same"]; ok || len(out.Effect.Changes) != 7 {
		t.Errorf("a rule that fires alike has nothing to measure, and a review kept twice counts once: %d changes", len(out.Effect.Changes))
	}
	if e := out.Effect; e.Fewer != 370 || e.FewerBlack != 372 || e.Measured != 3 || e.AsPredicted != 2 || e.Measuring != 1 ||
		e.Added != 1 || e.AddedCaught != 2 {
		t.Errorf("effect: %+v", e)
	}
	// Then is the four weeks before lapilli serve began, 57 firings a day;
	// now the three since: 7 days of 57, 9 of 27 and 5 of 17.
	if th, nw := out.Effect.Then, out.Effect.Now; th == nil || nw == nil || out.Effect.ThenFirst ||
		th.Firings != 28*57 || th.White != 4 || nw.Firings != 7*57+9*27+5*17 || nw.White != 5 || nw.Incidents != 3 || nw.Missed != 0 {
		t.Errorf("then and now: %+v %+v", th, nw)
	}
	rec := out.Record
	files := 0
	for _, w := range rec.Weeks {
		files += w.File
	}
	if files != 8 || rec.Quiet || !rec.LastIncident.Equal(today.Add(-7*day+10*time.Hour)) || len(rec.Weeks) != weeks {
		t.Errorf("record: %d incidents from the file, %+v", files, rec)
	}
	if u := out.Use; u.Edits != 3 || u.People != 2 || u.ByKind["tasks"] != 2 || u.ByKind["comments"] != 1 ||
		u.TasksMade != 2 || u.TasksDone != 1 || u.Open != 1 || u.Overdue != 0 || u.Suggested != 2 || u.ActedOn != 1 {
		t.Errorf("use: %+v", u)
	}

	h := s.Handler()
	for _, l := range []string{"en", "ko"} {
		r := httptest.NewRequest(http.MethodGet, "/outcomes", nil)
		r.Header.Set("Accept-Language", l)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, "Noisy") || !strings.Contains(body, ">224<") {
			t.Errorf("/outcomes in %s: %d\n%s", l, w.Code, body)
		}
	}
	_, body, _ := get(t, h, "/outcomes")
	for _, want := range []string{"370 fewer firings", "2 of 3 changes measured came out as predicted", "Of firings that caught nothing (●), 372 fewer.", "as predicted", "not as predicted", "rule written", "rule taken away", "no longer replayed", "measuring"} {
		if !strings.Contains(body, want) {
			t.Errorf("/outcomes should say %q", want)
		}
	}
	_, body, _ = get(t, h, "/")
	if !strings.Contains(body, "370 fewer firings since the rule changes") || !strings.Contains(body, `href="/outcomes"`) {
		t.Errorf("the overview should sum the outcomes up:\n%s", body)
	}
	_, body, _ = get(t, h, "/metrics")
	for _, want := range []string{"lapilli_serve_firings_avoided 370\n", "lapilli_serve_nothing_avoided 372\n", `lapilli_serve_rule_changes{outcome="as_predicted"} 2`, `lapilli_serve_rule_changes{outcome="otherwise"} 1`,
		`lapilli_serve_rule_changes{outcome="measuring"} 1`, fmt.Sprintf("lapilli_serve_incident_last_timestamp_seconds %d\n", today.Add(-7*day+10*time.Hour).Unix()),
		"lapilli_serve_edits 3\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics should have %q:\n%s", want, body)
		}
	}
	_, body, _ = get(t, h, "/api/v1/outcomes")
	var got outcomes
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Effect.Fewer != 370 || len(got.Effect.Changes) != 7 {
		t.Errorf("/api/v1/outcomes: %v\n%s", err, body)
	}
	line, err := s.outcomesLine(ctx)
	if err != nil || !strings.Contains(line, "370 fewer firings") || !strings.Contains(line, "2 of 3 changes") {
		t.Errorf("the digest's line: %q %v", line, err)
	}
	// Shown for a team, the card says it is everyone's.
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "db", Rules: "^Cpu$"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := get(t, h, "/?team=db"); !strings.Contains(body, "For all teams.") {
		t.Error("the outcomes card, for a team, should say they are for all teams")
	}
}

// With no incident written down for two weeks, the record is said to have
// gone quiet, on the page, in the overview and in the digest, whatever
// impact windows --impact finds.
func TestOutcomesQuiet(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Incidents = "" })
	o := s.cfg.Options
	for d := now.Add(-20 * 24 * time.Hour).Truncate(24 * time.Hour); d.Before(now); d = d.Add(24 * time.Hour) {
		fs := []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "search"}, Start: d.Add(time.Hour), End: d.Add(2 * time.Hour)}}
		f := ledger.Found{From: d, To: d.Add(24*time.Hour - time.Second), Firings: fs, Run: ledger.Run{Kind: "backfill", Started: d, Finished: d}}
		// --impact finds a window each day, and nobody decides on them.
		f.ImpactOn, f.ImpactSince = true, d
		f.Impact = []incident.Incident{{Start: d.Add(time.Hour), End: d.Add(90 * time.Minute), Labels: incident.Labels{"service": {"search"}}}}
		if err := s.l.Record(ctx, f, o); err != nil {
			t.Fatal(err)
		}
	}
	out, err := s.outcomes(ctx)
	if err != nil || !out.Record.Quiet || !out.Record.LastIncident.IsZero() || out.Record.Undecided == 0 {
		t.Fatalf("no incident written down in twenty days should be quiet, impact windows or not: %v %+v", err, out.Record)
	}
	h := s.Handler()
	if _, body, _ := get(t, h, "/outcomes"); !strings.Contains(body, "No incident written down has started in") {
		t.Errorf("/outcomes should ask about the incidents:\n%s", body)
	}
	if _, body, _ := get(t, h, "/"); !strings.Contains(body, "are they still being written down?") {
		t.Errorf("the overview should ask about the incidents")
	}
	if line, err := s.outcomesLine(ctx); err != nil || !strings.Contains(line, "are they still being written down?") {
		t.Errorf("the digest's line: %q %v", line, err)
	}
}
