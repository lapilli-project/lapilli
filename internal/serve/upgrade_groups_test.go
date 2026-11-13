package serve

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/verdict"
)

// testdata/v0.31.2-groups.db.gz is a ledger lapilli serve v0.31.2 wrote,
// before Lapilli read a Prometheus group's labels and limit, from rules in
// groups with both (groupsBefore, then groupsNow): rules named apart by
// their groups, a team of one of them by name, a note and a task; a change
// applied (proposal 1, Errors's for to 15m), one proposed (2, Busy
// [shop-eu]'s to 10m), and one merged but not yet read back (3, Busy
// [shop-us]'s to 20m). Its rules were read from groupsDir.
const groupsDir = "/var/folders/xw/j491mff118xg5rqrdvtssb7c0000gn/T/TestZZFixture5218048/001/rules"

// groupsNow are the rules the ledger was left with: proposal 1 applied.
const groupsNow = `groups:
  - name: shop
    labels: {team: shop}
    limit: 10
    rules:
      - alert: Errors
        expr: errors > 5
        for: 15m
        labels: {severity: page}
  - name: shop-eu
    labels: {region: eu}
    rules:
      - alert: Busy
        expr: busy > 1
        for: 2m
  - name: shop-us
    labels: {region: us}
    limit: 5
    rules:
      - alert: Busy
        expr: busy > 1
        for: 5m
`

// upgraded is a server, changes on, on a copy of the ledger v0.31.2 wrote,
// its rules where they were then, as they were left.
func upgraded(t *testing.T) (*Server, string) {
	t.Helper()
	ledgertest.SQLiteOnly(t, "a ledger an earlier lapilli wrote")
	s, dir := server(t, func(c *Config) {
		c.UserHeader = "X-Forwarded-Email"
		c.Editors = []string{"*"}
		c.Admins = []string{ana}
		c.Approvers = []string{ana, cy}
		c.Changes = true
		c.ChangesToken = tokenFile(t, changesToken)
		c.Ledger = filepath.Join(t.TempDir(), "ledger")
		os.MkdirAll(c.Ledger, 0o750)
		copyLedger(t, filepath.Join(c.Ledger, "ledger.db"), groupsDir, c.RulesPath)
	})
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(groupsNow), 0o644)
	return s, dir
}

// copyLedger writes the fixture's ledger to path, with where its rules
// were read from (was) made where they are read from now (is), as on a
// machine that kept its paths.
func copyLedger(t *testing.T, path, was, is string) {
	t.Helper()
	in, err := os.Open("testdata/v0.31.2-groups.db.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	z, err := gzip.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, z); err != nil {
		t.Fatal(err)
	}
	out.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tables, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var n string
		tables.Scan(&n)
		names = append(names, n)
	}
	tables.Close()
	moved := 0
	for _, table := range names {
		cols, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
		if err != nil {
			t.Fatal(err)
		}
		var cs []string
		for cols.Next() {
			var c string
			cols.Scan(&c)
			cs = append(cs, c)
		}
		cols.Close()
		for _, c := range cs {
			q := `UPDATE "` + table + `" SET "` + c + `" = replace("` + c + `", ?, ?) WHERE typeof("` + c + `") = 'text' AND instr("` + c + `", ?) > 0`
			res, err := db.Exec(q, was, is, was)
			if err != nil {
				t.Fatal(err)
			}
			n, _ := res.RowsAffected()
			moved += int(n)
		}
	}
	if moved == 0 {
		t.Fatal("the fixture's rules weren't where it says")
	}
}

// versionsOf are the ledger's versions of the rules.
func versionsOf(t *testing.T, s *Server) []ledger.Version {
	t.Helper()
	vs, err := s.l.Versions(context.Background(), time.Time{}, time.Now().Add(24*365*time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	return vs
}

// A ledger written before Lapilli read a group's labels and limit reads
// as it did: no new version of the rules, each rule keeps its name and
// what hangs on it (a team's list, a note, a task, its firings), and a
// change proposed, merged or applied then goes on as it would have.
func TestUpgradeGroups(t *testing.T) {
	ctx := context.Background()
	want := []string{"Errors", "Busy [shop-eu]", "Busy [shop-us]"}

	t.Run("names and history", func(t *testing.T) {
		s, _ := upgraded(t)
		h := s.Handler()
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		s.jobs(ctx, true)
		vs := versionsOf(t, s)
		if len(vs) != 2 {
			t.Errorf("%d versions of the rules, want the 2 the ledger had", len(vs))
		}
		latest, _, _ := s.l.Latest(ctx)
		if _, names, err := s.named(ctx, latest.ID); err != nil || !slices.Equal(names, want) {
			t.Errorf("names %q, want %q (%v)", names, want, err)
		}
		if _, body, _ := ui(t, h, http.MethodGet, "/rules?team=eu", bo, "", nil); !strings.Contains(body, `href="/rules/Busy%20%5Bshop-eu%5D?team=eu"`) {
			t.Errorf("the team's rules don't list Busy [shop-eu]:\n%s", body)
		}
		if _, body, _ := ui(t, h, http.MethodGet, "/rules/Busy%20%5Bshop-eu%5D", bo, "", nil); !strings.Contains(body, "the eu note") {
			t.Errorf("the note isn't on the rule's page")
		}
		if _, body := api(t, h, "GET", "/api/v1/tasks/1", ""); !strings.Contains(body, `"rule": "Busy [shop-us]"`) {
			t.Errorf("the task: %s", body)
		}
		_, body := api(t, h, "GET", "/api/v1/report?from=2026-10-27T00:00:00Z&to=2026-11-03T12:00:00Z&firings=false", "")
		var rep verdict.Report
		json.Unmarshal([]byte(body), &rep)
		var got []string
		fired := 0
		for _, r := range rep.Rules {
			got = append(got, r.Rule)
			fired += r.Firings
		}
		slices.Sort(got)
		if w := slices.Sorted(slices.Values(want)); !slices.Equal(got, w) || fired == 0 {
			t.Errorf("the report's rules %q, %d firings; want %q", got, fired, w)
		}
	})

	t.Run("a change proposed before", func(t *testing.T) {
		s, _ := upgraded(t)
		h := s.Handler()
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		if _, body, _ := ui(t, h, http.MethodGet, "/proposals", bo, "", nil); strings.Contains(body, "the rule changed since") {
			t.Errorf("a proposal is taken as stale:\n%s", body)
		}
		if code, body := approve(t, h, 2, ana); code != http.StatusSeeOther {
			t.Fatalf("approving: %d %s", code, body)
		}
		s.jobs(ctx, false)
		if p := state(t, s, 2); p.State != ledger.StateReady {
			t.Errorf("approved before, checked again after: %s (%+v)", p.State, p)
		}
	})

	t.Run("a change merged before, read back after", func(t *testing.T) {
		s, dir := upgraded(t)
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		s.jobs(ctx, false)
		p := state(t, s, 3)
		if p.State != ledger.StateMerged {
			t.Fatalf("merged, not yet read back: %s (%+v)", p.State, p)
		}
		os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(p.Document), 0o644)
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		s.jobs(ctx, false)
		if p := state(t, s, 3); p.State != ledger.StateApplied {
			t.Errorf("read back: %s (%+v)", p.State, p)
		}
	})

	t.Run("the undo of a change applied before", func(t *testing.T) {
		s, _ := upgraded(t)
		h := s.Handler()
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		key := keyFor(t, h, 1, ana, "undo")
		if key == "" {
			t.Fatal("no undo offered")
		}
		code, body, hd := ui(t, h, http.MethodPost, "/proposals/1/undo", ana, "same-origin", url.Values{"key": {key}})
		if code != http.StatusSeeOther {
			t.Fatalf("undo: %d %s", code, body)
		}
		var id int64
		json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &id)
		s.jobs(ctx, false)
		if u := state(t, s, id); u.State != ledger.StateReady || u.Follows != 1 {
			t.Errorf("the undo: %s (%+v)", u.State, u)
		}
	})

	// A change to a group's labels, or its limit, alone is a new version,
	// its rules named as they were.
	t.Run("a group's labels changed after", func(t *testing.T) {
		s, dir := upgraded(t)
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		for i, edit := range [][2]string{{"labels: {region: eu}", "labels: {region: eu-west}"}, {"limit: 5", "limit: 6"}} {
			now, _ := os.ReadFile(filepath.Join(dir, "shop.yaml"))
			os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(strings.Replace(string(now), edit[0], edit[1], 1)), 0o644)
			if err := s.look(ctx, &raw); err != nil {
				t.Fatal(err)
			}
			if n := len(versionsOf(t, s)); n != 3+i {
				t.Errorf("%s: %d versions, want %d", edit[1], n, 3+i)
			}
		}
		latest, _, _ := s.l.Latest(ctx)
		if _, names, _ := s.named(ctx, latest.ID); !slices.Equal(names, want) {
			t.Errorf("names %q, want %q", names, want)
		}
		// The rule's page lists the version, with the labels its alerts
		// carry.
		if _, body, _ := ui(t, s.Handler(), http.MethodGet, "/rules/Busy%20%5Bshop-eu%5D", bo, "", nil); !strings.Contains(body, "<td>region=eu-west</td>") ||
			!strings.Contains(body, "<td>region=eu</td>") {
			t.Errorf("the rule's versions don't show its group's labels")
		}
	})
}

// Following a history of the rules, a version that changes only a group's
// labels is a version; one that changes nothing isn't.
func TestFollowGroupLabels(t *testing.T) {
	ctx := context.Background()
	hist := t.TempDir()
	add := func(at time.Time, group string) {
		v := filepath.Join(hist, at.Format("20060102T150405Z"))
		os.MkdirAll(v, 0o755)
		os.WriteFile(filepath.Join(v, "rules.yaml"), []byte("groups:\n  - name: shop\n"+group+"    rules:\n      - {alert: Errors, expr: errors > 5, for: 2m}\n"), 0o644)
	}
	add(now.Add(-90*time.Hour), "")
	add(now.Add(-60*time.Hour), "    labels: {team: shop}\n")
	s, _ := server(t, func(c *Config) { c.RulesPath, c.RulesHistory = "", hist })
	if err := s.history(ctx); err != nil {
		t.Fatal(err)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if vs := versionsOf(t, s); len(vs) != 2 || !vs[1].Since.Equal(now.Add(-60*time.Hour)) {
		t.Errorf("read from the history: versions %+v, want the first and the one that labelled its group", vs)
	}
	// Commits fetched since, followed: one that labels its group again,
	// otherwise, and one that changes nothing.
	add(now.Add(-40*time.Hour), "    labels: {team: checkout}\n")
	add(now.Add(-30*time.Hour), "    labels: {team: checkout}\n")
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if vs := versionsOf(t, s); len(vs) != 3 || !vs[2].Since.Equal(now.Add(-40*time.Hour)) {
		t.Errorf("followed: versions %+v, want one more, from when it labelled its group again", vs)
	}
}
