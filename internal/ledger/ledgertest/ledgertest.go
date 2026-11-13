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
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
	if _, err := c.Exec(ctx, q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}
