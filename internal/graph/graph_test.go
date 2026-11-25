package graph

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
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
	// 200 services call each other: 39,800 pairs, so the 20,000th edge is
	// well inside the bound on services.
	n := 0
	for i := 0; n < MaxEdgesPerHour; i++ {
		for j := 0; j < 200 && n < MaxEdgesPerHour; j++ {
			if i%200 == j {
				continue
			}
			if !b.Add(t0, fmt.Sprint("a", i%200), fmt.Sprint("a", j)) {
				t.Fatalf("stopped at %d", n)
			}
			n++
		}
	}
	// The same edge again, and another hour, are not past it.
	if !b.Add(t0, "a0", "a1") || !b.Add(t0.Add(time.Hour), "a0", "b") || b.TooMany() {
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
	if _, ok := g.Depends("a0", "a1", window(0, 0), 1); ok {
		t.Error("a graph that kept none found a path")
	}
}

// The walk is per incident, not per firing: with more incidents (windows)
// than any cache holds, asked round and round about each by many firings,
// the graph walks once for each, and each firing's answer is a lookup.
func TestReachWalksOncePerIncident(t *testing.T) {
	b := NewBuilder()
	for i := 0; i < 2000; i++ {
		b.Add(t0, fmt.Sprint("a", i), fmt.Sprint("a", i+1))
	}
	// An edge in each of the hours the windows below are in.
	const incidents = 3000
	for i := 0; i < incidents; i++ {
		b.Add(t0.Add(time.Duration(i)*time.Hour), "x", "y")
	}
	g := b.Graph()
	wins := make([]Window, incidents)
	for i := range wins {
		at := t0.Add(time.Duration(i) * time.Hour)
		wins[i] = Window{at, at}
	}
	const firings = 20
	for f := 0; f < firings; f++ {
		for i := 0; i < incidents; i++ {
			if p, ok := g.Depends("a0", "a3", wins[i], 3); i == 0 && (!ok || len(p) != 4) {
				t.Fatalf("path %v %v", p, ok)
			}
			g.Reach([]string{"a0"}, wins[i], 3, false)
		}
	}
	// Depends and Reach of a0 share a walk: one for each incident, 20
	// firings × 3,000 incidents asked. (Hour 0 holds the chain; the rest's
	// walk finds nothing, and is kept the same.)
	if got := g.Walks(); got != incidents {
		t.Errorf("%d walks for %d incidents asked %d times each, want one each", got, incidents, firings)
	}
}

// An answer from a walked window allocates nothing but its key.
func TestReachAnswerAllocations(t *testing.T) {
	b := NewBuilder()
	for i := 0; i < 2000; i++ {
		b.Add(t0, fmt.Sprint("a", i), fmt.Sprint("a", i+1))
	}
	g := b.Graph()
	w := window(0, 0)
	g.Depends("a0", "a3", w, 3)
	if n := testing.AllocsPerRun(50, func() { g.Depends("a0", "a3", w, 3) }); n > 8 {
		t.Errorf("%v allocations to answer from a walked window", n)
	}
}

// Reach says what Depends says, every service by its shortest way, both
// ways, and holds to depth and to the window.
func TestReach(t *testing.T) {
	b := NewBuilder()
	b.Add(t0, "cart", "checkout")
	b.Add(t0, "checkout", "payments")
	b.Add(t0, "checkout", "inventory")
	b.Add(t0, "payments", "db")
	b.Add(t0.Add(5*time.Hour), "cart", "late")
	g := b.Graph()
	r := g.Reach([]string{"cart"}, window(0, 0), 2, false)
	want := map[string][]string{
		"checkout": {"cart", "checkout"}, "payments": {"cart", "checkout", "payments"}, "inventory": {"cart", "checkout", "inventory"},
	}
	if len(r) != len(want) {
		t.Errorf("reached %v, want %v", r, want)
	}
	for k, v := range want {
		if !slices.Equal(r[k], v) {
			t.Errorf("way to %s: %v, want %v", k, r[k], v)
		}
	}
	if _, ok := r["late"]; ok {
		t.Error("an edge outside the window reached")
	}
	up := g.Reach([]string{"db"}, window(0, 0), 3, true)
	if p := up["cart"]; !slices.Equal(p, []string{"cart", "checkout", "payments", "db"}) {
		t.Errorf("a caller reaches the callee: %v", p)
	}
	// Of two values, the shorter way; the earlier value on a tie.
	two := g.Reach([]string{"payments", "cart"}, window(0, 0), 3, false)
	if p := two["db"]; !slices.Equal(p, []string{"payments", "db"}) {
		t.Errorf("shortest of two values: %v", p)
	}
	if g.Reach([]string{"nobody"}, window(0, 0), 3, true) == nil {
		// an empty set is fine; it only mustn't panic.
		t.Log("empty")
	}
	var none *Graph
	if none.Reach([]string{"a"}, window(0, 0), 1, false) != nil {
		t.Error("a nil graph reached")
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"checkout", "payments-db", "ns/checkout", "서비스", "a b", "x.y_z"} {
		if !ValidName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{
		"", "a\u202eb", "a\x00b", "a\nb", "a\tb", "a\u200bb", "a\u2066b", "a → b", "a,b", "a\u00a0b", "a\u2028b",
		"bad\xffutf8", strings.Repeat("x", MaxName+1),
	} {
		if ValidName(bad) {
			t.Errorf("%q kept", bad)
		}
	}
	if !ValidName(strings.Repeat("x", MaxName)) {
		t.Error("a name of MaxName refused")
	}
}

func TestAddRefusesNames(t *testing.T) {
	b := NewBuilder()
	b.Add(t0, "checkout", "pay\u202ements")
	b.Add(t0, "a → b", "c")
	b.Add(t0, "a, b", "c")
	b.Add(t0, "ok", "fine")
	g := b.Graph()
	if g.Edges() != 1 || g.Refused() != 3 || b.Refused() != 3 {
		t.Errorf("edges %d, refused %d: want 1 edge, 3 names refused", g.Edges(), g.Refused())
	}
}

func TestTooManyNodesInAnHour(t *testing.T) {
	b := NewBuilder()
	// Each edge names two new services: the bound is on services, not edges.
	i := 0
	for ; i < MaxNodesPerHour/2; i++ {
		if !b.Add(t0, fmt.Sprint("a", i), fmt.Sprint("b", i)) {
			t.Fatalf("stopped at %d", i)
		}
	}
	if b.TooMany() {
		t.Fatal("stopped at the bound")
	}
	// Another hour has its own count.
	if !b.Add(t0.Add(time.Hour), "a0", "b0") {
		t.Fatal("another hour stopped")
	}
	if b.Add(t0, "one", "more") || !b.TooMany() {
		t.Fatal("did not stop past the bound")
	}
	if g := b.Graph(); !g.TooMany() || g.Edges() != 0 {
		t.Errorf("kept %d edges, too many %v", g.Edges(), g.TooMany())
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
	if want := []string{"00:00 a>b", "00:00 b>c", "01:00 b>c"}; !slices.Equal(got, want) {
		t.Errorf("each: %v, want %v", got, want)
	}
	var nilGraph *Graph
	nilGraph.Each(func(time.Time, string, string) { t.Error("an edge of nothing") })
}

func TestRead(t *testing.T) {
	h := &fakeHistory{series: []prom.Series{
		// Samples inside hours, as a 10m step gives: the hour each is in.
		edgeSeries("checkout", "payments-db", t0.Add(30*time.Minute), t0.Add(90*time.Minute), t0.Add(100*time.Minute)),
		edgeSeries("cart", "checkout", t0.Add(30*time.Minute)),
		{Labels: map[string]string{"src": "lonely"}, Samples: []prom.Sample{{T: t0.Add(30 * time.Minute), V: 1}}}, // no callee
		edgeSeries("same", "same", t0.Add(30*time.Minute)),
	}}
	q := Query{Expr: "up", From: "src", To: "dst", Step: time.Hour}
	g, err := Read(context.Background(), h, q, t0.Add(10*time.Minute), t0.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// From the hour the start is in, to the step past the end: the last
	// sample covers the hour before it.
	if want := "up|2026-10-26T00:00:00Z|2026-10-26T04:00:00Z|1h0m0s"; len(h.asked) != 1 || h.asked[0] != want {
		t.Errorf("asked %v, want %q", h.asked, want)
	}
	if g.Edges() != 2 || g.Rows() != 3 {
		t.Errorf("edges %d rows %d, want 2 pairs, 3 hours", g.Edges(), g.Rows())
	}
	if p, ok := g.Depends("cart", "payments-db", window(0, 2), 2); !ok || !slices.Equal(p, []string{"cart", "checkout", "payments-db"}) {
		t.Errorf("path %v %v", p, ok)
	}

	for _, bad := range []Query{{From: "a", To: "b", Step: time.Hour}, {Expr: "x", From: "a", To: "a", Step: time.Hour}, {Expr: "x", From: "a", To: "b", Step: time.Minute}, {Expr: "x", From: "a", To: "b", Step: 2 * time.Hour}} {
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

// A sample is stored in the hour it covers: one at an hour's start, the
// hour before; none for a value that isn't a call (NaN, zero, negative).
func TestReadHourCovered(t *testing.T) {
	zero := edgeSeries("z", "y", t0.Add(30*time.Minute))
	zero.Samples[0].V = 0
	neg := edgeSeries("n", "y", t0.Add(30*time.Minute))
	neg.Samples[0].V = -1
	nan := edgeSeries("m", "y", t0.Add(30*time.Minute))
	nan.Samples[0].V = math.NaN()
	h := &fakeHistory{series: []prom.Series{
		edgeSeries("a", "b", t0.Add(time.Hour)),                  // 01:00 says 00:00 to 01:00
		edgeSeries("c", "d", t0.Add(time.Hour+time.Nanosecond)),  // 01:00:00.000000001 is in 01:00
		edgeSeries("e", "f", t0.Add(2*time.Hour+30*time.Minute)), // inside the hour of 02:00
		zero, neg, nan,
	}}
	q := Query{Expr: "up", From: "src", To: "dst", Step: 10 * time.Minute}
	g, err := Read(context.Background(), h, q, t0, t0.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if g.Edges() != 3 {
		t.Errorf("edges %d, want 3 (no call has a value of 0, less, or NaN)", g.Edges())
	}
	hour := func(n int, from, to string) bool {
		_, ok := g.Depends(from, to, Window{t0.Add(time.Duration(n) * time.Hour), t0.Add(time.Duration(n) * time.Hour)}, 1)
		return ok
	}
	if !hour(0, "a", "b") || hour(1, "a", "b") {
		t.Error("a sample at 01:00 is the hour of 00:00 alone")
	}
	if hour(0, "c", "d") || !hour(1, "c", "d") {
		t.Error("a sample just past 01:00 is the hour of 01:00")
	}
	if !hour(2, "e", "f") || hour(3, "e", "f") {
		t.Error("a sample at 02:30 is the hour of 02:00")
	}
}
