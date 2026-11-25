package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/graph"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// The service graph a catch by dependency is judged by is kept in the
// ledger (docs/design/dependencies.md, "In the ledger"): each run reads it
// from the history for the hours it judges, keeps its edges by the hour,
// and judging again, with the history away, asks it nothing.

// MaxJudgeEdges is the most edges one judgement loads: the edges of the
// hours of its incidents' windows, each an hour before its start to its end,
// the windows apart, not the hours between them. Past it the judgement is
// made with no graph, as a graph read past graph.MaxEdgesPerHour is none: no
// catch by dependency, rather than one by part of a graph; and it is said
// (EdgesOver), on the pages, in the report and in the log. A graph of 5,950
// pairs an hour, the most seen, fills it with 168 hours of windows, a week's
// worth; one judgement of a four-week period has the hours its incidents'
// windows span, not the four weeks'. A week of the most an hour may have is
// 3.36 million; this is the memory a judgement may hold, about 50 MB.
const MaxJudgeEdges = 1_000_000

// maxJudgeEdges is MaxJudgeEdges, which tests lower.
var maxJudgeEdges = MaxJudgeEdges

// The most edges the ledger keeps: in a day, and in all (counting each for
// every hour it held). A read that would go past either is not kept, its
// hours are not read, and the Settings page says so, until the edges of the
// days past --keep go or the graph is narrowed. A day of the most seen,
// 5,950 pairs an hour, is 142,800; four weeks of it, which a change of the
// expression reads, 4.0 million.
const (
	MaxEdgesPerDay = 200_000
	MaxEdgesKept   = 5_000_000
)

// LowerJudgeEdges sets the most edges a judgement loads to n, for tests of
// what a judgement past it says; the func it returns puts it back.
func LowerJudgeEdges(n int) (restore func()) {
	was := maxJudgeEdges
	maxJudgeEdges = n
	return func() { maxJudgeEdges = was }
}

// maxEdgesPerDay and maxEdgesKept are the bounds, which tests lower.
var (
	maxEdgesPerDay = MaxEdgesPerDay
	maxEdgesKept   = MaxEdgesKept
)

// ErrEdgesPerDay and ErrEdgesKept say a read has more edges than are kept.
var (
	ErrEdgesPerDay = fmt.Errorf("the service graph has more than %d edges in a day: none of it is kept, and no firing of those hours is judged by it. Narrow --depends to the calls between services (sum by the two labels)", MaxEdgesPerDay)
	ErrEdgesKept   = fmt.Errorf("the ledger holds as many edges as it keeps (%d, counting each for every hour it held): the hours are not read until what is past --keep is let go. Narrow --depends to the calls between services (sum by the two labels)", MaxEdgesKept)
)

// metaEdgesKey is the meta key of what the edges the ledger holds were read
// with (EdgesKey): the expression, its labels and its step.
const metaEdgesKey = "edges-key"

// EdgesRead is a service graph read over a span, as a run or a read again
// found it, to be kept (Found.Edges): From to To are the span asked, and
// Graph what it returned, none (nil) for one that couldn't be read, which
// EdgeFailed notes instead.
type EdgesRead struct {
	Graph    *graph.Graph
	From, To time.Time
	// Key is what it was read with, as the round that read it had it
	// (EdgesKey): when the ledger holds another by the time it is kept,
	// because the graph was changed meanwhile, it isn't kept. "" keeps it
	// whatever the ledger holds.
	Key string
}

// EdgesKey is what the graph the ledger holds was read with, as lapilli
// serve keys it; "" for none.
func (l *Ledger) EdgesKey(ctx context.Context) (string, error) {
	return l.meta(ctx, l.r, metaEdgesKey)
}

// ReplaceEdges drops what the ledger holds of the graph and keeps a graph
// read with another expression, labels or step (key; nil for none yet, or
// for the graph turned off) in its place, in one write. It judges nothing:
// the key is what the stones are judged by, and a ledger judged by another
// judges every firing again (lapilli serve).
func (l *Ledger) ReplaceEdges(ctx context.Context, key string, e *EdgesRead) error {
	return l.write(ctx, func(t *txn) error {
		for _, q := range []string{"DELETE FROM edges", "DELETE FROM edge_spans", "DELETE FROM edge_retries"} {
			if _, err := t.ExecContext(t.ctx, q); err != nil {
				return err
			}
		}
		if err := setMeta(t.ctx, t, metaEdgesKey, key); err != nil {
			return err
		}
		return l.keepEdges(t, e)
	})
}

// EdgesKept is how many edges the ledger holds, counting each for every
// hour it held, and the spans the graph was read over, the earliest first.
func (l *Ledger) EdgesKept(ctx context.Context) (rows int64, spans [][2]time.Time, err error) {
	if err = l.r.QueryRowContext(ctx, "SELECT count(*) FROM edges").Scan(&rows); err != nil {
		return 0, nil, err
	}
	r, err := l.r.QueryContext(ctx, "SELECT from_at, to_at FROM edge_spans ORDER BY from_at")
	if err != nil {
		return 0, nil, err
	}
	defer r.Close()
	for r.Next() {
		var a, b int64
		if err := r.Scan(&a, &b); err != nil {
			return 0, nil, err
		}
		spans = append(spans, [2]time.Time{unix(a), unix(b)})
	}
	return rows, spans, r.Err()
}

// AddEdges keeps a graph read over a part of a span being read again after
// ReplaceEdges, which judges nothing: the ledger is judged again as a whole
// once the key it is judged by has changed.
func (l *Ledger) AddEdges(ctx context.Context, e *EdgesRead) error {
	return l.write(ctx, func(t *txn) error { return l.keepEdges(t, e) })
}

// KeepEdges keeps a graph read outside a run (a retry, or a read again
// after a change of the expression), and judges the span again, which a
// run's own judging doesn't reach.
func (l *Ledger) KeepEdges(ctx context.Context, e *EdgesRead, o verdict.Options) error {
	err := l.write(ctx, func(t *txn) error {
		if err := l.keepEdges(t, e); err != nil {
			return err
		}
		if e == nil || e.Graph == nil || e.Graph.TooMany() {
			return nil
		}
		if e.Key != "" {
			if held, err := l.meta(t.ctx, t, metaEdgesKey); err != nil || held != e.Key {
				return err
			}
		}
		return pend(t, [][2]int64{{e.From.Unix() - int64(o.MaxLead()/time.Second), e.To.Unix()}})
	})
	if err == nil {
		l.judgeSoon(ctx, o)
	}
	return err
}

// keepEdges keeps the edges of a graph read and the span they were read
// over, as one with the spans it overlaps or touches. A graph that kept none
// for having too many in an hour keeps no span: nothing is known of its
// hours.
func (l *Ledger) keepEdges(t *txn, e *EdgesRead) error {
	if e == nil || e.Graph == nil || e.Graph.TooMany() {
		return nil
	}
	if e.Key != "" {
		if held, err := l.meta(t.ctx, t, metaEdgesKey); err != nil || held != e.Key {
			return err
		}
	}
	// Many rows to a statement, in both databases: a four-week read has
	// millions.
	var args []any
	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		n := len(args) / 3
		q := "INSERT INTO edges (hour, from_svc, to_svc) VALUES " + strings.TrimSuffix(strings.Repeat("(?, ?, ?), ", n), ", ") +
			" ON CONFLICT (hour, from_svc, to_svc) DO NOTHING"
		_, err := t.ExecContext(t.ctx, q, args...)
		args = args[:0]
		return err
	}
	var failed error
	e.Graph.Each(func(hour time.Time, from, to string) {
		if failed != nil {
			return
		}
		args = append(args, hour.Unix(), from, to)
		if len(args) >= edgesPerInsert*3 {
			failed = flush()
		}
	})
	if failed == nil {
		failed = flush()
	}
	if failed != nil {
		return failed
	}
	// The hour the read ended in, which the read only began, isn't known
	// whole: the span ends where it starts, and the next run, which reads
	// from an hour before its own start, reads it again.
	from, to := graph.HourStart(graph.Hour(e.From)).Unix(), graph.HourStart(graph.Hour(e.To)).Unix()
	if to <= from {
		return nil
	}
	rows, err := t.QueryContext(t.ctx, "SELECT from_at, to_at FROM edge_spans WHERE to_at >= ? AND from_at <= ?", from, to)
	if err != nil {
		return err
	}
	var olds []int64
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			rows.Close()
			return err
		}
		olds = append(olds, a)
		from, to = min(from, a), max(to, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range olds {
		if _, err := t.ExecContext(t.ctx, "DELETE FROM edge_spans WHERE from_at = ?", a); err != nil {
			return err
		}
	}
	// What failed to be read inside it is read, and no gap.
	if _, err := t.ExecContext(t.ctx, "DELETE FROM edge_retries WHERE gap = 1 AND from_at >= ? AND to_at <= ?", from, to); err != nil {
		return err
	}
	_, err = t.ExecContext(t.ctx, "INSERT INTO edge_spans (from_at, to_at) VALUES (?, ?)", from, to)
	return err
}

// edgesPerInsert is how many edges a statement inserts: 3,000 parameters,
// well under what SQLite and Postgres take.
const edgesPerInsert = 1000

// EdgeFailed keeps a span a run could not read the graph of, for the next
// run to read once (EdgeRetries, EdgeRetried, EdgeGaveUp). One that overlaps
// or touches another, a gap too, is made one with it, to be read once.
func (l *Ledger) EdgeFailed(ctx context.Context, from, to time.Time) error {
	return l.write(ctx, func(t *txn) error {
		a, b := from.Unix(), to.Unix()
		rows, err := t.QueryContext(t.ctx, "SELECT from_at, to_at FROM edge_retries WHERE to_at >= ? AND from_at <= ?", a, b)
		if err != nil {
			return err
		}
		var olds []int64
		for rows.Next() {
			var x, y int64
			if err := rows.Scan(&x, &y); err != nil {
				rows.Close()
				return err
			}
			olds = append(olds, x)
			a, b = min(a, x), max(b, y)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, x := range olds {
			if _, err := t.ExecContext(t.ctx, "DELETE FROM edge_retries WHERE from_at = ?", x); err != nil {
				return err
			}
		}
		_, err = t.ExecContext(t.ctx, "INSERT INTO edge_retries (from_at, to_at) VALUES (?, ?)", a, b)
		return err
	})
}

// EdgeRetries are the spans a run could not read the graph of, the
// earliest first.
func (l *Ledger) EdgeRetries(ctx context.Context) ([][2]time.Time, error) {
	return l.edgeSpans(ctx, 0)
}

// EdgeGaps are the spans the graph could not be read of twice, known to be
// missing from it: no run reads them again, and a read over them (a change
// of the expression, or a longer run) takes them away.
func (l *Ledger) EdgeGaps(ctx context.Context) ([][2]time.Time, error) {
	return l.edgeSpans(ctx, 1)
}

// EdgeGaveUp keeps the span that starts at from as a known gap, as it failed
// to be read again: it is not retried.
func (l *Ledger) EdgeGaveUp(ctx context.Context, from time.Time) error {
	return l.write(ctx, func(t *txn) error {
		_, err := t.ExecContext(t.ctx, "UPDATE edge_retries SET gap = 1 WHERE from_at = ?", from.Unix())
		return err
	})
}

func (l *Ledger) edgeSpans(ctx context.Context, gap int) ([][2]time.Time, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT from_at, to_at FROM edge_retries WHERE gap = ? ORDER BY from_at", gap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]time.Time
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out = append(out, [2]time.Time{unix(a), unix(b)})
	}
	return out, rows.Err()
}

// EdgeRetried drops the span that starts at from: it was read again, or
// tried, and isn't again.
func (l *Ledger) EdgeRetried(ctx context.Context, from time.Time) error {
	return l.write(ctx, func(t *txn) error {
		_, err := t.ExecContext(t.ctx, "DELETE FROM edge_retries WHERE from_at = ?", from.Unix())
		return err
	})
}

// withGraph gives the options the graph the ledger holds for the incidents
// to be judged against (verdict.Options.Depends), when they have a
// dependency to judge by and no graph yet: the edges of each incident's
// window, the hours from one before its start to its end, the windows apart
// and the hours between them not loaded, maxJudgeEdges at most in all; past
// it the options are given a graph with none, as a graph read past
// graph.MaxEdgesPerHour is. A caller that gave the graph (a try) keeps it.
func (l *Ledger) withGraph(ctx context.Context, q queryer, o verdict.Options, incs []incident.Incident) (verdict.Options, error) {
	o, _, err := l.graphFor(ctx, q, o, incs)
	return o, err
}

// graphFor is withGraph, saying whether the graph was too big to load.
func (l *Ledger) graphFor(ctx context.Context, q queryer, o verdict.Options, incs []incident.Incident) (verdict.Options, bool, error) {
	if o.Depends == nil || o.Depends.Graph != nil || len(incs) == 0 {
		return o, false, nil
	}
	// An hour before each start, and the hour each end is in; the windows
	// that overlap or touch are one.
	ws := make([][2]int64, 0, len(incs))
	for _, in := range incs {
		a := graph.HourStart(graph.Hour(in.Start.Add(-time.Hour))).Unix()
		b := graph.HourStart(graph.Hour(in.EndOr(o.UnknownLen))).Unix()
		ws = append(ws, [2]int64{a, max(a, b)})
	}
	slices.SortFunc(ws, func(x, y [2]int64) int { return int(x[0] - y[0]) })
	merged := ws[:1]
	for _, w := range ws[1:] {
		if last := &merged[len(merged)-1]; w[0] <= last[1]+3600 {
			last[1] = max(last[1], w[1])
		} else {
			merged = append(merged, w)
		}
	}
	g := graph.NewBuilder()
	n, over := 0, false
	for _, w := range merged {
		rows, err := q.QueryContext(ctx, "SELECT hour, from_svc, to_svc FROM edges WHERE hour >= ? AND hour <= ? ORDER BY hour LIMIT ?", w[0], w[1], maxJudgeEdges-n+1)
		if err != nil {
			return o, false, err
		}
		for rows.Next() {
			var h int64
			var from, to string
			if err := rows.Scan(&h, &from, &to); err != nil {
				rows.Close()
				return o, false, err
			}
			if n++; n > maxJudgeEdges {
				over = true
				break
			}
			g.Add(unix(h), from, to)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return o, false, err
		}
		if over {
			g = graph.NewBuilder()
			g.Over()
			break
		}
	}
	d := *o.Depends
	d.Graph = g.Graph()
	o.Depends = &d
	return o, over, nil
}

// metaEdgesOver is the meta key of the span a judgement was made with no
// graph over, for there were more edges in its incidents' windows than
// maxJudgeEdges: "FROM,TO", in seconds. A judgement that covers the span and
// loads its graph takes it away.
const metaEdgesOver = "edges-over"

// noteOver keeps, or takes away, what a judgement of [lo, hi] says of the
// graph it was too big to load.
func (l *Ledger) noteOver(t *txn, lo, hi int64, over bool) error {
	cur, err := l.meta(t.ctx, t, metaEdgesOver)
	if err != nil {
		return err
	}
	a, b, ok := spanOf(cur)
	switch {
	case over:
		if ok {
			lo, hi = min(lo, a), max(hi, b)
		}
		return setMeta(t.ctx, t, metaEdgesOver, strconv.FormatInt(lo, 10)+","+strconv.FormatInt(hi, 10))
	case ok && lo <= a && hi >= b:
		return setMeta(t.ctx, t, metaEdgesOver, "")
	}
	return nil
}

func spanOf(v string) (a, b int64, ok bool) {
	x, y, ok := strings.Cut(v, ",")
	if !ok {
		return 0, 0, false
	}
	a, err1 := strconv.ParseInt(x, 10, 64)
	b, err2 := strconv.ParseInt(y, 10, 64)
	return a, b, err1 == nil && err2 == nil
}

// EdgesOver says whether a judgement over firings that started in [from,
// to) was made with no graph, for its incidents' windows had more edges
// than are loaded (MaxJudgeEdges): the report says the graph was too big,
// and the pages say so. It holds until a judgement covering the span loads
// the graph, when the edges are fewer or the incidents are.
func (l *Ledger) EdgesOver(ctx context.Context, from, to time.Time) (bool, error) {
	v, err := l.meta(ctx, l.r, metaEdgesOver)
	if err != nil {
		return false, err
	}
	a, b, ok := spanOf(v)
	return ok && a < to.Unix() && b >= from.Unix(), nil
}

// EdgesFit says whether a graph read fits what the ledger keeps (MaxEdgesPerDay,
// MaxEdgesKept): nil when it does, else ErrEdgesPerDay or ErrEdgesKept. With
// replacing, the edges the ledger holds are to go (ReplaceEdges) and aren't
// counted.
func (l *Ledger) EdgesFit(ctx context.Context, e *EdgesRead, replacing bool) error {
	if e == nil || e.Graph == nil || e.Graph.TooMany() {
		return nil
	}
	days := map[int64]int{}
	e.Graph.Each(func(hour time.Time, _, _ string) { days[hour.Unix()/86400]++ })
	for _, n := range days {
		if n > maxEdgesPerDay {
			return ErrEdgesPerDay
		}
	}
	if replacing {
		return nil
	}
	have, _, err := l.EdgesKept(ctx)
	if err != nil {
		return err
	}
	if have+int64(e.Graph.Rows()) > int64(maxEdgesKept) {
		return ErrEdgesKept
	}
	return nil
}

// EdgesStatus is what the leader says of reading the graph, kept for the
// other replicas to show: the last read that failed, why, and when a read
// over the last four weeks is tried again (Next, an hour after the first
// failure, then two, up to a day). Key is the graph it is of.
type EdgesStatus struct {
	Key   string    `json:"key,omitempty"`
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at,omitzero"`
	Tries int       `json:"tries,omitempty"`
	Next  time.Time `json:"next,omitzero"`
}

const metaEdgesStatus = "edges-status"

// EdgesStatus is what was kept of how reading the graph went; zero when it
// went well.
func (l *Ledger) EdgesStatus(ctx context.Context) (EdgesStatus, error) {
	var st EdgesStatus
	v, err := l.meta(ctx, l.r, metaEdgesStatus)
	if err != nil || v == "" {
		return st, err
	}
	if json.Unmarshal([]byte(v), &st) != nil {
		return EdgesStatus{}, nil
	}
	return st, nil
}

// SetEdgesStatus keeps how reading the graph went; a zero status, that it
// went well.
func (l *Ledger) SetEdgesStatus(ctx context.Context, st EdgesStatus) error {
	v := ""
	if st != (EdgesStatus{}) {
		b, _ := json.Marshal(st)
		v = string(b)
	}
	if cur, err := l.meta(ctx, l.r, metaEdgesStatus); err == nil && cur == v {
		return nil
	}
	return l.write(ctx, func(t *txn) error { return setMeta(t.ctx, t, metaEdgesStatus, v) })
}

// dependencyText is a catch's path as the ledger keeps it
// (catches.dependency): a JSON list, ” for none.
func dependencyText(path []string) string {
	if len(path) == 0 {
		return ""
	}
	b, _ := json.Marshal(path)
	return string(b)
}

// dependencyOf is a path as dependencyText keeps it.
func dependencyOf(text string) []string {
	if text == "" {
		return nil
	}
	var path []string
	if json.Unmarshal([]byte(text), &path) != nil {
		return nil
	}
	return path
}

// WithGraph is the options with the graph the ledger holds for the
// incidents (withGraph), for a caller that judges by verdict.Judge.
func (l *Ledger) WithGraph(ctx context.Context, o verdict.Options, incs []incident.Incident) (verdict.Options, error) {
	return l.withGraph(ctx, l.r, o, incs)
}

// EdgesIn is how many pairs of services held an edge in any hour of [from,
// to], an hour before from included, as a report says how big the graph it
// was judged by was (verdict.DependsSettings.Edges). Counted once for each
// generation of the ledger and span, as a report asks it every time.
func (l *Ledger) EdgesIn(ctx context.Context, from, to time.Time) (int, error) {
	a, b := graph.HourStart(graph.Hour(from.Add(-time.Hour))).Unix(), graph.HourStart(graph.Hour(to)).Unix()
	gen := l.Gen()
	l.edgesIn.Lock()
	if l.edgesIn.gen == gen {
		if n, ok := l.edgesIn.by[[2]int64{a, b}]; ok {
			l.edgesIn.Unlock()
			return n, nil
		}
	}
	l.edgesIn.Unlock()
	var n int
	err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT DISTINCT from_svc, to_svc FROM edges WHERE hour >= ? AND hour <= ?) AS pairs", a, b).Scan(&n)
	if err != nil {
		return 0, err
	}
	l.edgesIn.Lock()
	if l.edgesIn.gen != gen || l.edgesIn.by == nil || len(l.edgesIn.by) >= 64 {
		l.edgesIn.gen, l.edgesIn.by = gen, map[[2]int64]int{}
	}
	l.edgesIn.by[[2]int64{a, b}] = n
	l.edgesIn.Unlock()
	return n, nil
}
