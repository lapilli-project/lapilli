// Package ledgertest gives a test a Postgres ledger of its own when
// LAPILLI_TEST_POSTGRES names a server: a schema made for the test in the
// database it names, dropped when the test ends. Without it, the tests
// keep their ledgers in SQLite, as ever.
//
//	LAPILLI_TEST_POSTGRES='postgres://postgres@127.0.0.1:55433/postgres?sslmode=disable' go test ./...
package ledgertest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Env is the variable that names the server, as a postgres:// URL.
const Env = "LAPILLI_TEST_POSTGRES"

// On says whether the tests' ledgers are in Postgres.
func On() bool { return os.Getenv(Env) != "" }

// URL is a connection string for a schema of its own in the database Env
// names, made empty for the test and dropped after it; "" without Env.
func URL(t testing.TB) string {
	t.Helper()
	base := os.Getenv(Env)
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("%s: want a postgres:// URL", Env)
	}
	b := make([]byte, 6)
	rand.Read(b)
	schema := "lt_" + hex.EncodeToString(b)
	run(t, base, "CREATE SCHEMA "+schema)
	t.Cleanup(func() { run(t, base, "DROP SCHEMA "+schema+" CASCADE") })
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// DatabaseFile is a file holding a connection string for a database of its
// own on the server Env names, made for the test and dropped after it: for
// a test that ends every session holding an advisory lock, which are a
// database's, and would end other tests' in a database shared. "" without
// Env.
func DatabaseFile(t testing.TB) string {
	t.Helper()
	base := os.Getenv(Env)
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s: want a postgres:// URL", Env)
	}
	b := make([]byte, 6)
	rand.Read(b)
	db := "lt_" + hex.EncodeToString(b)
	run(t, base, "CREATE DATABASE "+db)
	t.Cleanup(func() { run(t, base, "DROP DATABASE "+db+" WITH (FORCE)") })
	u.Path = "/" + db
	f := filepath.Join(t.TempDir(), "ledger-url")
	if err := os.WriteFile(f, []byte(u.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// File is URL written to a file of its own, as --ledger-url-file takes it;
// "" without Env.
func File(t testing.TB) string {
	t.Helper()
	u := URL(t)
	if u == "" {
		return ""
	}
	f := filepath.Join(t.TempDir(), "ledger-url")
	if err := os.WriteFile(f, []byte(u+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// SQLiteOnly skips a test of what only SQLite has.
func SQLiteOnly(t testing.TB, why string) {
	t.Helper()
	if On() {
		t.Skip("SQLite's alone: " + why)
	}
}

func run(t testing.TB, conn, q string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := pgx.Connect(ctx, conn)
	if err != nil {
		t.Fatalf("%s: %v", Env, err)
	}
	defer c.Close(ctx)
	// A deadlock is tried again: dropping a schema whose ledger's last
	// session the server hasn't finished ending yet can deadlock with it
	// (seen on Postgres 15 in CI), and the next try finds it gone.
	for try := 1; ; try++ {
		_, err := c.Exec(ctx, q)
		var pe *pgconn.PgError
		if err == nil {
			return
		}
		if !errors.As(err, &pe) || pe.Code != "40P01" || try == 5 {
			t.Fatalf("%s: %v", q, err)
		}
		time.Sleep(time.Duration(try) * 200 * time.Millisecond)
	}
}

// Digest is a digest of every row of every table in the schema a
// connection string's search_path names first: the same while nothing is
// written to them. Postgres's own counts of rows written
// (pg_stat_user_tables) are kept when a session's statistics are flushed,
// not as it writes, so a test of no writes reads the rows.
func Digest(t testing.TB, conn string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := pgx.Connect(ctx, conn)
	if err != nil {
		t.Fatalf("%s: %v", Env, err)
	}
	defer c.Close(ctx)
	rows, err := c.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, name := range tables {
		var sum string
		q := "SELECT coalesce(md5(string_agg(md5(x::text), '' ORDER BY md5(x::text))), '') FROM " + pgx.Identifier{name}.Sanitize() + " x"
		if err := c.QueryRow(ctx, q).Scan(&sum); err != nil {
			t.Fatal(err)
		}
		b.WriteString(name + ":" + sum + "\n")
	}
	return b.String()
}
