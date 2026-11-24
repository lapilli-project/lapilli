package serve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// configFile is a --config declaring a team, a quiet window, a report and
// two roles.
const configFile = `teams:
  - name: payments
    rules: ^Errors$
    labels: {team: payments}
    tz: Asia/Seoul
windows:
  - title: Nightly backup
    team: payments
    start: 2026-11-02T02:00
    end: 2026-11-02T03:00
    every: day
    rules: ^Backup
reports:
  - name: payments weekly
    team: payments
    schedule: weekly mon@09:00
    sections: [summary, noise]
    dests: [team:payments]
grants:
  - group: payments-oncall
    team: payments
  - user: ana@example.com
`

// writeConfig writes a --config file, as Kubernetes swaps a ConfigMap's:
// in place of the one there.
func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// configServer is rolesServer with a --config, kept to once, as the leader
// does when it starts.
func configServer(t *testing.T, body string, change func(*Config)) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, body)
	s, _, _ := rolesServer(t, func(c *Config) {
		c.Config = path
		if change != nil {
			change(c)
		}
	})
	readTwice(t, s)
	return s, path
}

// readTwice reads --config as the leader does once it changed: the first
// read noted, changing nothing, and the second, configEvery after, keeping
// the ledger to it.
func readTwice(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	clockOn(s, configEvery)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
}

// clockOn moves the server's clock on by d, from where it stands.
func clockOn(s *Server, d time.Duration) {
	at := s.wall().Add(d)
	s.wall = func() time.Time { return at }
}

// A config is read strictly: a key it doesn't know, a value of the wrong
// kind, a key given twice, a second document or a file past its size are
// each refused, with the line; what it declares is read as the forms take
// it.
func TestParseConfig(t *testing.T) {
	parallel(t)
	d, err := ParseConfig([]byte(configFile), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Teams) != 1 || d.Teams[0].Line != 2 || d.Teams[0].TZ != "Asia/Seoul" || d.Teams[0].Labels["team"][0] != "payments" {
		t.Errorf("teams: %+v", d.Teams)
	}
	if len(d.Windows) != 1 || d.Windows[0].Team != "payments" || !d.Windows[0].Start.Equal(time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)) || d.Windows[0].Every != "day" {
		t.Errorf("windows: %+v", d.Windows)
	}
	if len(d.Reports) != 1 || !d.Reports[0].Enabled || !slices.Equal(d.Reports[0].Sections, []string{"summary", "noise"}) {
		t.Errorf("reports: %+v", d.Reports)
	}
	if len(d.Grants) != 2 || d.Grants[0].Who != "group:payments-oncall" || d.Grants[1].Who != "user:ana@example.com" || d.Grants[1].Team != "" {
		t.Errorf("grants: %+v", d.Grants)
	}
	for _, c := range []struct{ body, want string }{
		{"teams:\n  - name: a\n    rule: ^A\n", `line 3: unknown key "rule" in a team`},
		{"team:\n  - name: a\n", `line 1: unknown key "team": want teams, windows, reports, grants, goals, marked, freezes, notify or expectations`},
		{"teams:\n  name: a\n", "line 2: teams is a list"},
		{"teams:\n  - name: [a]\n", "line 2: name is a text"},
		{"teams:\n  - name: a\n    name: b\n", "line 3: name is given twice, on line 2 too"},
		{"teams: []\n---\nteams: []\n", "line 2: one document only"},
		{"windows:\n  - title: w\n    start: tomorrow\n", `line 3: start "tomorrow": want a time`},
		{"reports:\n  - name: r\n    enabled: maybe\n", "line 3: enabled is true or false"},
		{"grants:\n  - user: a\n    group: b\n", "line 2: a role is given to a user or a group"},
		{"teams:\n  - rules: ^A\n", "line 2: a team has a name"},
		{"- a\n", "line 1: want a mapping"},
		{"teams: [\n", "isn't YAML"},
		// Nothing at all: an empty or half-written file, read as a
		// ConfigMap is swapped, isn't taken for "declare nothing".
		{"", "the file declares nothing"},
		{"# nothing\n", "the file declares nothing"},
		{"~\n", "the file declares nothing"},
		// A key with nothing after it, as a file cut short reads.
		{"teams:\n", "line 1: teams has nothing after it: give teams: [] for none"},
		{"teams: []\nwindows:\n", "line 2: windows has nothing after it"},
		// A value with nothing after it, as a file cut short reads: read as
		// nothing, a role's team would be every team, a window's rules
		// every rule.
		{"grants:\n  - user: mallory@example.com\n    team:\n", "line 3: team has nothing after it: leave the key out for every team"},
		{"windows:\n  - title: freeze\n    team: payments\n    rules:\n", "line 4: rules has nothing after it: leave the key out for every rule"},
		{"teams:\n  - name: payments\n    tz: ~\n", "line 3: tz has nothing after it: give it a value, or leave the key out"},
		// Or blanks, as much.
		{"grants:\n  - user: mallory@example.com\n    team: \"\"\n", "line 3: team has nothing after it: leave the key out for every team"},
		{"grants:\n  - user: mallory@example.com\n    team: ' '\n", "line 3: team has nothing after it: leave the key out for every team"},
		{"windows:\n  - title: freeze\n    team: \"\"\n", "line 3: team has nothing after it: leave the key out for every team"},
		{"windows:\n  - title: freeze\n    team: payments\n    rules: \"  \"\n", "line 4: rules has nothing after it: leave the key out for every rule"},
		// What Helm, reading values as YAML 1.1, gives as a boolean or a
		// number where a name is wanted.
		{"teams:\n  - name: false\n    rules: ^A\n", "line 2: name is false, read as a boolean or a number"},
		{"grants:\n  - user: 83\n", "line 2: user is 83, read as a boolean or a number"},
		{"windows:\n  - {title: 1.1, start: 2026-11-02T02:00, end: 2026-11-02T03:00}\n", "line 2: title is 1.1, read as a boolean or a number"},
		{"teams:\n  - {name: a, labels: {canary: true}}\n", "label canary's value true is read as a boolean or a number"},
		{"teams:\n  - {name: a, labels: {version: 1.10}}\n", "label version's value 1.10 is read as a boolean or a number"},
		// More than the ledger keeps, refused as it is read.
		{"windows:\n" + strings.Repeat("  - {title: w, start: 2026-11-02T02:00, end: 2026-11-02T03:00}\n", ledger.MaxWindows+1), "a config declares 500 quiet windows at most"},
	} {
		_, err := ParseConfig([]byte(c.body), time.UTC)
		if err == nil || !strings.Contains(configText("en", err), c.want) {
			t.Errorf("%q: %v, want %q", c.body, err, c.want)
		}
	}
	// Past a ConfigMap's size.
	big := filepath.Join(t.TempDir(), "big.yaml")
	os.WriteFile(big, []byte("teams: []\n"+strings.Repeat("# padding\n", maxConfig/10+1)), 0o644)
	if _, _, err := ReadConfig(big, time.UTC); err == nil || !strings.Contains(err.Error(), "more than 1048576 bytes") {
		t.Errorf("a file past the size: %v", err)
	}
	// Said to declare nothing, it does.
	for _, body := range []string{"{}\n", "teams: []\n", "teams: []\ngrants: []\n"} {
		if d, err := ParseConfig([]byte(body), time.UTC); err != nil || len(d.Teams)+len(d.Windows)+len(d.Reports)+len(d.Grants) != 0 {
			t.Errorf("%q: %+v %v", body, d, err)
		}
	}
	// Quoted, or a whole number for a label's value, it is a text; a team
	// renamed says what it was.
	d, err = ParseConfig([]byte("teams:\n  - {name: \"no\", renamed_from: 'off', rules: ^A, labels: {code: 500}}\ngrants:\n  - user: \"0123\"\n"), time.UTC)
	if err != nil || d.Teams[0].Name != "no" || d.Teams[0].RenamedFrom != "off" || d.Teams[0].Labels["code"][0] != "500" || d.Grants[0].Who != "user:0123" {
		t.Errorf("quoted: %+v %v", d, err)
	}
}

// teamOf is a team the ledger holds, by its name.
func teamOf(t *testing.T, s *Server, name string) ledger.Team {
	t.Helper()
	ts, _ := s.l.Teams(context.Background())
	for _, x := range ts {
		if x.Name == name {
			return x
		}
	}
	t.Fatalf("no team %s in %+v", name, ts)
	return ledger.Team{}
}

// The leader keeps the ledger to --config: when it starts, and when the
// file changes. A file that can't be read changes nothing; a thing in it
// that can't be kept is skipped, kept as it was, and the rest is kept. The
// pages say what, and why to an admin; put back, the last good file shows
// nothing wrong again.
func TestConfigReloaded(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, path := configServer(t, configFile, nil)
	if pay := teamOf(t, s, "payments"); !pay.Declared || pay.TZ != "Asia/Seoul" {
		t.Fatalf("payments: %+v", pay)
	}
	// payments made in the UI already (rolesServer), taken over.
	if c := s.configShown(); c == nil || c.Hash == "" || c.Applied.IsZero() || c.Did != (ledger.Reconciled{Made: 4, Adopted: 1}) {
		t.Errorf("shown: %+v", c)
	}
	// The same file read again writes nothing.
	gen := s.l.Gen()
	if err := s.reconcileConfig(ctx); err != nil || s.l.Gen() != gen {
		t.Errorf("the same file again: %v, generation %d → %d", err, gen, s.l.Gen())
	}
	berlin := strings.Replace(configFile, "tz: Asia/Seoul", "tz: Europe/Berlin", 1)
	writeConfig(t, path, berlin)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if teamOf(t, s, "payments").TZ != "Asia/Seoul" || s.configShown().Pending.IsZero() {
		t.Error("read once, the change is kept already")
	}
	if _, body := callAs(t, s.Handler(), "GET", "/settings", "boss@example.com", "", "", ""); !strings.Contains(body, "A new file was read at") {
		t.Error("settings doesn't say a new file was read once")
	}
	clockOn(s, configEvery)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if teamOf(t, s, "payments").TZ != "Europe/Berlin" || !s.configShown().Pending.IsZero() {
		t.Error("read twice, the change isn't kept")
	}
	h := s.Handler()
	for _, c := range []struct {
		body, want string
		whole      bool
	}{
		// Not readable as --config: nothing kept.
		{strings.Replace(berlin, "tz: Europe/Berlin", "tz: Asia/Seoul\n    color: red", 1), `unknown key "color" in a team`, true},
		// A team that can't be kept: kept as it was, and what names it too,
		// the window's change not kept.
		{strings.Replace(strings.Replace(berlin, "tz: Europe/Berlin", "tz: Mars/Olympus", 1), "end: 2026-11-02T03:00", "end: 2026-11-02T04:00", 1),
			`"Mars/Olympus" isn't a time zone`, false},
		// A window that can't be kept, the rest kept.
		{strings.Replace(strings.Replace(berlin, "every: day", "every: fortnight", 1), "dests: [team:payments]", "dests: [team:payments]\n    enabled: false", 1),
			`repeats every day, week or month`, false},
		// A report to nowhere: kept as it was.
		{strings.Replace(berlin, "dests: [team:payments]", "dests: [nowhere]", 1), `there is no destination called "nowhere"`, false},
	} {
		writeConfig(t, path, c.body)
		readTwice(t, s)
		if teamOf(t, s, "payments").TZ != "Europe/Berlin" {
			t.Errorf("%s: payments isn't kept as it was", c.want)
		}
		ws, _ := s.l.Windows(ctx, now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour))
		if ws[0].End.Sub(ws[0].Start) != time.Hour || ws[0].Every != "day" {
			t.Errorf("%s: the window isn't kept as it was: %+v", c.want, ws[0])
		}
		rs, _ := s.l.Scheduleds(ctx)
		if len(rs) != 1 || rs[0].Dests[0] != "team:payments" || rs[0].Enabled == strings.Contains(c.body, "enabled: false") {
			t.Errorf("%s: the report: %+v", c.want, rs)
		}
		shown := s.configShown()
		if !strings.Contains(shown.ErrText("en"), c.want) || shown.ErrText("ko") == shown.ErrText("en") || shown.Whole != c.whole {
			t.Errorf("shown: %q / %q, want %q, whole %v", shown.ErrText("en"), shown.ErrText("ko"), c.want, c.whole)
		}
		for _, p := range []string{"/settings", "/runs"} {
			code, body := callAs(t, h, "GET", p, "boss@example.com", "", "", "")
			if code != http.StatusOK || !strings.Contains(body, "lede") || !strings.Contains(body, htmlText(c.want)) {
				t.Errorf("%s doesn't say %q: %d", p, c.want, code)
			}
		}
		// Put back, after one that can't be read: nothing wrong is shown.
		writeConfig(t, path, berlin)
		readTwice(t, s)
		if e := s.configShown().ErrText("en"); e != "" {
			t.Errorf("%s: put back, it still says %q", c.want, e)
		}
	}
}

// A file that can't be read, then the one the ledger is kept to put back
// as it was: nothing is shown wrong any more.
func TestConfigPutBack(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, path := configServer(t, configFile, nil)
	writeConfig(t, path, "teams: [\n")
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if s.configShown().ErrText("en") == "" {
		t.Fatal("nothing said of a file that can't be read")
	}
	writeConfig(t, path, configFile) // the one applied
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if e := s.configShown().ErrText("en"); e != "" {
		t.Errorf("put back, it still says %q", e)
	}
}

// What can't be kept of --config is shown to an admin with why, which can
// name who holds a role or where a report goes; to anyone else, its line
// and kind alone, on the pages and in the API.
func TestConfigProblemsForAdmins(t *testing.T) {
	parallel(t)
	body := configFile + "  - user: mallory@example.com\n    team: nobody\n"
	s, _ := configServer(t, body, func(c *Config) { c.Viewers = []string{"*"} })
	h := s.Handler()
	for _, p := range []string{"/settings", "/runs", "/api/v1/settings"} {
		_, admin := callAs(t, h, "GET", p, "boss@example.com", "", "", "")
		_, viewer := callAs(t, h, "GET", p, "viewer@example.com", "", "", "")
		if !strings.Contains(admin, "mallory") || !strings.Contains(admin, "no team called nobody") {
			t.Errorf("%s: an admin isn't told why", p)
		}
		if strings.Contains(viewer, "mallory") || strings.Contains(viewer, "nobody") {
			t.Errorf("%s: a viewer is told whom: %.3000s", p, viewer)
		}
		if p != "/api/v1/settings" && !strings.Contains(viewer, "line 23: a role that can&#39;t be kept") {
			t.Errorf("%s: a viewer isn't told the line and kind:\n%.4000s", p, viewer)
		}
	}
}

// A bad file at start, as a pod restarted on it reads it: serve starts on
// the ledger as it is, and says why on its first read.
func TestConfigBadAtStart(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, "teams:\n  - name: payments\n    colour: red\n")
	s, _, _ := rolesServer(t, func(c *Config) { c.Config = path })
	if err := s.configTerm(ctx); err != nil {
		t.Fatal(err)
	}
	if c := s.configShown(); !c.Whole || !strings.Contains(c.ErrText("en"), "colour") || teamOf(t, s, "payments").Declared {
		t.Errorf("shown: %+v", c)
	}
}

// A replica that leads again keeps the ledger to the file, whatever an
// earlier term kept: another leader with an older copy may have since. As
// any file, it is read twice first: kept to at the term's first read
// again, configEvery after it begins.
func TestConfigNewTerm(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := configServer(t, configFile, nil)
	o, _ := s.options(ctx)
	if _, _, err := s.l.Reconcile(ctx, ledger.Declared{}, ledger.Leave{}, o); err != nil { // as another leader would
		t.Fatal(err)
	}
	if err := s.configTerm(ctx); err != nil {
		t.Fatal(err)
	}
	if teamOf(t, s, "payments").Declared {
		t.Error("kept to on the term's first read")
	}
	clockOn(s, configEvery)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !teamOf(t, s, "payments").Declared {
		t.Error("payments isn't declared once this replica leads again")
	}
}

// htmlText is a text as a page escapes it.
func htmlText(s string) string {
	return strings.NewReplacer(`"`, "&#34;", "'", "&#39;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// A thing that can't be kept for what the ledger lacks (a team it names
// that the UI hasn't made yet) is tried again once the ledger changed, not
// before a while: from configEvery, then twice as long each time.
func TestConfigTriedAgain(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	body := "windows:\n  - {title: freeze, team: platform, start: 2026-11-02T02:00, end: 2026-11-02T03:00}\n"
	s, _ := configServer(t, body, nil)
	if !strings.Contains(s.configShown().ErrText("en"), "no team called platform") {
		t.Fatalf("shown: %+v", s.configShown())
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "Platform", Rules: "^P"}, "boss"); err != nil {
		t.Fatal(err)
	}
	// At once: not tried again.
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.configShown().ErrText("en"), "no team called platform") {
		t.Error("tried again at once")
	}
	s.wall = func() time.Time { return time.Now().Add(time.Hour) }
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	ws, _ := s.l.Windows(ctx, now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour))
	if s.configShown().ErrText("en") != "" || len(ws) != 1 || !ws[0].Declared {
		t.Errorf("after the team was made: %q %+v", s.configShown().ErrText("en"), ws)
	}
}

// What --config declares is refused on every path that writes: the forms,
// the API, an admin's included; and MCP has no tool that writes it. The
// pages show it declared, its actions off, why beside them.
func TestDeclaredRefusedEveryPath(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := configServer(t, configFile, nil)
	h := s.Handler()
	ts, _ := s.l.Teams(ctx)
	var team ledger.Team
	for _, x := range ts {
		if x.Name == "payments" {
			team = x
		}
	}
	ws, _ := s.l.Windows(ctx, now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour))
	rs, _ := s.l.Scheduleds(ctx)
	gs, _ := s.l.Grants(ctx)
	if len(ws) != 1 || len(rs) != 1 || len(gs) != 2 {
		t.Fatalf("declared: %+v %+v %+v", ws, rs, gs)
	}
	w, r, g := itoa(ws[0].ID), itoa(rs[0].ID), itoa(gs[0].ID)
	form := func(kv ...string) string {
		v := url.Values{}
		for i := 0; i+1 < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v.Encode()
	}
	for _, c := range []struct{ method, path, body string }{
		// The forms.
		{"POST", "/teams", form("id", itoa(team.ID), "name", "payments", "rules", "^Pay")},
		{"POST", "/teams/" + itoa(team.ID) + "/delete", ""},
		{"POST", "/windows", form("id", w, "title", "changed", "start", "2026-11-02T02:00", "end", "2026-11-02T03:00")},
		{"POST", "/windows/" + w + "/delete", ""},
		{"POST", "/reports", form("id", r, "name", "payments weekly", "schedule", "daily@09:00", "dests", "team:payments", "team", itoa(team.ID))},
		{"POST", "/reports/" + r + "/delete", ""},
		{"POST", "/roles/" + g + "/delete", ""},
		// The API.
		{"PUT", "/api/v1/teams/" + itoa(team.ID), `{"name":"payments","rules":"^Pay"}`},
		{"DELETE", "/api/v1/teams/" + itoa(team.ID), ""},
		{"PUT", "/api/v1/windows/" + w, `{"title":"changed","start":"2026-11-02T02:00:00Z","end":"2026-11-02T03:00:00Z"}`},
		{"DELETE", "/api/v1/windows/" + w, ""},
		{"PUT", "/api/v1/reports/" + r, `{"name":"payments weekly","schedule":"daily@09:00","dests":["team:payments"],"team_id":` + itoa(team.ID) + `}`},
		{"DELETE", "/api/v1/reports/" + r, ""},
		{"DELETE", "/api/v1/roles/" + g, ""},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		if strings.HasPrefix(c.path, "/api/") {
			req.Header.Set("Content-Type", "application/json")
		} else {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("X-Forwarded-Email", "boss@example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "declared in config") {
			t.Errorf("%s %s: %d %.300s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	// As they were.
	again, _ := s.l.Teams(ctx)
	if len(again) != len(ts) || slices.ContainsFunc(again, func(x ledger.Team) bool { return x.Name == "payments" && x.Rules != "^Errors$" }) {
		t.Errorf("teams: %+v", again)
	}
	if ws2, _ := s.l.Windows(ctx, now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour)); len(ws2) != 1 || ws2[0].Title != "Nightly backup" {
		t.Errorf("windows: %+v", ws2)
	}
	if rs2, _ := s.l.Scheduleds(ctx); len(rs2) != 1 || rs2[0].Schedule != "weekly mon@09:00" {
		t.Errorf("reports: %+v", rs2)
	}
	if gs2, _ := s.l.Grants(ctx); len(gs2) != 2 {
		t.Errorf("roles: %+v", gs2)
	}

	// MCP: no tool writes a team, a quiet window, a report or a role. Those
	// that write are proposals' and drafts' alone.
	srv := httptest.NewServer(h)
	defer srv.Close()
	cs := connect(t, srv.URL, "boss@example.com")
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range res.Tools {
		if (x.Annotations == nil || !x.Annotations.ReadOnlyHint) && x.Name != "propose" && x.Name != "backtest_again" && x.Name != "propose_expectation" {
			t.Errorf("MCP tool %s writes", x.Name)
		}
	}

	// The pages: declared, its actions off, why beside them.
	for _, p := range []string{"/teams", "/teams?edit=" + itoa(team.ID), "/windows", "/windows/" + w, "/reports", "/reports/" + r, "/roles"} {
		code, body := callAs(t, h, "GET", p, "boss@example.com", "", "", "")
		if code != http.StatusOK || !strings.Contains(body, `class="badge declared status-unknown"`) {
			t.Errorf("%s: %d, no declared badge", p, code)
		}
		if p != "/teams" && p != "/windows" && p != "/reports" && (!strings.Contains(body, "Declared in config — change it in values") || !strings.Contains(body, " disabled ")) {
			t.Errorf("%s: no action off with why", p)
		}
	}
}

// Without --config, what a file declared before is the UI's again. With
// --config-prune, what the file no longer declares is deleted once it is
// read the same twice in a row: an empty file deletes nothing, nor a
// half-written one read once.
func TestConfigTakenAway(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, path := configServer(t, configFile, nil)
	s.cfg.Config = ""
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	ts, _ := s.l.Teams(ctx)
	if slices.ContainsFunc(ts, func(x ledger.Team) bool { return x.Declared }) || len(ts) != 2 {
		t.Errorf("without --config: %+v", ts)
	}
	s.cfg.Config, s.cfg.ConfigPrune = path, true
	if err := s.configTerm(ctx); err != nil {
		t.Fatal(err)
	}
	clockOn(s, configEvery)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		ts, _ := s.l.Teams(ctx)
		ws, _ := s.l.Windows(ctx, now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour))
		rs, _ := s.l.Scheduleds(ctx)
		gs, _ := s.l.Grants(ctx)
		return len(ts) + len(ws) + len(rs) + len(gs)
	}
	all := count()
	// Empty, then half written, each read once: nothing deleted.
	for _, body := range []string{"", configFile[:strings.Index(configFile, "windows:")]} {
		writeConfig(t, path, body)
		clockOn(s, configEvery)
		if err := s.reconcileConfig(ctx); err != nil {
			t.Fatal(err)
		}
		if n := count(); n != all {
			t.Errorf("%q read once: %d of %d left", body, n, all)
		}
	}
	// Whole again, then teams: [] read twice: deleted the second time.
	writeConfig(t, path, configFile)
	clockOn(s, configEvery)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, path, "teams: []\n")
	for i := 1; i <= 2; i++ {
		clockOn(s, configEvery)
		if err := s.reconcileConfig(ctx); err != nil {
			t.Fatal(err)
		}
		if gone := count() < all; gone != (i == 2) {
			t.Errorf("read %d times: %d of %d left", i, count(), all)
		}
	}
	ts, _ = s.l.Teams(ctx)
	rs, _ := s.l.Scheduleds(ctx)
	if slices.ContainsFunc(ts, func(x ledger.Team) bool { return x.Name == "payments" }) || len(rs) != 0 {
		t.Errorf("pruned: %+v %+v", ts, rs)
	}
	cs, _ := s.l.Changes(ctx, 100)
	if !slices.ContainsFunc(cs, func(c ledger.Change) bool { return c.By == ledger.ConfigBy && c.What == "team payments deleted" }) {
		t.Errorf("the activity log: %+v", cs)
	}
}

// GET /config.yaml is an admin's: what the ledger holds, declared and made
// in the UI alike, as --config takes it. Read back as --config, it makes,
// changes and deletes nothing, takes over what the UI made, and gives the
// same file again; read once more, it writes nothing at all.
func TestConfigExportRoundTrip(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, path := configServer(t, configFile, func(c *Config) { c.Loc, _ = time.LoadLocation("Asia/Seoul") })
	if s.cfg.Loc == nil {
		t.Skip("no Asia/Seoul")
	}
	o := s.cfg.Options
	ts, _ := s.l.Teams(ctx)
	var search int64
	for _, x := range ts {
		if x.Name == "search" {
			search = x.ID
		}
	}
	// Made in the UI: a team's hours, a window every month until a day,
	// one by cron with labels of two values, a report off, roles.
	if _, err := s.l.PutTeam(ctx, ledger.Team{ID: search, Name: "search", Rules: "^Search", WorkHours: "mon-fri@09:00-18:00", Note: "owns: search"}, "boss"); err != nil {
		t.Fatal(err)
	}
	kst := s.cfg.Loc
	for _, w := range []ledger.Window{
		{Title: "patching", Start: time.Date(2026, 11, 7, 22, 0, 0, 0, kst), End: time.Date(2026, 11, 8, 1, 0, 0, 0, kst), Every: "month", On: "first sat",
			Until: s.l.Midnight(s.l.Day(time.Date(2027, 6, 30, 12, 0, 0, 0, kst))), TeamID: search},
		{Title: "load test", Start: time.Date(2026, 11, 3, 2, 0, 0, 0, kst), End: time.Date(2026, 11, 3, 2, 30, 0, 0, kst), Every: "cron", On: "0 2 * * 2",
			Rules: "^Search", Labels: map[string][]string{"env": {"prod", "stage"}, "service": {"search"}}},
		{Title: "once", Start: time.Date(2026, 10, 1, 9, 0, 0, 0, kst), End: time.Date(2026, 10, 1, 9, 30, 15, 0, kst)},
	} {
		if _, err := s.l.PutWindow(ctx, w, "boss", o); err != nil {
			t.Fatalf("%s: %v", w.Title, err)
		}
	}
	if _, err := s.l.PutScheduled(ctx, ledger.Scheduled{Name: "search daily", TeamID: search, Zone: "team", Schedule: "0 9 * * 1-5", Span: "24h",
		Sections: []string{"noise", "summary"}, Dests: []string{"team:search"}}, "boss", now); err != nil {
		t.Fatal(err)
	}
	for _, g := range []ledger.Grant{{Who: "user:pat@example.com", TeamID: search}, {Who: "group:sre"}} {
		if _, err := s.l.PutGrant(ctx, g, "boss"); err != nil {
			t.Fatal(err)
		}
	}
	h := s.Handler()
	if code, _ := callAs(t, h, "GET", "/config.yaml", "ed@example.com", "", "", ""); code != http.StatusForbidden {
		t.Errorf("an editor reads /config.yaml: %d", code)
	}
	code, first := callAs(t, h, "GET", "/config.yaml", "boss@example.com", "", "", "")
	if code != http.StatusOK || !strings.Contains(first, "name: search") || !strings.Contains(first, "group: sre") {
		t.Fatalf("/config.yaml: %d\n%s", code, first)
	}
	before, _ := s.l.AllDeclarable(ctx)
	writeConfig(t, path, first)
	gen := s.l.Gen()
	readTwice(t, s)
	if e := s.configShown().ErrText("en"); e != "" {
		t.Fatalf("the export read back: %s\n%s", e, first)
	}
	did := s.configShown().Did
	if did.Made+did.Changed+did.Deleted+did.Undeclared != 0 || did.Adopted == 0 || s.l.Gen() == gen {
		t.Errorf("the export read back: %+v", did)
	}
	after, _ := s.l.AllDeclarable(ctx)
	if len(after.Teams) != len(before.Teams) || len(after.Windows) != len(before.Windows) || len(after.Reports) != len(before.Reports) || len(after.Grants) != len(before.Grants) {
		t.Errorf("the ledger after: %+v\nbefore: %+v", after, before)
	}
	for i := range after.Windows {
		if !after.Windows[i].Declared || after.Windows[i].ID != before.Windows[i].ID || after.Windows[i].By != before.Windows[i].By {
			t.Errorf("window %d: %+v, was %+v", i, after.Windows[i], before.Windows[i])
		}
	}
	_, second := callAs(t, h, "GET", "/config.yaml", "boss@example.com", "", "", "")
	if second != first {
		t.Errorf("exported again:\n%s\nwas:\n%s", second, first)
	}
	// Read once more, as a term begins: nothing at all.
	s.config.newTerm(s.wall())
	gen = s.l.Gen()
	readTwice(t, s)
	if s.l.Gen() != gen || s.config.applied == "" {
		cs, _ := s.l.Changes(ctx, 5)
		t.Errorf("once more: generation %d → %d: %+v %+v", gen, s.l.Gen(), s.configShown().Did, cs)
	}
}

// A team's name is unique in any letter case, from the forms and the API.
func TestTeamNameAnyCase(t *testing.T) {
	parallel(t)
	s, _, _ := rolesServer(t, nil)
	h := s.Handler()
	code, body := callAs(t, h, "POST", "/api/v1/teams", "boss@example.com", "", `{"name":"PAYMENTS","rules":"^P"}`, "same-origin")
	if code != http.StatusBadRequest || !strings.Contains(body, "unique in any letter case") {
		t.Errorf("a team called PAYMENTS beside payments: %d %s", code, body)
	}
	_, err := s.l.PutTeam(context.Background(), ledger.Team{Name: "Search", Rules: "^S"}, "boss")
	var inv ledger.ErrInvalid
	if !errors.As(err, &inv) {
		t.Errorf("Search beside search: %v", err)
	}
}

// A name in an export's comment is quoted, its line breaks escaped: a
// quiet window's title kept from before titles were one line, by a team's
// editor whose team is gone since, doesn't end the comment, and pasted back
// as --config declares no role. In the body, YAML quotes it; read back, it
// is skipped, as the ledger refuses such a title now.
func TestExportHostileNames(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	hostile := "x\ngrants:\n- {user: mallory@example.com}\n#"
	start := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	var d ledger.Declaring
	for _, title := range []string{hostile, "y\rgrants:\r- {user: mallory@example.com}\r#", "z grants: - {user: mallory@example.com}", "w\u0085- {user: mallory}"} {
		d.Windows = append(d.Windows, ledger.Window{Title: title, Start: start, End: start.Add(time.Hour), TeamID: -3}, // its team removed
			ledger.Window{Title: title, Start: start, End: start.Add(time.Hour)})
	}
	out := exportConfig(d, time.UTC)
	got, err := ParseConfig(out, time.UTC)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(got.Grants) != 0 || len(got.Windows) != 4 || got.Windows[0].Title != hostile {
		t.Errorf("pasted back: %+v\n%s", got, out)
	}
	// The comment is one line, each title in it quoted; the body holds
	// them as block scalars YAML reads as text.
	var left string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "# Left out") {
			left = line
		}
		if strings.HasPrefix(line, "grants:") {
			t.Errorf("a line of its own: %q\n%s", line, out)
		}
	}
	for _, want := range []string{`"x\ngrants:\n- {user: mallory@example.com}\n#"`, `"y\rgrants:`, `"z\u2028grants:`, `"w\u0085-`} {
		if !strings.Contains(left, want) {
			t.Errorf("the comment hasn't %s: %q", want, left)
		}
	}
	// Read back, each such title is skipped, and no role is made.
	s, path := configServer(t, configFile, nil)
	writeConfig(t, path, string(out))
	readTwice(t, s)
	gs, _ := s.l.Grants(ctx)
	if slices.ContainsFunc(gs, func(g ledger.Grant) bool { return strings.Contains(g.Who, "mallory") }) {
		t.Errorf("mallory was given a role: %+v", gs)
	}
	if c := s.configShown(); len(c.Problems) != 4 || c.Problems[0].Kind != ledger.KindWindow {
		t.Errorf("shown: %+v", c)
	}
}

// A team of the UI's that something declared names isn't renamed, from the
// forms or the API: the file would name a team that isn't there. Changed
// but for its name, it is.
func TestRenameNamedTeam(t *testing.T) {
	parallel(t)
	body := "windows:\n  - {title: freeze, team: search, start: 2026-11-02T02:00, end: 2026-11-02T03:00}\n"
	s, _ := configServer(t, body, nil)
	if e := s.configShown().ErrText("en"); e != "" {
		t.Fatal(e)
	}
	search := teamOf(t, s, "search")
	h := s.Handler()
	code, out := callAs(t, h, "PUT", "/api/v1/teams/"+itoa(search.ID), "boss@example.com", "", `{"name":"searching","rules":"^Search"}`, "same-origin")
	if code != http.StatusConflict || !strings.Contains(out, "declared in config names this team") {
		t.Errorf("renamed through the API: %d %s", code, out)
	}
	r := httptest.NewRequest("POST", "/teams", strings.NewReader(url.Values{"id": {itoa(search.ID)}, "name": {"searching"}, "rules": {"^Search"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("X-Forwarded-Email", "boss@example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Errorf("renamed through the form: %d", w.Code)
	}
	if code, out := callAs(t, h, "PUT", "/api/v1/teams/"+itoa(search.ID), "boss@example.com", "", `{"name":"search","rules":"^Search","note":"ours"}`, "same-origin"); code != http.StatusOK {
		t.Errorf("changed but for its name: %d %s", code, out)
	}
	if teamOf(t, s, "search").Note != "ours" {
		t.Error("the note isn't kept")
	}
}

// A role the file no longer gives is taken away, --config-prune or not,
// one the UI made and the file took over among them, once the file is
// read the same twice in a row, configEvery apart.
func TestConfigGrantRevoked(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, path := configServer(t, configFile, nil)
	writeConfig(t, path, strings.Replace(configFile, "  - user: ana@example.com\n", "", 1))
	// Read once, and again at once: kept, as a file read half written
	// takes no one's role.
	for range 2 {
		if err := s.reconcileConfig(ctx); err != nil {
			t.Fatal(err)
		}
		if gs, _ := s.l.Grants(ctx); !slices.ContainsFunc(gs, func(g ledger.Grant) bool { return g.Who == "user:ana@example.com" && g.Declared }) {
			t.Errorf("read once, ana's role: %+v", gs)
		}
	}
	clockOn(s, configEvery)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	gs, _ := s.l.Grants(ctx)
	cs, _ := s.l.Changes(ctx, 50)
	if slices.ContainsFunc(gs, func(g ledger.Grant) bool { return g.Who == "user:ana@example.com" }) ||
		!slices.ContainsFunc(cs, func(c ledger.Change) bool {
			return c.By == ledger.ConfigBy && c.What == ledger.RoleTaken+"user:ana@example.com"
		}) {
		t.Errorf("ana's role: %+v", gs)
	}
}

// With --config-prune, a file read, then one that can't be read, then the
// first again, hasn't been read twice in a row: nothing is deleted until
// it is read once more.
func TestConfigPruneReadTwiceInARow(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, path := configServer(t, configFile, func(c *Config) { c.ConfigPrune = true })
	short := configFile[:strings.Index(configFile, "reports:")]
	writeConfig(t, path, short)
	read := func(body string) int {
		t.Helper()
		writeConfig(t, path, body)
		clockOn(s, configEvery)
		if err := s.reconcileConfig(ctx); err != nil {
			t.Fatal(err)
		}
		rs, _ := s.l.Scheduleds(ctx)
		return len(rs)
	}
	if n := read(short); n != 1 {
		t.Fatalf("read once: %d reports", n)
	}
	if n := read("teams: [\n"); n != 1 {
		t.Fatalf("then a file that can't be read: %d reports", n)
	}
	if n := read(short); n != 1 {
		t.Errorf("the first again, after one that couldn't be read: %d reports, want it kept", n)
	}
	if n := read(short); n != 0 {
		t.Errorf("read twice in a row: %d reports, want none", n)
	}
}

// A new file is tried again from configEvery, however long the last waited;
// an error reading the settings is tried again after a while too.
func TestConfigBackoff(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	bad := "windows:\n  - {title: freeze, team: platform, start: 2026-11-02T02:00, end: 2026-11-02T03:00}\n"
	s, path := configServer(t, bad, nil)
	at := time.Now()
	for range 4 {
		at = at.Add(time.Hour)
		s.wall = func() time.Time { return at }
		if _, err := s.l.PutTeam(ctx, ledger.Team{Name: fmt.Sprint("t", at.Unix()), Rules: "^T"}, "boss"); err != nil {
			t.Fatal(err)
		}
		if err := s.reconcileConfig(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if s.config.backoff <= configEvery {
		t.Fatalf("tried again four times, backoff %s", s.config.backoff)
	}
	writeConfig(t, path, bad+"  - {title: other, team: nowhere, start: 2026-11-02T02:00, end: 2026-11-02T03:00}\n")
	readTwice(t, s)
	if s.config.backoff != configEvery {
		t.Errorf("a new file waits %s, want %s", s.config.backoff, configEvery)
	}
	// The settings can't be read: tried again after a while, not at once.
	writeConfig(t, path, configFile)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	clockOn(s, configEvery)
	s.Close()
	if err := s.reconcileConfig(ctx); err == nil {
		t.Fatal("no error from a closed ledger")
	}
	if !s.config.failAt.After(s.wall()) {
		t.Errorf("not tried again after a while: %v", s.config.failAt)
	}
}

// An error of the ledger's holds off the file it met for a while, even
// read the same twice, when it would delete what it no longer names, or
// put back after one that couldn't be read.
func TestConfigErrorBacksOff(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, path := configServer(t, configFile, func(c *Config) { c.ConfigPrune = true })
	writeConfig(t, path, strings.Replace(configFile, "  - user: ana@example.com\n", "", 1))
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if s.config.pending == "" {
		t.Fatal("not noted, read once")
	}
	clockOn(s, configEvery)
	s.Close() // the ledger fails every write
	if err := s.reconcileConfig(ctx); err == nil {
		t.Fatal("read twice, not tried")
	}
	for range 3 {
		if err := s.reconcileConfig(ctx); err != nil {
			t.Errorf("tried again at once: %v", err)
		}
	}
	clockOn(s, time.Hour)
	if err := s.reconcileConfig(ctx); err == nil {
		t.Error("not tried again after a while")
	}
}

// Without --config, what a file declared before is the UI's again, roles
// included, none taken away, whatever the UI holds: a chart rolled back to
// before --config takes no one's role away. Settings says how many, and
// to an admin which.
func TestConfigNoFileGivesBack(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := configServer(t, configFile, func(c *Config) { c.Viewers = []string{"*"} })
	s.cfg.Config = ""
	if err := s.configTerm(ctx); err != nil {
		t.Fatal(err)
	}
	gs, _ := s.l.Grants(ctx)
	if len(gs) != 2 || slices.ContainsFunc(gs, func(g ledger.Grant) bool { return g.Declared }) {
		t.Errorf("roles without --config: %+v", gs)
	}
	cs, _ := s.l.Changes(ctx, 50)
	if slices.ContainsFunc(cs, func(c ledger.Change) bool { return strings.HasPrefix(c.What, ledger.RoleTaken) }) {
		t.Error("a role taken away without --config")
	}
	h := s.Handler()
	_, admin := callAs(t, h, "GET", "/settings", "boss@example.com", "", "", "")
	_, viewer := callAs(t, h, "GET", "/settings", "viewer@example.com", "", "", "")
	for who, body := range map[string]string{"an admin": admin, "a viewer": viewer} {
		if !strings.Contains(body, "5 of what a file declared were given back to the UI") {
			t.Errorf("%s isn't told how many were given back", who)
		}
	}
	if !strings.Contains(admin, "user:ana@example.com") || strings.Contains(viewer, "ana@example.com") {
		t.Errorf("whom a role was given to: to an admin %v, to a viewer %v", strings.Contains(admin, "ana@"), strings.Contains(viewer, "ana@"))
	}
}

// A new file is kept to only once it is read the same twice in a row,
// configEvery apart: a file cut short after a role's holder, read once,
// gives no one a role, of every team or of any, nor does one read again
// too soon; another between makes two reads not in a row.
func TestConfigKeptOnlyReadTwice(t *testing.T) {
	ctx := context.Background()
	s, path := configServer(t, configFile, nil)
	mallory := func() []ledger.Grant {
		gs, _ := s.l.Grants(ctx)
		return slices.DeleteFunc(gs, func(g ledger.Grant) bool { return !strings.Contains(g.Who, "mallory") })
	}
	cut := configFile + "  - user: mallory@example.com\n"
	whole := cut + "    team: payments\n"
	read := func(body string, after time.Duration) {
		t.Helper()
		writeConfig(t, path, body)
		clockOn(s, after)
		if err := s.reconcileConfig(ctx); err != nil {
			t.Fatal(err)
		}
	}
	read(cut, 0)
	read(cut, configEvery-time.Second) // too soon
	if gs := mallory(); len(gs) != 0 {
		t.Errorf("cut short, read once and again too soon: %+v", gs)
	}
	read(whole, configEvery)
	read(cut, configEvery)
	read(whole, configEvery)
	if gs := mallory(); len(gs) != 0 {
		t.Errorf("whole, cut short, whole: %+v", gs)
	}
	read(whole, configEvery)
	if gs := mallory(); len(gs) != 1 || gs[0].TeamID == 0 {
		t.Errorf("whole, read twice in a row: %+v", gs)
	}
}

// An error of the ledger's between two reads of a file makes them not in a
// row: kept to one file, another read twice that the ledger fails to keep
// to, then the first again, takes no role away; the other is kept to only
// once it is read twice in a row again.
func TestConfigErrorBetweenReads(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, path := configServer(t, configFile, nil)
	ana := func() bool {
		gs, _ := s.l.Grants(ctx)
		return slices.ContainsFunc(gs, func(g ledger.Grant) bool { return g.Who == "user:ana@example.com" })
	}
	without := strings.Replace(configFile, "  - user: ana@example.com\n", "", 1)
	writeConfig(t, path, without)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	clockOn(s, configEvery)
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.reconcileConfig(gone); err == nil {
		t.Fatal("no error from the ledger")
	}
	writeConfig(t, path, configFile)
	clockOn(s, configEvery)
	if err := s.reconcileConfig(ctx); err != nil || !ana() {
		t.Fatalf("the first again: %v, ana's role %v", err, ana())
	}
	writeConfig(t, path, without)
	clockOn(s, time.Hour) // past what the error waits
	if err := s.reconcileConfig(ctx); err != nil || !ana() {
		t.Errorf("the other, read once again: %v, ana's role %v", err, ana())
	}
	clockOn(s, configEvery)
	if err := s.reconcileConfig(ctx); err != nil || ana() {
		t.Errorf("read twice in a row: %v, ana's role %v", err, ana())
	}
}

// A team a full ledger makes only once others are let go, in the same
// write, is made after what names it was read: that is tried again, not
// left until the ledger changes for another reason.
func TestConfigDeferredTeamTriedAgain(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, path := configServer(t, "teams: []\n", nil)
	for i := range ledger.MaxTeams - 2 { // payments and search are the UI's
		if _, err := s.l.PutTeam(ctx, ledger.Team{Name: fmt.Sprint("ui", i), Rules: "^U"}, "boss"); err != nil {
			t.Fatal(err)
		}
	}
	file := func(from int, also string) string {
		var b strings.Builder
		b.WriteString("teams:\n")
		for i := from; i < ledger.MaxDeclaredTeams; i++ {
			fmt.Fprintf(&b, "  - {name: d%d, rules: ^D}\n", i)
		}
		return b.String() + also
	}
	writeConfig(t, path, file(0, ""))
	readTwice(t, s)
	// One let go for one made, and a window of it.
	writeConfig(t, path, file(1, "  - {name: fresh, rules: ^F}\nwindows:\n  - {title: freeze, team: fresh, start: 2026-11-02T02:00, end: 2026-11-02T03:00}\n"))
	readTwice(t, s)
	if !strings.Contains(s.configShown().ErrText("en"), "no team called fresh") {
		t.Fatalf("shown: %q", s.configShown().ErrText("en"))
	}
	clockOn(s, configEvery)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	ws, _ := s.l.Windows(ctx, now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour))
	if len(ws) != 1 || !ws[0].Declared || s.configShown().ErrText("en") != "" {
		t.Errorf("tried again: %+v, %q", ws, s.configShown().ErrText("en"))
	}
}

// A role the file took out, read once, then a leader without --config,
// then --config again: the role is taken away once the file is read twice,
// not left the UI's for good.
func TestConfigRolledBackAndForward(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, path := configServer(t, configFile, nil)
	writeConfig(t, path, strings.Replace(configFile, "  - user: ana@example.com\n", "", 1))
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	// Rolled back past --config.
	s.cfg.Config = ""
	if err := s.configTerm(ctx); err != nil {
		t.Fatal(err)
	}
	gs, _ := s.l.Grants(ctx)
	if !slices.ContainsFunc(gs, func(g ledger.Grant) bool { return g.Who == "user:ana@example.com" && !g.Declared }) {
		t.Fatalf("without --config, ana's role: %+v", gs)
	}
	// And forward again.
	s.cfg.Config = path
	if err := s.configTerm(ctx); err != nil {
		t.Fatal(err)
	}
	clockOn(s, configEvery)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	gs, _ = s.l.Grants(ctx)
	if slices.ContainsFunc(gs, func(g ledger.Grant) bool { return g.Who == "user:ana@example.com" }) || len(gs) != 1 || !gs[0].Declared {
		t.Errorf("forward again, the roles: %+v", gs)
	}
}

// A read of another file while one waits out an error of the ledger's
// makes two reads of a third not in a row.
func TestConfigBackoffBreaksTheRow(t *testing.T) {
	ctx := context.Background()
	s, path := configServer(t, configFile, nil)
	mallory := func() bool {
		gs, _ := s.l.Grants(ctx)
		return slices.ContainsFunc(gs, func(g ledger.Grant) bool { return strings.Contains(g.Who, "mallory") })
	}
	b := strings.Replace(configFile, "  - user: ana@example.com\n", "", 1)
	a := configFile + "  - user: mallory@example.com\n    team: payments\n"
	read := func(body string, after time.Duration) {
		t.Helper()
		writeConfig(t, path, body)
		clockOn(s, after)
		if err := s.reconcileConfig(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// B read twice, the ledger failing.
	read(b, 0)
	clockOn(s, configEvery)
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.reconcileConfig(gone); err == nil {
		t.Fatal("no error from the ledger")
	}
	// A read once, B while it waits, A again.
	read(a, time.Second)
	read(b, time.Second)
	read(a, configEvery)
	if mallory() {
		t.Error("A kept to, read with B between")
	}
	read(a, configEvery)
	if !mallory() {
		t.Error("A read twice in a row isn't kept to")
	}
}

// A file the ledger couldn't be kept to is shown so, with when it is tried
// again, not as one read once; kept to after, it is said no more.
func TestConfigWriteFailureShown(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, path := configServer(t, configFile, nil)
	writeConfig(t, path, strings.Replace(configFile, "  - user: ana@example.com\n", "", 1))
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	clockOn(s, configEvery)
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.reconcileConfig(gone); err == nil {
		t.Fatal("no error from the ledger")
	}
	if c := s.configShown(); c.WriteFailed.IsZero() || !c.Retry.After(c.WriteFailed) || !c.Pending.IsZero() {
		t.Errorf("shown: %+v", c)
	}
	_, body := callAs(t, s.Handler(), "GET", "/settings", "boss@example.com", "", "", "")
	if !strings.Contains(body, "couldn&#39;t be kept to it at") || strings.Contains(body, "A new file was read at") {
		t.Error("settings doesn't say the ledger couldn't be kept to it")
	}
	clockOn(s, time.Hour)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if c := s.configShown(); !c.WriteFailed.IsZero() || c.Applied.IsZero() {
		t.Errorf("kept to after: %+v", c)
	}
}

// Files other than the one applied, read for minutes, none the same twice
// in a row (a ConfigMap changing back and forth): Settings says since
// when, it is logged once, and the one applied read twice in a row ends
// it.
func TestConfigStuckShown(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, path := configServer(t, configFile, nil)
	var logged bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	other := strings.Replace(configFile, "tz: Asia/Seoul", "tz: Europe/Berlin", 1)
	read := func(body string) {
		t.Helper()
		writeConfig(t, path, body)
		clockOn(s, configEvery)
		if err := s.reconcileConfig(ctx); err != nil {
			t.Fatal(err)
		}
	}
	read(other)
	since := s.wall()
	for i := range 2 * int(configStuck/configEvery) {
		if i%2 == 0 {
			read(configFile)
		} else {
			read(other)
		}
	}
	if c := s.configShown(); !c.PendingSince.Equal(since) || teamOf(t, s, "payments").TZ != "Asia/Seoul" {
		t.Errorf("shown: %+v", c)
	}
	if n := strings.Count(logged.String(), "no file has been read the same twice in a row since"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, logged.String())
	}
	if _, body := callAs(t, s.Handler(), "GET", "/settings", "boss@example.com", "", "", ""); !strings.Contains(body, "have been read since") {
		t.Error("settings doesn't say since when")
	}
	read(configFile)
	read(configFile)
	if c := s.configShown(); !c.PendingSince.IsZero() {
		t.Errorf("the one applied, read twice: %+v", c)
	}
}

// Terms too short to read a file twice keep the ledger from it: that is
// shown since the first of them, and logged.
func TestConfigShortTermsShown(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, path := configServer(t, configFile, nil)
	var logged bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	writeConfig(t, path, strings.Replace(configFile, "tz: Asia/Seoul", "tz: Europe/Berlin", 1))
	var since time.Time
	for i := range int(configStuck/(5*time.Second)) + 2 {
		if err := s.configTerm(ctx); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			since = s.wall()
		}
		clockOn(s, 5*time.Second)
	}
	if c := s.configShown(); !c.PendingSince.Equal(since) || teamOf(t, s, "payments").TZ != "Asia/Seoul" {
		t.Errorf("shown: %+v, since %v", c, since)
	}
	if n := strings.Count(logged.String(), "no file has been read the same twice in a row since"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, logged.String())
	}
}

// What no --config gave back is an admin's to keep as the UI's, one or all,
// from Settings, which says how, a role's too; no one else's, nor a page
// elsewhere's.
func TestKeepGivenBackOnSettings(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := configServer(t, configFile, func(c *Config) { c.Viewers = []string{"*"} })
	s.cfg.Config = ""
	if err := s.configTerm(ctx); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	_, admin := callAs(t, h, "GET", "/settings", "boss@example.com", "", "", "")
	_, viewer := callAs(t, h, "GET", "/settings", "viewer@example.com", "", "", "")
	if !strings.Contains(admin, `action="/settings/given-back"`) || !strings.Contains(admin, "keep it here, or delete it on the Roles page and give it again") {
		t.Error("an admin isn't shown how to keep them")
	}
	if strings.Contains(viewer, `action="/settings/given-back"`) || !strings.Contains(viewer, `disabled aria-describedby="givenback-why"`) {
		t.Error("a viewer is shown the form, or not why it is off")
	}
	gb, _ := s.l.GivenBack(ctx)
	ana := gb[slices.IndexFunc(gb, func(g ledger.GivenBackRow) bool { return g.Name == "user:ana@example.com" })]
	one := url.Values{"kind": {ana.Kind}, "id": {itoa(ana.ID)}}
	if code, _, _ := ui(t, h, "POST", "/settings/given-back", "viewer@example.com", "same-origin", one); code != http.StatusForbidden {
		t.Errorf("a viewer keeps one: %d", code)
	}
	if code, _, _ := ui(t, h, "POST", "/settings/given-back", "boss@example.com", "cross-site", one); code != http.StatusForbidden {
		t.Errorf("a page elsewhere keeps one: %d", code)
	}
	if code, _, _ := ui(t, h, "POST", "/settings/given-back", "boss@example.com", "same-origin", one); code != http.StatusSeeOther {
		t.Errorf("an admin keeps one: %d", code)
	}
	if left, _ := s.l.GivenBack(ctx); len(left) != len(gb)-1 {
		t.Errorf("after one: %+v", left)
	}
	if code, _, _ := ui(t, h, "POST", "/settings/given-back", "boss@example.com", "same-origin", url.Values{"all": {"1"}}); code != http.StatusSeeOther {
		t.Errorf("an admin keeps them all: %d", code)
	}
	if left, _ := s.l.GivenBack(ctx); len(left) != 0 {
		t.Errorf("after all: %+v", left)
	}
	cs, _ := s.l.Changes(ctx, 50)
	if !slices.ContainsFunc(cs, func(c ledger.Change) bool {
		return c.By == "boss@example.com" && c.What == ledger.RoleGiven+"user:ana@example.com kept as the UI's: given back without --config, it is no file's to take away"
	}) {
		t.Errorf("the activity log: %+v", cs)
	}
}

// A file read twice that the ledger keeps failing to keep to, for longer
// than files are waited for, isn't said to be one never read twice in a
// row: not on Settings, beside its write's failure, nor in the log.
func TestConfigFailingNotStuck(t *testing.T) {
	ctx := context.Background()
	s, path := configServer(t, configFile, nil)
	var logged bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	writeConfig(t, path, strings.Replace(configFile, "  - user: ana@example.com\n", "", 1))
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	for range 8 {
		clockOn(s, configBackoffMost)
		if err := s.reconcileConfig(gone); err == nil {
			t.Fatal("no error from the ledger")
		}
	}
	clockOn(s, time.Second) // read while it waits
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if c := s.configShown(); c.WriteFailed.IsZero() || !c.PendingSince.IsZero() {
		t.Errorf("shown: %+v", c)
	}
	_, body := callAs(t, s.Handler(), "GET", "/settings", "boss@example.com", "", "", "")
	if !strings.Contains(body, "couldn&#39;t be kept to it at") || strings.Contains(body, "have been read since") {
		t.Error("settings says both that it was read twice and that none was")
	}
	if strings.Contains(logged.String(), "no file has been read the same twice in a row since") {
		t.Errorf("logged as never read twice:\n%s", logged.String())
	}
}

// The file the ledger is kept to, tried again for what couldn't be kept of
// it, that the ledger fails to keep to again, is said so: not as a new
// file read twice.
func TestConfigRetryFailureShown(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	body := "windows:\n  - {title: freeze, team: platform, start: 2026-11-02T02:00, end: 2026-11-02T03:00}\n"
	s, _ := configServer(t, body, nil)
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "platform", Rules: "^P"}, "boss"); err != nil {
		t.Fatal(err)
	}
	clockOn(s, time.Hour)
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.reconcileConfig(gone); err == nil {
		t.Fatal("not tried again")
	}
	if c := s.configShown(); c.WriteFailed.IsZero() || !c.Again {
		t.Errorf("shown: %+v", c)
	}
	_, page := callAs(t, s.Handler(), "GET", "/settings", "boss@example.com", "", "", "")
	if !strings.Contains(page, "couldn&#39;t be kept again to the file it is kept to") || strings.Contains(page, "A new file was read twice in a row") {
		t.Error("settings doesn't say the file kept to failed again")
	}
}

// --config's path, which says how serve is run as the other files' do, is
// an admin's to read: on the Settings page and in its API, a reader who
// isn't one is told of --config by its flag.
func TestConfigFileAnAdminsAlone(t *testing.T) {
	parallel(t)
	s, path := configServer(t, configFile, nil)
	h := s.Handler()
	for _, who := range []string{"ed@example.com", "viewer@example.com"} {
		if _, body := callAs(t, h, http.MethodGet, "/settings", who, "", "", "same-origin"); strings.Contains(body, path) || !strings.Contains(body, "declared in <code><bdi>--config</bdi></code>") {
			t.Errorf("%s, the Settings page: the path shown %v", who, strings.Contains(body, path))
		}
		if _, body := callAs(t, h, http.MethodGet, "/api/v1/settings", who, "", "", "same-origin"); strings.Contains(body, path) {
			t.Errorf("%s, /api/v1/settings: %s", who, body)
		}
	}
	for _, p := range []string{"/settings", "/api/v1/settings"} {
		if _, body := callAs(t, h, http.MethodGet, p, "boss@example.com", "", "", "same-origin"); !strings.Contains(body, path) {
			t.Errorf("an admin doesn't read the path on %s", p)
		}
	}
}
