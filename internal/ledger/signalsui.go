package ledger

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// Named impact signals set on the Settings page (docs/design/signals.md,
// step 3): an admin adds a signal, changes one, renames one in place or
// takes one away, or sets the rule entries in place of the start's, and
// each change is kept in one write with the signals in effect
// (SyncSignalSet): the same bits, ties and judging again as the command
// line's.

// SignalsChange is a change an admin made to the named impact signals or
// the rule entries: the settings it sets, by key ("" deletes one: back to
// the start); a signal renamed in place (Rename, From to To; From "" for
// the impact signal with no name, which the first named one saved in the
// UI makes a named one; To "" for the last named one taken away, which
// gives the one with no name back its windows); what goes in the activity
// log, each with its before and after; and Planned, which says whether the
// settings, as the write reads them, are those the change was worked out
// from: else nothing is written (ErrSignalsChanged).
type SignalsChange struct {
	Settings map[string]string
	Rename   bool
	From, To string
	Log      []Logged
	Planned  func(set map[string]Setting) bool
}

// ErrSignalsChanged is a change to the named signals worked out from
// settings another write changed since: two admins at once, or a
// replica's. Nothing of it is written; looked at again, it can be saved.
var ErrSignalsChanged = errors.New("the impact signals were changed meanwhile, by another admin or another replica: look at them again, and save again")

// Logged is a change as the activity log keeps it: what, and before and
// after, kept as JSON.
type Logged struct {
	What          string
	Before, After any
}

// ChangeSignals makes an admin's change to the named impact signals, in one
// write: the settings it sets, a signal renamed, the activity log, and the
// signals in effect after it, worked out from the settings as the write
// leaves them (from), kept as SyncSignalSet keeps them. One worked out from
// settings changed since (c.Planned), or a rename to a name the ledger
// keeps a signal of, or windows of one, is refused, and nothing is
// written.
func (l *Ledger) ChangeSignals(ctx context.Context, c SignalsChange, from SignalsFrom, by string) (SignalsSynced, error) {
	var out SignalsSynced
	err := l.write(ctx, func(t *txn) error {
		if c.Planned != nil {
			was, err := settingsOf(t.ctx, t)
			if err != nil {
				return err
			}
			if !c.Planned(was) {
				return ErrSignalsChanged
			}
		}
		if c.Rename {
			if err := l.renameSignal(t, c.From, c.To); err != nil {
				return err
			}
		}
		if err := putSettings(t, c.Settings, by, false); err != nil {
			return err
		}
		for _, x := range c.Log {
			if err := change(t, by, x.What, x.Before, x.After); err != nil {
				return err
			}
		}
		settings, err := settingsOf(t.ctx, t)
		if err != nil {
			return err
		}
		set, match := from(settings)
		out, err = l.syncSignals(t, set, match, false)
		return err
	})
	return out, err
}

// metaSignalsGen counts the writes that changed how the named signals
// judge (SyncSignalSet's that pend every firing, renames): options read
// before one judge nothing it pended (staleSignals).
const metaSignalsGen = "signals_gen"

// SignalsGen is the generation of the named signals, for
// verdict.Options.SignalsGen, read before the settings the options are
// worked out from: never 0.
func (l *Ledger) SignalsGen(ctx context.Context) (int64, error) {
	v, err := l.meta(ctx, l.r, metaSignalsGen)
	n, _ := strconv.ParseInt(v, 10, 64)
	return n + 1, err
}

// bumpSignalsGen counts a write that changes how the named signals judge.
func bumpSignalsGen(t *txn) error {
	v, err := (&Ledger{}).meta(t.ctx, t, metaSignalsGen)
	if err != nil {
		return err
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	t.stale = 0
	return setMeta(t.ctx, t, metaSignalsGen, strconv.FormatInt(n+1, 10))
}

// ErrSignalsMoved is JudgePending asked with options read before the named
// signals last changed: what is pended is left, for options read again.
var ErrSignalsMoved = errors.New("the impact signals changed since the options were read: what is pended is judged with them read again")

// staleSignals reports whether o was read before the named signals last
// changed, within a write: by its generation (o.SignalsGen), and by its
// signals and rule entries not being those the ledger keeps. With
// neither, it can't be told, and isn't.
func (l *Ledger) staleSignals(t *txn, o verdict.Options) (bool, error) {
	if t.stale != 0 {
		return t.stale == 1, nil
	}
	stale := false
	if o.SignalsGen != 0 {
		v, err := l.meta(t.ctx, t, metaSignalsGen)
		if err != nil {
			return false, err
		}
		n, _ := strconv.ParseInt(v, 10, 64)
		stale = n+1 != o.SignalsGen
	}
	// And, of the generation in effect, its signals and entries as the
	// ledger keeps them: options read of it, with signals kept from before
	// (another replica's change, a cache), are stale too.
	switch {
	case stale:
	case o.Signals.Named():
		rows, err := l.signalRows(t.ctx, t)
		if err != nil {
			return false, err
		}
		named := 0
		for _, r := range rows {
			if r.Name == "" {
				continue
			}
			named++
			i := slices.IndexFunc(o.Signals.List, func(x verdict.Signal) bool { return x.Name == r.Name })
			if i < 0 {
				stale = true
				break
			}
			x := o.Signals.List[i]
			if x.Bit != r.Bit || x.Record != r.Record || !sameMatch(x.Match, r.Match) {
				stale = true
				break
			}
		}
		if !stale && named != len(o.Signals.List) {
			stale = true
		}
		if !stale {
			es, err := ruleEntries(t.ctx, t)
			if err != nil {
				return false, err
			}
			stale = !sameEntries(es, o.Signals.Entries)
		}
	}
	t.stale = 2
	if stale {
		t.stale = 1
	}
	return stale, nil
}

// errNameTaken is a signal renamed to a name the ledger keeps a signal of,
// or the windows of one since taken away.
func errNameTaken(name string) error {
	return invalid("the ledger keeps a signal named %s, or the windows of one since taken away: give it another name", name)
}

// renameSignal renames a signal in place, within a write: its row, with its
// bit, what keeps the record by it and what it has seen; its windows'
// signal, and their keys, each impact-TO- and the minute it had, the tasks
// and comments on each moved with it, and what was decided of each kept
// (it is the row's); and the incidents a confirmation of one wrote, which
// carry its signal. From "", the impact signal with no name becomes a
// named one: given the lowest bit free, its windows keep their keys
// (impact- and the minute), which the runs and a read again know it by
// still (sameImpact). To must be a name no signal row, and no incident,
// has.
func (l *Ledger) renameSignal(t *txn, from, to string) error {
	if from == to {
		return nil
	}
	// Renaming is a change to how the signals judge: options read before
	// it judge nothing it pends.
	if err := bumpSignalsGen(t); err != nil {
		return err
	}
	if to == "" {
		return l.unnameSignal(t, from)
	}
	var n int
	if err := t.QueryRowContext(t.ctx, `SELECT (SELECT count(*) FROM impact_signals WHERE name = ?1)
		+ (SELECT count(*) FROM incidents WHERE signal = ?1)`, to).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errNameTaken(to)
	}
	// A run under way read it by the name it had, which keeps nothing once
	// written (no row of that name): from where the runs have got to, it is
	// read as one missed, and catches up.
	if err := missedFromCursor(t, from); err != nil {
		return err
	}
	if from == "" {
		have, err := l.signalRows(t.ctx, t)
		if err != nil {
			return err
		}
		used := map[int]bool{}
		for _, r := range have {
			if r.Name != "" {
				used[r.Bit] = true
			}
		}
		carried, err := carriedBits(t)
		if err != nil {
			return err
		}
		bit := freeBit(used, carried)
		if bit < 0 {
			return invalid("more than %d named signals: take one away first", verdict.MaxSignals)
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE impact_signals SET name = ?, bit = ? WHERE name = ''", to, bit); err != nil {
			return err
		}
		_, err = t.ExecContext(t.ctx, "UPDATE incidents SET signal = ? WHERE origin = 'impact' AND signal = ''", to)
		return err
	}
	if _, err := t.ExecContext(t.ctx, "UPDATE impact_signals SET name = ? WHERE name = ?", to, from); err != nil {
		return err
	}
	rows, err := t.QueryContext(t.ctx, "SELECT key FROM incidents WHERE origin = 'impact' AND signal = ? ORDER BY start_at, key", from)
	if err != nil {
		return err
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	reserved := map[string]bool{}
	for _, old := range keys {
		key, err := freeKey(t, renamedKey(old, from, to), reserved)
		if err != nil {
			return err
		}
		reserved[key] = true
		if _, err := t.ExecContext(t.ctx, "UPDATE incidents SET key = ?, signal = ? WHERE key = ?", key, to, old); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE tasks SET incident = ? WHERE incident = ?", key, old); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE comments SET on_key = ? WHERE on_kind = 'incident' AND on_key = ?", key, old); err != nil {
			return err
		}
	}
	// What a confirmation of one of its windows wrote carries its signal.
	_, err = t.ExecContext(t.ctx, "UPDATE incidents SET signal = ? WHERE origin != 'impact' AND signal = ?", to, from)
	return err
}

// UnnamedTiedTo is the name the windows of the impact signal with no name
// are given once named signals are in effect: the name the Settings page
// gives the one with no name when the first named signal is saved there
// (docs/design/signals.md, "Open questions").
const UnnamedTiedTo = "impact"

// TiedBy and TiedWhat are how the activity log notes the windows
// tieUnnamed tied.
const (
	TiedBy   = "lapilli"
	TiedWhat = "impact windows of the signal with no name tied to the signal " + UnnamedTiedTo
)

// tieUnnamed ties the windows of the impact signal with no name the ledger
// keeps to the signal impact, within the leader's write that keeps the
// named signals in effect (set), once any is: serve started with named
// signals on a ledger that holds windows the one with no name found
// before. Left so, each would be an incident with no signal, which keeps
// every rule's record, a rule's an entry ties to a signal that keeps none
// among them, as lapilli verdict, reading the same signals afresh, doesn't
// (docs/design/signals.md, "Step 3, as built").
//
// With impact among the signals in effect, and no row or window of that
// name kept, it is the Settings page's rename (renameSignal from ""), as
// when the first named signal is saved there: the row, with what keeps its
// record, and the windows, keys and all, given the lowest bit free; it
// reports the signal so made, which no alert is tied to yet. Else the
// windows alone take the name, their keys kept: impact's in effect,
// matched on its labels, or a signal's no longer read, as the windows of
// one taken away are kept, with its name; the firings that could catch
// them, from --lead before them (set.Lead) to their end, are pended to be
// judged again. Either way the activity log notes it, and out says how
// many.
func (l *Ledger) tieUnnamed(t *txn, set SignalSet, out *SignalsSynced) (bool, error) {
	rows, err := t.QueryContext(t.ctx, "SELECT start_at, end_at FROM incidents WHERE origin = 'impact' AND signal = '' ORDER BY start_at")
	if err != nil {
		return false, err
	}
	var spans [][2]int64
	for rows.Next() {
		var start, end int64
		if err := rows.Scan(&start, &end); err != nil {
			rows.Close()
			return false, err
		}
		spans = append(spans, [2]int64{start - int64(set.Lead/time.Second), end})
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(spans) == 0 {
		return false, err
	}
	i := slices.IndexFunc(set.Defs, func(d SignalDef) bool { return d.Name == UnnamedTiedTo })
	var kept int
	if err := t.QueryRowContext(t.ctx, `SELECT (SELECT count(*) FROM impact_signals WHERE name = ?1)
		+ (SELECT count(*) FROM incidents WHERE signal = ?1)`, UnnamedTiedTo).Scan(&kept); err != nil {
		return false, err
	}
	made := i >= 0 && kept == 0
	if made {
		if err := l.renameSignal(t, "", UnnamedTiedTo); err != nil {
			return false, err
		}
	} else {
		var match []string
		if i >= 0 {
			match = set.Defs[i].Match
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE incidents SET signal = ?, match_labels = ? WHERE origin = 'impact' AND signal = ''", UnnamedTiedTo, matchText(match)); err != nil {
			return false, err
		}
		if err := pend(t, spans); err != nil {
			return false, err
		}
	}
	out.Tied, out.TiedRead = len(spans), i >= 0
	return made, change(t, TiedBy, TiedWhat, map[string]any{"signal": "", "windows": len(spans)},
		map[string]any{"signal": UnnamedTiedTo, "windows": len(spans), "read": i >= 0})
}

// renamedKey is a window's key, of a signal renamed from to to: impact-TO-
// and what came after impact-FROM-, or, of a key with no signal's name in
// it (the windows of the signal with no name, renamed), after impact-.
func renamedKey(key, from, to string) string {
	if rest, ok := strings.CutPrefix(key, "impact-"+from+"-"); ok {
		return "impact-" + to + "-" + rest
	}
	return "impact-" + to + "-" + strings.TrimPrefix(key, "impact-")
}

// SignalWindowsSince counts each signal's windows that started since a
// time, by name, "" for the signal with no name: kept, decided or not, of
// signals in effect or taken away.
func (l *Ledger) SignalWindowsSince(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT signal, count(*) FROM incidents WHERE origin = 'impact' AND start_at >= ? GROUP BY signal", since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// DecidedSignal are a signal's windows something was decided about that a
// read of it from from on would take the place of, or end there, as
// DecidedImpact is of the signal with no name: for a try.
func (l *Ledger) DecidedSignal(ctx context.Context, name string, from time.Time) ([]Incident, error) {
	return l.incidents(ctx, `SELECT `+incidentCols+`
		FROM incidents i WHERE i.origin = 'impact' AND i.signal = ? AND i.state != '' AND (i.start_at >= ? OR i.end_at > ?) ORDER BY i.start_at, i.key`, name, from.Unix(), from.Unix())
}

// SignalNamesKept are the names the ledger keeps a named signal of, or the
// windows or confirmations of one since taken away: names a signal renamed
// in place can't take.
func (l *Ledger) SignalNamesKept(ctx context.Context) (map[string]bool, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT name FROM impact_signals WHERE name != ''
		UNION SELECT DISTINCT signal FROM incidents WHERE signal != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// missedFromCursor keeps a signal's row missed from where the runs have got
// to (the cursor), or from earlier, as it was: a run under way when it was
// renamed, which read it by its old name, keeps nothing of it, and the
// next run reads it from there. With no run under way, the next reads from
// the cursor anyway, and its read clears it.
func missedFromCursor(t *txn, name string) error {
	cur, err := (&Ledger{}).meta(t.ctx, t, "cursor")
	if err != nil || cur == "" {
		return err
	}
	at, err := strconv.ParseInt(cur, 10, 64)
	if err != nil {
		return nil
	}
	_, err = t.ExecContext(t.ctx, "UPDATE impact_signals SET missed_at = ? WHERE name = ? AND (missed_at = 0 OR missed_at > ?)", at, name, at)
	return err
}

// unnameSignal gives the impact signal with no name back the row and the
// windows of the named signal it was made (renameSignal from ""), the last
// named one taken away when the one with no name in effect is read with
// the same criterion: what keeps its record stands, and its windows are
// its own again, matched on the match in effect, those found while it was
// named keyed as the one with no name keys them (impact- and the minute),
// their tasks and comments with them; a confirmation's incident keeps the
// labels it was confirmed with. The row the runs kept of it while it stood
// aside (turned off) goes. One the runs had missed, behind, keeps no record
// from where it was missed until it is read again (a floor), as the one
// with no name has no catching up of its own. Every alert is tied again,
// to none, and every firing judged again, as with no named signal.
func (l *Ledger) unnameSignal(t *txn, from string) error {
	var missed int64
	if err := t.QueryRowContext(t.ctx, "SELECT missed_at FROM impact_signals WHERE name = ?", from).Scan(&missed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if _, err := t.ExecContext(t.ctx, "DELETE FROM impact_signals WHERE name = ''"); err != nil {
		return err
	}
	if _, err := t.ExecContext(t.ctx, `UPDATE impact_signals SET name = '', bit = 0, expr = '', for_ns = 0, match_labels = NULL, keeps_record = 1,
		set_by = '', set_at = 0, seen = '', seen_to = 0, missed_at = 0 WHERE name = ?`, from); err != nil {
		return err
	}
	// Missed before where the runs have got to: behind, hours no run read.
	// Missed from there (renamed, or named, with no run since), it is
	// read from there anyway.
	cur, err := (&Ledger{}).meta(t.ctx, t, "cursor")
	if err != nil {
		return err
	}
	if at, err := strconv.ParseInt(cur, 10, 64); missed > 0 && err == nil && missed < at {
		if err := setSigMeta(t, "", metaImpactFloor, strconv.FormatInt(missed, 10)); err != nil {
			return err
		}
	}
	rows, err := t.QueryContext(t.ctx, "SELECT key FROM incidents WHERE origin = 'impact' AND signal = ? ORDER BY start_at, key", from)
	if err != nil {
		return err
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	reserved := map[string]bool{}
	for _, old := range keys {
		key := old
		if rest, ok := strings.CutPrefix(old, "impact-"+from+"-"); ok {
			if key, err = freeKey(t, "impact-"+rest, reserved); err != nil {
				return err
			}
			reserved[key] = true
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE incidents SET key = ?, signal = '', match_labels = NULL WHERE key = ?", key, old); err != nil {
			return err
		}
		if key != old {
			if _, err := t.ExecContext(t.ctx, "UPDATE tasks SET incident = ? WHERE incident = ?", key, old); err != nil {
				return err
			}
			if _, err := t.ExecContext(t.ctx, "UPDATE comments SET on_key = ? WHERE on_kind = 'incident' AND on_key = ?", key, old); err != nil {
				return err
			}
		}
	}
	// A confirmation's incident, of no signal now, as the one with no
	// name's are, keeps the labels it was confirmed with.
	if _, err := t.ExecContext(t.ctx, "UPDATE incidents SET signal = '' WHERE origin != 'impact' AND signal = ?", from); err != nil {
		return err
	}
	if _, err := t.ExecContext(t.ctx, "UPDATE series SET signals = -1"); err != nil {
		return err
	}
	t.naming = 0
	if err := l.tieUntied(t, verdict.Options{}); err != nil {
		return err
	}
	return pendAll(t)
}

// adoptRead keeps, of a run of named signals under way when the one with no
// name was given its own back, its read of the criterion the one with no
// name has as the one with no name's: its windows its own, what keeps its
// record by it; one missed, no record from where it was, until it is read
// again (a floor). It reports whether there was one such read, and the
// earliest start whose stones may have changed.
func (l *Ledger) adoptRead(t *txn, f Found, o verdict.Options) (int64, bool, error) {
	lo := f.From.Unix()
	crit, err := sigMeta(t.ctx, t, "", metaImpactCriterion)
	if err != nil || crit == "" {
		return lo, false, err
	}
	i := slices.IndexFunc(f.Signals, func(r SignalRead) bool { return r.Criterion == crit })
	if i < 0 {
		return lo, false, nil
	}
	r := f.Signals[i]
	if r.Missed {
		floor, err := sigMeta(t.ctx, t, "", metaImpactFloor)
		if err != nil {
			return lo, true, err
		}
		if at, err := strconv.ParseInt(floor, 10, 64); err == nil && at <= r.From.Unix() {
			return lo, true, nil
		}
		return lo, true, setSigMeta(t, "", metaImpactFloor, strconv.FormatInt(r.From.Unix(), 10))
	}
	run := signalRun{on: true, since: r.Since, from: r.From, criterion: r.Criterion, lead: o.Lead}
	ws := make([]incident.Incident, 0, len(r.Windows))
	for _, w := range r.Windows {
		w.Signal, w.Match = "", nil
		ws = append(ws, w)
	}
	changed, err := l.criterionChanged(t, run)
	if err != nil {
		return lo, true, err
	}
	if lo, err = l.impact(t, run, ws, changed); err != nil {
		return lo, true, err
	}
	return lo, true, l.impactSince(t, run)
}
