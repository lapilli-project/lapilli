package graph

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

var t0 = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

type fakeHistory struct {
	series []prom.Series
	err    error
	asked  []string
}

func (f *fakeHistory) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	f.asked = append(f.asked, fmt.Sprintf("%s|%s|%s|%s", expr, start.Format(time.RFC3339), end.Format(time.RFC3339), step))
	return f.series, f.err
}

func edgeSeries(from, to string, at ...time.Time) prom.Series {
	s := prom.Series{Labels: map[string]string{"src": from, "dst": to, "job": "x"}}
	for _, t := range at {
		s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
	}
	return s
}

func window(fromHour, toHour int) Window {
	return Window{Start: t0.Add(time.Duration(fromHour) * time.Hour), End: t0.Add(time.Duration(toHour)*time.Hour + 30*time.Minute)}
}

func TestDepends(t *testing.T) {
	b := NewBuilder()
	for _, e := range [][2]string{{"a", "b"}, {"b", "c"}, {"c", "d"}, {"a", "x"}, {"x", "c"}, {"d", "a"}, {"s", "s"}, {"", "q"}} {
		b.Add(t0, e[0], e[1])
	}
	g := b.Graph()
	if g.Edges() != 6 || g.Rows() != 6 {
		t.Errorf("edges %d rows %d, want 6 (a self-loop and an empty name are none)", g.Edges(), g.Rows())
	}
	for _, c := range []struct {
		from, to string
		depth    int
		want     []string
	}{
		{"a", "b", 1, []string{"a", "b"}},
		{"a", "c", 1, nil},
		{"a", "c", 2, []string{"a", "b", "c"}}, // by b, before x: the names order the ties
		{"a", "d", 2, nil},
		{"a", "d", 3, []string{"a", "b", "c", "d"}},
		{"a", "d", 99, []string{"a", "b", "c", "d"}},
		{"b", "a", 3, []string{"b", "c", "d", "a"}},
		{"b", "a", 2, nil},
		{"b", "a", 0, nil},
		{"a", "a", 3, nil},
		{"a", "nope", 3, nil},
		{"nope", "a", 3, nil},
	} {
		got, ok := g.Depends(c.from, c.to, window(0, 0), c.depth)
		if ok != (c.want != nil) || !slices.Equal(got, c.want) {
			t.Errorf("%s → %s depth %d: %v %v, want %v", c.from, c.to, c.depth, got, ok, c.want)
		}
	}
	var nilGraph *Graph
	if _, ok := nilGraph.Depends("a", "b", window(0, 0), 1); ok || nilGraph.Edges() != 0 || nilGraph.TooMany() {
		t.Error("a nil graph has edges")
	}
}

// Edges count from the hours of the window: a time in an hour is in it.
func TestDependsWindow(t *testing.T) {
	b := NewBuilder()
	b.Add(t0.Add(5*time.Hour+59*time.Minute), "a", "b")
	g := b.Graph()
	for _, c := range []struct {
		w  Window
		ok bool
	}{
		{Window{t0.Add(5 * time.Hour), t0.Add(5 * time.Hour)}, true},
		{Window{t0.Add(4 * time.Hour), t0.Add(5 * time.Hour)}, true},
		{Window{t0.Add(5*time.Hour + 30*time.Minute), t0.Add(8 * time.Hour)}, true},
		{Window{t0.Add(6 * time.Hour), t0.Add(8 * time.Hour)}, false},
		{Window{t0.Add(3 * time.Hour), t0.Add(4*time.Hour + 59*time.Minute)}, false},
		// An end before the start is its start's hour.
		{Window{t0.Add(5 * time.Hour), t0.Add(time.Hour)}, true},
	} {
		if _, ok := g.Depends("a", "b", c.w, 1); ok != c.ok {
			t.Errorf("window %v: %v, want %v", c.w, ok, c.ok)
		}
	}
}

// The hours don't depend on the zone the machine is in.
func TestHourIsUTC(t *testing.T) {
	seoul := time.FixedZone("KST", 9*3600)
	at := time.Date(2026, 10, 26, 8, 59, 0, 0, seoul) // 23:59 UTC the day before
	if got := HourStart(Hour(at)); !got.Equal(time.Date(2026, 10, 25, 23, 0, 0, 0, time.UTC)) || got.Location() != time.UTC {
		t.Errorf("hour of %v is %v", at, got)
	}
	if Hour(t0.Add(-time.Second)) != Hour(t0)-1 || HourStart(Hour(time.Unix(-1, 0))).Unix() != -3600 {
		t.Error("hours before the epoch round up")
	}
}

func TestBound(t *testing.T) {
	b := NewBuilder()
	for i := 0; i < MaxEdgesPerHour; i++ {
		if !b.Add(t0, fmt.Sprint("a", i), "b") {
			t.Fatalf("stopped at %d", i)
		}
	}
	// The same edge again, and another hour, are not past it.
	if !b.Add(t0, "a0", "b") || !b.Add(t0.Add(time.Hour), "a0", "b") || b.TooMany() {
		t.Fatal("stopped too early")
	}
	if b.Add(t0, "past", "b") || !b.TooMany() {
		t.Fatal("did not stop past the bound")
	}
	if b.Add(t0.Add(2*time.Hour), "x", "y") {
		t.Error("kept adding after it stopped")
	}
	g := b.Graph()
	if !g.TooMany() || g.Edges() != 0 || g.Rows() != 0 {
		t.Errorf("graph: too many %v, edges %d, rows %d: want none", g.TooMany(), g.Edges(), g.Rows())
	}
	if _, ok := g.Depends("a0", "b", window(0, 0), 1); ok {
		t.Error("a graph that kept none found a path")
	}
}

// Edges at the bound make no allocation grow with a window's asks: the
// adjacency of a window is worked out once.
func TestAdjacencyCached(t *testing.T) {
	b := NewBuilder()
	for i := 0; i < 2000; i++ {
		b.Add(t0, fmt.Sprint("a", i), fmt.Sprint("a", i+1))
	}
	g := b.Graph()
	w := window(0, 0)
	g.Depends("a0", "a3", w, 3)
	n := testing.AllocsPerRun(20, func() { g.Depends("a0", "a3", w, 3) })
	if n > 40 {
		t.Errorf("%v allocations for a walk over a worked-out window", n)
	}
	if len(g.cache) != 1 {
		t.Errorf("%d windows worked out", len(g.cache))
	}
	for i := 0; i < 3*maxCached; i++ {
		g.Depends("a0", "a3", Window{t0.Add(time.Duration(i) * time.Hour), t0.Add(time.Duration(i) * time.Hour)}, 3)
	}
	if len(g.cache) > maxCached {
		t.Errorf("%d windows kept, at most %d", len(g.cache), maxCached)
	}
}

func TestEach(t *testing.T) {
	b := NewBuilder()
	b.Add(t0.Add(time.Hour), "b", "c")
	b.Add(t0, "b", "c")
	b.Add(t0, "a", "b")
	var got []string
	b.Graph().Each(func(h time.Time, from, to string) {
		got = append(got, fmt.Sprintf("%s %s>%s", h.Format("15:04"), from, to))
	})
	if want := []string{"00:00 b>c", "00:00 a>b", "01:00 b>c"}; !slices.Equal(got, want) {
		t.Errorf("each: %v, want %v", got, want)
	}
	var nilGraph *Graph
	nilGraph.Each(func(time.Time, string, string) { t.Error("an edge of nothing") })
}

func TestRead(t *testing.T) {
	h := &fakeHistory{series: []prom.Series{
		edgeSeries("checkout", "payments-db", t0, t0.Add(time.Hour), t0.Add(90*time.Minute)),
		edgeSeries("cart", "checkout", t0),
		{Labels: map[string]string{"src": "lonely"}, Samples: []prom.Sample{{T: t0, V: 1}}}, // no callee
		edgeSeries("same", "same", t0),
	}}
	q := Query{Expr: "up", From: "src", To: "dst", Step: time.Hour}
	g, err := Read(context.Background(), h, q, t0.Add(10*time.Minute), t0.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// From the hour the start is in.
	if want := "up|2026-10-26T00:00:00Z|2026-10-26T03:00:00Z|1h0m0s"; len(h.asked) != 1 || h.asked[0] != want {
		t.Errorf("asked %v, want %q", h.asked, want)
	}
	if g.Edges() != 2 || g.Rows() != 3 {
		t.Errorf("edges %d rows %d, want 2 pairs, 3 hours", g.Edges(), g.Rows())
	}
	if p, ok := g.Depends("cart", "payments-db", window(0, 2), 2); !ok || !slices.Equal(p, []string{"cart", "checkout", "payments-db"}) {
		t.Errorf("path %v %v", p, ok)
	}

	for _, bad := range []Query{{From: "a", To: "b", Step: time.Hour}, {Expr: "x", From: "a", To: "a", Step: time.Hour}, {Expr: "x", From: "a", To: "b", Step: time.Minute}} {
		if _, err := Read(context.Background(), h, bad, t0, t0); err == nil {
			t.Errorf("%+v read", bad)
		}
	}
	h.err = fmt.Errorf("refused")
	if _, err := Read(context.Background(), h, q, t0, t0); err == nil {
		t.Error("an error from the history is lost")
	}
}

func TestReadTooMany(t *testing.T) {
	var s []prom.Series
	for i := 0; i <= MaxEdgesPerHour; i++ {
		s = append(s, edgeSeries(fmt.Sprint("a", i), "b", t0))
	}
	g, err := Read(context.Background(), &fakeHistory{series: s}, Query{Expr: "up", From: "src", To: "dst", Step: time.Hour}, t0, t0.Add(time.Hour))
	if err != nil || !g.TooMany() || g.Edges() != 0 {
		t.Errorf("%v: too many %v edges %d", err, g.TooMany(), g.Edges())
	}
}
