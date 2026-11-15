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
	// Nothing, or an empty mapping, declares nothing.
	for _, body := range []string{"", "{}\n", "# nothing\n", "teams:\n"} {
		if d, err := ParseConfig([]byte(body), time.UTC); err != nil || len(d.Teams)+len(d.Windows)+len(d.Reports)+len(d.Grants) != 0 {
			t.Errorf("%q: %+v %v", body, d, err)
		}
	}
}

// The leader keeps the ledger to --config: when it starts, and when the
// file changes; a file that can't be used changes nothing, the pages say
// why, and the next good one is kept again.
func TestConfigReloaded(t *testing.T) {
	ctx := context.Background()
	s, path := configServer(t, configFile, nil)
	ts, _ := s.l.Teams(ctx)
	pay := slices.IndexFunc(ts, func(x ledger.Team) bool { return x.Name == "payments" })
	if pay < 0 || !ts[pay].Declared || ts[pay].TZ != "Asia/Seoul" {
		t.Fatalf("teams: %+v", ts)
	}
	// payments made in the UI already (rolesServer), taken over.
	if c := s.configShown(); c == nil || c.Hash == "" || c.Applied.IsZero() || c.Did != (ledger.Reconciled{Made: 4, Changed: 1}) {
		t.Errorf("shown: %+v", c)
	}
	// The same file read again writes nothing.
	gen := s.l.Gen()
	if err := s.reconcileConfig(ctx); err != nil || s.l.Gen() != gen {
		t.Errorf("the same file again: %v, generation %d → %d", err, gen, s.l.Gen())
	}
	// Changed.
	writeConfig(t, path, strings.Replace(configFile, "tz: Asia/Seoul", "tz: Europe/Berlin", 1))
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	ts, _ = s.l.Teams(ctx)
	if ts[slices.IndexFunc(ts, func(x ledger.Team) bool { return x.Name == "payments" })].TZ != "Europe/Berlin" {
		t.Errorf("after a change: %+v", ts)
	}
	// Broken, then wrong for the ledger: neither changes anything, and the
	// pages say why.
	for _, c := range []struct{ body, want string }{
		{strings.Replace(configFile, "tz: Asia/Seoul", "tz: Asia/Seoul\n    color: red", 1), `unknown key "color" in a team`},
		{strings.Replace(configFile, "tz: Asia/Seoul", "tz: Mars/Olympus", 1), `"Mars/Olympus" isn't a time zone`},
		{strings.Replace(configFile, "dests: [team:payments]", "dests: [nowhere]", 1), `there is no destination called "nowhere"`},
	} {
		writeConfig(t, path, c.body)
		gen := s.l.Gen()
		if err := s.reconcileConfig(ctx); err != nil {
			t.Fatalf("a file that can't be used: %v", err)
		}
		if s.l.Gen() != gen {
			t.Errorf("%s: the ledger changed", c.want)
		}
		ts, _ = s.l.Teams(ctx)
		if ts[slices.IndexFunc(ts, func(x ledger.Team) bool { return x.Name == "payments" })].TZ != "Europe/Berlin" {
			t.Errorf("%s: the last good file isn't kept: %+v", c.want, ts)
		}
		shown := s.configShown()
		if !strings.Contains(shown.ErrText("en"), c.want) || shown.ErrText("ko") == shown.ErrText("en") {
			t.Errorf("shown: %q / %q, want %q", shown.ErrText("en"), shown.ErrText("ko"), c.want)
		}
		h := s.Handler()
		for _, p := range []string{"/settings", "/runs"} {
			code, body := callAs(t, h, "GET", p, "boss@example.com", "", "", "")
			if code != http.StatusOK || !strings.Contains(body, "lede") || !strings.Contains(body, htmlText(c.want)) {
				t.Errorf("%s doesn't say %q: %d", p, c.want, code)
			}
		}
	}
	// Good again.
	writeConfig(t, path, configFile)
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if shown := s.configShown(); shown.ErrText("en") != "" {
		t.Errorf("good again, still says %q", shown.ErrText("en"))
	}
	ts, _ = s.l.Teams(ctx)
	if ts[slices.IndexFunc(ts, func(x ledger.Team) bool { return x.Name == "payments" })].TZ != "Asia/Seoul" {
		t.Errorf("good again: %+v", ts)
	}
}

// htmlText is a text as a page escapes it.
func htmlText(s string) string {
	return strings.NewReplacer(`"`, "&#34;", "'", "&#39;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// A file that can't be used for what the ledger lacks (a team it names
// that the UI hasn't made yet) is tried again once the ledger changes.
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

// Without --config, what a file declared before is the UI's again; with
// --config-prune, what the file no longer declares is deleted.
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
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, path, "teams: []\n")
	if err := s.reconcileConfig(ctx); err != nil {
		t.Fatal(err)
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
