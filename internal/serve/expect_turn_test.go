package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/scenario"
	"lapilli.dev/internal/scenario/scenariotest"
	"lapilli.dev/internal/verdict"
)

// Guard: serve runs promtool one at a time, a round's scenario and an
// agent's check alike, each taking --promtool-memory beside serve's own:
// an agent's check while a round's run holds the turn waits within its own
// deadline, and says so if its turn doesn't come; one that waits runs once
// the round's ends.
func TestPromtoolOneAtATime(t *testing.T) {
	ctx := context.Background()
	expect := "expectations:\n  - rule: Errors\n    scenarios: [tests/errors.yaml]\n"
	s, _, _, count := scenarioServer(t, expect, map[string]string{"tests/errors.yaml": scenariotest.Test("wait")}, true)
	s.expect.Lock()
	s.expect.key = ""
	s.expect.Unlock()
	done := make(chan error, 1)
	go func() { done <- s.expectAfter(ctx) }()
	for count() < 1 {
		select {
		case err := <-done:
			t.Fatalf("the round's run ended (%v) before it was seen", err)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	test, err := scenario.Rewrite([]byte(scenariotest.Test("pass")))
	if err != nil {
		t.Fatal(err)
	}
	agent := func(ctx context.Context) *draftRun {
		return &draftRun{ctx: ctx, s: s, tests: map[string][]byte{"t.yaml": test}, groups: [2][]byte{[]byte("groups: []\n"), []byte("groups: []\n")}}
	}
	// Its deadline is what it waits within: the round's run holds the
	// turn until it is released.
	wctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	r := agent(wctx).RunScenario("after", false, "Errors", "t.yaml")
	cancel()
	if r.State != verdict.ExpectUnknown || r.Why != whyPromtoolBusy || count() != 1 {
		t.Errorf("beside the round's run: %+v, %d runs", r, count())
	}
	got := make(chan verdict.ScenarioResult, 1)
	go func() { got <- agent(ctx).RunScenario("after", false, "Errors", "t.yaml") }()
	os.WriteFile(filepath.Join(filepath.Dir(s.cfg.Promtool), "release"), nil, 0o600)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r := <-got; r.State != verdict.ExpectMet || count() != 2 {
		t.Errorf("once the round's ended: %+v, %d runs", r, count())
	}
	if c := checksOf(t, s, "Errors")["scenario tests/errors.yaml"]; c.Result != ledger.ExpectMet {
		t.Errorf("the round's: %+v", c)
	}
}
