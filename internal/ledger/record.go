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
	"strings"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// Rule is what the ledger knows of a rule besides its firings.
type Rule struct {
	Name, Group, Severity string
	// Unsupported is why it couldn't be replayed the last time it was.
	Unsupported string
	// Source is, for a rule whose firings are recorded elsewhere and copied
	// in, the source they come from; "" for a replayed rule.
	Source string
}

// Found is what a run found over the period [From, To].
type Found struct {
	From, To time.Time
	// Missed is, by rule, an earlier From a run before couldn't replay the
	// rule from: its firings are replaced from there.
	Missed map[string]time.Time
	// Since is, by rule, how long before its From its replay started, so
	// as to see a firing's pending time; its From for a rule not in it. A
	// firing that started before its From continues one the ledger has.
	Since map[string]time.Time
	// Fresh is, by rule, a time after which a firing the replay saw start
	// started then, pending in the replay and not already on before it:
	// its start plus the rule's longest "for" and group interval.
	Fresh map[string]time.Time
	// Rules are the rules the run knew of.
	Rules []Rule
	// Keep are rules the run couldn't replay this time, a query failing or
	// time running out: their firings are kept as they were, and the next
	// run replays them from this one's From.
	Keep map[string]bool
	// Firings are every rule's firings, including those that started
	// before From and were still on at it.
	Firings []firing.Firing
	// Impact, when there is an impact expression, is what it found from
	// ImpactSince on, read with ImpactCriterion (the expression and
	// --impact-for, as lapilli serve keys them; "" for one not told
	// apart, as before 0.49).
	Impact          []incident.Incident
	ImpactOn        bool
	ImpactSince     time.Time
	ImpactCriterion string
	// ImpactAssumed is the criterion a ledger that kept none (an earlier
	// lapilli's) is taken to have been read with: the command line's.
	ImpactAssumed string
	// Signals are the named impact signals the run read, each on its own
	// (docs/design/signals.md): with them, the unnamed signal isn't read,
	// and the run turns it off.
	Signals []SignalRead
	Run     Run
}

// SignalRead is a named impact signal as a run read it, with Criterion
// (its "for" and expression, as lapilli serve keys them): from Since on,
// its windows from From on, the run's From or where a run before missed
// it, within a week. Missed says it couldn't be read: a later run reads it
// from the earliest From it was missed at, and meanwhile its windows, and
// the record it keeps, stand. To, when it isn't zero and is before the
// run's To, is where a read catching up after a miss, a piece at a time,
// goes on from (PieceResume, a little before where its last piece ended):
// a later run reads it from there, as from where it was missed. Seen is what its series returned,
// of the labels it is matched on.
type SignalRead struct {
	Name, Criterion string
	Match           []string
	Since, From, To time.Time
	Windows         []incident.Incident
	Missed          bool
	Seen            *verdict.Seen
}

// signalRun is a signal as a run, or a read again, read it: its name, ""
// for the unnamed signal, and match; whether it was read; from when
// (since), its windows replacing those found before from from on; the
// criterion it was read with, and, of a ledger an earlier lapilli kept,
// the one taken to have been read before.
type signalRun struct {
	name      string
	match     []string
	on        bool
	since     time.Time
	from      time.Time
	criterion string
	assumed   string
	// lead is the lead firings are judged with (verdict.Options): of a
	// named signal's window going on with other labels, its firings before
	// from are pended to be judged again from its start less it.
	lead time.Duration
}

// unnamed is the unnamed signal as f read it.
func (f Found) unnamed() signalRun {
	return signalRun{on: f.ImpactOn, since: f.ImpactSince, from: f.From, criterion: f.ImpactCriterion, assumed: f.ImpactAssumed}
}

// from is where a rule's firings are replaced from.
func (f Found) from(rule string) time.Time {
	if m, ok := f.Missed[rule]; ok && m.Before(f.From) {
		return m
	}
	return f.From
}

func (f Found) since(rule string) time.Time {
	if s, ok := f.Since[rule]; ok {
		return s
	}
	return f.from(rule)
}

// Run is one run, kept so the runs can be looked back on.
type Run struct {
	ID                int64
	Kind              string // backfill, hourly, review, digest
	Started, Finished time.Time
	From, To          time.Time
	Firings           int
	Failed            map[string]string // rule to why it couldn't be replayed
	Error             string
}

// missedFor is how long a rule's missed period is held for it: past that,
// it is given up.
const missedFor = 7 * 24 * time.Hour

// Record puts what a run found in the ledger, in place of what earlier runs
// found from From on, gives the stones again where they may have changed,
// and moves the cursor to To. It all happens, or none of it does.
func (l *Ledger) Record(ctx context.Context, f Found, o verdict.Options) error {
	return l.write(ctx, func(t *txn) error {
		// A recorded rule holding a name the run judges a rule under is
		// named apart first.
		seen := make(map[string]bool, len(f.Rules))
		var names []string
		for _, r := range f.Rules {
			if !seen[r.Name] {
				seen[r.Name] = true
				names = append(names, r.Name)
			}
		}
		for _, x := range f.Firings {
			if !seen[x.Rule] {
				seen[x.Rule] = true
				names = append(names, x.Rule)
			}
		}
		list, _, err := l.latestTies(t.ctx, t)
		if err != nil {
			return err
		}
		kept, err := l.tie(t, list)
		if err != nil {
			return err
		}
		if err := l.yield(t, names, kept); err != nil {
			return err
		}
		if err := l.rules(t, f.Rules); err != nil {
			return err
		}
		to := f.To.Unix()
		lo := f.From.Unix()
		for name := range f.Missed {
			if !f.Keep[name] {
				lo = min(lo, f.from(name).Unix())
			}
		}
		// The alerts of the firings below, found or made at once, in the
		// order the loops below come to them, as SQLite makes them one at a
		// time: their rules' ids and theirs are the same in both, and so
		// are ties broken by id.
		if t.d == postgres {
			var keys []seriesKey
			for _, continued := range []bool{true, false} {
				for _, x := range f.Firings {
					before := x.Start.Before(f.from(x.Rule))
					if f.Keep[x.Rule] || before != continued || !before && x.Start.After(f.To) {
						continue
					}
					rule, err := l.ruleID(t, x.Rule)
					if err != nil {
						return err
					}
					k, err := keyOf(t.d, rule, x.Labels)
					if err != nil {
						return err
					}
					keys = append(keys, k)
				}
			}
			if err := l.findSeries(t, keys); err != nil {
				return err
			}
		}
		// Firings that started before their rule's From: an alert the
		// ledger has as still on, or one that started in the margin and was
		// found before.
		continued := map[int64]bool{}
		for _, x := range f.Firings {
			if !x.Start.Before(f.from(x.Rule)) || f.Keep[x.Rule] {
				continue
			}
			rule, err := l.ruleID(t, x.Rule)
			if err != nil {
				return err
			}
			sid, err := l.seriesID(t, rule, x.Labels)
			if err != nil {
				return err
			}
			var id, start int64
			// The same firing, found before; or, for one the replay saw
			// only from where it started, already on, the one on then.
			s := x.Start.Unix()
			if fresh, ok := f.Fresh[x.Rule]; ok && x.Start.After(fresh) {
				err = t.QueryRowContext(t.ctx, "SELECT id, start_at FROM firings WHERE series_id = ? AND start_at = ? LIMIT 1", sid, s).Scan(&id, &start)
			} else {
				err = t.QueryRowContext(t.ctx, `SELECT id, start_at FROM firings WHERE series_id = ? AND start_at <= ?
					AND (start_at = ? OR end_at IS NULL OR end_at >= ?) ORDER BY start_at = ? DESC, start_at DESC LIMIT 1`,
					sid, s, s, f.since(x.Rule).Unix(), s).Scan(&id, &start)
			}
			switch {
			case errors.Is(err, sql.ErrNoRows):
				if id, err = l.insert(t, rule, sid, x); err != nil {
					return err
				}
				lo = min(lo, s)
			case err != nil:
				return err
			default:
				if _, err := t.ExecContext(t.ctx, "UPDATE firings SET end_at = ? WHERE id = ?", nullable(x.End), id); err != nil {
					return err
				}
			}
			continued[id] = true
		}
		// An alert the ledger has as still on, not found on now, resolved
		// before the replay started. A recorded one ends when its source
		// says so.
		rows, err := t.QueryContext(t.ctx, `SELECT f.id, f.start_at, r.name FROM firings f JOIN rules r ON r.id = f.rule_id
			WHERE f.end_at IS NULL AND f.start_at < ? AND r.source = ''`, f.From.Unix())
		if err != nil {
			return err
		}
		ended := map[int64]int64{}
		for rows.Next() {
			var id, start int64
			var name string
			if err := rows.Scan(&id, &start, &name); err != nil {
				rows.Close()
				return err
			}
			if continued[id] || f.Keep[name] || start >= f.from(name).Unix() {
				continue
			}
			ended[id] = f.since(name).Unix()
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for id, at := range ended {
			if _, err := t.ExecContext(t.ctx, "UPDATE firings SET end_at = "+t.d.greatest("start_at", "?")+" WHERE id = ?", at, id); err != nil {
				return err
			}
		}

		// From From on, the run's firings replace what was there; for a
		// rule an earlier run missed, from where it missed. Recorded
		// firings are their source's to replace.
		keep := make([]string, 0, len(f.Keep))
		for name := range f.Keep {
			keep = append(keep, name)
		}
		slices.Sort(keep)
		keepJSON, _ := json.Marshal(keep)
		if _, err := t.ExecContext(t.ctx, `DELETE FROM firings WHERE start_at >= ? AND start_at <= ?
			AND rule_id NOT IN (`+t.d.pick("SELECT r.id FROM rules r, json_each(?) k WHERE r.name = k.value",
			"SELECT id FROM rules WHERE name IN ("+t.d.texts("?")+")")+`)
			AND rule_id NOT IN (SELECT id FROM rules WHERE source != '')`, f.From.Unix(), to, string(keepJSON)); err != nil {
			return err
		}
		for name := range f.Missed {
			if f.Keep[name] || !f.from(name).Before(f.From) {
				continue
			}
			if _, err := t.ExecContext(t.ctx, `DELETE FROM firings WHERE rule_id = (SELECT id FROM rules WHERE name = ? AND source = '')
				AND start_at >= ? AND start_at < ?`, name, f.from(name).Unix(), f.From.Unix()); err != nil {
				return err
			}
		}
		n := 0
		ins, err := t.inserting("firings", []string{"series_id", "rule_id", "start_at", "end_at", "day"}, []string{"BIGINT", "BIGINT", "BIGINT", "BIGINT", "BIGINT"})
		if err != nil {
			return err
		}
		for _, x := range f.Firings {
			if x.Start.Before(f.from(x.Rule)) || x.Start.After(f.To) || f.Keep[x.Rule] {
				continue
			}
			rule, err := l.ruleID(t, x.Rule)
			if err != nil {
				ins.close()
				return err
			}
			sid, err := l.seriesID(t, rule, x.Labels)
			if err != nil {
				ins.close()
				return err
			}
			if err := ins.add(sid, rule, x.Start.Unix(), nullable(x.End), l.day(x.Start.Unix())); err != nil {
				ins.close()
				return err
			}
			n++
		}
		if err := ins.close(); err != nil {
			return err
		}

		changed, err := l.criterionChanged(t, f.unnamed())
		if err != nil {
			return err
		}
		if f.ImpactOn {
			// Read with another criterion than the runs before, a window
			// on at the start is this one's own, not one of theirs going on.
			ilo, err := l.impact(t, f.unnamed(), f.Impact, changed)
			if err != nil {
				return err
			}
			lo = min(lo, ilo)
		}
		if err := l.impactSince(t, f.unnamed()); err != nil {
			return err
		}
		for _, r := range f.Signals {
			ilo, err := l.signalRecord(t, f, r, o)
			if err != nil {
				return err
			}
			lo = min(lo, ilo)
		}
		if err := l.judge(t, lo-int64(o.Lead/time.Second), to, o); err != nil {
			return err
		}
		if err := l.miss(t, f); err != nil {
			return err
		}
		f.Run.Firings = n
		if err := l.addRun(t, f.Run); err != nil {
			return err
		}
		return setMeta(t.ctx, t, "cursor", strconv.FormatInt(to, 10))
	})
}

// impactNone is impact_since once a run didn't read the impact signal.
const impactNone = "none"

// impactSince keeps from when the runs read the impact signal (--impact),
// all of it, without a run between that didn't: what keeps the record of
// the firings since (ImpactSince). A run that doesn't read it ends that
// (impactNone), and forgets the criteria kept and read, the data and the
// floor; the first to read it with none kept (a new ledger's), keeps its
// own start and its criterion. An earlier window found isn't taken for
// one: the runs between may not have read it. Read again after a run that
// didn't (turned on again), it keeps its own start too, and the weeks
// before wait to be read again (againDue). A ledger an earlier lapilli
// kept, which kept no criterion, is taken to have been read with
// ImpactAssumed.
//
// A run that read another criterion than the one the record is kept by
// leaves the record's start, its criterion and its data as they were, and
// keeps a floor at its start (impact_floor): from there the windows are
// another's, and no record is kept until the criterion in effect is read
// again (ReadAgain). More such runs, of the same criterion or others, or
// of the one kept again, leave it where it is, or move it earlier, to the
// start of one that replays from before it. impact_read is what each run
// read.
//
// A since is never earlier than where the criterion's data begins
// (ImpactData), when that is known; one earlier than where it was looked
// for from forgets the data, to be looked for again from there.
//
// Each signal keeps its own, in its row of impact_signals: the unnamed
// signal's, as meta kept it before schema 022, and each named one's.
func (l *Ledger) impactSince(t *txn, r signalRun) error {
	was, err := sigMeta(t.ctx, t, r.name, metaImpactSince)
	if err != nil {
		return err
	}
	others := []string{metaImpactCriterion, metaImpactAgain, metaImpactData, metaImpactRead, metaImpactFloor}
	if !r.on {
		n, err := sigMetaSet(t, r.name, others...)
		if err != nil {
			return err
		}
		if was == impactNone && n == 0 {
			return nil
		}
		if err := delSigMeta(t, r.name, others...); err != nil {
			return err
		}
		return setSigMeta(t, r.name, metaImpactSince, impactNone)
	}
	crit, err := sigMeta(t.ctx, t, r.name, metaImpactCriterion)
	if err != nil {
		return err
	}
	n, perr := strconv.ParseInt(was, 10, 64)
	kept := perr == nil // a record is kept, by crit, or of an earlier lapilli's
	if kept && crit == "" && r.assumed != "" {
		crit = r.assumed
		if err := setSigMeta(t, r.name, metaImpactCriterion, crit); err != nil {
			return err
		}
	}
	read := r.criterion
	if read == "" {
		read = crit // a run from before 0.49, which doesn't say
	}
	if err := l.setSigMetaIf(t, r.name, metaImpactRead, read); err != nil {
		return err
	}
	if read != "" && kept && crit != read {
		floor, err := sigMeta(t.ctx, t, r.name, metaImpactFloor)
		if err != nil {
			return err
		}
		if at, err := strconv.ParseInt(floor, 10, 64); err == nil && at <= r.from.Unix() {
			return nil
		}
		return setSigMeta(t, r.name, metaImpactFloor, strconv.FormatInt(r.from.Unix(), 10))
	}
	if read != "" && crit != read {
		if err := setSigMeta(t, r.name, metaImpactCriterion, read); err != nil {
			return err
		}
	}
	if was == impactNone && read != "" {
		if err := setSigMeta(t, r.name, metaImpactAgain, againDue); err != nil {
			return err
		}
	}
	since := r.since.Unix()
	if kept && n <= since {
		return nil
	}
	d, err := l.impactData(t.ctx, t, r.name)
	if err != nil {
		return err
	}
	switch {
	case d == nil || d.Criterion != read:
	case since < d.From.Unix():
		// Read from before where its data was looked for (a run that
		// replays from further back than a read again began): nothing is
		// known of the weeks between, so it is looked for again from the
		// new start, and no record is kept until it is (ImpactKept.From).
		if err := delSigMeta(t, r.name, metaImpactData); err != nil {
			return err
		}
	case !d.Floor.IsZero():
		since = max(since, d.Floor.Unix())
	}
	return setSigMeta(t, r.name, metaImpactSince, strconv.FormatInt(since, 10))
}

// setSigMetaIf sets what a signal's row keeps under a meta key to v, when
// it isn't already: a run that reads what the one before read writes
// nothing more.
func (l *Ledger) setSigMetaIf(t *txn, name, key, v string) error {
	if v == "" {
		return nil
	}
	was, err := sigMeta(t.ctx, t, name, key)
	if err != nil || was == v {
		return err
	}
	return setSigMeta(t, name, key, v)
}

// signalRecord keeps what a run read of a named signal (SignalRead): its
// windows in place of its own from where it read them, what keeps the
// record by it, and what its series returned. One missed is kept from
// where it was first missed, its windows and record standing; one read
// from later than it was missed was given up on, past a week, and begins
// again as one turned on again: its record from this read, and a read
// again due. It returns the earliest start whose firings' stones may have
// changed.
func (l *Ledger) signalRecord(t *txn, f Found, r SignalRead, o verdict.Options) (int64, error) {
	var missed int64
	if err := t.QueryRowContext(t.ctx, "SELECT missed_at FROM impact_signals WHERE name = ?", r.Name).Scan(&missed); errors.Is(err, sql.ErrNoRows) {
		// A signal the ledger doesn't keep (SyncSignals hasn't made its
		// row, with its own bit): nothing of it is kept, its windows nor
		// its record, until it does.
		return f.From.Unix(), nil
	} else if err != nil {
		return 0, err
	}
	if r.Missed {
		if missed == 0 || r.From.Unix() < missed {
			if _, err := t.ExecContext(t.ctx, "UPDATE impact_signals SET missed_at = ? WHERE name = ?", r.From.Unix(), r.Name); err != nil {
				return 0, err
			}
		}
		return f.From.Unix(), nil
	}
	run := signalRun{name: r.Name, match: r.Match, on: true, since: r.Since, from: r.From, criterion: r.Criterion, lead: o.Lead}
	if missed != 0 && r.From.Unix() > missed {
		// Given up on: turned off, and on again by this read.
		if err := l.impactSince(t, signalRun{name: r.Name}); err != nil {
			return 0, err
		}
	}
	if missed != 0 {
		if _, err := t.ExecContext(t.ctx, "UPDATE impact_signals SET missed_at = 0 WHERE name = ?", r.Name); err != nil {
			return 0, err
		}
	}
	// What it reads is read for what it has seen from its first read on
	// (AddSeen), whenever the record by it begins.
	if _, err := t.ExecContext(t.ctx, "UPDATE impact_signals SET seen_to = ? WHERE name = ? AND seen_to = 0", r.Since.Unix(), r.Name); err != nil {
		return 0, err
	}
	changed, err := l.criterionChanged(t, run)
	if err != nil {
		return 0, err
	}
	lo, err := l.impact(t, run, r.Windows, changed)
	if err != nil {
		return 0, err
	}
	if err := l.impactSince(t, run); err != nil {
		return 0, err
	}
	if r.Seen != nil {
		if err := l.addSeen(t, o.Match, map[string]*verdict.Seen{r.Name: r.Seen}); err != nil {
			return 0, err
		}
	}
	if !r.To.IsZero() && r.To.Before(f.To) {
		// Caught up to there, a piece at a time: read from there next
		// (PieceResume).
		if _, err := t.ExecContext(t.ctx, "UPDATE impact_signals SET missed_at = ? WHERE name = ?", r.To.Unix(), r.Name); err != nil {
			return 0, err
		}
	}
	return lo, nil
}

// miss keeps, for the next run, where each rule this run couldn't replay
// was missed from, and forgets those it could.
func (l *Ledger) miss(t *txn, f Found) error {
	missed, err := l.missed(t.ctx, t)
	if err != nil {
		return err
	}
	for name := range f.Keep {
		at := f.from(name)
		if was, ok := missed[name]; !ok || at.Before(was) {
			missed[name] = at
		}
	}
	for _, r := range f.Rules {
		if !f.Keep[r.Name] {
			delete(missed, r.Name)
		}
	}
	for name, at := range missed {
		if f.To.Sub(at) > missedFor {
			delete(missed, name) // given up: too long to keep replaying
		}
	}
	b, err := json.Marshal(missed)
	if err != nil {
		return err
	}
	return setMeta(t.ctx, t, "missed", string(b))
}

// Missed is, by rule, where a run couldn't replay it from, for the next
// run to start there.
func (l *Ledger) Missed(ctx context.Context) (map[string]time.Time, error) {
	return l.missed(ctx, l.r)
}

func (l *Ledger) missed(ctx context.Context, q querier) (map[string]time.Time, error) {
	v, err := l.meta(ctx, q, "missed")
	out := map[string]time.Time{}
	if err != nil || v == "" {
		return out, err
	}
	return out, json.Unmarshal([]byte(v), &out)
}

func (l *Ledger) insert(t *txn, rule, series int64, x firing.Firing) (int64, error) {
	var id int64
	err := t.QueryRowContext(t.ctx, "INSERT INTO firings (series_id, rule_id, start_at, end_at, day) VALUES (?, ?, ?, ?, ?) RETURNING id",
		series, rule, x.Start.Unix(), nullable(x.End), l.day(x.Start.Unix())).Scan(&id)
	return id, err
}

// rules keeps what the run knew of each rule.
func (l *Ledger) rules(t *txn, rs []Rule) error {
	for _, r := range rs {
		id, err := l.ruleID(t, r.Name)
		if err != nil {
			return err
		}
		// A recorded rule's severity is how its source tells it apart: an
		// alarm's history keeps its own.
		if _, err := t.ExecContext(t.ctx, "UPDATE rules SET grp = ?, severity = CASE WHEN source = '' THEN ? ELSE severity END, unsupported = ? WHERE id = ?",
			r.Group, r.Severity, r.Unsupported, id); err != nil {
			return err
		}
	}
	return nil
}

// impact puts the impact windows a run found in place of those found
// before from From on, keeping what people decided about them, and the
// tasks and comments on them (remapImpact). A window on at From goes on
// from the one found before, unless fresh says that one was read with
// another criterion: then the one before ends at From. It returns the
// earliest start whose firings' stones may have changed.
//
// Two rules, of how much a window found now and one found before share:
// a decision carries over at half of the new window (decidedFor), so that
// a window split, or found shorter, keeps what people said of it; a key,
// and the tasks and comments on it, at half of both (sameImpact,
// remapImpact), so that what was written about one window never lands on
// one mostly elsewhere. A window found takes what was decided about one
// found before (confirmed or dismissed) that covers half of it or more,
// sharing a value of each label both have: of several, the one that
// covers most of it, confirmed before dismissed on a tie, then the
// earliest. A decision is read from every window on from From on, one that
// started before it too, as it was before anything was ended there.
//
// A signal's windows are its own (docs/design/signals.md, "Windows: each
// signal's own"): only those of r's signal are put in place of, ended,
// carried on, or have their decisions, keys, tasks and comments taken, and
// a named signal's keys carry its name.
func (l *Ledger) impact(t *txn, r signalRun, found []incident.Incident, fresh bool) (int64, error) {
	from := r.from.Unix()
	lo := from
	rows, err := t.QueryContext(t.ctx, `SELECT key, start_at, end_at, state, labels, updated, updated_by FROM incidents
		WHERE origin = 'impact' AND signal = ? AND (start_at >= ? OR end_at > ?) ORDER BY start_at, key`, r.name, from, from)
	if err != nil {
		return 0, err
	}
	var before []oldWindow
	for rows.Next() {
		var o oldWindow
		var labels string
		if err := rows.Scan(&o.key, &o.start, &o.end, &o.state, &labels, &o.updated, &o.by); err != nil {
			rows.Close()
			return 0, err
		}
		if err := json.Unmarshal([]byte(labels), &o.labels); err != nil {
			rows.Close()
			return 0, err
		}
		before = append(before, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if fresh {
		// Read with another criterion: one found before and on at From
		// ends there, and judged again from its start.
		for _, o := range before {
			if o.start < from {
				lo = min(lo, o.start)
			}
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE incidents SET end_at = ? WHERE origin = 'impact' AND signal = ? AND start_at < ? AND end_at > ?", from, r.name, from, from); err != nil {
			return 0, err
		}
	}
	if _, err := t.ExecContext(t.ctx, "DELETE FROM catches WHERE incident_id IN (SELECT id FROM incidents WHERE origin = 'impact' AND signal = ? AND start_at >= ?)", r.name, from); err != nil {
		return 0, err
	}
	if _, err := t.ExecContext(t.ctx, "DELETE FROM incidents WHERE origin = 'impact' AND signal = ? AND start_at >= ?", r.name, from); err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	var made []madeWindow
	// The keys of the windows taken away: each for the window found now
	// that is the same one, and for no other.
	taken, reserved := map[string]bool{}, map[string]bool{}
	for _, o := range before {
		if o.start >= from {
			reserved[o.key] = true
		}
	}
	for _, in := range found {
		if r.name != "" {
			// A named signal's, bounded; the unnamed signal's as every
			// release before kept them, so that a window found again is
			// known by its labels, and keeps its key and what was decided.
			in.Labels = keptWindowLabels(in.Labels)
		}
		labels, err := json.Marshal(orEmpty(in.Labels))
		if err != nil {
			return 0, err
		}
		if in.Start.Before(r.from) && !fresh && r.name != "" {
			// A named signal's, found from before where it reads: the
			// window found before with its labels that it meets goes on, to
			// the later end of the two (the same one, found again in the
			// hour before); one of none such, which began too late to be
			// found then (not yet on for its for), is put in as new.
			key, start, end, was, err := goesOn(t, r, from, in)
			if err != nil {
				return 0, err
			}
			if key != "" {
				// With every value of the labels both have, as windows
				// found together that overlap are one (incident.FromImpact).
				merged, err := json.Marshal(orEmpty(keptWindowLabels(mergeLabels(was, in.Labels))))
				if err != nil {
					return 0, err
				}
				before, err := json.Marshal(orEmpty(was))
				if err != nil {
					return 0, err
				}
				if in.End.Unix() > end || string(merged) != string(before) {
					if _, err := t.ExecContext(t.ctx, "UPDATE incidents SET end_at = ?, labels = ? WHERE key = ?", max(in.End.Unix(), end), string(merged), key); err != nil {
						return 0, err
					}
				}
				// A later end changes no stone before from, and the run
				// judges from from on. Other labels change those of its
				// firings before from too: judged again a week a write
				// (JudgePending), not in this one, which a window on for
				// weeks would hold.
				if string(merged) != string(before) && start-int64(r.lead/time.Second) < from {
					if err := pend(t, [][2]int64{{start - int64(r.lead/time.Second), from}}); err != nil {
						return 0, err
					}
				}
				continue
			}
		} else if in.Start.Before(r.from) && !fresh {
			// Still going at the start: the window found before goes on.
			var key string
			err := t.QueryRowContext(t.ctx, `SELECT key FROM incidents WHERE origin = 'impact' AND signal = ? AND start_at < ? AND end_at >= ?
				ORDER BY start_at DESC LIMIT 1`, r.name, from, r.since.Unix()).Scan(&key)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
			if err == nil {
				if _, err := t.ExecContext(t.ctx, "UPDATE incidents SET end_at = ? WHERE key = ?", in.End.Unix(), key); err != nil {
					return 0, err
				}
				continue
			}
		}
		if in.Start.Before(r.from) {
			lo = min(lo, in.Start.Unix())
		}
		state, updated, by := "", now, ""
		if d := decidedFor(before, in); d != nil {
			state, updated, by = d.state, d.updated, d.by
		}
		// The key of the window found before it is, if one is: of the same
		// minute, sharing a value of each label both have, the two sharing
		// half of each. Else one no window has, nor anything kept on one
		// (freeKey).
		key := sameImpact(before, from, taken, r.name, in)
		if key == "" {
			if key, err = freeKey(t, windowKeyBase(r.name, in), reserved); err != nil {
				return 0, err
			}
		}
		taken[key] = true
		if _, err := t.ExecContext(t.ctx, `INSERT INTO incidents (key, origin, state, counts, title, start_at, end_at, labels, updated, updated_by, signal, match_labels)
			VALUES (?, 'impact', ?, 0, ?, ?, ?, ?, ?, ?, ?, ?)`,
			key, state, in.Title, in.Start.Unix(), in.End.Unix(), string(labels), updated, by, r.name, signalMatch(r)); err != nil {
			return 0, err
		}
		made = append(made, madeWindow{key, in})
	}
	return lo, remapImpact(t, before, from, made)
}

// goesOn is the window of r's signal, begun before from, that in, found
// from before from too, is: with labels in shares a value of each of
// (share), and on at some time in it (its end at in's start or later, its
// start by in's end), the latest begun of several; its key, start, end
// and labels, or "" for none.
func goesOn(t *txn, r signalRun, from int64, in incident.Incident) (string, int64, int64, incident.Labels, error) {
	rows, err := t.QueryContext(t.ctx, `SELECT key, start_at, end_at, labels FROM incidents WHERE origin = 'impact' AND signal = ? AND start_at < ? AND end_at >= ? AND start_at <= ?
		ORDER BY start_at DESC, key`, r.name, from, in.Start.Unix(), in.End.Unix())
	if err != nil {
		return "", 0, 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, labels string
		var start, end int64
		if err := rows.Scan(&key, &start, &end, &labels); err != nil {
			return "", 0, 0, nil, err
		}
		var l incident.Labels
		if err := json.Unmarshal([]byte(labels), &l); err != nil {
			return "", 0, 0, nil, err
		}
		if share(l, in.Labels) {
			return key, start, end, l, nil
		}
	}
	return "", 0, 0, nil, rows.Err()
}

// mergeLabels are a and b as one window's, as incident.FromImpact merges
// windows that overlap: the labels both have, with every value of each,
// in order.
func mergeLabels(a, b incident.Labels) incident.Labels {
	out := incident.Labels{}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			continue
		}
		vs := slices.Clone(va)
		for _, v := range vb {
			if !slices.Contains(vs, v) {
				vs = append(vs, v)
			}
		}
		slices.Sort(vs)
		out[k] = vs
	}
	return out
}

// PieceResume is where a read of a named signal catching up after a miss,
// in pieces, goes on from after a turn's last piece ended at end: before
// it by the signal's for and a step, so that a window begun within it,
// not yet on for its for at end, is found whole by the next, as a run's
// read is after the run before; by settle at most, as runs overlap, so
// that each turn gets further; on the step's grid.
func PieceResume(end time.Time, signalFor, settle, step time.Duration) time.Time {
	// On the step's grid, as a run's from is, the overlap only longer: a
	// range is evaluated at its start, and a step on from it.
	return end.Add(-min(signalFor+step, max(settle, 0))).Truncate(step)
}

// keptWindowLabels are a named signal's window's labels as the ledger
// keeps them, bounded as an incident's are (maxLabelBytes in all, as
// JSON): each name and value cut as an alert's are (verdict.KeptName,
// KeptValue), a label of more than 64 values left out
// (incident.MaxWindowValues, as a window is found with), and, past 4 KB,
// the labels that don't fit left out, the rest kept in order: a label left
// out constrains nothing, so the window is caught by more, never by fewer.
// Within the bounds, as they are. A label's values are kept as a window's
// are found (incident.KeepValue): each looked for among the 64 kept, never
// among all of them.
func keptWindowLabels(l incident.Labels) incident.Labels {
	within := true
	for k, vs := range l {
		within = within && verdict.KeptName(k) == k && len(vs) <= incident.MaxWindowValues && !slices.ContainsFunc(vs, func(v string) bool { return verdict.KeptValue(v) != v })
	}
	if within {
		if b, err := json.Marshal(orEmpty(l)); err == nil && len(b) <= maxLabelBytes {
			return l
		}
	}
	out := incident.Labels{}
	size := 2
	for _, k := range slices.Sorted(maps.Keys(l)) {
		kk := verdict.KeptName(k)
		if _, taken := out[kk]; taken {
			continue
		}
		// Each value cut, in order; past 64 none: the same whether a
		// window's values came at once or a piece at a time (mergeLabels).
		var vs []string
		ok := true
		for _, v := range l[k] {
			if vs, ok = incident.KeepValue(vs, verdict.KeptValue(v)); !ok {
				break
			}
		}
		if !ok {
			continue
		}
		b, _ := json.Marshal(map[string][]string{kk: vs})
		if size+len(b)-1 > maxLabelBytes {
			continue
		}
		out[kk], size = vs, size+len(b)-1
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// signalMatch is the labels a signal's windows are matched on, as a column
// keeps them: NULL for the unnamed signal's, and a named one's with none
// of its own, which follow the match in effect.
func signalMatch(r signalRun) any {
	if r.name == "" {
		return nil
	}
	return matchText(r.match)
}

// oldWindow is an impact window as it was before a run or a read again
// put others in its place.
type oldWindow struct {
	key                 string
	start, end, updated int64
	state, by           string
	labels              incident.Labels
}

// madeWindow is a window a run or a read again put in, by its key.
type madeWindow struct {
	key string
	in  incident.Incident
}

// overlap is how many seconds [a, b] and [c, d] share.
func overlap(a, b, c, d int64) int64 { return max(0, min(b, d)-max(a, c)) }

// halfOfBoth reports whether [a, b] and [c, d] share half of each, or
// more: what a key, and the tasks and comments on it, need to pass from a
// window to another.
func halfOfBoth(a, b, c, d int64) bool {
	ov := overlap(a, b, c, d)
	return b > a && d > c && 2*ov >= b-a && 2*ov >= d-c
}

// decidedFor is what was decided about the windows found before that holds
// for a window found now (impact): nil for none.
func decidedFor(before []oldWindow, in incident.Incident) *oldWindow {
	s, e := in.Start.Unix(), in.End.Unix()
	var best *oldWindow
	var most int64
	for i := range before {
		d := &before[i]
		ov := overlap(d.start, d.end, s, e)
		if d.state == "" || 2*ov < e-s || e <= s || !share(d.labels, in.Labels) {
			continue
		}
		if best == nil || ov > most || ov == most && d.state == "confirmed" && best.state != "confirmed" {
			best, most = d, ov
		}
	}
	return best
}

// sameImpact is the key of the window found before from from on that a
// window found now is: it started in the same minute (its key is the
// minute's, or that with -2, -3 …), shares a value of each label both have,
// and the two share half of each (halfOfBoth); "" for none, or one taken
// already.
func sameImpact(before []oldWindow, from int64, taken map[string]bool, name string, in incident.Incident) string {
	base := windowKeyBase(name, in)
	s, e := in.Start.Unix(), in.End.Unix()
	for _, o := range before {
		if o.start < from || taken[o.key] || o.key != base && !strings.HasPrefix(o.key, base+"-") {
			continue
		}
		if halfOfBoth(o.start, o.end, s, e) && share(o.labels, in.Labels) {
			return o.key
		}
	}
	return ""
}

// freeKey is base, or, when it is taken, base with -2, -3 … after it: taken
// by a window of the ledger, a window taken away (reserved), or a task or a
// comment kept on it, so that what was kept on one window never lands on
// another.
func freeKey(t *txn, base string, reserved map[string]bool) (string, error) {
	key := base
	for n := 2; ; n++ {
		var x int
		err := t.QueryRowContext(t.ctx, `SELECT 1 FROM incidents WHERE key = ?1
			UNION ALL SELECT 1 FROM tasks WHERE incident = ?1
			UNION ALL SELECT 1 FROM comments WHERE on_kind = 'incident' AND on_key = ?1 LIMIT 1`, key).Scan(&x)
		if errors.Is(err, sql.ErrNoRows) && !reserved[key] {
			return key, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		key = fmt.Sprintf("%s-%d", base, n)
	}
}

// remapImpact moves the tasks and comments on a window found before from
// from on that is no longer there (no window found now is the same one,
// sameImpact) to the window found now that covers most of it, sharing a
// value of each label both have, the two sharing half of each
// (halfOfBoth), whether or not it took its decision (decidedFor holds a
// decision to half the new window alone). One that none is so near keeps
// its key, which no window gets again (freeKey), and the Cleanup page
// lists it (OrphanedImpact).
func remapImpact(t *txn, before []oldWindow, from int64, made []madeWindow) error {
	keys := map[string]bool{}
	for _, m := range made {
		keys[m.key] = true
	}
	for _, o := range before {
		if o.start < from || keys[o.key] {
			continue
		}
		var n int
		if err := t.QueryRowContext(t.ctx, `SELECT (SELECT count(*) FROM tasks WHERE incident = ?) +
			(SELECT count(*) FROM comments WHERE on_kind = 'incident' AND on_key = ?)`, o.key, o.key).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		to, most := "", int64(0)
		for _, m := range made {
			ov := overlap(o.start, o.end, m.in.Start.Unix(), m.in.End.Unix())
			if ov > most && share(o.labels, m.in.Labels) && halfOfBoth(o.start, o.end, m.in.Start.Unix(), m.in.End.Unix()) {
				to, most = m.key, ov
			}
		}
		if to == "" {
			continue
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE tasks SET incident = ? WHERE incident = ?", to, o.key); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE comments SET on_key = ? WHERE on_kind = 'incident' AND on_key = ?", to, o.key); err != nil {
			return err
		}
	}
	return nil
}

// share reports whether, for every label both have, they have a value in
// common.
func share(a, b incident.Labels) bool {
	for k, va := range a {
		if vb, ok := b[k]; ok && !slices.ContainsFunc(va, func(v string) bool { return slices.Contains(vb, v) }) {
			return false
		}
	}
	return true
}

func orEmpty(l incident.Labels) incident.Labels {
	if l == nil {
		return incident.Labels{}
	}
	return l
}

func (l *Ledger) addRun(t *txn, r Run) error {
	failed, err := json.Marshal(orEmptyMap(r.Failed))
	if err != nil {
		return err
	}
	_, err = t.ExecContext(t.ctx, `INSERT INTO runs (kind, started, finished, period_from, period_to, firings, failed, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, r.Kind, r.Started.Unix(), r.Finished.Unix(), unixOr0(r.From), unixOr0(r.To),
		r.Firings, string(failed), r.Error)
	return err
}

// AddRun keeps a run that recorded nothing: one that failed, a digest, a
// review.
func (l *Ledger) AddRun(ctx context.Context, r Run) error {
	return l.write(ctx, func(t *txn) error { return l.addRun(t, r) })
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func unixOr0(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// severities joins a rule's severities, as verdict.Judge does.
func Severities(ss []string) string {
	var out []string
	for _, s := range ss {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return strings.Join(out, "/")
}
