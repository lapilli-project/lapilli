package ledger

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// Every text column of a Postgres ledger compares byte by byte, as
// SQLite's does: each is declared COLLATE "C". The end-to-end test on a
// real Postgres checks the columns it finds; this checks each file as it
// is written, so a column added without it fails here first. A cast (AS
// TEXT) isn't a column, and comments are left out.
func TestPostgresTextIsCollatedC(t *testing.T) {
	files, err := fs.Glob(pgSchema, "schema/postgres/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no Postgres schema files: %v", err)
	}
	column := regexp.MustCompile(`\bTEXT\b`)
	for _, f := range files {
		b, err := fs.ReadFile(pgSchema, f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if c := strings.Index(line, "--"); c >= 0 {
				line = line[:c]
			}
			for _, at := range column.FindAllStringIndex(line, -1) {
				before := strings.TrimSpace(line[:at[0]])
				if strings.HasSuffix(strings.ToUpper(before), " AS") || strings.EqualFold(before, "AS") {
					continue
				}
				if !strings.HasPrefix(strings.TrimSpace(line[at[1]:]), `COLLATE "C"`) {
					t.Errorf("%s:%d: a text column not declared COLLATE \"C\": %s", f, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
}
