package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/oncall"
	"lapilli.dev/internal/verdict"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var verb = regexp.MustCompile(`%(?:\[\d+\])?[-+# 0]*\d*(?:\.\d*)?(?:\[\d+\])?[a-zA-Z%]`)

// verbs are a text's verbs by the argument each takes, ignoring order.
func verbs(s string) []string {
	var out []string
	for _, v := range verb.FindAllString(s, -1) {
		if v == "%%" {
			continue
		}
		out = append(out, v[len(v)-1:])
	}
	slices.Sort(out)
	return out
}

// Every translation takes the same arguments as its English, and gives
// back what it is given.
func TestCatalogs(t *testing.T) {
	for lang, m := range catalogs {
		for en, x := range m {
			if a, b := verbs(en), verbs(x); !slices.Equal(a, b) {
				t.Errorf("%s: %q has %v, its translation %q has %v", lang, en, a, x, b)
			}
			if strings.Contains(fmt.Sprintf(x, 1, 2, 3, 4, 5, 6), "%!") && len(verbs(x)) > 0 {
				args := make([]any, 0, len(verbs(en)))
				for _, v := range verb.FindAllString(en, -1) {
					switch v[len(v)-1] {
					case 'd':
						args = append(args, 1)
					case 'f':
						args = append(args, 1.0)
					case 's', 'q', 'v':
						args = append(args, "x")
					}
				}
				if got := fmt.Sprintf(x, args...); strings.Contains(got, "%!") {
					t.Errorf("%s: %q formats as %q", lang, x, got)
				}
			}
		}
	}
}

// pagePaths are every page, with something on each once pagesServer has
// written it.
var pagePaths = []string{"/", "/rules", "/rules?weeks=1", "/rules/Errors", "/incidents", "/incidents/INC-1", "/incidents/impact-20261103T0600Z",
	"/firings?day=2026-11-02", "/settings", "/settings?try=1&lead=1h", "/runs", "/incidents/none",
	"/teams", "/teams?edit=1", "/windows", "/windows/1", "/tasks", "/tasks?status=all", "/tasks/1", "/?team=payments", "/rules?team=payments", "/outcomes",
	"/reports", "/reports?edit=1", "/reports/1", "/reports/2", "/reports?edit=digest", "/reports/digest", "/roles", "/windows/2", "/windows/3",
	"/rules?sort=refired", "/rules?sort=deploys&weeks=12", "/incidents?source=impact&team=payments", "/incidents?source=written", "/rules/Flapping",
	"/rules/Limited"}

// pagesServer is a server with something on every page: a team, a note,
// quiet windows, a task, roles, reports, comments, and a run that failed.
func pagesServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "ops")
	os.WriteFile(dest, []byte("http://127.0.0.1:1/x"), 0o600)
	s, rules := server(t, func(c *Config) {
		c.CheckReplay, c.CheckContent, c.ReportDests = true, true, map[string]string{"ops": dest}
		// An on-call schedule, read once, and once that failed.
		c.OnCall = &oncall.Config{Schedules: []*oncall.Schedule{{Name: "primary"}, {Name: "secondary"}}}
	})
	// A rule in a group with a limit, which a replay doesn't hold it to.
	os.WriteFile(filepath.Join(rules, "limited.yaml"), []byte("groups:\n  - name: limited\n    limit: 5\n    labels: {team: shop}\n    rules:\n      - {alert: Limited, expr: errors > 5}\n"), 0o644)
	s.h = &alertsHistory{} // the replay set beside what really fired
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	s.checkEmpty([]string{grafanaAlertsSeries}, now.Add(-24*time.Hour))
	o := s.cfg.Options
	tid, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "Errors"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "Errors", TeamID: tid, Status: "tune", Runbook: "https://x.example", Note: "n"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutWindow(ctx, ledger.Window{Title: "w", Start: now.Add(-50 * time.Hour), End: now.Add(-30 * time.Hour)}, "ana", o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "t", Kind: "tune", Rule: "Errors", TeamID: tid, Due: now}, "ana"); err != nil {
		t.Fatal(err)
	}
	// Windows every month, by cron, and a team's; a role given; the
	// digest set in the UI.
	for _, w := range []ledger.Window{
		{Title: "monthly", Start: time.Date(2026, 10, 3, 2, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 3, 3, 0, 0, 0, time.UTC), Every: "month", On: "first sat"},
		{Title: "cron", Start: time.Date(2026, 10, 3, 2, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 3, 3, 0, 0, 0, time.UTC), Every: "cron", On: "0 2 * * 6",
			TeamID: tid, Until: now},
	} {
		if _, err := s.l.PutWindow(ctx, w, "ana", o); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.l.PutGrant(ctx, ledger.Grant{Who: "user:pat", TeamID: tid}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutGrant(ctx, ledger.Grant{Who: "group:sre"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := s.l.PutDigest(ctx, ledger.DigestSet{Schedule: "weekly tue@10:00", Dests: []string{"ops", "gone"}}, "ana", now); err != nil {
		t.Fatal(err)
	}
	// A report holding everything, and one whose team was removed.
	if _, err := s.l.PutScheduled(ctx, ledger.Scheduled{Name: "payments weekly", TeamID: tid, Zone: "team", Schedule: "weekly mon@09:00",
		Sections: ledger.ReportSections, Dests: []string{"ops"}, Enabled: true}, "ana", now); err != nil {
		t.Fatal(err)
	}
	gone, err := s.l.PutTeam(ctx, ledger.Team{Name: "gone", Rules: "^Gone"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutScheduled(ctx, ledger.Scheduled{Name: "gone daily", TeamID: gone, Schedule: "0 9 * * *", Span: "7d", Dests: []string{"ops", "old"}}, "ana", now); err != nil {
		t.Fatal(err)
	}
	if err := s.l.DeleteTeam(ctx, gone, "ana"); err != nil {
		t.Fatal(err)
	}
	for _, on := range [][2]string{{"rule", "Errors"}, {"task", "1"}, {"incident", "INC-1"}} {
		if _, err := s.l.PutComment(ctx, ledger.Comment{On: on[0], Key: on[1], Body: "c"}, "anonymous"); err != nil {
			t.Fatal(err)
		}
	}
	// A comment copied in from the task's issue.
	if _, err := s.l.CopyComment(ctx, 1, "seen in the tracker", "sam", "c-1", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// An alert re-firing, copied in, and a deploy of what it is about
	// while it fired (docs/design/noc.md).
	day := s.l.Midnight(s.l.Day(now) - 1)
	var flaps []firing.Firing
	for k := 0; k < 4; k++ {
		at := day.Add(10*time.Hour + time.Duration(k)*20*time.Minute)
		flaps = append(flaps, firing.Firing{Rule: "Flapping", Labels: map[string]string{"service": "search"}, Start: at, End: at.Add(5 * time.Minute)})
	}
	if _, err := s.l.PutFirings(ctx, "test-src", day, day.Add(24*time.Hour), flaps, "firings-token", o); err != nil {
		t.Fatal(err)
	}
	o.DeployAfter = 10 * time.Minute
	if _, err := s.l.SyncDeploys(ctx, []ledger.Deploy{{Start: day.Add(10 * time.Hour), Labels: map[string]string{"service": "search"}, Revision: "4f2a9c1"}}, o, ledger.DeploysRead{}); err != nil {
		t.Fatal(err)
	}
	if err := s.l.JudgePending(ctx, o); err != nil {
		t.Fatal(err)
	}
	// A rule that nearly caught the impact window nothing caught.
	week := s.l.Midnight(ledger.Week(s.l.Day(now)))
	if err := s.l.ReplaceResult(ctx, "near_misses", now, week, week.Add(7*24*time.Hour),
		[]verdict.NearMiss{{Incident: "impact-20261103T0600Z", Rule: "Errors", Held: time.Minute, For: 2 * time.Minute, NewFor: time.Minute}}); err != nil {
		t.Fatal(err)
	}
	read := s.onCallRead("primary")
	read.At, read.OK, read.Turns, read.From, read.To = now, now, 3, now.Add(-72*time.Hour), now.Add(72*time.Hour)
	read.Unknown, read.Notes = []string{"pat"}, []string{"an event with no one in it"}
	s.onCallRead("secondary").Err = "feed.example.com: 503"
	s.status.set("replaying %s → %s", "a", "b")
	s.fail("a run", fmt.Errorf("boom"))
	return s
}

// Every page in each language finds a translation for every text it
// shows, says which language it is in, and offers the others.
func TestPagesInEachLanguage(t *testing.T) {
	h := pagesServer(t).Handler()
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	for _, n := range names {
		for _, p := range pagePaths {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Accept-Language", n.Code+";q=0.9, de;q=0.5")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if !strings.Contains(w.Body.String(), `<html lang="`+n.Code+`">`) {
				t.Errorf("%s %s: %d, not in %s", n.Code, p, w.Code, n.Code)
			}
		}
	}
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()

	// The language chosen on the page wins, and is kept.
	r := httptest.NewRequest(http.MethodGet, "/lang?to=ko&back=/rules%3Fweeks%3D12", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/rules?weeks=12" || !strings.Contains(w.Header().Get("Set-Cookie"), "lang=ko") {
		t.Errorf("choosing ko: %d %v", w.Code, w.Header())
	}
	r = httptest.NewRequest(http.MethodGet, "/rules", nil)
	r.Header.Set("Accept-Language", "en")
	r.AddCookie(&http.Cookie{Name: "lang", Value: "ko"})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "규칙") {
		t.Error("the cookie should win over the browser's language")
	}
	// Never back to another site.
	for _, back := range []string{"//evil.example/x", "https://evil.example/", "/\\evil.example", "javascript:alert(1)"} {
		r := httptest.NewRequest(http.MethodGet, "/lang?to=en&back="+back, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") || strings.Contains(loc, "evil") {
			t.Errorf("back=%s went to %q", back, loc)
		}
	}
}

var messages = regexp.MustCompile(`(?:\b(?:invalid|bad|checkText)\(|\btr\([\w.]+,|\bstatus\.set\(|\bs\.fail\()\s*"((?:[^"\\]|\\.)*)"`)

// Every message a write can be refused with, kept apart from its values,
// every line told to Slack, and every text the worker's status line shows
// on each page while it works or once it failed, has a translation: they
// are shown only when something is refused, told or being done, so
// rendering the pages doesn't find them.
func TestRefusalsTranslated(t *testing.T) {
	for _, dir := range []string{".", "../ledger", "../schedule"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range messages.FindAllStringSubmatch(string(b), -1) {
				msg, err := strconv.Unquote(`"` + m[1] + `"`)
				if err != nil {
					t.Fatal(err)
				}
				if msg == "" {
					continue // the status line emptied
				}
				for lang, c := range catalogs {
					if _, ok := c[msg]; !ok {
						t.Errorf("%s: %s has no translation for %q", f, lang, msg)
					}
				}
			}
		}
	}
}

// No catalog gives one English text twice: the second would quietly win.
func TestCatalogsNoDuplicates(t *testing.T) {
	files, err := catalogFiles.ReadDir("i18n")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := catalogFiles.ReadFile("i18n/" + f.Name())
		if err != nil {
			t.Fatal(err)
		}
		d := json.NewDecoder(bytes.NewReader(b))
		if _, err := d.Token(); err != nil { // {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				t.Fatal(err)
			}
			var v string
			if err := d.Decode(&v); err != nil {
				t.Fatalf("%s: %q: %v", f.Name(), k, err)
			}
			if seen[k.(string)] {
				t.Errorf("%s gives %q twice", f.Name(), k)
			}
			seen[k.(string)] = true
		}
	}
}

// TestChartLangs: the Helm chart lets serve.notify.lang be every language --lang
// takes (values.yaml says "en, es, ja or ko").
func TestChartLangs(t *testing.T) {
	b, err := os.ReadFile("../../charts/lapilli/templates/_helpers.tpl")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`has \(\$s\.notify\.lang \| default "en"\) \(list ([^)]*)\)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no serve.notify.lang check in _helpers.tpl")
	}
	allowed := map[string]bool{}
	for _, q := range regexp.MustCompile(`"(\w+)"`).FindAllSubmatch(m[1], -1) {
		allowed[string(q[1])] = true
	}
	for _, l := range Langs() {
		if !allowed[l] {
			t.Errorf("--lang takes %q, the chart refuses serve.notify.lang=%s (_helpers.tpl allows %s)", l, l, m[1])
		}
	}
}

// TestChartLokiAlone: the chart's check that serve has rules to replay
// counts a Loki ruler's rules given through extraArgs (--loki-loaded-rules,
// --loki-rules), as the CHANGELOG says the chart takes them; checked with
// helm template beside this test.
func TestChartLokiAlone(t *testing.T) {
	b, err := os.ReadFile("../../charts/lapilli/templates/_helpers.tpl")
	if err != nil {
		t.Fatal(err)
	}
	tpl := string(b)
	for _, want := range []string{`hasPrefix "--loki-loaded-rules"`, `hasPrefix "--loki-rules"`, `(not $r.alarms) (not $loki)`} {
		if !strings.Contains(tpl, want) {
			t.Errorf("_helpers.tpl hasn't %s", want)
		}
	}
}
