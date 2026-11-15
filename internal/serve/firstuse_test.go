package serve

import (
	"context"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// TestShownDur: a time a page shows is rounded as a person reads one, and a
// rule's for, whole minutes or seconds, is as it was.
func TestShownDur(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"}, {300 * time.Microsecond, "1ms"}, {40*time.Millisecond + 400*time.Microsecond, "40ms"},
		{340 * time.Millisecond, "0.3s"}, {3412345678, "3.4s"}, {3 * time.Second, "3s"}, {30 * time.Second, "30s"},
		{59970 * time.Millisecond, "1m"}, {61500 * time.Millisecond, "1m2s"}, {125*time.Second + 400*time.Millisecond, "2m5s"},
		{90 * time.Second, "1m30s"}, {15 * time.Minute, "15m"}, {time.Hour, "1h"}, {90 * time.Minute, "1h30m"},
		{time.Hour + 5*time.Second + 300*time.Millisecond, "1h0m5s"}, {-1500 * time.Millisecond, "-1.5s"},
	} {
		if got := shownDur(c.d); got != c.want {
			t.Errorf("shownDur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
	// The runs page says how long the last run took so.
	s, _ := server(t, nil)
	s.status.Lock()
	s.status.took = 3412345678
	s.status.Unlock()
	if _, body, _ := get(t, s.Handler(), "/runs"); !strings.Contains(body, "The last run took 3.4s.") {
		t.Errorf("the runs page doesn't say the last run took 3.4s:\n%s", body)
	}
}

// TestFiringsLastDay: the firings page asked for no day opens the last day
// with firings, of the rule asked, and says so once; asked for a day, that
// day, said nothing of; with none in the twelve weeks, today, said so.
func TestFiringsLastDay(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()
	fs, err := s.l.Firings(ctx, ledger.FiringQuery{From: s.shownFrom(), To: now.Add(24 * time.Hour), Limit: 1})
	if err != nil || len(fs) == 0 {
		t.Fatalf("no firing: %v", err)
	}
	last := fs[0].Start.UTC().Format("2006-01-02")
	if today := now.Format("2006-01-02"); last == today {
		t.Fatalf("the last firing is today's (%s): the test says nothing", today)
	}
	said := "The last day with firings."
	_, body, _ := get(t, h, "/firings")
	if !strings.Contains(body, "<h1>Firings, "+last+"</h1>") || strings.Count(body, said) != 1 {
		t.Errorf("/firings isn't %s, said once:\n%s", last, body)
	}
	// Its links name their days, as before.
	if !strings.Contains(body, `href="/firings?day=`) {
		t.Error("/firings: the days before and after have no day= in their links")
	}
	_, body, _ = get(t, h, "/firings?day="+now.Format("2006-01-02"))
	if !strings.Contains(body, "<h1>Firings, "+now.Format("2006-01-02")+"</h1>") || strings.Contains(body, said) {
		t.Errorf("/firings?day= of today isn't today, said nothing of:\n%s", body)
	}
	_, body, _ = get(t, h, "/firings?rule=NothingIsCalledThis")
	if !strings.Contains(body, "<h1>Firings, "+now.Format("2006-01-02")+"</h1>") || !strings.Contains(body, "No firing to show in the last twelve weeks.") {
		t.Errorf("/firings of a rule that never fired isn't today, said so:\n%s", body)
	}
}
