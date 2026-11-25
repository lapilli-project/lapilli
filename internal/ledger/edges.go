package ledger

import (
	"context"
	"encoding/json"
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
// hours its incidents' windows span (an hour before each one's start to its
// end). Past it the judgement is made with no graph, as a graph read past
// graph.MaxEdgesPerHour is none: no catch by dependency, rather than one by
// part of a graph. A week of the most an hour may have is 3.36 million; this
// is the memory a judgement may hold, about 50 MB.
const MaxJudgeEdges = 1_000_000

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
		if err := l.keepEdges(t, e); err != nil {
			return err
		}
		return setMeta(t.ctx, t, metaEdgesKey, key)
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
	ins, err := t.PrepareContext(t.ctx, "INSERT INTO edges (hour, from_svc, to_svc) VALUES (?, ?, ?) ON CONFLICT (hour, from_svc, to_svc) DO NOTHING")
	if err != nil {
		return err
	}
	defer ins.Close()
	var failed error
	e.Graph.Each(func(hour time.Time, from, to string) {
		if failed == nil {
			_, failed = ins.ExecContext(t.ctx, hour.Unix(), from, to)
		}
	})
	if failed != nil {
		return failed
	}
	from, to := graph.HourStart(graph.Hour(e.From)).Unix(), e.To.Unix()
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
	_, err = t.ExecContext(t.ctx, "INSERT INTO edge_spans (from_at, to_at) VALUES (?, ?)", from, to)
	return err
}

// EdgeFailed keeps a span a run could not read the graph of, for the next
// run to read once (EdgeRetries, EdgeRetried). One that overlaps or touches
// another is made one with it.
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
	rows, err := l.r.QueryContext(ctx, "SELECT from_at, to_at FROM edge_retries ORDER BY from_at")
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
// dependency to judge by and no graph yet: the edges of the hours from one
// before the earliest start to the latest end, MaxJudgeEdges at most; past
// it the options are given a graph with none, as a graph read past
// graph.MaxEdgesPerHour is. A caller that gave the graph (a try) keeps it.
func (l *Ledger) withGraph(ctx context.Context, q queryer, o verdict.Options, incs []incident.Incident) (verdict.Options, error) {
	if o.Depends == nil || o.Depends.Graph != nil || len(incs) == 0 {
		return o, nil
	}
	lo, hi := incs[0].Start, incs[0].EndOr(o.UnknownLen)
	for _, in := range incs[1:] {
		if in.Start.Before(lo) {
			lo = in.Start
		}
		if e := in.EndOr(o.UnknownLen); e.After(hi) {
			hi = e
		}
	}
	// An hour before the earliest start, and the hour the latest end is in.
	a, b := graph.HourStart(graph.Hour(lo.Add(-time.Hour))).Unix(), graph.HourStart(graph.Hour(hi)).Unix()
	rows, err := q.QueryContext(ctx, "SELECT hour, from_svc, to_svc FROM edges WHERE hour >= ? AND hour <= ? ORDER BY hour LIMIT ?", a, b, MaxJudgeEdges+1)
	if err != nil {
		return o, err
	}
	defer rows.Close()
	g := graph.NewBuilder()
	n := 0
	for rows.Next() {
		var h int64
		var from, to string
		if err := rows.Scan(&h, &from, &to); err != nil {
			return o, err
		}
		if n++; n > MaxJudgeEdges {
			g = graph.NewBuilder()
			g.Over()
			break
		}
		g.Add(unix(h), from, to)
	}
	if err := rows.Err(); err != nil {
		return o, err
	}
	d := *o.Depends
	d.Graph = g.Graph()
	o.Depends = &d
	return o, nil
}

// dependencyText is a catch's path as the ledger keeps it
// (catches.dependency): a JSON list, '' for none.
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

