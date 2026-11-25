package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/response"
	"lapilli.dev/internal/verdict"
)

// What people did when a rule fired (docs/design/responses.md), as the
// ledger keeps it (schema 034): the pages a paging tool sent, the changes
// each run read from kube-state-metrics for the hours it judged, and from
// them, with the deploys and the people's words the ledger keeps already,
// each firing's response and whether it was useful, worked out as stones
// are (respond), so that judging the ledger again asks nothing outside it.

// ResponsesFile is the source of the pages --responses reads.
const ResponsesFile = "file"

// ResponseReach is how long before a page was sent the firing it answers
// may have started: a page is tied to a firing that was on when it was
// sent, and one lasts days at most before its page. A page written or
// changed has the firings that started this long before it judged again.
const ResponseReach = 14 * 24 * time.Hour

// PutResponsesResult is what a write of pages changed.
type PutResponsesResult struct {
	// Pages are those the write held, and Added, Changed and Removed how
	// many were made, differed and taken away.
	Pages   int `json:"pages"`
	Added   int `json:"added"`
	Changed int `json:"changed"`
	Removed int `json:"removed"`
}

// Differs reports whether the write changed the ledger.
func (r PutResponsesResult) Differs() bool { return r.Added+r.Changed+r.Removed > 0 }

type pageRow struct {
	sent, ack, res int64
	resBy, alerts  string
}

func pageRowOf(p response.Page) pageRow {
	return pageRow{sent: p.Sent.Unix(), ack: unixOr0(p.Acknowledged), res: unixOr0(p.Resolved), resBy: p.ResolvedBy, alerts: alertsText(p.Alerts)}
}

func pageOf(id string, r pageRow) (response.Page, error) {
	p := response.Page{ID: id, Sent: unix(r.sent), ResolvedBy: r.resBy}
	if r.ack != 0 {
		p.Acknowledged = unix(r.ack)
	}
	if r.res != 0 {
		p.Resolved = unix(r.res)
	}
	var err error
	p.Alerts, err = alertsOf(r.alerts)
	return p, err
}

// SyncResponses makes the pages of source those given, as the file owns
// them: any it doesn't hold go. sum is a digest of what they were read from,
// kept to know an unchanged file again (ResponsesUnchanged).
func (l *Ledger) SyncResponses(ctx context.Context, source, sum string, pages []response.Page, o verdict.Options) (PutResponsesResult, error) {
	return l.putResponses(ctx, source, sum, nil, pages, o)
}

// PutResponses makes the pages of source sent in [from, to) those given,
// as PUT /api/v1/responses/SOURCE does; the pages it has from other times
// stay. A page given must have been sent in the period.
func (l *Ledger) PutResponses(ctx context.Context, source string, from, to time.Time, pages []response.Page, o verdict.Options) (PutResponsesResult, error) {
	for _, p := range pages {
		if p.Sent.Before(from) || !p.Sent.Before(to) {
			return PutResponsesResult{}, invalid("page %s was sent %s, outside the period from %s to %s: send the pages sent in it", p.ID,
				p.Sent.UTC().Format(time.RFC3339), from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
		}
	}
	return l.putResponses(ctx, source, "", &[2]int64{from.Unix(), to.Unix()}, pages, o)
}

func (l *Ledger) putResponses(ctx context.Context, source, sum string, span *[2]int64, pages []response.Page, o verdict.Options) (PutResponsesResult, error) {
	res := PutResponsesResult{Pages: len(pages)}
	want := make(map[string]pageRow, len(pages))
	for _, p := range pages {
		want[p.ID] = pageRowOf(p)
	}
	var spans [][2]int64
	err := l.write(ctx, func(t *txn) error {
		res = PutResponsesResult{Pages: len(pages)}
		spans = spans[:0]
		reach := int64(ResponseReach / time.Second)
		mark := func(sent int64) { spans = append(spans, [2]int64{sent - reach, sent}) }
		q := "SELECT id, sent_at, acknowledged_at, resolved_at, resolved_by, alerts FROM responses WHERE source = ?"
		args := []any{source}
		if span != nil {
			q += " AND sent_at >= ? AND sent_at < ?"
			args = append(args, span[0], span[1])
		}
		rows, err := t.QueryContext(t.ctx, q, args...)
		if err != nil {
			return err
		}
		have := map[string]pageRow{}
		for rows.Next() {
			var id string
			var r pageRow
			var ack, rs sql.NullInt64
			if err := rows.Scan(&id, &r.sent, &ack, &rs, &r.resBy, &r.alerts); err != nil {
				rows.Close()
				return err
			}
			r.ack, r.res = ack.Int64, rs.Int64
			have[id] = r
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// A page given again by an id the source holds from another time
		// is moved: its old time judged again too.
		if span != nil && len(want) > 0 {
			ids := slices.Sorted(maps.Keys(want))
			for len(ids) > 0 {
				part := ids[:min(len(ids), pgRows)]
				ids = ids[len(part):]
				b, _ := json.Marshal(part)
				rows, err := t.QueryContext(t.ctx, "SELECT id, sent_at, acknowledged_at, resolved_at, resolved_by, alerts FROM responses WHERE source = ? AND id IN ("+t.d.texts("?")+")", source, string(b))
				if err != nil {
					return err
				}
				for rows.Next() {
					var id string
					var r pageRow
					var ack, rs sql.NullInt64
					if err := rows.Scan(&id, &r.sent, &ack, &rs, &r.resBy, &r.alerts); err != nil {
						rows.Close()
						return err
					}
					r.ack, r.res = ack.Int64, rs.Int64
					have[id] = r
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					return err
				}
			}
		}
		del, err := t.PrepareContext(t.ctx, "DELETE FROM responses WHERE source = ? AND id = ?")
		if err != nil {
			return err
		}
		defer del.Close()
		for _, id := range slices.Sorted(maps.Keys(have)) {
			if _, ok := want[id]; ok {
				continue
			}
			if _, err := del.ExecContext(t.ctx, source, id); err != nil {
				return err
			}
			mark(have[id].sent)
			res.Removed++
		}
		up, err := t.PrepareContext(t.ctx, `INSERT INTO responses (source, id, sent_at, acknowledged_at, resolved_at, resolved_by, alerts) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (source, id) DO UPDATE SET sent_at = excluded.sent_at, acknowledged_at = excluded.acknowledged_at, resolved_at = excluded.resolved_at,
			resolved_by = excluded.resolved_by, alerts = excluded.alerts`)
		if err != nil {
			return err
		}
		defer up.Close()
		for _, id := range slices.Sorted(maps.Keys(want)) {
			w := want[id]
			h, ok := have[id]
			if ok && h == w {
				continue
			}
			var ack, rs sql.NullInt64
			if w.ack != 0 {
				ack = sql.NullInt64{Int64: w.ack, Valid: true}
			}
			if w.res != 0 {
				rs = sql.NullInt64{Int64: w.res, Valid: true}
			}
			if _, err := up.ExecContext(t.ctx, source, id, w.sent, ack, rs, w.resBy, w.alerts); err != nil {
				return err
			}
			mark(w.sent)
			if ok {
				mark(h.sent)
				res.Changed++
			} else {
				res.Added++
			}
		}
		if sum != "" {
			if err := setMeta(t.ctx, t, "responses-read."+source, sum); err != nil {
				return err
			}
		}
		if !res.Differs() {
			return nil
		}
		return pend(t, near(slices.Clone(spans)))
	})
	if err != nil {
		return res, err
	}
	if res.Differs() {
		l.judgeSoon(ctx, o)
	}
	return res, nil
}

// ResponsesUnchanged reports whether a source's pages were last read from
// what has the digest sum: they needn't be read again.
func (l *Ledger) ResponsesUnchanged(ctx context.Context, source, sum string) (bool, error) {
	if sum == "" {
		return false, nil
	}
	was, err := l.meta(ctx, l.r, "responses-read."+source)
	return was == sum, err
}

// ResponsePages are the pages sent in [from, to), the earliest first, with
// the source each came from.
func (l *Ledger) ResponsePages(ctx context.Context, from, to time.Time) ([]response.Page, error) {
	return l.pagesBetween(ctx, l.r, from.Unix(), to.Unix()-1)
}

// pagesBetween are the pages sent in [lo, hi], in order of their sent time
// then their id.
func (l *Ledger) pagesBetween(ctx context.Context, q queryer, lo, hi int64) ([]response.Page, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, sent_at, acknowledged_at, resolved_at, resolved_by, alerts FROM responses
		WHERE sent_at >= ? AND sent_at <= ? ORDER BY sent_at, id, source`, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []response.Page
	for rows.Next() {
		var id string
		var r pageRow
		var ack, rs sql.NullInt64
		if err := rows.Scan(&id, &r.sent, &ack, &rs, &r.resBy, &r.alerts); err != nil {
			return nil, err
		}
		r.ack, r.res = ack.Int64, rs.Int64
		p, err := pageOf(id, r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ResponseInputs says what the ledger keeps to find responses from: pages,
// changes read from the history, deploys.
type ResponseInputs struct {
	Pages, Rollouts, Deploys bool
}

// Any reports whether the ledger keeps any of them.
func (r ResponseInputs) Any() bool { return r.Pages || r.Rollouts || r.Deploys }

// ResponseInputs is what the ledger keeps responses to be found from.
func (l *Ledger) ResponseInputs(ctx context.Context) (ResponseInputs, error) {
	return l.inputs(ctx, l.r)
}

func (l *Ledger) inputs(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (ResponseInputs, error) {
	var in ResponseInputs
	for _, x := range []struct {
		table string
		to    *bool
	}{{"responses", &in.Pages}, {"rollout_spans", &in.Rollouts}, {"deploys", &in.Deploys}} {
		var n int
		if err := q.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM "+x.table+" LIMIT 1) AS one").Scan(&n); err != nil {
			return in, err
		}
		*x.to = n > 0
	}
	return in, nil
}

// KeepRollouts keeps what a run read of the changes made to workloads
// (response.ReadRollouts), outside a run: for tests and callers that don't
// record one.
func (l *Ledger) KeepRollouts(ctx context.Context, r *response.Rollouts, seen time.Time, o verdict.Options) error {
	err := l.write(ctx, func(t *txn) error { return l.keepRollouts(t, r, seen.Unix(), seen.Unix()) })
	if err == nil {
		l.judgeSoon(ctx, o)
	}
	return err
}

// keepRollouts keeps the changes, the spans each workload's are known over
// and the workloads the pods own, read at seen. A change new to the ledger
// puts the firings that started before lo, and were on when it was made,
// back to be judged again: those a run's own judging doesn't reach.
func (l *Ledger) keepRollouts(t *txn, r *response.Rollouts, seen, lo int64) error {
	if r == nil {
		return nil
	}
	ins, err := t.PrepareContext(t.ctx, `INSERT INTO rollouts (kind, at, namespace, workload_kind, workload, revision) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (kind, at, namespace, workload_kind, workload) DO NOTHING`)
	if err != nil {
		return err
	}
	defer ins.Close()
	earliest := int64(-1)
	for _, c := range r.Changes {
		res, err := ins.ExecContext(t.ctx, string(c.Kind), c.At.Unix(), c.Namespace, c.WorkloadKind, c.Workload, c.Revision)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 && (earliest < 0 || c.At.Unix() < earliest) {
			earliest = c.At.Unix()
		}
	}
	// A workload's spans are kept apart and whole: one that overlaps or
	// touches another is one.
	ws := make([]response.Workload, 0, len(r.Coverage))
	for w := range r.Coverage {
		ws = append(ws, w)
	}
	slices.SortFunc(ws, func(a, b response.Workload) int {
		return strings.Compare(a.Namespace+"\x00"+a.Kind+"\x00"+a.Name, b.Namespace+"\x00"+b.Kind+"\x00"+b.Name)
	})
	find, err := t.PrepareContext(t.ctx, "SELECT from_at, to_at FROM rollout_spans WHERE namespace = ? AND workload_kind = ? AND workload = ? AND to_at >= ? AND from_at <= ?")
	if err != nil {
		return err
	}
	defer find.Close()
	drop, err := t.PrepareContext(t.ctx, "DELETE FROM rollout_spans WHERE namespace = ? AND workload_kind = ? AND workload = ? AND from_at = ?")
	if err != nil {
		return err
	}
	defer drop.Close()
	put, err := t.PrepareContext(t.ctx, "INSERT INTO rollout_spans (namespace, workload_kind, workload, from_at, to_at) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer put.Close()
	for _, w := range ws {
		for _, s := range r.Coverage[w] {
			from, to := s.From.Unix(), s.To.Unix()
			if s.To.IsZero() {
				to = seen
			}
			rows, err := find.QueryContext(t.ctx, w.Namespace, w.Kind, w.Name, from, to)
			if err != nil {
				return err
			}
			var olds [][2]int64
			for rows.Next() {
				var a, b int64
				if err := rows.Scan(&a, &b); err != nil {
					rows.Close()
					return err
				}
				olds = append(olds, [2]int64{a, b})
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if len(olds) == 1 && olds[0][0] <= from && olds[0][1] >= to {
				continue
			}
			for _, o := range olds {
				from, to = min(from, o[0]), max(to, o[1])
				if _, err := drop.ExecContext(t.ctx, w.Namespace, w.Kind, w.Name, o[0]); err != nil {
					return err
				}
			}
			if _, err := put.ExecContext(t.ctx, w.Namespace, w.Kind, w.Name, from, to); err != nil {
				return err
			}
		}
	}
	own, err := t.PrepareContext(t.ctx, `INSERT INTO rollout_owners (namespace, pod, workload_kind, workload, seen_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (namespace, pod) DO UPDATE SET workload_kind = excluded.workload_kind, workload = excluded.workload, seen_at = excluded.seen_at`)
	if err != nil {
		return err
	}
	defer own.Close()
	pods := make([]response.Pod, 0, len(r.Owners))
	for p := range r.Owners {
		pods = append(pods, p)
	}
	slices.SortFunc(pods, func(a, b response.Pod) int {
		return strings.Compare(a.Namespace+"\x00"+a.Name, b.Namespace+"\x00"+b.Name)
	})
	for _, p := range pods {
		w := r.Owners[p]
		if _, err := own.ExecContext(t.ctx, p.Namespace, p.Name, w.Kind, w.Name, seen); err != nil {
			return err
		}
	}
	if r.AutoscalersKnown {
		if err := setMeta(t.ctx, t, "rollouts-autoscalers", "1"); err != nil {
			return err
		}
	}
	if earliest < 0 {
		return nil
	}
	var first sql.NullInt64
	if err := t.QueryRowContext(t.ctx, "SELECT min(start_at) FROM firings WHERE start_at >= ? AND start_at < ? AND (end_at IS NULL OR end_at >= ?)",
		earliest-int64(ResponseReach/time.Second), lo, earliest).Scan(&first); err != nil {
		return err
	}
	if first.Valid {
		return pend(t, [][2]int64{{first.Int64, lo - 1}})
	}
	return nil
}

// respFiring is a firing a response is found for.
type respFiring struct {
	ID      int64
	Rule    string
	Labels  map[string]string
	Start   time.Time
	End     time.Time
	White   bool
	Person  int64
	Excused bool
}

// responder finds the responses of a batch of firings from what the ledger
// keeps. Each firing's changes are counted over its week (the ledger's
// weeks), so that a firing's response doesn't depend on which others are
// judged with it.
type responder struct {
	l      *Ledger
	ctx    context.Context
	q      queryer
	o      verdict.Options
	now    time.Time
	pages  *response.Index
	in     ResponseInputs
	hpa    bool
	owners map[response.Pod]response.Workload
	covers []struct {
		labels map[string]string
		from   time.Time
	}
	last  map[int64]time.Time // by week: the latest a firing of it ends, or now
	weeks map[int64]*response.ChangeSet
	rx    interface {
		QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	}
}

func (r *responder) within(rule string) time.Duration {
	if r.o.Within != nil {
		return r.o.Within(rule)
	}
	return 2 * max(r.o.ResponseStep, time.Minute)
}

// newResponder reads, for the firings fs, the pages that may answer them and
// the workloads their pods belong to; the changes of each week are read when
// a firing of it is asked about.
func (l *Ledger) newResponder(ctx context.Context, q queryer, rx interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, o verdict.Options, fs []respFiring, now time.Time) (*responder, error) {
	r := &responder{l: l, ctx: ctx, q: q, rx: rx, o: o, now: now, last: map[int64]time.Time{}, weeks: map[int64]*response.ChangeSet{},
		owners: map[response.Pod]response.Workload{}}
	var err error
	if r.in, err = l.inputs(ctx, rx); err != nil {
		return nil, err
	}
	if len(fs) == 0 || !r.in.Any() {
		return r, nil
	}
	lo, hi := fs[0].Start, time.Time{}
	var maxWithin time.Duration
	pods := map[string]bool{}
	for _, f := range fs {
		if f.Start.Before(lo) {
			lo = f.Start
		}
		end := f.End
		if end.IsZero() {
			end = now
		}
		if end.After(hi) {
			hi = end
		}
		maxWithin = max(maxWithin, r.within(f.Rule))
		w := Week(l.day(f.Start.Unix()))
		if end.After(r.last[w]) {
			r.last[w] = end
		}
		if p := f.Labels["pod"]; p != "" && f.Labels["namespace"] != "" {
			pods[p] = true
		}
	}
	if r.in.Pages {
		pages, err := l.pagesBetween(ctx, q, lo.Unix(), hi.Add(maxWithin).Unix())
		if err != nil {
			return nil, err
		}
		r.pages = response.NewIndex(pages)
	}
	if r.in.Rollouts && len(pods) > 0 {
		names := slices.Sorted(maps.Keys(pods))
		for len(names) > 0 {
			part := names[:min(len(names), pgRows)]
			names = names[len(part):]
			b, _ := json.Marshal(part)
			rows, err := q.QueryContext(ctx, "SELECT namespace, pod, workload_kind, workload FROM rollout_owners WHERE pod IN ("+l.d.texts("?")+")", string(b))
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var ns, pod, kind, name string
				if err := rows.Scan(&ns, &pod, &kind, &name); err != nil {
					rows.Close()
					return nil, err
				}
				r.owners[response.Pod{Namespace: ns, Name: pod}] = response.Workload{Namespace: ns, Kind: kind, Name: name}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
	}
	if r.in.Deploys {
		rows, err := q.QueryContext(ctx, "SELECT ls.labels, min(d.start_at) FROM deploys d JOIN deploy_labels ls ON ls.id = d.labels_id GROUP BY ls.labels")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var text string
			var from int64
			if err := rows.Scan(&text, &from); err != nil {
				rows.Close()
				return nil, err
			}
			var m map[string]string
			if err := json.Unmarshal([]byte(text), &m); err != nil {
				rows.Close()
				return nil, err
			}
			if len(m) == 0 {
				m = nil
			}
			r.covers = append(r.covers, struct {
				labels map[string]string
				from   time.Time
			}{m, unix(from)})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if r.in.Rollouts {
		var v string
		if err := rx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'rollouts-autoscalers'").Scan(&v); err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		r.hpa = v == "1"
	}
	return r, nil
}

// changes are the changes of week w, read from its Monday to the latest a
// firing of it ends; counted over the week.
func (r *responder) changes(w int64) (*response.ChangeSet, error) {
	if cs, ok := r.weeks[w]; ok {
		return cs, nil
	}
	from, to := unix(r.l.midnight(w)), unix(r.l.midnight(w+7))
	cs := response.NewChangeSet(from, to)
	r.weeks[w] = cs
	until := max(to.Unix(), r.last[w].Unix())
	if r.in.Rollouts {
		rs := &response.Rollouts{Coverage: map[response.Workload][]response.Span{}, Owners: r.owners, AutoscalersKnown: r.hpa}
		rows, err := r.q.QueryContext(r.ctx, "SELECT kind, at, namespace, workload_kind, workload, revision FROM rollouts WHERE at >= ? AND at <= ? ORDER BY at", from.Unix(), until)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c response.Change
			var kind string
			var at int64
			if err := rows.Scan(&kind, &at, &c.Namespace, &c.WorkloadKind, &c.Workload, &c.Revision); err != nil {
				rows.Close()
				return nil, err
			}
			c.Kind, c.At = response.Kind(kind), unix(at)
			rs.Changes = append(rs.Changes, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows, err = r.q.QueryContext(r.ctx, "SELECT namespace, workload_kind, workload, from_at, to_at FROM rollout_spans WHERE to_at >= ? AND from_at <= ?", from.Unix(), until)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var w response.Workload
			var a, b int64
			if err := rows.Scan(&w.Namespace, &w.Kind, &w.Name, &a, &b); err != nil {
				rows.Close()
				return nil, err
			}
			rs.Coverage[w] = append(rs.Coverage[w], response.Span{From: unix(a), To: unix(b)})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		cs.AddRollouts(rs)
	}
	if r.in.Deploys {
		ds, err := r.l.deploys(r.ctx, r.q, "WHERE d.start_at >= ? AND d.start_at <= ?", from.Unix(), until)
		if err != nil {
			return nil, err
		}
		cs.AddDeploys(ds)
		for _, c := range r.covers {
			cs.AddDeployCoverage(c.labels, c.from)
		}
	}
	return cs, nil
}

// respond is a firing's response and whether it was useful, apart from the
// record that judges the firings that caught nothing: usefulness doesn't
// count that (Useful with judged false), which the tallies' readers do for
// the weeks whose record is kept.
func (r *responder) respond(f respFiring) (response.Response, response.Usefulness, error) {
	var answer response.Answer
	switch {
	case f.Person&personYes != 0:
		answer = response.AnswerYes
	case f.Person&personNo != 0:
		answer = response.AnswerNo
	}
	ev := response.Evidence{Pages: r.pages, Match: r.o.RolloutMatch, Routine: r.o.Routine}
	if ev.Routine == 0 {
		ev.Routine = response.DefaultRoutine
	}
	if r.in.Rollouts || r.in.Deploys {
		cs, err := r.changes(Week(r.l.day(f.Start.Unix())))
		if err != nil {
			return response.Response{}, 0, err
		}
		ev.Changes = cs
	}
	resp := ev.Respond(response.Firing{Rule: f.Rule, Labels: f.Labels, Start: f.Start, End: f.End, On: f.End.IsZero()}, r.within(f.Rule), r.now)
	return resp, response.Useful(f.White, false, answer, resp, resp.Known), nil
}

// The numbers firings.response keeps are response.Type's.

// respond works out, for the firings that started in [lo, hi], what was done
// about each and whether it was useful, and keeps it (firings.response,
// routine, useful), as stones are kept. A firing a quiet window excused has
// none.
func (l *Ledger) respond(t *txn, lo, hi int64, o verdict.Options) error {
	rows, err := t.QueryContext(t.ctx, `SELECT f.id, f.series_id, r.name, f.start_at, f.end_at, f.white, f.person, f.excused, f.response, f.routine, f.useful, s.labels
		FROM firings f JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id
		WHERE f.start_at >= ? AND f.start_at <= ? ORDER BY f.start_at, f.id`, lo, hi)
	if err != nil {
		return err
	}
	type row struct {
		respFiring
		resp, routine, useful int64
	}
	var fs []row
	for rows.Next() {
		var x row
		var series, start int64
		var end sql.NullInt64
		var text string
		if err := rows.Scan(&x.ID, &series, &x.Rule, &start, &end, &x.White, &x.Person, &x.Excused, &x.resp, &x.routine, &x.useful, &text); err != nil {
			rows.Close()
			return err
		}
		if x.Labels, err = l.labels(series, text); err != nil {
			rows.Close()
			return err
		}
		x.Start, x.End = unix(start), fromNull(end)
		fs = append(fs, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(fs) == 0 {
		return err
	}
	now := o.To
	if now.IsZero() {
		now = time.Now()
	}
	live := make([]respFiring, 0, len(fs))
	for _, x := range fs {
		if !x.Excused {
			live = append(live, x.respFiring)
		}
	}
	r, err := l.newResponder(t.ctx, t, t, o, live, now)
	if err != nil {
		return err
	}
	up, err := t.updating("firings", []string{"response", "routine", "useful"})
	if err != nil {
		return err
	}
	defer up.close()
	for _, x := range fs {
		resp, useful := int64(response.None), int64(response.Unknown)
		routine := int64(0)
		if !x.Excused {
			rs, u, err := r.respond(x.respFiring)
			if err != nil {
				return err
			}
			resp, useful = int64(rs.Type), int64(u)
			if rs.Change != nil && (rs.Type == response.Acknowledged || rs.Type == response.Unanswered) {
				routine = 1
			}
		}
		if resp == x.resp && routine == x.routine && useful == x.useful {
			continue
		}
		if err := up.add(resp, routine, useful, x.ID); err != nil {
			return err
		}
	}
	return up.close()
}

// ResponsesOf finds what was done about each of the firings, by their id, as
// judging does, with the page and the change each rests on: for pages that
// show a firing or list them. now is when a firing still on is taken to
// end.
func (l *Ledger) ResponsesOf(ctx context.Context, fs []Firing, o verdict.Options, now time.Time) (map[int64]response.Response, error) {
	in := make([]respFiring, 0, len(fs))
	for _, f := range fs {
		in = append(in, respFiring{ID: f.ID, Rule: f.Rule, Labels: f.Labels, Start: f.Start, End: f.End, White: f.White, Person: f.Person, Excused: f.Excused})
	}
	r, err := l.newResponder(ctx, l.r, l.r, o, in, now)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]response.Response, len(fs))
	for _, f := range in {
		if f.Excused {
			continue
		}
		rs, _, err := r.respond(f)
		if err != nil {
			return nil, err
		}
		out[f.ID] = rs
	}
	return out, nil
}

// RolloutsKept says, of the changes read from the history, whether any was
// and whether autoscalers' targets were found.
func (l *Ledger) RolloutsKept(ctx context.Context) (rolled, autoscalers bool, err error) {
	in, err := l.inputs(ctx, l.r)
	if err != nil {
		return false, false, err
	}
	v, err := l.meta(ctx, l.r, "rollouts-autoscalers")
	return in.Rollouts, v == "1", err
}

// CountPages is how many pages were sent in [from, to).
func (l *Ledger) CountPages(ctx context.Context, from, to time.Time) (int, error) {
	var n int
	err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM responses WHERE sent_at >= ? AND sent_at < ?", from.Unix(), to.Unix()).Scan(&n)
	return n, err
}

// CountChanges is how many changes were read in [from, to).
func (l *Ledger) CountChanges(ctx context.Context, from, to time.Time) (int, error) {
	var n, m int
	if err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM rollouts WHERE at >= ? AND at < ?", from.Unix(), to.Unix()).Scan(&n); err != nil {
		return 0, err
	}
	err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM deploys WHERE start_at >= ? AND start_at < ?", from.Unix(), to.Unix()).Scan(&m)
	return n + m, err
}

// Kinds of response, as lapilli verdict says them (verdict.Response*).
func responseKind(t response.Type) string {
	switch t {
	case response.Changed:
		return verdict.ResponseChanged
	case response.ChangedRoutine:
		return verdict.ResponseRoutine
	case response.Acknowledged:
		return verdict.ResponseAcknowledged
	case response.Unanswered:
		return verdict.ResponseUnanswered
	}
	return verdict.ResponseNone
}

// ResponseOfJudged is a firing's response as a report gives it, from what
// was found for it: its kind and whether it was useful, with the change and
// the page it rests on. judged says its record judges it, should it have
// caught nothing.
func ResponseOfJudged(resp response.Response, white, judged bool, answer string) verdict.FiringResponse {
	out := verdict.FiringResponse{Kind: responseKind(resp.Type)}
	switch u := response.Useful(white, judged, response.Answer(answer), resp, resp.Known); u {
	case response.IsUseful, response.NotUseful:
		b := u == response.IsUseful
		out.Useful = &b
	}
	if c := resp.Change; c != nil {
		labels := c.Labels
		if c.Kind != response.Deploy {
			labels = map[string]string{"namespace": c.Namespace, strings.ToLower(c.WorkloadKind): c.Workload}
		}
		out.Change = &verdict.ResponseChange{Kind: string(c.Kind), At: c.At, Labels: labels, Routine: resp.Type != response.Changed}
	}
	if p := resp.Page; p != nil {
		out.Page = &verdict.ResponsePage{ID: p.ID, Sent: p.Sent, Acknowledged: p.Acknowledged, Resolved: p.Resolved, ResolvedBy: p.ResolvedBy}
	}
	return out
}

// respondJudged gives each firing of a report its response, as judging
// found it, with the page and the change it rests on.
func (l *Ledger) respondJudged(ctx context.Context, js []verdict.Judged, o verdict.Options, now time.Time) error {
	in := make([]respFiring, len(js))
	for i, j := range js {
		in[i] = respFiring{ID: int64(i), Rule: j.Rule, Labels: j.Labels, Start: j.Start, End: j.End, White: j.White}
		switch j.Action {
		case verdict.ActionYes:
			in[i].Person = personYes
		case verdict.ActionNo:
			in[i].Person = personNo
		}
	}
	r, err := l.newResponder(ctx, l.r, l.r, o, in, now)
	if err != nil {
		return err
	}
	for i, f := range in {
		resp, _, err := r.respond(f)
		if err != nil {
			return err
		}
		fr := ResponseOfJudged(resp, js[i].White, !js[i].NoRecord, js[i].Action)
		js[i].Response = &fr
	}
	return nil
}
