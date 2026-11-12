package ledger

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/verdict"
)

// Deploy is when something was deployed (docs/design/noc.md): a service, a
// namespace, as its labels say, from Start to End.
type Deploy struct {
	Start    time.Time         `json:"start"`
	End      time.Time         `json:"end"`
	Labels   map[string]string `json:"labels,omitempty"`
	Revision string            `json:"revision,omitempty"`
	// Source is the history it was read from: a file's name.
	Source string `json:"source,omitempty"`
}

// Bounds on what the deploys file holds: the ledger keeps MaxDeploys of
// them, each lasting MaxDeployLen at most; a firing counts as during one up
// to MaxDeployAfter after it ends.
const (
	MaxDeploys     = 50000
	MaxDeployLen   = 24 * time.Hour
	MaxDeployAfter = 24 * time.Hour
)

// During reports whether a firing came during the deploy: it started from
// the deploy's start to after its end, and has the value of a label the
// deploy names, any of them (the same service, or the same namespace). A
// deploy that names no label is everything's.
func (d Deploy) During(f firing.Firing, after time.Duration) bool {
	if f.Start.Before(d.Start) || f.Start.After(d.End.Add(after)) {
		return false
	}
	return deployOf(d.Labels, f.Labels)
}

func deployOf(deployed, labels map[string]string) bool {
	if len(deployed) == 0 {
		return true
	}
	for k, v := range deployed {
		if labels[k] == v {
			return true
		}
	}
	return false
}

// check refuses a deploy the ledger won't keep.
func (d Deploy) check() error {
	switch {
	case d.Start.IsZero():
		return invalid("a deploy needs a start")
	case d.End.Before(d.Start):
		return invalid("a deploy ends after it starts")
	case d.End.Sub(d.Start) > MaxDeployLen:
		return invalid("a deploy lasts a day at most")
	case d.Start.Year() < 1970 || d.End.Year() > 9999:
		return invalid("a deploy's times are from 1970 to 9999")
	case len(d.Revision) > 200 || !utf8.ValidString(d.Revision):
		return invalid("a deploy's revision is 200 bytes at most")
	case len(d.Source) > 200 || !utf8.ValidString(d.Source):
		return invalid("a deploy's source is 200 bytes at most")
	case len(d.Labels) > 16:
		return invalid("a deploy names 16 labels at most")
	}
	n := 0
	for k, v := range d.Labels {
		n += len(k) + len(v)
		if k == "" || v == "" || !utf8.ValidString(k) || !utf8.ValidString(v) {
			return invalid("a deploy's labels each have a name and a value")
		}
	}
	if n > 4096 {
		return invalid("a deploy's labels are 4 KB at most")
	}
	return nil
}

// key is what tells two deploys apart: one given twice is kept once.
func (d Deploy) key() string {
	b, _ := json.Marshal(orEmptyMap(d.Labels)) // keys sorted
	return strconv.FormatInt(d.Start.Unix(), 10) + "\x00" + strconv.FormatInt(d.End.Unix(), 10) + "\x00" + string(b) + "\x00" + d.Revision + "\x00" + d.Source
}

// SyncDeploys makes the ledger's deploys those given, which the deploys
// file owns, and gives the firings they meet, before and after, their
// marks again: what changed, or every deploy's when o.DeployAfter differs
// from what they were marked with. The span is judged a week at a time, the
// first two seconds' worth before it returns; the worker judges the rest
// (JudgePending). changed says whether anything did.
func (l *Ledger) SyncDeploys(ctx context.Context, ds []Deploy, o verdict.Options) (changed bool, err error) {
	if len(ds) > MaxDeploys {
		return false, invalid("the ledger keeps %d deploys at most: give a later start, or fewer services", MaxDeploys)
	}
	if o.DeployAfter < 0 || o.DeployAfter > MaxDeployAfter {
		return false, invalid("--deploy-after is a day at most")
	}
	want := make(map[string]Deploy, len(ds))
	var errs []error
	for _, d := range ds {
		d.Start, d.End = d.Start.Truncate(time.Second), d.End.Truncate(time.Second)
		if d.End.IsZero() {
			d.End = d.Start
		}
		if err := d.check(); err != nil {
			errs = append(errs, err)
			continue
		}
		want[d.key()] = d
	}
	if err := errors.Join(errs...); err != nil {
		return false, err
	}
	after := int64(o.DeployAfter / time.Second)
	err = l.write(ctx, func(t *txn) error {
		was, err := l.meta(t.ctx, t, "deploy-after")
		if err != nil {
			return err
		}
		reach, moved := after, false
		if was != "" && was != strconv.FormatInt(after, 10) {
			old, _ := strconv.ParseInt(was, 10, 64)
			reach, moved = max(after, old), true
		}
		have, err := l.deploys(t.ctx, t, "")
		if err != nil {
			return err
		}
		var spans [][2]int64
		span := func(d Deploy) { spans = append(spans, [2]int64{d.Start.Unix(), d.End.Unix() + reach}) }
		del, err := t.PrepareContext(t.ctx, "DELETE FROM deploys WHERE id = ?")
		if err != nil {
			return err
		}
		defer del.Close()
		kept := map[string]bool{}
		for _, h := range have {
			k := h.key()
			if _, ok := want[k]; ok && !kept[k] {
				kept[k] = true
				if moved {
					span(h.Deploy)
				}
				continue
			}
			if _, err := del.ExecContext(t.ctx, h.id); err != nil {
				return err
			}
			span(h.Deploy)
		}
		ins, err := t.PrepareContext(t.ctx, "INSERT INTO deploys (start_at, end_at, labels, revision, source) VALUES (?, ?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer ins.Close()
		for _, k := range slices.Sorted(maps.Keys(want)) {
			if kept[k] {
				continue
			}
			d := want[k]
			b, err := json.Marshal(orEmptyMap(d.Labels))
			if err != nil {
				return err
			}
			if _, err := ins.ExecContext(t.ctx, d.Start.Unix(), d.End.Unix(), string(b), d.Revision, d.Source); err != nil {
				return err
			}
			span(d)
		}
		if err := setMeta(t.ctx, t, "deploy-after", strconv.FormatInt(after, 10)); err != nil {
			return err
		}
		changed = len(spans) > 0
		return pend(t, near(spans))
	})
	if err != nil || !changed {
		return changed, err
	}
	l.judgeSoon(ctx, o)
	return true, nil
}

// near puts spans in order and makes those less than six hours apart one:
// judging a span counts its days again, so a deploy every few hours is
// judged a week at a time rather than each on its own.
func near(spans [][2]int64) [][2]int64 {
	slices.SortFunc(spans, func(a, b [2]int64) int { return cmp.Compare(a[0], b[0]) })
	var out [][2]int64
	for _, s := range spans {
		if n := len(out); n > 0 && s[0] < out[n-1][1]+6*3600 {
			out[n-1][1] = max(out[n-1][1], s[1])
			continue
		}
		out = append(out, s)
	}
	return out
}

// deployRow is a deploy as kept, and its row.
type deployRow struct {
	id int64
	Deploy
}

func (l *Ledger) deploys(ctx context.Context, q queryer, where string, args ...any) ([]deployRow, error) {
	rows, err := q.QueryContext(ctx, "SELECT id, start_at, end_at, labels, revision, source FROM deploys "+where+" ORDER BY start_at, id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []deployRow
	for rows.Next() {
		var d deployRow
		var start, end int64
		var labels string
		if err := rows.Scan(&d.id, &start, &end, &labels, &d.Revision, &d.Source); err != nil {
			return nil, err
		}
		d.Start, d.End = unix(start), unix(end)
		if err := json.Unmarshal([]byte(labels), &d.Labels); err != nil {
			return nil, err
		}
		if len(d.Labels) == 0 {
			d.Labels = nil
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// deploysMeeting are the deploys a firing that started in [lo, hi] can have
// come during, the earliest first.
func (l *Ledger) deploysMeeting(ctx context.Context, q queryer, lo, hi int64, after time.Duration) ([]Deploy, error) {
	reach := int64((after + MaxDeployLen) / time.Second)
	rows, err := l.deploys(ctx, q, "WHERE start_at <= ? AND start_at >= ? AND end_at >= ?", hi, lo-reach, lo-int64(after/time.Second))
	if err != nil {
		return nil, err
	}
	out := make([]Deploy, len(rows))
	for i, r := range rows {
		out[i] = r.Deploy
	}
	return out, nil
}

// Deploys are the deploys that started in [from, to), the earliest first.
func (l *Ledger) Deploys(ctx context.Context, from, to time.Time) ([]Deploy, error) {
	rows, err := l.deploys(ctx, l.r, "WHERE start_at >= ? AND start_at < ?", from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	out := make([]Deploy, len(rows))
	for i, r := range rows {
		out[i] = r.Deploy
	}
	return out, nil
}

// HasDeploys reports whether the ledger keeps any deploy.
func (l *Ledger) HasDeploys(ctx context.Context) (bool, error) {
	var n int
	err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM deploys LIMIT 1)").Scan(&n)
	return n > 0, err
}

// during works out, for firings judged over a span, whether each came
// during one of ds, the deploys meeting the span: ds sorted by start.
func during(ds []Deploy, after time.Duration) func(firing.Firing) bool {
	if len(ds) == 0 {
		return func(firing.Firing) bool { return false }
	}
	return func(f firing.Firing) bool {
		// The deploys that started by the firing's start; of those, one
		// started more than MaxDeployLen and after before can't reach it.
		n := sort.Search(len(ds), func(i int) bool { return ds[i].Start.After(f.Start) })
		for i := n - 1; i >= 0 && !ds[i].Start.Before(f.Start.Add(-MaxDeployLen-after)); i-- {
			if ds[i].During(f, after) {
				return true
			}
		}
		return false
	}
}

// DeployText is a deploy as a person reads it: its labels and revision.
func DeployText(d Deploy) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(d.Labels)) {
		parts = append(parts, k+"="+d.Labels[k])
	}
	if d.Revision != "" {
		parts = append(parts, "@"+d.Revision)
	}
	return strings.Join(parts, " ")
}
