package serve

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// keptIncident is an incident in the week before now's, the one the weekly
// digest judges, that no firing of the tests catches, as its service is
// none of theirs: with it, the weeks around it have a record, and their
// firings are judged, noisy or not (verdict.RecordKept). The test server's
// own incident, INC-1, started on the Monday after that week, so the week
// has none of its own.
var keptIncident = incident.Incident{ID: "INC-0", Title: "Kept the record", Start: time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC),
	End: time.Date(2026, 10, 28, 3, 30, 0, 0, time.UTC), Labels: incident.Labels{"service": {"billing"}}}

// keepRecord writes keptIncident into the incidents file a test server
// reads, before its first run.
func keepRecord(c *Config) {
	b, err := os.ReadFile(c.Incidents)
	if err != nil {
		panic(err)
	}
	b = append(b, "  - {id: INC-0, title: Kept the record, start: 2026-10-28T03:00:00Z, end: 2026-10-28T03:30:00Z, labels: {service: billing}}\n"...)
	if err := os.WriteFile(c.Incidents, b, 0o644); err != nil {
		panic(err)
	}
}

// keepRecordIn writes keptIncident down in a server's ledger, as the UI
// does.
func keepRecordIn(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.l.Put(context.Background(), keptIncident, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
}

// TestFacetsNoRecord: with no incident in the weeks shown or the two before
// them, no rule is noisy: the six that fire as often as a noisy rule are
// listed as having no record, with no precision, and the list says why
// once; so do the overview, first among what to look at, a rule's page,
// the summary and MCP.
func TestFacetsNoRecord(t *testing.T) {
	ctx := context.Background()
	s := facetServerOf(t, false)
	all := rulesFor(t, s, "weeks=4")
	perf := map[string]int{}
	for _, o := range all.Facets[0].Options {
		perf[o.Value] = o.Count
	}
	if want := map[string]int{"noisy": 0, "unrecorded": 6, "ok": 11, "silent": 8, "spare": 0}; !reflect.DeepEqual(perf, want) {
		t.Errorf("performance: %v, want %v", perf, want)
	}
	if !all.NoRecord {
		t.Error("the list should say there is no record")
	}
	un := rulesFor(t, s, "weeks=4&perf=unrecorded")
	if len(un.Rows) != 6 || slices.ContainsFunc(un.Rows, func(r ruleRow) bool { return r.Noisy || r.Precision != "–" || !r.NoRecord }) {
		t.Errorf("perf=unrecorded: %+v", un.Rows)
	}
	h := s.Handler()
	if code, _, hd := get(t, h, "/rules?q=is:unrecorded"); code != http.StatusSeeOther || hd.Get("Location") != "/rules?perf=unrecorded" {
		t.Errorf("is:unrecorded: %d %q", code, hd.Get("Location"))
	}
	_, body, _ := get(t, h, "/rules?weeks=4")
	for _, want := range []string{"No incident was written down in these weeks or the two before their end", `<span aria-hidden="true">?</span> No record`} {
		if !strings.Contains(html.UnescapeString(body), want) {
			t.Errorf("/rules: no %q", want)
		}
	}
	if strings.Count(body, "No incident was written down in these weeks") != 1 {
		t.Error("/rules: said more than once")
	}

	d, err := s.overview(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	v := d.(dashboard)
	if len(v.Actions) == 0 || v.Actions[0].Text != "No incident written down to judge the firings by" || v.Actions[0].Kind != "" {
		t.Errorf("what to look at: %+v", v.Actions)
	}
	if slices.ContainsFunc(v.Actions, func(a action) bool { return strings.HasPrefix(a.Text, "Look at ") }) {
		t.Errorf("no rule is noisy: %+v", v.Actions)
	}
	cur := v.Weeks[0]
	if !cur.NoRecord || cur.Precision() != "–" || cur.Noisy != 0 {
		t.Errorf("this week: %+v", cur)
	}

	_, body, _ = get(t, h, "/rules/R00")
	if !strings.Contains(html.UnescapeString(body), "not known: no incident written down to judge the firings by") {
		t.Error("/rules/R00: its precision isn't said to be unknown")
	}

	_, out, err := s.toolRules(ctx, nil, rulesIn{Weeks: 4})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Rules {
		if r.Firings > 0 && (!r.NoRecord || r.Precision != nil || r.Noisy) {
			t.Errorf("MCP rules: %+v", r)
		}
	}
	w := ledger.Week(s.l.Day(now))
	sum, err := s.summaryOf(ctx, w, w, []*ledger.Team{nil}, []string{"critical"})
	if err != nil {
		t.Fatal(err)
	}
	if r := sum.Rows[0]; !r.NoRecord || r.Precision != nil || r.Firings == 0 {
		t.Errorf("summary: %+v", r)
	}
}

// TestDigestNoRecord: the week before has firings and no incident written
// down in it or in the week before it: the digest counts them and doesn't
// judge them, says so once, and suggests no longer "for".
func TestDigestNoRecord(t *testing.T) {
	var got string
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	}))
	defer slack.Close()
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte(slack.URL+"/services/T/B/SECRET"), 0o600)
	s, _ := server(t, func(c *Config) { c.Slack = hook; c.Backfill = 10 * 24 * time.Hour })
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if !strings.Contains(got, "Lapilli  2026-10-26 00:00 → 2026-11-02 00:00") || strings.Count(got, "can't be told from one nobody wrote down") != 1 ||
		strings.Contains(got, "2m → 15m") {
		t.Errorf("the digest of the week before:\n%s", got)
	}
	rs, err := s.l.Results(ctx, "suggestions", 1)
	if err != nil || len(rs) != 1 {
		t.Fatalf("%v %d", err, len(rs))
	}
	var ss []verdict.Suggestion
	if err := rs[0].Decode(&ss); err != nil || len(ss) != 0 {
		t.Errorf("suggested %+v %v", ss, err)
	}
	from := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	rep, err := s.l.Report(ctx, from, from.Add(7*24*time.Hour), s.cfg.Options, false)
	if err != nil {
		t.Fatal(err)
	}
	line, nums, err := s.noiseSection(rep, from, from.Add(7*24*time.Hour), time.UTC,
		func(from, to time.Time) (*verdict.Report, error) {
			return s.l.Report(ctx, from, to, s.cfg.Options, false)
		},
		func() (*verdict.Report, error) {
			return s.l.Report(ctx, from.Add(-7*24*time.Hour), from, s.cfg.Options, false)
		})
	if err != nil || !strings.HasPrefix(line, "No incident written down from 2026-10-26") || nums.(map[string]any)["no_record"] != true {
		t.Errorf("the report's noise: %q %v %v", line, nums, err)
	}
}

// noRecordPaths are the pages that say there is no record, of a server
// with none (facetServerOf).
var noRecordPaths = []string{"/", "/rules", "/rules?perf=unrecorded", "/rules/R00", "/rules/R00?weeks=1"}

// TestNoRecordPages: what the pages say with no record has a translation
// in every language, and they pass the accessibility checks: of a server
// with no incident at all, and of the test server's week before, which has
// none of its own (keptIncident).
func TestNoRecordPages(t *testing.T) {
	ctx := context.Background()
	none := facetServerOf(t, false).Handler()
	week, _ := server(t, nil)
	raw := ""
	if err := week.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	week.jobs(ctx, true)
	weekPaths := []string{"/rules?week=2026-10-26"}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	for _, x := range []struct {
		h     http.Handler
		paths []string
	}{{none, noRecordPaths}, {week.Handler(), weekPaths}} {
		for _, n := range names {
			for _, p := range x.paths {
				r := httptest.NewRequest(http.MethodGet, p, nil)
				r.Header.Set("Accept-Language", n.Code)
				w := httptest.NewRecorder()
				x.h.ServeHTTP(w, r)
				if w.Code != http.StatusOK {
					t.Errorf("%s %s: %d", n.Code, p, w.Code)
				}
			}
		}
		pageA11y(t, x.h, "", x.paths...)
	}
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
	if _, body, _ := get(t, week.Handler(), "/rules?week=2026-10-26"); !strings.Contains(body, "No incident was written down in the week of 2026-10-26") {
		t.Error("a week's list doesn't say it has no record")
	}
}
