package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Who a firing reached, worked out and kept (schema 035, docs/design/routing.md):
// the alert manager's routing run over the firings the ledger has, a run's
// hours at a time. Nothing here is recorded: the notifications really sent
// are kept apart (notifications), and a page that shows these says which
// kind it shows.

// MaxRoutedPages is how many page times RoutedOf gives for a firing, to
// show: the count is of all of them.
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

// RoutedSent is a notification that told of a firing, as a run worked it out.
type RoutedSent struct {
	At             time.Time
	Receiver, Kind string
	Page           bool
}

// RoutedFiring is what the model worked out for a firing. What KeepRouted
// is given of it is Sent, the notifications of the hours the run worked
// out, and what it says besides (Grouped, the receivers that send nothing,
// Muted). What RoutedOf gives back is counted, when it is read, from the
// notifications kept for the firing in the hours worked out: those of every
// run that worked out some of its life; nothing is counted when it is
// written, so that no missing row is a count of none.
type RoutedFiring struct {
	FiringID int64
	// Notified are the notifications that told of it, Paged the pages
	// among them and FirstPage when the first went (zero for none): all of
	// them, however many. PageAts is a sample for display, the first
	// MaxRoutedPages page times with their receivers: no number is counted
	// from it (EachRoutedPage gives every page).
	Notified, Paged int
	// Grouped says a notification that told of it told of another rule's
	// alert as well.
	Grouped   bool
	FirstPage time.Time
	PageAts   []RoutedPageAt
	Receivers []RoutedReceiver
	Muted     []RoutedMute
	// Sent are, for KeepRouted, its notifications in [from, to]. Whole says
	// the run saw the firing from its start, so that what it says of the
	// firing's mutes and receivers that send nothing replaces what was
	// kept; otherwise it is added to it.
	Sent  []RoutedSent
	Whole bool
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
// those hours is dropped (the notifications there, and the notifications
// that told of firings in them), so that no page reads it as none; what a
// firing reached in the hours that are worked out is kept, and counted when
// it is read: an hour not known makes that hour not known, and no other.
func (l *Ledger) MarkRoutedUnknown(ctx context.Context, from, to time.Time, why string) error {
	if why == "" {
		why = "not worked out"
	}
	return l.write(ctx, func(t *txn) error {
		if _, err := t.ExecContext(t.ctx, "DELETE FROM routed WHERE at >= ? AND at <= ?", from.Unix(), to.Unix()); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM firing_routed_sent WHERE at >= ? AND at <= ?", from.Unix(), to.Unix()); err != nil {
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
// the notifications, in place of those kept in them, and the notifications
// that told of each firing, in place of those kept for them in the same
// hours. [from, to) is then worked out, with a caveat when there is one
// (why: kept whole). What each firing reached is counted when it is read,
// from the notifications kept for it in the hours worked out (RoutedOf), so
// that a run that saw a firing for a few hours of the days it was on never
// rewrites the rest, and nothing here reads the notifications kept before
// for a firing: the only reads are the rows of the firings the run gives
// (their mutes and receivers that send nothing), 400 firings to a query.
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
		if _, err := t.ExecContext(t.ctx, "DELETE FROM firing_routed_sent WHERE at >= ? AND at <= ?", from.Unix(), to.Unix()); err != nil {
			return err
		}
		sent, err := t.inserting("firing_routed_sent", []string{"firing_id", "at", "receiver", "kind", "page"}, []string{"BIGINT", "BIGINT", "TEXT", "TEXT", "BIGINT"})
		if err != nil {
			return err
		}
		ids := make([]int64, 0, len(fs))
		given := map[int64]RoutedFiring{}
		for _, f := range fs {
			if _, dup := given[f.FiringID]; !dup {
				ids = append(ids, f.FiringID)
			}
			given[f.FiringID] = f
			for _, x := range f.Sent {
				if x.At.Before(from) || x.At.After(to) {
					continue
				}
				page := int64(0)
				if x.Page {
					page = 1
				}
				if err := sent.add(f.FiringID, x.At.Unix(), x.Receiver, x.Kind, page); err != nil {
					sent.close()
					return err
				}
			}
		}
		if err := sent.close(); err != nil {
			return err
		}
		if err := l.setCover(t, from, to, true, why); err != nil {
			return err
		}
		slices.Sort(ids)
		return l.keepStatic(t, ids, given)
	})
}

// keepStatic keeps what a run said of each of the firings besides the
// notifications that told of it: what muted it, the receivers that send
// nothing, and whether it was grouped. They are added to what was kept, or,
// from a run that saw the firing whole, replace it.
func (l *Ledger) keepStatic(t *txn, ids []int64, given map[int64]RoutedFiring) error {
	for part := range slices.Chunk(ids, 400) {
		in, args := inList(part)
		exist := map[int64]bool{}
		rows, err := t.QueryContext(t.ctx, "SELECT id FROM firings WHERE id IN ("+in+")", args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			exist[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		kept := map[int64]RoutedFiring{}
		rows, err = t.QueryContext(t.ctx, "SELECT firing_id, grouped, receivers, muted FROM firing_routed WHERE firing_id IN ("+in+")", args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			f, err := scanRouted(rows)
			if err != nil {
				rows.Close()
				return err
			}
			kept[f.FiringID] = f
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM firing_routed WHERE firing_id IN ("+in+")", args...); err != nil {
			return err
		}
		fin, err := t.inserting("firing_routed", []string{"firing_id", "grouped", "receivers", "muted"}, []string{"BIGINT", "BIGINT", "TEXT", "TEXT"})
		if err != nil {
			return err
		}
		for _, id := range part {
			if !exist[id] {
				continue
			}
			g := given[id]
			static, had := kept[id]
			if !had || g.Whole {
				static = RoutedFiring{}
			}
			static.Grouped = static.Grouped || g.Grouped
			for _, m := range g.Muted {
				if !slices.Contains(static.Muted, m) && len(static.Muted) < 100 {
					static.Muted = append(static.Muted, m)
				}
			}
			for _, rc := range g.Receivers {
				if rc.Empty && !slices.ContainsFunc(static.Receivers, func(x RoutedReceiver) bool { return x.Receiver == rc.Receiver }) {
					static.Receivers = append(static.Receivers, RoutedReceiver{Receiver: rc.Receiver, Empty: true})
				}
			}
			rc, _ := json.Marshal(nonNil(static.Receivers))
			mu, _ := json.Marshal(nonNil(static.Muted))
			grouped := int64(0)
			if static.Grouped {
				grouped = 1
			}
			if err := fin.add(id, grouped, string(rc), string(mu)); err != nil {
				fin.close()
				return err
			}
		}
		if err := fin.close(); err != nil {
			return err
		}
	}
	return nil
}

// inList is the placeholders and arguments of an IN list over ids.
func inList(ids []int64) (string, []any) {
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		marks[i], args[i] = "?", id
	}
	return strings.Join(marks, ", "), args
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
		for _, q := range []string{"DELETE FROM firing_routed_sent", "DELETE FROM firing_routed", "DELETE FROM routed", "DELETE FROM routed_cover"} {
			if _, err := t.ExecContext(t.ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
}

// RoutedOf is what was worked out for each of the firings given, by firing
// id, counted from the notifications kept for each in the hours worked out:
// one a run never saw isn't there, and neither is one some of whose life
// lies in hours that were tried and not worked out (what it reached isn't
// known, which is not none).
func (l *Ledger) RoutedOf(ctx context.Context, ids []int64) (map[int64]RoutedFiring, error) {
	out := map[int64]RoutedFiring{}
	var unknown [][2]int64
	err := l.eachRow(ctx, func(r scanner) error {
		var iv [2]int64
		if err := r.Scan(&iv[0], &iv[1]); err != nil {
			return err
		}
		unknown = append(unknown, iv)
		return nil
	}, "SELECT from_at, to_at FROM routed_cover WHERE ok = 0")
	if err != nil {
		return nil, err
	}
	for part := range slices.Chunk(ids, 400) {
		in, args := inList(part)
		kept := map[int64]RoutedFiring{}
		empties := map[int64][]RoutedReceiver{}
		err := l.eachRow(ctx, func(r scanner) error {
			f, err := scanRouted(r)
			if err != nil {
				return err
			}
			empties[f.FiringID], f.Receivers = f.Receivers, nil
			kept[f.FiringID] = f
			return nil
		}, "SELECT firing_id, grouped, receivers, muted FROM firing_routed WHERE firing_id IN ("+in+")", args...)
		if err != nil {
			return nil, err
		}
		if len(unknown) > 0 && len(kept) > 0 {
			err = l.eachRow(ctx, func(r scanner) error {
				var id, start int64
				var end sql.NullInt64
				if err := r.Scan(&id, &start, &end); err != nil {
					return err
				}
				e := int64(math.MaxInt64)
				if end.Valid {
					e = end.Int64
				}
				if slices.ContainsFunc(unknown, func(iv [2]int64) bool { return iv[0] <= e && iv[1] >= start }) {
					delete(kept, id)
				}
				return nil
			}, "SELECT id, start_at, end_at FROM firings WHERE id IN ("+in+")", args...)
			if err != nil {
				return nil, err
			}
		}
		// Counted, by receiver and integration, from the notifications kept.
		err = l.eachRow(ctx, func(r scanner) error {
			var id, n, paged int64
			var first sql.NullInt64
			var rc, kind string
			if err := r.Scan(&id, &rc, &kind, &n, &paged, &first); err != nil {
				return err
			}
			f, ok := kept[id]
			if !ok {
				return nil
			}
			f.Notified += int(n)
			f.Paged += int(paged)
			if first.Valid && (f.FirstPage.IsZero() || unix(first.Int64).Before(f.FirstPage)) {
				f.FirstPage = unix(first.Int64)
			}
			f.Receivers = append(f.Receivers, RoutedReceiver{Receiver: rc, Kind: kind, Notified: int(n), Paged: int(paged)})
			kept[id] = f
			return nil
		}, "SELECT firing_id, receiver, kind, count(*), coalesce(sum(page), 0), min(CASE WHEN page <> 0 THEN at END) FROM firing_routed_sent WHERE firing_id IN ("+in+") GROUP BY firing_id, receiver, kind ORDER BY firing_id, min(at), receiver, kind", args...)
		if err != nil {
			return nil, err
		}
		// A sample of the pages, to show.
		err = l.eachRow(ctx, func(r scanner) error {
			var id, at int64
			var rc string
			if err := r.Scan(&id, &at, &rc); err != nil {
				return err
			}
			if f, ok := kept[id]; ok {
				f.PageAts = append(f.PageAts, RoutedPageAt{unix(at), rc})
				kept[id] = f
			}
			return nil
		}, `SELECT firing_id, at, receiver FROM (SELECT firing_id, at, receiver, kind,
			row_number() OVER (PARTITION BY firing_id ORDER BY at, receiver, kind) AS rn
			FROM firing_routed_sent WHERE page <> 0 AND firing_id IN (`+in+`)) p WHERE rn <= `+fmt.Sprint(MaxRoutedPages)+` ORDER BY firing_id, at, receiver`, args...)
		if err != nil {
			return nil, err
		}
		for id, f := range kept {
			f.Receivers = append(f.Receivers, empties[id]...)
			out[id] = f
		}
	}
	return out, nil
}

// EachRoutedPage gives each page worked out for the firings given, in the
// hours worked out: every one, not a sample, for what is counted from them.
func (l *Ledger) EachRoutedPage(ctx context.Context, ids []int64, fn func(firingID int64, at time.Time, receiver string)) error {
	for part := range slices.Chunk(ids, 400) {
		in, args := inList(part)
		err := l.eachRow(ctx, func(r scanner) error {
			var id, at int64
			var rc string
			if err := r.Scan(&id, &at, &rc); err != nil {
				return err
			}
			fn(id, unix(at), rc)
			return nil
		}, "SELECT firing_id, at, receiver FROM firing_routed_sent WHERE page <> 0 AND firing_id IN ("+in+") ORDER BY firing_id, at, receiver, kind", args...)
		if err != nil {
			return err
		}
	}
	return nil
}

// scanRouted reads what is kept of a firing besides its notifications: the
// receivers that send nothing are in Receivers.
func scanRouted(r scanner) (RoutedFiring, error) {
	var f RoutedFiring
	var grouped int64
	var rc, mu string
	if err := r.Scan(&f.FiringID, &grouped, &rc, &mu); err != nil {
		return f, err
	}
	f.Grouped = grouped != 0
	if err := json.Unmarshal([]byte(rc), &f.Receivers); err != nil {
		return f, fmt.Errorf("firing %d's receivers: %w", f.FiringID, err)
	}
	if err := json.Unmarshal([]byte(mu), &f.Muted); err != nil {
		return f, fmt.Errorf("firing %d's mutes: %w", f.FiringID, err)
	}
	return f, nil
}

// RoutedPage is a page the model worked out: when it went and to which
// receiver, for which firing, its rule, and the firing's stones.
type RoutedPage struct {
	FiringID int64
	Rule     string
	At       time.Time
	Receiver string
	// Black is a firing that caught nothing (not white, not excused);
	// Useful is its usefulness as responses worked it out: 1, 0, or -1 for
	// not known.
	Black  bool
	Useful int
}

// RoutedPages are the pages worked out that went in [from, to), from the
// notifications kept for the firings in the hours worked out (each page is
// one: a firing's pages in a week are those of its hours in the week, whatever
// else of its life isn't known), the earliest first; at most limit of them,
// more saying there were more.
func (l *Ledger) RoutedPages(ctx context.Context, from, to time.Time, limit int) (out []RoutedPage, more bool, err error) {
	rows, err := l.r.QueryContext(ctx, `SELECT f.id, r.name, x.at, x.receiver, f.white, f.excused, f.useful
		FROM firing_routed_sent x JOIN firings f ON f.id = x.firing_id JOIN rules r ON r.id = f.rule_id
		WHERE x.page <> 0 AND x.at >= ? AND x.at < ?
		ORDER BY x.at, f.id, x.receiver, x.kind LIMIT ?`, from.Unix(), to.Unix(), limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var p RoutedPage
		var at, useful int64
		var white, excused bool
		if err := rows.Scan(&p.FiringID, &p.Rule, &at, &p.Receiver, &white, &excused, &useful); err != nil {
			return nil, false, err
		}
		p.At, p.Black, p.Useful = unix(at), !white && !excused, int(useful)
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
// window excused left out: the number a budget of pages counts. They are
// counted from the notifications kept for the firings, for the hours worked
// out; unknown are the weeks in which a firing started that was on in hours
// that were tried and not worked out: a number for such a week would be
// smaller than the pages that went, so none is to be read from it.
func (l *Ledger) RoutedPagesByWeek(ctx context.Context, from, to time.Time) (pages map[int64]map[string]int, unknown map[int64]bool, err error) {
	pages = map[int64]map[string]int{}
	unknown = map[int64]bool{}
	err = l.eachRow(ctx, func(r scanner) error {
		var name string
		var day, n int64
		if err := r.Scan(&name, &day, &n); err != nil {
			return err
		}
		w := Week(day)
		if pages[w] == nil {
			pages[w] = map[string]int{}
		}
		pages[w][name] += int(n)
		return nil
	}, `SELECT r.name, f.day, count(*) FROM firing_routed_sent x JOIN firings f ON f.id = x.firing_id JOIN rules r ON r.id = f.rule_id
		WHERE f.start_at >= ? AND f.start_at < ? AND f.excused = 0 AND x.page <> 0 GROUP BY r.name, f.day`, from.Unix(), to.Unix())
	if err != nil {
		return nil, nil, err
	}
	err = l.eachRow(ctx, func(r scanner) error {
		var day int64
		if err := r.Scan(&day); err != nil {
			return err
		}
		unknown[Week(day)] = true
		return nil
	}, `SELECT DISTINCT f.day FROM firings f JOIN routed_cover c ON c.ok = 0 AND c.from_at <= coalesce(f.end_at, 9223372036854775807) AND c.to_at >= f.start_at
		WHERE f.start_at >= ? AND f.start_at < ?`, from.Unix(), to.Unix())
	return pages, unknown, err
}

// RoutedTotals are the notifications worked out that went in [from, to) and
// told of a firing, counted once each however many firings they told of: how
// many, the pages among them, and how many groups they were for.
func (l *Ledger) RoutedTotals(ctx context.Context, from, to time.Time) (notified, paged, groups int, err error) {
	err = l.r.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(page), 0), count(DISTINCT grp) FROM routed WHERE at >= ? AND at < ? AND firings > 0`,
		from.Unix(), to.Unix()).Scan(&notified, &paged, &groups)
	return
}

// RoutedGroupKeys are the groups (their stable keys) of the notifications
// worked out that went in [from, to) and told of a firing. A group that goes
// on across stretches is the same key in each, so callers join the keys of
// several stretches and count them once.
func (l *Ledger) RoutedGroupKeys(ctx context.Context, from, to time.Time) ([]string, error) {
	var out []string
	err := l.eachRow(ctx, func(r scanner) error {
		var g string
		if err := r.Scan(&g); err != nil {
			return err
		}
		out = append(out, g)
		return nil
	}, "SELECT DISTINCT grp FROM routed WHERE at >= ? AND at < ? AND firings > 0", from.Unix(), to.Unix())
	return out, err
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

// RoutedNotifiedBefore is when each group last notified each integration
// kind of a receiver in [from, before), by the notifications worked out and
// kept: what a run that takes a group as notified before its own start
// counts the group's repeats from.
func (l *Ledger) RoutedNotifiedBefore(ctx context.Context, from, before time.Time) ([]RoutedLast, error) {
	var out []RoutedLast
	err := l.eachRow(ctx, func(r scanner) error {
		var x RoutedLast
		var at int64
		if err := r.Scan(&x.Group, &x.Receiver, &x.Kind, &at); err != nil {
			return err
		}
		x.At = unix(at)
		out = append(out, x)
		return nil
	}, "SELECT grp, receiver, kind, max(at) FROM routed WHERE at >= ? AND at < ? GROUP BY grp, receiver, kind", from.Unix(), before.Unix())
	return out, err
}

// RoutedLast is a group's last notification on the integrations of a kind
// of a receiver.
type RoutedLast struct {
	Group, Receiver, Kind string
	At                    time.Time
}
