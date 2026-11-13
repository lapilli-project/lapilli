package ledger

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
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
		case ty == "integer" && l.d == sqlite, ty == "bigint" && l.d == postgres:
			return "integer"
		case strings.Contains(ty, "int"):
			return ty + ", narrower than SQLite's"
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
	// A text compared other than byte by byte, in Postgres, isn't SQLite's.
	if l.d == postgres {
		rows, err := l.r.QueryContext(ctx, `SELECT table_name, column_name, collation_name FROM information_schema.columns
			WHERE table_schema = current_schema() AND data_type = 'text' AND collation_name IS DISTINCT FROM 'C'`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var tb, c string
			var coll *string
			if err := rows.Scan(&tb, &c, &coll); err != nil {
				rows.Close()
				return nil, err
			}
			out[tb+"."+c] += " collated as the database is"
		}
		rows.Close()
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
				if c, ok := strings.CutPrefix(k, "md5("); ok && strings.HasSuffix(c, ")") && x.unique {
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

// A text holding a NUL, or bytes that aren't UTF-8, is kept in Postgres,
// which refuses both, with U+FFFD in their place; SQLite keeps it as sent.
// A SQLite ledger holding such a text copies into Postgres, and is read
// back alike, the text compared as Postgres keeps it.
func TestPostgresText(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	id, err := l.PutTask(ctx, Task{Title: "Tune Errors", Body: "a\x00b\xffc"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.TaskByID(ctx, id)
	want := "a\x00b\xffc"
	if l.d == postgres {
		want = "a�b�c"
	}
	if err != nil || got.Body != want {
		t.Errorf("kept as %q, want %q (%v)", got.Body, want, err)
	}
	if l.d != postgres {
		return
	}
	src, err := Open(ctx, filepath.Join(t.TempDir(), "ledger.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.PutTask(ctx, Task{Title: "a\x00b", Body: "\xff"}, "ana"); err != nil {
		t.Fatal(err)
	}
	dst := open(t, nil)
	if _, err := Copy(ctx, src, dst, nil); err != nil {
		t.Fatal(err)
	}
}

// Closed, a ledger lets its lock go at once: the next process doesn't wait
// for the server to see the connection end, which a network can hold up
// (here, a proxy that passes a closed connection on a second late).
func TestPostgresLetsGoOnClose(t *testing.T) {
	u := ledgertest.URL(t)
	if u == "" {
		t.Skip("no " + ledgertest.Env)
	}
	ctx := context.Background()
	l, err := OpenPostgres(ctx, slowClose(t, u, time.Second), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	key := l.pg.key
	c, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	held := func() int {
		var n int
		if err := c.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND objsubid = 2
			AND classid::bigint = $1 AND objid::bigint = $2`, int64(lockClass), int64(key)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := held(); n != 1 {
		t.Fatalf("open, the lock is held %d times", n)
	}
	l.Close()
	if n := held(); n != 0 {
		t.Errorf("closed, the lock is still held")
	}
}

// slowClose is a connection string through a proxy to the server it names,
// which passes a session's end on late: its Terminate message, and the end
// of the connection.
func slowClose(t *testing.T, conn string, late time.Duration) string {
	t.Helper()
	u, err := url.Parse(conn)
	if err != nil {
		t.Fatal(err)
	}
	server := u.Host
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				s, err := net.Dial("tcp", server)
				if err != nil {
					c.Close()
					return
				}
				go func() { io.Copy(c, s); c.Close() }()
				terminate := []byte{'X', 0, 0, 0, 4}
				b := make([]byte, 64<<10)
				for {
					n, err := c.Read(b)
					if bytes.Equal(b[:n], terminate) {
						time.Sleep(late)
					}
					if n > 0 {
						s.Write(b[:n])
					}
					if err != nil {
						break
					}
				}
				time.Sleep(late)
				s.Close()
			}()
		}
	}()
	u.Host = ln.Addr().String()
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String()
}

// A connection made again while the server still holds the lock for this
// process's own session from before, which it can for minutes after a
// network fault, ends that session and takes the lock.
func TestPostgresOwnSessionEnded(t *testing.T) {
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
	admin, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	// The writer's session ends; another, standing for the session the
	// server would still keep, takes the lock, and is taken for this
	// process's own.
	if _, err := admin.Exec(ctx, "SELECT pg_terminate_backend($1)", int64(l.pg.pid)); err != nil {
		t.Fatal(err)
	}
	stale, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close(ctx)
	for range 100 {
		var took bool
		if err := stale.QueryRow(ctx, "SELECT pg_try_advisory_lock($1, $2)", int32(lockClass), l.pg.key).Scan(&took); err != nil {
			t.Fatal(err)
		}
		if took {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	l.pg.mu.Lock()
	l.pg.pid = stale.PgConn().PID()
	l.pg.mu.Unlock()
	var err2 error
	for range 2 {
		if err2 = l.SetMeta(ctx, "k", "v"); err2 == nil {
			break
		}
	}
	if err2 != nil {
		t.Fatalf("with its own old session holding the lock: %v", err2)
	}
	if err := stale.Ping(ctx); err == nil {
		t.Error("its old session wasn't ended")
	}
}

// A connection made again reads what it caches of the ledger again: a
// failover can lose the last writes, the rules and alerts it made with
// them.
func TestPostgresForgetsOnReconnect(t *testing.T) {
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
	fs := []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}}
	record(t, l, t0, t0.Add(24*time.Hour), fs)
	admin, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	// What a failover lost: the run, its alert and its rule.
	if _, err := admin.Exec(ctx, "SELECT pg_terminate_backend($1)", int64(l.pg.pid)); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "DELETE FROM catches; DELETE FROM firings; DELETE FROM series; DELETE FROM rules; DELETE FROM days; DELETE FROM weeks"); err != nil {
		t.Fatal(err)
	}
	f := Found{From: t0.Add(24 * time.Hour), To: t0.Add(48 * time.Hour), Run: Run{Kind: "test"},
		Firings: []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: t0.Add(25 * time.Hour), End: t0.Add(26 * time.Hour)}}}
	var werr error
	for range 2 {
		if werr = l.Record(ctx, f, opts); werr == nil {
			break
		}
	}
	if werr != nil {
		t.Fatalf("after a failover lost the rule: %v", werr)
	}
}

// The readers write nothing, and a transaction of reads sees the ledger as
// of one moment, as SQLite's do.
func TestReadersAsOfOneMoment(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	if _, err := l.r.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES ('x.r', 'w')"); err == nil {
		t.Error("a reader wrote")
	}
	if err := l.SetMeta(ctx, "k", "1"); err != nil {
		t.Fatal(err)
	}
	tx, err := l.r.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	read := func() string {
		var v string
		if err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'x.k'").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := read(); v != "1" {
		t.Fatalf("read %q", v)
	}
	if err := l.SetMeta(ctx, "k", "2"); err != nil {
		t.Fatal(err)
	}
	if v := read(); v != "1" {
		t.Errorf("a transaction of reads saw a write after it began: %q", v)
	}
}

// Arguments past a statement's last parameter are left out, as SQLite
// leaves them.
func TestBindLeavesOutTheRest(t *testing.T) {
	args := []driver.NamedValue{{Ordinal: 1, Value: "a"}, {Ordinal: 2, Value: "b"}, {Ordinal: 3, Value: "c"}}
	q, got := bind("a = ?1 OR b = ?2", args)
	if q != "a = $1 OR b = $2" || len(got) != 2 {
		t.Errorf("%q %v", q, got)
	}
	if _, got := bind("SELECT 1", args); len(got) != 0 {
		t.Errorf("none: %v", got)
	}
}
