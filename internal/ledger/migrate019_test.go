package ledger

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/ledger/ledgertest"
)

// undo019 takes back what 019 made, for the tests of the migrations
// before it.
var undo019 = []string{
	"DROP INDEX teams_name_fold",
	"ALTER TABLE teams DROP COLUMN name_key",
	"ALTER TABLE teams DROP COLUMN declared",
	"ALTER TABLE windows DROP COLUMN declared",
	"ALTER TABLE reports DROP COLUMN declared",
	"ALTER TABLE grants DROP COLUMN declared",
	"ALTER TABLE teams DROP COLUMN given_back",
	"ALTER TABLE windows DROP COLUMN given_back",
	"ALTER TABLE reports DROP COLUMN given_back",
	"ALTER TABLE grants DROP COLUMN given_back",
}

// A ledger at 018 holding teams whose names differ only in letter case
// (Payments, payments) and others migrates to 019 all the same: nothing is
// declared, every team is kept as it was, those alike are listed, and
// keep no key until one of them is renamed; the rest are keyed, and a new
// team can't take their names in another case. Either of those alike can
// be changed but for its name; renamed, the other is keyed too.
func TestMigrate019(t *testing.T) {
	ledgertest.SQLiteOnly(t, "a ledger an earlier lapilli wrote")
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(ctx, path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range append(slices.Concat(undo021, undo020, undo019), "PRAGMA user_version = 18") {
		if _, err := l.w.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	// As 018 kept them: a name unique as it is spelled, in any case.
	for _, name := range []string{"Payments", "payments", "search"} {
		if _, err := l.w.ExecContext(ctx, "INSERT INTO teams (name, rules, updated) VALUES (?, '^X', 1)", name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.w.ExecContext(ctx, "INSERT INTO windows (title, start_at, end_at, updated) VALUES ('deploy', 1, 2, 1)"); err != nil {
		t.Fatal(err)
	}
	l.Close()

	l2, err := Open(ctx, path, time.UTC)
	if err != nil {
		t.Fatalf("migrating a ledger whose teams' names differ only in case: %v", err)
	}
	defer l2.Close()
	var v int
	l2.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	if want := schemaVersion(t); v != want || want < 19 {
		t.Errorf("migrated to %d, want %d", v, want)
	}
	ts, err := l2.Teams(ctx)
	if err != nil || len(ts) != 3 {
		t.Fatalf("teams after migrating: %+v %v", ts, err)
	}
	for _, x := range ts {
		if x.Declared {
			t.Errorf("%s is declared after migrating", x.Name)
		}
	}
	w, err := l2.windows(ctx, l2.r, "")
	if err != nil || len(w) != 1 || w[0].Declared {
		t.Errorf("windows after migrating: %+v %v", w, err)
	}
	keys := func() map[string]sql.NullString {
		t.Helper()
		rows, err := l2.r.QueryContext(ctx, "SELECT name, name_key FROM teams")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		m := map[string]sql.NullString{}
		for rows.Next() {
			var n string
			var k sql.NullString
			rows.Scan(&n, &k)
			m[n] = k
		}
		return m
	}
	k := keys()
	if k["Payments"].Valid || k["payments"].Valid || k["search"].String != "search" {
		t.Errorf("keys after migrating: %v", k)
	}
	like, err := l2.LikeNames(ctx)
	if err != nil || len(like) != 1 || !slices.Equal(like[0], []string{"Payments", "payments"}) {
		t.Errorf("names alike: %v %v", like, err)
	}
	var pay, pay2, search int64
	for _, x := range ts {
		switch x.Name {
		case "Payments":
			pay = x.ID
		case "payments":
			pay2 = x.ID
		case "search":
			search = x.ID
		}
	}
	// A new team, or one renamed, can't take a name in another case.
	if _, err := l2.PutTeam(ctx, Team{Name: "SEARCH", Rules: "^Y"}, "ana"); !invalidErr(err) {
		t.Errorf("a new team called SEARCH beside search: %v", err)
	}
	if _, err := l2.PutTeam(ctx, Team{ID: search, Name: "PAYMENTS", Rules: "^X"}, "ana"); !invalidErr(err) {
		t.Errorf("search renamed PAYMENTS: %v", err)
	}
	// One of those alike changed but for its name, then renamed.
	if _, err := l2.PutTeam(ctx, Team{ID: pay2, Name: "payments", Rules: "^Pay", Note: "kept"}, "ana"); err != nil {
		t.Errorf("one of two alike changed but for its name: %v", err)
	}
	if _, err := l2.PutTeam(ctx, Team{ID: pay, Name: "Payments EU", Rules: "^X"}, "ana"); err != nil {
		t.Fatal(err)
	}
	k = keys()
	if k["Payments EU"].String != "payments eu" || k["payments"].String != "payments" {
		t.Errorf("keys after renaming one of two alike: %v", k)
	}
	if like, _ := l2.LikeNames(ctx); len(like) != 0 {
		t.Errorf("names alike after renaming: %v", like)
	}
	if _, err := l2.PutTeam(ctx, Team{Name: "PAYMENTS", Rules: "^Z"}, "ana"); !invalidErr(err) {
		t.Errorf("a new team called PAYMENTS beside payments: %v", err)
	}
}
