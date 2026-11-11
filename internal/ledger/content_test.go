package ledger

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// The label names a rule's alerts had are those of firings since a time,
// or still firing; a rule whose alerts had none fired all the same.
func TestLabelNames(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	fs := []firing.Firing{
		{Rule: "A", Labels: map[string]string{"service": "x"}, Start: at, End: at.Add(time.Minute)},
		{Rule: "A", Labels: map[string]string{"pod": "p"}, Start: at.AddDate(0, 0, -100), End: at.AddDate(0, 0, -100).Add(time.Minute)},
		{Rule: "B", Labels: map[string]string{}, Start: at, End: at.Add(time.Minute)},
		{Rule: "C", Labels: map[string]string{"old": "y"}, Start: at.AddDate(0, 0, -100)}, // still firing
	}
	record(t, l, at.AddDate(0, 0, -101), at.Add(time.Hour), fs)
	got, err := l.LabelNames(ctx, at.AddDate(0, 0, -84))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got["A"]["service"] || got["A"]["pod"] || got["B"] == nil || len(got["B"]) != 0 || !got["C"]["old"] {
		t.Errorf("%v", got)
	}
}
