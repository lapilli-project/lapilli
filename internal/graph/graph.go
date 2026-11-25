// Package graph is the service graph the history keeps: who calls whom, by
// the hour (docs/design/dependencies.md). It says which service depends on
// which; it keeps no record of whether users were hurt, so it only widens
// which firings catch an incident (verdict.Options.Depends).
package graph

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"lapilli.dev/internal/prom"
)

const (
	// MaxEdgesPerHour is the most edges one hour of the graph may have.
	// Past it the read stops and the graph keeps none: a part of a graph
	// would judge by part of the calls.
	MaxEdgesPerHour = 20000
	// MaxDepth is the most calls from a service a walk follows.
	MaxDepth = 3
	// MinStep is the finest step the graph is read at.
	MinStep = 10 * time.Minute
	// MaxStep is the coarsest: an expression counts calls over an hour
	// (`rate(...[1h]) > 0`), and a step longer than that leaves hours
	// with no sample to say what they held.
	MaxStep = time.Hour
	// DefaultStep is the step the graph is read at when none is given.
	DefaultStep = time.Hour
	// MaxName is the longest name of a service kept: a longer one isn't a
	// service's.
	MaxName = 256
	// maxCached is how many windows' adjacency a graph keeps worked out.
	maxCached = 1024
)

// Hour is the hour a time is in, as a count of hours since the epoch.
func Hour(t time.Time) int64 { return floorDiv(t.Unix(), 3600) }

// HourStart is the start of an hour (Hour), in UTC.
func HourStart(h int64) time.Time { return time.Unix(h*3600, 0).UTC() }

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

type edge struct{ from, to int32 }

// Window is the times an incident's edges are taken from: edges that held
// in any hour that Start or End, or a time between, is in.
type Window struct{ Start, End time.Time }

// Graph is who called whom, in each hour. Its zero value, and nil, are a
// graph with no edges. It is safe to use from several goroutines.
type Graph struct {
	names []string
	ids   map[string]int32
	hours map[int64][]edge // sorted, each once
	// distinct are the pairs that held in any hour.
	distinct int
	rows     int
	// tooMany says an hour had more than MaxEdgesPerHour edges: the graph
	// keeps none.
	tooMany bool

	mu    sync.Mutex
	cache map[[2]int64]map[int32][]int32
}

// Edges is how many pairs of services held an edge in any hour.
func (g *Graph) Edges() int {
	if g == nil {
		return 0
	}
	return g.distinct
}

// Rows is how many edges there are, counting each for every hour it held:
// what lapilli serve's ledger keeps.
func (g *Graph) Rows() int {
	if g == nil {
		return 0
	}
	return g.rows
}

// TooMany says the read stopped, with more than MaxEdgesPerHour edges in
// an hour: the graph keeps none.
func (g *Graph) TooMany() bool { return g != nil && g.tooMany }

// Each calls fn for each edge in each hour, the oldest hour first, each
// hour's edges by name.
func (g *Graph) Each(fn func(hour time.Time, from, to string)) {
	if g == nil {
		return
	}
	hs := make([]int64, 0, len(g.hours))
	for h := range g.hours {
		hs = append(hs, h)
	}
	slices.Sort(hs)
	for _, h := range hs {
		type pair struct{ from, to string }
		es := make([]pair, 0, len(g.hours[h]))
		for _, e := range g.hours[h] {
			es = append(es, pair{g.names[e.from], g.names[e.to]})
		}
		slices.SortFunc(es, func(a, b pair) int {
			if c := cmpName(a.from, b.from); c != 0 {
				return c
			}
			return cmpName(a.to, b.to)
		})
		for _, e := range es {
			fn(HourStart(h), e.from, e.to)
		}
	}
}

// Depends reports whether from calls to, directly or through other
// services, in at most depth calls (MaxDepth at most), over the edges that
// held in w; and the shortest way, from to, its services in call order.
// It is a breadth-first walk bounded by depth and the edges kept. A
// service never depends on itself.
func (g *Graph) Depends(from, to string, w Window, depth int) (path []string, ok bool) {
	if g == nil || len(g.hours) == 0 || from == to || depth < 1 {
		return nil, false
	}
	depth = min(depth, MaxDepth)
	src, dst := g.id(from), g.id(to)
	if src < 0 || dst < 0 {
		return nil, false
	}
	adj := g.adjacency(w)
	parent := map[int32]int32{src: -1}
	level := []int32{src}
	for d := 0; d < depth && len(level) > 0; d++ {
		var next []int32
		for _, n := range level {
			for _, m := range adj[n] {
				if _, seen := parent[m]; seen {
					continue
				}
				parent[m] = n
				if m == dst {
					var rev []string
					for x := m; x >= 0; x = parent[x] {
						rev = append(rev, g.names[x])
					}
					slices.Reverse(rev)
					return rev, true
				}
				next = append(next, m)
			}
		}
		level = next
	}
	return nil, false
}

func (g *Graph) id(name string) int32 {
	if i, ok := g.ids[name]; ok {
		return i
	}
	return -1
}

// adjacency is who each service called in any hour of w, sorted by id.
func (g *Graph) adjacency(w Window) map[int32][]int32 {
	lo, hi := Hour(w.Start), Hour(w.End)
	if hi < lo {
		hi = lo
	}
	key := [2]int64{lo, hi}
	g.mu.Lock()
	defer g.mu.Unlock()
	if a, ok := g.cache[key]; ok {
		return a
	}
	seen := map[edge]bool{}
	adj := map[int32][]int32{}
	for h, es := range g.hours {
		if h < lo || h > hi {
			continue
		}
		for _, e := range es {
			if !seen[e] {
				seen[e] = true
				adj[e.from] = append(adj[e.from], e.to)
			}
		}
	}
	for _, v := range adj {
		slices.SortFunc(v, func(a, b int32) int { return cmpName(g.names[a], g.names[b]) })
	}
	if g.cache == nil || len(g.cache) >= maxCached {
		g.cache = map[[2]int64]map[int32][]int32{}
	}
	g.cache[key] = adj
	return adj
}

func cmpName(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Builder gathers a graph's edges, hour by hour.
type Builder struct {
	ids     map[string]int32
	names   []string
	hours   map[int64]map[edge]bool
	tooMany bool
}

// NewBuilder returns a Builder with no edges.
func NewBuilder() *Builder {
	return &Builder{ids: map[string]int32{}, hours: map[int64]map[edge]bool{}}
}

// TooMany says an hour has had more than MaxEdgesPerHour edges added:
// Graph keeps none.
func (b *Builder) TooMany() bool { return b.tooMany }

// Over says the graph has more edges in an hour than are kept, as Add does
// past MaxEdgesPerHour: Graph keeps none. For a caller that knows it without
// adding them (the ledger, loading more edges than a judgement may hold).
func (b *Builder) Over() { b.tooMany = true }

// Add says from called to in the hour, a time in it. A name that is empty,
// longer than MaxName, or the same on both sides is no edge. It reports
// false once an hour has more than MaxEdgesPerHour: nothing more is kept.
func (b *Builder) Add(hour time.Time, from, to string) bool {
	if b.tooMany {
		return false
	}
	if from == "" || to == "" || from == to || len(from) > MaxName || len(to) > MaxName {
		return true
	}
	h := Hour(hour)
	es := b.hours[h]
	if es == nil {
		es = map[edge]bool{}
		b.hours[h] = es
	}
	e := edge{b.id(from), b.id(to)}
	if !es[e] {
		if len(es) >= MaxEdgesPerHour {
			b.tooMany = true
			return false
		}
		es[e] = true
	}
	return true
}

func (b *Builder) id(name string) int32 {
	if i, ok := b.ids[name]; ok {
		return i
	}
	i := int32(len(b.names))
	b.ids[name] = i
	b.names = append(b.names, name)
	return i
}

// Graph is the graph of what was added. With too many edges in an hour it
// has none, and says so (TooMany).
func (b *Builder) Graph() *Graph {
	g := &Graph{hours: map[int64][]edge{}}
	if b.tooMany {
		g.tooMany = true
		return g
	}
	g.names, g.ids = b.names, b.ids
	pairs := map[edge]bool{}
	for h, es := range b.hours {
		list := make([]edge, 0, len(es))
		for e := range es {
			list = append(list, e)
			pairs[e] = true
		}
		slices.SortFunc(list, func(a, c edge) int {
			if a.from != c.from {
				return int(a.from - c.from)
			}
			return int(a.to - c.to)
		})
		g.hours[h] = list
		g.rows += len(list)
	}
	g.distinct = len(pairs)
	return g
}

// History is what the graph is read from.
type History interface {
	QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error)
}

// Query is how a graph is read: an expression whose series each name a
// caller by label From and a callee by label To.
type Query struct {
	Expr     string
	From, To string
	Step     time.Duration
}

// Validate checks a query: an expression, two different label names, a
// step from MinStep to MaxStep.
func (q Query) Validate() error {
	switch {
	case q.Expr == "":
		return errors.New("no expression")
	case q.From == "" || q.To == "" || q.From == q.To:
		return errors.New("want two different labels, the caller's and the callee's")
	case q.Step < MinStep:
		return fmt.Errorf("step %s: %s at the finest", q.Step, MinStep)
	case q.Step > MaxStep:
		return fmt.Errorf("step %s: %s at the coarsest, as the expression counts calls over the hour", q.Step, MaxStep)
	}
	return nil
}

// Read reads the graph over from to to (hours: from the hour from is in to
// the hour to is in) as q says. The expression counts calls over the hour
// before each sample (`rate(...[1h]) > 0`), so a sample at T is stored in
// the hour it covers, the one T less a nanosecond is in: a sample at 10:00
// says 09:00 to 10:00, and one at 10:30 says what the hour of 10:00 held so
// far. The read goes to the step past to, to take the hour to is in whole. An hour with more than MaxEdgesPerHour
// edges stops the read: the graph keeps none, and says so (TooMany). A
// series without both labels, or with the same service on both, is no edge.
func Read(ctx context.Context, h History, q Query, from, to time.Time) (*Graph, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	start := HourStart(Hour(from))
	series, err := h.QueryRange(ctx, q.Expr, start, to.Add(q.Step), q.Step)
	if err != nil {
		return nil, err
	}
	b := NewBuilder()
	for _, s := range series {
		f, t := s.Labels[q.From], s.Labels[q.To]
		for _, p := range s.Samples {
			if math.IsNaN(p.V) || p.V <= 0 {
				continue
			}
			if !b.Add(p.T.Add(-time.Nanosecond), f, t) {
				break
			}
		}
		if b.TooMany() {
			break
		}
	}
	return b.Graph(), nil
}
