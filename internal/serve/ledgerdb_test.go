package serve

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
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
