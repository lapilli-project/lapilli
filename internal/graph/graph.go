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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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
	// MaxNodesPerHour is the most distinct services one hour of the graph
	// may name. Past it the read stops and the graph keeps none, as past
	// MaxEdgesPerHour: whoever can send a series can send a name, and a
	// graph of thousands of invented ones is no graph of a system.
	MaxNodesPerHour = 5000
	// MaxName is the longest name of a service kept: a longer one isn't a
	// service's. 317 holds any Kubernetes "namespace/name": a namespace is
	// a DNS label of at most 63 characters, a Deployment's name a DNS
	// subdomain of at most 253, and a slash (63 + 1 + 253); 128 refused real
	// Deployments.
	MaxName = 317
	// maxReachNodes is how many services' paths the graph keeps worked out,
	// summed over what it was asked about; past it they are dropped, and
	// worked out again as asked. A path is at most MaxDepth+1 names that
	// share the graph's own strings (a slice of four string headers, 64
	// bytes, and its map slot, about 250 bytes in all), so the cache is
	// about 64 MiB at most.
	maxReachNodes = 1 << 18
	// maxAdjEdges is how many edges the adjacency of the windows asked about
	// is kept for, summed (a window's edges count again for its reverse
	// side): about 50 bytes each, 25 MiB at most. With maxReachNodes and a
	// judgement's edges (ledger.MaxJudgeEdges, 1,000,000 at 8 bytes and
	// the names), a graph's memory is about 100 MiB at most. A window
	// larger than that is walked and not kept.
	maxAdjEdges = 1 << 19
)

// ValidName reports whether a name may be a service's in the graph: not
// empty, at most MaxName bytes, valid UTF-8 of printable characters only (no
// control, format or bidirectional-control character, no space of any kind,
// the plain one too), no arrow-like symbol (isArrow), and not ",", which the
// report writes a path and its lists by: a name with one could forge a hop,
// or look like one.
func ValidName(name string) bool {
	if name == "" || len(name) > MaxName || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if r == ',' || isArrow(r) || unicode.IsSpace(r) || !unicode.IsPrint(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// isArrow reports whether r is an arrow that could draw a hop: the Arrows
// block (U+2190 to U+21FF), the supplemental arrows A and B (U+27F0 to
// U+27FF, U+2900 to U+297F), the miscellaneous symbols and arrows block
// (U+2B00 to U+2BFF), and the halfwidth arrows (U+FFE9 to U+FFEC).
func isArrow(r rune) bool {
	return r >= 0x2190 && r <= 0x21FF || r >= 0x27F0 && r <= 0x27FF || r >= 0x2900 && r <= 0x297F ||
		r >= 0x2B00 && r <= 0x2BFF || r >= 0xFFE9 && r <= 0xFFEC
}

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

	// refused is how many names were refused (ValidName), each time offered.
	refused int
	// bigHours are the hours that had more than is kept and were dropped
	// alone (Builder.PerHour).
	bigHours int

	mu    sync.Mutex
	reach map[string]map[string][]string
	// reachNodes is the services' paths reach holds; walks how many walks
	// it took to fill it.
	reachNodes, walks int
	// adjs are the adjacency of the windows asked about, by window, so that
	// incidents sharing one don't build it again; adjEdges the edges they
	// hold, bounded by maxAdjEdges.
	adjs     map[adjKey]adjEntry
	adjEdges int
	adjBuilt int // how many were built, for a test
}

type adjKey struct {
	lo, hi int64
	both   bool
}

type adjEntry struct {
	adj, radj map[int32][]int32
	edges     int
}

// Refused is how many names offered to the graph were refused as no
// service's (ValidName), counted each time they were offered.
func (g *Graph) Refused() int {
	if g == nil {
		return 0
	}
	return g.refused
}

// HoursTooMany is how many hours had more edges or services than are kept
// and were left without edges alone, the rest kept (Builder.PerHour).
func (g *Graph) HoursTooMany() int {
	if g == nil {
		return 0
	}
	return g.bigHours
}

// Walks is how many times the graph walked its calls from a set of services
// to answer Reach and Depends: it grows with the sets asked about (an
// incident's, in a window), not with the questions put to each.
func (g *Graph) Walks() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.walks
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
// It answers from Reach. A service never depends on itself.
func (g *Graph) Depends(from, to string, w Window, depth int) (path []string, ok bool) {
	if g == nil || from == to {
		return nil, false
	}
	p := g.Reach([]string{from}, w, depth, false)[to]
	return p, p != nil
}

// Reach is every service that values reach, or with both reach too, in at
// most depth calls (MaxDepth at most), over the edges that held in w: each
// service by the shortest way to it, as Depends says it, in call order from
// the caller to the callee. The way of a service several values reach is
// the shortest; ties go to the earlier value, and to the value's side
// calling. A value is not its own way, and a name the graph doesn't know is
// reached by none.
//
// It walks once for a set of values, a window's hours, a depth and a
// direction, and keeps the result: the many firings an incident is asked
// about are map lookups in it. The set it holds is at most the services the
// window's edges name (MaxNodesPerHour for each hour), and what the graph
// keeps of them is bounded at maxReachNodes paths, then dropped and worked
// out again as asked. The map is shared: callers must not change it, or
// its paths.
func (g *Graph) Reach(values []string, w Window, depth int, both bool) map[string][]string {
	if g == nil || len(g.hours) == 0 || depth < 1 || len(values) == 0 {
		return nil
	}
	depth = min(depth, MaxDepth)
	lo, hi := hourRange(w)
	var key strings.Builder
	key.WriteString(strconv.FormatInt(lo, 10) + "," + strconv.FormatInt(hi, 10) + "," + strconv.Itoa(depth) + "," + strconv.FormatBool(both))
	for _, v := range values {
		key.WriteString("," + strconv.Itoa(len(v)) + ":" + v)
	}
	k := key.String()
	g.mu.Lock()
	defer g.mu.Unlock()
	if r, ok := g.reach[k]; ok {
		return r
	}
	g.walks++
	adj, radj := g.cachedAdjacency(lo, hi, both)
	best := map[string][]string{}
	for _, v := range values {
		src := g.id(v)
		if src < 0 {
			continue
		}
		g.walk(adj, src, depth, false, best)
		if both {
			g.walk(radj, src, depth, true, best)
		}
	}
	if g.reach == nil || g.reachNodes+len(best) > maxReachNodes {
		g.reach, g.reachNodes = map[string]map[string][]string{}, 0
	}
	g.reach[k] = best
	g.reachNodes += len(best)
	return best
}

// walk is a breadth-first walk from src over adj, depth calls at most,
// giving best each service reached by a way shorter than it has; reverse
// says adj is the calls the other way, and the ways are written back.
func (g *Graph) walk(adj map[int32][]int32, src int32, depth int, reverse bool, best map[string][]string) {
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
				next = append(next, m)
				var path []string
				for x := m; x >= 0; x = parent[x] {
					path = append(path, g.names[x])
				}
				if !reverse {
					slices.Reverse(path)
				}
				if have, ok := best[g.names[m]]; !ok || len(path) < len(have) {
					best[g.names[m]] = path
				}
			}
		}
		level = next
	}
}

// cachedAdjacency is adjacency, kept for the next incident in the same
// window while what is kept is within maxAdjEdges (dropped, all of it,
// when it would pass). The caller holds g.mu.
func (g *Graph) cachedAdjacency(lo, hi int64, both bool) (adj, radj map[int32][]int32) {
	k := adjKey{lo, hi, both}
	if e, ok := g.adjs[k]; ok {
		return e.adj, e.radj
	}
	adj, radj = g.adjacency(lo, hi, both)
	g.adjBuilt++
	n := 0
	for _, v := range adj {
		n += len(v)
	}
	if both {
		n *= 2
	}
	if n > maxAdjEdges {
		return adj, radj
	}
	if g.adjs == nil || g.adjEdges+n > maxAdjEdges {
		g.adjs, g.adjEdges = map[adjKey]adjEntry{}, 0
	}
	g.adjs[k] = adjEntry{adj, radj, n}
	g.adjEdges += n
	return adj, radj
}

func (g *Graph) id(name string) int32 {
	if i, ok := g.ids[name]; ok {
		return i
	}
	return -1
}

func hourRange(w Window) (lo, hi int64) {
	lo, hi = Hour(w.Start), Hour(w.End)
	if hi < lo {
		hi = lo
	}
	return lo, hi
}

// adjacency is who each service called in any hour from lo to hi, sorted by
// name, and, when reverse, who called each. The caller holds g.mu.
func (g *Graph) adjacency(lo, hi int64, reverse bool) (adj, radj map[int32][]int32) {
	seen := map[edge]bool{}
	adj = map[int32][]int32{}
	if reverse {
		radj = map[int32][]int32{}
	}
	add := func(h int64) {
		for _, e := range g.hours[h] {
			if !seen[e] {
				seen[e] = true
				adj[e.from] = append(adj[e.from], e.to)
				if reverse {
					radj[e.to] = append(radj[e.to], e.from)
				}
			}
		}
	}
	if hi-lo < int64(len(g.hours)) {
		for h := lo; h <= hi; h++ {
			add(h)
		}
	} else {
		for h := range g.hours {
			if h >= lo && h <= hi {
				add(h)
			}
		}
	}
	byName := func(a, b int32) int { return cmpName(g.names[a], g.names[b]) }
	for _, v := range adj {
		slices.SortFunc(v, byName)
	}
	for _, v := range radj {
		slices.SortFunc(v, byName)
	}
	return adj, radj
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
	nodes   map[int64]map[int32]bool
	refused int
	tooMany bool
	// perHour says an hour past the bounds is dropped alone (PerHour); big
	// are the hours dropped.
	perHour bool
	big     map[int64]bool
}

// PerHour makes an hour with more than MaxEdgesPerHour edges or
// MaxNodesPerHour services made only that hour too many, not the graph:
// the hour keeps none of its edges (HoursTooMany counts them), and what
// is added to it is ignored, while the other hours are kept. For the
// ledger, whose rows for an hour are the union of several reads.
func (b *Builder) PerHour() *Builder {
	b.perHour, b.big = true, map[int64]bool{}
	return b
}

// NewBuilder returns a Builder with no edges.
func NewBuilder() *Builder {
	return &Builder{ids: map[string]int32{}, hours: map[int64]map[edge]bool{}, nodes: map[int64]map[int32]bool{}}
}

// TooMany says an hour has had more than MaxEdgesPerHour edges added:
// Graph keeps none.
func (b *Builder) TooMany() bool { return b.tooMany }

// Over says the graph has more edges in an hour than are kept, as Add does
// past MaxEdgesPerHour: Graph keeps none. For a caller that knows it without
// adding them (the ledger, loading more edges than a judgement may hold).
func (b *Builder) Over() { b.tooMany = true }

// Refused is how many names Add refused as no service's (ValidName).
func (b *Builder) Refused() int { return b.refused }

// Add says from called to in the hour, a time in it. A name that isn't a
// service's (ValidName: empty, longer than MaxName, a character that isn't
// printable, "→" or ","), counted in Refused, or the same on both sides, is
// no edge. It reports false once an hour has more than MaxEdgesPerHour
// edges or MaxNodesPerHour services: nothing more is kept.
func (b *Builder) Add(hour time.Time, from, to string) bool {
	if b.tooMany {
		return false
	}
	if !ValidName(from) || !ValidName(to) {
		b.refused++
		return true
	}
	if from == to {
		return true
	}
	h := Hour(hour)
	if b.big[h] {
		return true
	}
	es := b.hours[h]
	if es == nil {
		es = map[edge]bool{}
		b.hours[h] = es
	}
	ns := b.nodes[h]
	if ns == nil {
		ns = map[int32]bool{}
		b.nodes[h] = ns
	}
	e := edge{b.id(from), b.id(to)}
	if !es[e] {
		if len(es) >= MaxEdgesPerHour {
			return b.tooManyIn(h)
		}
		es[e] = true
	}
	ns[e.from], ns[e.to] = true, true
	if len(ns) > MaxNodesPerHour {
		return b.tooManyIn(h)
	}
	return true
}

// tooManyIn is the hour h having more than is kept: the graph's, or, with
// PerHour, the hour's alone, which is dropped. It reports Add's answer.
func (b *Builder) tooManyIn(h int64) bool {
	if !b.perHour {
		b.tooMany = true
		return false
	}
	b.big[h] = true
	delete(b.hours, h)
	delete(b.nodes, h)
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
	g := &Graph{hours: map[int64][]edge{}, refused: b.refused, bigHours: len(b.big)}
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
