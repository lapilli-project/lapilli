package serve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/scenario"
	"lapilli.dev/internal/scenario/scenariotest"
)

// serve's checks after deploy, the guards a review found missing: a
// scenario's run kept as begun before promtool starts, a rule author's
// annotations bounded after the operator's expectations, and the weeks a
// rule only copied in has no copies of.

// A scenario's run is kept as begun, in the ledger, before promtool
// starts, and as found once it ends: had lapilli serve been killed as
// promtool ran (its container out of memory), the next leader would find
// it so, not finished, rather than nothing.
func TestScenarioRunKeptAsBegun(t *testing.T) {
	ctx := context.Background()
	expect := "expectations:\n  - rule: Errors\n    scenarios: [tests/errors.yaml]\n"
	s, _, _, count := scenarioServer(t, expect, map[string]string{"tests/errors.yaml": scenariotest.Test("wait")}, true)
	s.expect.Lock()
	s.expect.key = ""
	s.expect.Unlock()
	done := make(chan error, 1)
	go func() { done <- s.expectAfter(ctx) }()
	release := filepath.Join(filepath.Dir(s.cfg.Promtool), "release")
	for begun := false; !begun; {
		select {
		case err := <-done:
			t.Fatalf("the run ended (%v, %d runs) and was never kept as begun: %+v", err, count(), checksOf(t, s, "Errors"))
		default:
		}
		cs, err := s.l.ExpectChecks(ctx, ledger.ExpectQuery{Rule: "Errors", HasRule: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			if c.Scenario() && c.Detail == scenarioBegun && c.Why == whyRunFailed && c.Result == ledger.ExpectUnknown && c.Version != "" && c.Current {
				begun = true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	os.WriteFile(release, nil, 0o600)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c := checksOf(t, s, "Errors")["scenario tests/errors.yaml"]; c.Result != ledger.ExpectMet || c.Detail != "" || count() != 1 {
		t.Errorf("once it ended: %+v, %d runs", c, count())
	}
}

// A run a leader began and never ended (lapilli serve killed as promtool
// ran) is couldn't be run for the next leader, which doesn't run it again,
// so that a test the container can't hold doesn't kill it each term; once
// the rule's group, the test file or promtool changes, it is run again. Its
// row kept as begun, of a later week than the one it was to replace, is
// the one read.
func TestScenarioDiedNotRunAgain(t *testing.T) {
	ctx := context.Background()
	expect := "expectations:\n  - rule: Errors\n    scenarios: [tests/errors.yaml]\n"
	s, rulesDir, dir, count := scenarioServer(t, expect, map[string]string{"tests/errors.yaml": scenariotest.Test("pass")}, true)
	checkAgain(t, s)
	was := checksOf(t, s, "Errors")["scenario tests/errors.yaml"]
	if was.Result != ledger.ExpectMet || count() != 1 {
		t.Fatalf("first: %+v, %d runs", was, count())
	}
	// The rule changed, and a week on, a leader began its run again, and
	// was killed: what it kept as begun.
	os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte(strings.Replace(ruleFile, "for: 2m", "for: 15m", 1)), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now.Add(7 * 24 * time.Hour) }
	this := ledger.Week(s.l.Day(s.now()))
	e, sources, _, _ := s.expectations()
	groups, err := s.scenarioGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tests, _ := s.scenarioTests(e, sources)
	begun := was
	begun.Week, begun.Version = this, scenarioKey(s.cfg.Promtool, groups["Errors"], tests[expectFromFile+"\ntests/errors.yaml"].test)
	begun.Result, begun.Value, begun.Why, begun.Detail = ledger.ExpectUnknown, "–", whyRunFailed, scenarioBegun
	if begun.Version == was.Version || begun.Week == was.Week {
		t.Fatalf("not another version and week: %+v", begun)
	}
	if err := s.l.KeepScenarioRun(ctx, begun, this, now); err != nil {
		t.Fatal(err)
	}
	// The next leader.
	expectReadTwice(t, s)
	checkAgain(t, s)
	c := checksOf(t, s, "Errors")["scenario tests/errors.yaml"]
	if count() != 1 || c.Result != ledger.ExpectUnknown || c.Why != whyRunFailed || c.Detail != scenarioBegun || c.Version != begun.Version {
		t.Fatalf("the next leader: %+v, %d runs", c, count())
	}
	// The test file changed: run again.
	os.WriteFile(filepath.Join(dir, "tests", "errors.yaml"), []byte(strings.Replace(scenariotest.Test("pass"), "evaluation_interval: 1m", "evaluation_interval: 2m", 1)), 0o600)
	checkAgain(t, s)
	if c := checksOf(t, s, "Errors")["scenario tests/errors.yaml"]; c.Result != ledger.ExpectMet || count() != 2 {
		t.Errorf("its test file changed: %+v, %d runs", c, count())
	}
}

// A rule author's annotation can't stop the operator's checks: the
// operator's own expectations are held first, maxExpectHeld at most, past
// which the check fails, saying so; the annotations after, a rule's whole
// while they fit in what is left, the rest each not known, saying why.
func TestExpectAnnotationsBounded(t *testing.T) {
	ctx := context.Background()
	was := maxExpectHeld
	maxExpectHeld = 20
	defer func() { maxExpectHeld = was }()
	s, rulesDir := server(t, keepRecord)
	var b strings.Builder
	b.WriteString("groups:\n  - name: shop\n    rules:\n      - alert: Operators\n        expr: errors > 5\n")
	for i := range 8 {
		fmt.Fprintf(&b, "      - alert: Author%d\n        expr: errors > %d\n        annotations: {lapilli.dev/budget: \"9/week, 9 asleep, 9 refires\"}\n", i, i)
	}
	os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte(b.String()), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	file := filepath.Join(t.TempDir(), "expect.yaml")
	os.WriteFile(file, []byte("expectations:\n  - rule: Operators\n    budget: {week: 1}\n"), 0o600)
	s.cfg.Expect = file
	expectReadTwice(t, s)
	checkAgain(t, s)
	if cs := checksOf(t, s, "Operators"); len(cs) == 0 {
		t.Fatal("the operator's check of Operators stopped")
	}
	// 19 left: six rules' three, then the rest passed over.
	for i := range 8 {
		cs := checksOf(t, s, "Author"+fmt.Sprint(i))
		past := cs["budget "+s.mondayOf(ledger.Week(s.l.Day(now)))]
		switch {
		case i < 6 && (past.Why != "" || len(cs) < 3):
			t.Errorf("Author%d, within the bound: %+v", i, cs)
		case i >= 6 && (len(cs) != 1 || past.Result != ledger.ExpectUnknown || past.Why != whyAnnotationPast || past.Source != expectFromAnnotation):
			t.Errorf("Author%d, past it: %+v", i, cs)
		}
	}
	// The operator's own past the bound: the check fails, saying so.
	var many strings.Builder
	many.WriteString("expectations:\n")
	for i := range 8 {
		fmt.Fprintf(&many, "  - rule: Author%d\n    budget: {week: 1, asleep: 1, refires: 1}\n", i)
	}
	os.WriteFile(file, []byte(many.String()), 0o600)
	expectReadTwice(t, s)
	s.expect.Lock()
	s.expect.key = ""
	s.expect.Unlock()
	if err := s.expectAfter(ctx); err == nil || !strings.Contains(err.Error(), "by the expectations file and serve's configuration") {
		t.Errorf("the operator's own past the bound: %v", err)
	}
}

// A rule only copied in is known as far as its source's copies cover: a
// week they cover whole is met; a week past where they end isn't (no
// firing copied isn't none fired), whatever the runs' cursor says; nor is
// a catch of an incident they don't reach, not caught in them.
func TestExpectCopiedCoverage(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.RulesPath, c.Impact = "", "" })
	from := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC) // two Mondays, copied
	fs := []firing.Firing{{Rule: "Copied", Labels: map[string]string{"service": "x"}, Start: from.Add(26 * time.Hour), End: from.Add(27 * time.Hour)}}
	if _, err := s.l.PutFirings(ctx, "pager", from, from.Add(14*24*time.Hour), fs, ledger.FiringsCopier, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "expect.yaml")
	os.WriteFile(file, []byte("expectations:\n  - rule: Copied\n    budget: {week: 5}\n    catches: [{incident: INC-1, within: 5m}]\n"), 0o600)
	s.cfg.Expect = file
	s.jobs(ctx, true) // the incidents file read
	expectReadTwice(t, s)
	checkAgain(t, s)
	cs := checksOf(t, s, "Copied")
	if c := cs["catches INC-1"]; c.Result != ledger.ExpectUnknown || c.Why != whyNotCopied {
		t.Errorf("a catch of an incident past the copies: %+v", c)
	}
	delete(cs, "catches INC-1")
	var keys []string
	for k := range cs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if c := cs["budget week 2026-09-07"]; c.Result != ledger.ExpectMet {
		t.Errorf("a week the copies cover whole: %+v", c)
	}
	for _, k := range keys {
		if c := cs[k]; k > "budget week 2026-09-07" && (c.Result == ledger.ExpectMet || c.Result == ledger.ExpectOpen || c.Why != whyNotCopied) {
			t.Errorf("%s, past the copies: %+v", k, c)
		}
	}
	if len(keys) == 0 || !strings.HasPrefix(keys[len(keys)-1], "budget week 2026-11-02") {
		t.Errorf("weeks: %v", keys)
	}
}

// --promtool-memory caps each run serve makes, a round's and a check's an
// agent asked for: promtool's RLIMIT_DATA, where one can be set (Linux).
func TestScenarioRunsCapped(t *testing.T) {
	if !scenario.CanCap {
		t.Skip("no memory cap on " + runtime.GOOS)
	}
	expect := "expectations:\n  - rule: Errors\n    scenarios: [tests/errors.yaml]\n"
	s, _, _, _ := scenarioServer(t, expect, map[string]string{"tests/errors.yaml": scenariotest.Test("see")}, true)
	s.cfg.PromtoolMemory = 96 << 20
	checkAgain(t, s)
	if c := checksOf(t, s, "Errors")["scenario tests/errors.yaml"]; !strings.Contains(c.Detail, "rlimit_data "+strconv.Itoa(96<<20)+"\n") {
		t.Errorf("a round's run: %+v", c)
	}
	test, err := scenario.Rewrite([]byte(scenariotest.Test("see")))
	if err != nil {
		t.Fatal(err)
	}
	d := &draftRun{ctx: context.Background(), s: s, tests: map[string][]byte{"t.yaml": test}, groups: [2][]byte{[]byte("groups: []\n"), []byte("groups: []\n")}}
	if r := d.RunScenario("after", false, "Errors", "t.yaml"); !strings.Contains(r.Said, "rlimit_data "+strconv.Itoa(96<<20)+"\n") {
		t.Errorf("a check's run: %+v", r)
	}
}
