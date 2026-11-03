package firing

import (
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// at makes a series with a sample at each minute offset.
func at(labels map[string]string, minutes ...int) prom.Series {
	s := prom.Series{Labels: labels}
	for _, m := range minutes {
		s.Samples = append(s.Samples, prom.Sample{T: t0.Add(time.Duration(m) * time.Minute), V: 1})
	}
	return s
}

func span(from, to int) []int {
	var out []int
	for m := from; m <= to; m++ {
		out = append(out, m)
	}
	return out
}

func TestFromSeries(t *testing.T) {
	end := t0.Add(60 * time.Minute)
	series := []prom.Series{
		at(map[string]string{"__name__": "x", "pod": "a"}, span(0, 9)...),           // fires at 5, resolves at 10
		at(map[string]string{"pod": "b"}, span(0, 3)...),                            // pending only: never fires
		at(map[string]string{"pod": "c"}, append(span(10, 20), span(30, 60)...)...), // fires twice, the second still on
	}
	got := FromSeries("R", map[string]string{"severity": "page"}, series, time.Minute, For(5*time.Minute), 0, end)
	if len(got) != 3 {
		t.Fatalf("got %d firings: %+v", len(got), got)
	}
	a := got[0]
	if a.Labels["pod"] != "a" || !a.Start.Equal(t0.Add(5*time.Minute)) || !a.End.Equal(t0.Add(10*time.Minute)) {
		t.Errorf("pod a: %+v", a)
	}
	if _, ok := a.Labels["__name__"]; ok || a.Labels["severity"] != "page" {
		t.Errorf("labels %v: want the rule's labels and no __name__", a.Labels)
	}
	if c2 := got[2]; c2.Labels["pod"] != "c" || !c2.Start.Equal(t0.Add(35*time.Minute)) || !c2.End.IsZero() {
		t.Errorf("pod c second firing: %+v, want start 35m and still on", c2)
	}
}

func TestFromAlerts(t *testing.T) {
	series := []prom.Series{at(map[string]string{"alertname": "Down", "alertstate": "firing", "job": "api"}, span(0, 4)...)}
	got := FromAlerts(series, time.Minute, t0.Add(time.Hour))
	if len(got) != 1 || got[0].Rule != "Down" || !got[0].Start.Equal(t0) || got[0].Labels["job"] != "api" || got[0].Labels["alertname"] != "" {
		t.Errorf("%+v", got)
	}
}

func TestReadJSONL(t *testing.T) {
	got, err := ReadJSONL(strings.NewReader(`{"rule":"A","start":"2026-11-01T00:00:00Z","end":"2026-11-01T00:10:00Z"}

{"rule":"B","labels":{"x":"1"},"start":"2026-11-01T01:00:00Z"}
`))
	if err != nil || len(got) != 2 || !got[1].End.IsZero() {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := ReadJSONL(strings.NewReader(`{"start":"2026-11-01T00:00:00Z"}`)); err == nil {
		t.Error("a firing without a rule was accepted")
	}
}

func TestKeepFiringFor(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	at := func(mins ...int) prom.Series {
		s := prom.Series{Labels: map[string]string{"job": "api"}}
		for _, m := range mins {
			s.Samples = append(s.Samples, prom.Sample{T: t0.Add(time.Duration(m) * time.Minute), V: 1})
		}
		return s
	}
	// Holds at 0-2, gone 3-7, back at 8-9: one firing with keep 10m, ending
	// 10m after the step it was last seen; two without.
	s := at(0, 1, 2, 8, 9)
	kept := FromSeries("Flapping", nil, []prom.Series{s}, time.Minute, For(0), 10*time.Minute, t0.Add(time.Hour))
	if len(kept) != 1 || !kept[0].End.Equal(t0.Add(20*time.Minute)) {
		t.Errorf("with keep_firing_for 10m: %+v", kept)
	}
	plain := FromSeries("Flapping", nil, []prom.Series{s}, time.Minute, For(0), 0, t0.Add(time.Hour))
	if len(plain) != 2 || !plain[0].End.Equal(t0.Add(3*time.Minute)) {
		t.Errorf("without: %+v", plain)
	}
	// Still kept at the end of the period: no end.
	if open := FromSeries("Flapping", nil, []prom.Series{s}, time.Minute, For(0), 10*time.Minute, t0.Add(15*time.Minute)); len(open) != 1 || !open[0].End.IsZero() {
		t.Errorf("kept past the end: %+v", open)
	}
	// Pending, never firing: keep doesn't hold it.
	if p := FromSeries("Flapping", nil, []prom.Series{at(0, 1)}, time.Minute, For(5*time.Minute), 10*time.Minute, t0.Add(time.Hour)); len(p) != 0 {
		t.Errorf("pending only: %+v", p)
	}
}
