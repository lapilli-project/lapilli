package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
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
		{[]string{"serve", "--rules-history", hist, "--loaded-rules", "--prometheus", "http://x"}, "goes with --rules, not --loaded-rules"},
		{[]string{"serve", "--rules-history", bad, "--rules", rules, "--prometheus", "http://x"}, "last-week"},
	} {
		var errs strings.Builder
		if code := run(c.args, io.Discard, &errs); code != 2 || !strings.Contains(errs.String(), c.says) {
			t.Errorf("%v: exit %d, want 2 saying %q:\n%s", c.args, code, c.says, errs.String())
		}
	}
	// serve keeps the directory for itself, beside the one --rules; or,
	// without it, follows the directory.
	f, err := parseFlags([]string{"--rules", rules, "--rules-history", hist, "--prometheus", "http://x"}, func(string, string) (bool, error) { return false, nil })
	if err != nil || f.rulesHistory != hist || len(f.rules) != 1 {
		t.Errorf("serve: %+v %v", f, err)
	}
	f, err = parseFlags([]string{"--rules-history", hist, "--rules-select", "grafana_alert=1", "--prometheus", "http://x"}, func(string, string) (bool, error) { return false, nil })
	if err != nil || f.rulesHistory != hist || len(f.rules) != 0 || f.rulesSelect.String() != "grafana_alert=1" {
		t.Errorf("serve following: %+v %v", f, err)
	}
	if _, err := parseFlags([]string{"--rules-history", hist}, func(string, string) (bool, error) { return false, nil }); err == nil {
		t.Error("serve following without a history to replay against")
	}
}

// --rules beside --rules-history are the rules as they are now: the newest
// version, after the directory's newest, as serve takes them; not the
// oldest, which only the period's start would keep.
func TestVerdictRulesNowAndHistory(t *testing.T) {
	hist := historyOf(t, map[string]string{"2026-10-01T10:00:00Z": "2m", "20261020T083000Z": "15m"})
	now := filepath.Join(historyOf(t, map[string]string{"x": "30m"}), "x")
	inc := write(t, "incidents.yaml", "incidents: []\n")
	f, err := parseVerdictFlags([]string{"--rules", now, "--rules-history", hist, "--prometheus", "http://x", "--incidents", inc})
	if err != nil {
		t.Fatal(err)
	}
	f.opts.From, f.opts.To = time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	vs, err := loadVersions(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 3 || vs[0].Rules[0].For != 2*time.Minute || vs[2].Rules[0].For != 30*time.Minute ||
		!vs[2].From.Equal(time.Date(2026, 10, 20, 8, 30, 1, 0, time.UTC)) {
		for _, v := range vs {
			t.Logf("from %s for %s", v.From, v.Rules[0].For)
		}
		t.Errorf("want 2m, 15m from 10-20 08:30, and the rules given as now from a second after")
	}
}

// renderedChart is a chart rendered whole: a rule in a ConfigMap labelled
// for Grafana's sidecar, as of "for", another in one that isn't, and a
// Deployment.
func renderedChart(forDur string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata: {name: shop}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: a, labels: {grafana_alert: "1"}}
data:
  a.yaml: |
    groups: [{name: g, rules: [{alert: A, expr: a > 1, for: ` + forDur + `}]}]
---
apiVersion: v1
kind: ConfigMap
metadata: {name: b}
data:
  b.yaml: |
    groups: [{name: g, rules: [{alert: B, expr: b > 1}]}]
`
}

// --rules-select keeps, of the objects in the rule files, the labelled
// ones, in --rules and in each version of --rules-history alike; what was
// passed over is said, once.
func TestRulesSelect(t *testing.T) {
	hist := t.TempDir()
	for name, forDur := range map[string]string{"20261001T100000Z": "2m", "20261020T083000Z": "15m"} {
		os.MkdirAll(filepath.Join(hist, name), 0o755)
		os.WriteFile(filepath.Join(hist, name, "rules.yaml"), []byte(renderedChart(forDur)), 0o644)
	}
	inc := write(t, "incidents.yaml", "incidents: []\n")
	f, err := parseVerdictFlags([]string{"--rules-history", hist, "--rules-select", "grafana_alert=1", "--prometheus", "http://x", "--incidents", inc})
	if err != nil {
		t.Fatal(err)
	}
	f.opts.From, f.opts.To = time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	vs, err := loadVersions(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || len(vs[0].Rules) != 1 || vs[0].Rules[0].Name != "A" || vs[1].Rules[0].For != 15*time.Minute {
		t.Errorf("versions %+v", vs)
	}
	var said strings.Builder
	f.sayPicked(&said)
	if said.String() != "lapilli verdict: the rule files read held 2 documents that hold no rules, and 2 documents of rules the selector doesn't match: passed over\n" {
		t.Errorf("said %q", said.String())
	}
	if f.sayPicked(&said); strings.Count(said.String(), "\n") != 1 {
		t.Errorf("said twice: %q", said.String())
	}
	// Without it, both rules, and the Deployments still passed over.
	g, _ := parseVerdictFlags([]string{"--rules-history", hist, "--prometheus", "http://x", "--incidents", inc})
	g.opts = f.opts
	if vs, err := loadVersions(g); err != nil || len(vs[0].Rules) != 2 || g.picked != (rules.Picked{Other: 2}) {
		t.Errorf("without a selector: %+v %+v %v", vs, g.picked, err)
	}
	// A selector nothing matches says so.
	h, _ := parseVerdictFlags([]string{"--rules", filepath.Join(hist, "20261001T100000Z", "rules.yaml"), "--rules-select", "team=pay", "--prometheus", "http://x", "--incidents", inc})
	if _, err := loadVersions(h); err == nil || !strings.Contains(err.Error(), "--rules-select") || !strings.Contains(err.Error(), "2 documents of rules have other labels") {
		t.Errorf("none matched: %v", err)
	}

	r := write(t, "rules.yaml", renderedChart("2m"))
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"verdict", "--rules", r, "--rules-select", "a in (b", "--prometheus", "http://x", "--incidents", inc}, "--rules-select"},
		{[]string{"verdict", "--loaded-rules", "--rules-select", "a=b", "--prometheus", "http://x", "--incidents", inc}, "give --rules or --rules-history"},
		{[]string{"serve", "--loaded-rules", "--rules-select", "a=b", "--prometheus", "http://x"}, "give --rules or --rules-history"},
	} {
		var errs strings.Builder
		if code := run(c.args, io.Discard, &errs); code != 2 || !strings.Contains(errs.String(), c.says) {
			t.Errorf("%v: exit %d, want 2 saying %q:\n%s", c.args, code, c.says, errs.String())
		}
	}
}

// --changes writes a change to where the rules are kept: a followed
// history alone has no file to write to, and is refused at start.
func TestChangesFollowedRefused(t *testing.T) {
	hist := historyOf(t, map[string]string{"2026-10-01T10:00:00Z": "2m"})
	var errs strings.Builder
	args := []string{"serve", "--rules-history", hist, "--prometheus", "http://x", "--changes", "--user-header", "X-Forwarded-Email", "--admins", "ana@example.com",
		"--ledger", t.TempDir(), "--listen", "127.0.0.1:0"}
	if code := run(args, io.Discard, &errs); code != 2 || !strings.Contains(errs.String(), "followed alone, --rules-history is versions a recipe wrote") {
		t.Errorf("exit %d:\n%s", code, errs.String())
	}
}
