package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/scenario/scenariotest"
)

// writeIn writes files under dir, by their paths below it.
func writeIn(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// --promtool runs each scenario of a rule the change touches, before and
// after, against the rule's group as each side has it, its test file read
// in the directory of the expectations file that names it: met, broken,
// or not known with why, counted as budgets and catches are, so that
// --fail-expectations covers them. Without it, each is not known. One
// that couldn't be run after is on the summary: scenarios_unrun.
func TestCompareScenarios(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	base := write(t, "base.yaml", shopRules("5m"))
	pr := write(t, "pr.yaml", shopRules("2h"))
	exp := t.TempDir()
	writeIn(t, exp, map[string]string{
		"expectations.yaml": "expectations:\n  - rule: CheckoutErrors\n    scenarios: [tests/shop.yaml]\n",
		"tests/shop.yaml":   scenariotest.Test("fail-if 2h", "../../alerts/*.yaml"),
	})
	expect := filepath.Join(exp, "expectations.yaml")
	promtool := scenariotest.Install(t)
	lapilli := func(args ...string) (int, string, string) {
		var out, errs strings.Builder
		code := run(append([]string{"verdict", "--prometheus", "http://p", "--tz", "UTC", "--rules", pr, "--compare", base}, args...), &out, &errs)
		return code, out.String(), errs.String()
	}
	code, out, errs := lapilli("--expect", expect, "--promtool", promtool, "--changed-only", "--markdown")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	for _, want := range []string{`"expectations_broken":1`, `"scenarios_unrun":0`,
		"| `CheckoutErrors` | passes the scenario `tests/shop.yaml` | passes | fails | **✗ broken by this change** |",
		"- `CheckoutErrors`, `tests/shop.yaml`: `alertname: CheckoutErrors, time: 10m,` `got:[]`"} {
		if !strings.Contains(out, want) {
			t.Errorf("--markdown: no %q\n%s", want, out)
		}
	}
	if code, out, _ := lapilli("--expect", expect, "--promtool", promtool, "--json"); code != 0 || !strings.Contains(out, `"after_state": "broken"`) ||
		!strings.Contains(out, `"said": "    alertname: CheckoutErrors`) {
		t.Errorf("--json: exit %d\n%s", code, out)
	}
	if code, _, errs := lapilli("--expect", expect, "--promtool", promtool, "--fail-expectations"); code != 1 || !strings.Contains(errs, "breaks 1 expectations") {
		t.Errorf("--fail-expectations: exit %d\n%s", code, errs)
	}
	// Without --promtool: not known, never met; nothing fails.
	if code, out, errs := lapilli("--expect", expect, "--fail-expectations"); code != 0 || !strings.Contains(out, "not known: not run: no --promtool") ||
		!strings.Contains(out, "Expectations: 0 broken by this change, 0 broken before too, 0 met") {
		t.Errorf("without --promtool: exit %d\n%s\n%s", code, out, errs)
	}
	// A test file that isn't there: couldn't be run, and counted so.
	gone := filepath.Join(exp, "gone.yaml")
	os.WriteFile(gone, []byte("expectations:\n  - rule: CheckoutErrors\n    scenarios: [tests/none.yaml]\n"), 0o600)
	if code, out, _ := lapilli("--expect", gone, "--promtool", promtool, "--changed-only", "--markdown"); code != 0 || !strings.Contains(out, `"scenarios_unrun":1`) ||
		!strings.Contains(out, "not known: its test file can't be read") || !strings.Contains(out, "`no such file`") {
		t.Errorf("no test file: exit %d\n%s", code, out)
	}
	// --expect-before's scenarios are read in its own directory: the test
	// file before the change passes on both sides.
	was := t.TempDir()
	writeIn(t, was, map[string]string{
		"expectations.yaml": "expectations:\n  - rule: CheckoutErrors\n    scenarios: [tests/shop.yaml]\n",
		"tests/shop.yaml":   scenariotest.Test("pass"),
	})
	if code, out, _ := lapilli("--expect", expect, "--expect-before", filepath.Join(was, "expectations.yaml"), "--promtool", promtool); code != 0 ||
		!strings.Contains(out, "passes the scenario tests/shop.yaml (its test file edited in this change)") || !strings.Contains(out, "✗ broken by this change") {
		t.Errorf("--expect-before: exit %d\n%s", code, out)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--promtool", promtool}, "--promtool go with --expect"},
		{[]string{"--expect", expect, "--promtool", filepath.Join(t.TempDir(), "none")}, "--promtool "},
		{[]string{"--expect", expect, "--promtool", exp}, "is a directory"},
	} {
		if code, _, errs := lapilli(c.args...); code != 2 || !strings.Contains(errs, c.want) {
			t.Errorf("%v: exit %d\n%s", c.args, code, errs)
		}
	}
}

// With PROMTOOL, the recipe has lapilli run the scenarios EXPECT names,
// each commit's test files read beside its expectations file: a change
// that fails one, and FAIL_ON_EXPECTATIONS, fails the job; so does one
// whose scenario couldn't be run after it. A symbolic link beside EXPECT
// isn't read; PROMTOOL in the checkout, without EXPECT, or with
// LAPILLI_SERVE, is refused, as is --promtool given as an argument.
func TestBacktestRecipeScenarios(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	promtool := scenariotest.Install(t)
	count := scenariotest.Counted(t, promtool)
	expect := "expectations:\n  - rule: CheckoutErrors\n    scenarios: [tests/shop.yaml]\n"
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "checks/expectations.yaml": expect,
		"checks/tests/shop.yaml": scenariotest.Test("fail-if for: 2h", "../../alerts/*.yaml")})
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	settings := []string{"PATHS=alerts", "EXPECT=checks/expectations.yaml", "PROMTOOL=" + promtool}
	out, err := rr.run(append(settings, "FAIL_ON_EXPECTATIONS=true"), "--prometheus", prom.URL, "--tz", "UTC")
	if err == nil || !strings.Contains(rr.body(), "| `CheckoutErrors` | passes the scenario `tests/shop.yaml` | passes | fails | **✗ broken by this change** |") ||
		!strings.Contains(out, "the change breaks 1 expectations the rules in place meet (FAIL_ON_EXPECTATIONS=true)") || count() != 2 {
		t.Errorf("broken: %v, %d runs\n%s\n%s", err, count(), rr.body(), out)
	}
	// The pull request takes the test file away: couldn't be run after,
	// so it can't be judged.
	repo.git("checkout", "--quiet", "main")
	repo.pullRequest(map[string]string{"alerts/shop.yaml": shopRules("2h"), "checks/tests/shop.yaml": ""})
	rr.host.set(func(h *commentHost) { h.comments = nil })
	out, err = rr.run(append(settings, "FAIL_ON_EXPECTATIONS=true"), "--prometheus", prom.URL, "--tz", "UTC")
	if err == nil || !strings.Contains(out, "1 scenarios of the rules that differ couldn't be run, so they can't be judged") ||
		!strings.Contains(rr.body(), "not known: its test file can't be read") {
		t.Errorf("no test file after: %v\n%s\n%s", err, rr.body(), out)
	}
	// Not held to them: said, and the job passes.
	rr.host.set(func(h *commentHost) { h.comments = nil })
	if out, err := rr.run(settings, "--prometheus", prom.URL, "--tz", "UTC"); err != nil {
		t.Errorf("not held: %v\n%s", err, out)
	}
	// A symbolic link beside EXPECT: not read.
	repo.git("checkout", "--quiet", "main")
	repo.git("checkout", "--quiet", "-B", "pr")
	repo.write(map[string]string{"alerts/shop.yaml": shopRules("2h")})
	os.Symlink("/etc/passwd", filepath.Join(repo.dir, "checks", "tests", "passwd.yaml"))
	repo.git("add", "-A")
	repo.git("commit", "--quiet", "-m", "a link")
	repo.git("checkout", "--quiet", "--detach", "main")
	repo.git("merge", "--quiet", "--no-ff", "--no-edit", "pr")
	rr.host.set(func(h *commentHost) { h.comments = nil })
	rr.run(settings, "--prometheus", prom.URL)
	failure(t, rr.body(), "the pull request's commit holds a symbolic link beside EXPECT, which could lead to any file the job can read: not read (the job's log says where)")

	inClone := filepath.Join(repo.dir, "promtool")
	os.WriteFile(inClone, []byte("#!/bin/sh\necho SUCCESS\n"), 0o755)
	for _, c := range []struct {
		settings []string
		args     []string
		said     string
	}{
		{[]string{"PATHS=alerts", "PROMTOOL=" + promtool}, nil, "PROMTOOL runs the scenarios EXPECT holds the rules to: give EXPECT"},
		{[]string{"PATHS=alerts", "EXPECT=checks/expectations.yaml", "PROMTOOL=" + inClone}, nil, "is in the checkout, the pull request's, which it would run"},
		{[]string{"PATHS=alerts", "EXPECT=checks/expectations.yaml", "PROMTOOL=" + filepath.Join(t.TempDir(), "none")}, nil, "no such program here"},
		{[]string{"PATHS=alerts", "EXPECT=checks/expectations.yaml", "PROMTOOL=--debug"}, nil, "want promtool's path, or its name on PATH"},
		{[]string{"PATHS=alerts", "EXPECT=checks/expectations.yaml"}, []string{"--promtool", promtool}, "--promtool: the recipe gives lapilli promtool itself"},
		{[]string{"PATHS=alerts", "EXPECT=checks/expectations.yaml", "PROMTOOL=" + promtool, "LAPILLI_SERVE=https://lapilli-ci.example.com",
			"LAPILLI_SERVE_TOKEN_FILE=/dev/null"}, nil, "PROMTOOL: lapilli serve runs no scenario a pull request sends"},
	} {
		out, err := rr.run(c.settings, append([]string{"--prometheus", prom.URL}, c.args...)...)
		if c.settings[len(c.settings)-1] == "LAPILLI_SERVE_TOKEN_FILE=/dev/null" {
			out, err = rr.run(c.settings, c.args...)
		}
		if err == nil || !strings.Contains(out, c.said) {
			t.Errorf("%v %v: %v\n%s", c.settings, c.args, err, out)
		}
	}
}

// serve takes --promtool beside the expectations it runs the scenarios
// of, --expect's or --config's, resolved at start; alone, or one that
// isn't there, it is refused.
func TestServePromtool(t *testing.T) {
	rulesFile := write(t, "rules.yaml", shopRules("5m"))
	expect := write(t, "expect.yaml", "expectations:\n  - rule: CheckoutErrors\n    scenarios: [t.yaml]\n")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--promtool", scenariotest.Install(t)}, "--promtool runs the scenarios of the expectations, --expect's or --config's: give one"},
		{[]string{"--expect", expect, "--promtool", filepath.Join(t.TempDir(), "none")}, "--promtool "},
	} {
		var out, errs strings.Builder
		if code := run(append([]string{"serve", "--rules", rulesFile, "--prometheus", "http://p"}, c.args...), &out, &errs); code != 2 || !strings.Contains(errs.String(), c.want) {
			t.Errorf("%v: exit %d\n%s", c.args, code, errs.String())
		}
	}
}

// --promtool-memory is the memory one run of --promtool may take: 256Mi
// unless given, a whole number of Mi or Gi, 64Mi at least, and only with
// --promtool.
func TestPromtoolMemoryFlag(t *testing.T) {
	rules := write(t, "rules.yaml", shopRules("5m"))
	expect := write(t, "expectations.yaml", "expectations:\n  - rule: CheckoutErrors\n    scenarios: [t.yaml]\n")
	promtool := scenariotest.Install(t)
	base := []string{"--prometheus", "http://127.0.0.1:1", "--rules", rules, "--expect", expect, "--promtool", promtool}
	st, err := readServe(base, true)
	if err != nil || st.cfg.PromtoolMemory != 256<<20 {
		t.Fatalf("by default: %d %v", st.cfg.PromtoolMemory, err)
	}
	for v, want := range map[string]uint64{"64Mi": 64 << 20, "1Gi": 1 << 30, "512Mi": 512 << 20} {
		if st, err := readServe(append(append([]string{}, base...), "--promtool-memory", v), true); err != nil || st.cfg.PromtoolMemory != want {
			t.Errorf("%s: %d %v", v, st.cfg.PromtoolMemory, err)
		}
	}
	for _, v := range []string{"32Mi", "256M", "256", "1.5Gi", "-1Mi", "+1Gi", "Mi", "99999999999Gi"} {
		if _, err := readServe(append(append([]string{}, base...), "--promtool-memory", v), true); err == nil || !strings.Contains(err.Error(), "--promtool-memory") {
			t.Errorf("%s: %v", v, err)
		}
	}
	if _, err := readServe([]string{"--prometheus", "http://127.0.0.1:1", "--rules", rules, "--promtool-memory", "1Gi"}, true); err == nil ||
		!strings.Contains(err.Error(), "give --promtool") {
		t.Errorf("without --promtool: %v", err)
	}
}
