package serve

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

// nocBase is when the flapping alert starts: a Sunday evening, in the last
// whole week before now.
var nocBase = now.Add(-40 * time.Hour)

// nocHistory answers "flap > 0" with a search alert firing ten minutes in
// each half hour for four hours, re-firing seven times; "noise > 0" with a
// cart alert firing five minutes every two hours, from a day before, never
// re-firing; and "impact" with users hurt from 65 minutes after nocBase,
// for ten minutes, on search.
type nocHistory struct{}

func (nocHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	run := func(labels map[string]string, from, to time.Time) prom.Series {
		s := prom.Series{Labels: labels}
		for t := from; t.Before(to); t = t.Add(step) {
			if !t.Before(start) && !t.After(end) {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
			}
		}
		return s
	}
	var out []prom.Series
	switch expr {
	case "flap > 0":
		for k := 0; k < 8; k++ {
			at := nocBase.Add(time.Duration(k) * 30 * time.Minute)
			out = append(out, run(map[string]string{"service": "search"}, at, at.Add(10*time.Minute)))
		}
	case "noise > 0":
		for k := 0; k < 12; k++ {
			at := nocBase.Add(-20*time.Hour + time.Duration(k)*2*time.Hour)
			out = append(out, run(map[string]string{"service": "cart"}, at, at.Add(5*time.Minute)))
		}
	case "impact":
		out = append(out, run(map[string]string{"service": "search"}, nocBase.Add(65*time.Minute), nocBase.Add(75*time.Minute)))
	}
	var kept []prom.Series
	for _, s := range out {
		if len(s.Samples) > 0 {
			kept = append(kept, s)
		}
	}
	return kept, nil
}

const nocRules = `groups:
  - name: shop
    rules:
      - alert: Flap
        expr: flap > 0
        labels: {severity: critical}
      - alert: Noise
        expr: noise > 0
        labels: {severity: warning}
`

// nocServer is a server whose rules re-fire, one of whose firings came
// during a deploy of search, that caught an impact window: run once.
func nocServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	hist := filepath.Join(dir, "search.txt")
	// As recipes/versions/argocd.sh writes it: the deploy five minutes
	// before the third flap.
	os.WriteFile(hist, []byte("4f2a9c1e "+nocBase.Add(55*time.Minute).UTC().Format(time.RFC3339)+"\n"), 0o644)
	deploys := filepath.Join(dir, "deploys.yaml")
	os.WriteFile(deploys, []byte("deploys:\n  - labels: {service: search}\n    history: search.txt\n"), 0o644)
	s, rules := server(t, func(c *Config) {
		c.Deploys = deploys
		c.Options.DeployAfter = 10 * time.Minute
		c.Options.Hours = verdict.DefaultHours(time.UTC)
	})
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(nocRules), 0o644)
	s.h = nocHistory{}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "search", Rules: "^Flap$"}, "ana"); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if err := s.l.JudgePending(ctx, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	// Firings copied in from a source, and a notification an alert
	// manager sent: where things come from, on the dashboard.
	day := s.l.Midnight(s.l.Day(now) - 1)
	if _, err := s.l.PutFirings(ctx, "cloud-test", day, day.Add(24*time.Hour), []firing.Firing{{Rule: "DiskFull", Labels: map[string]string{"service": "cart"},
		Start: day.Add(4 * time.Hour), End: day.Add(5 * time.Hour)}}, "firings-token", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.Notify(ctx, ledger.Notification{At: now.Add(-time.Hour), Source: "alertmanager", Receiver: "oncall", Group: "g", Reason: "first",
		Alerts: []ledger.Alert{{Rule: "Flap", Labels: map[string]string{"service": "search"}, Start: now.Add(-time.Hour)}}}); err != nil {
		t.Fatal(err)
	}
	return s
}

// The deploys file: a history a recipe wrote, and a deploy written down;
// refused whole when it says something it shouldn't.
func TestLoadDeploys(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(text), 0o644)
		return p
	}
	write("checkout.txt", "4f2a9c1 2026-11-01T10:00:00Z\nb7e2d40 2026-11-02T11:30:00Z\n")
	ds, err := LoadDeploys(write("deploys.yaml", `deploys:
  - labels: {service: checkout, namespace: shop}
    history: checkout.txt
  - labels: {namespace: payments}
    start: 2026-11-02T09:00:00Z
    end: 2026-11-02T09:06:00Z
    revision: v1.2.3
`))
	if err != nil || len(ds) != 3 {
		t.Fatalf("%+v %v", ds, err)
	}
	if ds[0].Revision != "4f2a9c1" || ds[0].Labels["namespace"] != "shop" || ds[0].Source != "checkout.txt" || !ds[0].Start.Equal(ds[0].End) ||
		ds[2].Revision != "v1.2.3" || ds[2].End.Sub(ds[2].Start) != 6*time.Minute {
		t.Errorf("read: %+v", ds)
	}
	for name, text := range map[string]string{
		"no deploys key":       "incidents: []\n",
		"a history and start":  "deploys:\n  - {history: checkout.txt, start: 2026-11-02T09:00:00Z}\n",
		"neither":              "deploys:\n  - labels: {service: x}\n",
		"a history not there":  "deploys:\n  - {history: nowhere.txt}\n",
		"a line not a deploy":  "deploys:\n  - {history: bad.txt}\n",
		"a label not a string": "deploys:\n  - {labels: {service: [a, b]}, start: 2026-11-02T09:00:00Z}\n",
	} {
		write("bad.txt", "4f2a9c1 2026-11-01T10:00:00Z\nrm -rf / 2026-11-01T10:00:00Z\n")
		if _, err := LoadDeploys(write("d.yaml", text)); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	// Empty, it is no deploys: the file owns them.
	if ds, err := LoadDeploys(write("d.yaml", "deploys: []\n")); err != nil || len(ds) != 0 {
		t.Errorf("an empty list: %+v %v", ds, err)
	}
}

// Re-fires and firings during deploys, on the pages: a column and an order
// on the rules, a section on a rule's page, a tile and a column on the
// overview.
func TestNOCPages(t *testing.T) {
	s := nocServer(t)
	h := s.Handler()
	_, body, _ := get(t, h, "/rules?weeks=4")
	flap, noise := strings.Index(body, `href="/rules/Flap"`), strings.Index(body, `href="/rules/Noise"`)
	if flap < 0 || noise < 0 || noise > flap {
		t.Errorf("/rules: the noisiest first, Noise before Flap: %d %d", noise, flap)
	}
	for _, want := range []string{"During deploys", "88% of firings, 1 of 4 weeks", "precision 0% without them", `aria-current="page">caught nothing, the most first`} {
		if !strings.Contains(body, want) {
			t.Errorf("/rules: no %q", want)
		}
	}
	_, body, _ = get(t, h, "/rules?weeks=4&sort=refired")
	flap, noise = strings.Index(body, `href="/rules/Flap"`), strings.Index(body, `href="/rules/Noise"`)
	if flap < 0 || flap > noise || !strings.Contains(body, `aria-current="page">re-fired, the most first`) {
		t.Errorf("/rules?sort=refired: Flap first: %d %d", flap, noise)
	}
	_, body, _ = get(t, h, "/rules?sort=deploys")
	if strings.Index(body, `href="/rules/Flap"`) > strings.Index(body, `href="/rules/Noise"`) {
		t.Error("/rules?sort=deploys: Flap first")
	}
	_, body, _ = get(t, h, "/rules/Flap")
	for _, want := range []string{`id="refired"`, "Over twelve weeks, 7 of its 8 firings re-fired (88%), in 1 of the twelve weeks.",
		"Its alerts that re-fired most", "<code>service=search</code> <code>severity=critical</code> 7 times",
		"Of its firings over twelve weeks, 1 came during a deploy of what they are about, 1 of them catching something: precision 12% with them, 0% without."} {
		if !strings.Contains(body, want) {
			t.Errorf("/rules/Flap: no %q", want)
		}
	}
	_, body, _ = get(t, h, "/rules/Noise")
	if !strings.Contains(body, "None of its firings re-fired in twelve weeks.") || strings.Contains(body, "Its alerts that re-fired most") {
		t.Error("/rules/Noise: says it re-fired")
	}
	_, body, _ = get(t, h, "/")
	// The weekly table, wider with them, scrolls in a box the keyboard
	// reaches (axe's scrollable-region-focusable).
	for _, want := range []string{"1 of the last 11 whole weeks had one", "firings; precision 0% without them", "<th scope=\"col\" class=\"n\">During deploys</th>",
		`<div class="scroll" tabindex="0" role="region" aria-labelledby="by-week">`} {
		if !strings.Contains(body, want) {
			t.Errorf("/: no %q", want)
		}
	}
}

// The incidents, by team and by where they come from, with how soon the
// first alert came and what nearly caught those nothing did.
func TestNOCIncidents(t *testing.T) {
	s := nocServer(t)
	h := s.Handler()
	_, body, _ := get(t, h, "/incidents?source=impact")
	if !strings.Contains(body, "impact-") || strings.Contains(body, "INC-1") {
		t.Error("/incidents?source=impact: the impact window alone")
	}
	for _, want := range []string{"Of the 1 judged against: 1 caught, 0 caught only by noisy rules, 0 missed.",
		"The first alert of a rule that isn&#39;t noisy came, at the median, 5m before.", "<td>5m before</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("/incidents?source=impact: no %q", want)
		}
	}
	_, body, _ = get(t, h, "/incidents?source=written")
	if strings.Contains(body, "impact-") || !strings.Contains(body, "INC-1") || !strings.Contains(body, "1 missed") {
		t.Error("/incidents?source=written: the incident written down alone, missed")
	}
	// The search team owns the window its rule caught, not INC-1.
	_, body, _ = get(t, h, "/incidents?team=search")
	if !strings.Contains(body, "impact-") || strings.Contains(body, ">INC-1<") {
		t.Error("/incidents?team=search: the team's own")
	}
}

// /api/v1/summary: each week, for everyone and each team, in a shape that
// keeps its schema.
func TestSummaryAPI(t *testing.T) {
	s := nocServer(t)
	h := s.Handler()
	code, body, _ := get(t, h, "/api/v1/summary?from=2026-10-26&to=2026-11-02&team=*&critical=critical")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var sum summary
	if err := json.Unmarshal([]byte(body), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Schema != 1 || sum.Settings.Refire != "1h" || sum.Settings.DeployAfter != "10m" || !sum.Settings.Deploys || len(sum.Rows) != 4 {
		t.Fatalf("schema %d, settings %+v, %d rows", sum.Schema, sum.Settings, len(sum.Rows))
	}
	// The week of 2026-10-26: everyone's, then the search team's.
	all, team := sum.Rows[0], sum.Rows[1]
	if all.Team != "" || team.Team != "search" || all.Partial || !sum.Rows[2].Partial {
		t.Fatalf("rows: %+v", sum.Rows)
	}
	if all.Firings != 8+all.BySeverity["warning"].Firings || all.Refired.Firings != 7 || all.Refired.Rules != 1 ||
		all.Refired.Worst == nil || all.Refired.Worst.Rule != "Flap" || all.Refired.Worst.Refired != 7 {
		t.Errorf("everyone's re-fires: %+v", all.Refired)
	}
	if all.Critical.Firings != 8 || all.Critical.Caught != 1 || *all.Critical.Share != 0.125 {
		t.Errorf("critical: %+v", all.Critical)
	}
	if all.DuringDeploys.Firings != 1 || all.DuringDeploys.Caught != 1 || all.DuringDeploys.PrecisionWithout == nil || *all.DuringDeploys.PrecisionWithout != 0 {
		t.Errorf("during deploys: %+v", all.DuringDeploys)
	}
	if all.Impacts.Total != 1 || all.Impacts.Caught != 1 || all.Impacts.MedianLead == nil || *all.Impacts.MedianLead != 300 {
		t.Errorf("impacts: %+v", all.Impacts)
	}
	if team.Firings != 8 || team.BySeverity["warning"].Firings != 0 || team.Impacts.Total != 1 || team.Incidents.Total != 0 {
		t.Errorf("the search team's: %+v", team)
	}
	if all.OffHours == nil || all.Asleep == nil {
		t.Errorf("hours: %+v %+v", all.OffHours, all.Asleep)
	}
	// INC-1, written down and missed, is in the week of 2026-11-02.
	if w := sum.Rows[2]; w.Incidents.Total != 1 || w.Incidents.Missed != 1 || w.Incidents.MedianLead != nil {
		t.Errorf("the week of 2026-11-02: %+v", w.Incidents)
	}
	// The fields a dashboard reads, by name: the schema they keep.
	for _, field := range []string{`"schema"`, `"rows"`, `"critical"`, `"share_caught"`, `"by_severity"`, `"refired"`, `"worst"`, `"off_hours"`,
		`"asleep"`, `"during_deploys"`, `"precision_without"`, `"incidents"`, `"impacts"`, `"median_lead_seconds"`, `"noisy_only"`, `"missed"`, `"partial"`} {
		if !strings.Contains(body, field) {
			t.Errorf("no %s", field)
		}
	}
	// Refused: an unknown team, a span too long, to before from, a bad day.
	for _, q := range []string{"team=nobody", "from=2025-01-01&to=2026-11-02", "from=2026-11-02&to=2026-10-26", "from=yesterday"} {
		if code, _, _ := get(t, h, "/api/v1/summary?"+q); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}
	// Without from, to or team: everyone's, the four weeks before this one
	// and this one.
	_, body, _ = get(t, h, "/api/v1/summary")
	if err := json.Unmarshal([]byte(body), &sum); err != nil || len(sum.Rows) != 5 || sum.Rows[4].Team != "" {
		t.Errorf("by default: %d rows, %v", len(sum.Rows), err)
	}
	// And through MCP, the same.
	ts := httptest.NewServer(h)
	defer ts.Close()
	cs := connect(t, ts.URL, "viewer@example.com")
	var out summary
	if failed, text := call(t, cs, "summary", map[string]any{"from": "2026-10-26", "to": "2026-10-26", "team": "search"}, &out); failed ||
		len(out.Rows) != 1 || out.Rows[0].Team != "search" || out.Rows[0].Refired.Firings != 7 {
		t.Errorf("MCP summary: %s %+v", text, out.Rows)
	}
}

// /metrics: each rule's team as a label; firings during deploys; the weeks
// with a re-fire, everyone's and each team's; each incident by the teams
// that own it; when notifications and deploys last came.
func TestNOCMetrics(t *testing.T) {
	s := nocServer(t)
	_, body, _ := get(t, s.Handler(), "/metrics")
	for _, want := range []string{
		`lapilli_rule_refired{rule="Flap",severity="critical",team="search"} 7`,
		`lapilli_rule_firings{rule="Noise",severity="warning"} 12`,
		`lapilli_rule_firings_during_deploys{rule="Flap",severity="critical",team="search"} 1`,
		`lapilli_rule_caught_during_deploys{rule="Flap",severity="critical",team="search"} 1`,
		`lapilli_rule_firings_during_deploys{rule="Noise",severity="warning"} 0`,
		"lapilli_weeks_counted 1\n",
		"lapilli_refired_weeks 1\n",
		`lapilli_refired_weeks{team="search"} 1`,
		`lapilli_rule_refired_weeks{rule="Flap",severity="critical",team="search"} 1`,
		`lapilli_incident_state{incident="INC-1",source="written",state="missed"} 1`,
		`lapilli_incident_lead_seconds{incident="impact-`,
		`source="impact",team="search"} 300`,
		"lapilli_serve_deploys_last_timestamp_seconds " + strconv.FormatInt(nocBase.Add(55*time.Minute).Unix(), 10),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics: no %q", want)
		}
	}
	if !regexp.MustCompile(`lapilli_incident_state\{incident="impact-[0-9TZ]+",source="impact",team="search",state="caught"\} 1`).MatchString(body) {
		t.Error("/metrics: the impact window, caught, the search team's")
	}
	if strings.Contains(body, "lapilli_rule_refired_weeks{rule=\"Noise\"") {
		t.Error("/metrics: a rule that never re-fired has weeks with a re-fire")
	}
}

// The weekly digest, and a report's sections, say how many re-fired, the
// alert that did most, and the firings during deploys.
func TestNOCDigest(t *testing.T) {
	s := nocServer(t)
	ctx := context.Background()
	from := s.l.Midnight(ledger.Week(s.l.Day(now)) - 7)
	to := from.Add(7 * 24 * time.Hour)
	rep, err := s.digestReport(ctx, from, to, s.cfg.Options)
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.digestText(ctx, rep, nil, from, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Re-fired: 7 of ", "The most, Flap {service=search, severity=critical}: 7 times.", "Weeks with a re-fire, of the last 1 whole weeks: 1.",
		"During deploys: 1 firings, 1 of them catching something;"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest: no %q in\n%s", want, text)
		}
	}
	about := digestAbout(rep, text, "", nil)
	if err := s.aboutMore(ctx, about, rep, from, to, true); err != nil {
		t.Fatal(err)
	}
	if rf, ok := about["refired"].(*refires); !ok || rf.Refired != 7 || rf.Deploys != 1 || rf.Worst == nil {
		t.Errorf("digest.ready's refired: %+v", about["refired"])
	}
	b, err := s.preview(ctx, ledger.Scheduled{Name: "noc", Schedule: "weekly mon@09:00", Sections: []string{"refired", "deploys"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.Text, "Re-fired: 7 of") || !strings.Contains(b.Text, "During deploys: 1 firings") || b.Numbers["refired"] == nil {
		t.Errorf("a report's sections:\n%s", b.Text)
	}
}

// The chart's dashboard reads only what /metrics gives, by the names it
// gives them, and only the variables it defines; each panel and query has
// an id of its own. test/e2e/dashboard.sh asks a real Grafana each query.
func TestDashboardMetrics(t *testing.T) {
	b, err := os.ReadFile("../../charts/lapilli/dashboards/lapilli.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		UID        string `json:"uid"`
		Templating struct {
			List []struct{ Name string } `json:"list"`
		} `json:"templating"`
		Panels []struct {
			ID         int    `json:"id"`
			Type       string `json:"type"`
			Title      string `json:"title"`
			Datasource struct{ UID string }
			Targets    []struct {
				Expr  string `json:"expr"`
				RefID string `json:"refId"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	vars := map[string]bool{}
	for _, v := range d.Templating.List {
		vars[v.Name] = true
	}
	for _, v := range []string{"datasource", "instance", "team", "severity", "critical"} {
		if !vars[v] {
			t.Errorf("no variable %s", v)
		}
	}
	_, metrics, _ := get(t, nocServer(t).Handler(), "/metrics")
	ids := map[int]bool{}
	name := regexp.MustCompile(`\blapilli_[a-z_]+\b`)
	variable := regexp.MustCompile(`\$\{?(\w+)`)
	queries := 0
	for _, p := range d.Panels {
		if ids[p.ID] {
			t.Errorf("%s: id %d twice", p.Title, p.ID)
		}
		ids[p.ID] = true
		if p.Type == "row" {
			continue
		}
		if p.Datasource.UID != "${datasource}" || len(p.Targets) == 0 {
			t.Errorf("%s: data source %q, %d queries", p.Title, p.Datasource.UID, len(p.Targets))
		}
		refs := map[string]bool{}
		for _, q := range p.Targets {
			queries++
			if refs[q.RefID] {
				t.Errorf("%s: refId %s twice", p.Title, q.RefID)
			}
			refs[q.RefID] = true
			for _, m := range name.FindAllString(q.Expr, -1) {
				if !strings.Contains(metrics, "# TYPE "+m+" ") {
					t.Errorf("%s: %s isn't in /metrics", p.Title, m)
				}
			}
			for _, m := range variable.FindAllStringSubmatch(q.Expr, -1) {
				if !vars[m[1]] {
					t.Errorf("%s: $%s isn't a variable", p.Title, m[1])
				}
			}
		}
	}
	if d.UID != "lapilli" || queries < 20 {
		t.Errorf("uid %q, %d queries", d.UID, queries)
	}
}
