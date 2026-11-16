package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger/ledgertest"
)

// files are the names in dir, and the bytes of each.
func files(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range es {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = b
	}
	return out
}

// A probe of a SQLite ledger reads its schema and writes nothing: the
// file's bytes are as they were, and nothing is made beside it, closed or
// open in a serve.
func TestProbeSQLite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p, err := Probe(ctx, dir)
	if err != nil || p.There || p.Knows < 19 {
		t.Fatalf("no ledger: %+v %v", p, err)
	}
	if len(files(t, dir)) != 0 {
		t.Fatal("a probe made something where there was no ledger")
	}
	l, err := Open(ctx, filepath.Join(dir, "ledger.db"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SetMeta(ctx, "x", "y"); err != nil {
		t.Fatal(err)
	}
	l.Close()
	before := files(t, dir)
	p, err = Probe(ctx, dir)
	if err != nil || !p.There || p.Version != p.Knows {
		t.Fatalf("a closed ledger: %+v %v", p, err)
	}
	after := files(t, dir)
	if !slices.Equal(slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after))) {
		t.Errorf("files %v, then %v", slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after)))
	}
	for name, b := range before {
		if !bytes.Equal(after[name], b) {
			t.Errorf("%s changed", name)
		}
	}
	// Open in a serve, with a log beside it: read through it, the file
	// unchanged.
	l, err = Open(ctx, filepath.Join(dir, "ledger.db"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.SetMeta(ctx, "x", "z"); err != nil {
		t.Fatal(err)
	}
	main, err := os.ReadFile(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	if p, err = Probe(ctx, dir); err != nil || p.Version != p.Knows {
		t.Fatalf("an open ledger: %+v %v", p, err)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, "ledger.db")); !bytes.Equal(again, main) {
		t.Error("the open ledger's file changed")
	}
}

// A ledger at an older schema says so, as serve would migrate it.
func TestProbeSQLiteOlder(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t (x); PRAGMA user_version = 18"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	p, err := Probe(context.Background(), dir)
	if err != nil || !p.There || p.Version != 18 {
		t.Fatalf("%+v %v", p, err)
	}
}

// A probe of a Postgres ledger reads its schema, the user's privilege to
// create in it, and who holds its lock; on a session that refuses a write,
// and with no row of the ledger written. With LAPILLI_TEST_POSTGRES.
func TestProbePostgres(t *testing.T) {
	where := ledgertest.URL(t)
	if where == "" {
		t.Skip("needs " + ledgertest.Env)
	}
	ctx := context.Background()
	p, err := ProbePostgres(ctx, where)
	if err != nil || p.There || !p.Create || p.Holder != nil {
		t.Fatalf("no ledger yet: %+v %v", p, err)
	}
	l, err := OpenPostgres(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	p, err = ProbePostgres(ctx, where)
	if err != nil || !p.There || p.Version != p.Knows || p.Holder == nil || p.Holder.Alone {
		t.Fatalf("a ledger serve holds: %+v %v", p, err)
	}
	l.Close()
	was := ledgertest.Digest(t, where)
	if p, err = ProbePostgres(ctx, where); err != nil || p.Holder != nil {
		t.Fatalf("a ledger no one holds: %+v %v", p, err)
	}
	if now := ledgertest.Digest(t, where); now != was {
		t.Errorf("a row of the ledger changed:\n%s\n%s", was, now)
	}
	// Its session takes no write, whatever is asked on it.
	cfg, _, err := pgConfig(where)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.RuntimeParams["default_transaction_read_only"]; ok {
		t.Error("a startup parameter a pooler in session mode refuses")
	}
	c, err := probeConnect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, "CREATE TABLE preflight_wrote (x int)"); err == nil {
		t.Error("a read-only session made a table")
	}
}

// The probe's connection takes no write, with a log beside the ledger or
// not; and each of what holds it to that holds it alone: read-only, and
// query_only.
func TestProbeSQLiteRefusesWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	abs := filepath.Join(dir, "ledger.db")
	l, err := Open(ctx, abs, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	for _, immutable := range []bool{true, false} {
		for _, without := range []string{"", "mode=ro&", "&_pragma=query_only(1)"} {
			q := strings.Replace(probeQuery(immutable), without, "", 1)
			if without != "" && q == probeQuery(immutable) {
				t.Fatalf("%q isn't in the probe's query %q", without, q)
			}
			db, err := sql.Open("sqlite", "file:"+abs+"?"+q)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range []string{"CREATE TABLE preflight_wrote (x)", "PRAGMA user_version = 7", "INSERT INTO meta (k, v) VALUES ('x', 'y')"} {
				if _, err := db.ExecContext(ctx, w); err == nil {
					t.Errorf("immutable %v, without %q: %s was taken", immutable, without, w)
				}
			}
			db.Close()
		}
	}
	if p, err := Probe(ctx, dir); err != nil || p.Version != p.Knows {
		t.Fatalf("%+v %v", p, err)
	}
}
