package ledger

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"
)

// Rules left out of a source's copies (docs/design/grafanajudged.md,
// schema/030.sql): a source's copies cover a period for every rule of it
// (Spans), and a day lapilli serve copies in of Grafana's alert state
// history is written for the rules the history can say all of. A rule it
// can't (its name another rule's too, changes of it passed over, a firing
// no write holds) is left out of the day alone, and the period is kept
// here: over it the rule has no record, rather than a quiet one, also once
// it is judged again.

// maxLeftOut is how many periods are kept of one rule: past it, the two
// earliest are run together, the time between them left out too, so that
// nothing left out comes to read as copied.
const maxLeftOut = 64

// maxLeftOutRules is how many rules of one source have periods kept. A
// variable, so tests can make it small.
var maxLeftOutRules = 5000

// ErrLeftOut is a period, a source or a rule LeaveOut doesn't keep.
var ErrLeftOut = errors.New("not a period a rule was left out the ledger keeps")

// ErrLeftOutFull is a rule to leave out of a source whose rules with
// periods kept are as many as are kept (maxLeftOutRules): none of the
// write is kept.
var ErrLeftOutFull = errors.New("as many rules of the source with periods left out as the ledger keeps")

// RuleName reports whether name can name a rule copied in, as PutFirings
// takes one: 1 to 512 characters, all of which show. A rule of another
// name has no firing copied in, so no record to leave it out of.
func RuleName(name string) bool {
	return name != "" && utf8.RuneCountInString(name) <= maxRuleName && plain(name)
}

// LeaveOut keeps that rules, by the names their source sends them by, were
// left out of what was copied in for source over [from, to): a period that
// meets or overlaps one kept of the rule is run together with it. Kept
// already, nothing is written. A rule past as many as have periods kept
// lets go of the periods of the rules no longer among now, the source's
// rules, those whose last period ended earliest first, as many as make
// room; with none to let go of (now nil, the source's rules not known), it
// fails the write, as ErrLeftOutFull.
func (l *Ledger) LeaveOut(ctx context.Context, source string, rules, now []string, from, to time.Time) error {
	switch {
	case !ValidSource(source):
		return fmt.Errorf("%w: not a source's name", ErrLeftOut)
	case !from.Equal(from.Truncate(time.Second)) || !to.Equal(to.Truncate(time.Second)) || !to.After(from) || !sane(from) || !sane(to):
		return fmt.Errorf("%w: a period is of whole seconds, from 1970 to 9999, its end after its start", ErrLeftOut)
	}
	rules = slices.Compact(slices.Sorted(slices.Values(rules)))
	for _, r := range rules {
		if !RuleName(r) {
			return fmt.Errorf("%w: a rule's name is 1 to 512 characters, all of which show", ErrLeftOut)
		}
	}
	if len(rules) == 0 {
		return nil
	}
	// Looked at first apart from a write: a round that leaves the same
	// rules out of the same day again writes nothing.
	if del, add, err := l.leavingOut(ctx, l.r, source, rules, now, from.Unix(), to.Unix()); err != nil || len(del)+len(add) == 0 {
		return err
	}
	return l.write(ctx, func(t *txn) error {
		del, add, err := l.leavingOut(t.ctx, t, source, rules, now, from.Unix(), to.Unix())
		if err != nil {
			return err
		}
		for _, id := range del {
			if _, err := t.ExecContext(t.ctx, "DELETE FROM left_out WHERE id = ?", id); err != nil {
				return err
			}
		}
		for _, a := range add {
			if _, err := t.ExecContext(t.ctx, "INSERT INTO left_out (source, rule, from_at, to_at) VALUES (?, ?, ?, ?)", source, a.rule, a.from, a.to); err != nil {
				return err
			}
		}
		return nil
	})
}

// leftOutRow is a period to keep of a rule.
type leftOutRow struct {
	rule     string
	from, to int64
}

// leavingOut is what keeping [from, to) of rules, sorted, changes in what
// is kept of source: the rows that go, and those that come.
func (l *Ledger) leavingOut(ctx context.Context, q reader, source string, rules, now []string, from, to int64) (del []int64, add []leftOutRow, err error) {
	// How many rules of the source have a period kept, counted when a new
	// one is to be; past as many as are kept, those let go of.
	known := -1
	var gone []string
	for _, rule := range rules {
		rows, err := q.QueryContext(ctx, "SELECT id, from_at, to_at FROM left_out WHERE source = ? AND rule = ? ORDER BY from_at", source, rule)
		if err != nil {
			return nil, nil, err
		}
		type row struct {
			id       int64
			from, to int64
		}
		var have []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.from, &r.to); err != nil {
				rows.Close()
				return nil, nil, err
			}
			have = append(have, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		if len(have) == 0 {
			if known < 0 {
				if err := q.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT DISTINCT rule FROM left_out WHERE source = ?) AS rules", source).Scan(&known); err != nil {
					return nil, nil, err
				}
			}
			if known >= maxLeftOutRules {
				if gone == nil {
					gone = []string{}
					if now != nil {
						if gone, err = l.leftOutGone(ctx, q, source, slices.Concat(rules, now)); err != nil {
							return nil, nil, err
						}
					}
				}
				if len(gone) == 0 {
					return nil, nil, fmt.Errorf("%w: %d", ErrLeftOutFull, maxLeftOutRules)
				}
				ids, err := l.leftOutIDs(ctx, q, source, gone[0])
				if err != nil {
					return nil, nil, err
				}
				del, gone, known = append(del, ids...), gone[1:], known-1
			}
			known++
		}
		// What is kept of the rule with the period: those it meets run
		// together with it, then the earliest, past as many as are kept.
		want := [][2]int64{{from, to}}
		for _, r := range have {
			want = append(want, [2]int64{r.from, r.to})
		}
		want = runTogether(want)
		for len(want) > maxLeftOut {
			want[1][0] = want[0][0]
			want = want[1:]
		}
		for _, r := range have {
			if !slices.Contains(want, [2]int64{r.from, r.to}) {
				del = append(del, r.id)
			}
		}
		for _, w := range want {
			if !slices.ContainsFunc(have, func(r row) bool { return r.from == w[0] && r.to == w[1] }) {
				add = append(add, leftOutRow{rule, w[0], w[1]})
			}
		}
	}
	return del, add, nil
}

// leftOutGone are the rules of source with periods kept that aren't among
// keep, those whose last period ended earliest first.
func (l *Ledger) leftOutGone(ctx context.Context, q reader, source string, keep []string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT rule, max(to_at) AS last FROM left_out WHERE source = ? GROUP BY rule ORDER BY last, rule", source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kept := make(map[string]bool, len(keep))
	for _, r := range keep {
		kept[r] = true
	}
	gone := []string{}
	for rows.Next() {
		var rule string
		var last int64
		if err := rows.Scan(&rule, &last); err != nil {
			return nil, err
		}
		if !kept[rule] {
			gone = append(gone, rule)
		}
	}
	return gone, rows.Err()
}

// leftOutIDs are the rows kept of a rule of source.
func (l *Ledger) leftOutIDs(ctx context.Context, q reader, source, rule string) ([]int64, error) {
	rows, err := q.QueryContext(ctx, "SELECT id FROM left_out WHERE source = ? AND rule = ?", source, rule)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// runTogether are spans in time, those that meet or overlap as one.
func runTogether(spans [][2]int64) [][2]int64 {
	slices.SortFunc(spans, func(a, b [2]int64) int { return cmp.Compare(a[0], b[0]) })
	out := spans[:1]
	for _, s := range spans[1:] {
		if last := &out[len(out)-1]; s[0] <= last[1] {
			last[1] = max(last[1], s[1])
		} else {
			out = append(out, s)
		}
	}
	return out
}

// LeftOut are the periods each rule of a source was left out of its
// copies, by the name the source sends the rule by (RuleRow.Sent), the
// earliest first: none for most sources.
func (l *Ledger) LeftOut(ctx context.Context, source string) (map[string][][2]time.Time, error) {
	m, err := l.leftOut(ctx, l.r, source)
	if err != nil || len(m) == 0 {
		return nil, err
	}
	out := make(map[string][][2]time.Time, len(m))
	for rule, ps := range m {
		for _, p := range ps {
			out[rule] = append(out[rule], [2]time.Time{unix(p[0]), unix(p[1])})
		}
	}
	return out, nil
}

func (l *Ledger) leftOut(ctx context.Context, q reader, source string) (map[string][][2]int64, error) {
	rows, err := q.QueryContext(ctx, "SELECT rule, from_at, to_at FROM left_out WHERE source = ? ORDER BY rule, from_at", source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][][2]int64{}
	for rows.Next() {
		var rule string
		var p [2]int64
		if err := rows.Scan(&rule, &p[0], &p[1]); err != nil {
			return nil, err
		}
		out[rule] = append(out[rule], p)
	}
	return out, rows.Err()
}

// leftOutOf are the periods one rule of source, by the name the source
// sends it by, was left out of its copies, the earliest first: its rows
// alone read.
func (l *Ledger) leftOutOf(ctx context.Context, q reader, source, rule string) ([][2]int64, error) {
	rows, err := q.QueryContext(ctx, "SELECT from_at, to_at FROM left_out WHERE source = ? AND rule = ? ORDER BY from_at", source, rule)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]int64
	for rows.Next() {
		var p [2]int64
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Without is spans but for the periods of out, each in time and apart:
// what a source's copies cover of a rule left out of them over out.
func Without(spans, out [][2]time.Time) [][2]time.Time {
	if len(out) == 0 {
		return spans
	}
	secs := func(ts [][2]time.Time) [][2]int64 {
		s := make([][2]int64, len(ts))
		for i, t := range ts {
			s[i] = [2]int64{t[0].Unix(), t[1].Unix()}
		}
		return s
	}
	var left [][2]time.Time
	for _, s := range without(secs(spans), secs(out)) {
		left = append(left, [2]time.Time{unix(s[0]), unix(s[1])})
	}
	return left
}

// without is Without, of Unix seconds.
func without(spans, out [][2]int64) [][2]int64 {
	if len(out) == 0 {
		return spans
	}
	var left [][2]int64
	for _, s := range spans {
		lo := s[0]
		for _, o := range out {
			if o[1] <= lo {
				continue
			}
			if o[0] >= s[1] {
				break
			}
			if o[0] > lo {
				left = append(left, [2]int64{lo, o[0]})
			}
			lo = o[1]
		}
		if lo < s[1] {
			left = append(left, [2]int64{lo, s[1]})
		}
	}
	return left
}

// SpansOf are what a source's copies cover of one of its rules, by the
// name the ledger has it under: Spans, but for the periods the rule was
// left out of them (LeaveOut).
func (l *Ledger) SpansOf(ctx context.Context, source, name string) ([][2]time.Time, error) {
	spans, err := l.Spans(ctx, source)
	if err != nil || len(spans) == 0 {
		return spans, err
	}
	// The name its source sends it by: its own, for a rule none of whose
	// firings was copied in.
	sent := name
	if err := l.r.QueryRowContext(ctx, "SELECT sent FROM rules WHERE name = ? AND source = ?", name, source).Scan(&sent); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	ps, err := l.leftOutOf(ctx, l.r, source, sent)
	if err != nil {
		return nil, err
	}
	out := make([][2]time.Time, len(ps))
	for i, p := range ps {
		out[i] = [2]time.Time{unix(p[0]), unix(p[1])}
	}
	return Without(spans, out), nil
}

// CoverageOf is Coverage of one of a source's rules: what its copies
// cover of it without a gap, and without a period it was left out of them,
// up to the end of the latest. Zero for a source never written.
func (l *Ledger) CoverageOf(ctx context.Context, source, name string) (from, to time.Time, err error) {
	spans, err := l.SpansOf(ctx, source, name)
	if err != nil || len(spans) == 0 {
		return time.Time{}, time.Time{}, err
	}
	last := spans[len(spans)-1]
	return last[0], last[1], nil
}
