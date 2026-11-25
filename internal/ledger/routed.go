package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Who a firing reached, worked out and kept (schema 035, docs/design/routing.md):
// the alert manager's routing run over the firings the ledger has, a run's
// hours at a time. Nothing here is recorded: the notifications really sent
// are kept apart (notifications), and a page that shows these says which
// kind it shows.

// MaxRoutedPages is how many page times a firing keeps: past them it keeps
// the count alone.
const MaxRoutedPages = 200

// RoutedNotification is a notification the model worked out, on one
// integration of its receiver.
type RoutedNotification struct {
	At             time.Time
	Receiver, Kind string
	Page           bool
	Reason, Group  string
	// Firings is how many firings it told of.
	Firings int
}

// RoutedReceiver is what a firing reached on a receiver's integrations of
// one kind: the notifications that told of it and the pages among them. A
// receiver that sends nothing is one with Empty.
type RoutedReceiver struct {
	Receiver string `json:"receiver"`
	Kind     string `json:"kind,omitempty"`
	Notified int    `json:"notified,omitempty"`
	Paged    int    `json:"paged,omitempty"`
	Empty    bool   `json:"empty,omitempty"`
}

// RoutedPageAt is a page: when it went, and to which receiver.
type RoutedPageAt struct {
	At       time.Time
	Receiver string
}

// RoutedMute is what kept a firing out of a notification: Kind is
// inhibition, silence, mute_time_interval or active_time_interval.
type RoutedMute struct {
	Kind       string `json:"kind"`
	By         string `json:"by"`
	SourceRule string `json:"source_rule,omitempty"`
}

// RoutedFiring is what the model worked out for a firing.
type RoutedFiring struct {
	FiringID int64
	// Notified are the notifications that told of it, Paged the pages
	// among them, FirstPage when the first went (zero for none) and
	// PageAts when and to which receiver each did, at most MaxRoutedPages
	// of them.
	Notified, Paged int
	// Grouped says a notification that told of it told of another rule's
	// alert as well.
	Grouped   bool
	FirstPage time.Time
	PageAts   []RoutedPageAt
	Receivers []RoutedReceiver
	Muted     []RoutedMute
}

// RoutedInterval is a stretch of time a run tried to work the notifications
// of out: Worked says it did. Why is, for one that wasn't (the work stopped,
// failed, the configuration couldn't be read, or its firings were recorded
// again), what went wrong, and for one worked out a caveat ("" for none),
// kept whole.
type RoutedInterval struct {
	From, To time.Time
	Worked   bool
	Why      string
}

// RoutedCover is the stretches tried, the earliest first, none overlapping
// another.
type RoutedCover []RoutedInterval

// RoutedCover reads which hours were worked out and which weren't.
func (l *Ledger) RoutedCover(ctx context.Context) (RoutedCover, error) {
	var out RoutedCover
	err := l.eachRow(ctx, func(r scanner) error {
		var from, to, ok int64
		var x RoutedInterval
		if err := r.Scan(&from, &to, &ok, &x.Why); err != nil {
			return err
		}
		x.From, x.To, x.Worked = unix(from), unix(to), ok != 0
		out = append(out, x)
		return nil
	}, "SELECT from_at, to_at, ok, why FROM routed_cover ORDER BY from_at")
	return out, err
}

// Any reports whether some stretch was worked out.
func (c RoutedCover) Any() bool {
	return slices.ContainsFunc(c, func(x RoutedInterval) bool { return x.Worked })
}

// First is where the first stretch worked out starts, and Last where the
// latest ends: ok is false when none was.
func (c RoutedCover) First() (t time.Time, ok bool) {
	for _, x := range c {
		if x.Worked {
			return x.From, true
		}
	}
	return time.Time{}, false
}

// Last is where the latest stretch worked out ends.
func (c RoutedCover) Last() (t time.Time, ok bool) {
	for i := len(c) - 1; i >= 0; i-- {
		if c[i].Worked {
			return c[i].To, true
		}
	}
	return time.Time{}, false
}

// Covers reports whether every moment of [from, to) is in a stretch worked
// out: an hour not tried, or one that was and wasn't worked out, is not.
// An empty span is covered.
func (c RoutedCover) Covers(from, to time.Time) bool {
	return len(c.Gaps(from, to)) == 0
}

// Gaps are the parts of [from, to) no stretch worked out covers, in order.
func (c RoutedCover) Gaps(from, to time.Time) [][2]time.Time {
	var out [][2]time.Time
	at := from
	for _, x := range c {
		if !x.Worked || !x.To.After(at) {
			continue
		}
		if !x.From.Before(to) {
			break
		}
		if x.From.After(at) {
			out = append(out, [2]time.Time{at, x.From})
		}
		at = x.To
		if !at.Before(to) {
			return out
		}
	}
	if at.Before(to) {
		out = append(out, [2]time.Time{at, to})
	}
	return out
}

// Unknown is why the notifications of some of [from, to) are not known: the
// text of the first stretch in it that was tried and wasn't worked out, or
// one worked out with a caveat; "" for none.
func (c RoutedCover) Unknown(from, to time.Time) string {
	for _, x := range c {
		if x.Why != "" && x.From.Before(to) && x.To.After(from) {
			return x.Why
		}
	}
	return ""
}

// setCover makes [from, to) one stretch, over what was there.
func (l *Ledger) setCover(t *txn, from, to time.Time, worked bool, why string) error {
	f, e := from.Unix(), to.Unix()
	if e <= f {
		return nil
	}
	type iv struct {
		from, to int64
		ok       int64
		why      string
	}
	var over []iv
	rows, err := t.QueryContext(t.ctx, "SELECT from_at, to_at, ok, why FROM routed_cover WHERE to_at >= ? AND from_at <= ? ORDER BY from_at", f, e)
	if err != nil {
		return err
	}
	for rows.Next() {
		var x iv
		if err := rows.Scan(&x.from, &x.to, &x.ok, &x.why); err != nil {
			rows.Close()
			return err
		}
		over = append(over, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	ok := int64(0)
	if worked {
		ok = 1
	}
	now := iv{f, e, ok, why}
	var keep []iv
	for _, x := range over {
		if _, err := t.ExecContext(t.ctx, "DELETE FROM routed_cover WHERE from_at = ?", x.from); err != nil {
			return err
		}
		switch {
		case x.to == f || x.from == e:
			// Touching: joined below when alike.
			if x.ok == now.ok && x.why == now.why {
				now.from, now.to = min(now.from, x.from), max(now.to, x.to)
			} else {
				keep = append(keep, x)
			}
		default:
			if x.from < f {
				keep = append(keep, iv{x.from, f, x.ok, x.why})
			}
			if x.to > e {
				keep = append(keep, iv{e, x.to, x.ok, x.why})
			}
		}
	}
	for _, x := range append(keep, now) {
		if _, err := t.ExecContext(t.ctx, "INSERT INTO routed_cover (from_at, to_at, ok, why) VALUES (?, ?, ?, ?)", x.from, x.to, x.ok, x.why); err != nil {
			return err
		}
	}
	return nil
}

// trimCover drops stretches of the cover before a time, cutting the
// one that crosses it: with the rows they were for.
func (l *Ledger) trimCover(t *txn, before int64) error {
	if _, err := t.ExecContext(t.ctx, "DELETE FROM routed_cover WHERE to_at <= ?", before); err != nil {
		return err
	}
	_, err := t.ExecContext(t.ctx, "UPDATE routed_cover SET from_at = ? WHERE from_at < ?", before, before)
	return err
}

// MarkRoutedUnknown records that the notifications of [from, to) are not
// known, and why: the work stopped or failed, the routing configuration
// couldn't be read, or the firings were recorded again. What was kept of
// those hours is dropped (the notifications there, and what the firings
// that were on in them reached), so that no page reads it as none.
func (l *Ledger) MarkRoutedUnknown(ctx context.Context, from, to time.Time, why string) error {
	if why == "" {
		why = "not worked out"
	}
	return l.write(ctx, func(t *txn) error {
		if _, err := t.ExecContext(t.ctx, "DELETE FROM routed WHERE at >= ? AND at <= ?", from.Unix(), to.Unix()); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, `DELETE FROM firing_routed WHERE firing_id IN (SELECT id FROM firings WHERE start_at <= ? AND (end_at IS NULL OR end_at >= ?))`,
			to.Unix(), from.Unix()); err != nil {
			return err
		}
		return l.setCover(t, from, to, false, why)
	})
}

// FiringsOverlapping are the firings that were on at some time in
// [from, to]: started by to, and not ended before from, with their labels;
// those on at some time in [own, to] first, the earliest first in each, at
// most limit of them: when there are more, it is those that began before own
// and ended before it that are left out, the earliest of them first.
// more says there were more.
func (l *Ledger) FiringsOverlapping(ctx context.Context, from, to, own time.Time, limit int) (out []Firing, more bool, err error) {
	rows, err := l.r.QueryContext(ctx, `SELECT f.id, r.name, s.id, s.labels, f.start_at, f.end_at
		FROM firings f JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id
		WHERE f.start_at <= ? AND (f.end_at IS NULL OR f.end_at >= ?)
		ORDER BY CASE WHEN f.end_at IS NULL OR f.end_at >= ? THEN 0 ELSE 1 END, f.start_at, f.id LIMIT ?`, to.Unix(), from.Unix(), own.Unix(), limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var f Firing
		var sid, start int64
		var end sql.NullInt64
		var labels string
		if err := rows.Scan(&f.ID, &f.Rule, &sid, &labels, &start, &end); err != nil {
			return nil, false, err
		}
		if f.Labels, err = l.labels(sid, labels); err != nil {
			return nil, false, err
		}
		f.Series = sid
		f.Start, f.End = unix(start), fromNull(end)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// KeepRouted keeps what a run worked out of the hours from [from, to]:
// the notifications, in place of those kept in them, and what each of the
// firings reached, in place of what was kept for them. [from, to) is then
// worked out, with a caveat when there is one (why: kept whole).
func (l *Ledger) KeepRouted(ctx context.Context, from, to time.Time, why string, ns []RoutedNotification, fs []RoutedFiring) error {
	return l.write(ctx, func(t *txn) error {
		if _, err := t.ExecContext(t.ctx, "DELETE FROM routed WHERE at >= ? AND at <= ?", from.Unix(), to.Unix()); err != nil {
			return err
		}
		ins, err := t.inserting("routed", []string{"at", "receiver", "kind", "page", "reason", "grp", "firings"}, []string{"BIGINT", "TEXT", "TEXT", "BIGINT", "TEXT", "TEXT", "BIGINT"})
		if err != nil {
			return err
		}
		for _, n := range ns {
			page := int64(0)
			if n.Page {
				page = 1
			}
			if err := ins.add(n.At.Unix(), n.Receiver, n.Kind, page, n.Reason, n.Group, int64(n.Firings)); err != nil {
				ins.close()
				return err
			}
		}
		if err := ins.close(); err != nil {
			return err
		}
		for part := range slices.Chunk(fs, 400) {
			marks := make([]string, len(part))
			args := make([]any, len(part))
			for i, f := range part {
				marks[i], args[i] = "?", f.FiringID
			}
			if _, err := t.ExecContext(t.ctx, "DELETE FROM firing_routed WHERE firing_id IN ("+strings.Join(marks, ", ")+")", args...); err != nil {
				return err
			}
		}
		fin, err := t.inserting("firing_routed", []string{"firing_id", "notified", "paged", "first_page", "grouped", "page_ats", "receivers", "muted"},
			[]string{"BIGINT", "BIGINT", "BIGINT", "BIGINT", "BIGINT", "TEXT", "TEXT", "TEXT"})
		if err != nil {
			return err
		}
		for _, f := range fs {
			ats := make([][2]any, 0, min(len(f.PageAts), MaxRoutedPages))
			for _, a := range f.PageAts[:min(len(f.PageAts), MaxRoutedPages)] {
				ats = append(ats, [2]any{a.At.Unix(), a.Receiver})
			}
			a, _ := json.Marshal(ats)
			rc, _ := json.Marshal(nonNil(f.Receivers))
			mu, _ := json.Marshal(nonNil(f.Muted))
			first := int64(0)
			if !f.FirstPage.IsZero() {
				first = f.FirstPage.Unix()
			}
			grouped := int64(0)
			if f.Grouped {
				grouped = 1
			}
			if err := fin.add(f.FiringID, int64(f.Notified), int64(f.Paged), first, grouped, string(a), string(rc), string(mu)); err != nil {
				fin.close()
				return err
			}
		}
		if err := fin.close(); err != nil {
			return err
		}
		return l.setCover(t, from, to, true, why)
	})
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ForgetRouted drops everything worked out, and the span: for a ledger
// whose routing is no longer given (--notifications=false is only a view,
// and keeps it), or to be worked out again.
func (l *Ledger) ForgetRouted(ctx context.Context) error {
	return l.write(ctx, func(t *txn) error {
		for _, q := range []string{"DELETE FROM firing_routed", "DELETE FROM routed", "DELETE FROM routed_cover"} {
			if _, err := t.ExecContext(t.ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
}

// RoutedOf is what was worked out for each of the firings given, by firing
// id; one worked out of none isn't there.
func (l *Ledger) RoutedOf(ctx context.Context, ids []int64) (map[int64]RoutedFiring, error) {
	out := map[int64]RoutedFiring{}
	for part := range slices.Chunk(ids, 400) {
		marks := make([]string, len(part))
		args := make([]any, len(part))
		for i, id := range part {
			marks[i], args[i] = "?", id
		}
		rows, err := l.r.QueryContext(ctx, `SELECT firing_id, notified, paged, first_page, grouped, page_ats, receivers, muted FROM firing_routed
			WHERE firing_id IN (`+strings.Join(marks, ", ")+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			f, err := scanRouted(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[f.FiringID] = f
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// pageAts reads the page times a firing keeps: a JSON list of [Unix
// seconds, receiver].
func pageAts(text string) ([]RoutedPageAt, error) {
	var raw []struct {
		At       int64
		Receiver string
	}
	var pairs [][2]json.RawMessage
	if err := json.Unmarshal([]byte(text), &pairs); err != nil {
		return nil, err
	}
	raw = make([]struct {
		At       int64
		Receiver string
	}, len(pairs))
	for i, p := range pairs {
		if err := json.Unmarshal(p[0], &raw[i].At); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(p[1], &raw[i].Receiver); err != nil {
			return nil, err
		}
	}
	var out []RoutedPageAt
	for _, r := range raw {
		out = append(out, RoutedPageAt{unix(r.At), r.Receiver})
	}
	return out, nil
}

func scanRouted(r scanner) (RoutedFiring, error) {
	var f RoutedFiring
	var first, grouped int64
	var ats, rc, mu string
	if err := r.Scan(&f.FiringID, &f.Notified, &f.Paged, &first, &grouped, &ats, &rc, &mu); err != nil {
		return f, err
	}
	f.Grouped = grouped != 0
	if first > 0 {
		f.FirstPage = unix(first)
	}
	var err error
	if f.PageAts, err = pageAts(ats); err != nil {
		return f, fmt.Errorf("firing %d's page times: %w", f.FiringID, err)
	}
	if err := json.Unmarshal([]byte(rc), &f.Receivers); err != nil {
		return f, fmt.Errorf("firing %d's receivers: %w", f.FiringID, err)
	}
	if err := json.Unmarshal([]byte(mu), &f.Muted); err != nil {
		return f, fmt.Errorf("firing %d's mutes: %w", f.FiringID, err)
	}
	return f, nil
}

// RoutedPage is a firing the model worked out pages for: its rule and
// stones, and when its pages went.
type RoutedPage struct {
	FiringID int64
	Rule     string
	Start    time.Time
	// Black is a firing that caught nothing (not white, not excused);
	// Useful is its usefulness as responses worked it out: 1, 0, or -1 for
	// not known.
	Black    bool
	Useful   int
	Notified int
	Paged    int
	PageAts  []RoutedPageAt
}

// RoutedPages are the firings that were on at some time in [from, to) and
// were worked out to have a notification, with the worked-out firings'
// stones, the earliest start first; at most limit of them, more saying
// there were more.
func (l *Ledger) RoutedPages(ctx context.Context, from, to time.Time, limit int) (out []RoutedPage, more bool, err error) {
	rows, err := l.r.QueryContext(ctx, `SELECT f.id, r.name, f.start_at, f.white, f.excused, f.useful, x.notified, x.paged, x.page_ats
		FROM firing_routed x JOIN firings f ON f.id = x.firing_id JOIN rules r ON r.id = f.rule_id
		WHERE x.notified > 0 AND f.start_at < ? AND (f.end_at IS NULL OR f.end_at >= ?)
		ORDER BY f.start_at, f.id LIMIT ?`, to.Unix(), from.Unix(), limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var p RoutedPage
		var start, useful int64
		var white, excused bool
		var ats string
		if err := rows.Scan(&p.FiringID, &p.Rule, &start, &white, &excused, &useful, &p.Notified, &p.Paged, &ats); err != nil {
			return nil, false, err
		}
		p.Start, p.Black, p.Useful = unix(start), !white && !excused, int(useful)
		if p.PageAts, err = pageAts(ats); err != nil {
			return nil, false, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// RoutedCount is the notifications worked out for a receiver over a span,
// and the pages among them.
type RoutedCount struct {
	Notifications, Pages int
}

// RoutedByReceiver are the notifications worked out that went in
// [from, to), counted by receiver.
func (l *Ledger) RoutedByReceiver(ctx context.Context, from, to time.Time) (map[string]RoutedCount, error) {
	out := map[string]RoutedCount{}
	err := l.eachRow(ctx, func(r scanner) error {
		var name string
		var c RoutedCount
		if err := r.Scan(&name, &c.Notifications, &c.Pages); err != nil {
			return err
		}
		out[name] = c
		return nil
	}, "SELECT receiver, count(*), coalesce(sum(page), 0) FROM routed WHERE at >= ? AND at < ? GROUP BY receiver", from.Unix(), to.Unix())
	return out, err
}

// RecordedCount is the notifications recorded for a receiver as it is
// named in the records, and when the first came.
type RecordedCount struct {
	Notifications int
	First         time.Time
}

// RecordedByReceiver are the notifications recorded that came in
// [from, to), counted by the receiver's name as recorded (an event the
// alert manager's recorder posts names the integration as well:
// receiver/kind, receiver/kind[1]).
func (l *Ledger) RecordedByReceiver(ctx context.Context, from, to time.Time) (map[string]RecordedCount, error) {
	out := map[string]RecordedCount{}
	err := l.eachRow(ctx, func(r scanner) error {
		var name string
		var n, first int64
		if err := r.Scan(&name, &n, &first); err != nil {
			return err
		}
		out[name] = RecordedCount{Notifications: int(n), First: unix(first)}
		return nil
	}, "SELECT receiver, count(*), min(at) FROM notifications WHERE at >= ? AND at < ? GROUP BY receiver", from.Unix(), to.Unix())
	return out, err
}

// RecordedFirst is when the first notification kept for a receiver came,
// by its name as recorded.
func (l *Ledger) RecordedFirst(ctx context.Context) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	err := l.eachRow(ctx, func(r scanner) error {
		var name string
		var first int64
		if err := r.Scan(&name, &first); err != nil {
			return err
		}
		out[name] = unix(first)
		return nil
	}, "SELECT receiver, min(at) FROM notifications GROUP BY receiver")
	return out, err
}

// RoutedPagesByWeek are the pages worked out for the firings that started
// in [from, to), by the week each started in and by rule, those a quiet
// window excused left out: the number a budget of pages counts.
func (l *Ledger) RoutedPagesByWeek(ctx context.Context, from, to time.Time) (map[int64]map[string]int, error) {
	out := map[int64]map[string]int{}
	err := l.eachRow(ctx, func(r scanner) error {
		var name string
		var day, n int64
		if err := r.Scan(&name, &day, &n); err != nil {
			return err
		}
		w := Week(day)
		if out[w] == nil {
			out[w] = map[string]int{}
		}
		out[w][name] += int(n)
		return nil
	}, `SELECT r.name, f.day, sum(x.paged) FROM firing_routed x JOIN firings f ON f.id = x.firing_id JOIN rules r ON r.id = f.rule_id
		WHERE f.start_at >= ? AND f.start_at < ? AND f.excused = 0 AND x.paged > 0 GROUP BY r.name, f.day`, from.Unix(), to.Unix())
	return out, err
}

// RoutedTotals are the notifications worked out that went in [from, to) and
// told of a firing, counted once each however many firings they told of: how
// many, the pages among them, and how many groups they were for.
func (l *Ledger) RoutedTotals(ctx context.Context, from, to time.Time) (notified, paged, groups int, err error) {
	err = l.r.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(page), 0), count(DISTINCT grp) FROM routed WHERE at >= ? AND at < ? AND firings > 0`,
		from.Unix(), to.Unix()).Scan(&notified, &paged, &groups)
	return
}

// RoutedByReceiverKind are the notifications worked out that went in
// [from, to), counted by receiver and the kind of integration.
func (l *Ledger) RoutedByReceiverKind(ctx context.Context, from, to time.Time) (map[string]map[string]RoutedCount, error) {
	out := map[string]map[string]RoutedCount{}
	err := l.eachRow(ctx, func(r scanner) error {
		var name, kind string
		var c RoutedCount
		if err := r.Scan(&name, &kind, &c.Notifications, &c.Pages); err != nil {
			return err
		}
		if out[name] == nil {
			out[name] = map[string]RoutedCount{}
		}
		out[name][kind] = c
		return nil
	}, "SELECT receiver, kind, count(*), coalesce(sum(page), 0) FROM routed WHERE at >= ? AND at < ? GROUP BY receiver, kind", from.Unix(), to.Unix())
	return out, err
}

// RecordedHours are the hours (Unix seconds of each start) in which a
// notification was recorded in [from, to), by receiver as it is named in
// the records.
func (l *Ledger) RecordedHours(ctx context.Context, from, to time.Time) (map[string]map[int64]bool, error) {
	out := map[string]map[int64]bool{}
	err := l.eachRow(ctx, func(r scanner) error {
		var name string
		var h int64
		if err := r.Scan(&name, &h); err != nil {
			return err
		}
		if out[name] == nil {
			out[name] = map[int64]bool{}
		}
		out[name][h*3600] = true
		return nil
	}, "SELECT DISTINCT receiver, at / 3600 FROM notifications WHERE at >= ? AND at < ?", from.Unix(), to.Unix())
	return out, err
}
