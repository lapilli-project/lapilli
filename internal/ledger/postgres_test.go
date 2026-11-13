package ledger

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql/driver"
	"errors"
	"fmt"
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
	"lapilli.dev/internal/incident"
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
	// A server that answers nothing: where, and that it timed out, by the
	// string's connect_timeout or the caller's deadline.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	at := silent.Addr().String()
	short, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	for _, c := range []struct {
		ctx  context.Context
		conn string
	}{
		{context.Background(), "postgres://lgr_ana:s3cretPassw0rd@" + at + "/lgr_ledger?sslmode=disable&connect_timeout=1"},
		{short, "postgres://lgr_ana:s3cretPassw0rd@" + at + "/lgr_ledger?sslmode=disable"},
	} {
		_, err := OpenPostgres(c.ctx, c.conn, time.UTC)
		if !errors.Is(err, ErrConnect) || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "connecting to "+at+" timed out") ||
			strings.Contains(err.Error(), "%!") {
			t.Errorf("a server that answers nothing: %v", err)
		}
		for _, s := range secret {
			if err != nil && strings.Contains(err.Error(), s) {
				t.Errorf("an error says %q: %v", s, err)
			}
		}
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
	l.pg.own[stale.PgConn().PID()] = true
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

// An Azure alarm's id is compared in any letter case of its ASCII letters,
// and its other letters as they are, in either database: an id with an É,
// asked for in another case of its ASCII letters, fires; asked for with an
// é, it doesn't, as SQLite's lower() leaves an É.
func TestFiringAlarmFoldsASCII(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	id := "/subscriptions/S/resourceGroups/rg/providers/Microsoft.Insights/metricAlerts/Échec-paiement"
	now := time.Now().Truncate(time.Second)
	if _, err := l.PutFirings(ctx, "azure-prod", now.Add(-time.Hour), now, []firing.Firing{
		{Rule: "Échec-paiement", Labels: map[string]string{AlarmID: strings.ToLower(id[:60]) + id[60:]}, Start: now.Add(-30 * time.Minute)},
	}, FiringsCopier, opts); err != nil {
		t.Fatal(err)
	}
	for ask, want := range map[string]bool{
		id: true, strings.ToUpper(id[:20]) + id[20:]: true, strings.Replace(id, "Échec", "échec", 1): false,
	} {
		if on, err := l.FiringAlarm(ctx, "azure-prod", ask, true); err != nil || on != want {
			t.Errorf("%s: firing %v, want %v (%v)", ask, on, want, err)
		}
	}
}

// Any role that may connect can take a ledger's lock: the refusal names the
// role that holds it, whatever the session calls itself, so a lock taken
// by another role is told from one of lapilli's own.
func TestPostgresLockedSaysRole(t *testing.T) {
	u := ledgertest.URL(t)
	if u == "" {
		t.Skip("no " + ledgertest.Env)
	}
	ctx := context.Background()
	l, err := OpenPostgres(ctx, u, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	key := l.pg.key
	l.Close()
	b := make([]byte, 4)
	rand.Read(b)
	role := fmt.Sprintf("lt_role_%x", b)
	admin, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'lt-role-pw'"); err != nil {
		t.Skipf("no role to make: %v", err)
	}
	defer admin.Exec(ctx, "DROP ROLE "+role)
	pu, _ := url.Parse(u)
	pu.User = url.UserPassword(role, "lt-role-pw")
	q := pu.Query()
	q.Set("application_name", "lapilli")
	pu.RawQuery = q.Encode()
	other, err := pgx.Connect(ctx, pu.String())
	if err != nil {
		t.Skipf("the role can't connect here: %v", err)
	}
	if _, err := other.Exec(ctx, "SELECT pg_advisory_lock($1, $2)", int32(lockClass), key); err != nil {
		t.Fatal(err)
	}
	_, err = OpenPostgres(ctx, u, time.UTC)
	other.Close(ctx)
	var locked *LockedError
	if !errors.As(err, &locked) || locked.Role != role || !strings.Contains(err.Error(), `role "`+role+`"`) {
		t.Errorf("held by another role: %v", err)
	}
}

// A run makes its rules and alerts in the same order in either database,
// SQLite's: those still on from before its period first, as Record comes
// to them. Ties broken by an alert's id (the most re-fired) break alike.
func TestSeriesMadeInSQLitesOrder(t *testing.T) {
	ctx := context.Background()
	from := t0.Add(12 * time.Hour)
	f := Found{From: from, To: from.Add(12 * time.Hour), Firings: []firing.Firing{
		{Rule: "Errors", Labels: map[string]string{"service": "a"}, Start: from.Add(time.Hour), End: from.Add(70 * time.Minute)},
		{Rule: "Errors", Labels: map[string]string{"service": "a"}, Start: from.Add(90 * time.Minute), End: from.Add(100 * time.Minute)},
		{Rule: "Latency", Labels: map[string]string{"service": "c"}, Start: from.Add(2 * time.Hour), End: from.Add(3 * time.Hour)},
		{Rule: "Errors", Labels: map[string]string{"service": "b"}, Start: from.Add(-10 * time.Minute), End: from.Add(5 * time.Minute)},
		{Rule: "Saturation", Labels: map[string]string{"service": "d"}, Start: from.Add(-20 * time.Minute), End: from.Add(time.Minute)},
		{Rule: "Errors", Labels: map[string]string{"service": "b"}, Start: from.Add(20 * time.Minute), End: from.Add(30 * time.Minute)},
	}, Run: Run{Kind: "test", Started: from.Add(12 * time.Hour), Finished: from.Add(12 * time.Hour), From: from, To: from.Add(12 * time.Hour)}}
	made := func(l *Ledger) []string {
		if err := l.Record(ctx, f, opts); err != nil {
			t.Fatal(err)
		}
		rows, err := l.r.QueryContext(ctx, "SELECT s.id, r.id, r.name, s.labels FROM series s JOIN rules r ON r.id = s.rule_id ORDER BY s.id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var sid, rid int64
			var name, labels string
			rows.Scan(&sid, &rid, &name, &labels)
			out = append(out, fmt.Sprint(sid, " ", rid, " ", name, " ", labels))
		}
		top, err := l.RefiredOf(ctx, "Errors", t0, t0.Add(48*time.Hour), 1)
		if err != nil || len(top) != 1 {
			t.Fatalf("re-fired: %v %v", top, err)
		}
		return append(out, "top "+top[0].Labels["service"])
	}
	lite, err := Open(ctx, filepath.Join(t.TempDir(), "ledger.db"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer lite.Close()
	want := made(lite)
	if got := made(open(t, time.UTC)); !slices.Equal(got, want) {
		t.Errorf("made:\n  %q\nSQLite made:\n  %q", got, want)
	}
}

// A NUL in a label, which every way in takes, as SQLite keeps it: a run,
// a source's firings, a notification and a task are kept whole in either
// database, the alert found again the next time, and a ledger holding one
// copies into Postgres and is read there. Postgres keeps U+FFFD in its
// place, json.Marshal's \u0000 too, which jsonb refuses (series_alarm,
// notified_alarm, a change's status).
func TestNULInLabels(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	nul := map[string]string{"service": "checkout", "path": "/x\x00"}
	run := func(at time.Time) {
		t.Helper()
		f := Found{From: at, To: at.Add(time.Hour), Run: Run{Kind: "test", Started: at.Add(time.Hour), Finished: at.Add(time.Hour), From: at, To: at.Add(time.Hour)},
			Firings: []firing.Firing{
				{Rule: "Legit", Labels: map[string]string{"service": "checkout"}, Start: at.Add(10 * time.Minute), End: at.Add(20 * time.Minute)},
				{Rule: "HighErrors", Labels: nul, Start: at.Add(10 * time.Minute), End: at.Add(20 * time.Minute)},
			}}
		if err := l.Record(ctx, f, opts); err != nil {
			t.Fatalf("a run with a NUL in a label: %v", err)
		}
	}
	run(t0)
	run(t0.Add(time.Hour))
	var n int
	if err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM series s JOIN rules r ON r.id = s.rule_id WHERE r.name = 'HighErrors'").Scan(&n); err != nil || n != 1 {
		t.Errorf("the alert with a NUL, after two runs: %d series (%v)", n, err)
	}
	if _, err := l.PutFirings(ctx, "cloudwatch-prod", t0, t0.Add(time.Hour), []firing.Firing{
		{Rule: "Other", Labels: map[string]string{"x": "a\x00", AlarmID: "arn:a\x00"}, Start: t0.Add(10 * time.Minute), End: t0.Add(20 * time.Minute)},
	}, FiringsCopier, opts); err != nil {
		t.Errorf("a source's firing with a NUL in a label: %v", err)
	}
	at := t0.Add(30 * time.Minute)
	n2 := Notification{At: at, Source: "alertmanager", Receiver: "pager", Group: `{}:{alertname="HighErrors"}`,
		Alerts: []Alert{{Rule: "HighErrors", Labels: nul, Start: t0.Add(10 * time.Minute)}, {Rule: "HighErrors", Labels: map[string]string{"alarm_id": "\x00"}, Start: at}}}
	if err := n2.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Notify(ctx, n2); err != nil {
		t.Errorf("a notification with a NUL in a label: %v", err)
	}
	// The next, which doesn't say the alert's start, finds it in the last.
	again := Notification{At: at.Add(5 * time.Minute), Source: n2.Source, Receiver: n2.Receiver, Group: n2.Group,
		Alerts: []Alert{{Rule: "HighErrors", Labels: nul, Resolved: true}}}
	if _, err := l.Notify(ctx, again); err != nil {
		t.Errorf("the alert with a NUL resolved: %v", err)
	}
	if ns, err := l.Notifications(ctx, again.At, again.At.Add(time.Second)); err != nil || len(ns) != 1 || !ns[0].Alerts[0].Start.Equal(t0.Add(10*time.Minute)) {
		t.Errorf("the start of the alert with a NUL, resolved: %+v %v", ns, err)
	}
	if _, err := l.Pages(ctx, t0, t0.Add(24*time.Hour), func(string) time.Duration { return time.Hour }); err != nil {
		t.Errorf("its pages: %v", err)
	}
	if _, err := l.PutTask(ctx, Task{Title: "Poison\x00", Body: "a\x00b"}, "eve"); err != nil {
		t.Fatal(err)
	}
	if ts, err := l.TrackerTasks(ctx, TrackerQuery{}); err != nil || len(ts) != 1 || ts[0].StatusBy != "eve" {
		t.Errorf("the tracker job's read after a task with a NUL: %+v %v", ts, err)
	}
	// A SQLite ledger that kept them, copied into this one's database.
	src, err := Open(ctx, filepath.Join(t.TempDir(), "ledger.db"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Notify(ctx, n2); err != nil {
		t.Fatal(err)
	}
	if _, err := src.PutTask(ctx, Task{Title: "a\x00b"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.PutFirings(ctx, "cloud", t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "DiskFull", Labels: map[string]string{"service": "a\x00b"}, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}}, "ana", opts); err != nil {
		t.Fatal(err)
	}
	dst := open(t, nil)
	if _, err := Copy(ctx, src, dst, nil); err != nil {
		t.Fatalf("a SQLite ledger with NULs copied: %v", err)
	}
	if _, err := dst.TrackerTasks(ctx, TrackerQuery{}); err != nil {
		t.Errorf("copied, the tracker's tasks: %v", err)
	}
	if _, err := dst.PutFirings(ctx, "cloud", t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "DiskFull", Labels: map[string]string{"service": "a\x00b"}, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}}, "ana", opts); err != nil {
		t.Errorf("copied, the same firing sent again: %v", err)
	}
}

// A JSON text Postgres reads as jsonb has its NULs, and only those,
// written U+FFFD (pgJSON); any other text keeps them as written (pgText).
func TestJSONNUL(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":"x\u0000y"}`:           "{\"a\":\"x�y\"}",
		`{"a\u0000":"\u0000\u0000"}`: "{\"a�\":\"��\"}",
		`{"a":"x\\u0000y"}`:          `{"a":"x\\u0000y"}`,
		`{"a":"\"\u0000"}`:           "{\"a\":\"\\\"�\"}",
		`["\\\u0000"]`:               "[\"\\\\�\"]",
		`{"a":"\u00001"}`:            "{\"a\":\"�1\"}",
	} {
		if got := pgJSON(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
		if got := pgText(in); got != in {
			t.Errorf("%s, not read as jsonb: %s", in, got)
		}
	}
	if got := pgJSON(`not json \u0000`); got != `not json \u0000` {
		t.Errorf("a text that isn't JSON: %s", got)
	}
	// A parameter read as jsonb, and that one alone.
	args := []driver.NamedValue{{Ordinal: 1, Value: `["a\u0000"]`}, {Ordinal: 2, Value: `["a\u0000"]`}}
	jsonParams("SELECT value FROM jsonb_array_elements_text(CAST($1 AS jsonb)) WHERE $2 != ''", args)
	if args[0].Value != "[\"a\uFFFD\"]" || args[1].Value != `["a\u0000"]` {
		t.Errorf("parameters: %q, %q", args[0].Value, args[1].Value)
	}
}

// An incident with a NUL in a label or its title, written again as it is,
// is the same incident in either database: SyncFile finds nothing changed,
// a copier's Put writes nothing, the second time. A text that is a JSON
// document, a task's body pasted from a log, is kept as written, its
// \u0000 too: only what Postgres reads as jsonb is written otherwise.
func TestNULSameAgain(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	in := incident.Incident{ID: "INC-9", Title: "Checkout\x00 down", Start: t0.Add(2 * time.Hour), End: t0.Add(3 * time.Hour),
		Labels: incident.Labels{"service": {"checkout\x00"}}}
	for i := range 3 {
		changed, err := l.SyncFile(ctx, []incident.Incident{in}, opts)
		if err != nil || changed != (i == 0) {
			t.Errorf("SyncFile %d: changed %v, %v", i+1, changed, err)
		}
	}
	c := incident.Incident{ID: "CP-1", Title: "Copied\x00", Start: t0.Add(5 * time.Hour), End: t0.Add(6 * time.Hour),
		Labels: incident.Labels{"service": {"pay\x00"}}}
	var gens []int64
	for i := range 3 {
		if _, err := l.Put(ctx, c, Copier, opts); err != nil {
			t.Fatalf("Put %d: %v", i+1, err)
		}
		gens = append(gens, l.Gen())
	}
	if gens[1] != gens[0] || gens[2] != gens[0] {
		t.Errorf("a copier's Put of the same incident wrote again: generations %v", gens)
	}
	body := `{"msg":"read \u0000 at offset 3"}`
	id, err := l.PutTask(ctx, Task{Title: "a log", Body: body}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := l.TaskByID(ctx, id); err != nil || got.Body != body {
		t.Errorf("a task's body as written: %q %v", got.Body, err)
	}
}
