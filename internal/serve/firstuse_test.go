package serve

import (
	"strings"
	"testing"
	"time"
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
