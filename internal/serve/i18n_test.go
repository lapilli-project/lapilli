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
	"lapilli.dev/internal/incident"
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
	"/firings?day=2026-11-02", "/firings?day=2026-11-01", "/settings", "/settings?try=1&lead=1h", "/runs", "/incidents/none",
	// The incident criteria tried: another expression, off, and one that
	// doesn't read.
	"/settings?impact-try=1&impact=impact+%3E+1&impact-for=10m", "/settings?impact-try=1&impact=off", "/settings?impact-try=1&impact=sum%28",
	"/teams", "/teams?edit=1", "/windows", "/windows/1", "/tasks", "/tasks?status=all", "/tasks/1", "/?team=payments", "/rules?team=payments", "/outcomes",
	"/reports", "/reports?edit=1", "/reports/1", "/reports/2", "/reports?edit=digest", "/reports/digest", "/roles", "/windows/2", "/windows/3",
	"/rules?sort=refired", "/rules?sort=deploys&weeks=12", "/incidents?source=impact&team=payments", "/incidents?source=written", "/incidents?weeks=1", "/rules/Flapping",
	"/rules/Limited", "/search", "/search?q=i", "/search?q=nothing-is-called-this", "/rules?perf=silent&weeks=12",
	"/rules?perf=ok&severity=none&trust=differs&q=i+label%3Aservice%3Dsearch+group%3Ashop+foo%3Abar", "/rules?team=payments&q=nothing-is-called-this",
	// Many at once, and ways in: what a bulk write did, the lists over a
	// week, a day's firings by a label, the overview's marks off.
	"/rules?bulk=task&done=3&theirs=2&refused=1",
	"/rules?bulk=propose&done=2&made=1&made=2&already=1&same=1&nosuggest=1&unreplayed=1&absent=1&limit=1&refused=1&editors=1&failed=1&untried=1", "/firings?day=2026-11-02&label=service%3Dcheckout&bulk=real&done=1&already=1",
	"/rules?week=2026-10-26&perf=noisy", "/incidents?week=2026-10-26&state=missed", "/?marks=none", "/?marks=deploys",
	// A rule's page in each tab, and over each period.
	"/rules/Errors?tab=firings", "/rules/Errors?tab=versions", "/rules/Errors?tab=proposals", "/rules/Errors?tab=activity", "/rules/Errors?tab=expectations",
	"/rules/Flapping?tab=firings", "/rules/Errors?weeks=1", "/rules/Errors?weeks=4&tab=firings", "/rules/Limited?tab=versions",
	// The goals, everyone's and a team's (docs/design/goals.md).
	"/goals", "/goals?team=payments",

	// Marked windows: the page, one made here, the file's, a team's;
	// the firings of a kind.
	"/marked", "/marked?team=payments", "/marked/1", "/marked/2", "/marked/3", "/firings?day=2026-11-02&kind=sale",
	"/rules?during=sale", "/rules?outside=sale&weeks=12", "/rules?during=deploy",

	// Sources as objects, and the activity log, filtered, a page past
	// the first (docs/design/serveops.md).
	"/sources", "/activity", "/activity?kind=task&team=payments", "/activity?who=ana&since=2026-10-01&until=2026-11-30"}

// pagesServer is a server with something on every page: a team, a note,
// quiet windows, a task, roles, reports, comments, and a run that failed.
func pagesServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "ops")
	os.WriteFile(dest, []byte("http://127.0.0.1:1/x"), 0o600)
	s, rules := server(t, func(c *Config) {
		// The digest's week judged: its noisy rule, suggested a longer for.
		keepRecord(c)
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
	s.checkEmpty([]string{grafanaAlertsSeries}, "", "", now.Add(-24*time.Hour))
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
	// Marked windows: a sale made here over the day's firings, a weekly
	// launch of search from the marked file, and a team's campaign ahead.
	if _, err := s.l.PutMarked(ctx, ledger.Marked{Kind: "sale", Title: "Autumn sale", Start: day, End: day.Add(24 * time.Hour)}, "ana", o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.SyncMarked(ctx, []ledger.Marked{{Kind: "launch", Title: "Search, new ranking", Start: day.Add(-14 * 24 * time.Hour).Add(9 * time.Hour),
		End: day.Add(-14 * 24 * time.Hour).Add(12 * time.Hour), Every: "week", Labels: map[string]string{"service": "search"}}}, o, ledger.MarkedRead{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutMarked(ctx, ledger.Marked{Kind: "campaign", Title: "Newsletter", Start: now.Add(48 * time.Hour), End: now.Add(50 * time.Hour), TeamID: tid}, "ana", o); err != nil {
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
	// Goals: everyone's, payments's own, and each team's; and over a
	// period, everyone's of the sale and each team's of the launch.
	declare(t, s, "goals:\n  - {name: precise, measure: precision, at_least: 0.5}\n  - {name: paged, team: payments, measure: critical.firings, critical: [critical, page], at_most: 40}\n"+
		"  - {name: no-refire, team: \"*\", measure: refired.firings, at_most: 0}\n"+
		"  - {name: sale-no-worse, each: sale, measure: precision, no_worse_than: before, by: 0.05}\n"+
		"  - {name: launch-missed, team: \"*\", each: launch, measure: incidents.missed, at_most: 0}\n")
	s.status.set("replaying %s → %s", "a", "b")
	s.fail("a run", fmt.Errorf("boom"))
	return s
}

// peopleSay gives pagesServer's s people's verdicts (docs/design/judging.md,
// item 3), apart from it, as the goldens made of it hold it as it was: an
// Errors firing on search said unrelated to INC-V, written over it, and
// needing no action; another answered yes, its verdict to look at again.
func peopleSay(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	o := s.cfg.Options
	if _, err := s.l.Put(ctx, incident.Incident{ID: "INC-V", Title: "search", Start: now.Add(-40 * time.Hour), End: now.Add(-40*time.Hour + 15*time.Minute),
		Labels: incident.Labels{"service": {"search"}}}, "ana", o); err != nil {
		t.Fatal(err)
	}
	errs, err := s.l.Firings(ctx, ledger.FiringQuery{From: now.Add(-51 * time.Hour), To: now, Rule: "Errors"})
	if err != nil {
		t.Fatal(err)
	}
	said := 0
	for _, f := range errs {
		if f.Labels["service"] != "search" {
			continue
		}
		k := ledger.FiringKey{ID: f.ID, Series: f.Series, Start: f.Start}
		by := ledger.VerdictBy{By: "ana", Admin: true, Now: now}
		switch {
		case f.White:
			yes, no := true, "no"
			if _, err := s.l.SetVerdict(ctx, k, ledger.VerdictChange{Unrelated: &yes, Action: &no}, by, o); err != nil {
				t.Fatal(err)
			}
			said++
		case said < 2 && f.Start.Before(now.Add(-45*time.Hour)):
			yes := "yes"
			if _, err := s.l.SetVerdict(ctx, k, ledger.VerdictChange{Action: &yes}, by, o); err != nil {
				t.Fatal(err)
			}
			ledgerExec(t, s, "UPDATE verdicts SET moved = 'gone' WHERE start_at = ?", f.Start.Unix())
			said = 2
		}
	}
}

// Every page in each language finds a translation for every text it
// shows, says which language it is in, and offers the others.
func TestPagesInEachLanguage(t *testing.T) {
	s := pagesServer(t)
	peopleSay(t, s)
	h := s.Handler()
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	for _, n := range names {
		for _, p := range pagePaths {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Header.Set("Sec-Fetch-Site", "none") // a page asked for in a browser, as axe does
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

// A refusal that says another (incident L-1 would grow past its bounds:
// …) says both in the reader's language, not the one within in English.
func TestRefusalWithinTranslated(t *testing.T) {
	err := ledger.ErrInvalid{Format: "incident %s would grow past its bounds: %s",
		Args: []any{"L-1", ledger.ErrInvalid{Format: "an incident lasts 31 days at most"}}}
	for lang, want := range map[string]string{
		"en": "incident L-1 would grow past its bounds: an incident lasts 31 days at most",
		"ko": "L-1 인시던트가 한도를 넘어 커지게 돼요: 인시던트는 최대 31일까지예요",
	} {
		if got := message(lang, err); got != want {
			t.Errorf("%s: %q, want %q", lang, got, want)
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
