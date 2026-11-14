package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
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
// re-firing, and once two weeks before; and "impact" with users hurt from 65 minutes after nocBase,
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
		// And once two weeks before: the ledger's first firing, in a week
		// it has part of.
		at := nocBase.Add(-14 * 24 * time.Hour)
		out = append(out, run(map[string]string{"service": "cart"}, at, at.Add(5*time.Minute)))
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
		c.Backfill = 20 * 24 * time.Hour
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
	ds, _, err := LoadDeploys(write("deploys.yaml", `deploys:
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
		if _, _, err := LoadDeploys(write("d.yaml", text)); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	// A history is a regular file beside the deploys file or below it, by
	// a relative path: never an absolute one, one climbing out, or a link
	// out; what is refused says the name given, not where it led.
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	write("sub/h.txt", "4f2a9c1 2026-11-01T10:00:00Z\n")
	if ds, _, err := LoadDeploys(write("d.yaml", "deploys:\n  - {labels: {service: a}, history: sub/h.txt}\n")); err != nil || len(ds) != 1 {
		t.Errorf("a history below: %+v %v", ds, err)
	}
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("4f2a9c1 2026-11-01T10:00:00Z\n"), 0o644)
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "link.txt"))
	os.Mkdir(filepath.Join(dir, "adir"), 0o755)
	for _, h := range []string{filepath.Join(outside, "secret"), "../" + filepath.Base(outside) + "/secret", "sub/../../x", "link.txt", "adir"} {
		_, _, err := LoadDeploys(write("d.yaml", "deploys:\n  - {labels: {service: a}, history: \""+h+"\"}\n"))
		if err == nil {
			t.Errorf("history %s taken", h)
		} else if strings.Contains(err.Error(), outside) && !strings.HasPrefix(h, "/") {
			t.Errorf("history %s: the error says where it led: %v", h, err)
		}
	}
	// Empty, it is no deploys: the file owns them.
	if ds, _, err := LoadDeploys(write("d.yaml", "deploys: []\n")); err != nil || len(ds) != 0 {
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
	// Each rule's link carries the period, as every link does.
	flap, noise := strings.Index(body, `href="/rules/Flap?weeks=4"`), strings.Index(body, `href="/rules/Noise?weeks=4"`)
	if flap < 0 || noise < 0 || noise > flap {
		t.Errorf("/rules: the noisiest first, Noise before Flap: %d %d", noise, flap)
	}
	for _, want := range []string{"During deploys", "88% of firings, 1 of 2 whole weeks", "precision 0% without them", `aria-current="page">caught nothing, the most first`} {
		if !strings.Contains(body, want) {
			t.Errorf("/rules: no %q", want)
		}
	}
	_, body, _ = get(t, h, "/rules?weeks=4&sort=refired")
	flap, noise = strings.Index(body, `href="/rules/Flap?weeks=4"`), strings.Index(body, `href="/rules/Noise?weeks=4"`)
	if flap < 0 || flap > noise || !strings.Contains(body, `aria-current="page">re-fired, the most first`) {
		t.Errorf("/rules?sort=refired: Flap first: %d %d", flap, noise)
	}
	_, body, _ = get(t, h, "/rules?sort=deploys")
	if strings.Index(body, `href="/rules/Flap"`) > strings.Index(body, `href="/rules/Noise"`) {
		t.Error("/rules?sort=deploys: Flap first")
	}
	_, body, _ = get(t, h, "/rules/Flap?tab=firings")
	for _, want := range []string{`id="refired"`, "7 of its 8 firings re-fired (88%). 1 of the last 2 whole weeks had one.",
		"Its alerts that re-fired most", "<code>service=search</code> <code>severity=critical</code> 7 times",
		"Of its firings, 1 came during a deploy of what they are about, 1 of them catching something: precision 12% with them, 0% without."} {
		if !strings.Contains(body, want) {
			t.Errorf("/rules/Flap: no %q", want)
		}
	}
	_, body, _ = get(t, h, "/rules/Noise?tab=firings")
	if !strings.Contains(body, "None of its firings re-fired.") || strings.Contains(body, "Its alerts that re-fired most") {
		t.Error("/rules/Noise: says it re-fired")
	}
	_, body, _ = get(t, h, "/")
	// The weekly table, wider with them, scrolls in a box the keyboard
	// reaches (axe's scrollable-region-focusable).
	for _, want := range []string{"1 of the last 2 whole weeks had one", "firings; precision 0% without them", "<th scope=\"col\" class=\"n\">During deploys</th>",
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
		"lapilli_weeks_counted 2\n",
		"lapilli_refired_weeks 1\n",
		`lapilli_refired_weeks{team="search"} 1`,
		`lapilli_rule_refired_weeks{rule="Flap",severity="critical",team="search"} 1`,
		`lapilli_incident_state{incident="INC-1",source="written",state="missed"} 1`,
		`lapilli_incident_lead_seconds{incident="impact-`,
		`lapilli_incident_team{incident="INC-1"} 1`,
		"lapilli_serve_deploys_last_timestamp_seconds " + strconv.FormatInt(nocBase.Add(55*time.Minute).Unix(), 10),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics: no %q", want)
		}
	}
	if !regexp.MustCompile(`lapilli_incident_state\{incident="impact-[0-9TZ]+",source="impact",state="caught"\} 1`).MatchString(body) ||
		!regexp.MustCompile(`lapilli_incident_team\{incident="impact-[0-9TZ]+",team="search"\} 1`).MatchString(body) ||
		!regexp.MustCompile(`lapilli_incident_lead_seconds\{incident="impact-[0-9TZ]+",source="impact"\} 300`).MatchString(body) {
		t.Error("/metrics: the impact window, caught 5m before, the search team's")
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
	for _, want := range []string{"Re-fired: 7 of ", "The most, Flap {service=search, severity=critical}: 7 times.", "Weeks with a re-fire, of the last 2 whole weeks: 1.",
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
	// Severities: critical, several at once, matching a rule whose versions'
	// differ; incidents joined to their teams, each counted once.
	text := string(b)
	for _, want := range []string{`severity=~\"(.+/)?($critical)(/.+)?\"`, `"name": "critical"`, `"type": "custom"`, `"multi": true`,
		`* on (instance, incident) group_left () max by (instance, incident) (lapilli_incident_team{`, `"noValue": "no run yet"`,
		`lapilli_serve_deploys_read_timestamp_seconds`, `max(lapilli_weeks_counted{`} {
		if !strings.Contains(text, want) {
			t.Errorf("the dashboard hasn't %s", want)
		}
	}
	for _, gone := range []string{`"type": "textbox"`, `lapilli_incident_state{instance=~\"$instance\",team=~`} {
		if strings.Contains(text, gone) {
			t.Errorf("the dashboard still has %s", gone)
		}
	}
}

// What a summary is asked for is bounded: each team once, no more than
// there are, * alone; 32 critical severities of 64 bytes at most. One of
// every team, or of more than 53 rows, takes the heavy turn, through the API
// and MCP alike; and a summary given up halfway stops at the next row.
func TestSummaryBounded(t *testing.T) {
	s := nocServer(t)
	ctx := context.Background()
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "cart", Rules: "^Noise$"}, "ana"); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	sevs := func(n int) string {
		var out []string
		for i := range n {
			out = append(out, fmt.Sprintf("s%d", i))
		}
		return strings.Join(out, ",")
	}
	for q, want := range map[string]int{
		"team=search&team=search":               200, // the same team twice: once
		"team=search&team=cart&team=search":     400, // more than there are
		"team=*&team=search":                    400,
		"critical=" + sevs(33):                  400,
		"critical=" + strings.Repeat("c", 65):   400,
		"critical=" + sevs(32):                  200,
		"critical=critical,critical,critical":   200,
		"team=search&team=cart&from=2026-10-26": 200,
	} {
		code, body, _ := get(t, h, "/api/v1/summary?"+q)
		if code != want {
			t.Errorf("%.60s: %d, want %d: %.200s", q, code, want, body)
		}
	}
	_, body, _ := get(t, h, "/api/v1/summary?team=search&team=search&from=2026-10-26&to=2026-10-26")
	var sum summary
	if err := json.Unmarshal([]byte(body), &sum); err != nil || len(sum.Rows) != 1 {
		t.Errorf("the same team twice: %d rows, %v", len(sum.Rows), err)
	}
	// The heavy turn taken by another request: every team's waits for it,
	// then is refused; a light one isn't held up.
	defer func(was time.Duration) { busyWait = was }(busyWait)
	busyWait = 50 * time.Millisecond
	s.heavy <- struct{}{}
	for q, want := range map[string]int{"team=*": 503, "from=2025-11-03&to=2026-11-02&team=search&team=cart": 503, "team=search": 200} {
		if code, _, _ := get(t, h, "/api/v1/summary?"+q); code != want {
			t.Errorf("with the heavy turn taken, %s: %d, want %d", q, code, want)
		}
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	cs := connect(t, ts.URL, "viewer@example.com")
	var out summary
	if failed, text := call(t, cs, "summary", map[string]any{"team": "*"}, &out); !failed || !strings.Contains(text, "busy") {
		t.Errorf("MCP summary of every team, with the heavy turn taken: %v %s", failed, text)
	}
	if failed, text := call(t, cs, "summary", map[string]any{"critical": sevs(40)}, &out); !failed || !strings.Contains(text, "32 severities") {
		t.Errorf("MCP summary, 40 critical severities: %v %s", failed, text)
	}
	<-s.heavy
	if code, _, _ := get(t, h, "/api/v1/summary?team=*"); code != 200 {
		t.Errorf("the turn given back: %d", code)
	}
	// Given up after the first row, the rest aren't worked out.
	cctx, cancel := context.WithCancel(ctx)
	defer func() { summaryRowSeen = nil }()
	rows := 0
	summaryRowSeen = func() { rows++; cancel() }
	w := ledger.Week(s.l.Day(now))
	own, _ := s.owners(ctx)
	who := []*ledger.Team{nil, &own.teams[0], &own.teams[1]}
	if _, err := s.summaryOf(cctx, w, w, who, []string{"critical"}); err == nil || rows != 1 {
		t.Errorf("given up: %d rows worked out, %v", rows, err)
	}
}

// shiftHistory answers "flaky > 0" with an api alert firing six times on a
// Sunday and six on the Monday after, three hours apart, the Monday 10:05
// one catching INC-API: noisy over the seven days to now, not in the
// incident's week.
type shiftHistory struct{}

func (shiftHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	var out []prom.Series
	add := func(at time.Time) {
		s := prom.Series{Labels: map[string]string{"service": "api"}}
		for t := at; t.Before(at.Add(10 * time.Minute)); t = t.Add(step) {
			if !t.Before(start) && !t.After(end) {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
			}
		}
		if len(s.Samples) > 0 {
			out = append(out, s)
		}
	}
	if expr == "flaky > 0" {
		sun, mon := time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC), time.Date(2026, 11, 2, 1, 5, 0, 0, time.UTC)
		for k := 0; k < 6; k++ {
			add(sun.Add(time.Duration(k) * 3 * time.Hour))
			add(mon.Add(time.Duration(k) * 3 * time.Hour))
		}
	}
	return out, nil
}

// An incident is in the same state on /incidents, in the summary and in
// /metrics: its catches judged noisy or not by the rule's stones in the
// incident's week, not over the seven days to now.
func TestIncidentStateEverywhere(t *testing.T) {
	ctx := context.Background()
	s, rules := server(t, nil)
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte("groups:\n  - name: shop\n    rules:\n      - alert: Flaky\n        expr: flaky > 0\n        labels: {severity: critical}\n"), 0o644)
	os.WriteFile(s.cfg.Incidents, []byte("incidents:\n  - {id: INC-API, title: API down, start: 2026-11-02T10:00:00Z, end: 2026-11-02T10:30:00Z, labels: {service: api}}\n"), 0o644)
	s.h = shiftHistory{}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()
	_, body, _ := get(t, h, "/incidents")
	if !strings.Contains(body, "Of the 1 judged against: 1 caught,") {
		t.Fatalf("/incidents: %s", regexp.MustCompile(`Of the \d+ judged against:[^<]*`).FindString(body))
	}
	_, body, _ = get(t, h, "/metrics")
	for _, want := range []string{`lapilli_incident_state{incident="INC-API",source="written",state="caught"} 1`,
		`lapilli_incidents{state="caught",source="written"} 1`, `lapilli_incident_lead_seconds{incident="INC-API",source="written"} -300`} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics: no %s", want)
		}
	}
	// Over the seven days to now the rule is noisy: 12 firings, one white.
	if !strings.Contains(body, `lapilli_rule_firings{rule="Flaky",severity="critical"} 12`) {
		t.Error("/metrics: Flaky fired 12 times")
	}
}

// Incidents each forty teams own, written by an editor or the incidents
// token: /metrics gives each a series, and which teams own them in no more
// than 500 series, not one series for each incident and team.
func TestIncidentSeriesBounded(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	var vals []string
	for i := 0; i < 40; i++ {
		v := fmt.Sprintf("s%d", i)
		vals = append(vals, v)
		if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "team" + v, Labels: map[string][]string{"service": {v}}}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 600; i++ {
		at := now.Add(-time.Duration(i) * 10 * time.Minute)
		in := incident.Incident{ID: fmt.Sprintf("X-%04d", i), Title: "x", Start: at, End: at.Add(5 * time.Minute), Labels: incident.Labels{"service": vals}}
		if _, err := s.l.Put(ctx, in, "eve", s.cfg.Options); err != nil {
			t.Fatal(err)
		}
	}
	_, body, _ := get(t, s.MetricsHandler(), "/metrics")
	states, teams := strings.Count(body, "\nlapilli_incident_state{"), strings.Count(body, "\nlapilli_incident_team{")
	if states != 500 || teams != 500 {
		t.Errorf("%d incident series and %d of their teams, want 500 each", states, teams)
	}
	// The latest kept.
	if !strings.Contains(body, `lapilli_incident_team{incident="X-0000",team="teams0"} 1`) {
		t.Error("the latest incident's teams left out")
	}
}

// Before the first run, /metrics gives no time for it, nor for how far the
// ledger has firings: a dashboard shows none, not the year 1.
func TestNoRunNoTime(t *testing.T) {
	s, _ := server(t, nil)
	_, body, _ := get(t, s.Handler(), "/metrics")
	for _, name := range []string{"lapilli_last_run_timestamp_seconds", "lapilli_serve_cursor_timestamp_seconds"} {
		if strings.Contains(body, "\n"+name+" ") {
			t.Errorf("before the first run: %s", regexp.MustCompile(name+` .*`).FindString(body))
		}
	}
}

// What this batch shows renders on the pages the language and the
// accessibility tests check (pagesServer, a11yServer): the re-fires, the
// firings during deploys, near misses on the incidents; and the tables
// that scroll sideways do so in a box the keyboard reaches.
func TestNOCRendered(t *testing.T) {
	s, paths := a11yServer(t)
	h := s.Handler()
	seen := map[string]bool{}
	for _, p := range paths {
		_, body, _ := get(t, h, p)
		for _, want := range []string{`id="refired"`, ">During deploys<", "came during a deploy", "nearly caught by", "re-fired, the most first",
			`<div class="scroll" tabindex="0" role="region" aria-labelledby="refired-h">`, `<div class="scroll" tabindex="0" role="region" aria-labelledby="by-week">`} {
			if strings.Contains(body, want) {
				seen[want] = true
			}
		}
	}
	for _, want := range []string{`id="refired"`, ">During deploys<", "came during a deploy", "nearly caught by", "re-fired, the most first",
		`<div class="scroll" tabindex="0" role="region" aria-labelledby="refired-h">`, `<div class="scroll" tabindex="0" role="region" aria-labelledby="by-week">`} {
		if !seen[want] {
			t.Errorf("%s isn't on any page the accessibility tests check", want)
		}
	}
}

// Without --deploys, a ledger that keeps deploys says so on the status,
// with how to take them away; given a file again, the status clears.
func TestDeploysUnread(t *testing.T) {
	s := nocServer(t)
	ctx := context.Background()
	file := s.cfg.Deploys
	s.cfg.Deploys = ""
	s.jobs(ctx, false)
	_, body, _ := get(t, s.Handler(), "/runs")
	if !strings.Contains(body, "--deploys isn&#39;t given") || !strings.Contains(body, "deploys: []") {
		t.Errorf("/runs without --deploys: %s", regexp.MustCompile(`(?s)class="(?:bad|status)[^>]*>.{0,300}`).FindString(body))
	}
	s.cfg.Deploys = file
	s.jobs(ctx, false)
	if _, body, _ = get(t, s.Handler(), "/runs"); strings.Contains(body, "--deploys isn&#39;t given") {
		t.Error("the status kept saying so with --deploys given")
	}
}

// A runbook that is a path in the rules' repository is a link on the rule's
// page under --runbook-base, and a path without it.
func TestRunbookBase(t *testing.T) {
	ctx := context.Background()
	for base, want := range map[string]string{
		"": "<code>docs/runbooks/errors.md#high</code>",
		"https://git.example.com/org/monitoring/blob/main/": `<a href="https://git.example.com/org/monitoring/blob/main/docs/runbooks/errors.md#high" rel="noopener noreferrer">docs/runbooks/errors.md#high</a>`,
	} {
		s, rules := server(t, func(c *Config) { c.CheckContent, c.RunbookBase = true, base })
		os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(ruleFile+"        annotations: {summary: errors, runbook_url: \"docs/runbooks/errors.md#high\"}\n"), 0o644)
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		s.jobs(ctx, true)
		if _, body, _ := get(t, s.Handler(), "/rules/Errors"); !strings.Contains(body, want) {
			t.Errorf("--runbook-base %q: no %s", base, want)
		}
	}
}

// The chart's ServiceMonitor keeps lapilli's labels (a rule's team stays
// team, not exported_team) and bounds a scrape.
func TestChartServiceMonitor(t *testing.T) {
	b, err := os.ReadFile("../../charts/lapilli/templates/service.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"honorLabels: true", "sampleLimit: {{ int .Values.serve.serviceMonitor.sampleLimit }}"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the ServiceMonitor hasn't %s", want)
		}
	}
}
