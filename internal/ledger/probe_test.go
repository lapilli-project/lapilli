package ledger

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
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

// logged makes a SQLite ledger in dir whose file is at schema 19 and its
// write-ahead log at 23, as a serve that migrated it and hasn't
// checkpointed since leaves it, and keeps it open: closed, the log would
// be checkpointed and gone.
func logged(t *testing.T, dir string) *sql.DB {
	t.Helper()
	u := fileURI(filepath.Join(dir, "ledger.db"))
	db, err := sql.Open("sqlite", u.String()+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"CREATE TABLE t (x)", "PRAGMA user_version = 19", "PRAGMA wal_checkpoint(TRUNCATE)", "INSERT INTO t VALUES (1)", "PRAGMA user_version = 23"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// copied copies the files named from one directory to another, as a copy
// of a volume, or what a serve that stopped hard left, would hold them.
func copied(t *testing.T, from, to string, names ...string) {
	t.Helper()
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(from, n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// unchanged fails the test on a file made in dir, or one whose bytes
// changed, since before.
func unchanged(t *testing.T, dir string, before map[string][]byte) {
	t.Helper()
	after := files(t, dir)
	for name, b := range after {
		if was, ok := before[name]; !ok {
			t.Errorf("%s was made (%d bytes)", name, len(b))
		} else if !bytes.Equal(was, b) {
			t.Errorf("%s changed", name)
		}
	}
	if len(after) < len(before) {
		t.Errorf("files %v, then %v", slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after)))
	}
}

// holdEnv names, to the test binary run again, a directory to hold a
// ledger open in, as a serve in another process does: within a process
// SQLite shares the index (-shm) between connections, so what a probe
// does to a serve's index is seen only across two.
const holdEnv = "LAPILLI_TEST_HOLD_LEDGER"

// TestHoldLedger is that other process: it makes a ledger as logged does,
// says so on its standard output, and holds it open until its standard
// input is closed.
func TestHoldLedger(t *testing.T) {
	dir := os.Getenv(holdEnv)
	if dir == "" {
		t.Skip("TestProbeSQLiteBeside runs it, as another process")
	}
	db := logged(t, dir)
	defer db.Close()
	fmt.Println("held")
	io.Copy(io.Discard, os.Stdin)
}

// Whatever is beside a SQLite ledger, the probe reads its schema as serve
// would, and neither makes a file beside it nor changes one: its
// write-ahead log with no index (a serve that stopped hard, its -shm
// cleared; a copy of the two files), the log and its index left by a
// serve that stopped hard, and a serve in another process holding both.
// The schema is the log's, never the file's alone.
func TestProbeSQLiteBeside(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	db := logged(t, src)
	defer db.Close()
	for name, beside := range map[string][]string{
		"a log, no index":          {"ledger.db", "ledger.db-wal"},
		"a log and its index":      {"ledger.db", "ledger.db-wal", "ledger.db-shm"},
		"a log, its index of 0400": {"ledger.db", "ledger.db-wal", "ledger.db-shm"},
	} {
		dir := t.TempDir()
		copied(t, src, dir, beside...)
		if strings.Contains(name, "0400") {
			os.Chmod(filepath.Join(dir, "ledger.db-shm"), 0o400)
		}
		before := files(t, dir)
		p, err := Probe(ctx, dir)
		if err != nil || !p.There || p.Version != 23 {
			t.Errorf("%s: %+v %v", name, p, err)
		}
		unchanged(t, dir, before)
	}
	// A serve in another process holds the ledger, its log and its index.
	held := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHoldLedger$", "-test.count=1")
	cmd.Env = append(os.Environ(), holdEnv+"="+held)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	defer in.Close()
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "held\n" {
		t.Fatalf("the other process: %q %v", line, err)
	}
	before := files(t, held)
	if _, ok := before["ledger.db-shm"]; !ok {
		t.Fatalf("a serve's ledger with no index: %v", slices.Sorted(maps.Keys(before)))
	}
	p, err := Probe(ctx, held)
	if err != nil || p.Version != 23 {
		t.Errorf("held by a serve: %+v %v", p, err)
	}
	unchanged(t, held, before)
}

// stamp is what a file is as a probe must leave it: its size, its mode
// and when it was last written.
type stamp struct {
	size  int64
	mode  os.FileMode
	mtime time.Time
}

// untouched dates each file in dirs an hour back, so that a write is seen
// by its time however soon it comes, and gives a check that fails the
// test on a file made or removed in them since, or one whose size, mode
// or time changed. A link is taken as itself, not as what it leads to.
func untouched(t *testing.T, dirs ...string) func(what string) {
	t.Helper()
	stamps := func(dir string) map[string]stamp {
		es, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]stamp{}
		for _, e := range es {
			fi, err := os.Lstat(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out[e.Name()] = stamp{fi.Size(), fi.Mode(), fi.ModTime()}
		}
		return out
	}
	back := time.Now().Add(-time.Hour).Truncate(time.Second)
	before := map[string]map[string]stamp{}
	for _, dir := range dirs {
		es, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if e.Type().IsRegular() {
				if err := os.Chtimes(filepath.Join(dir, e.Name()), back, back); err != nil {
					t.Fatal(err)
				}
			}
		}
		before[dir] = stamps(dir)
	}
	return func(what string) {
		t.Helper()
		for _, dir := range dirs {
			was, now := before[dir], stamps(dir)
			for name, s := range now {
				if w, ok := was[name]; !ok {
					t.Errorf("%s: %s was made (%d bytes)", what, name, s.size)
				} else if w != s {
					t.Errorf("%s: %s changed: %+v, then %+v", what, name, w, s)
				}
			}
			for name := range was {
				if _, ok := now[name]; !ok {
					t.Errorf("%s: %s was removed", what, name)
				}
			}
		}
	}
}

// A ledger whose file is shorter than a page, a write-ahead log beside
// it, isn't opened: SQLite would take it for an empty database and delete
// the log, which holds the ledger. The probe says it is cut short, and
// every file is left as it was. A short file alone is opened as before, a
// new database to SQLite.
func TestProbeSQLiteCutShort(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	db := logged(t, src)
	defer db.Close()
	whole, err := os.ReadFile(filepath.Join(src, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 100, smallestPage - 1} {
		for _, beside := range [][]string{{"ledger.db-wal"}, {"ledger.db-wal", "ledger.db-shm"}} {
			what := fmt.Sprintf("%d bytes, %v", size, beside)
			dir := t.TempDir()
			copied(t, src, dir, beside...)
			if err := os.WriteFile(filepath.Join(dir, "ledger.db"), whole[:size], 0o600); err != nil {
				t.Fatal(err)
			}
			check := untouched(t, dir)
			if p, err := Probe(ctx, dir); !errors.Is(err, ErrCutShort) || !p.There {
				t.Errorf("%s: %+v %v", what, p, err)
			}
			check(what)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ledger.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	check := untouched(t, dir)
	if p, err := Probe(ctx, dir); err != nil || !p.There || p.Version != 0 {
		t.Errorf("an empty file alone: %+v %v", p, err)
	}
	check("an empty file alone")
}

// A ledger.db that is a link to a file elsewhere is read where it leads,
// as SQLite reads it: its log beside the file it leads to, and its index
// or none, are read through as they would be there, the schema the log's,
// and nothing made or changed beside the link or the file. One that leads
// to a file cut short is refused as that file is.
func TestProbeSQLiteLink(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	db := logged(t, src)
	defer db.Close()
	linked := func(t *testing.T, names ...string) (real, link string) {
		real, link = t.TempDir(), t.TempDir()
		copied(t, src, real, names...)
		if err := os.Symlink(filepath.Join(real, "ledger.db"), filepath.Join(link, "ledger.db")); err != nil {
			t.Skip("no symbolic links here:", err)
		}
		return real, link
	}
	for name, names := range map[string][]string{
		"a log, no index":     {"ledger.db", "ledger.db-wal"},
		"a log and its index": {"ledger.db", "ledger.db-wal", "ledger.db-shm"},
		"no log":              {"ledger.db"},
	} {
		real, link := linked(t, names...)
		want := 23
		if len(names) == 1 {
			want = 19 // all of it in the file
		}
		check := untouched(t, real, link)
		if p, err := Probe(ctx, link); err != nil || !p.There || p.Version != want {
			t.Errorf("%s: %+v %v, want schema %d", name, p, err, want)
		}
		check(name)
	}
	real, link := linked(t, "ledger.db-wal", "ledger.db-shm")
	if err := os.WriteFile(filepath.Join(real, "ledger.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	check := untouched(t, real, link)
	if _, err := Probe(ctx, link); !errors.Is(err, ErrCutShort) {
		t.Errorf("a link to a file cut short: %v", err)
	}
	check("a link to a file cut short")
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
	l, err := Open(ctx, filepath.Join(dir, "ledger.db"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	src := t.TempDir()
	db := logged(t, src)
	defer db.Close()
	for how, files := range map[probing][]string{indexed: {"ledger.db", "ledger.db-wal", "ledger.db-shm"}, unindexed: {"ledger.db", "ledger.db-wal"}} {
		d := t.TempDir()
		copied(t, src, d, files...)
		if got, err := beside(filepath.Join(d, "ledger.db")); got != how || err != nil {
			t.Fatalf("%v is said %v %v", files, got, err)
		}
	}
	for _, how := range []probing{alone, indexed, unindexed} {
		at := dir
		if how != alone {
			at = t.TempDir()
			files := []string{"ledger.db", "ledger.db-wal", "ledger.db-shm"}
			if how == unindexed {
				files = files[:2]
			}
			copied(t, src, at, files...)
		}
		abs := filepath.Join(at, "ledger.db")
		for _, without := range []string{"", "mode=ro&", "&_pragma=query_only(1)"} {
			q := strings.Replace(probeQuery(how), without, "", 1)
			if without != "" && q == probeQuery(how) {
				t.Fatalf("%q isn't in the probe's query %q", without, q)
			}
			db, done, err := openProbe(abs, how, q)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range []string{"CREATE TABLE preflight_wrote (x)", "PRAGMA user_version = 7", "INSERT INTO meta (k, v) VALUES ('x', 'y')", "INSERT INTO t VALUES (2)"} {
				if _, err := db.ExecContext(ctx, w); err == nil {
					t.Errorf("%v, without %q: %s was taken", how, without, w)
				}
			}
			done()
		}
	}
	if p, err := Probe(ctx, dir); err != nil || p.Version != p.Knows {
		t.Fatalf("%+v %v", p, err)
	}
}
