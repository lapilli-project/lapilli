package ledger

import (
	"context"
	"strings"
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
	got, all, err := l.AlertCounts(context.Background(), t0, t0.Add(24*time.Hour), 3)
	if err != nil {
		t.Fatal(err)
	}
	if all["R"] != 6 || all["Q"] != 1 {
		t.Errorf("all %v", all)
	}
	var lines []string
	for _, a := range got {
		lines = append(lines, a.Rule+" "+a.Labels["service"]+" "+string(rune('0'+a.Firings)))
	}
	if want := "R a 3,R b 2,"; len(lines) != 3 || !strings.HasPrefix(joinLines(lines), want) || !strings.HasSuffix(lines[2], " 1") {
		t.Errorf("%s, want %s", joinLines(lines), want)
	}
}

// A rule's alerts that caught nothing, the most first, as many as asked,
// and how many there were.
func TestRuleAlerts(t *testing.T) {
	l := open(t, time.UTC)
	var fs []firing.Firing
	for i, svc := range []string{"a", "b", "a", "a", "b", "c"} {
		at := t0.Add(time.Duration(i) * time.Hour)
		fs = append(fs, firing.Firing{Rule: "R", Labels: map[string]string{"service": svc}, Start: at, End: at.Add(time.Minute)})
	}
	record(t, l, t0, t0.Add(24*time.Hour), fs)
	top, n, err := l.RuleAlerts(context.Background(), "R", t0, t0.Add(24*time.Hour), 2)
	if err != nil || n != 3 || len(top) != 2 || top[0].Labels["service"] != "a" || top[0].Firings != 3 || top[1].Firings != 2 {
		t.Errorf("%+v %d %v", top, n, err)
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
