package serve

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
)

// updateGoalsWeekly writes the golden files of TestGoalsWeeklyUnchanged
// afresh: run once, on the release before goals over a period were built
// (v0.55.0), so that what goals of each week give can be held to it, byte
// for byte (docs/design/goals.md, step 3).
var updateGoalsWeekly = flag.Bool("update-goals-weekly", false, "write testdata/goals-weekly afresh")

// Goals of each week, as every lapilli serve before goals over a period
// worked them out: the summary, the Goals page, the overview's card and
// lines, /metrics, the digest, a report's section, MCP, and what the
// leader tells, makes and says after its rounds, the same byte for byte
// as the release before (testdata/goals-weekly).
func TestGoalsWeeklyUnchanged(t *testing.T) {
	ctx := context.Background()
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example/x\n"), 0o600)
	s := goalServer(t, func(c *Config) { c.NotifyWebhook, c.GoalTasks = hook, true })
	declare(t, s, theGoals)
	got := map[string]string{}
	h := s.Handler()
	for name, path := range map[string]string{
		"summary.json":          "/api/v1/summary?from=2026-10-05&to=2026-11-02&team=*",
		"summary-everyone.json": "/api/v1/summary?from=2026-10-05&to=2026-11-02",
		"summary-search.json":   "/api/v1/summary?from=2026-10-26&to=2026-11-02&team=search",
	} {
		code, body, _ := get(t, h, path)
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, code, body)
		}
		got[name] = body
	}
	page := func(path, lang string) string {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if lang != "" {
			r.Header.Set("Accept-Language", lang)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	// The Goals page, from its heading to the end of what it holds.
	main := func(body string) string {
		i, j := strings.Index(body, "<h1>"), strings.Index(body, "</main>")
		if i < 0 || j < i {
			t.Fatalf("no page: %s", body)
		}
		return body[i:j]
	}
	got["goals.html"] = main(page("/goals", ""))
	got["goals-payments.html"] = main(page("/goals?team=payments", ""))
	got["goals-search-ko.html"] = main(page("/goals?team=search", "ko"))
	// The overview's Goals card and the lines of goals in What to look
	// at, as the page shows them and as they are worked out.
	for name, path := range map[string]string{"overview.html": "/", "overview-payments.html": "/?team=payments"} {
		body := page(path, "")
		var parts []string
		if i := strings.Index(body, "<section class=\"card\">\n  <h2>Goals</h2>"); i >= 0 {
			j := strings.Index(body[i:], "</section>")
			parts = append(parts, body[i:i+j])
		}
		for _, li := range strings.Split(body, "<li>") {
			if strings.Contains(li, "#goal-") || strings.Contains(li, "/goals/task") {
				if k := strings.Index(li, "</li>"); k >= 0 {
					parts = append(parts, li[:k])
				}
			}
		}
		got[name] = strings.Join(parts, "\n----\n")
	}
	for name, team := range map[string]string{"overview-everyone.json": "", "overview-search.json": "search"} {
		var tm *ledger.Team
		if team != "" {
			x := teamOf(t, s, team)
			tm = &x
		}
		card, as, err := s.goalsOverview(ctx, "en", tm)
		if err != nil {
			t.Fatal(err)
		}
		// What the page and MCP show of them.
		type shownAction struct {
			Text, Detail, Href, Kind, Goal, GoalTeam, Week string
			GoalTeamID, Task                               int64
		}
		var sa []shownAction
		for _, a := range as {
			sa = append(sa, shownAction{a.Text, a.Detail, a.Href, a.Kind, a.Goal, a.GoalTeam, a.Week, a.GoalTeamID, a.Task})
		}
		var sc any
		if card != nil {
			sc = struct {
				Week             time.Time
				Met, Of, Unknown int
				Missed           []string
			}{card.Week, card.Met, card.Of, card.Unknown, card.Missed}
		}
		b, _ := json.MarshalIndent(struct {
			Card    any
			Actions any
		}{sc, sa}, "", " ")
		got[name] = string(b)
	}
	// The digest's line and digest.ready's goals; a report's section.
	from := dayAt("2026-10-26", 0)
	for name, team := range map[string]string{"digest-everyone.txt": "", "digest-search.txt": "search", "digest-payments.txt": "payments"} {
		var tm *ledger.Team
		if team != "" {
			x := teamOf(t, s, team)
			tm = &x
		}
		line, goals, err := s.goalsLine(ctx, from, tm)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(goals)
		got[name] = line + "\n" + string(b)
	}
	for name, span := range map[string][2]time.Time{
		"report-section.txt":      {dayAt("2026-10-14", 0), dayAt("2026-11-03", 0)},
		"report-section-none.txt": {dayAt("2026-10-28", 0), dayAt("2026-11-01", 0)},
	} {
		line, nums, err := s.goalsSection(ctx, span[0], span[1], nil, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(nums)
		got[name] = line + "\n" + string(b)
	}
	pay := teamOf(t, s, "payments")
	if line, nums, err := s.goalsSection(ctx, dayAt("2026-10-12", 0), dayAt("2026-11-02", 0), &pay, time.UTC); err != nil {
		t.Fatal(err)
	} else {
		b, _ := json.Marshal(nums)
		got["report-section-payments.txt"] = line + "\n" + string(b)
	}
	// MCP's summary and overview.
	ts := httptest.NewServer(h)
	defer ts.Close()
	cs := connect(t, ts.URL, "viewer@example.com")
	for name, c := range map[string]struct {
		tool string
		args map[string]any
	}{
		"mcp-summary.json":  {"summary", map[string]any{"from": "2026-10-19", "to": "2026-11-02", "team": "*"}},
		"mcp-overview.json": {"overview", nil},
	} {
		var out map[string]json.RawMessage
		if failed, text := call(t, cs, c.tool, c.args, &out); failed {
			t.Fatalf("%s: %s", c.tool, text)
		}
		if c.tool == "overview" {
			got[name] = string(out["goals"])
			continue
		}
		b, _ := json.Marshal(out)
		got[name] = string(b)
	}

	// The leader's rounds: what it works out, /metrics, what it tells,
	// the tasks it makes and what it says on them, as TestGoalsAfterRun
	// takes them.
	was := goalTasksARun
	goalTasksARun = 1
	defer func() { goalTasksARun = was }()
	var leader []string
	round := func(what string) {
		t.Helper()
		if err := s.goalsAfter(ctx); err != nil {
			t.Fatal(err)
		}
		shown := *s.goalsShownNow()
		shown.At = time.Time{}
		b, _ := json.Marshal(shown)
		leader = append(leader, "# "+what, string(b))
	}
	round("the first round")
	_, metrics, _ := get(t, h, "/metrics")
	var lines []string
	for _, l := range strings.Split(metrics, "\n") {
		if strings.Contains(l, "lapilli_goal_") {
			lines = append(lines, l)
		}
	}
	got["metrics.prom"] = strings.Join(lines, "\n")
	round("the second round")
	s.now = func() time.Time { return now.Add(7 * 24 * time.Hour) }
	if _, err := s.l.SyncFile(ctx, append(slices.Clone(goalIncidents),
		incident.Incident{ID: "INC-D", Start: dayAt("2026-11-04", 5), End: dayAt("2026-11-04", 6), Labels: incident.Labels{"service": {"cart"}}}), s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if err := s.l.Record(ctx, ledger.Found{From: now, To: now.Add(7 * 24 * time.Hour), Rules: []ledger.Rule{{Name: "Paged", Severity: "critical"}},
		Run: ledger.Run{Kind: "hourly", Started: now, Finished: now}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	round("a week on")
	if _, err := s.l.SyncFile(ctx, []incident.Incident{goalIncidents[0], goalIncidents[1],
		{ID: "INC-D", Start: dayAt("2026-11-04", 5), End: dayAt("2026-11-04", 6), Labels: incident.Labels{"service": {"cart"}}}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	round("INC-C taken out")
	round("again")
	got["leader.txt"] = strings.Join(leader, "\n")
	due, err := s.l.Due(ctx, "webhook", now.Add(30*24*time.Hour), 500)
	if err != nil {
		t.Fatal(err)
	}
	var told []string
	for _, n := range due {
		if n.Event != "goal.missed" {
			continue
		}
		var body map[string]any
		json.Unmarshal(n.Body, &body)
		delete(body, "at")
		b, _ := json.Marshal(body)
		var x notice
		json.Unmarshal(n.Body, &x)
		told = append(told, string(b), s.line(x), s.pageOf(x))
	}
	got["told.txt"] = strings.Join(told, "\n")
	tasks, err := s.l.Tasks(ctx, ledger.TaskQuery{AnyGoal: true})
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(tasks, func(a, b ledger.Task) int { return int(a.ID - b.ID) })
	var made []string
	for _, x := range tasks {
		made = append(made, fmt.Sprintf("#%d %s %s team %d goal %s by %s\n%s", x.ID, x.Status, x.Kind, x.TeamID, x.Goal, x.CreatedBy, x.Title), x.Body)
		cs, err := s.l.Comments(ctx, "task", fmt.Sprint(x.ID))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			made = append(made, "  said: "+c.Body)
		}
		st, ok, err := s.l.GoalTask(ctx, x.ID)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(st)
		made = append(made, fmt.Sprintf("  kept: %v %s", ok, b))
	}
	told0, made0, err := s.l.GoalMarks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal([]map[string]int64{told0, made0})
	got["tasks.txt"] = strings.Join(append(made, "marks: "+string(b)), "\n")
	// Run history's line of the tasks that wait.
	_, runs, _ := get(t, h, "/runs")
	if i := strings.Index(runs, "tasks for goals missed wait"); i >= 0 {
		got["runs.txt"] = runs[max(0, i-40) : i+60]
	} else {
		got["runs.txt"] = "none waiting"
	}

	dir := filepath.Join("testdata", "goals-weekly")
	for name, body := range got {
		body = unnamedNormal(body)
		path := filepath.Join(dir, name)
		if *updateGoalsWeekly {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if body != string(want) {
			t.Errorf("%s differs from the release before goals over a period:\n%s", name, firstDiff(string(want), body))
		}
	}
	// Every file is held: none left over from a name since changed.
	if !*updateGoalsWeekly {
		es, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if _, ok := got[e.Name()]; !ok {
				t.Errorf("testdata/goals-weekly/%s is held by nothing", e.Name())
			}
		}
	}
}
