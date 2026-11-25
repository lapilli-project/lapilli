package serve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/routing"
)

// silencesServer is a server with --silences a file of n silences of other
// alerts, and one firing of Errors with recorded mutes told of in its hours:
// m in them, and one from long before.
func silencesServer(t *testing.T, n, m int) (*Server, time.Time, time.Time) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("[")
	for i := range n {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"id":"file-%d","matchers":[{"name":"alertname","value":"Other%d","isRegex":false}],"startsAt":"2026-11-01T00:00:00Z","endsAt":"2026-11-30T00:00:00Z"}`, i, i)
	}
	sb.WriteString("]")
	file := filepath.Join(t.TempDir(), "silences.json")
	if err := os.WriteFile(file, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	am := routedFile(t, routedAM)
	s, _ := server(t, func(c *Config) { c.Notifications, c.AlertmanagerConfig, c.Silences = true, am, file })
	ctx := context.Background()
	from, to := now.Add(-4*time.Hour).Truncate(time.Hour), now.Truncate(time.Hour)
	start := from.Add(time.Hour)
	if err := s.l.Record(ctx, ledger.Found{From: from, To: to, Run: ledger.Run{Kind: "test", Started: from, Finished: from, From: from, To: to},
		Firings: []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "a", "severity": "page"}, Start: start, End: start.Add(time.Hour)}}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	var ms []ledger.Mute
	for i := range m {
		ms = append(ms, ledger.Mute{At: start.Add(time.Minute), Last: start.Add(10 * time.Minute), Rule: "Errors", Kind: ledger.MutedSilence, Ref: fmt.Sprint("rec-", i),
			Labels: map[string]string{"alertname": "Errors", "service": "a", "severity": "page"}})
	}
	// One from twenty days before: it doesn't touch the hours.
	ms = append(ms, ledger.Mute{At: from.Add(-20 * 24 * time.Hour), Last: from.Add(-20*24*time.Hour + time.Minute), Rule: "Errors", Kind: ledger.MutedSilence, Ref: "old",
		Labels: map[string]string{"alertname": "Errors", "service": "a", "severity": "page"}})
	if _, errs, err := s.l.KeepMutes(ctx, ms); err != nil {
		t.Fatal(errs, err)
	}
	return s, from, to
}

// The silences of the file and the recorded mutes that touch the hours add
// up to what the model takes; more than that, and the hours are not known,
// with the reason, never worked out as if there were none. A mute of long
// before the hours is not one of them.
func TestRoutedSilencesOverflow(t *testing.T) {
	parallel(t)
	s, from, to := silencesServer(t, 4, 2)
	ctx := context.Background()
	s.routed.limits = routedLimits{silences: 5}
	s.workOutRouted(ctx, from, to)
	c, _ := s.l.RoutedCover(ctx)
	if c.Covers(from, to) || !strings.Contains(c.Unknown(from, to), "more than 5 silences") || !strings.Contains(c.Unknown(from, to), "4 of --silences and the 2 the recorder") {
		t.Fatalf("6 silences against 5: %+v", c)
	}
	// With room for them (the old mute makes no seventh), the hours are
	// worked out.
	s.routed.limits = routedLimits{silences: 6}
	s.workOutRouted(ctx, from, to)
	if c, _ := s.l.RoutedCover(ctx); !c.Covers(from, to) {
		t.Errorf("6 silences against 6: %+v", c)
	}
}

// The model takes the silences of a file as large as the reader does.
func TestRoutedSilencesTakenAsReadable(t *testing.T) {
	if routing.MaxSilences != 100000 {
		t.Errorf("MaxSilences %d: a --silences file of 100,000 is read, and worked out with", routing.MaxSilences)
	}
	var sils []routing.Silence
	for i := range routing.MaxSilences {
		sils = append(sils, routing.Silence{ID: fmt.Sprint(i), Matchers: nil, Start: now, End: now})
	}
	cfg, err := routing.NewConfig(&routing.Route{Receiver: "r"}, []routing.Receiver{{Name: "r"}}, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Run(nil, routing.Options{Silences: sils}); err != nil {
		t.Errorf("a run of the most silences read: %v", err)
	}
	if _, err := cfg.Run(nil, routing.Options{Silences: append(sils, sils[0])}); err == nil {
		t.Error("a run of more silences than are read")
	}
}

// The catch-up has a time of its own to work in across its pieces: one that
// is over leaves the gaps for the next run, and a piece that stopped isn't
// tried at every round, but after an hour, two, four, to a day.
func TestRoutedCatchUpBounds(t *testing.T) {
	parallel(t)
	s, _ := workedServer(t, nil)
	ctx := context.Background()
	from, to, _, _ := routedSpan(s.l, ctx)
	d1 := from.Truncate(24 * time.Hour).Add(24 * time.Hour)
	for _, at := range []time.Time{d1, d1.Add(24 * time.Hour)} {
		if err := s.l.MarkRoutedUnknown(ctx, at, at.Add(time.Hour), "a test's failure"); err != nil {
			t.Fatal(err)
		}
	}
	gaps := func() int { c, _ := s.l.RoutedCover(ctx); return len(c.Gaps(d1, to)) }
	if gaps() != 2 {
		t.Fatalf("%d gaps", gaps())
	}
	// A deadline that is over: nothing is tried.
	s.routed.limits = routedLimits{catchUp: -time.Second}
	s.catchUpRouted(ctx, to)
	if gaps() != 2 || len(s.routed.tried) != 0 {
		t.Errorf("a catch-up with no time worked something out: %d gaps, %v tried", gaps(), s.routed.tried)
	}
	// Pieces that stop, as a model given no work does.
	s.routed.limits = routedLimits{budget: 1}
	tried := func() (n int) {
		for _, x := range s.routed.tried {
			n = max(n, x.failed)
		}
		return
	}
	clock := now
	s.now = func() time.Time { return clock }
	for i, step := range []struct {
		after  time.Duration
		failed int // the most any piece failed in a row, after the round
	}{{0, 1}, {30 * time.Minute, 1}, {31 * time.Minute, 2}, {90 * time.Minute, 2}, {31 * time.Minute, 3}, {3 * time.Hour, 3}, {2 * time.Hour, 4}} {
		clock = clock.Add(step.after)
		s.catchUpRouted(ctx, to)
		if tried() != step.failed {
			t.Fatalf("round %d (+%v): failed %d times in a row, want %d: %+v", i, step.after, tried(), step.failed, s.routed.tried)
		}
	}
	// And the wait doubles to a day, no more.
	if w := (routedTry{failed: 20}).wait(); w != routedBackoffMax {
		t.Errorf("the wait after 20 failures is %v", w)
	}
	if w := (routedTry{failed: 1}).wait(); w != time.Hour {
		t.Errorf("the wait after one is %v", w)
	}
	if w := (routedTry{failed: 4}).wait(); w != 8*time.Hour {
		t.Errorf("the wait after four is %v", w)
	}
	// With the bounds lifted, the next try works them out and forgets them.
	s.routed.limits = routedLimits{}
	clock = clock.Add(2 * routedBackoffMax)
	s.catchUpRouted(ctx, to)
	if gaps() != 0 || len(s.routed.tried) != 0 {
		t.Errorf("after: %d gaps, %v tried", gaps(), s.routed.tried)
	}
}

// A page worked out just before an hour and the one recorded just after it
// are one: a recorded notification of the receiver in the hour of the minute
// before or after the worked-out page's time is its copy.
func TestRecordedNearAcrossHours(t *testing.T) {
	h := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	recorded := map[int64]bool{h.Add(time.Hour).Unix(): true}
	for _, c := range []struct {
		at   time.Time
		want bool
	}{
		{h.Add(time.Hour + 5*time.Second), true},     // in the recorded hour
		{h.Add(time.Hour - 20*time.Second), true},    // twenty seconds before it
		{h.Add(time.Hour - 59*time.Second), true},    // the window's edge
		{h.Add(time.Hour - 2*time.Minute), false},    // well before: its own
		{h.Add(2*time.Hour + 30*time.Minute), false}, // a later hour
		{h.Add(2*time.Hour - 30*time.Second), true},  // the recorded hour's end
		{h.Add(2*time.Hour + 20*time.Second), true},  // and just after it
		{h.Add(2*time.Hour + 2*time.Minute), false},  // but not far
	} {
		if got := recordedNear(recorded, c.at); got != c.want {
			t.Errorf("%v: %v, want %v", c.at, got, c.want)
		}
	}
}
