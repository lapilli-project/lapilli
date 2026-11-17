package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --impact-file is read as written: an anchor or an alias, which would
// repeat a value past what the file's bytes bound, is refused, and a rule
// entry's labels are held as a team's are (64 values of 256 bytes a label).
func TestImpactFileBounded(t *testing.T) {
	signals := "signals:\n  - {name: probe, expr: probe_success == 0}\n"
	for _, c := range []struct{ name, text, want string }{
		{"an alias", signals + "rules:\n  - rules: &r ^Errors$\n    signals: [probe]\n  - rules: *r\n    signals: [probe]\n", "anchor or alias"},
		{"!!binary in a list", signals + "rules:\n  - labels: {service: [!!binary " + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\xff", 200))) + "]}\n    signals: [probe]\n", "!!str"},
		{"a value of 300 bytes", signals + "rules:\n  - labels: {service: " + strings.Repeat("y", 300) + "}\n    signals: [probe]\n", "values of up to 256"},
	} {
		path := filepath.Join(t.TempDir(), "impact.yaml")
		if err := os.WriteFile(path, []byte(c.text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readImpactFile(path); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	path := filepath.Join(t.TempDir(), "impact.yaml")
	if err := os.WriteFile(path, []byte(signals+"rules:\n  - {rules: ^Errors$, signals: [probe], labels: {service: [a, b]}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readImpactFile(path); err != nil {
		t.Errorf("as written: %v", err)
	}
}
