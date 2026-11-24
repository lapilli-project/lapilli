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
	parallel(t)
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

// An entry may say judged: false in place of signals; a file whose
// entries all say it needs no signal; signals: [] is refused naming it,
// and judged: false beside signals, or with true, is refused.
func TestImpactFileJudgedFalse(t *testing.T) {
	parallel(t)
	signals := "signals:\n  - {name: probe, expr: probe_success == 0}\n"
	for _, c := range []struct{ name, text, want string }{
		{"signals: []", signals + "rules:\n  - {rules: ^Watchdog$, signals: []}\n", "judged: false"},
		{"both", signals + "rules:\n  - {rules: ^Watchdog$, signals: [probe], judged: false}\n", "not both"},
		{"judged: true", signals + "rules:\n  - {rules: ^Watchdog$, judged: true}\n", "judged: true"},
		{"a match with it", signals + "rules:\n  - {rules: ^Watchdog$, judged: false, match: [service]}\n", "how they are judged"},
		{"no signal, an entry judged", "rules:\n  - {rules: ^Watchdog$, judged: false}\n  - {rules: ^Errors$, signals: [probe]}\n", "no signals"},
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
	if err := os.WriteFile(path, []byte("rules:\n  - {rules: ^Watchdog$, judged: false}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sigs, es, err := readImpactFile(path)
	if err != nil || len(sigs) != 0 || len(es) != 1 || !es[0].NotJudged() || es[0].Signals != nil {
		t.Errorf("entries judged by nothing alone: %v %+v %v", sigs, es, err)
	}
	f := &verdictFlags{impactFile: path}
	if err := f.signalsRead(); err != nil || f.opts.Signals == nil || f.opts.Signals.Named() || !f.opts.NotJudged("Watchdog") {
		t.Errorf("read: %v %+v", err, f.opts.Signals)
	}
	if _, es := f.signalDefs(); len(es) != 1 {
		t.Errorf("serve is given %d entries, want 1", len(es))
	}
	// With the signal with no name beside it.
	f = &verdictFlags{impactFile: path, impact: "up == 0"}
	if err := f.signalsRead(); err != nil {
		t.Errorf("with --impact PROMQL: %v", err)
	}
}
