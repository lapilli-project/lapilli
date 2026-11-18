package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// Impact signals, each named, read, started and judged on its own
// (docs/design/signals.md), kept in impact_signals (schema 022): a row a
// signal, '' for the unnamed one (--impact PROMQL), each with the six
// v0.49.0 kept in meta for the one signal there was (ImpactKept), what it
// has returned and read (seen), and from when a run couldn't read it.

// sigColumn is the column of impact_signals that holds what v0.49.0 kept
// in meta under key.
var sigColumn = map[string]string{
	metaImpactSince:     "since",
	metaImpactCriterion: "criterion",
	metaImpactAgain:     "again",
	metaImpactData:      "data",
	metaImpactRead:      "last_read",
	metaImpactFloor:     "floor_at",
}

// sigMeta is what a signal's row keeps under a meta key of v0.49.0's: ""
// for none, or no row.
func sigMeta(ctx context.Context, q querier, name, key string) (string, error) {
	col, ok := sigColumn[key]
	if !ok {
		return "", fmt.Errorf("no column for %s", key)
	}
	var v string
	err := q.QueryRowContext(ctx, "SELECT "+col+" FROM impact_signals WHERE name = ?", name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// setSigMeta keeps v under a meta key of v0.49.0's, in a signal's row:
// the unnamed signal's made when there is none, as meta's key was; a
// named one's only when SyncSignals made it, with its own bit, and none
// for one it hasn't (a replica that follows a leader started without it,
// or a sync that failed), which keeps nothing.
func setSigMeta(t *txn, name, key, v string) error {
	col, ok := sigColumn[key]
	if !ok {
		return fmt.Errorf("no column for %s", key)
	}
	if name != "" {
		_, err := t.ExecContext(t.ctx, "UPDATE impact_signals SET "+col+" = ? WHERE name = ?", v, name)
		return err
	}
	_, err := t.ExecContext(t.ctx, "INSERT INTO impact_signals (name, "+col+") VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET "+col+" = excluded."+col, name, v)
	return err
}

// delSigMeta forgets what a signal's row keeps under meta keys of
// v0.49.0's, as deleting them from meta did.
func delSigMeta(t *txn, name string, keys ...string) error {
	for _, k := range keys {
		col, ok := sigColumn[k]
		if !ok {
			return fmt.Errorf("no column for %s", k)
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE impact_signals SET "+col+" = '' WHERE name = ?", name); err != nil {
			return err
		}
	}
	return nil
}

// sigMetaSet counts the meta keys of v0.49.0's a signal's row keeps
// something under.
func sigMetaSet(t *txn, name string, keys ...string) (int, error) {
	n := 0
	for _, k := range keys {
		v, err := sigMeta(t.ctx, t, name, k)
		if err != nil {
			return 0, err
		}
		if v != "" {
			n++
		}
	}
	return n, nil
}

// SignalDef is a signal as serve was started with it, or an admin set it:
// what it reads, for how long a series returns something before it is a
// window, the labels its windows are matched on (nil for the match in
// effect), and whether reading it keeps the record; and, of one an admin
// set on the Settings page, who and when (SetBy "" for the command
// line's or the file's).
type SignalDef struct {
	Name   string
	Expr   string
	For    time.Duration
	Match  []string
	Record bool
	SetBy  string
	SetAt  time.Time
}

// sameDef reports whether a signal's row keeps def as given: its
// expression, "for", match and record, and who set it in the UI and when.
func sameDef(r SignalRow, d SignalDef) bool {
	return r.Expr == d.Expr && r.For == d.For && sameMatch(r.Match, d.Match) && r.Record == d.Record &&
		r.SetBy == d.SetBy && unixOr0(r.SetAt) == unixOr0(d.SetAt)
}

// sameMatch reports whether two matches are the same: nil, the match in
// effect, is not empty, none.
func sameMatch(a, b []string) bool { return slices.Equal(a, b) && (a == nil) == (b == nil) }

// SignalRow is a signal as the ledger keeps it: as it was set, its bit in
// the signals an alert is tied to, what keeps the record by it, and from
// when a run couldn't read it (zero for none).
type SignalRow struct {
	SignalDef
	Bit    int
	Kept   ImpactKept
	Missed time.Time
}

// MatchOr is the labels a signal's windows are matched on: its own, or
// else those in effect.
func (d SignalDef) MatchOr(inEffect []string) []string {
	if d.Match != nil {
		return d.Match
	}
	return inEffect
}

// matchText is a match as a column keeps it: NULL for the match in effect.
func matchText(m []string) any {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// matchOf reads a match as matchText kept it.
func matchOf(v sql.NullString) []string {
	if !v.Valid {
		return nil
	}
	m := []string{}
	if json.Unmarshal([]byte(v.String), &m) != nil {
		return nil
	}
	return m
}

// SignalRows are the named signals the ledger keeps, and the unnamed one
// when it is kept, by bit and then name: what each was set to, its bit,
// what keeps the record by it, and from when a run couldn't read it.
func (l *Ledger) SignalRows(ctx context.Context) ([]SignalRow, error) { return l.signalRows(ctx, l.r) }

func (l *Ledger) signalRows(ctx context.Context, q reader) ([]SignalRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, bit, expr, for_ns, match_labels, keeps_record, since, criterion, again, data, last_read, floor_at, missed_at,
		set_by, set_at FROM impact_signals ORDER BY bit, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SignalRow
	for rows.Next() {
		var r SignalRow
		var forNS, missed, setAt int64
		var match sql.NullString
		var since, crit, again, data, read, floor string
		if err := rows.Scan(&r.Name, &r.Bit, &r.Expr, &forNS, &match, &r.Record, &since, &crit, &again, &data, &read, &floor, &missed, &r.SetBy, &setAt); err != nil {
			return nil, err
		}
		r.For, r.Match = time.Duration(forNS), matchOf(match)
		if setAt > 0 {
			r.SetAt = unix(setAt)
		}
		r.Kept = keptOf(since, crit, again, data, read, floor)
		if missed > 0 {
			r.Missed = unix(missed)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// keptOf is what keeps the record by a signal, from its row's six.
func keptOf(since, crit, again, data, read, floor string) ImpactKept {
	var k ImpactKept
	if n, err := strconv.ParseInt(since, 10, 64); err == nil && since != impactNone {
		k.Since = unix(n)
	}
	k.Criterion, k.Again, k.Read = crit, again, read
	if data != "" {
		var d ImpactData
		if json.Unmarshal([]byte(data), &d) == nil {
			k.Data = &d
		}
	}
	if n, err := strconv.ParseInt(floor, 10, 64); err == nil {
		k.Floor = unix(n)
	}
	return k
}

// SignalKept reads what keeps the record by a signal, by name ("" for the
// unnamed one), in one read.
func (l *Ledger) SignalKept(ctx context.Context, name string) (ImpactKept, error) {
	var since, crit, again, data, read, floor string
	err := l.r.QueryRowContext(ctx, "SELECT since, criterion, again, data, last_read, floor_at FROM impact_signals WHERE name = ?", name).
		Scan(&since, &crit, &again, &data, &read, &floor)
	if errors.Is(err, sql.ErrNoRows) {
		return ImpactKept{}, nil
	}
	if err != nil {
		return ImpactKept{}, err
	}
	return keptOf(since, crit, again, data, read, floor), nil
}

// RuleEntries are the rule entries the ledger keeps, in order.
func (l *Ledger) RuleEntries(ctx context.Context) ([]verdict.RuleEntry, error) {
	return ruleEntries(ctx, l.r)
}

func ruleEntries(ctx context.Context, q reader) ([]verdict.RuleEntry, error) {
	rows, err := q.QueryContext(ctx, "SELECT pattern, labels, signals, match_labels FROM impact_rules ORDER BY pos")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []verdict.RuleEntry
	for rows.Next() {
		var e verdict.RuleEntry
		var labels, signals string
		var match sql.NullString
		if err := rows.Scan(&e.Rules, &labels, &signals, &match); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(labels), &e.Labels); err != nil {
			return nil, err
		}
		if len(e.Labels) == 0 {
			e.Labels = nil
		}
		if err := json.Unmarshal([]byte(signals), &e.Signals); err != nil {
			return nil, err
		}
		e.Match = matchOf(match)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SignalsSynced is what keeping the signals serve was started with
// changed: Set, a signal added or removed, or its match or whether it
// keeps the record, or the match in effect, which those with no match of
// their own follow; Entries, the rule entries. Either judges every firing
// again (pended, in the same write: whichever replica leads next judges
// them), and Set works out again which signals each alert is tied to.
// Tied is how many windows of the impact signal with no name were tied to
// the signal impact (tieUnnamed), TiedRead whether impact is among the
// signals in effect, which reads it on.
type SignalsSynced struct {
	Set, Entries bool
	Tied         int
	TiedRead     bool
}

// metaSignalsMatch is the match in effect alerts were last tied on, as
// JSON: a change to it ties every alert again, and what the signals that
// follow it have seen begins again.
const metaSignalsMatch = "signals_match"

// matchKey is a match as metaSignalsMatch keeps it.
func matchKey(m []string) string {
	if m == nil {
		m = []string{}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// SyncSignals keeps the named signals in effect and the rule entries, as
// serve was started with them: each signal's row made, or its expression,
// "for", match and whether it keeps the record kept as given, a bit given
// to each new one, the lowest free; a named signal no longer given taken
// away, its windows kept, as incidents, with its name; and the entries put
// in place of those kept. With signals named, the unnamed one isn't read,
// and a run turns it off. What keeps the record by each signal is the
// runs' to keep (Record). When the set changed, which signals each alert
// is tied to is worked out again at once, on match, the match in effect;
// its tallies are counted again by the judging a change asks for.
func (l *Ledger) SyncSignals(ctx context.Context, defs []SignalDef, entries []verdict.RuleEntry, match []string) (SignalsSynced, error) {
	return l.SyncSignalSet(ctx, func(map[string]Setting) (SignalSet, []string) { return SignalSet{Defs: defs, Entries: entries}, match })
}

// SignalsFrom works out, from the settings the UI changed as a write reads
// them, the named signals in effect, the rule entries and the match in
// effect: lapilli serve's, the command line's with the UI's over them. It
// is called within the write that keeps them, so that what another write
// changed before it is never undone by a set worked out before.
type SignalsFrom func(set map[string]Setting) (SignalSet, []string)

// SignalSet is what SyncSignalSet keeps: the named signals in effect, each
// with who set it in the UI and when, and the rule entries, EntriesUI when
// they are the UI's, in place of the start's (docs/design/signals.md,
// step 3). Lead is --lead in effect, which is not kept: a firing up to it
// before a window may catch it, so the windows tieUnnamed ties are judged
// again from it.
type SignalSet struct {
	Defs      []SignalDef
	Entries   []verdict.RuleEntry
	EntriesUI bool
	Lead      time.Duration
}

// SyncSignalSet is SyncSignals of the signals in effect, the start's and
// the UI's, worked out from the settings as its write reads them (from):
// who set each in the UI and when, and whether the entries are the UI's,
// kept beside them (impact_signals.set_by and set_at, impact_rules.ui),
// changing no judging. With named signals in effect, the windows of the
// impact signal with no name the ledger keeps from before are tied to
// the signal impact in the same write (tieUnnamed), once: the leader's
// first round after serve was started with named signals.
func (l *Ledger) SyncSignalSet(ctx context.Context, from SignalsFrom) (SignalsSynced, error) {
	var out SignalsSynced
	// Read first: a round that changes nothing writes nothing, and keeps
	// every page kept for the ledger's generation.
	settings, err := l.Settings(ctx)
	if err != nil {
		return out, err
	}
	set, match := from(settings)
	same, err := l.signalsAre(ctx, set, match)
	if err != nil {
		return out, err
	}
	if same {
		if !namedIn(set.Defs) {
			return out, nil
		}
		if some, err := unnamedWindows(ctx, l.r); err != nil || !some {
			return out, err
		}
	}
	err = l.write(ctx, func(t *txn) error {
		settings, err := settingsOf(t.ctx, t)
		if err != nil {
			return err
		}
		set, match := from(settings)
		out, err = l.syncSignals(t, set, match, true)
		return err
	})
	return out, err
}

// namedIn reports whether defs name a signal.
func namedIn(defs []SignalDef) bool {
	return slices.ContainsFunc(defs, func(d SignalDef) bool { return d.Name != "" })
}

// unnamedWindows reports whether the ledger keeps a window of the impact
// signal with no name.
func unnamedWindows(ctx context.Context, q reader) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM incidents WHERE origin = 'impact' AND signal = '' LIMIT 1) AS one").Scan(&n)
	return n > 0, err
}

// syncSignals is SyncSignalSet within a write; tie, its round's, ties the
// windows of the signal with no name (tieUnnamed) first.
func (l *Ledger) syncSignals(t *txn, set SignalSet, match []string, tie bool) (SignalsSynced, error) {
	defs, entries := set.Defs, set.Entries
	out := SignalsSynced{}
	err := func() error {
		t.naming = 0 // read again after this write's own
		if tie {
			made, err := l.tieUnnamed(t, set, &out)
			if err != nil {
				return err
			}
			// The signal impact made of the one with no name is one no
			// alert is tied to yet: tied, and judged, as one added.
			out.Set = made
		}
		have, err := l.signalRows(t.ctx, t)
		if err != nil {
			return err
		}
		used := map[int]bool{}
		byName := map[string]SignalRow{}
		for _, r := range have {
			byName[r.Name] = r
			if r.Name != "" {
				used[r.Bit] = true
			}
		}
		// A bit an alert or a tally still carries is given to a new signal
		// only when no other is free: until every firing is judged again
		// (pendAll, below, in this write), a tally of a signal taken away
		// would be read as the one added in its place. With eight, one
		// replaced, there is none other: its tallies are counted again
		// before long, and its alerts are tied again here.
		carried, err := carriedBits(t)
		if err != nil {
			return err
		}
		for _, r := range have {
			if r.Name == "" || slices.ContainsFunc(defs, func(d SignalDef) bool { return d.Name == r.Name }) {
				continue
			}
			if _, err := t.ExecContext(t.ctx, "DELETE FROM impact_signals WHERE name = ?", r.Name); err != nil {
				return err
			}
			delete(used, r.Bit)
			out.Set = true
		}
		for _, d := range defs {
			if d.Name == "" {
				continue
			}
			r, ok := byName[d.Name]
			if !ok {
				bit := freeBit(used, carried)
				if bit < 0 {
					return fmt.Errorf("more than %d named signals", verdict.MaxSignals)
				}
				used[bit] = true
				if _, err := t.ExecContext(t.ctx, `INSERT INTO impact_signals (name, bit, expr, for_ns, match_labels, keeps_record, set_by, set_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
					d.Name, bit, d.Expr, int64(d.For), matchText(d.Match), d.Record, d.SetBy, unixOr0(d.SetAt)); err != nil {
					return err
				}
				out.Set = true
				continue
			}
			if sameDef(r, d) {
				continue
			}
			if !sameMatch(r.Match, d.Match) || r.Record != d.Record {
				out.Set = true
			}
			if _, err := t.ExecContext(t.ctx, "UPDATE impact_signals SET expr = ?, for_ns = ?, match_labels = ?, keeps_record = ?, set_by = ?, set_at = ? WHERE name = ?",
				d.Expr, int64(d.For), matchText(d.Match), d.Record, d.SetBy, unixOr0(d.SetAt), d.Name); err != nil {
				return err
			}
			if !sameMatch(r.Match, d.Match) {
				// Of other labels, what it has seen begins again; and its
				// windows are matched on its labels now, as the judging
				// again this asks for judges them (a confirmation's
				// incident keeps the labels it was confirmed with).
				if err := seenAgain(t, r); err != nil {
					return err
				}
				if _, err := t.ExecContext(t.ctx, "UPDATE incidents SET match_labels = ? WHERE origin = 'impact' AND signal = ?", matchText(d.Match), d.Name); err != nil {
					return err
				}
			}
		}
		was, err := ruleEntries(t.ctx, t)
		if err != nil {
			return err
		}
		wasUI, err := entriesUI(t.ctx, t)
		if err != nil {
			return err
		}
		switch {
		case !sameEntries(was, entries):
			out.Entries = true
			if _, err := t.ExecContext(t.ctx, "DELETE FROM impact_rules"); err != nil {
				return err
			}
			for i, e := range entries {
				labels, err := json.Marshal(orEmpty(e.Labels))
				if err != nil {
					return err
				}
				signals, err := json.Marshal(e.Signals)
				if err != nil {
					return err
				}
				if _, err := t.ExecContext(t.ctx, "INSERT INTO impact_rules (pos, pattern, labels, signals, match_labels, ui) VALUES (?, ?, ?, ?, ?, ?)",
					i+1, e.Rules, string(labels), string(signals), matchText(e.Match), set.EntriesUI); err != nil {
					return err
				}
			}
		case len(entries) > 0 && wasUI != set.EntriesUI:
			// The same entries, the UI's now or the start's again:
			// nothing is judged otherwise.
			if _, err := t.ExecContext(t.ctx, "UPDATE impact_rules SET ui = ?", set.EntriesUI); err != nil {
				return err
			}
		}
		// The match in effect, which the signals with none of their own
		// follow: changed, what they have seen begins again.
		was2, err := l.meta(t.ctx, t, metaSignalsMatch)
		if err != nil {
			return err
		}
		if named := namedIn(defs); named && was2 != matchKey(match) {
			if was2 != "" {
				out.Set = true
				for _, d := range defs {
					if r, ok := byName[d.Name]; ok && d.Name != "" && d.Match == nil {
						if err := seenAgain(t, r); err != nil {
							return err
						}
					}
				}
			}
			if err := setMeta(t.ctx, t, metaSignalsMatch, matchKey(match)); err != nil {
				return err
			}
		}
		if out.Set {
			// Which signals each alert is tied to, worked out again from
			// the signals now in effect.
			if _, err := t.ExecContext(t.ctx, "UPDATE series SET signals = -1"); err != nil {
				return err
			}
			if err := l.tieUntied(t, verdict.Options{Match: match}); err != nil {
				return err
			}
		}
		if out.Set || out.Entries {
			// Every firing judged again, a week at a time (JudgePending):
			// pended here, so that a restart or another leader takes it up;
			// by options read after this write (bumpSignalsGen).
			if err := bumpSignalsGen(t); err != nil {
				return err
			}
			return pendAll(t)
		}
		return nil
	}()
	return out, err
}

// entriesUI reports whether the rule entries the ledger keeps are the
// UI's: none kept says no.
func entriesUI(ctx context.Context, q reader) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM impact_rules WHERE ui != 0 LIMIT 1) AS one").Scan(&n)
	return n > 0, err
}

// seenAgain begins again what a signal has seen, of other labels: looked
// at again from where its reads began (its since), so that a value it
// read only in the weeks before is learned again; from its next read, for
// one with no since yet.
func seenAgain(t *txn, r SignalRow) error {
	var from int64
	if !r.Kept.Since.IsZero() {
		from = r.Kept.Since.Unix()
	}
	_, err := t.ExecContext(t.ctx, "UPDATE impact_signals SET seen = '', seen_to = ? WHERE name = ?", from, r.Name)
	return err
}

// freeBit is the lowest bit no signal has and no alert or tally carries;
// else the lowest no signal has; else -1, all eight had.
func freeBit(used map[int]bool, carried int64) int {
	for _, uncarried := range []bool{true, false} {
		for bit := 0; bit <= verdict.MaxSignalBit; bit++ {
			if !used[bit] && (!uncarried || carried&(1<<bit) == 0) {
				return bit
			}
		}
	}
	return -1
}

// pendAll pends the span of every firing and incident, to be judged again
// a week at a time: what JudgeAll judges, so that, done, it needn't be.
func pendAll(t *txn) error {
	var lo, hi sql.NullInt64
	if err := t.QueryRowContext(t.ctx, `SELECT min(t), max(t) FROM (SELECT start_at AS t FROM firings
		UNION ALL SELECT start_at FROM incidents) AS starts`).Scan(&lo, &hi); err != nil || !lo.Valid {
		return err
	}
	return pend(t, [][2]int64{{lo.Int64, hi.Int64}})
}

// carriedBits are the signals' bits an alert or a tally carries, as one
// set of bits.
func carriedBits(t *txn) (int64, error) {
	var out int64
	for bit := 0; bit <= verdict.MaxSignalBit; bit++ {
		b := int64(1) << bit
		for _, table := range []string{"series", "days", "weeks", "marked_days", "marked_weeks"} {
			var n int
			if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM (SELECT 1 FROM "+table+" WHERE signals > 0 AND (signals & CAST(? AS BIGINT)) != 0 LIMIT 1) AS one", b).Scan(&n); err != nil {
				return 0, err
			}
			if n > 0 {
				out |= b
				break
			}
		}
	}
	return out, nil
}

// retieSignal works out again which signals the alerts tied to a named
// signal, or that fell back to every one, are tied to, as what it has
// seen begins again (a read again of a new expression): the span of their
// firings is pended, to be counted again.
func (l *Ledger) retieSignal(t *txn, name string, match []string) error {
	var bit int
	if err := t.QueryRowContext(t.ctx, "SELECT bit FROM impact_signals WHERE name = ?", name).Scan(&bit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	rows, err := t.QueryContext(t.ctx, "SELECT id FROM series WHERE signals > 0 AND (signals & CAST(? AS BIGINT)) != 0", int64(1)<<bit|verdict.FellBackBit)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return err
	}
	for part := range slices.Chunk(ids, pgRows) {
		b, err := json.Marshal(part)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE series SET signals = -1 WHERE id IN ("+t.d.ints("?")+")", string(b)); err != nil {
			return err
		}
	}
	if err := l.tieUntied(t, verdict.Options{Match: match}); err != nil {
		return err
	}
	lo, hi, err := seriesSpan(t, ids)
	if err != nil || lo > hi {
		return err
	}
	return pend(t, [][2]int64{{lo, hi}})
}

// signalsAre reports whether the ledger keeps the named signals and rule
// entries given, as given.
func (l *Ledger) signalsAre(ctx context.Context, set SignalSet, match []string) (bool, error) {
	defs, entries := set.Defs, set.Entries
	if namedIn(defs) {
		if was, err := l.meta(ctx, l.r, metaSignalsMatch); err != nil || was != matchKey(match) {
			return false, err
		}
	}
	rows, err := l.signalRows(ctx, l.r)
	if err != nil {
		return false, err
	}
	n := 0
	for _, r := range rows {
		if r.Name == "" {
			continue
		}
		n++
		i := slices.IndexFunc(defs, func(d SignalDef) bool { return d.Name == r.Name })
		if i < 0 {
			return false, nil
		}
		if !sameDef(r, defs[i]) {
			return false, nil
		}
	}
	if n != len(defs) {
		return false, nil
	}
	was, err := ruleEntries(ctx, l.r)
	if err != nil || !sameEntries(was, entries) {
		return false, err
	}
	ui, err := entriesUI(ctx, l.r)
	return err == nil && (len(entries) == 0 || ui == set.EntriesUI), err
}

// SignalWindowsBetween counts a signal's windows that started in [from,
// to): what, beside a read's own, bounds the windows it keeps in a week.
func (l *Ledger) SignalWindowsBetween(ctx context.Context, name string, from, to time.Time) (int, error) {
	var n int
	err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM incidents WHERE origin = 'impact' AND signal = ? AND start_at >= ? AND start_at < ?",
		name, from.Unix(), to.Unix()).Scan(&n)
	return n, err
}

// SameEntries reports whether two lists of rule entries say the same, as
// impact_rules keeps them: each as the JSON it is kept as, so that one a
// write keeps and reads back otherwise (a value that isn't UTF-8, kept
// with U+FFFD) is the same, and a round that keeps the start's never
// writes them again, and judges every firing again, for nothing.
func SameEntries(a, b []verdict.RuleEntry) bool { return sameEntries(a, b) }

func sameEntries(a, b []verdict.RuleEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if entryKey(a[i]) != entryKey(b[i]) {
			return false
		}
	}
	return true
}

// entryKey is a rule entry as impact_rules keeps it: its pattern, labels,
// signals and match (null for each signal's own), as JSON.
func entryKey(e verdict.RuleEntry) string {
	b, _ := json.Marshal(struct {
		Rules   string
		Labels  incident.Labels
		Signals []string
		Match   any
	}{e.Rules, orEmpty(e.Labels), orNone(e.Signals), matchText(e.Match)})
	// As read back: the bytes JSON keeps as U+FFFD are that, written out.
	var v any
	if json.Unmarshal(b, &v) == nil {
		b, _ = json.Marshal(v)
	}
	return string(b)
}

// seenOf is what each named signal has returned and read, by name, of the
// labels it is matched on (match, the match in effect, for those with
// none of their own): one kept of other labels is begun again.
func seenOf(ctx context.Context, q reader, match []string) (map[string]*verdict.Seen, []SignalRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, bit, match_labels, keeps_record, seen FROM impact_signals WHERE name != '' ORDER BY bit`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := map[string]*verdict.Seen{}
	var list []SignalRow
	for rows.Next() {
		var r SignalRow
		var m sql.NullString
		var text string
		if err := rows.Scan(&r.Name, &r.Bit, &m, &r.Record, &text); err != nil {
			return nil, nil, err
		}
		r.Match = matchOf(m)
		on := r.MatchOr(match)
		seen := verdict.NewSeen(on)
		if text != "" {
			var kept verdict.Seen
			if json.Unmarshal([]byte(text), &kept) == nil && kept.For(on) {
				seen = &kept
			}
		}
		out[r.Name] = seen
		list = append(list, r)
	}
	return out, list, rows.Err()
}

// tieSignals are the named signals in effect as the ledger keeps them,
// each with what it has seen, to tie alerts by their labels.
func tieSignals(t *txn, match []string) (*verdict.Signals, error) {
	seen, rows, err := seenOf(t.ctx, t, match)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	list := make([]verdict.Signal, 0, len(rows))
	for _, r := range rows {
		list = append(list, verdict.Signal{Name: r.Name, Match: r.Match, Record: r.Record, Bit: r.Bit, Seen: seen[r.Name]})
	}
	return verdict.NewSignals(list, nil), nil
}

// tieUntied works out which signals each alert waiting for it is tied to
// (series.signals -1), by its labels and what each named signal has seen,
// on the match in effect o.Match: 0, none, with no signal named, in one
// statement. An alert waits only from the write that made it, or the one
// that changed the signals, until it is judged in it, before its tallies:
// its firings are all within what that write judges, and none was
// counted under -1.
func (l *Ledger) tieUntied(t *txn, o verdict.Options) error {
	// Most writes make no alert: what each signal has seen is read only
	// when one waits (series_untied).
	var n int
	if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM (SELECT 1 FROM series WHERE signals < 0 LIMIT 1) AS one").Scan(&n); err != nil || n == 0 {
		return err
	}
	sig, err := tieSignals(t, o.Match)
	if err != nil {
		return err
	}
	if !sig.Named() {
		_, err := t.ExecContext(t.ctx, "UPDATE series SET signals = 0 WHERE signals < 0")
		return err
	}
	rows, err := t.QueryContext(t.ctx, "SELECT id, labels FROM series WHERE signals < 0")
	if err != nil {
		return err
	}
	by := map[int64][]int64{} // signals to the alerts tied to them
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			rows.Close()
			return err
		}
		labels, err := l.labels(id, text)
		if err != nil {
			rows.Close()
			return err
		}
		bits := sig.Bits(sig.TiesByLabels(labels, o.Match))
		by[bits] = append(by[bits], id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// A statement for each set of signals, which most alerts share.
	for _, bits := range slices.Sorted(maps.Keys(by)) {
		for part := range slices.Chunk(by[bits], pgRows) {
			b, err := json.Marshal(part)
			if err != nil {
				return err
			}
			if _, err := t.ExecContext(t.ctx, "UPDATE series SET signals = ? WHERE id IN ("+t.d.ints("?")+")", bits, string(b)); err != nil {
				return err
			}
		}
	}
	return nil
}

// named reports whether the ledger keeps a named impact signal, within a
// write: read once (untied's).
func (t *txn) named() (bool, error) {
	if t.naming == 0 {
		if _, err := t.untied(); err != nil {
			return false, err
		}
	}
	return t.naming == 1, nil
}

// untied is what a new alert's signals are as it is made: -1, to be tied in
// the same write (tieUntied), while a named signal is kept; else 0, none,
// every alert's with no signal named, which then needs no more writing.
func (t *txn) untied() (int64, error) {
	if t.naming == 0 {
		var n int
		if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM (SELECT 1 FROM impact_signals WHERE name != '' LIMIT 1) AS one").Scan(&n); err != nil {
			return 0, err
		}
		t.naming = 2
		if n > 0 {
			t.naming = 1
		}
	}
	if t.naming == 1 {
		return -1, nil
	}
	return 0, nil
}

// seriesSpan is the span of the firings of the alerts ids, lo > hi for
// none.
func seriesSpan(t *txn, ids []int64) (int64, int64, error) {
	lo, hi := int64(1), int64(0)
	for part := range slices.Chunk(ids, pgRows) {
		b, err := json.Marshal(part)
		if err != nil {
			return 1, 0, err
		}
		var a, z sql.NullInt64
		if err := t.QueryRowContext(t.ctx, "SELECT min(start_at), max(start_at) FROM firings WHERE series_id IN ("+t.d.ints("?")+")", string(b)).Scan(&a, &z); err != nil {
			return 1, 0, err
		}
		if !a.Valid {
			continue
		}
		if lo > hi {
			lo, hi = a.Int64, z.Int64
		} else {
			lo, hi = min(lo, a.Int64), max(hi, z.Int64)
		}
	}
	return lo, hi, nil
}

// addSeen keeps what named signals have returned or read beside what they
// had (seenOf), by name, and works out again which signals the alerts it
// may tie otherwise are tied to: those with a label's value one of them
// saw first now, or a label one of them now takes to have every value of,
// which alone can be tied to more, or to fewer once one that keeps the
// record ties them in place of all that do (verdict.FellBackBit). It reads
// each alert's labels once, from the cache, and writes only those whose
// signals change; the span of their firings is pended, to be counted
// again a week at a time (JudgePending).
func (l *Ledger) addSeen(t *txn, match []string, more map[string]*verdict.Seen) error {
	if len(more) == 0 {
		return nil
	}
	seen, rows, err := seenOf(t.ctx, t, match)
	if err != nil {
		return err
	}
	gained := map[string]map[string]bool{} // label to the values first seen of it
	every := map[string]bool{}
	for _, r := range rows {
		m, ok := more[r.Name]
		if !ok {
			continue
		}
		was, err := sigSeenText(t, r.Name)
		if err != nil {
			return err
		}
		before := seen[r.Name].Clone()
		changed := seen[r.Name].Merge(m)
		b, err := json.Marshal(seen[r.Name])
		if err != nil {
			return err
		}
		if !changed && was == string(b) {
			continue
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE impact_signals SET seen = ? WHERE name = ?", string(b), r.Name); err != nil {
			return err
		}
		vs, all := seen[r.Name].Gained(before)
		for k, xs := range vs {
			if gained[k] == nil {
				gained[k] = map[string]bool{}
			}
			for _, x := range xs {
				gained[k][x] = true
			}
		}
		for _, k := range all {
			every[k] = true
		}
	}
	if len(gained) == 0 && len(every) == 0 {
		return nil
	}
	sig, err := tieSignals(t, match)
	if err != nil || !sig.Named() {
		return err
	}
	rs, err := t.QueryContext(t.ctx, "SELECT id, labels, signals FROM series WHERE signals >= 0")
	if err != nil {
		return err
	}
	type retie struct {
		id, bits int64
	}
	var changed []retie
	for rs.Next() {
		var id, bits int64
		var text string
		if err := rs.Scan(&id, &text, &bits); err != nil {
			rs.Close()
			return err
		}
		labels, err := l.labels(id, text)
		if err != nil {
			rs.Close()
			return err
		}
		touched := false
		for k, v := range labels {
			if gained[k][v] || every[k] {
				touched = true
				break
			}
		}
		if !touched {
			continue
		}
		if now := sig.Bits(sig.TiesByLabels(labels, match)); now != bits {
			changed = append(changed, retie{id, now})
		}
	}
	rs.Close()
	if err := rs.Err(); err != nil || len(changed) == 0 {
		return err
	}
	up, err := t.updating("series", []string{"signals"})
	if err != nil {
		return err
	}
	defer up.close()
	ids := make([]int64, 0, len(changed))
	for _, c := range changed {
		if err := up.add(c.bits, c.id); err != nil {
			return err
		}
		ids = append(ids, c.id)
	}
	if err := up.close(); err != nil {
		return err
	}
	if retied != nil {
		retied(len(ids))
	}
	lo, hi, err := seriesSpan(t, ids)
	if err != nil || lo > hi {
		return err
	}
	return pend(t, [][2]int64{{lo, hi}})
}

// retied, when set, is told how many alerts' signals a look at what the
// signals saw changed: for tests of what it writes.
var retied func(n int)

// sigSeenText is what a signal's row keeps of what it has seen, as text.
func sigSeenText(t *txn, name string) (string, error) {
	var v string
	err := t.QueryRowContext(t.ctx, "SELECT seen FROM impact_signals WHERE name = ?", name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// AddSeen keeps what named signals have read, by name (seen from what a
// signal reads: docs/design/signals.md, "The record, signal by signal"),
// each selector's series counted by the signal's match labels over the
// read, and how far each was read (to); and has the alerts that may be
// tied otherwise worked out again.
func (l *Ledger) AddSeen(ctx context.Context, match []string, more map[string]*verdict.Seen, to map[string]time.Time) error {
	return l.write(ctx, func(t *txn) error {
		for _, name := range slices.Sorted(maps.Keys(to)) {
			if _, err := t.ExecContext(t.ctx, "UPDATE impact_signals SET seen_to = ? WHERE name = ?", to[name].Unix(), name); err != nil {
				return err
			}
		}
		return l.addSeen(t, match, more)
	})
}

// SeenState is what a named signal has seen, of the labels it is matched
// on, and how far its selectors were read for it, from its first read:
// zero when what it kept was of other labels, and is begun again.
type SeenState struct {
	Seen *verdict.Seen
	To   time.Time
}

// SeenOf is what each named signal has seen, by name, of the labels it is
// matched on (its own, or match, the match in effect), and how far its
// selectors were read for it.
func (l *Ledger) SeenOf(ctx context.Context, match []string) (map[string]SeenState, error) {
	seen, _, err := seenOf(ctx, l.r, match)
	if err != nil {
		return nil, err
	}
	rows, err := l.r.QueryContext(ctx, "SELECT name, seen, seen_to FROM impact_signals WHERE name != ''")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SeenState{}
	for rows.Next() {
		var name, text string
		var to int64
		if err := rows.Scan(&name, &text, &to); err != nil {
			return nil, err
		}
		st := SeenState{Seen: seen[name]}
		var kept verdict.Seen
		if to > 0 && (text == "" || json.Unmarshal([]byte(text), &kept) == nil && kept.For(st.Seen.Labels())) {
			st.To = unix(to)
		}
		out[name] = st
	}
	return out, rows.Err()
}

// windowKeyBase is the key a signal's window is given, before -2, -3 …:
// impact- and its start's minute, a named signal's name between them.
func windowKeyBase(name string, in incident.Incident) string {
	if name == "" {
		return "impact-" + in.Start.UTC().Format("20060102T1504Z")
	}
	return "impact-" + name + "-" + in.Start.UTC().Format("20060102T1504Z")
}

// RuleSignals are the sets of signals a rule's alerts are tied to, as the
// ledger keeps them (verdict.SignalBits), each once, in order.
func (l *Ledger) RuleSignals(ctx context.Context, rule string) ([]int64, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT DISTINCT s.signals FROM series s JOIN rules r ON r.id = s.rule_id
		WHERE r.name = ? AND s.signals >= 0 ORDER BY s.signals`, rule)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var b int64
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ImpactWindowsMeeting are the impact windows on at some time in [from,
// to], decided or not, the earliest first: those of other signals beside
// one, as a window's page shows them.
func (l *Ledger) ImpactWindowsMeeting(ctx context.Context, from, to time.Time) ([]Incident, error) {
	return l.incidents(ctx, `SELECT `+incidentCols+` FROM incidents i WHERE i.origin = 'impact' AND i.start_at <= ? AND i.end_at >= ?
		ORDER BY i.start_at, i.key LIMIT 200`, to.Unix(), from.Unix())
}
