package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/report"
	"lapilli.dev/internal/rules"
)

// putAs writes an incident through the incidents port, with the token.
func putAs(t *testing.T, s *Server, token, id, start string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/incidents/"+id, strings.NewReader(`{"title": "t", "start": "`+start+`"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.IncidentsHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", id, w.Code, w.Body.String())
	}
}

// sourceRowOf is the row of the Incident sources table a source names.
func sourceRowOf(body, name string) string {
	i := strings.Index(body, `id="incident-sources"`)
	if i < 0 {
		return ""
	}
	body = body[i:]
	j := strings.Index(body, name)
	if j < 0 {
		return ""
	}
	start := strings.LastIndex(body[:j], "<tr>")
	end := strings.Index(body[j:], "</tr>")
	return body[start : j+end]
}

// What a tool's job copies in is listed by source, its ids' prefix: when
// it last wrote, unchanged or not, and how many incidents in four weeks;
// quiet when nothing came for two days. Only an admin sees the port and
// the token's file; no one, the token.
func TestSourcesIntake(t *testing.T) {
	token := "an-incidents-token-0123"
	file := tokenFile(t, token)
	s := criteriaServer(t, false, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file })
	h := s.Handler()
	putAs(t, s, token, "PD-1", "2026-11-01T00:00:00Z")
	putAs(t, s, token, "pd-2", "2026-10-01T00:00:00Z") // over four weeks ago: not counted
	putAs(t, s, token, "OPS-3", "2026-11-02T00:00:00Z")
	_, body := askAs(t, h, http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	pd := sourceRowOf(body, "PD-")
	if !strings.Contains(pd, "(PagerDuty)") || !strings.Contains(pd, "✓ working") || !strings.Contains(pd, "last received 2026-11-03 12:00") ||
		!strings.Contains(pd, `<td class="n">1</td>`) || !strings.Contains(pd, "port 127.0.0.1:0, token in "+file) {
		t.Errorf("PD:\n%s", pd)
	}
	if ops := sourceRowOf(body, "OPS-"); !strings.Contains(ops, "✓ working") || strings.Contains(ops, "(") {
		t.Errorf("OPS:\n%s", ops)
	}
	if strings.Contains(body, token) {
		t.Error("the token is shown")
	}
	_, body = askAs(t, h, http.MethodGet, "/settings", nil, "bo@example.com", "same-origin")
	if strings.Contains(body, file) || strings.Contains(body, "127.0.0.1:0") {
		t.Error("an editor sees the port and the token's file")
	}
	// Two days on, OPS's job wrote again, PD's didn't.
	s.now = func() time.Time { return now.Add(49 * time.Hour) }
	putAs(t, s, token, "OPS-3", "2026-11-02T00:00:00Z")
	_, body = askAs(t, h, http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if pd := sourceRowOf(body, "PD-"); !strings.Contains(pd, "⚠&#xFE0E; quiet") && !strings.Contains(pd, "⚠︎ quiet") || !strings.Contains(pd, "is its job still running?") {
		t.Errorf("PD, two days on:\n%s", pd)
	}
	if ops := sourceRowOf(body, "OPS-"); !strings.Contains(ops, "✓ working") || !strings.Contains(ops, "last received 2026-11-05 13:00") {
		t.Errorf("OPS, written again:\n%s", ops)
	}
}

// A source's writes are kept once in receivedEvery by a replica, not each
// write: a job writes every incident each hour.
func TestSourcesReceivedSeldom(t *testing.T) {
	ctx := context.Background()
	token := "an-incidents-token-0123"
	s := criteriaServer(t, false, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", tokenFile(t, token) })
	for _, after := range []time.Duration{0, time.Minute, receivedEvery + time.Minute} {
		s.now = func() time.Time { return now.Add(after) }
		putAs(t, s, token, "PD-1", "2026-11-01T00:00:00Z")
		got, err := s.l.Received(ctx)
		want := now
		if after > receivedEvery {
			want = now.Add(after)
		}
		if err != nil || !got["PD"].Equal(want) {
			t.Errorf("after %s: %s %v", after, got["PD"], err)
		}
	}
}

// The incidents file and the impact signal: read, or failing with why; a
// source not set up says so, in no colour.
func TestSourcesReadOrFailing(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, true, nil)
	h := s.Handler()
	_, body, _ := get(t, h, "/settings")
	if row := sourceRowOf(body, "Incidents file"); !strings.Contains(row, "✓ working") || !strings.Contains(row, "last read 2026-11-03 12:00") {
		t.Errorf("file:\n%s", row)
	}
	if row := sourceRowOf(body, "Impact signal"); !strings.Contains(row, "✓ working") || !strings.Contains(row, `<td class="n">1</td>`) {
		t.Errorf("impact:\n%s", row)
	}
	if row := sourceRowOf(body, "Intake API"); !strings.Contains(row, `status-unknown">? not set up`) {
		t.Errorf("intake:\n%s", row)
	}
	os.WriteFile(s.cfg.Incidents, []byte("not: [incidents"), 0o644)
	if err := s.l.SetSettings(ctx, map[string]string{"impact": "turned"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now.Add(2 * time.Hour) } // a run due
	s.jobs(ctx, true)
	_, body, _ = get(t, h, "/settings")
	if row := sourceRowOf(body, "Incidents file"); !strings.Contains(row, `status-bad">✕ failing`) || !strings.Contains(row, "yaml") {
		t.Errorf("file failing:\n%s", row)
	}
	if row := sourceRowOf(body, "Impact signal"); !strings.Contains(row, `status-bad">✕ failing`) || !strings.Contains(row, "slow down") {
		t.Errorf("impact failing:\n%s", row)
	}
}

// sourcesStates are deployments whose Incident sources draw each state
// besides those of a11yServer's, which has no incidents port: under
// /sources, a tool's job gone quiet, the incidents file failing and the
// impact signal read; under /intake, a port that nothing came through yet,
// and nothing written in four weeks; under /nodata, an impact signal one of
// whose selectors has no data; under /floor, a criterion saved and read by
// a run, not yet read again. For TestStatusKinds and TestA11yServe.
func sourcesStates(t *testing.T) map[string]*Server {
	t.Helper()
	ctx := context.Background()
	token := "an-incidents-token-0123"
	intake := func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", tokenFile(t, token) }
	quiet := criteriaServer(t, true, intake)
	quiet.now = func() time.Time { return now.Add(-72 * time.Hour) }
	putAs(t, quiet, token, "PD-1", "2026-10-30T00:00:00Z")
	os.WriteFile(quiet.cfg.Incidents, []byte("not: [incidents"), 0o644)
	quiet.now = func() time.Time { return now.Add(2 * time.Hour) }
	quiet.jobs(ctx, true)
	// An impact signal one of whose selectors has no data.
	none := criteriaServer(t, false, nil)
	hist(none).begins = map[string][]time.Time{"probe_failed": {}}
	if err := none.l.SetSettings(ctx, map[string]string{"impact": "impact or probe_failed == 1"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	none.jobs(ctx, false)
	// A criterion saved, its read again failing, and a run reading it: a
	// floor.
	floor := criteriaServer(t, false, nil)
	saveFailing(t, floor, "impact > 1")
	runDue(floor, 2)
	floor.jobs(ctx, true)
	return map[string]*Server{"/sources": quiet, "/intake": criteriaServer(t, false, intake), "/nodata": none, "/floor": floor}
}

// What is wrong with the deploys file or the marked file is said without
// its path to a reader who isn't an admin, in the status banner of every
// page, as the incidents file's is: by what it is.
func TestFilePathsInBannerAnAdminsAlone(t *testing.T) {
	ctx := context.Background()
	for _, which := range []string{"deploys", "marked"} {
		t.Run(which, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private-mount", which+".yaml")
			s := criteriaServer(t, true, func(c *Config) {
				switch which {
				case "deploys":
					c.Deploys = path
				case "marked":
					c.Marked = path
				}
			})
			runDue(s, 2)
			s.jobs(ctx, true)
			h := s.Handler()
			for _, who := range []string{"carl@example.com", "bo@example.com"} {
				for _, p := range []string{"/", "/settings"} {
					code, body := askAs(t, h, http.MethodGet, p, nil, who, "same-origin")
					if i := strings.Index(body, filepath.Dir(path)); code != http.StatusOK || i >= 0 {
						t.Errorf("%s, %s: %d, the path on the page: %s", who, p, code, body[max(0, i-120):min(len(body), max(0, i)+120)])
					}
					if !strings.Contains(body, "the "+which+" file") {
						t.Errorf("%s, %s: the banner doesn't say which file", who, p)
					}
				}
			}
			if _, body := askAs(t, h, http.MethodGet, "/", nil, "ana@example.com", "same-origin"); !strings.Contains(body, path) {
				t.Error("an admin doesn't read the path")
			}
		})
	}
}

// The paths taken out are those of every file serve was started with,
// the longest first, as given and as a path joined under one says it; a
// bare name is left in the sentences it is a word of.
func TestRedactPaths(t *testing.T) {
	dir := t.TempDir()
	s, _ := server(t, func(c *Config) {
		c.RulesPath, c.Config = dir+"/rules/", dir+"/rules/config.yaml"
		c.Slack, c.ReportDests = dir+"/hooks/slack", map[string]string{"ops": dir + "/hooks/ops"}
		c.ChangesPaused = "ledger"
	})
	for in, want := range map[string]string{
		"keeping the ledger to --config: open " + dir + "/rules/config.yaml: permission denied": "keeping the ledger to --config: open the config file: permission denied",
		"reading the rules: " + dir + "/rules/shop.yaml: line 3":                                "reading the rules: the path given to --rules/shop.yaml: line 3",
		"notifying: --report-dest: open " + dir + "/hooks/ops: no such file":                    "notifying: --report-dest: open the path given to --report-dest: no such file",
		"the ledger: disk full": "the ledger: disk full",
	} {
		if got := s.redactPaths(in, "en"); got != want {
			t.Errorf("%q\n became %q,\n want %q", in, got, want)
		}
	}
}

// A path is taken out where a text says it whole, never inside a word:
// a directory given with a separator at its end, as a shell completes one,
// is a bare name once it is let go, and stays in the sentences it is a
// word of; a path is taken out where a separator follows it, and kept in
// a longer name; and what is put in place of one isn't looked in again.
func TestRedactPathsWhole(t *testing.T) {
	sentences := []string{
		"reading the rules: a rule's expression didn't parse",
		"the ledger: database or disk is full; data kept as it was",
		"replaying again from the history of rule versions",
		"reading the rules: open rules.d: the history is read again.",
	}
	for _, c := range []Config{
		{RulesPath: "rules/", Ledger: "data/", RulesHistory: "history/"},
		{RulesPath: "rules", Ledger: "data", RulesHistory: "history"},
		{RulesPath: ".", Ledger: "./", RulesHistory: "../"},
	} {
		s := &Server{cfg: c}
		for _, in := range sentences {
			if got := s.redactPaths(in, "en"); got != in {
				t.Errorf("%+v: %q\n became %q", c, in, got)
			}
		}
	}
	s := &Server{cfg: Config{RulesPath: "./x", Ledger: "data/", Marked: "./x/marked.yaml"}}
	for in, want := range map[string]string{
		"open ./x/shop.yaml: permission denied":  "open the path given to --rules/shop.yaml: permission denied",
		`reading "./x/marked.yaml": line 2`:      `reading "the marked file": line 2`,
		"open ./xy/shop.yaml and ../x/shop.yaml": "open ./xy/shop.yaml and ../x/shop.yaml",
		"the rules in ./x.":                      "the rules in the path given to --rules.",
	} {
		if got := s.redactPaths(in, "en"); got != want {
			t.Errorf("%q\n became %q,\n want %q", in, got, want)
		}
	}
	// "given" in what takes the place of x/y is the replacement's.
	ps := []filePath{{"x/y", "--rules"}, {"given", "--ledger"}}
	if got, want := hidePaths("open x/y: given up", ps, "en"), "open the path given to --rules: the path given to --ledger up"; got != want {
		t.Errorf("became %q, want %q", got, want)
	}
}

// A history behind a link, as a git-sync or a mounted volume keeps one, is
// walked where the link leads (rules.History), and what fails there says
// that path: it is taken out too, as the path given is.
func TestRedactPathsLinkTarget(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "store", "rev-1")
	team := filepath.Join(real, "2026-11-01T00:00:00Z", "team")
	if err := os.MkdirAll(team, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(team, "r.yaml"), []byte("groups: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "versions")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(team, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(team, 0o755) })
	_, err := rules.History(link)
	if err == nil {
		t.Skip("the version was walked: this user reads what it may not")
	}
	s := &Server{cfg: Config{RulesHistory: link}}
	got := s.redactPaths("reading the rules: --rules-history: "+err.Error(), "en")
	if strings.Contains(got, "store") || !strings.Contains(got, "the path given to --rules-history/2026-11-01T00:00:00Z/team") {
		t.Errorf("%v\n became %q", err, got)
	}
}

// Why a run failed, and why a rule couldn't be replayed, are said without
// the paths of the files serve was started with to a reader who isn't an
// admin, as the banner says them: on the run history, in /api/v1/runs and
// in the overview's last run. The digest's webhook file that can't be read
// is one.
func TestRunsFailedPathsAnAdminsAlone(t *testing.T) {
	ctx := context.Background()
	hook := filepath.Join(t.TempDir(), "private-mount", "slack-hook")
	s := criteriaServer(t, true, func(c *Config) { c.Slack = hook })
	err := report.Slack(ctx, s.cfg.Slack, "x")
	if err == nil || !strings.Contains(err.Error(), hook) {
		t.Fatalf("the digest's error doesn't say the file: %v", err)
	}
	if err := s.l.AddRun(ctx, ledger.Run{Kind: "hourly", Started: s.now(), Finished: s.now(), From: s.now().Add(-time.Hour), To: s.now(),
		Failed: map[string]string{"SomeRule": "reading " + hook + ".d/extra: no such file"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.l.AddRun(ctx, ledger.Run{Kind: "digest", Started: s.now(), Finished: s.now(), From: s.now().Add(-7 * 24 * time.Hour), To: s.now(), Error: err.Error()}); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	for _, p := range []string{"/runs", "/api/v1/runs", "/"} {
		for _, who := range []string{"carl@example.com", "bo@example.com"} {
			code, body := askAs(t, h, http.MethodGet, p, nil, who, "same-origin")
			if i := strings.Index(body, filepath.Dir(hook)); code != http.StatusOK || i >= 0 {
				t.Errorf("%s, %s: %d, the path is shown: %s", who, p, code, body[max(0, i-80):min(len(body), max(0, i)+80)])
			}
			if !strings.Contains(body, "the path given to --slack-webhook-file") {
				t.Errorf("%s, %s: the run doesn't say which file", who, p)
			}
		}
		if _, body := askAs(t, h, http.MethodGet, p, nil, "ana@example.com", "same-origin"); !strings.Contains(body, hook) {
			t.Errorf("%s: an admin doesn't read the path", p)
		}
	}
	for _, who := range []string{"carl@example.com", "ana@example.com"} {
		_, body := askAs(t, h, http.MethodGet, "/runs", nil, who, "same-origin")
		if strings.Contains(body, hook+".d") == (who == "carl@example.com") || !strings.Contains(body, "SomeRule") {
			t.Errorf("%s: why a rule couldn't be replayed, as shown: the path there %v", who, strings.Contains(body, hook+".d"))
		}
	}
}
