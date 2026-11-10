package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Tying a busy rule's alerts to its firings looks only at the firings near
// each alert: forty thousand of each in well under a second, not the
// square of it.
func TestTieOneBusyRule(t *testing.T) {
	tr := &tier{within: func(string) time.Duration { return 2 * time.Minute }, ids: map[string][]int64{"A": {1}},
		near: map[string][]tied{}, memo: map[string]*tied{}}
	const n = 40000
	for i := range n {
		tr.near["A"] = append(tr.near["A"], tied{id: int64(i), start: int64(i * 60), labels: map[string]string{"pod": fmt.Sprint(i)}})
	}
	began := time.Now()
	for i := range n {
		f, err := tr.tie(Alert{Rule: "A", Labels: map[string]string{"pod": fmt.Sprint(i), "cluster": "x"}, Start: time.Unix(int64(i*60+30), 0)})
		if err != nil || f == nil || f.id != int64(i) {
			t.Fatalf("alert %d tied to %+v, %v", i, f, err)
		}
	}
	if took := time.Since(began); took > time.Duration(float64(time.Second)*slow) {
		t.Errorf("%d alerts took %s", n, took)
	}
}

// Each source counts from its own first notification: one connected later
// doesn't make the weeks before it look unpaged.
func TestNotifiedSinceBySource(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	day := func(d int) time.Time { return t0.Add(time.Duration(d) * 24 * time.Hour) }
	for _, n := range []Notification{
		{At: day(1), Source: "alertmanager", Receiver: "r", Group: "g1", Alerts: []Alert{{Rule: "Errors", Start: day(0)}}},
		{At: day(10), Source: "azure", Receiver: "r", Group: "g2", Alerts: []Alert{{Rule: "disk-full", Start: day(5), Resolved: true}}},
	} {
		if _, err := l.Notify(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	since, err := l.NotifiedSince(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !since["Errors"].Equal(day(1)) || !since["disk-full"].Equal(day(10)) {
		t.Errorf("since %v", since)
	}
}
