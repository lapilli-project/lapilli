package ledger

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/verdict"
)

// Firings recorded elsewhere (docs/design/recorded.md): a job copies them
// in, a source and a period at a time, and they are judged as the firings a
// replay finds are. A recorded firing is a row of firings; its rule is a
// row of rules with a source.

// FiringsCopier is who a job copying firings in writes as. It isn't a
// person, so outcomes don't count it as use.
const FiringsCopier = "firings-token"

// Bounds on what is copied in.
const (
	// MaxSources is how many sources the ledger keeps.
	MaxSources = 50
	// MaxRecordedPeriod is the longest period one write replaces.
	MaxRecordedPeriod = 31 * 24 * time.Hour
	// MaxRecorded is how many firings one write holds at most.
	MaxRecorded = 100_000
	// maxRuleName is how long a rule's name from outside is, in characters:
	// CloudWatch's alarm names are 255 at most, Google Cloud's policies'
	// 512.
	maxRuleName = 512
	// skew is how far ahead of this clock a source's may run.
	skew = time.Hour
)

// How much a token can add, whatever it sends a write at a time. Variables,
// so tests can make them small.
var (
	// maxSourceRules is how many rules one source's firings may be of, and
	// maxRecordedRules how many all sources' may: each is a row of the
	// rules pages and a series of /metrics.
	maxSourceRules   = 5000
	maxRecordedRules = 20000
	// maxSourceSeries is how many alerts, a rule and its labels each, one
	// source may have; those left without firings go when --keep drops
	// their firings.
	maxSourceSeries = 500_000
)

// Queries that must use rules_recorded, the index of recorded rules: its
// rows are those with a source, so each also asks for a source that isn't
// empty, for SQLite to see it may.
const (
	qRecordedRule = "SELECT id FROM rules WHERE source = ? AND source != '' AND sent = ? AND severity = ?"
	qRecordedTwin = "SELECT EXISTS (SELECT 1 FROM rules WHERE source = ? AND source != '' AND sent = ? AND severity != ?)"
)

var sourceRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$`)

// ValidSource reports whether name can name a source: lowercase letters,
// digits and dashes, 40 at most, not starting or ending with a dash.
func ValidSource(name string) bool { return sourceRE.MatchString(name) }

// Source is where recorded firings come from, as the ledger keeps it.
type Source struct {
	Name string `json:"name"`
	// Written is when a job last wrote to it, and By who that was.
	Written time.Time `json:"written"`
	By      string    `json:"by"`
	// From and To are the furthest period written, the one ending last,
	// and Firings how many it held: an older period written again doesn't
	// move them back.
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Firings int       `json:"firings"`
	// Changed is when a write last changed the firings the ledger had.
	Changed time.Time `json:"changed,omitzero"`
	// Rules is how many rules the source's firings are of.
	Rules int `json:"rules"`
}

// Recorded is what writing a source's firings for a period did.
type Recorded struct {
	// Firings are those sent; Added and Removed, what changed in the
	// ledger, a firing that changed its end both.
	Firings int `json:"firings"`
	Added   int `json:"added"`
	Removed int `json:"removed"`
}

// Changed reports whether the ledger's firings changed.
func (r Recorded) Changed() bool { return r.Added+r.Removed > 0 }

// recordedKey is a recorded rule as its source tells it apart: by its name
// and severity.
type recordedKey struct{ sent, severity string }

// PutFirings puts a source's firings that start in [from, to) in place of
// those the ledger has, and gives stones again where they changed. Sent
// again as they are, nothing changes and nothing is judged again; only the
// source's last write is noted. It all happens, or none of it does.
func (l *Ledger) PutFirings(ctx context.Context, source string, from, to time.Time, fs []firing.Firing, by string, o verdict.Options) (Recorded, error) {
	out := Recorded{Firings: len(fs)}
	// The ledger keeps times to the second: a period is of whole seconds,
	// so two that meet share none, and each firing starts and ends at the
	// second it was in.
	if !from.Equal(from.Truncate(time.Second)) || !to.Equal(to.Truncate(time.Second)) {
		return out, invalid("from and to are whole seconds, such as 2026-11-02T00:00:00Z")
	}
	fs = slices.Clone(fs)
	for i := range fs {
		fs[i].Start, fs[i].End = fs[i].Start.Truncate(time.Second), fs[i].End.Truncate(time.Second)
	}
	if err := checkRecorded(source, from, to, fs, time.Now()); err != nil {
		return out, err
	}
	// One firing for each alert and start: sent twice, the one that ended,
	// and that ended last.
	type key struct {
		rule   recordedKey
		labels string
		start  int64
	}
	type want struct {
		rule   recordedKey
		labels string
		end    sql.NullInt64
	}
	wants := map[key]want{}
	for _, f := range fs {
		labels := f.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		b, err := json.Marshal(labels)
		if err != nil {
			return out, err
		}
		rk := recordedKey{f.Rule, labels["severity"]}
		text := l.d.kept(string(b))
		k := key{rk, text, f.Start.Unix()}
		end := nullable(f.End)
		if was, ok := wants[k]; ok && (was.end.Valid && (!end.Valid || was.end.Int64 >= end.Int64) || !was.end.Valid && !end.Valid) {
			continue
		}
		wants[k] = want{rk, text, end}
	}
	err := l.write(ctx, func(t *txn) error {
		var known bool
		if err := t.QueryRowContext(t.ctx, "SELECT EXISTS (SELECT 1 FROM sources WHERE name = ?)", source).Scan(&known); err != nil {
			return err
		}
		if !known {
			var n int
			if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM sources").Scan(&n); err != nil {
				return err
			}
			if n >= MaxSources {
				return invalid("the ledger keeps %d sources at most", MaxSources)
			}
		}
		// How many rules there are, the source's and all sources', counted
		// when a new one is to be made.
		rules := struct {
			counted       bool
			source, total int
		}{}
		ids := map[recordedKey]int64{}
		type have struct {
			id  int64
			end sql.NullInt64
		}
		// What the ledger has of the source over the period, by series and
		// start.
		type at struct{ series, start int64 }
		had := map[at][]have{}
		rows, err := t.QueryContext(t.ctx, `SELECT f.id, f.series_id, f.start_at, f.end_at FROM firings f JOIN rules r ON r.id = f.rule_id
			WHERE r.source = ? AND r.source != '' AND f.start_at >= ? AND f.start_at < ?`, source, from.Unix(), to.Unix())
		if err != nil {
			return err
		}
		for rows.Next() {
			var h have
			var a at
			if err := rows.Scan(&h.id, &a.series, &a.start, &h.end); err != nil {
				rows.Close()
				return err
			}
			had[a] = append(had[a], h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// The source's alerts, read at once: one not among them is new.
		series := map[seriesKey]int64{}
		rows, err = t.QueryContext(t.ctx, "SELECT s.id, s.rule_id, s.labels FROM series s JOIN rules r ON r.id = s.rule_id WHERE r.source = ? AND r.source != ''", source)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var k seriesKey
			if err := rows.Scan(&id, &k.rule, &k.labels); err != nil {
				rows.Close()
				return err
			}
			series[k] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		type add struct {
			rule, series, start int64
			end                 sql.NullInt64
			// fresh is, for an alert new to the ledger, its place in
			// made, and series its id once made.
			fresh int
		}
		var adds []add
		// The alerts new to the ledger, made together once all are known.
		var made []seriesKey
		// In order, so a write names new rules the same way each time.
		keys := slices.SortedFunc(maps.Keys(wants), func(a, b key) int {
			return cmp.Or(cmp.Compare(a.rule.sent, b.rule.sent), cmp.Compare(a.rule.severity, b.rule.severity),
				cmp.Compare(a.labels, b.labels), cmp.Compare(a.start, b.start))
		})
		for _, k := range keys {
			w := wants[k]
			id, ok := ids[w.rule]
			if !ok {
				var made bool
				if id, made, err = l.recordedRule(t, source, w.rule, func() error {
					if !rules.counted {
						if err := t.QueryRowContext(t.ctx, `SELECT count(*) FILTER (WHERE source = ?), count(*) FROM rules WHERE source != ''`,
							source).Scan(&rules.source, &rules.total); err != nil {
							return err
						}
						rules.counted = true
					}
					switch {
					case rules.source >= maxSourceRules:
						return invalid("a source's firings are of %d rules at most", maxSourceRules)
					case rules.total >= maxRecordedRules:
						return invalid("the ledger keeps %d rules copied in at most, from all sources", maxRecordedRules)
					}
					return nil
				}); err != nil {
					return err
				}
				if made {
					rules.source++
					rules.total++
				}
				ids[w.rule] = id
			}
			sk := seriesKey{id, w.labels}
			sid, ok := series[sk]
			if !ok {
				if len(series) >= maxSourceSeries {
					return invalid("a source has %d alerts at most, a rule and its labels each", maxSourceSeries)
				}
				// Made below; none of the ledger's firings is of it.
				sid = -int64(len(made)) - 1
				made = append(made, sk)
				series[sk] = sid
			}
			if sid < 0 {
				adds = append(adds, add{rule: id, start: k.start, end: w.end, fresh: int(-sid)})
				continue
			}
			a := at{sid, k.start}
			if i := slices.IndexFunc(had[a], func(h have) bool { return h.end == w.end }); i >= 0 {
				had[a] = slices.Delete(had[a], i, i+1)
				continue
			}
			adds = append(adds, add{rule: id, series: sid, start: k.start, end: w.end})
		}
		sids, err := l.makeSeries(t, made)
		if err != nil {
			return err
		}
		for i, sk := range made {
			series[sk], t.series[sk] = sids[i], sids[i]
		}
		for i := range adds {
			if adds[i].fresh > 0 {
				adds[i].series = sids[adds[i].fresh-1]
			}
		}
		var gone []have
		for _, hs := range had {
			gone = append(gone, hs...)
		}
		out.Added, out.Removed = len(adds), len(gone)
		now := time.Now().Unix()
		if out.Changed() {
			del, err := t.deleting("firings")
			if err != nil {
				return err
			}
			for _, h := range gone {
				if err := del.add(h.id); err != nil {
					del.close()
					return err
				}
			}
			if err := del.close(); err != nil {
				return err
			}
			ins, err := t.inserting("firings", []string{"series_id", "rule_id", "start_at", "end_at", "day"}, []string{"BIGINT", "BIGINT", "BIGINT", "BIGINT", "BIGINT"})
			if err != nil {
				return err
			}
			// In order, so a series' firings keep their ids in time.
			slices.SortFunc(adds, func(a, b add) int {
				return cmp.Or(cmp.Compare(a.series, b.series), cmp.Compare(a.start, b.start))
			})
			for _, a := range adds {
				if err := ins.add(a.series, a.rule, a.start, a.end, l.day(a.start)); err != nil {
					ins.close()
					return err
				}
			}
			if err := ins.close(); err != nil {
				return err
			}
			// The stones of what changed, of the incidents near it, and of a
			// series' next firing, which may have refired after one taken
			// away.
			// As far as a changed firing reached: the next firing of its
			// alert may have refired after it, or no longer.
			hi := to.Unix()
			for _, a := range adds {
				hi = max(hi, a.end.Int64)
			}
			for _, h := range gone {
				hi = max(hi, h.end.Int64)
			}
			// But no further than a period past the period, as an incident
			// written down reaches no further than its 31 days: one firing
			// sent with a far end would otherwise judge the whole ledger
			// again, each time. The next firing of an alert that ran on
			// longer is judged again when a write or a run reaches it.
			hi = min(hi, to.Unix()+int64(MaxRecordedPeriod/time.Second))
			lead, refire := int64(o.Lead/time.Second), int64(o.RefireWithin/time.Second)
			if err := l.judge(t, from.Unix()-lead, hi+refire, o); err != nil {
				return err
			}
			if err := change(t, by, "firings", map[string]any{"source": source, "from": from.UTC(), "to": to.UTC(), "removed": out.Removed},
				map[string]any{"source": source, "firings": len(fs), "added": out.Added}); err != nil {
				return err
			}
		}
		t.recorded = out.Changed()
		// An alarm's history, copied in, is the alarm's: named as it is.
		if out.Changed() {
			list, _, err := l.latestTies(t.ctx, t)
			if err != nil {
				return err
			}
			if _, err := l.tie(t, list); err != nil {
				return err
			}
		}
		// A period may run on past now, a job sending today's; the source
		// has written up to now, not to its end.
		end := max(from.Unix(), min(to.Unix(), now))
		if err := l.copied(t, source, from.Unix(), end); err != nil {
			return err
		}
		// ?7 is a bool: 1 or 0 to Postgres (CheckNamedValue).
		_, err = t.ExecContext(t.ctx, `INSERT INTO sources (name, written, written_by, period_from, period_to, firings, changed)
			VALUES (?1, ?2, ?3, ?4, ?5, ?6, CASE WHEN `+t.d.pick("?7", "?7 = 1")+` THEN `+t.d.pick("?2", "CAST(?2 AS BIGINT)")+` ELSE 0 END)
			ON CONFLICT (name) DO UPDATE SET written = excluded.written, written_by = excluded.written_by,
				period_from = CASE WHEN excluded.period_to >= sources.period_to THEN excluded.period_from ELSE sources.period_from END,
				firings = CASE WHEN excluded.period_to >= sources.period_to THEN excluded.firings ELSE sources.firings END,
				period_to = `+t.d.greatest("sources.period_to", "excluded.period_to")+`,
				changed = CASE WHEN `+t.d.pick("?7", "?7 = 1")+` THEN excluded.written ELSE sources.changed END`,
			source, now, by, from.Unix(), end, len(fs), out.Changed())
		return err
	})
	return out, err
}

// maxSpans is how many spans of a source's copies are kept: enough for
// as many gaps, the earliest let go.
const maxSpans = 64

// copied keeps the periods a source's firings were copied in for, as spans
// run together where they meet or overlap, the latest maxSpans, in the
// meta table (copied.SOURCE): what its copies cover, without a gap, is the
// latest span (Coverage). A source written before they were kept starts
// from its last period.
func (l *Ledger) copied(t *txn, source string, from, to int64) error {
	spans, err := l.spans(t.ctx, t, source)
	if err != nil {
		return err
	}
	spans = append(spans, [2]int64{from, to})
	slices.SortFunc(spans, func(a, b [2]int64) int { return cmp.Compare(a[0], b[0]) })
	merged := spans[:1]
	for _, s := range spans[1:] {
		if last := &merged[len(merged)-1]; s[0] <= last[1] {
			last[1] = max(last[1], s[1])
		} else {
			merged = append(merged, s)
		}
	}
	merged = merged[max(0, len(merged)-maxSpans):]
	b, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	return setMeta(t.ctx, t, "copied."+source, string(b))
}

// spans are the spans of a source's copies (copied), the earliest first.
func (l *Ledger) spans(ctx context.Context, q querier, source string) ([][2]int64, error) {
	v, err := l.meta(ctx, q, "copied."+source)
	if err != nil {
		return nil, err
	}
	var spans [][2]int64
	if v != "" {
		return spans, json.Unmarshal([]byte(v), &spans)
	}
	var from, to int64
	switch err := q.QueryRowContext(ctx, "SELECT period_from, period_to FROM sources WHERE name = ?", source).Scan(&from, &to); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return [][2]int64{{from, to}}, nil
}

// Coverage is what a source's copies cover without a gap, up to the end of
// the latest period copied in: from the start of the latest span. Zero for
// a source never written.
func (l *Ledger) Coverage(ctx context.Context, source string) (from, to time.Time, err error) {
	spans, err := l.spans(ctx, l.r, source)
	if err != nil || len(spans) == 0 {
		return time.Time{}, time.Time{}, err
	}
	last := spans[len(spans)-1]
	return unix(last[0]), unix(last[1]), nil
}

// checkRecorded checks a write of a source's firings before it is made, at
// now.
func checkRecorded(source string, from, to time.Time, fs []firing.Firing, now time.Time) error {
	ahead := now.Add(skew)
	switch {
	case !ValidSource(source):
		return invalid("a source is lowercase letters, digits and dashes, 40 at most, such as cloudwatch-prod")
	case from.IsZero() || to.IsZero():
		return invalid("say which period the firings are of: from and to, as RFC 3339 times")
	case !to.After(from):
		return invalid("to must be after from")
	case to.Sub(from) > MaxRecordedPeriod:
		return invalid("a period is 31 days at most")
	case !sane(from) || !sane(to):
		return invalid("a period is from 1970 to 9999")
	case from.After(ahead):
		return invalid("a period can't start in the future")
	case len(fs) > MaxRecorded:
		return invalid("one write holds %d firings at most: send a shorter period", MaxRecorded)
	}
	for i, f := range fs {
		n := i + 1
		switch {
		case f.Rule == "" || utf8.RuneCountInString(f.Rule) > maxRuleName || !utf8.ValidString(f.Rule):
			return invalid("firing %d: a rule's name is 1 to 512 characters", n)
		case !plain(f.Rule):
			return invalid("firing %d: a rule's name has a character that doesn't show, such as a line break or a zero-width space", n)
		case f.Start.Before(from) || !f.Start.Before(to):
			return invalid("firing %d starts at %s, outside the period: send it with a period it starts in", n, f.Start.UTC().Format(time.RFC3339))
		case f.Start.After(ahead):
			return invalid("firing %d starts in the future", n)
		case !f.End.IsZero() && f.End.Before(f.Start):
			return invalid("firing %d ends before it starts", n)
		case !f.End.IsZero() && !sane(f.End):
			return invalid("firing %d: its end is from 1970 to 9999", n)
		case f.End.After(ahead):
			return invalid("firing %d ends in the future: send it without an end while it is on", n)
		case len(f.Labels) > 32:
			return invalid("firing %d has more than 32 labels", n)
		}
		size := 0
		for k, v := range f.Labels {
			if k == "" || !utf8.ValidString(k) || !utf8.ValidString(v) {
				return invalid("firing %d: a label needs a name, and labels are UTF-8", n)
			}
			if !plain(k) || k == "severity" && !plain(v) {
				return invalid("firing %d: a label's name, or its severity, has a character that doesn't show", n)
			}
			size += len(k) + len(v) + 6
		}
		if size > maxLabelBytes {
			return invalid("firing %d: its labels are 4 KB at most in all", n)
		}
	}
	return nil
}

// recordedRule is the id of a source's rule, made if there is none, when
// room says there is room for one. A new one is named as the source names
// it when no other rule has that name; otherwise it is told apart,
// "checkout-5xx [cloudwatch-prod]".
func (l *Ledger) recordedRule(t *txn, source string, k recordedKey, room func() error) (id int64, made bool, err error) {
	err = t.QueryRowContext(t.ctx, qRecordedRule, source, k.sent, k.severity).Scan(&id)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return id, false, err
	}
	if err := room(); err != nil {
		return 0, false, err
	}
	name, err := l.freeName(t, source, k, nil)
	if err != nil {
		return 0, false, err
	}
	if err := t.QueryRowContext(t.ctx, "INSERT INTO rules (name, severity, source, sent) VALUES (?, ?, ?, ?) RETURNING id",
		name, k.severity, source, k.sent).Scan(&id); err != nil {
		return 0, false, err
	}
	t.rules[name] = id
	return id, true, nil
}

// freeName is the first name for a source's rule that no rule has, no
// version of the rules names, no note, task or comment is about, and
// claimed doesn't hold: its own; told apart by its source, or by its
// severity when the source has the name at another; then by both. A note
// or a task can be written for a rule before it is: a rule copied in
// doesn't take its name, to carry the note off when the rule comes.
func (l *Ledger) freeName(t *txn, source string, k recordedKey, claimed map[string]bool) (string, error) {
	var twin bool
	if err := t.QueryRowContext(t.ctx, qRecordedTwin, source, k.sent, k.severity).Scan(&twin); err != nil {
		return "", err
	}
	versioned, err := t.versionNames()
	if err != nil {
		return "", err
	}
	tries := []string{k.sent}
	if !twin || k.severity == "" {
		tries = append(tries, fmt.Sprintf("%s [%s]", k.sent, source))
	}
	if k.severity != "" {
		if twin {
			tries = append(tries, fmt.Sprintf("%s [severity=%s]", k.sent, k.severity))
		}
		tries = append(tries, fmt.Sprintf("%s [%s, severity=%s]", k.sent, source, k.severity))
	}
	for i := 2; i < 1000; i++ {
		tries = append(tries, fmt.Sprintf("%s [%s %d]", k.sent, source, i))
	}
	for _, name := range tries {
		if claimed[name] || versioned[name] {
			continue
		}
		var taken bool
		if err := t.QueryRowContext(t.ctx, `SELECT EXISTS (SELECT 1 FROM rules WHERE name = ?1)
			OR EXISTS (SELECT 1 FROM notes WHERE rule = ?1) OR EXISTS (SELECT 1 FROM tasks WHERE rule = ?1)
			OR EXISTS (SELECT 1 FROM comments WHERE on_kind = 'rule' AND on_key = ?1)`, name).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return name, nil
		}
	}
	return "", fmt.Errorf("no free name for %s's rule %q", source, k.sent)
}

// yield gives the names a replay judges rules under to the replayed rules:
// a recorded rule holding one is named apart, and what people kept about it
// under that name goes with it; but for one an alarm's history holds,
// which is the alarm's (kept).
func (l *Ledger) yield(t *txn, names []string, kept map[string]bool) error {
	if len(names) == 0 {
		return nil
	}
	rows, err := t.QueryContext(t.ctx, "SELECT id, name, source, sent, severity FROM rules WHERE source != ''")
	if err != nil {
		return err
	}
	type rec struct {
		id                           int64
		name, source, sent, severity string
	}
	var held []rec
	claimed := make(map[string]bool, len(names))
	for _, n := range names {
		claimed[n] = true
	}
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.name, &r.source, &r.sent, &r.severity); err != nil {
			rows.Close()
			return err
		}
		if claimed[r.name] && !kept[r.name] {
			held = append(held, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range held {
		name, err := l.freeName(t, r.source, recordedKey{r.sent, r.severity}, claimed)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE rules SET name = ? WHERE id = ?", name, r.id); err != nil {
			return err
		}
		for _, q := range []string{
			t.d.pick("UPDATE OR IGNORE notes SET rule = ? WHERE rule = ?",
				"UPDATE notes SET rule = ?1 WHERE rule = ?2 AND NOT EXISTS (SELECT 1 FROM notes n WHERE n.rule = ?1)"),
			"UPDATE tasks SET rule = ? WHERE rule = ?",
			"UPDATE comments SET on_key = ? WHERE on_kind = 'rule' AND on_key = ?",
		} {
			if _, err := t.ExecContext(t.ctx, q, name, r.name); err != nil {
				return err
			}
		}
		if t.gone == nil {
			t.gone = map[string]bool{}
		}
		t.gone[r.name] = true
		delete(t.rules, r.name)
		t.rules[name] = r.id
		t.all = true
		t.recorded = true
	}
	return nil
}

// SharedLabels are, for each recorded rule, the labels all its alerts have
// alike: what a team owns it by, as a replayed rule is owned by its
// labels.
func (l *Ledger) SharedLabels(ctx context.Context) (map[string]map[string]string, error) {
	at := l.recorded.Load()
	l.shared.Lock()
	defer l.shared.Unlock()
	if l.shared.m != nil && l.shared.at == at {
		return l.shared.m, nil
	}
	// Each label and value counted once over the alerts, beside how many
	// alerts the rule has: those all have.
	rows, err := l.r.QueryContext(ctx, `WITH n AS (SELECT s.rule_id, count(*) AS alerts FROM series s JOIN rules r ON r.id = s.rule_id
			WHERE r.source != '' GROUP BY s.rule_id),
		kv AS (SELECT s.rule_id, j.key, j.value, count(*) AS alerts FROM n JOIN series s ON s.rule_id = n.rule_id, `+l.d.keys("s.labels", "j")+`
			GROUP BY s.rule_id, j.key, j.value)
		SELECT r.name, kv.key, kv.value FROM kv JOIN n ON n.rule_id = kv.rule_id AND n.alerts = kv.alerts JOIN rules r ON r.id = kv.rule_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]map[string]string{}
	for rows.Next() {
		var rule, k, v string
		if err := rows.Scan(&rule, &k, &v); err != nil {
			return nil, err
		}
		if m[rule] == nil {
			m[rule] = map[string]string{}
		}
		m[rule][k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	l.shared.at, l.shared.m = at, m
	return m, nil
}

// Sources are the sources firings are recorded from, by name.
func (l *Ledger) Sources(ctx context.Context) ([]Source, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT s.name, s.written, s.written_by, s.period_from, s.period_to, s.firings, s.changed,
		(SELECT count(*) FROM rules r WHERE r.source = s.name AND r.source != '') FROM sources s ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var s Source
		var written, from, to, changed int64
		if err := rows.Scan(&s.Name, &written, &s.By, &from, &to, &s.Firings, &changed, &s.Rules); err != nil {
			return nil, err
		}
		s.Written, s.From, s.To = unix(written), unix(from), unix(to)
		if changed > 0 {
			s.Changed = unix(changed)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
