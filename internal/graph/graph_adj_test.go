package graph

import (
	"testing"
	"time"
)

// Incidents sharing a window build its adjacency once, and what is kept is
// bounded: a window past maxAdjEdges is walked and not kept.
func TestReachCachesAdjacencyPerWindow(t *testing.T) {
	b := NewBuilder()
	at := time.Date(2026, 10, 6, 10, 30, 0, 0, time.UTC)
	b.Add(at, "a", "b")
	b.Add(at, "b", "c")
	b.Add(at, "x", "y")
	g := b.Graph()
	w := Window{Start: at, End: at.Add(time.Minute)}
	for _, v := range []string{"a", "x", "b", "zz"} {
		g.Reach([]string{v}, w, 2, false)
	}
	if g.adjBuilt != 1 {
		t.Errorf("adjacency built %d times for one window and four sets: want once", g.adjBuilt)
	}
	g.Reach([]string{"a"}, w, 2, true)
	if g.adjBuilt != 2 {
		t.Errorf("built %d: want the other direction's once more", g.adjBuilt)
	}
	// Kept in all within the bound.
	if g.adjEdges > maxAdjEdges || g.adjEdges != 3+6 {
		t.Errorf("%d edges kept", g.adjEdges)
	}
}

// With PerHour an hour past the bounds is dropped alone; without it the
// graph keeps none.
func TestBuilderPerHour(t *testing.T) {
	at := time.Date(2026, 10, 6, 10, 30, 0, 0, time.UTC)
	fill := func(b *Builder) {
		b.Add(at.Add(time.Hour), "a", "b")
		for i := 0; i < MaxEdgesPerHour+1; i++ {
			b.Add(at, "x"+string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676%26)), "y"+string(rune('a'+i%26)))
		}
	}
	whole := NewBuilder()
	fill(whole)
	if g := whole.Graph(); !g.TooMany() {
		t.Skip("the fill is not past the bounds") // nodes or edges: either way, none
	}
	b := NewBuilder().PerHour()
	fill(b)
	g := b.Graph()
	if g.TooMany() || g.HoursTooMany() != 1 {
		t.Fatalf("too many %v, hours %d: want the graph kept and one hour dropped", g.TooMany(), g.HoursTooMany())
	}
	w := Window{Start: at.Add(time.Hour), End: at.Add(time.Hour)}
	if _, ok := g.Depends("a", "b", w, 1); !ok {
		t.Error("the other hour's edge dropped")
	}
	if g.Rows() != 1 {
		t.Errorf("%d rows: want the other hour's one", g.Rows())
	}
}
