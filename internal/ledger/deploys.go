package ledger

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
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
// them, each lasting MaxDeployLen at most, their labels MaxDeployLabels
// bytes in all, counted for each deploy; a firing counts as during one up
// to MaxDeployAfter before its start or after its end. A deploy starts no
// later than DeployAhead after the file is read.
const (
	MaxDeploys      = 50000
	MaxDeployLen    = 24 * time.Hour
	MaxDeployAfter  = 24 * time.Hour
	MaxDeployLabels = 8 << 20
	DeployAhead     = time.Hour
)

// During reports whether a firing came during the deploy: it started from
// before the deploy's start to after its end, and agrees with the deploy on
// every label both have, one at least (the same service, or a namespace's
// alert during a deploy in it). A label the firing hasn't stands for any
// value. A deploy that names no label is everything's.
func (d Deploy) During(f firing.Firing, before, after time.Duration) bool {
	if f.Start.Before(d.Start.Add(-before)) || f.Start.After(d.End.Add(after)) {
		return false
	}
	return deployOf(d.Labels, f.Labels)
}

func deployOf(deployed, labels map[string]string) bool {
	if len(deployed) == 0 {
		return true
	}
	shared := false
	for k, v := range deployed {
		have, ok := labels[k]
		if !ok {
			continue
		}
		if have != v {
			return false
		}
		shared = true
	}
	return shared
}

// check refuses a deploy the ledger won't keep, read at at.
func (d Deploy) check(at time.Time) error {
	switch {
	case d.Start.IsZero():
		return invalid("a deploy needs a start")
	case d.End.Before(d.Start):
		return invalid("a deploy ends after it starts")
	case d.End.Sub(d.Start) > MaxDeployLen:
		return invalid("a deploy lasts a day at most")
	case d.Start.Year() < 1970 || d.End.Year() > 9999:
		return invalid("a deploy's times are from 1970 to 9999")
	case d.Start.After(at.Add(DeployAhead)):
		return invalid("a deploy starts by an hour after the deploys file is read: %s is later", d.Start.UTC().Format(time.RFC3339))
	case len(d.Revision) > 200 || !utf8.ValidString(d.Revision):
		return invalid("a deploy's revision is 200 bytes at most")
	case len(d.Source) > 200 || !utf8.ValidString(d.Source):
		return invalid("a deploy's source is 200 bytes at most")
	case len(d.Labels) > 16:
		return invalid("a deploy names 16 labels at most")
	}
	if n := labelBytes(d.Labels); n > 4096 {
		return invalid("a deploy's labels are 4 KB at most")
	}
	for k, v := range d.Labels {
		if k == "" || v == "" || !utf8.ValidString(k) || !utf8.ValidString(v) {
			return invalid("a deploy's labels each have a name and a value")
		}
	}
	return nil
}

func labelBytes(m map[string]string) int {
	n := 0
	for k, v := range m {
		n += len(k) + len(v)
	}
	return n
}

// deployLabelsText is a label set as the ledger keeps it: one text for
// one set.
func deployLabelsText(m map[string]string) string {
	b, _ := json.Marshal(orEmptyMap(m)) // keys sorted
	return string(b)
}

// key is what tells two deploys apart: one given twice is kept once.
func deployKey(start, end int64, labels, revision, source string) string {
	return strconv.FormatInt(start, 10) + "\x00" + strconv.FormatInt(end, 10) + "\x00" + labels + "\x00" + revision + "\x00" + source
}

// DeploysRead is a read of the deploys file: when, and a digest of what it
// and its histories held, which an unchanged file is known by.
type DeploysRead struct {
	At  time.Time
	Sum string
}

// SyncDeploys makes the ledger's deploys those given, which the deploys
// file owns, and gives the firings they meet, before and after, their
// marks again: what changed, or every deploy's when the time before or
// after them differs from what they were marked with. The spans are judged
// a week at a time, the first two seconds' worth before it returns; the
// worker judges the rest (JudgePending). A read whose Sum is the last
// read's, with the same times before and after, changes nothing and reads
// nothing more. changed says whether anything did.
func (l *Ledger) SyncDeploys(ctx context.Context, ds []Deploy, o verdict.Options, read DeploysRead) (changed bool, err error) {
	if len(ds) > MaxDeploys {
		return false, invalid("the ledger keeps %d deploys at most: give a later start, or fewer services", MaxDeploys)
	}
	if o.DeployAfter < 0 || o.DeployAfter > MaxDeployAfter || o.DeployBefore < 0 || o.DeployBefore > MaxDeployAfter {
		return false, invalid("--deploy-before and --deploy-after are a day at most")
	}
	before, after := int64(o.DeployBefore/time.Second), int64(o.DeployAfter/time.Second)
	reachNow := reach(o)
	if same, err := l.DeploysUnchanged(ctx, read, o); err != nil || same {
		return false, err
	}
	if read.At.IsZero() {
		read.At = time.Now()
	}
	type wanted struct {
		Deploy
		labels string
	}
	want := make(map[string]wanted, len(ds))
	texts := map[uintptr]string{} // a history's lines share their labels: written as text once
	var errs []error
	total := 0
	for _, d := range ds {
		d.Start, d.End = d.Start.Truncate(time.Second), d.End.Truncate(time.Second)
		if d.End.IsZero() {
			d.End = d.Start
		}
		if err := d.check(read.At); err != nil {
			errs = append(errs, err)
			continue
		}
		if total += labelBytes(d.Labels); total > MaxDeployLabels {
			return false, invalid("the deploys' labels are %d MB in all at most, counted for each deploy", MaxDeployLabels>>20)
		}
		p := reflect.ValueOf(d.Labels).Pointer()
		text, ok := texts[p]
		if !ok || p == 0 {
			text = deployLabelsText(d.Labels)
			texts[p] = text
		}
		want[deployKey(d.Start.Unix(), d.End.Unix(), text, d.Revision, d.Source)] = wanted{d, text}
	}
	if err := errors.Join(errs...); err != nil {
		return false, err
	}
	err = l.write(ctx, func(t *txn) error {
		was, err := l.meta(t.ctx, t, "deploy-reach")
		if err != nil {
			return err
		}
		reachBefore, reachAfter, moved := before, after, false
		if was != "" && was != reachNow {
			var b, a int64
			if x, y, ok := strings.Cut(was, " "); ok {
				b, _ = strconv.ParseInt(x, 10, 64)
				a, _ = strconv.ParseInt(y, 10, 64)
			}
			reachBefore, reachAfter, moved = max(before, b), max(after, a), true
		}
		// The label sets kept, by their text.
		ids := map[string]int64{}
		rows, err := t.QueryContext(t.ctx, "SELECT id, labels FROM deploy_labels")
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var text string
			if err := rows.Scan(&id, &text); err != nil {
				rows.Close()
				return err
			}
			ids[text] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		textOf := make(map[int64]string, len(ids))
		for text, id := range ids {
			textOf[id] = text
		}
		var spans [][2]int64
		span := func(start, end int64) { spans = append(spans, [2]int64{start - reachBefore, end + reachAfter}) }
		del, err := t.PrepareContext(t.ctx, "DELETE FROM deploys WHERE id = ?")
		if err != nil {
			return err
		}
		defer del.Close()
		rows, err = t.QueryContext(t.ctx, "SELECT id, start_at, end_at, labels_id, revision, source FROM deploys")
		if err != nil {
			return err
		}
		type row struct {
			id, start, end, labels int64
			revision, source       string
		}
		var have []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.start, &r.end, &r.labels, &r.revision, &r.source); err != nil {
				rows.Close()
				return err
			}
			have = append(have, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		kept := map[string]bool{}
		for _, h := range have {
			k := deployKey(h.start, h.end, textOf[h.labels], h.revision, h.source)
			if _, ok := want[k]; ok && !kept[k] {
				kept[k] = true
				if moved {
					span(h.start, h.end)
				}
				continue
			}
			if _, err := del.ExecContext(t.ctx, h.id); err != nil {
				return err
			}
			span(h.start, h.end)
		}
		ins, err := t.PrepareContext(t.ctx, "INSERT INTO deploys (start_at, end_at, labels_id, revision, source) VALUES (?, ?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer ins.Close()
		for _, k := range slices.Sorted(maps.Keys(want)) {
			if kept[k] {
				continue
			}
			d := want[k]
			id, ok := ids[d.labels]
			if !ok {
				if err := t.QueryRowContext(t.ctx, "INSERT INTO deploy_labels (labels) VALUES (?) RETURNING id", d.labels).Scan(&id); err != nil {
					return err
				}
				ids[d.labels] = id
			}
			if _, err := ins.ExecContext(t.ctx, d.Start.Unix(), d.End.Unix(), id, d.Revision, d.Source); err != nil {
				return err
			}
			span(d.Start.Unix(), d.End.Unix())
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM deploy_labels WHERE id NOT IN (SELECT labels_id FROM deploys)"); err != nil {
			return err
		}
		if err := setMeta(t.ctx, t, "deploy-reach", reachNow); err != nil {
			return err
		}
		if read.Sum != "" {
			if err := setMeta(t.ctx, t, "deploys-read", read.Sum+" "+reachNow); err != nil {
				return err
			}
		}
		changed = len(spans) > 0
		// No firing has started after the newest the ledger has: a run
		// judges its own period, deploys and all.
		var newest sql.NullInt64
		if err := t.QueryRowContext(t.ctx, "SELECT max(start_at) FROM firings").Scan(&newest); err != nil {
			return err
		}
		var some [][2]int64
		for _, s := range spans {
			if newest.Valid && s[0] <= newest.Int64 {
				some = append(some, [2]int64{s[0], min(s[1], newest.Int64)})
			}
		}
		return pend(t, near(some))
	})
	if err != nil || !changed {
		return changed, err
	}
	l.judgeSoon(ctx, o)
	return true, nil
}

// reach is the times before and after deploys, as the ledger notes what
// they were marked with.
func reach(o verdict.Options) string {
	return strconv.FormatInt(int64(o.DeployBefore/time.Second), 10) + " " + strconv.FormatInt(int64(o.DeployAfter/time.Second), 10)
}

// DeploysUnchanged reports whether a read of the deploys file is the last
// one kept, by its digest, with the same times before and after deploys:
// what it holds needn't be read.
func (l *Ledger) DeploysUnchanged(ctx context.Context, read DeploysRead, o verdict.Options) (bool, error) {
	if read.Sum == "" {
		return false, nil
	}
	was, err := l.meta(ctx, l.r, "deploys-read")
	return was == read.Sum+" "+reach(o), err
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

// deploys are the deploys a where clause on d picks, the earliest first,
// each label set read once.
func (l *Ledger) deploys(ctx context.Context, q queryer, where string, args ...any) ([]Deploy, error) {
	rows, err := q.QueryContext(ctx, `SELECT d.start_at, d.end_at, d.labels_id, ls.labels, d.revision, d.source
		FROM deploys d JOIN deploy_labels ls ON ls.id = d.labels_id `+where+" ORDER BY d.start_at, d.id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deploy
	sets := map[int64]map[string]string{}
	for rows.Next() {
		var d Deploy
		var start, end, id int64
		var labels string
		if err := rows.Scan(&start, &end, &id, &labels, &d.Revision, &d.Source); err != nil {
			return nil, err
		}
		d.Start, d.End = unix(start), unix(end)
		m, ok := sets[id]
		if !ok {
			if err := json.Unmarshal([]byte(labels), &m); err != nil {
				return nil, err
			}
			if len(m) == 0 {
				m = nil
			}
			sets[id] = m
		}
		d.Labels = m
		out = append(out, d)
	}
	return out, rows.Err()
}

// deploysMeeting are the deploys a firing that started in [lo, hi] can have
// come during, the earliest first.
func (l *Ledger) deploysMeeting(ctx context.Context, q queryer, lo, hi int64, before, after time.Duration) ([]Deploy, error) {
	reach := int64((after + MaxDeployLen) / time.Second)
	return l.deploys(ctx, q, "WHERE d.start_at <= ? AND d.start_at >= ? AND d.end_at >= ?", hi+int64(before/time.Second), lo-reach, lo-int64(after/time.Second))
}

// Deploys are the deploys that started in [from, to), the earliest first.
func (l *Ledger) Deploys(ctx context.Context, from, to time.Time) ([]Deploy, error) {
	return l.deploys(ctx, l.r, "WHERE d.start_at >= ? AND d.start_at < ?", from.Unix(), to.Unix())
}

// HasDeploys reports whether the ledger keeps any deploy.
func (l *Ledger) HasDeploys(ctx context.Context) (bool, error) {
	var n int
	err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM deploys LIMIT 1) AS one").Scan(&n)
	return n > 0, err
}

// during works out, for firings judged over a span, whether each came
// during one of ds, the deploys meeting the span: ds sorted by start.
func during(ds []Deploy, before, after time.Duration) func(firing.Firing) bool {
	if len(ds) == 0 {
		return func(firing.Firing) bool { return false }
	}
	// The longest of them: a history's deploys are instants, so a firing
	// is set beside the few that started in the minutes before it, not a
	// day of them.
	var longest time.Duration
	for _, d := range ds {
		longest = max(longest, d.End.Sub(d.Start))
	}
	return func(f firing.Firing) bool {
		// The deploys that started by before after the firing's start; of
		// those, one started more than the longest and after before it
		// can't reach it.
		n := sort.Search(len(ds), func(i int) bool { return ds[i].Start.After(f.Start.Add(before)) })
		for i := n - 1; i >= 0 && !ds[i].Start.Before(f.Start.Add(-longest-after)); i-- {
			if ds[i].During(f, before, after) {
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
