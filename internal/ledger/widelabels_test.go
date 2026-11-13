package ledger

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// A replayed alert with a label value of 300 bytes (a URL, an image with
// its digest and tag) is kept cut to 256, while what really fired (the
// ALERTS series) is read as the API gives it: lapilli serve's replay check
// (checkDay: ReplayedFirings beside firing.FromAlerts) takes what really
// fired as it would be kept (verdict.KeptValue), and the two agree, as
// before 0.34.1, not one alert as two misses.
func TestReplayCheckLongLabelValue(t *testing.T) {
	l := open(t, time.UTC)
	url := "https://checkout.example.com/api/v2/orders?" + strings.Repeat("q", 260)
	f := firing.Firing{Rule: "Errors", Labels: map[string]string{"alertname": "Errors", "service": "checkout", "url": url},
		Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}
	record(t, l, t0, t0.Add(6*time.Hour), []firing.Firing{f})
	replayed, err := l.ReplayedFirings(context.Background(), t0, t0.Add(6*time.Hour))
	if err != nil || len(replayed) != 1 {
		t.Fatalf("%v, %d", err, len(replayed))
	}
	known := []rules.Rule{{Name: "Errors", Interval: time.Minute}}
	c, ok := verdict.CheckSpan(context.Background(), known, replayed, []firing.Firing{f}, t0, t0.Add(6*time.Hour), time.Minute)
	if !ok {
		t.Fatal("not checked")
	}
	if r := c.Rules["Errors"]; r == nil || r.Alike != 1 || len(r.Misses) != 0 {
		t.Errorf("the same alert, replayed and really fired, isn't alike: %+v (replayed url %d bytes, real %d)", r, len(replayed[0].Labels["url"]), len(url))
	}
}

// Two values that differ only in a character that turns text around are
// kept apart, a hash of what was replaced after them; two names that
// differ so, in one set, are two names, the same each time.
func TestKeptLabelsTurnsApart(t *testing.T) {
	a := keptLabels(map[string]string{"service": "checkout", "x": "disk \u202efull"})
	b := keptLabels(map[string]string{"service": "checkout", "x": "disk \u202dfull"})
	if reflect.DeepEqual(a, b) {
		t.Errorf("two alerts apart kept as one: %q", a["x"])
	}
	in := map[string]string{"service": "checkout", "a\u202eb": "1", "a\u202db": "2"}
	first := keptLabels(in)
	for range 200 {
		if got := keptLabels(in); !reflect.DeepEqual(got, first) {
			t.Fatalf("one label set kept two ways: %v and %v", first, got)
		}
	}
}

// keptLabels runs for each firing (seriesID), before the series is looked
// up, and a set past 4 KB is sorted, each label's length as JSON counted
// once; it was written out again at each comparison. A range answer may hold a series of 4 MB of labels
// (valueBytes), some 280,000 short ones, and a series that flaps fires once
// each other step: a hostile API's one series of 15,000 labels firing 20
// times holds the ledger's writer, in Record, for seconds (70 ms at
// 05a1887), some 150 ms a firing; 100,000 labels, 0.55 s a firing, 720
// firings a day. The run's context has no deadline there.
func TestWideSeriesRecordCost(t *testing.T) {
	m := map[string]string{"alertname": "Down"}
	for i := range 15000 {
		m[fmt.Sprintf("l%06d", i)] = "v"
	}
	var fs []firing.Firing
	for i := range 20 {
		s := t0.Add(time.Duration(2*i) * time.Minute)
		fs = append(fs, firing.Firing{Rule: "Down", Labels: m, Start: s, End: s.Add(time.Minute)})
	}
	l := open(t, time.UTC)
	began := time.Now()
	record(t, l, t0, t0.Add(time.Hour), fs)
	took := time.Since(began)
	t.Logf("20 firings of one series of 15,000 labels recorded in %s", took)
	if took > time.Second*time.Duration(slow) {
		t.Errorf("20 firings of one wide series took %s to record, the ledger's writer held: the labels are kept again for each firing", took)
	}
}
