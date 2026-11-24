package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/scenario/scenariotest"
)

func TestMain(m *testing.M) {
	// Run as promtool, through scenariotest.Install's link: a stand-in.
	scenariotest.Fake()
	done, err := freshLedgers()
	if err != nil {
		fmt.Fprintln(os.Stderr, "the fresh ledger:", err)
		os.Exit(1)
	}
	code := m.Run()
	done()
	os.Exit(code)
}

// scenarioServer is serve with its rules read and run once, held to an
// expectations file in a directory of its own, with the test files given,
// and promtool's stand-in when promtool is true; with the stand-in's runs
// counted.
func scenarioServer(t *testing.T, expect string, tests map[string]string, promtool bool) (*Server, string, string, func() int) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "expectations.yaml")
	os.WriteFile(file, []byte(expect), 0o600)
	for p, body := range tests {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte(body), 0o600)
	}
	link := scenariotest.Install(t)
	count := scenariotest.Counted(t, link)
	hook := filepath.Join(t.TempDir(), "hook")
	os.WriteFile(hook, []byte("https://hooks.example/x\n"), 0o600)
	s, rulesDir := server(t, func(c *Config) {
		c.Expect, c.NotifyWebhook = file, hook
		if promtool {
			c.Promtool = link
		}
	})
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	expectReadTwice(t, s)
	return s, rulesDir, dir, count
}

// A scenario is run after a round against the rule's group as the latest
// version has it, its test file read in the expectations file's
// directory: met, broken (with what promtool said), or not known and why.
// It is run once for what it is run on, and again when the rule's group
// or its test file changes; a few a round, the rest the rounds after. A
// broken one is told, and shown on the rule's tab, as the others are.
func TestExpectationScenarios(t *testing.T) {
	ctx := context.Background()
	expect := "expectations:\n  - rule: Errors\n    scenarios: [tests/errors.yaml, tests/none.yaml]\n"
	s, rulesDir, dir, count := scenarioServer(t, expect, map[string]string{"tests/errors.yaml": scenariotest.Test("fail-if for: 15m")}, true)
	checkAgain(t, s)
	errs := checksOf(t, s, "Errors")
	c := errs["scenario tests/errors.yaml"]
	if c.Result != ledger.ExpectMet || c.Value != "passes" || c.Version == "" || c.Held != "passes" || c.Week != ledger.Week(s.l.Day(now)) || count() != 1 {
		t.Fatalf("run: %+v, %d runs", c, count())
	}
	if c := errs["scenario tests/none.yaml"]; c.Result != ledger.ExpectUnknown || c.Why != whyTestUnread || c.Detail != "no such file" {
		t.Errorf("no test file: %+v", c)
	}
	// Nothing changed: not run again.
	checkAgain(t, s)
	if count() != 1 {
		t.Errorf("run again for nothing: %d", count())
	}

	// The rule's for raised: its group isn't what it was, run again,
	// failing now; told, once.
	os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte(strings.Replace(ruleFile, "for: 2m", "for: 15m", 1)), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	checkAgain(t, s)
	c = checksOf(t, s, "Errors")["scenario tests/errors.yaml"]
	if c.Result != ledger.ExpectBroken || c.Value != "fails" || !strings.Contains(c.Detail, "got:[]") || count() != 2 {
		t.Fatalf("run again: %+v, %d runs", c, count())
	}
	ns := expectNotices(t, s)
	if len(ns) != 1 || ns[0].Scenario != "tests/errors.yaml" || ns[0].Rule != "Errors" || ns[0].Value != "fails" {
		t.Fatalf("told: %+v", ns)
	}
	if got := s.line(notice{Event: "expectation.broken", Expectation: &ns[0]}); got != "lapilli: Errors fails the scenario tests/errors.yaml, which it is expected to pass" {
		t.Errorf("Slack: %q", got)
	}
	checkAgain(t, s)
	if ns := expectNotices(t, s); len(ns) != 1 {
		t.Errorf("told again: %+v", ns)
	}

	// The rule's tab, the overview and the API say it, in each language.
	h := s.Handler()
	_, body, _ := get(t, h, "/rules/Errors?tab=expectations")
	for _, want := range []string{"passes the scenario tests/errors.yaml", "broken, fails", "got:[]", "its test file can&#39;t be read", `action="/expectations/task"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the tab: no %q", want)
		}
	}
	_, body, _ = get(t, h, "/")
	if !strings.Contains(body, "Errors fails the scenario tests/errors.yaml, which it is expected to pass") {
		t.Error("the overview doesn't say it")
	}
	_, body, _ = get(t, h, "/api/v1/expectations?rule=Errors")
	if !strings.Contains(body, `"scenario tests/errors.yaml"`) || !strings.Contains(body, `"detail": "    alertname: CheckoutErrors`) {
		t.Errorf("the API: %s", body)
	}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	for _, lang := range []string{"ko", "ja", "es"} {
		r := httptest.NewRequest(http.MethodGet, "/rules/Errors?tab=expectations", nil)
		r.Header.Set("Accept-Language", lang)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if lang == "ko" && !strings.Contains(w.Body.String(), "시나리오") {
			t.Errorf("ko: no 시나리오")
		}
		_ = s.expectSentence(lang, c, 0)
		_ = s.expectTitle(lang, c, 0)
	}
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()

	// The test file edited, to one that passes: run again.
	os.WriteFile(filepath.Join(dir, "tests", "errors.yaml"), []byte(scenariotest.Test("pass")), 0o600)
	checkAgain(t, s)
	if c := checksOf(t, s, "Errors")["scenario tests/errors.yaml"]; c.Result != ledger.ExpectMet || count() != 3 {
		t.Errorf("the test file edited: %+v, %d runs", c, count())
	}

	// expectScenariosARun a round: the rest the next.
	was := expectScenariosARun
	expectScenariosARun = 1
	defer func() { expectScenariosARun = was }()
	os.WriteFile(filepath.Join(dir, "tests", "a.yaml"), []byte(scenariotest.Test("pass")), 0o600)
	os.WriteFile(filepath.Join(dir, "tests", "b.yaml"), []byte(scenariotest.Test("fail")), 0o600)
	os.WriteFile(s.cfg.Expect, []byte("expectations:\n  - rule: Errors\n    scenarios: [tests/a.yaml, tests/b.yaml]\n"), 0o600)
	expectReadTwice(t, s)
	checkAgain(t, s)
	errs = checksOf(t, s, "Errors")
	if a, b := errs["scenario tests/a.yaml"], errs["scenario tests/b.yaml"]; a.Result != ledger.ExpectMet || b.Why != whyNotRunYet || count() != 4 {
		t.Fatalf("a round: %+v %+v, %d runs", a, b, count())
	}
	if err := s.expectAfter(ctx); err != nil { // the round after: the key wasn't kept
		t.Fatal(err)
	}
	if b := checksOf(t, s, "Errors")["scenario tests/b.yaml"]; b.Result != ledger.ExpectBroken || count() != 5 {
		t.Errorf("the round after: %+v, %d runs", b, count())
	}
}

// Without --promtool, a scenario is not known, never met, and nothing is
// run.
func TestExpectationScenariosNoPromtool(t *testing.T) {
	parallel(t)
	s, _, _, count := scenarioServer(t, "expectations:\n  - rule: Errors\n    scenarios: [tests/errors.yaml]\n",
		map[string]string{"tests/errors.yaml": scenariotest.Test("pass")}, false)
	checkAgain(t, s)
	if c := checksOf(t, s, "Errors")["scenario tests/errors.yaml"]; c.Result != ledger.ExpectUnknown || c.Why != whyNoPromtool || count() != 0 {
		t.Errorf("%+v, %d runs", c, count())
	}
}

// Guard: a pull request's backtest names scenarios in its expectations,
// which serve is never sent and never runs, whatever --promtool it has,
// and whatever files beside its own --expect the paths name: each is said
// as not run through lapilli serve, counted as nothing unrun.
func TestBacktestScenariosNotRun(t *testing.T) {
	parallel(t)
	link := scenariotest.Install(t)
	count := scenariotest.Counted(t, link)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tests"), 0o755)
	os.WriteFile(filepath.Join(dir, "tests", "shop.yaml"), []byte(scenariotest.Test("fail")), 0o600)
	own := filepath.Join(dir, "expectations.yaml")
	os.WriteFile(own, []byte("expectations:\n  - rule: Errors\n    scenarios: [tests/shop.yaml]\n"), 0o600)
	b := btServer(t, nil, "", func(c *Config) { c.Promtool, c.Expect = link, own })
	md, rep := b.done(t, b.plain, btBody(ruleFile, changedRules, map[string]any{"expect": "expectations:\n  - rule: Errors\n    scenarios: [tests/shop.yaml]\n"}))
	if !strings.Contains(md, "| `Errors` | passes the scenario `tests/shop.yaml` | – | – | not known: not run through lapilli serve |") || count() != 0 {
		t.Errorf("%d runs:\n%s", count(), md)
	}
	if sum := rep["summary"].(map[string]any); sum["scenarios_unrun"] != 0.0 || sum["expectations_broken"] != 0.0 {
		t.Errorf("summary: %v", sum)
	}
}
