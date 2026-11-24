package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// samplesHeld is a history whose answers to a range are held to 1,000
// samples (--max-samples 1000), as prom.Client says its own.
type samplesHeld struct{ fakeHistory }

func (samplesHeld) MaxSamples() int { return 1000 }

// TestTooWideFailsUnreplayed: a rule whose labels on each of its series
// are more than --max-samples lets a replay hold isn't replayed, and says
// so in its own words, not a query failed: --fail-unreplayed counts it, as
// a rule's writer could make a rule so to keep it out of a backtest
// unseen, where it was taken for a query refused as written and the run
// passed.
func TestTooWideFailsUnreplayed(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return samplesHeld{}, nil }
	now = func() time.Time { return t0 }
	var labels strings.Builder
	for i := range 700 {
		fmt.Fprintf(&labels, "          l%03d: v\n", i)
	}
	rs := write(t, "rules.yaml", "groups:\n  - name: shop\n    rules:\n      - alert: CheckoutErrors\n        expr: errors > 5\n        labels:\n"+labels.String())
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-7, title: Checkout down, start: \"2026-11-01T21:00:00Z\", end: \"2026-11-01T22:00:00Z\"}\n")
	verdictOf := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"verdict", "--prometheus", "http://p", "--incidents", incidents, "--tz", "UTC", "--rules", rs}, args...), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	code, out, _ := verdictOf("--json", "--fail-nothing-replayed=false")
	var rep verdict.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 {
		t.Fatalf("exit %d: %v", code, err)
	}
	if len(rep.Unreplayed) != 1 || !strings.HasPrefix(rep.Unreplayed[0].Reason, rules.TooWide) || strings.HasPrefix(rep.Unreplayed[0].Reason, rules.QueryFailed) {
		t.Errorf("unreplayed %+v", rep.Unreplayed)
	}
	if code, _, errs := verdictOf("--fail-nothing-replayed=false", "--fail-unreplayed"); code != 1 || !strings.Contains(errs, "1 of 1 rules couldn't be replayed") {
		t.Errorf("--fail-unreplayed: exit %d: %s", code, errs)
	}
}
