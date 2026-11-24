package serve

import (
	"context"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
)

// samplesHeld is the test history, its ranges held to one sample
// (--max-samples 1), as prom.Client says its own.
type samplesHeld struct{ *history }

func (samplesHeld) MaxSamples() int { return 1 }

// TestTooWideKept: a rule whose labels on its series are too many to
// replay is kept as it was, and its run says it failed, as one whose query
// failed is: it was taken for a rule that can't be replayed, its firings
// dropped and the window recorded as replayed empty, never to be tried
// again.
func TestTooWideKept(t *testing.T) {
	parallel(t)
	s, _ := server(t, nil)
	s.h = samplesHeld{&history{}}
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	f, _, err := s.find(ctx, now.Add(-72*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	wide := 0
	for _, r := range f.Rules {
		if !strings.HasPrefix(r.Unsupported, rules.TooWide) {
			continue
		}
		wide++
		if !f.Keep[r.Name] || !strings.HasPrefix(f.Run.Failed[r.Name], rules.TooWide) {
			t.Errorf("%s: kept %t, failed %q", r.Name, f.Keep[r.Name], f.Run.Failed[r.Name])
		}
	}
	if wide == 0 {
		t.Fatal("no rule too wide to replay")
	}
}
