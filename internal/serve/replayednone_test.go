package serve

import (
	"context"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
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
	// The banner says what failed in plain words: the history refused.
	if !strings.Contains(body, "Nothing answered at") {
		t.Error("no banner saying the history refused")
	}
	// The history answers again: the next run replays, and the banner goes.
	s.h = &history{}
	s.now = func() time.Time { return now.Add(2 * time.Hour) }
	s.jobs(ctx, true)
	if _, body, _ = get(t, s.Handler(), "/"); strings.Contains(body, "Nothing answered at") {
		t.Error("the banner stays after a run that replayed")
	}

	asked := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	unread := rules.HistoryUnread + "connection refused"
	if got := replayedNone(asked("A", "B"), map[string]string{"A": unread}); got != "" {
		t.Errorf("one of two failed: %q, want no error", got)
	}
	if got := replayedNone(asked("A"), map[string]string{"A": unread}); got != "couldn't replay its one rule: "+unread {
		t.Errorf("its one rule: %q", got)
	}
	if got := replayedNone(asked("A", "B"), map[string]string{"A": unread, "B": rules.Unfinished}); !strings.HasPrefix(got, "replayed none of the 2 rules") {
		t.Errorf("two failed: %q", got)
	}
	if got := replayedNone(asked("A"), map[string]string{"A": rules.QueryFailed + "HTTP 400: bad_data"}); got != "" {
		t.Errorf("one refused as written: %q, want no error", got)
	}
	if got := replayedNone(asked("A", "B"), map[string]string{"A": unread, "B": rules.TooWide + "x"}); got != "" {
		t.Errorf("one too wide: %q, want no error", got)
	}
	if got := replayedNone(nil, nil); got != "" {
		t.Errorf("no rules: %q", got)
	}
}

// A rule never sent as a query (here a LogQL one with no Loki to ask) can't
// fail, so one doesn't hide a Prometheus that is down: the rules asked all
// failed with the history unread, and the run says it replayed none.
func TestReplayedNoneNotAskedDontCount(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "loki")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "logs.yaml"), []byte("groups:\n  - name: logs\n    rules:\n      - alert: LogErrors\n        expr: sum(count_over_time({app=\"a\"} |= \"error\" [5m])) > 5\n"), 0o644)
	s, _ := server(t, func(c *Config) { c.Loki, c.LokiRules, c.Impact = "http://loki:3100", dir, "" })
	s.h = nothingAnswers{&history{}}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	f, _, err := s.find(ctx, now.Add(-3*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	var logq bool
	for _, r := range f.Rules {
		if r.Name == "LogErrors" {
			logq = r.Unsupported != "" && !rules.Unanswered(r.Unsupported)
		}
	}
	if !logq {
		t.Fatalf("the LogQL rule isn't one that can't be replayed: %+v", f.Rules)
	}
	if f.Run.Error == "" {
		t.Errorf("a rule not asked hid the history not answering: failed %v", f.Run.Failed)
	}
}

// Every rule refused as written is the history answering: the run isn't a
// failed one, a source isn't down after three of them, and the banner
// doesn't say the history failed.
func TestReplayedNoneRefusedAsWritten(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, rulesDir := server(t, func(c *Config) { c.Backfill = 3 * time.Hour; c.Impact = "" })
	os.WriteFile(filepath.Join(rulesDir, "shop.yaml"), []byte("groups:\n  - name: shop\n    rules:\n      - {alert: One, expr: nope(up)}\n      - {alert: Two, expr: nope(up)}\n"), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	f, _, err := s.find(ctx, now.Add(-3*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Run.Failed) != 2 || f.Run.Error != "" {
		t.Fatalf("refused as written: failed %v, error %q", f.Run.Failed, f.Run.Error)
	}
	for i := range 4 {
		s.now = func() time.Time { return now.Add(time.Duration(i) * time.Hour) }
		s.jobs(ctx, true)
	}
	runs, err := s.l.Runs(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if (r.Kind == "hourly" || r.Kind == "backfill") && r.Error != "" {
			t.Errorf("run %s: error %q", r.Kind, r.Error)
		}
	}
	items, err := s.sourceList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ID == "prometheus" && (it.Down || it.State == "failing") {
			t.Errorf("the history is %s, down %v: %q", it.State, it.Down, it.LastError)
		}
	}
	if v := s.statusView("en", true); v.Err != "" || v.ErrLead != "" {
		t.Errorf("the banner says the history failed: %q", v.Err)
	}
}
