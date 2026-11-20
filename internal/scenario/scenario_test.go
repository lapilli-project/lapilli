package scenario

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/scenario/scenariotest"
	"lapilli.dev/internal/verdict"
)

func TestMain(m *testing.M) {
	scenariotest.Fake()
	os.Exit(m.Run())
}

// shop is a group of a recording rule and two alert rules, one told apart
// from another of its name, and a Grafana rule promtool doesn't run.
func shop() ([]rules.Rule, []rules.Recording) {
	alerts := []rules.Rule{
		{Name: "CheckoutErrors [severity=page]", Alert: "CheckoutErrors", Group: "shop", Source: "alerts/shop.yaml", Expr: "errors:rate5m > 5",
			For: 10 * time.Minute, KeepFiringFor: 90 * time.Second, Interval: 30 * time.Second, Limit: 10, GroupLabels: map[string]string{"team": "payments"},
			Labels: map[string]string{"severity": "page"}, Prometheus: true,
			Says: rules.Says{Summary: "Checkout fails", Annotations: map[string]string{"runbook_url": "https://runbooks.example/checkout"}}},
		{Name: "Quiet", Group: "shop", Source: "alerts/shop.yaml", Expr: "vector(0) > 1", Prometheus: true},
		{Name: "Panel", Group: "shop", Source: "alerts/shop.yaml", Graph: &rules.Graph{}, Prometheus: false},
	}
	recs := []rules.Recording{{Record: "errors:rate5m", Group: "shop", Source: "alerts/shop.yaml", Expr: "rate(errors[5m])", Labels: map[string]string{"team": "payments"}}}
	return alerts, recs
}

// A group is written as a Prometheus rule file holds it: its own settings,
// its recording rules first, each alert rule under the name its alerts
// carry, with what it says; what promtool doesn't run left out.
func TestGroup(t *testing.T) {
	alerts, recs := shop()
	b, err := Group(alerts, recs)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Groups []struct {
			Name, Interval string
			Limit          int
			Labels         map[string]string
			Rules          []map[string]any
		}
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Groups) != 1 {
		t.Fatalf("%s", b)
	}
	g := f.Groups[0]
	if g.Name != "shop" || g.Interval != "30s" || g.Limit != 10 || g.Labels["team"] != "payments" || len(g.Rules) != 3 {
		t.Fatalf("%s", b)
	}
	if g.Rules[0]["record"] != "errors:rate5m" || g.Rules[1]["alert"] != "CheckoutErrors" || g.Rules[1]["for"] != "10m" ||
		g.Rules[1]["keep_firing_for"] != "1m30s" || g.Rules[2]["alert"] != "Quiet" {
		t.Errorf("%s", b)
	}
	a, _ := g.Rules[1]["annotations"].(map[string]any)
	if a["summary"] != "Checkout fails" || a["runbook_url"] != "https://runbooks.example/checkout" {
		t.Errorf("annotations: %s", b)
	}
	if strings.Contains(string(b), "Panel") {
		t.Errorf("a Grafana rule written: %s", b)
	}
	if _, err := Group(alerts[2:], nil); err == nil {
		t.Error("a group of nothing promtool runs written")
	}
}

// A test file is run as promtool reads one, but for what it may make
// promtool read: whatever its rule_files named, the group Lapilli wrote
// alone; its group_eval_order left out. A key promtool's files don't have,
// one with no tests, more than one document, or aliases past what the
// rules may make, aren't run.
func TestRewrite(t *testing.T) {
	b, err := Rewrite([]byte(scenariotest.Test("pass", "/etc/passwd", "../../secret.yaml", "*.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	if err := yaml.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if files, _ := f["rule_files"].([]any); len(files) != 1 || files[0] != "rules.yaml" {
		t.Errorf("rule_files: %s", b)
	}
	if _, ok := f["group_eval_order"]; ok || f["evaluation_interval"] != "1m" || f["tests"] == nil {
		t.Errorf("%s", b)
	}
	// No rule_files at all: given ours.
	if b, err := Rewrite([]byte("tests:\n  - name: pass\n")); err != nil || !strings.Contains(string(b), "rule_files:\n  - rules.yaml\n") {
		t.Errorf("%s %v", b, err)
	}
	bomb := "a: &a [x, x, x, x, x, x, x, x, x]\nb: &b [*a, *a, *a, *a, *a, *a, *a, *a, *a]\nc: &c [*b, *b, *b, *b, *b, *b, *b, *b, *b]\nd: &d [*c, *c, *c, *c, *c, *c, *c, *c, *c]\ne: &e [*d, *d, *d, *d, *d, *d, *d, *d, *d]\nf: &f [*e, *e, *e, *e, *e, *e, *e, *e, *e]\ng: &g [*f, *f, *f, *f, *f, *f, *f, *f, *f]\nh: [*g, *g, *g, *g, *g, *g, *g, *g, *g]\ntests: [{name: pass}]\n"
	for name, text := range map[string]string{
		"unknown key": "tests: [{name: pass}]\nrun: ['.*']\n",
		"no tests":    "rule_files: [x.yaml]\n",
		"empty tests": "tests: []\n",
		"two docs":    "tests: [{name: pass}]\n---\ntests: [{name: pass}]\n",
		"a list":      "- tests\n",
		"empty":       "",
		"bomb":        bomb,
	} {
		if _, err := Rewrite([]byte(text)); err == nil {
			t.Errorf("%s: rewritten", name)
		}
	}
}

// run runs a stand-in's mode against shop's group, as Sides would.
func run(t *testing.T, p Promtool, mode string) Result {
	t.Helper()
	alerts, recs := shop()
	g, err := Group(alerts, recs)
	if err != nil {
		t.Fatal(err)
	}
	test, err := Rewrite([]byte(scenariotest.Test(mode)))
	if err != nil {
		t.Fatal(err)
	}
	return p.Run(context.Background(), g, test)
}

// What promtool answers decides: SUCCESS and 0 passed; FAILED: and 1
// failed, with what it said; anything else, a crash, an exit of another
// status, or an answer it doesn't give, couldn't be run, never failed.
func TestRun(t *testing.T) {
	p := Promtool{Path: scenariotest.Install(t)}
	if r := run(t, p, "pass"); r.State != Passed {
		t.Errorf("pass: %+v", r)
	}
	r := run(t, p, "fail")
	if r.State != Failed || !strings.HasPrefix(r.Said, "    alertname: CheckoutErrors, time: 10m,\n        exp:[0:") || strings.Contains(r.Said, "FAILED") {
		t.Errorf("fail: %+v", r)
	}
	for mode, why := range map[string]string{
		"crash":   "promtool crashed",
		"garbage": "promtool's answer isn't one it gives",
		"exit2":   "promtool exited 2, saying what it doesn't say of a test",
	} {
		if r := run(t, p, mode); r.State != Unrun || r.Why != why {
			t.Errorf("%s: %+v", mode, r)
		}
	}
	if r := run(t, Promtool{Path: filepath.Join(t.TempDir(), "none")}, "pass"); r.State != Unrun || r.Why != "promtool couldn't be started" {
		t.Errorf("none: %+v", r)
	}
	// Stopped as the run's own time ends: not a time out of promtool's.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, recs := shop()
	g, _ := Group(alerts, recs)
	test, _ := Rewrite([]byte(scenariotest.Test("pass")))
	if r := p.Run(ctx, g, test); r.State != Unrun || r.Why != "stopped: the run's time ran out" {
		t.Errorf("stopped: %+v", r)
	}
}

// Guard: promtool is started directly at the path the operator gave, a
// path a shell would split and run (Install's), with test rules test.yaml
// alone, and nothing of Lapilli's environment: a secret in it isn't
// promtool's. It reads the group Lapilli wrote, whatever the test file's
// rule_files named.
func TestRunGivenNothingElse(t *testing.T) {
	t.Setenv("LAPILLI_TEST_SECRET", "s3cret-value")
	p := Promtool{Path: scenariotest.Install(t)}
	alerts, recs := shop()
	g, _ := Group(alerts, recs)
	test, err := Rewrite([]byte(scenariotest.Test("see", "/etc/passwd", "../outside.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	r := p.Run(context.Background(), g, test)
	if r.State != Failed {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.Said, `args ["test" "rules" "test.yaml"]`) || !strings.Contains(r.Said, `env ["TMPDIR=`) ||
		strings.Contains(r.Said, "s3cret") || !strings.Contains(r.Said, "alert: CheckoutErrors") || strings.Contains(r.Said, "root:") {
		t.Errorf("%s", r.Said)
	}
}

// Guard: a run that takes longer than its limit is stopped, and couldn't
// be run: not passed, whatever promtool would have said after.
func TestRunTimeLimit(t *testing.T) {
	p := Promtool{Path: scenariotest.Install(t), Timeout: 50 * time.Millisecond}
	if r := run(t, p, "sleep"); r.State != Unrun || r.Why != "promtool took longer than 50ms" {
		t.Errorf("%+v", r)
	}
}

// Guard: what promtool says past the cap is neither kept nor read: an
// answer whose SUCCESS comes after megabytes of it is one promtool doesn't
// give.
func TestRunOutputCap(t *testing.T) {
	p := Promtool{Path: scenariotest.Install(t), Output: 4096}
	if r := run(t, p, "flood"); r.State != Unrun || r.Why != "promtool's answer isn't one it gives" {
		t.Errorf("%+v", r)
	}
	var c capped
	c.most = 10
	if n, err := c.Write([]byte(strings.Repeat("y", 100))); n != 100 || err != nil || c.b.Len() != 10 {
		t.Errorf("%d %v %d", n, err, c.b.Len())
	}
}

// Guard: a test file is read inside the directory of the expectations
// file that names it: not one a path leads outside of by .., nor by a
// symbolic link, nor past MaxTest bytes; a link inside it is followed.
func TestReadTest(t *testing.T) {
	top := t.TempDir()
	dir := filepath.Join(top, "alerts")
	os.MkdirAll(filepath.Join(dir, "tests"), 0o755)
	os.WriteFile(filepath.Join(top, "secret.yaml"), []byte("tests: [{name: secret}]\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "tests", "shop.yaml"), []byte(scenariotest.Test("pass")), 0o600)
	os.WriteFile(filepath.Join(dir, "big.yaml"), make([]byte, MaxTest+1), 0o600)
	os.Symlink(filepath.Join(top, "secret.yaml"), filepath.Join(dir, "out.yaml"))
	os.Symlink("tests/shop.yaml", filepath.Join(dir, "in.yaml"))
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, p := range []string{"tests/shop.yaml", "in.yaml"} {
		if b, err := ReadTest(root, p); err != nil || !strings.Contains(string(b), "name: \"pass\"") {
			t.Errorf("%s: %v", p, err)
		}
	}
	for p, why := range map[string]string{
		"../secret.yaml": "it leads outside the expectations file's directory",
		"out.yaml":       "it leads outside the expectations file's directory",
		"none.yaml":      "no such file",
		"tests":          "not a file",
		"big.yaml":       "more than 1048576 bytes",
	} {
		if b, err := ReadTest(root, p); err == nil || err.Error() != why || len(b) > 0 {
			t.Errorf("%s: %v %q", p, err, b)
		}
	}
}

// Sides runs each test file against each group once, whatever the rules
// it holds; reads it in the directory of the expectations file that named
// it; and says not known, never met, of a rule promtool doesn't run, one
// it doesn't have, a test file it can't read, and past MaxRuns.
func TestSides(t *testing.T) {
	link := scenariotest.Install(t)
	count := scenariotest.Counted(t, link)
	after, before := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(after, "t.yaml"), []byte(scenariotest.Test("fail-if 2h")), 0o600)
	os.WriteFile(filepath.Join(before, "t.yaml"), []byte(scenariotest.Test("pass")), 0o600)
	os.WriteFile(filepath.Join(after, "bad.yaml"), []byte("tests: [{name: pass}]\nrun: [x]\n"), 0o600)
	alerts, recs := shop()
	slow := append([]rules.Rule(nil), alerts...)
	slow[0].For = 2 * time.Hour
	s := &Sides{Promtool: Promtool{Path: link}, Rules: [2][]rules.Rule{alerts, slow}, Recorded: [2][]rules.Recording{recs, recs}}
	var err error
	if s.After, err = os.OpenRoot(after); err != nil {
		t.Fatal(err)
	}
	if s.Before, err = os.OpenRoot(before); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rule := "CheckoutErrors [severity=page]"
	if r := s.RunScenario("before", false, rule, "t.yaml"); r.State != verdict.ExpectMet || r.Test == "" {
		t.Errorf("before: %+v", r)
	}
	r := s.RunScenario("after", false, rule, "t.yaml")
	if r.State != verdict.ExpectBroken || !strings.Contains(r.Said, "got:[]") {
		t.Errorf("after: %+v", r)
	}
	// The same group and test file again, by another rule of it: not run
	// again.
	if r := s.RunScenario("after", false, "Quiet", "t.yaml"); r.State != verdict.ExpectBroken || count() != 2 {
		t.Errorf("again: %+v, %d runs", r, count())
	}
	// From --expect-before's directory: its own t.yaml.
	if r := s.RunScenario("after", true, rule, "t.yaml"); r.State != verdict.ExpectMet || count() != 3 {
		t.Errorf("before's file: %+v, %d runs", r, count())
	}
	for name, c := range map[string]struct {
		rule, path, why string
		unrun           bool
	}{
		"Grafana":    {"Panel", "t.yaml", whyNotProm, false},
		"unknown":    {"Nothing", "t.yaml", whyNoRule, false},
		"missing":    {rule, "none.yaml", whyTestUnread, true},
		"unreadable": {rule, "bad.yaml", whyTestUnread, true},
	} {
		if r := s.RunScenario("after", false, c.rule, c.path); r.State != verdict.ExpectUnknown || r.Why != c.why || r.Unrun != c.unrun {
			t.Errorf("%s: %+v", name, r)
		}
	}
	was := MaxRuns
	MaxRuns = 3
	defer func() { MaxRuns = was }()
	os.WriteFile(filepath.Join(after, "u.yaml"), []byte(scenariotest.Test("fail")), 0o600)
	if r := s.RunScenario("after", false, rule, "u.yaml"); r.State != verdict.ExpectUnknown || r.Why != whyTooMany || !r.Unrun || count() != 3 {
		t.Errorf("past MaxRuns: %+v, %d runs", r, count())
	}
}

// With a real promtool on PATH, a scenario runs as promtool runs it: a
// test that holds passes, one that doesn't fails, whatever rule_files it
// named. Skipped without one.
func TestRealPromtool(t *testing.T) {
	path, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("no promtool on PATH")
	}
	p := Promtool{Path: path}
	alerts := []rules.Rule{{Name: "CheckoutErrors", Group: "shop", Expr: `errors > 5`, For: 5 * time.Minute, Labels: map[string]string{"severity": "page"},
		Prometheus: true}}
	g, err := Group(alerts, nil)
	if err != nil {
		t.Fatal(err)
	}
	test := func(want string) []byte {
		b, err := Rewrite([]byte(`rule_files: [/etc/passwd]
tests:
  - interval: 1m
    input_series:
      - series: 'errors{service="checkout"}'
        values: '10x20'
    alert_rule_test:
      - eval_time: 10m
        alertname: CheckoutErrors
        exp_alerts:` + want))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if r := p.Run(context.Background(), g, test("\n          - exp_labels: {severity: page, service: checkout}\n")); r.State != Passed {
		t.Errorf("held: %+v", r)
	}
	if r := p.Run(context.Background(), g, test(" []\n")); r.State != Failed || !strings.Contains(r.Said, "CheckoutErrors") {
		t.Errorf("not held: %+v", r)
	}
}
