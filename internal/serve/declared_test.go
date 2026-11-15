package serve

import (
	"context"
	"errors"
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
	if err := s.reconcileConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, path
}

// A config is read strictly: a key it doesn't know, a value of the wrong
// kind, a key given twice, a second document or a file past its size are
// each refused, with the line; what it declares is read as the forms take
// it.
func TestParseConfig(t *testing.T) {
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
		{"team:\n  - name: a\n", `line 1: unknown key "team": want teams, windows, reports or grants`},
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
	for _, body := range []string{"{}\n", "teams: []\n", "teams:\n"} {
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
	if teamOf(t, s, "payments").TZ != "Europe/Berlin" {
		t.Error("the change isn't kept")
	}
	h := s.Handler()
	for _, c := range []struct {
		body, want string
		whole      bool
	}{
		// Not readable as --config: nothing kept.
		{strings.Replace(berlin, "tz: Europe/Berlin", "tz: Asia/Seoul\n    color: red", 1), `unknown key "color" in a team`, true},
		// A team that can't be kept: kept as it was, the window changed.
		{strings.Replace(strings.Replace(berlin, "tz: Europe/Berlin", "tz: Mars/Olympus", 1), "end: 2026-11-02T03:00", "end: 2026-11-02T04:00", 1),
			`"Mars/Olympus" isn't a time zone`, false},
		// A report to nowhere: kept as it was.
		{strings.Replace(berlin, "dests: [team:payments]", "dests: [nowhere]", 1), `there is no destination called "nowhere"`, false},
	} {
		writeConfig(t, path, c.body)
		if err := s.reconcileConfig(ctx); err != nil {
			t.Fatalf("a file that can't all be kept: %v", err)
		}
		if teamOf(t, s, "payments").TZ != "Europe/Berlin" {
			t.Errorf("%s: payments isn't kept as it was", c.want)
		}
		ws, _ := s.l.Windows(ctx, now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour))
		if long := ws[0].End.Sub(ws[0].Start) == 2*time.Hour; long != strings.Contains(c.body, "04:00") {
			t.Errorf("%s: the rest isn't kept: %+v", c.want, ws[0])
		}
		rs, _ := s.l.Scheduleds(ctx)
		if len(rs) != 1 || rs[0].Dests[0] != "team:payments" {
			t.Errorf("%s: the report isn't kept as it was: %+v", c.want, rs)
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
		if err := s.reconcileConfig(ctx); err != nil {
			t.Fatal(err)
		}
		if e := s.configShown().ErrText("en"); e != "" {
			t.Errorf("%s: put back, it still says %q", c.want, e)
		}
	}
}

// A file that can't be read, then the one the ledger is kept to put back
// as it was: nothing is shown wrong any more.
func TestConfigPutBack(t *testing.T) {
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
// the ledger as it is, and says why.
func TestConfigBadAtStart(t *testing.T) {
	ctx := context.Background()
	s, _ := configServer(t, "teams:\n  - name: payments\n    colour: red\n", nil)
	if err := s.configTerm(ctx); err != nil {
		t.Fatal(err)
	}
	if c := s.configShown(); !c.Whole || !strings.Contains(c.ErrText("en"), "colour") || teamOf(t, s, "payments").Declared {
		t.Errorf("shown: %+v", c)
	}
}

// A replica that leads again keeps the ledger to the file, whatever an
// earlier term kept: another leader with an older copy may have since.
func TestConfigNewTerm(t *testing.T) {
	ctx := context.Background()
	s, _ := configServer(t, configFile, nil)
	o, _ := s.options(ctx)
	if _, _, err := s.l.Reconcile(ctx, ledger.Declared{}, ledger.LeftUndeclare, o); err != nil { // as another leader would
		t.Fatal(err)
	}
	if err := s.configTerm(ctx); err != nil {
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
	// that write are proposals' alone.
	srv := httptest.NewServer(h)
	defer srv.Close()
	cs := connect(t, srv.URL, "boss@example.com")
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range res.Tools {
		if (x.Annotations == nil || !x.Annotations.ReadOnlyHint) && x.Name != "propose" && x.Name != "backtest_again" {
			t.Errorf("MCP tool %s writes", x.Name)
		}
	}

	// The pages: declared, its actions off, why beside them.
	for _, p := range []string{"/teams", "/teams?edit=" + itoa(team.ID), "/windows", "/windows/" + w, "/reports", "/reports/" + r, "/roles"} {
		code, body := callAs(t, h, "GET", p, "boss@example.com", "", "", "")
		if code != http.StatusOK || !strings.Contains(body, `class="badge declared"`) {
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
		if err := s.reconcileConfig(ctx); err != nil {
			t.Fatal(err)
		}
		if n := count(); n != all {
			t.Errorf("%q read once: %d of %d left", body, n, all)
		}
	}
	// Whole again, then teams: [] read twice: deleted the second time.
	writeConfig(t, path, configFile)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, path, "teams: []\n")
	for i := 1; i <= 2; i++ {
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
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
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
	// Read once more: nothing at all.
	s.config.applied = ""
	gen = s.l.Gen()
	if err := s.reconcileConfig(ctx); err != nil || s.l.Gen() != gen {
		cs, _ := s.l.Changes(ctx, 5)
		t.Errorf("once more: %v, generation %d → %d: %+v %+v", err, gen, s.l.Gen(), s.configShown().Did, cs)
	}
}

// A team's name is unique in any letter case, from the forms and the API.
func TestTeamNameAnyCase(t *testing.T) {
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
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
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
