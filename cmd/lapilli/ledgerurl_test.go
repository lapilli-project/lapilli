package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lapilli serve takes --ledger-url-file, refused beside --ledger, and an
// empty file is refused at once.
func TestServeLedgerURLFile(t *testing.T) {
	rulesFile := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: A, expr: a}\n")
	empty := filepath.Join(t.TempDir(), "url")
	os.WriteFile(empty, []byte("\n"), 0o600)
	var out, errs bytes.Buffer
	if code := run([]string{"serve", "--rules", rulesFile, "--prometheus", "http://x", "--ledger-url-file", empty}, &out, &errs); code != 2 ||
		!strings.Contains(errs.String(), "the file is empty") {
		t.Errorf("an empty file: %d %s", code, errs.String())
	}
	full := filepath.Join(t.TempDir(), "url")
	os.WriteFile(full, []byte("postgres://ana:s3cretPassw0rd@127.0.0.1:1/ledger?sslmode=disable&connect_timeout=2\n"), 0o600)
	errs.Reset()
	if code := run([]string{"serve", "--rules", rulesFile, "--prometheus", "http://x", "--ledger", t.TempDir(), "--ledger-url-file", full}, &out, &errs); code != 2 ||
		!strings.Contains(errs.String(), "give one") {
		t.Errorf("--ledger beside it: %d %s", code, errs.String())
	}
	errs.Reset()
	if code := run([]string{"serve", "--rules", rulesFile, "--prometheus", "http://127.0.0.1:1", "--ledger-url-file", full, "--listen", "127.0.0.1:0"}, &out, &errs); code != 1 ||
		!strings.Contains(errs.String(), "can't connect to the ledger's database") || strings.Contains(errs.String(), "s3cret") || strings.Contains(errs.String(), "ana") {
		t.Errorf("a server that isn't there: %d %s", code, errs.String())
	}
}
