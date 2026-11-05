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

// keepOutcomes keeps eight weeks of five rules, four of them changed two
// weeks ago and one three days ago, with an incident a week, a suggestion
// acted on, and two people's edits. The ledger notes edits as they
// happen, so today is today.
func keepOutcomes(t *testing.T, s *Server, today time.Time) {
	t.Helper()
	ctx := context.Background()
	o := s.cfg.Options
	const day = 24 * time.Hour
	start, changed, recent := today.Add(-56*day), today.Add(-14*day), today.Add(-3*day)

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
	// Flappy 10 throughout, whatever its change said; Late 2.
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
		verdict.RuleChange{Rule: "Same", Before: rr(3), After: rr(3)}) // fires alike: no prediction
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
}

// TestOutcomes checks what the outcomes make of keepOutcomes' ledger
// against sums done by hand.
func TestOutcomes(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.UserHeader = "X-Forwarded-Email" })
	today := time.Now().UTC().Truncate(24 * time.Hour)
	s.now = func() time.Time { return today.Add(12 * time.Hour) }
	const day = 24 * time.Hour
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
	if x := by["New"]; x.State != "added" || x.Since.Caught != 2 || x.Fewer != 0 {
		t.Errorf("New: %+v", x)
	}
	if x := by["Flappy"]; x.State != "otherwise" || x.Fewer != 0 {
		t.Errorf("Flappy: %+v", x)
	}
	if x := by["Late"]; x.State != "measuring" {
		t.Errorf("Late: %+v", x)
	}
	if _, ok := by["Same"]; ok || len(out.Effect.Changes) != 5 {
		t.Errorf("a rule that fires alike has nothing to measure: %d changes", len(out.Effect.Changes))
	}
	if e := out.Effect; e.Fewer != 294 || e.Measured != 2 || e.AsPredicted != 1 || e.Measuring != 1 || e.Added != 1 || e.AddedCaught != 2 {
		t.Errorf("effect: %+v", e)
	}
	// Then is the four weeks before lapilli serve began, 37 firings a day;
	// now the three since, 7 days of 37 and 14 of 17.
	if th, nw := out.Effect.Then, out.Effect.Now; th == nil || nw == nil || out.Effect.ThenFirst ||
		th.Firings != 28*37 || th.White != 4 || nw.Firings != 7*37+14*17 || nw.White != 5 || nw.Incidents != 3 || nw.Missed != 0 {
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
	if u := out.Use; u.Edits != 2 || u.People != 2 || u.ByKind["tasks"] != 1 || u.ByKind["comments"] != 1 ||
		u.TasksMade != 1 || u.TasksDone != 1 || u.Suggested != 2 || u.ActedOn != 1 {
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
	for _, want := range []string{"294 fewer firings", "1 of 2 changes measured came out as predicted", "as predicted", "not as predicted", "rule written", "rule taken away", "measuring"} {
		if !strings.Contains(body, want) {
			t.Errorf("/outcomes should say %q", want)
		}
	}
	_, body, _ = get(t, h, "/")
	if !strings.Contains(body, "294 fewer firings since the rule changes") || !strings.Contains(body, `href="/outcomes"`) {
		t.Errorf("the overview should sum the outcomes up:\n%s", body)
	}
	_, body, _ = get(t, h, "/metrics")
	for _, want := range []string{"lapilli_serve_firings_avoided 294\n", "lapilli_serve_nothing_avoided 294\n", `lapilli_serve_rule_changes{outcome="as_predicted"} 1`,
		`lapilli_serve_rule_changes{outcome="measuring"} 1`, fmt.Sprintf("lapilli_serve_incident_last_timestamp_seconds %d\n", today.Add(-7*day+10*time.Hour).Unix()),
		"lapilli_serve_edits 2\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics should have %q:\n%s", want, body)
		}
	}
	_, body, _ = get(t, h, "/api/v1/outcomes")
	var got outcomes
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Effect.Fewer != 294 || len(got.Effect.Changes) != 5 {
		t.Errorf("/api/v1/outcomes: %v\n%s", err, body)
	}
	line, err := s.outcomesLine(ctx)
	if err != nil || !strings.Contains(line, "294 fewer firings") || !strings.Contains(line, "1 of 2 changes") {
		t.Errorf("the digest's line: %q %v", line, err)
	}
}

// With no incident for two weeks, the record is said to have gone quiet,
// on the page, in the overview and in the digest.
func TestOutcomesQuiet(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Incidents = "" })
	o := s.cfg.Options
	for d := now.Add(-20 * 24 * time.Hour).Truncate(24 * time.Hour); d.Before(now); d = d.Add(24 * time.Hour) {
		fs := []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "search"}, Start: d.Add(time.Hour), End: d.Add(2 * time.Hour)}}
		if err := s.l.Record(ctx, ledger.Found{From: d, To: d.Add(24*time.Hour - time.Second), Firings: fs,
			Run: ledger.Run{Kind: "backfill", Started: d, Finished: d}}, o); err != nil {
			t.Fatal(err)
		}
	}
	out, err := s.outcomes(ctx)
	if err != nil || !out.Record.Quiet || !out.Record.LastIncident.IsZero() {
		t.Fatalf("no incident in twenty days should be quiet: %v %+v", err, out.Record)
	}
	h := s.Handler()
	if _, body, _ := get(t, h, "/outcomes"); !strings.Contains(body, "No incident in the ledger has started in") {
		t.Errorf("/outcomes should ask about the incidents:\n%s", body)
	}
	if _, body, _ := get(t, h, "/"); !strings.Contains(body, "are they still being written down?") {
		t.Errorf("the overview should ask about the incidents")
	}
	if line, err := s.outcomesLine(ctx); err != nil || !strings.Contains(line, "are they still being written down?") {
		t.Errorf("the digest's line: %q %v", line, err)
	}
}
