package ledger

import (
	"cmp"
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

// ResponsesCopier is who a job sending the pages of a paging tool writes as.
// It isn't a person, so outcomes don't count it as use.
const ResponsesCopier = "responses-token"

// ResponsesFile is the source of the pages --responses reads.
const ResponsesFile = "file"

// ResponseReach is how long before a page was sent the firing it answers
// may have started: a page is tied to a firing that was on when it was
// sent, and one lasts days at most before its page. A page written or
// changed has the firings that started this long before it judged again.
const ResponseReach = 14 * 24 * time.Hour

// Bounds on the pages kept, as the firings PUT's (MaxSources, MaxRecordedPeriod,
// skew): a source may be written a period of 31 days at a time, pages sent
// more than an hour ahead of this clock are refused, and every judge and
// view reads the pages of a firing's rule and time only, so a token's writes
// can't make them read more than these allow. Variables, so tests can make
// them small.
var (
	// maxSourcePages is how many pages one source keeps (the firings'
	// maxSourceSeries): 500 000, a page of an incident a minute for a year.
	maxSourcePages = 500_000
	// maxWeekPages is how many pages sent in one ledger week one source
	// keeps: 200 000, 20 000 a day for each of 10 busy sources' worth. It is
	// held for each source apart, so that one token can't stop the writes of
	// another source (the file's among them): the pages of a week are
	// MaxSources x this at most.
	maxWeekPages = 200_000
)

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
	switch {
	case !ValidSource(source):
		return PutResponsesResult{}, invalid("a source is lowercase letters, digits and dashes, 40 at most, such as pagerduty-prod")
	case from.IsZero() || to.IsZero():
		return PutResponsesResult{}, invalid("say which period the pages were sent in: from and to, as RFC 3339 times")
	case !to.After(from):
		return PutResponsesResult{}, invalid("to must be after from")
	case to.Sub(from) > MaxRecordedPeriod:
		return PutResponsesResult{}, invalid("a period is 31 days at most")
	case !sane(from) || !sane(to):
		return PutResponsesResult{}, invalid("a period is from 1970 to 9999")
	case from.After(time.Now().Add(skew)):
		return PutResponsesResult{}, invalid("a period can't start in the future")
	}
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
	if len(pages) > response.MaxPages {
		return res, invalid("one write holds %d pages at most: send a shorter period", response.MaxPages)
	}
	ahead := time.Now().Add(skew).Unix()
	want := make(map[string]pageRow, len(pages))
	rulesOf := make(map[string][]string, len(pages))
	for _, p := range pages {
		if p.Sent.Unix() > ahead || !sane(p.Sent) {
			return res, invalid("page %s was sent %s: a page is sent from 1970 until an hour from now", p.ID, p.Sent.UTC().Format(time.RFC3339))
		}
		want[p.ID] = pageRowOf(p)
		for _, a := range p.Alerts {
			if !slices.Contains(rulesOf[p.ID], a.Rule) {
				rulesOf[p.ID] = append(rulesOf[p.ID], a.Rule)
			}
		}
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
		delRules, err := t.PrepareContext(t.ctx, "DELETE FROM response_rules WHERE source = ? AND id = ?")
		if err != nil {
			return err
		}
		defer delRules.Close()
		putRule, err := t.PrepareContext(t.ctx, "INSERT INTO response_rules (rule, sent_at, source, id) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING")
		if err != nil {
			return err
		}
		defer putRule.Close()
		var weeks []int64
		for _, id := range slices.Sorted(maps.Keys(have)) {
			if _, ok := want[id]; ok {
				continue
			}
			if _, err := del.ExecContext(t.ctx, source, id); err != nil {
				return err
			}
			if _, err := delRules.ExecContext(t.ctx, source, id); err != nil {
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
			if _, err := delRules.ExecContext(t.ctx, source, id); err != nil {
				return err
			}
			for _, rule := range rulesOf[id] {
				if _, err := putRule.ExecContext(t.ctx, rule, w.sent, source, id); err != nil {
					return err
				}
			}
			if wk := Week(l.day(w.sent)); !slices.Contains(weeks, wk) {
				weeks = append(weeks, wk)
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
		if err := l.boundResponses(t, source, weeks); err != nil {
			return err
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

// boundResponses holds what a write left to the bounds: the sources there are
// (a new one counts), the pages of the source, and the pages of the source
// sent in each week the write put a page in. The write is rolled back if it is over.
func (l *Ledger) boundResponses(t *txn, source string, weeks []int64) error {
	var known bool
	if err := t.QueryRowContext(t.ctx, "SELECT EXISTS (SELECT 1 FROM response_sources WHERE source = ?)", source).Scan(&known); err != nil {
		return err
	}
	if !known {
		var n int
		if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM response_sources").Scan(&n); err != nil {
			return err
		}
		if n >= MaxSources {
			return invalid("the ledger keeps pages of %d sources at most", MaxSources)
		}
		if _, err := t.ExecContext(t.ctx, "INSERT INTO response_sources (source) VALUES (?) ON CONFLICT DO NOTHING", source); err != nil {
			return err
		}
	}
	var n int
	if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM responses WHERE source = ?", source).Scan(&n); err != nil {
		return err
	}
	if n > maxSourcePages {
		return invalid("a source keeps %d pages at most", maxSourcePages)
	}
	for _, w := range weeks {
		if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM responses WHERE source = ? AND sent_at >= ? AND sent_at < ?", source, l.midnight(w), l.midnight(w+7)).Scan(&n); err != nil {
			return err
		}
		if n > maxWeekPages {
			return invalid("%d pages or more of this source were sent in one week: a source keeps %d a week at most", n, maxWeekPages)
		}
	}
	return nil
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
// then their id, as ResponsePages gives them: for tests and pages that
// list them.
func (l *Ledger) pagesBetween(ctx context.Context, q queryer, lo, hi int64) ([]response.Page, error) {
	rows, err := q.QueryContext(ctx, `SELECT source, id, sent_at, acknowledged_at, resolved_at, resolved_by, alerts FROM responses
		WHERE sent_at >= ? AND sent_at <= ? ORDER BY sent_at, id, source`, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPages(rows)
}

func scanPages(rows *sql.Rows) ([]response.Page, error) {
	var out []response.Page
	for rows.Next() {
		var source, id string
		var r pageRow
		var ack, rs sql.NullInt64
		if err := rows.Scan(&source, &id, &r.sent, &ack, &rs, &r.resBy, &r.alerts); err != nil {
			return nil, err
		}
		r.ack, r.res = ack.Int64, rs.Int64
		p, err := pageOf(id, r)
		if err != nil {
			return nil, err
		}
		p.Source = source
		out = append(out, p)
	}
	return out, rows.Err()
}

// pageWindow is where the pages a firing may have been sent as are: those
// that name rule, sent in [from, to].
type pageWindow struct {
	rule     string
	from, to int64
}

// pagesNear are the pages judging firings reads: for each window, the first
// response.MaxFindScan pages that name its rule and were sent in it (those
// Index.Find looks at, which looks at no more), in order of their sent time
// then source and id (ties at the cut may fall otherwise than Find's order
// of sent time, id and source). The windows of a rule that meet are asked
// together, and again one by one only when more than MaxFindScan pages are in
// them. The pages' alerts (16 KB each at most) are read for those only, so a
// judgement reads, for a rule, at most MaxFindScan pages for each window of
// its firings, and no more than the rule has in them: not every page of the
// rule over the days the firings span, whoever wrote them. Returned in order
// of their sent time, then id and source.
func (l *Ledger) pagesNear(ctx context.Context, q queryer, ws []pageWindow) ([]response.Page, error) {
	byRule := map[string][][2]int64{}
	for _, w := range ws {
		byRule[w.rule] = append(byRule[w.rule], [2]int64{w.from, w.to})
	}
	type key struct{ source, id string }
	seen := map[key]bool{}
	bySource := map[string][]string{}
	keys := func(rule string, from, to int64, n int) ([]key, error) {
		rows, err := q.QueryContext(ctx, `SELECT source, id FROM response_rules WHERE rule = ? AND sent_at >= ? AND sent_at <= ?
			ORDER BY sent_at, source, id LIMIT ?`, rule, from, to, n)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var got []key
		for rows.Next() {
			var k key
			if err := rows.Scan(&k.source, &k.id); err != nil {
				return nil, err
			}
			got = append(got, k)
		}
		return got, rows.Err()
	}
	add := func(ks []key) {
		for _, k := range ks {
			if !seen[k] {
				seen[k] = true
				bySource[k.source] = append(bySource[k.source], k.id)
			}
		}
	}
	for _, rule := range slices.Sorted(maps.Keys(byRule)) {
		wins := byRule[rule]
		slices.SortFunc(wins, func(a, b [2]int64) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1])) })
		wins = slices.Compact(wins)
		for i := 0; i < len(wins); {
			j, hi := i+1, wins[i][1]
			for j < len(wins) && wins[j][0] <= hi {
				hi = max(hi, wins[j][1])
				j++
			}
			// Every page of the windows together, unless there are more
			// than one window's scan reads.
			ks, err := keys(rule, wins[i][0], hi, response.MaxFindScan+1)
			if err != nil {
				return nil, err
			}
			if len(ks) <= response.MaxFindScan {
				add(ks)
			} else {
				for _, w := range wins[i:j] {
					ks, err := keys(rule, w[0], w[1], response.MaxFindScan)
					if err != nil {
						return nil, err
					}
					add(ks)
				}
			}
			i = j
		}
	}
	var out []response.Page
	for _, source := range slices.Sorted(maps.Keys(bySource)) {
		ids := bySource[source]
		for len(ids) > 0 {
			part := ids[:min(len(ids), pgRows)]
			ids = ids[len(part):]
			b, _ := json.Marshal(part)
			rows, err := q.QueryContext(ctx, `SELECT source, id, sent_at, acknowledged_at, resolved_at, resolved_by, alerts FROM responses
				WHERE source = ? AND id IN (`+l.d.texts("?")+")", source, string(b))
			if err != nil {
				return nil, err
			}
			got, err := scanPages(rows)
			rows.Close()
			if err != nil {
				return nil, err
			}
			l.pagesRead.Add(int64(len(got)))
			out = append(out, got...)
		}
	}
	slices.SortFunc(out, func(a, b response.Page) int {
		if c := a.Sent.Compare(b.Sent); c != 0 {
			return c
		}
		if c := strings.Compare(a.ID, b.ID); c != 0 {
			return c
		}
		return strings.Compare(a.Source, b.Source)
	})
	return out, nil
}

// ResponseInputs says what the ledger keeps to find responses from: pages,
// changes read from the history, deploys.
type ResponseInputs struct {
	Pages, Rollouts, Deploys bool
}

// Any reports whether the ledger keeps any of them.
func (r ResponseInputs) Any() bool { return r.Pages || r.Rollouts || r.Deploys }

// Shown reports whether what was done about the firings is counted and shown:
// the ledger keeps pages or changes read from the history, or rules are found
// noisy by what was done about their firings (o.NoisyBy useful), where a
// person's "needed action" is one of the answers. A ledger that keeps deploys
// alone, as serve did before responses, shows none while rules are found
// noisy by what they caught.
func (r ResponseInputs) Shown(o verdict.Options) bool {
	return r.Pages || r.Rollouts || o.NoisyBy == verdict.NoisyByUseful
}

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
	var fresh []changeTarget
	for _, c := range r.Changes {
		res, err := ins.ExecContext(t.ctx, string(c.Kind), c.At.Unix(), c.Namespace, c.WorkloadKind, c.Workload, c.Revision)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fresh = append(fresh, changeTarget{at: c.At.Unix(), namespace: c.Namespace, workloadKind: c.WorkloadKind, workload: c.Workload})
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
			// Known over more than it was: the firings that were on in it
			// are judged again, though no change was found (a read made
			// again, or a run that found none).
			fresh = append(fresh, changeTarget{cover: true, namespace: w.Namespace, workloadKind: w.Kind, workload: w.Name, from: from, to: to})
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
	return l.pendTargets(t, fresh, lo-1)
}

// changeTarget is a change that is new to the ledger and what it was made to:
// a rollout's workload, or a deploy's labels, which a firing it is about
// carries every one of (response.Carries, as Matching ties it). Or, when
// cover is set, a workload whose changes became known over [from, to], which
// the firings that were on in it are judged again for.
type changeTarget struct {
	at                     int64
	namespace              string
	workloadKind, workload string
	labels                 map[string]string
	deploy                 bool
	cover                  bool
	from, to               int64
}

// podOwners finds, and keeps, the workload a pod belongs to, as the runs saw.
type podOwners struct {
	t  *txn
	by map[response.Pod]*response.Workload
}

func (o *podOwners) of(ns, pod string) (*response.Workload, error) {
	k := response.Pod{Namespace: ns, Name: pod}
	if w, ok := o.by[k]; ok {
		return w, nil
	}
	var w response.Workload
	err := o.t.QueryRowContext(o.t.ctx, "SELECT workload_kind, workload FROM rollout_owners WHERE namespace = ? AND pod = ?", ns, pod).Scan(&w.Kind, &w.Name)
	if err == sql.ErrNoRows {
		o.by[k] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	w.Namespace = ns
	o.by[k] = &w
	return &w, nil
}

// matches reports whether a firing with labels may be of the change's
// target. A workload's firing carries its namespace and names it by a label
// of its kind, by a pod it owns, or by any label's value (--rollout-match,
// which isn't known here, names one): too many are put back, never too few.
func (c changeTarget) matches(labels map[string]string, own *podOwners) (bool, error) {
	if c.deploy {
		return response.Carries(labels, c.labels), nil
	}
	if labels["namespace"] != c.namespace {
		return false, nil
	}
	for _, v := range labels {
		if v == c.workload {
			return true, nil
		}
	}
	if pod := labels["pod"]; pod != "" {
		w, err := own.of(c.namespace, pod)
		if err != nil || w == nil {
			return false, err
		}
		return w.Kind == c.workloadKind && w.Name == c.workload, nil
	}
	return false, nil
}

// pendTargets puts firings back to be judged for changes new to the ledger
// (or taken from it) and for coverage that grew: the firings of what each
// change was made to (its workload, the pods it owns, or the deploy's
// labels) that started in the ledger week it was made in, whose routine
// counts it changes (counted by the week a firing started in), and those that
// started up to ResponseReach before it and were on when it was made, which
// it answers; and the firings of a workload that were on while its changes
// became known. Each firing is put back by itself (spans of its start,
// joined to others within 6 hours, so at most 28 a week of firings a change
// concerns), not the week from the first of a target's to its last. Only
// firings that started no later than hi: later ones are judged by the run
// that brought the changes.
func (l *Ledger) pendTargets(t *txn, ts []changeTarget, hi int64) error {
	if len(ts) == 0 {
		return nil
	}
	own := &podOwners{t: t, by: map[response.Pod]*response.Workload{}}
	byWeek := map[int64][]changeTarget{}
	var covers []changeTarget
	for _, c := range ts {
		if c.cover {
			covers = append(covers, c)
			continue
		}
		w := Week(l.day(c.at))
		byWeek[w] = append(byWeek[w], c)
	}
	var spans [][2]int64
	reach := int64(ResponseReach / time.Second)
	for _, w := range slices.Sorted(maps.Keys(byWeek)) {
		cs := byWeek[w]
		first := cs[0].at
		for _, c := range cs {
			first = min(first, c.at)
		}
		monday, next := l.midnight(w), l.midnight(w+7)
		// The week's firings, and those before it that were on at the
		// first change of the week: which series they are of, then the
		// firings of those that the changes are made to.
		where := `f.start_at <= ? AND ((f.start_at >= ? AND f.start_at < ?) OR (f.start_at >= ? AND f.start_at < ? AND (f.end_at IS NULL OR f.end_at >= ?)))`
		args := []any{hi, monday, next, first - reach, monday, first}
		// One statement for the week: each firing and the series it is of.
		rows, err := t.QueryContext(t.ctx, `SELECT s.id, s.labels, f.start_at FROM firings f JOIN series s ON s.id = f.series_id WHERE `+where+` ORDER BY s.id`, args...)
		if err != nil {
			return err
		}
		type startRow struct {
			series, start int64
		}
		var starts []startRow
		labelsOf := map[int64]string{}
		for rows.Next() {
			var x startRow
			var text string
			if err := rows.Scan(&x.series, &text, &x.start); err != nil {
				rows.Close()
				return err
			}
			labelsOf[x.series] = text
			starts = append(starts, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// (The owners are asked once the rows are read: a transaction on
		// Postgres has one statement open at a time.)
		matched := map[int64]bool{}
		for id, text := range labelsOf {
			var m map[string]string
			if err := json.Unmarshal([]byte(text), &m); err != nil {
				return err
			}
			for _, c := range cs {
				ok, err := c.matches(m, own)
				if err != nil {
					return err
				}
				if ok {
					matched[id] = true
					break
				}
			}
		}
		for _, x := range starts {
			if matched[x.series] {
				spans = append(spans, [2]int64{x.start, x.start})
			}
		}
	}
	if len(covers) > 0 {
		lo, top := covers[0].from, covers[0].to
		byNS := map[string][]changeTarget{}
		for _, c := range covers {
			lo, top = min(lo, c.from), max(top, c.to)
			byNS[c.namespace] = append(byNS[c.namespace], c)
		}
		rows, err := t.QueryContext(t.ctx, `SELECT s.id, s.labels, f.start_at, f.end_at FROM firings f JOIN series s ON s.id = f.series_id
			WHERE f.start_at <= ? AND f.start_at >= ? AND (f.end_at IS NULL OR f.end_at >= ?) ORDER BY s.id`, min(hi, top), lo-reach, lo)
		if err != nil {
			return err
		}
		type firingRow struct {
			series, start int64
			end           sql.NullInt64
			labels        string
		}
		var onRows []firingRow
		for rows.Next() {
			var x firingRow
			if err := rows.Scan(&x.series, &x.labels, &x.start, &x.end); err != nil {
				rows.Close()
				return err
			}
			onRows = append(onRows, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var series int64 = -1
		var mine []changeTarget
		for _, x := range onRows {
			if x.series != series {
				series, mine = x.series, nil
				var m map[string]string
				if err := json.Unmarshal([]byte(x.labels), &m); err != nil {
					return err
				}
				for _, c := range byNS[m["namespace"]] {
					ok, err := c.matches(m, own)
					if err != nil {
						return err
					}
					if ok {
						mine = append(mine, c)
					}
				}
			}
			for _, c := range mine {
				if x.start <= c.to && (!x.end.Valid || x.end.Int64 >= c.from) {
					spans = append(spans, [2]int64{x.start, x.start})
					break
				}
			}
		}
	}
	return pend(t, near(spans))
}

// RolloutFailed keeps a span a run could not read the changes of, for the
// next run to read once (RolloutRetries, RolloutRetried). One that overlaps
// or touches another is made one with it.
func (l *Ledger) RolloutFailed(ctx context.Context, from, to time.Time) error {
	return l.write(ctx, func(t *txn) error {
		a, b := from.Unix(), to.Unix()
		rows, err := t.QueryContext(t.ctx, "SELECT from_at, to_at FROM rollout_retries WHERE to_at >= ? AND from_at <= ?", a, b)
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
			if _, err := t.ExecContext(t.ctx, "DELETE FROM rollout_retries WHERE from_at = ?", x); err != nil {
				return err
			}
		}
		_, err = t.ExecContext(t.ctx, "INSERT INTO rollout_retries (from_at, to_at) VALUES (?, ?)", a, b)
		return err
	})
}

// RolloutRetries are the spans a run could not read the changes of, the
// earliest first.
func (l *Ledger) RolloutRetries(ctx context.Context) ([][2]time.Time, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT from_at, to_at FROM rollout_retries ORDER BY from_at")
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

// RolloutRetried drops the span that starts at from: it was read again, or
// tried, and isn't again.
func (l *Ledger) RolloutRetried(ctx context.Context, from time.Time) error {
	return l.write(ctx, func(t *txn) error {
		_, err := t.ExecContext(t.ctx, "DELETE FROM rollout_retries WHERE from_at = ?", from.Unix())
		return err
	})
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
	var windows []pageWindow
	pods := map[string]bool{}
	for _, f := range fs {
		end := f.End
		if end.IsZero() {
			end = now
		}
		// Where the page of a firing may have been sent: Evidence.Respond's.
		for _, n := range response.RuleNames(f.Rule) {
			windows = append(windows, pageWindow{n, f.Start.Unix(), end.Add(r.within(f.Rule)).Unix()})
		}
		w := Week(l.day(f.Start.Unix()))
		if end.After(r.last[w]) {
			r.last[w] = end
		}
		if p := f.Labels["pod"]; p != "" && f.Labels["namespace"] != "" {
			pods[p] = true
		}
	}
	if r.in.Pages {
		pages, err := l.pagesNear(ctx, q, windows)
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
	// A firing a quiet window excused has none, and asks for none of the
	// pages or changes the others do.
	in := make([]respFiring, 0, len(fs))
	for _, f := range fs {
		if !f.Excused {
			in = append(in, respFiring{ID: f.ID, Rule: f.Rule, Labels: f.Labels, Start: f.Start, End: f.End, White: f.White, Person: f.Person})
		}
	}
	r, err := l.newResponder(ctx, l.r, l.r, o, in, now)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]response.Response, len(in))
	for _, f := range in {
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
		in[i] = respFiring{ID: int64(i), Rule: j.Rule, Labels: j.Labels, Start: j.Start, End: j.End, White: j.White, Excused: j.Excused}
		switch j.Action {
		case verdict.ActionYes:
			in[i].Person = personYes
		case verdict.ActionNo:
			in[i].Person = personNo
		}
	}
	live := make([]respFiring, 0, len(in))
	for _, f := range in {
		if !f.Excused {
			live = append(live, f)
		}
	}
	r, err := l.newResponder(ctx, l.r, l.r, o, live, now)
	if err != nil {
		return err
	}
	for i, f := range in {
		// A firing a quiet window excused has none, here as in the ledger's
		// firings and tallies, and as ResponsesOf gives it.
		if f.Excused {
			continue
		}
		resp, _, err := r.respond(f)
		if err != nil {
			return err
		}
		fr := ResponseOfJudged(resp, js[i].White, !js[i].NoRecord, js[i].Action)
		js[i].Response = &fr
	}
	return nil
}
