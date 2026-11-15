package ledger

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// Each alert that fired in a span, and how many times, by rule, the most
// first; one that fired before the span isn't among them.
func TestAlertCounts(t *testing.T) {
	l := open(t, time.UTC)
	var fs []firing.Firing
	for i, svc := range []string{"a", "b", "a", "a", "b", "c"} {
		at := t0.Add(time.Duration(i) * time.Hour)
		fs = append(fs, firing.Firing{Rule: "R", Labels: map[string]string{"service": svc}, Start: at, End: at.Add(time.Minute)})
	}
	fs = append(fs, firing.Firing{Rule: "Q", Labels: map[string]string{"service": "a"}, Start: t0.Add(30 * time.Minute), End: t0.Add(31 * time.Minute)},
		firing.Firing{Rule: "Q", Labels: map[string]string{"service": "old"}, Start: t0.Add(-time.Hour), End: t0.Add(-59 * time.Minute)})
	record(t, l, t0.Add(-2*time.Hour), t0.Add(24*time.Hour), fs)
	got, err := l.AlertCounts(context.Background(), t0, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, a := range got {
		lines = append(lines, a.Rule+" "+a.Labels["service"]+" "+string(rune('0'+a.Firings)))
	}
	if want := "Q a 1,R a 3,R b 2,R c 1"; joinLines(lines) != want {
		t.Errorf("%s, want %s", joinLines(lines), want)
	}
}

func joinLines(ls []string) string {
	out := ""
	for i, l := range ls {
		if i > 0 {
			out += ","
		}
		out += l
	}
	return out
}
