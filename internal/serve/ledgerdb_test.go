package serve

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
)

// ledgerExec runs a statement on a server's ledger from outside lapilli,
// as whoever could reach it would: on its SQLite file, or in its Postgres
// schema. The statement's parameters are ?, numbered for Postgres.
func ledgerExec(t *testing.T, s *Server, q string, args ...any) {
	t.Helper()
	db := ledgerDB(t, s)
	defer db.Close()
	if s.cfg.LedgerURLFile != "" {
		n := 0
		q = replaceEach(q, "?", func() string { n++; return "$" + strconv.Itoa(n) })
	}
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

// ledgerDB is a server's ledger opened from outside lapilli.
func ledgerDB(t *testing.T, s *Server) *sql.DB {
	t.Helper()
	if s.cfg.LedgerURLFile != "" {
		b, err := os.ReadFile(s.cfg.LedgerURLFile)
		if err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("pgx", strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	db, err := sql.Open("sqlite", filepath.Join(s.cfg.Ledger, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// urlWith is a connection string file's string with a parameter set, in a
// file of its own.
func urlWith(t *testing.T, file, key, value string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	f := filepath.Join(t.TempDir(), "ledger-url")
	if err := os.WriteFile(f, []byte(u.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func replaceEach(s, old string, with func() string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, old)
		if i < 0 {
			return b.String() + s
		}
		b.WriteString(s[:i] + with())
		s = s[i+len(old):]
	}
}

// ledgerHolds says a text is anywhere in a server's ledger: in its SQLite
// file's bytes, or in any row of its Postgres schema.
func ledgerHolds(t *testing.T, s *Server, text string) bool {
	t.Helper()
	if s.cfg.LedgerURLFile == "" {
		b, err := os.ReadFile(filepath.Join(s.cfg.Ledger, "ledger.db"))
		if err != nil {
			t.Fatal(err)
		}
		return bytes.Contains(b, []byte(text))
	}
	db := ledgerDB(t, s)
	defer db.Close()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema()")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	if len(tables) < 10 {
		t.Fatalf("the ledger's tables: %v", tables)
	}
	for _, tb := range tables {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+tb+" x WHERE strpos(x::text, $1) > 0", text).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return true
		}
	}
	return false
}

// serve stops when its ledger's lock is lost for good: another process may
// write the ledger, and this one writes nothing more, nor sends a notice.
func TestRunStopsWhenTheLockIsLost(t *testing.T) {
	if !ledgertest.On() {
		t.Skip("the ledgers are SQLite's")
	}
	s, _ := server(t, func(c *Config) { c.LedgerURLFile = ledgertest.DatabaseFile(t) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ran := make(chan error, 1)
	go func() { ran <- s.Run(ctx) }()
	db := ledgerDB(t, s)
	defer db.Close()
	// A process takes the ledger alone meanwhile, and writes: it waits for
	// the lock, and serve's sessions end. A replica's sessions (the lease's,
	// the side pool's, its refreshes') come back within a second, but a try
	// for the lock shared doesn't pass one waiting for it alone, so the
	// process gets it. The database is the test's own, as are the locks in
	// it; pg_locks is the whole server's: those of this database alone.
	const here = "locktype = 'advisory' AND objsubid = 2 AND database = (SELECT oid FROM pg_database WHERE datname = current_database())"
	var class, key int64
	for class == 0 {
		if ctx.Err() != nil {
			t.Fatal("serve never held its ledger's lock")
		}
		db.QueryRowContext(ctx, "SELECT classid::bigint, objid::bigint FROM pg_locks WHERE granted AND mode = 'ShareLock' AND "+here+" LIMIT 1").Scan(&class, &key)
		time.Sleep(10 * time.Millisecond)
	}
	alone, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer alone.Close()
	var pid int
	alone.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid)
	took := make(chan error, 1)
	go func() {
		_, err := alone.ExecContext(ctx, "SELECT pg_advisory_lock($1, $2)", int32(uint32(class)), int32(uint32(key)))
		took <- err
	}()
	for waits := false; !waits; {
		db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_locks WHERE NOT granted AND pid = $1 AND "+here+")", pid).Scan(&waits)
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := db.ExecContext(ctx, "SELECT pg_terminate_backend(pid, 1000) FROM pg_locks WHERE granted AND pid <> $1 AND "+here, pid); err != nil {
		t.Fatal(err)
	}
	if err := <-took; err != nil {
		t.Fatalf("taking the ledger alone: %v", err)
	}
	// What a process alone does first, when it opens the ledger.
	if _, err := alone.ExecContext(ctx, "UPDATE schema_version SET writers = writers + 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := alone.ExecContext(ctx, "SELECT pg_advisory_unlock($1, $2)", int32(uint32(class)), int32(uint32(key))); err != nil {
		t.Fatal(err)
	}
	// serve finds out at its next write.
	s.l.SetMeta(ctx, "after", "1")
	select {
	case err := <-ran:
		if !errors.Is(err, ledger.ErrLockLost) {
			t.Errorf("Run: %v, want ErrLockLost", err)
		}
	case <-ctx.Done():
		t.Fatal("serve went on")
	}
}

// A notice found due is sent only while this process holds the ledger:
// one whose writer's session went, while a process took the ledger alone,
// sends nothing.
func TestSendDueHoldsTheLedger(t *testing.T) {
	if !ledgertest.On() {
		t.Skip("the ledgers are SQLite's")
	}
	var sent atomic.Int64
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent.Add(1) }))
	defer recv.Close()
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte(recv.URL+"/hook"), 0o600)
	s, _ := server(t, func(c *Config) {
		c.UserHeader, c.NotifyWebhook = "X-Forwarded-Email", hook
		c.LedgerURLFile = ledgertest.DatabaseFile(t)
	})
	ctx := context.Background()
	if code, body := api(t, s.Handler(), "POST", "/api/v1/tasks", `{"title": "tune Errors", "rule": "Errors"}`); code != http.StatusCreated {
		t.Fatalf("task: %d %s", code, body)
	}
	db := ledgerDB(t, s)
	defer db.Close()
	const here = "locktype = 'advisory' AND granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())"
	if _, err := db.ExecContext(ctx, "SELECT pg_terminate_backend(pid, 5000) FROM pg_locks WHERE "+here); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(s.cfg.LedgerURLFile)
	other, err := ledger.OpenPostgresAlone(ctx, strings.TrimSpace(string(b)), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	for range 2 {
		s.sendDue(ctx, "webhook", time.Now().Add(time.Minute))
	}
	if n := sent.Load(); n != 0 {
		t.Errorf("a process whose lock another took sent %d notices", n)
	}
}
