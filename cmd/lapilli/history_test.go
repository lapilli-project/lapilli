package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// historyOf writes versions of a rule, by "for", into a directory of the
// kind --rules-history reads.
func historyOf(t *testing.T, versions map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, forDur := range versions {
		v := filepath.Join(dir, name)
		os.MkdirAll(v, 0o755)
		body := "groups:\n  - name: g\n    rules:\n      - {alert: A, expr: a > 1, for: " + forDur + "}\n"
		if err := os.WriteFile(filepath.Join(v, "rules.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// For verdict, --rules-history DIR is each of its versions given as --rules
// DIR@TIME.
func TestVerdictRulesHistory(t *testing.T) {
	hist := historyOf(t, map[string]string{"2026-10-01T10:00:00Z": "2m", "20261020T083000Z": "15m"})
	inc := write(t, "incidents.yaml", "incidents: []\n")
	common := []string{"--prometheus", "http://x", "--incidents", inc}
	f, err := parseVerdictFlags(append([]string{"--rules-history", hist}, common...))
	if err != nil {
		t.Fatal(err)
	}
	g, err := parseVerdictFlags(append([]string{
		"--rules", filepath.Join(hist, "2026-10-01T10:00:00Z") + "@2026-10-01T10:00:00Z",
		"--rules", filepath.Join(hist, "20261020T083000Z") + "@2026-10-20T08:30:00Z"}, common...))
	if err != nil {
		t.Fatal(err)
	}
	same := func(a, b ruleFile) bool { return a.path == b.path && a.from.Equal(b.from) }
	if !slices.EqualFunc(f.rules, g.rules, same) {
		t.Errorf("--rules-history %v, --rules @TIME %v", f.rules, g.rules)
	}
	// Read as versions: only the latest from before the period matters, and
	// the first is then from before it.
	f.opts.From, f.opts.To = time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	vs, err := loadVersions(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || vs[0].Rules[0].For != 2*time.Minute || vs[1].Rules[0].For != 15*time.Minute {
		t.Errorf("versions %+v", vs)
	}
	f.opts.From = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	if vs, err := loadVersions(f); err != nil || len(vs) != 1 || vs[0].Rules[0].For != 15*time.Minute {
		t.Errorf("a period after the last change: %+v %v", vs, err)
	}
	f.opts.From = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err := loadVersions(f); err == nil || !strings.Contains(err.Error(), "after the period starts") {
		t.Errorf("a period before the history: %v", err)
	}
}

func TestRulesHistoryRefused(t *testing.T) {
	hist := historyOf(t, map[string]string{"2026-10-01T10:00:00Z": "2m"})
	bad := historyOf(t, map[string]string{"last-week": "2m"})
	rules := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: A, expr: a > 1}\n")
	inc := write(t, "incidents.yaml", "incidents: []\n")
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"verdict", "--rules-history", bad, "--prometheus", "http://x", "--incidents", inc}, "last-week"},
		{[]string{"verdict", "--rules-history", hist, "--loaded-rules", "--prometheus", "http://x", "--incidents", inc}, "goes with --rules, not --loaded-rules"},
		{[]string{"verdict", "--rules-history", hist, "--grafana", "http://g", "--prometheus", "http://x", "--incidents", inc}, "goes with --rules, not --loaded-rules"},
		{[]string{"verdict", "--rules-history", hist, "--rules", rules, "--compare", rules, "--prometheus", "http://x", "--incidents", inc}, "leave out --rules-history"},
		{[]string{"serve", "--rules-history", hist, "--prometheus", "http://x"}, "give --rules as well"},
		{[]string{"serve", "--rules-history", hist, "--loaded-rules", "--prometheus", "http://x"}, "goes with --rules, not --loaded-rules"},
		{[]string{"serve", "--rules-history", bad, "--rules", rules, "--prometheus", "http://x"}, "last-week"},
	} {
		var errs strings.Builder
		if code := run(c.args, io.Discard, &errs); code != 2 || !strings.Contains(errs.String(), c.says) {
			t.Errorf("%v: exit %d, want 2 saying %q:\n%s", c.args, code, c.says, errs.String())
		}
	}
	// serve keeps the directory for itself, beside the one --rules.
	f, err := parseFlags([]string{"--rules", rules, "--rules-history", hist, "--prometheus", "http://x"}, func(string, string) (bool, error) { return false, nil })
	if err != nil || f.rulesHistory != hist || len(f.rules) != 1 {
		t.Errorf("serve: %+v %v", f, err)
	}
}
