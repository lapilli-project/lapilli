package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
)

// lapilli ledger copy copies a SQLite ledger into an empty one, Postgres's
// when LAPILLI_TEST_POSTGRES names a server, and says so; one copied into
// a second time is refused, as is a ledger that isn't there. No error says
// the connection string.
func TestLedgerCopy(t *testing.T) {
	ctx := context.Background()
	from := filepath.Join(t.TempDir(), "ledger")
	os.MkdirAll(from, 0o750)
	l, err := ledger.Open(ctx, filepath.Join(from, "ledger.db"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SetMeta(ctx, "ledger-id", "0123456789ab"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetSettings(ctx, map[string]string{"lead": "20m"}, "ana"); err != nil {
		t.Fatal(err)
	}
	l.Close()
	to := []string{"--to", filepath.Join(t.TempDir(), "copy")}
	if f := ledgertest.File(t); f != "" {
		to = []string{"--to-url-file", f}
	}
	var out, errs bytes.Buffer
	if code := run(append([]string{"ledger", "copy", "--ledger", from}, to...), &out, &errs); code != 0 || !strings.Contains(out.String(), "read them back alike") ||
		!strings.Contains(errs.String(), "copied settings: 1 rows") {
		t.Fatalf("%d\n%s\n%s", code, out.String(), errs.String())
	}
	out.Reset()
	errs.Reset()
	if code := run(append([]string{"ledger", "copy", "--ledger", from}, to...), &out, &errs); code != 1 || !strings.Contains(errs.String(), "isn't empty") {
		t.Errorf("copied into twice: %d %s", code, errs.String())
	}
	errs.Reset()
	if code := run([]string{"ledger", "copy", "--ledger", filepath.Join(t.TempDir(), "none"), "--to", t.TempDir()}, &out, &errs); code != 1 ||
		!strings.Contains(errs.String(), "no ledger in") {
		t.Errorf("a ledger that isn't there: %d %s", code, errs.String())
	}
	bad := filepath.Join(t.TempDir(), "url")
	os.WriteFile(bad, []byte("postgres://ana:s3cretPassw0rd@db.example:abc/ledger\n"), 0o600)
	errs.Reset()
	if code := run([]string{"ledger", "copy", "--ledger", from, "--to-url-file", bad}, &out, &errs); code != 1 ||
		strings.Contains(errs.String(), "s3cret") || strings.Contains(errs.String(), "db.example") {
		t.Errorf("a connection string that doesn't parse: %d %s", code, errs.String())
	}
	for _, args := range [][]string{
		{"ledger", "copy", "--to", t.TempDir()},
		{"ledger", "copy", "--ledger", from},
		{"ledger", "copy", "--ledger", from, "--ledger-url-file", bad, "--to", t.TempDir()},
		{"ledger", "copy", "--ledger", from, "--to", from},
		{"ledger", "move"},
	} {
		if code := run(args, &out, &errs); code != 2 {
			t.Errorf("%q: %d, want 2", args, code)
		}
	}
}
