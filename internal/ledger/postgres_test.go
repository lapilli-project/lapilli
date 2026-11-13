package ledger

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger/ledgertest"
)

// SQLite's parameters become Postgres's, numbered as SQLite numbers them;
// what is quoted, or a comment, is left as it is.
func TestRebind(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT 1":                                     "SELECT 1",
		"a = ? AND b = ?":                              "a = $1 AND b = $2",
		"a = ?1 OR b = ?1 AND c = ?2":                  "a = $1 OR b = $1 AND c = $2",
		"a = ?3 AND b = ?":                             "a = $3 AND b = $4",
		"a = ? AND b = ?5 AND c = ?":                   "a = $1 AND b = $5 AND c = $6",
		"x = '?' AND y = ?":                            "x = '?' AND y = $1",
		"x = 'it''s ?' AND y = ?":                      "x = 'it''s ?' AND y = $1",
		`"we?ird" = ?`:                                 `"we?ird" = $1`,
		"a = ? -- why?\nAND b = ?":                     "a = $1 -- why?\nAND b = $2",
		"a = ? /* what? */ AND b = ?":                  "a = $1 /* what? */ AND b = $2",
		"VALUES (?, ?), (?, ?)":                        "VALUES ($1, $2), ($3, $4)",
		"x = 'unterminated ?":                          "x = 'unterminated ?",
		"WHERE a IN (SELECT value FROM json_each(?4))": "WHERE a IN (SELECT value FROM json_each($4))",
	} {
		if got, _ := rebind(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// What a connection string holds is said in no error: not the string, nor
// its password, user or database. pgx's own errors say the string, its
// password masked.
func TestPostgresErrorsSayNoSecret(t *testing.T) {
	ctx := context.Background()
	secret := []string{"s3cretPassw0rd", "lgr_ana", "db.example", "lgr_ledger"}
	for _, conn := range []string{
		"postgres://lgr_ana:s3cretPassw0rd@db.example:abc/lgr_ledger",
		"postgres://lgr_ana:s3cret%zzPassw0rd@db.example/lgr_ledger",
		"postgres://lgr_ana:s3cretPassw0rd@db.example:5432/lgr_ledger?sslmode=bogus",
		"host=db.example port=x user=lgr_ana password=s3cretPassw0rd dbname=lgr_ledger",
	} {
		_, err := OpenPostgres(ctx, conn, time.UTC)
		if !errors.Is(err, ErrBadURL) {
			t.Errorf("%s: %v, want ErrBadURL", conn, err)
		}
		for _, s := range secret {
			if err != nil && strings.Contains(err.Error(), s) {
				t.Errorf("an error says %q: %v", s, err)
			}
		}
	}
	// A server that isn't there: where was tried, and nothing more.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = OpenPostgres(ctx, "postgres://lgr_ana:s3cretPassw0rd@"+addr+"/lgr_ledger?sslmode=disable&connect_timeout=5", time.UTC)
	if !errors.Is(err, ErrConnect) {
		t.Fatalf("%v, want ErrConnect", err)
	}
	for _, s := range secret {
		if strings.Contains(err.Error(), s) {
			t.Errorf("an error says %q: %v", s, err)
		}
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("the error doesn't say where: %v", err)
	}
	// A password that turns up in a server's answer is taken out.
	if got := scrub("FATAL: no such role s3cretPassw0rd", []string{"s3cretPassw0rd"}); strings.Contains(got, "s3cret") {
		t.Errorf("scrubbed: %s", got)
	}
}

// The Postgres migrations go as far as SQLite's: a Postgres ledger starts
// whole at the first of them, and each SQLite migration after it has a
// Postgres one of its number. A SQLite migration added alone fails here.
func TestPostgresMigrationsKeepUp(t *testing.T) {
	files, err := fs.Glob(schema, "schema/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	var lite []int
	for _, f := range files {
		n, err := strconv.Atoi(strings.TrimSuffix(path.Base(f), ".sql"))
		if err != nil {
			t.Fatalf("%s: not numbered", f)
		}
		lite = append(lite, n)
	}
	slices.Sort(lite)
	_, pg, err := pgFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(pg) == 0 {
		t.Fatal("no Postgres migrations")
	}
	if pg[0] != 17 {
		t.Errorf("a Postgres ledger starts at schema %d, want 17, the schema it was first written in", pg[0])
	}
	for i, n := range lite {
		if n != i+1 {
			t.Fatalf("SQLite's migrations skip %d", i+1)
		}
	}
	want := lite[slices.Index(lite, pg[0]):]
	if !slices.Equal(pg, want) {
		t.Errorf("Postgres migrations %v, want %v: one for each SQLite migration from %d on", pg, want, pg[0])
	}
}

// A ledger in Postgres has SQLite's tables, columns and indexes: the same
// names, kinds (integer, text, bytes), NOT NULLs, defaults and keys.
func TestPostgresSchemaSame(t *testing.T) {
	u := ledgertest.URL(t)
	if u == "" {
		t.Skip("no " + ledgertest.Env)
	}
	ctx := context.Background()
	lite, err := Open(ctx, t.TempDir()+"/ledger.db", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer lite.Close()
	pg, err := OpenPostgres(ctx, u, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	a, err := describe(ctx, lite)
	if err != nil {
		t.Fatal(err)
	}
	b, err := describe(ctx, pg)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range sortedKeys(a, b) {
		if a[k] != b[k] {
			t.Errorf("%s:\n  SQLite   %q\n  Postgres %q", k, a[k], b[k])
		}
	}
}

func sortedKeys(ms ...map[string]string) []string {
	var ks []string
	for _, m := range ms {
		for k := range m {
			if !slices.Contains(ks, k) {
				ks = append(ks, k)
			}
		}
	}
	slices.Sort(ks)
	return ks
}

// describe is a ledger's schema, by table and column and by index, in
// words both databases give alike.
func describe(ctx context.Context, l *Ledger) (map[string]string, error) {
	out := map[string]string{}
	kind := func(ty string) string {
		switch ty = strings.ToLower(ty); {
		case strings.Contains(ty, "int"):
			return "integer"
		case ty == "text":
			return "text"
		case ty == "blob" || ty == "bytea":
			return "bytes"
		}
		return ty
	}
	tables, err := l.tables(ctx)
	if err != nil {
		return nil, err
	}
	for _, tb := range tables {
		cols, err := l.columns(ctx, tb)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, c := range cols {
			names = append(names, c.name)
			def := c.def
			out[tb+"."+c.name] = kind(c.ty) + " notnull=" + strconv.FormatBool(c.notNull || c.pk) + " default=" + def + " pk=" + strconv.FormatBool(c.pk)
		}
		out[tb] = strings.Join(names, ", ")
	}
	idx, err := indexes(ctx, l)
	if err != nil {
		return nil, err
	}
	for k, v := range idx {
		out[k] = v
	}
	return out, nil
}

// indexes are a ledger's indexes: those made by name, by name, with their
// table, columns, and whether they are unique or partial; a UNIQUE of a
// table's, by its table and columns. A key on an expression is "(expr)";
// Postgres's md5 of a label set, which keeps it unique where an index
// can't hold it, stands for the label set. Primary keys are the columns'.
func indexes(ctx context.Context, l *Ledger) (map[string]string, error) {
	out := map[string]string{}
	type index struct {
		table, name         string
		unique, partial, pk bool
		constraint          bool
		keys                []string
	}
	var all []index
	if l.d == postgres {
		rows, err := l.r.QueryContext(ctx, `SELECT t.relname, i.relname, x.indisunique, x.indpred IS NOT NULL, x.indisprimary,
				EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conindid = x.indexrelid AND c.contype = 'u'),
				ARRAY(SELECT pg_get_indexdef(x.indexrelid, k, true) FROM generate_series(1, x.indnkeyatts) k)::text
			FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid JOIN pg_class t ON t.oid = x.indrelid
			WHERE t.relnamespace = to_regnamespace(current_schema()) AND t.relname != 'schema_version'`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var x index
			var keys string
			if err := rows.Scan(&x.table, &x.name, &x.unique, &x.partial, &x.pk, &x.constraint, &keys); err != nil {
				return nil, err
			}
			for _, k := range strings.Split(strings.Trim(keys, "{}"), ",") {
				k = strings.Trim(k, `"`)
				if c, ok := strings.CutPrefix(k, "md5("); ok && strings.HasSuffix(c, ")") {
					k = strings.TrimSuffix(c, ")")
					x.constraint = true // for SQLite's UNIQUE
				}
				x.keys = append(x.keys, k)
			}
			all = append(all, x)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	} else {
		rows, err := l.r.QueryContext(ctx, `SELECT m.name, il.name, il."unique", il.partial, il.origin = 'pk', il.origin = 'u'
			FROM sqlite_master m, pragma_index_list(m.name) il WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite%'`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var x index
			if err := rows.Scan(&x.table, &x.name, &x.unique, &x.partial, &x.pk, &x.constraint); err != nil {
				rows.Close()
				return nil, err
			}
			all = append(all, x)
		}
		rows.Close()
		for i := range all {
			cols, err := l.r.QueryContext(ctx, "SELECT coalesce(name, '') FROM pragma_index_info(?) ORDER BY seqno", all[i].name)
			if err != nil {
				return nil, err
			}
			for cols.Next() {
				var k string
				cols.Scan(&k)
				all[i].keys = append(all[i].keys, k)
			}
			cols.Close()
		}
	}
	for _, x := range all {
		for i, k := range x.keys {
			if k == "" || strings.ContainsAny(k, "( ") {
				x.keys[i] = "(expr)"
			}
		}
		on := x.table + "(" + strings.Join(x.keys, ", ") + ")"
		switch {
		case x.pk:
		case x.constraint:
			out["unique "+on] = "unique"
		default:
			out["index "+x.name] = on + " unique=" + strconv.FormatBool(x.unique) + " partial=" + strconv.FormatBool(x.partial)
		}
	}
	return out, nil
}

// One process writes a Postgres ledger: a second is refused while the first
// holds it, saying who does, and opens it once the first has let go.
func TestPostgresOneWriter(t *testing.T) {
	u := ledgertest.URL(t)
	if u == "" {
		t.Skip("no " + ledgertest.Env)
	}
	ctx := context.Background()
	a, err := OpenPostgres(ctx, u, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	_, err = OpenPostgres(ctx, u, time.UTC)
	var locked *LockedError
	if !errors.As(err, &locked) || locked.PID == 0 || !strings.Contains(err.Error(), "in use by another process") {
		t.Fatalf("a second writer: %v", err)
	}
	if err := a.SetMeta(ctx, "k", "a"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	b, err := OpenPostgres(ctx, u, time.UTC)
	if err != nil {
		t.Fatalf("once the first let go: %v", err)
	}
	defer b.Close()
	if v, _ := b.Meta(ctx, "k"); v != "a" {
		t.Errorf("the first's write: %q", v)
	}
	// Another schema of the same database is another ledger, with a lock
	// of its own.
	c, err := OpenPostgres(ctx, ledgertest.URL(t), time.UTC)
	if err != nil {
		t.Fatalf("another schema: %v", err)
	}
	c.Close()
}

// A writer whose connection drops connects again, takes the lock again and
// goes on writing; one that finds another process wrote the ledger in
// between writes nothing more, and says so.
func TestPostgresDroppedConnection(t *testing.T) {
	u := ledgertest.URL(t)
	if u == "" {
		t.Skip("no " + ledgertest.Env)
	}
	ctx := context.Background()
	l, err := OpenPostgres(ctx, u, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "Errors", Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}})
	drop := func() {
		t.Helper()
		c, err := pgx.Connect(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(ctx)
		var n int
		if err := c.QueryRow(ctx, "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE pid = $1", int64(l.pg.pid)).Scan(&n); err != nil || n != 1 {
			t.Fatalf("ending the writer's session: %d %v", n, err)
		}
		// Until the server has ended it.
		for range 100 {
			var on bool
			c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)", int64(l.pg.pid)).Scan(&on)
			if !on {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	drop()
	// The write the drop fails, if any, and the next one goes through.
	if err := l.SetMeta(ctx, "after", "1"); err != nil {
		if err := l.SetMeta(ctx, "after", "1"); err != nil {
			t.Fatalf("after the connection dropped: %v", err)
		}
	}
	if v, _ := l.Meta(ctx, "after"); v != "1" {
		t.Errorf("written after: %q", v)
	}
	select {
	case <-l.Lost():
		t.Fatal("the lock is taken for lost")
	default:
	}
	// Dropped again, and another process takes the ledger in between.
	drop()
	other, err := OpenPostgres(ctx, u, time.UTC)
	if err != nil {
		t.Fatalf("another process, while the first is away: %v", err)
	}
	other.Close()
	var werr error
	for range 2 {
		if werr = l.SetMeta(ctx, "late", "1"); werr != nil && errors.Is(werr, ErrLockLost) {
			break
		}
	}
	if !errors.Is(werr, ErrLockLost) {
		t.Fatalf("written after another process took the ledger: %v", werr)
	}
	select {
	case <-l.Lost():
	default:
		t.Fatal("Lost isn't closed")
	}
	if err := l.SetMeta(ctx, "later", "1"); !errors.Is(err, ErrLockLost) {
		t.Errorf("a write once lost: %v", err)
	}
}

// A database whose schema holds tables a ledger has, but that no lapilli
// made, is left alone; one written by a newer lapilli is refused.
func TestPostgresRefusesWhatIsntItsOwn(t *testing.T) {
	u := ledgertest.URL(t)
	if u == "" {
		t.Skip("no " + ledgertest.Env)
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "CREATE TABLE firings (id int)"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPostgres(ctx, u, time.UTC); err == nil || !strings.Contains(err.Error(), "not a ledger this lapilli made") {
		t.Errorf("a schema with a firings table of its own: %v", err)
	}
	c.Exec(ctx, "DROP TABLE firings")
	l, err := OpenPostgres(ctx, u, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if _, err := c.Exec(ctx, "UPDATE schema_version SET version = version + 1"); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)
	if _, err := OpenPostgres(ctx, u, time.UTC); !errors.Is(err, ErrNewer) {
		t.Errorf("written by a newer lapilli: %v", err)
	}
}

// Ledger tests that write files keep them in their own directories: HOME
// is never theirs.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "lapilli-ledger-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
