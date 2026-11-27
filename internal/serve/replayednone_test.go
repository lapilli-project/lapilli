package serve

import (
	"context"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// nothingAnswers is the test history with nothing answering: every range read
// is refused, as a Prometheus that is down refuses it.
type nothingAnswers struct{ *history }

func (nothingAnswers) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]prom.Series, error) {
	return nil, &url.Error{Op: "Post", URL: "http://127.0.0.1:9/api/v1/query_range",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}
}

// TestReplayedNoneFailed: a run whose every rule failed is recorded as a
// failed run, so that its hours aren't taken for replayed: ReplayedFrom
// stays zero, and the overview says the run replayed none of the rules,
// once, and no week before it as zero. One rule replayed is no failed run.
func TestReplayedNoneFailed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Backfill = 3 * time.Hour; c.Impact = "" })
	s.h = nothingAnswers{&history{}}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	runs, err := s.l.Runs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	replaying := 0
	for _, r := range runs {
		if r.Kind != "hourly" && r.Kind != "backfill" {
			continue
		}
		replaying++
		if len(r.Failed) == 0 || !strings.Contains(r.Error, "replayed none of the") && !strings.Contains(r.Error, "couldn't replay its one rule") {
			t.Errorf("run %s, %d failed: error %q, want it to say it replayed none", r.Kind, len(r.Failed), r.Error)
		}
	}
	if replaying == 0 {
		t.Fatal("no run replayed")
	}
	if from, err := s.l.ReplayedFrom(ctx); err != nil || !from.IsZero() {
		t.Errorf("ReplayedFrom %s, %v: a run that replayed nothing replayed from nowhere", from, err)
	}
	_, body, _ := get(t, s.Handler(), "/")
	if n := strings.Count(body, "replayed none of the") + strings.Count(body, "replay its one rule"); n != 1 {
		t.Errorf("the overview says it replayed none %d times, want once", n)
	}
	if strings.Contains(body, "failed: replayed none") || strings.Contains(body, "failed: couldn") {
		t.Error("the overview gives the run's recorded error, not its own sentence")
	}

	if got := replayedNone(map[string]time.Duration{"A": 0, "B": 0}, map[string]string{"A": "query failed: x"}); got != "" {
		t.Errorf("one of two failed: %q, want no error", got)
	}
	if got := replayedNone(map[string]time.Duration{"A": 0}, map[string]string{"A": "query failed: x"}); got != "couldn't replay its one rule: query failed: x" {
		t.Errorf("its one rule: %q", got)
	}
	if got := replayedNone(nil, nil); got != "" {
		t.Errorf("no rules: %q", got)
	}
}
