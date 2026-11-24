package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// htmlTags are a page's tags, left out to read its text.
var htmlTags = regexp.MustCompile(`<[^>]*>`)

// lateServer has INC-L written down: the search service from 14:30 to 15:01
// on Sunday, which Errors' search firing at 15:02 comes just after, its
// condition holding from 15:00, its for of 2m running past the end.
func lateServer(t *testing.T) *Server {
	t.Helper()
	s, _ := server(t, func(c *Config) {
		keepRecord(c)
		b, err := os.ReadFile(c.Incidents)
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, "  - {id: INC-L, title: Search slow, start: 2026-11-01T14:30:00Z, end: 2026-11-01T15:01:00Z, labels: {service: search}}\n"...)
		if err := os.WriteFile(c.Incidents, b, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	s.ready.Store(true)
	return s
}

// A firing whose condition held in an incident but whose alert came only
// once it was over is its alert, too late (docs/how-it-judges.md, "An alert
// after the incident"): lapilli serve says so as lapilli verdict does, of
// the same firings and incidents, how long before its start each rule's
// condition held taken from the versions in effect; in /api/v1/report, the
// incidents' page and its own, the rule's page, MCP, in each language.
func TestServeAfterEnd(t *testing.T) {
	s := lateServer(t)
	ctx := context.Background()
	from, to := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), now.Add(time.Hour)
	code, body, _ := get(t, s.Handler(), "/api/v1/report?from="+from.Format(time.RFC3339)+"&to="+to.Format(time.RFC3339))
	if code != http.StatusOK {
		t.Fatalf("report: %d %s", code, body)
	}
	var rep verdict.Report
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatal(err)
	}
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	o.From, o.To = from, to
	var fs []firing.Firing
	for _, j := range rep.Firings {
		f := j.Firing
		f.Held = time.Time{} // worked out again, from the rules
		fs = append(fs, f)
	}
	var incs []incident.Incident
	for _, in := range rep.Incidents {
		incs = append(incs, in.Incident)
	}
	vs, err := s.versions(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	var known []rules.Rule
	for _, v := range vs {
		known = append(known, v.Rules...)
	}
	want := verdict.Judge(known, fs, incs, o)
	say := func(r *verdict.Report) []string {
		var out []string
		for _, rr := range r.Rules {
			out = append(out, fmt.Sprintf("rule %s late %d", rr.Rule, rr.Late))
		}
		for _, in := range r.Incidents {
			for _, c := range in.FiredAfter {
				out = append(out, fmt.Sprintf("incident %s after %s %s", in.ID, c.Rule, c.Delay))
			}
		}
		for _, j := range r.Firings {
			out = append(out, fmt.Sprintf("firing %s %s %s after %v held %s", j.Rule, j.Labels["service"], j.Start.UTC().Format("01-02 15:04"), j.After, j.Held.UTC().Format("15:04")))
		}
		slices.Sort(out)
		return out
	}
	got, w := say(&rep), say(want)
	if !slices.Equal(got, w) {
		t.Fatalf("serve and verdict differ:\n serve %q\nverdict %q", got, w)
	}
	if !slices.Contains(got, "rule Errors late 1") || !slices.Contains(got, "incident INC-L after Errors 32m0s") ||
		!slices.Contains(got, "firing Errors search 11-01 15:02 after [INC-L] held 15:00") {
		t.Errorf("not said of INC-L: %q", got)
	}

	h := s.Handler()
	for _, c := range []struct{ path, want string }{
		{"/incidents", "✗ nothing in time; after its end: Errors +1m"},
		{"/incidents/INC-L", "Nothing in time."},
		{"/incidents/INC-L", "+1m after its end"},
		{"/rules/Errors", "1 of the firings that caught nothing came after an incident they agree with was over"},
	} {
		if _, body, _ := get(t, h, c.path); !strings.Contains(html.UnescapeString(htmlTags.ReplaceAllString(body, "")), c.want) {
			t.Errorf("%s: no %q", c.path, c.want)
		}
	}
	_, inc, err := s.toolIncidents(ctx, nil, incidentsIn{})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(inc.Incidents, func(x incidentOut) bool { return x.ID == "INC-L" })
	if i < 0 || inc.Incidents[i].Status != "missed" || len(inc.Incidents[i].FiredAfter) != 1 || inc.Incidents[i].FiredAfter[0] != (catchOut{Rule: "Errors", Delay: 32 * 60}) {
		t.Errorf("MCP's incidents: %+v", inc.Incidents)
	}
	if _, r, err := s.toolRule(ctx, nil, ruleIn{Name: "Errors"}); err != nil || r.Rule.Late != 1 {
		t.Errorf("MCP's rule: %+v %v", r.Rule, err)
	}
	if _, rs, err := s.toolRules(ctx, nil, rulesIn{Weeks: 4}); err != nil || len(rs.Rules) != 1 || rs.Rules[0].Late != 1 {
		t.Errorf("MCP's rules: %+v %v", rs.Rules, err)
	}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	pageA11y(t, h, "", "/incidents", "/incidents/INC-L", "/rules/Errors")
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
}

// Errors with no for from 15:01:30, a minute and a half after INC-L ended,
// before its firing of 15:02 that came after it: one version held its
// condition only from the firing's start, so no version but every one says
// the firing came after INC-L, and neither the incident's page nor the
// list says it did, as lapilli verdict asks every version of a period.
func TestServeAfterEndEveryVersion(t *testing.T) {
	parallel(t)
	s := lateServer(t)
	ctx := context.Background()
	noFor := strings.Replace(ruleFile, "for: 2m", "for: 0s", 1)
	if _, err := s.l.AddVersion(ctx, ledger.Version{Since: time.Date(2026, 11, 1, 15, 1, 30, 0, time.UTC), Digest: "no for",
		Docs: []rules.Doc{{Source: "shop.yaml", Data: []byte(noFor)}}, Names: []string{"Errors"}}); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	for _, path := range []string{"/incidents", "/incidents/INC-L"} {
		_, body, _ := get(t, h, path)
		if text := html.UnescapeString(htmlTags.ReplaceAllString(body, "")); strings.Contains(text, "after its end") || strings.Contains(text, "Nothing in time") {
			t.Errorf("%s says Errors came after INC-L, its condition held from its start by one version", path)
		}
	}
	_, inc, err := s.toolIncidents(ctx, nil, incidentsIn{})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range inc.Incidents {
		if len(x.FiredAfter) > 0 {
			t.Errorf("MCP: %s fired after %+v", x.ID, x.FiredAfter)
		}
	}
}

// Past what one read looks at (ledger.LateSpans here), what came after an
// incident's end isn't all said, and serve says so, not "nothing fired"
// alone: late_cut in the report and MCP, a line on the pages, in each
// language.
func TestServeAfterEndCut(t *testing.T) {
	was := ledger.LateSpans
	ledger.LateSpans = 0
	t.Cleanup(func() { ledger.LateSpans = was })
	s := lateServer(t)
	ctx := context.Background()
	h := s.Handler()
	from, to := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), now.Add(time.Hour)
	_, body, _ := get(t, h, "/api/v1/report?from="+from.Format(time.RFC3339)+"&to="+to.Format(time.RFC3339))
	var rep struct {
		LateCut bool `json:"late_cut"`
	}
	if err := json.Unmarshal([]byte(body), &rep); err != nil || !rep.LateCut {
		t.Errorf("the report: late_cut %v %v", rep.LateCut, err)
	}
	const said = "More firings came near the ends of incidents than one read looks at"
	for _, path := range []string{"/incidents", "/incidents/INC-L", "/rules/Errors"} {
		if _, body, _ := get(t, h, path); !strings.Contains(html.UnescapeString(body), said) {
			t.Errorf("%s doesn't say it was cut", path)
		}
	}
	if _, inc, err := s.toolIncidents(ctx, nil, incidentsIn{}); err != nil || !inc.LateCut {
		t.Errorf("MCP's incidents: late_cut %v %v", inc.LateCut, err)
	}
	if _, r, err := s.toolRule(ctx, nil, ruleIn{Name: "Errors"}); err != nil || !r.Rule.LateCut {
		t.Errorf("MCP's rule: late_cut %v %v", r.Rule.LateCut, err)
	}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	pageA11y(t, h, "", "/incidents", "/incidents/INC-L", "/rules/Errors")
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
}
