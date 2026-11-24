package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/scenario/scenariotest"
)

// A pull request that edits only a scenario's test file, its rule alike on
// both sides, runs that scenario before and after, each side's test file
// read beside its own expectations file (--expect-before's, --expect's):
// one that fails after it is broken by the change, though nothing that
// decides when a rule fires changed, and --fail-expectations fails it. So
// with another rule changed beside it. A test file alike on both sides
// runs nothing, and nothing changed is said as it was.
func TestCompareScenarioTestEdited(t *testing.T) {
	alone(t)
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	rulesFile := write(t, "rules.yaml", shopRules("5m"))
	expect := "expectations:\n  - rule: CheckoutErrors\n    scenarios: [tests/shop.yaml]\n"
	side := func(mode string) string {
		dir := t.TempDir()
		writeIn(t, dir, map[string]string{"expectations.yaml": expect, "tests/shop.yaml": scenariotest.Test(mode)})
		return filepath.Join(dir, "expectations.yaml")
	}
	before, after := side("pass"), side("fail")
	promtool := scenariotest.Install(t)
	count := scenariotest.Counted(t, promtool)
	lapilli := func(pr, base, was, is string, args ...string) (int, string, string) {
		var out, errs strings.Builder
		code := run(slices.Concat([]string{"verdict", "--prometheus", "http://p", "--tz", "UTC", "--rules", pr, "--compare", base,
			"--expect", is, "--expect-before", was, "--promtool", promtool}, standInMemory, args), &out, &errs)
		return code, out.String(), errs.String()
	}
	code, out, errs := lapilli(rulesFile, rulesFile, before, after, "--changed-only", "--markdown")
	if code != 0 || count() != 2 {
		t.Fatalf("exit %d, %d runs\n%s\n%s", code, count(), out, errs)
	}
	for _, want := range []string{`"nothing_changed":true`, `"expected":true`, `"expectations_broken":1`, "### Lapilli backtest: nothing changed",
		"| `CheckoutErrors` | passes the scenario `tests/shop.yaml` (its test file edited in this change) | passes | fails | **✗ broken by this change** |"} {
		if !strings.Contains(out, want) {
			t.Errorf("--markdown: no %q\n%s", want, out)
		}
	}
	if code, out, _ := lapilli(rulesFile, rulesFile, before, after); code != 0 || !strings.Contains(out, "nothing to replay") ||
		!strings.Contains(out, "Expectations: 1 broken by this change") {
		t.Errorf("text: exit %d\n%s", code, out)
	}
	if code, out, _ := lapilli(rulesFile, rulesFile, before, after, "--json"); code != 0 || !strings.Contains(out, `"edited": true`) {
		t.Errorf("--json: exit %d\n%s", code, out)
	}
	if code, _, errs := lapilli(rulesFile, rulesFile, before, after, "--fail-expectations"); code != 1 || !strings.Contains(errs, "breaks 1 expectations") {
		t.Errorf("--fail-expectations: exit %d\n%s", code, errs)
	}

	// Another rule changed beside it: the scenario's rule, alike, is run
	// too.
	pr := write(t, "pr.yaml", strings.Replace(shopRules("5m"), "expr: never", "expr: never_again", 1))
	if code, out, _ := lapilli(pr, rulesFile, before, after, "--changed-only", "--markdown"); code != 0 || !strings.Contains(out, `"expectations_broken":1`) ||
		!strings.Contains(out, "passes the scenario `tests/shop.yaml` (its test file edited in this change) | passes | fails |") {
		t.Errorf("beside a rule changed: exit %d\n%s", code, out)
	}

	// The test file alike on both sides: nothing run, nothing changed as
	// it was.
	runs := count()
	code, out, _ = lapilli(rulesFile, rulesFile, before, side("pass"), "--changed-only", "--markdown")
	if code != 0 || count() != runs || strings.Contains(out, "Expectations") || !strings.Contains(out, `"expected":false`) {
		t.Errorf("alike: exit %d, %d runs\n%s", code, count()-runs, out)
	}
}

// Through the recipe, a pull request that edits only a scenario's test
// file, to one that fails, is judged by it though nothing that decides when
// a rule fires changed: the comment is posted, saying so, and
// FAIL_ON_EXPECTATIONS fails the job. One that edits neither a rule nor a
// test file posts nothing, as before.
func TestBacktestRecipeScenarioTestEdited(t *testing.T) {
	needRecipeTools(t)
	prom := promStandIn(t)
	promtool := scenariotest.Install(t)
	count := scenariotest.Counted(t, promtool)
	expect := "expectations:\n  - rule: CheckoutErrors\n    scenarios: [tests/shop.yaml]\n"
	repo := newPRRepo(t, map[string]string{"alerts/shop.yaml": shopRules("5m"), "checks/expectations.yaml": expect,
		"checks/tests/shop.yaml": scenariotest.Test("pass")})
	repo.pullRequest(map[string]string{"checks/tests/shop.yaml": scenariotest.Test("fail")})
	rr := newRecipeRun(t, backtestHosts[0], repo)
	settings := []string{"PATHS=alerts", "EXPECT=checks/expectations.yaml", "PROMTOOL=" + promtool, "FAIL_ON_EXPECTATIONS=true"}
	args := append([]string{"--prometheus", prom.URL, "--tz", "UTC"}, standInMemory...)
	out, err := rr.run(settings, args...)
	if err == nil || !strings.Contains(rr.body(), "### Lapilli backtest: nothing changed") ||
		!strings.Contains(rr.body(), "(its test file edited in this change) | passes | fails | **✗ broken by this change** |") ||
		!strings.Contains(out, "the change breaks 1 expectations the rules in place meet (FAIL_ON_EXPECTATIONS=true)") || count() != 2 {
		t.Errorf("the test file edited: %v, %d runs\n%s\n%s", err, count(), rr.body(), out)
	}
	// Neither a rule nor a test file: nothing posted, the job passes.
	repo.git("checkout", "--quiet", "main")
	repo.pullRequest(map[string]string{"README.md": "rules\n"})
	rr.host.set(func(h *commentHost) { h.comments = nil })
	if out, err := rr.run(settings, args...); err != nil || len(rr.host.comments) != 0 || !strings.Contains(out, "none posted") {
		t.Errorf("nothing edited: %v\n%s", err, out)
	}
}
