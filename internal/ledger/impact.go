package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// What the impact signal was read with, and from when (docs/design/serve.md,
// "The impact criterion"), kept in meta: impact_since, from when the runs
// read it, all of it (verdict.Options.ImpactFrom); impact_criterion, the
// criterion the record is kept by, the expression and --impact-for as
// lapilli serve keys them; impact_again, the criterion the weeks before the
// runs' first read of it were read again with (ReadAgain), or found needing
// none, againDue while they wait to be; impact_data, when each selector
// the criterion reads first had data (ImpactData); impact_read, the
// criterion the last run read; and impact_floor, from when no record is
// kept, while the runs read another criterion than the one kept, until the
// one in effect is read again. A lapilli before 0.49 kept only the first.
const (
	metaImpactSince     = "impact_since"
	metaImpactCriterion = "impact_criterion"
	metaImpactAgain     = "impact_again"
	metaImpactData      = "impact_data"
	metaImpactRead      = "impact_read"
	metaImpactFloor     = "impact_floor"
	// againDue is impact_again while the weeks before a new criterion's
	// first read wait to be read again with it.
	againDue = "-"
)

// ImpactKept is what keeps the record by the impact signal, as the ledger
// has it: Since, from when the runs read it, all of it, zero for never, or
// not since a run that didn't; Criterion, the criterion the record is kept
// by, "" when none is (never read, or read by a lapilli before 0.49 and
// not since); Again, the criterion the weeks before Since were read again
// with (ReadAgain), or found needing none (MarkAgain), "" for none and "-"
// while it waits; Data, where the data of the selectors Criterion reads
// begins, nil before it was looked for; Read, the criterion the last run
// read, "" before one said; and Floor, from when no record is kept, zero
// for none.
//
// Floor is the start of the earliest run since the last read again that
// read another criterion than Criterion: from there on, the ledger's
// windows are that one's, and nothing read the weeks before with it. It
// stands until a read again of the criterion in effect then (ReadAgain),
// whichever criteria the runs read meanwhile, Criterion again among them.
type ImpactKept struct {
	Since     time.Time
	Criterion string
	Again     string
	Data      *ImpactData
	Read      string
	Floor     time.Time
}

// AgainDone reports whether the weeks before the runs read criterion were
// read again with it, or found needing none.
func (k ImpactKept) AgainDone(criterion string) bool { return k.Again == criterion }

// From is from when the record is kept by the impact signal, by the one
// rule (docs/design/serve.md, "The impact criterion"): it holds only for
// weeks that were read, with data, by the criterion in effect, and, after
// a change, replaced by a successful read of that criterion. Since, never
// before where the data of every selector Criterion reads begins
// (ImpactData.Floor); zero, none at all, before that data was looked for,
// while one of them has none, and while a Floor stands. The record has one
// start and no end (verdict.Options.ImpactFrom), so none is kept before a
// Floor either: what is kept is never more than the rule holds.
func (k ImpactKept) From() time.Time {
	d := k.Data
	if k.Since.IsZero() || !k.Floor.IsZero() || d == nil || d.Criterion != k.Criterion || d.NoData {
		return time.Time{}
	}
	return later(k.Since, d.Floor)
}

// ImpactData is where the data of the selectors a criterion reads begins,
// as lapilli serve looked for it from From to CheckedTo: when each first
// had a sample (Selectors). Floor is the first whole week of the ledger
// after the latest of them, of a selector that had none at From: the
// record is kept by the signal from there at the earliest, as only there
// did every selector it reads have data. Zero when each had data at From.
// NoData says one had none at all, nor its stand-ins, each (Effective):
// the record isn't kept by it.
type ImpactData struct {
	Criterion string         `json:"criterion"`
	Selectors []SelectorData `json:"selectors"`
	From      time.Time      `json:"from"`
	CheckedTo time.Time      `json:"checked_to"`
	Floor     time.Time      `json:"floor,omitzero"`
	NoData    bool           `json:"no_data,omitempty"`
}

// SelectorData is a selector, as written in the expression with its
// offset or @, and when its first sample was found: zero for none yet;
// AtStart, where the look began, so it may have had data before. With For,
// it is a stand-in, read for that selector's sake, made of Of
// (rules.StandIns).
type SelectorData struct {
	Sel     string    `json:"sel"`
	For     string    `json:"for,omitempty"`
	Of      string    `json:"of,omitempty"`
	First   time.Time `json:"first,omitzero"`
	AtStart bool      `json:"at_start,omitempty"`
	// To is how far it was looked for, zero before it was: a look that
	// stopped part of the way goes on from there.
	To time.Time `json:"to,omitzero"`
}

// SelectorsOf are the selectors whose data an impact signal's expression
// is kept by: those it reads (rules.Selectors), then their stand-ins
// (rules.StandIns), none found yet.
func SelectorsOf(expr string) ([]SelectorData, error) {
	sels, err := rules.Selectors(expr)
	if err != nil {
		return nil, err
	}
	stands, err := rules.StandIns(expr)
	if err != nil {
		return nil, err
	}
	out := make([]SelectorData, 0, len(sels)+len(stands))
	for _, sel := range sels {
		out = append(out, SelectorData{Sel: sel})
	}
	for _, x := range stands {
		out = append(out, SelectorData{Sel: x.Sel, For: x.For, Of: x.Of})
	}
	return out, nil
}

// Complete reports whether every selector, and stand-in, had data.
func (d *ImpactData) Complete() bool {
	return !slices.ContainsFunc(d.Selectors, func(x SelectorData) bool { return x.First.IsZero() })
}

// Late is the selector whose data begins the latest, after the look began,
// and the others' earlier: what holds the record back, when more than one
// selector is read and they don't begin together; the first of those that
// begin the latest. "" for none. A selector with no data of its own begins
// where its stand-ins say (Effective).
func (d *ImpactData) Late() string {
	sels := d.Effective()
	var latest time.Time
	for _, x := range sels {
		if x.First.IsZero() {
			return ""
		}
		if !x.AtStart && x.First.After(latest) {
			latest = x.First
		}
	}
	if latest.IsZero() {
		return ""
	}
	late, earlier := "", false
	for _, x := range sels {
		switch {
		case x.AtStart || x.First.Before(latest):
			earlier = true
		case late == "":
			late = x.Sel
		}
	}
	if !earlier {
		return ""
	}
	return late
}

// Floor works d's Floor and NoData out, by the ledger's weeks.
func (l *Ledger) Floor(d *ImpactData) { l.floor(d) }

func (l *Ledger) floor(d *ImpactData) { FloorBy(d, l.WeekOnOrAfter) }

// FloorBy works d's Floor and NoData out, by the one rule, of its
// selectors as they keep the record (Effective): NoData when a selector
// had no data at all; else the latest of when each selector that had none
// at d.From first had some, each moved to onOrAfter it (the ledger's first
// whole week then, or, judging each firing by its own time, that time
// itself); zero when each had data at From.
func FloorBy(d *ImpactData, onOrAfter func(time.Time) time.Time) {
	sels := d.Effective()
	d.Floor = time.Time{}
	d.NoData = slices.ContainsFunc(sels, func(x SelectorData) bool { return x.First.IsZero() })
	if d.NoData {
		return
	}
	for _, x := range sels {
		if !x.AtStart {
			d.Floor = later(d.Floor, onOrAfter(x.First))
		}
	}
}

// Effective are d's selectors as they keep the record, its stand-ins left
// out: each whose stand-ins (SelectorData.For), of those made of a
// selector with data of its own, all had data, from the latest of them,
// when that is before its own data or it has none. The expression reads
// the selectors they are made of, which see what it would, so long as the
// labels it narrows by were there: nothing it matches happened, as a 5xx
// of a service that served none. One whose stand-ins weren't all looked
// for yet keeps its own (Pending).
func (d *ImpactData) Effective() []SelectorData {
	out := d.read()
	for i, b := range out {
		if x, ok, _ := d.stoodIn(b.Sel); ok && (b.First.IsZero() || x.First.Before(b.First)) {
			out[i].First, out[i].AtStart = x.First, x.AtStart
		}
	}
	return out
}

// Pending reports whether a selector of d with data of its own has a
// stand-in not looked for yet as far as that data, which may begin it
// earlier: its start holds for now, but moves impact_since nowhere.
func (d *ImpactData) Pending() bool {
	for _, b := range d.read() {
		if _, _, unknown := d.stoodIn(b.Sel); unknown && !b.First.IsZero() {
			return true
		}
	}
	return false
}

// read are d's selectors, its stand-ins left out.
func (d *ImpactData) read() []SelectorData {
	var out []SelectorData
	for _, x := range d.Selectors {
		if x.For == "" {
			out = append(out, x)
		}
	}
	return out
}

// stoodIn is, of sel's stand-ins made of a selector with data of its own,
// the one whose data begins the latest, when every one had data: each
// reads more series than sel does, the latest the fewest. Unknown, when
// one of them with no data found wasn't looked for yet, or not as far as
// sel's own first sample: none, till it is. A stand-in made of a selector
// with no data, which has none either, says nothing of sel's labels.
func (d *ImpactData) stoodIn(sel string) (x SelectorData, ok, unknown bool) {
	var own time.Time
	for _, b := range d.read() {
		if b.Sel == sel {
			own = b.First
		}
	}
	for _, s := range d.Selectors {
		if s.For != sel || !d.HasData(s.Of) {
			continue
		}
		switch {
		case s.First.IsZero() && (s.To.IsZero() || !own.IsZero() && s.To.Before(own)):
			return SelectorData{}, false, true
		case s.First.IsZero():
			return SelectorData{}, false, false
		case !ok || s.First.After(x.First):
			x, ok = s, true
		}
	}
	return x, ok, false
}

// HasData reports whether d's selector sel had data of its own; "" as
// had.
func (d *ImpactData) HasData(sel string) bool {
	return sel == "" || slices.ContainsFunc(d.Selectors, func(x SelectorData) bool { return x.Sel == sel && x.For == "" && !x.First.IsZero() })
}

// StandInOf is, of a selector of d, the stand-in it begins by
// (Effective), and from when: nothing it matches happened, though what it
// would read was there; "" for none, it beginning by its own data.
func (d *ImpactData) StandInOf(sel string) (string, time.Time) {
	x, ok, _ := d.stoodIn(sel)
	if !ok {
		return "", time.Time{}
	}
	for _, b := range d.read() {
		if b.Sel == sel && !b.First.IsZero() && !x.First.Before(b.First) {
			return "", time.Time{}
		}
	}
	return x.Sel, x.First
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// WeekOnOrAfter is the start of the first whole week of the ledger's at t
// or after it: t, when a week starts there.
func (l *Ledger) WeekOnOrAfter(t time.Time) time.Time {
	w := Week(l.day(t.Unix()))
	if at := unix(l.midnight(w)); !at.Before(t) {
		return at
	}
	return unix(l.midnight(w + 7))
}

// ImpactKept reads what keeps the record by the impact signal with no
// name, in one read (SignalKept).
func (l *Ledger) ImpactKept(ctx context.Context) (ImpactKept, error) { return l.SignalKept(ctx, "") }

// impactData is the ImpactData kept of a signal, nil for none.
func (l *Ledger) impactData(ctx context.Context, q querier, name string) (*ImpactData, error) {
	v, err := sigMeta(ctx, q, name, metaImpactData)
	if err != nil || v == "" {
		return nil, err
	}
	var d ImpactData
	if err := json.Unmarshal([]byte(v), &d); err != nil {
		return nil, nil // looked for again
	}
	return &d, nil
}

// SetImpactData keeps where the data of the criterion the ledger keeps the
// record by the unnamed signal begins (SetSignalData).
func (l *Ledger) SetImpactData(ctx context.Context, d ImpactData) error {
	return l.SetSignalData(ctx, "", d)
}

// SetSignalData keeps where the data of the criterion the ledger keeps the
// record by a signal begins, and moves its since forward to its Floor:
// never back. Of another criterion, it keeps nothing.
func (l *Ledger) SetSignalData(ctx context.Context, name string, d ImpactData) error {
	l.floor(&d)
	return l.write(ctx, func(t *txn) error {
		crit, err := sigMeta(t.ctx, t, name, metaImpactCriterion)
		if err != nil || crit != d.Criterion {
			return err
		}
		b, err := json.Marshal(d)
		if err != nil {
			return err
		}
		if err := setSigMeta(t, name, metaImpactData, string(b)); err != nil {
			return err
		}
		was, err := sigMeta(t.ctx, t, name, metaImpactSince)
		if err != nil {
			return err
		}
		if n, err := strconv.ParseInt(was, 10, 64); err == nil && !d.Floor.IsZero() && n < d.Floor.Unix() && !d.Pending() {
			return setSigMeta(t, name, metaImpactSince, strconv.FormatInt(d.Floor.Unix(), 10))
		}
		return nil
	})
}

// criterionChanged reports whether a run read a signal with another
// criterion than the run before it (lastRead), so that a window on at its
// start is its own, not one of theirs going on. Not when the run says
// none (one from before 0.49), nor when the runs before didn't read it.
func (l *Ledger) criterionChanged(t *txn, r signalRun) (bool, error) {
	if !r.on || r.criterion == "" {
		return false, nil
	}
	last, err := l.lastRead(t, r)
	return last != "" && last != r.criterion, err
}

// lastRead is the criterion the run before r read (impact_read); of a
// ledger whose runs didn't say, the one kept, or, of one read that kept
// none, r.assumed; "" when they read none.
func (l *Ledger) lastRead(t *txn, r signalRun) (string, error) {
	for _, key := range []string{metaImpactRead, metaImpactCriterion} {
		if v, err := sigMeta(t.ctx, t, r.name, key); err != nil || v != "" {
			return v, err
		}
	}
	since, err := sigMeta(t.ctx, t, r.name, metaImpactSince)
	if _, perr := strconv.ParseInt(since, 10, 64); err != nil || perr != nil {
		return "", err
	}
	return r.assumed, nil
}

// ImpactRead is an impact signal read again over [From, To], To where the
// runs have got to (the cursor), with Criterion: the windows it found, and
// where the data of the selectors it reads begins over the span (Data).
// Signal is its name, "" for the unnamed signal; Match, a named signal's
// match, nil for the match in effect; Seen, what its series returned, of
// the labels it is matched on.
type ImpactRead struct {
	From, To  time.Time
	Criterion string
	Windows   []incident.Incident
	Data      ImpactData
	Signal    string
	Match     []string
	Seen      *verdict.Seen
}

// ErrRunsMoved is an ImpactRead the runs went past while it was read: it is
// read again, to where they are.
var ErrRunsMoved = errors.New("the runs moved on while the impact signal was read again")

// ReadAgain puts what the impact signal read again over [r.From, r.To]
// found in place of the windows found before from r.From on, keeping what
// people decided about them and the tasks and comments on them (impact),
// ends a window found before on at r.From there, and pends the span to be
// judged again a week at a time (JudgePending): from then on, the record
// is kept by r.Criterion (impact_since r.From, or, later, where its data
// begins), the ledger's windows to r.To are its own (impact_read), and the
// floor is gone. Before r.From, the windows found before stay, as the
// incidents they were when they were read, and keep the record of the
// weeks near them as an incident does; the signal read with another
// criterion keeps none.
func (l *Ledger) ReadAgain(ctx context.Context, r ImpactRead, o verdict.Options) error {
	r.Data.Criterion = r.Criterion
	l.floor(&r.Data)
	return l.write(ctx, func(t *txn) error {
		cur, err := l.meta(t.ctx, t, "cursor")
		if err != nil {
			return err
		}
		if cur != strconv.FormatInt(r.To.Unix(), 10) {
			return ErrRunsMoved
		}
		from := r.From.Unix()
		lo, err := l.impact(t, signalRun{name: r.Signal, match: r.Match, from: r.From, since: r.From, on: true}, r.Windows, true)
		if err != nil {
			return err
		}
		since := later(r.From, r.Data.Floor)
		b, err := json.Marshal(r.Data)
		if err != nil {
			return err
		}
		for _, k := range []string{metaImpactSince, metaImpactCriterion, metaImpactAgain, metaImpactData, metaImpactRead} {
			v := map[string]string{metaImpactSince: strconv.FormatInt(since.Unix(), 10), metaImpactCriterion: r.Criterion,
				metaImpactAgain: r.Criterion, metaImpactData: string(b), metaImpactRead: r.Criterion}[k]
			if err := setSigMeta(t, r.Signal, k, v); err != nil {
				return err
			}
		}
		if err := delSigMeta(t, r.Signal, metaImpactFloor); err != nil {
			return err
		}
		if r.Signal != "" {
			// What it has seen is of the expression read now: read again
			// from the start of this read (AddSeen), what its windows
			// returned beside it.
			if _, err := t.ExecContext(t.ctx, "UPDATE impact_signals SET seen = '', seen_to = ?, missed_at = 0 WHERE name = ?", r.From.Unix(), r.Signal); err != nil {
				return err
			}
			if r.Seen != nil {
				if err := l.addSeen(t, o.Match, map[string]*verdict.Seen{r.Signal: r.Seen}); err != nil {
					return err
				}
			}
		}
		return pend(t, [][2]int64{{min(lo, from) - int64(o.Lead/time.Second), r.To.Unix()}})
	})
}

// MarkAgain keeps that the weeks before the runs read criterion, of the
// unnamed signal, need no read again (MarkAgainOf).
func (l *Ledger) MarkAgain(ctx context.Context, criterion string) error {
	return l.MarkAgainOf(ctx, "", criterion)
}

// MarkAgainOf keeps that the weeks before the runs read a signal's
// criterion need no read again: the runs read it from their first, or a
// lapilli before 0.49 read it, which kept no criterion, and it is taken
// for the one read now.
func (l *Ledger) MarkAgainOf(ctx context.Context, name, criterion string) error {
	return l.write(ctx, func(t *txn) error {
		if err := setSigMeta(t, name, metaImpactCriterion, criterion); err != nil {
			return err
		}
		return setSigMeta(t, name, metaImpactAgain, criterion)
	})
}

// Where incidents came from (docs/design/serve.md, "Incident sources").

// MaxReceived is how many sources' last writes through the incidents port
// the ledger keeps, OtherSources among them: past it, a source it doesn't
// know is kept as OtherSources, so that ids of no source (a flood of UUIDs)
// don't push the sources it knows out.
const MaxReceived = 64

// OtherSources is where the sources past MaxReceived are kept, together.
const OtherSources = "*"

// metaReceived is, by source, when one last wrote through the incidents
// port, kept with UpdateMeta.
const metaReceived = "incidents-received"

// NoteReceived keeps that source wrote through the incidents port at at:
// what tells a job that stopped from a tool with nothing new. Kept aside
// (UpdateMeta), merged with what other replicas kept.
func (l *Ledger) NoteReceived(ctx context.Context, source string, at time.Time) error {
	return l.UpdateMeta(ctx, metaReceived, func(old string) string {
		m := map[string]int64{}
		if old != "" {
			// One that doesn't decode is begun again.
			_ = json.Unmarshal([]byte(old), &m)
		}
		if _, ok := m[source]; !ok && len(m) >= MaxReceived-1 && source != OtherSources {
			source = OtherSources
		}
		if at.Unix() > m[source] {
			m[source] = at.Unix()
		}
		b, _ := json.Marshal(m)
		return string(b)
	})
}

// Received is, by source, when one last wrote through the incidents port.
func (l *Ledger) Received(ctx context.Context) (map[string]time.Time, error) {
	v, err := l.Meta(ctx, metaReceived)
	out := map[string]time.Time{}
	if err != nil || v == "" {
		return out, err
	}
	var m map[string]int64
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		return out, nil
	}
	for k, at := range m {
		out[k] = unix(at)
	}
	return out, nil
}

// Copied is an incident a tool's job copied in: its id, start and when it
// last changed.
type Copied struct {
	ID             string
	Start, Updated time.Time
}

// Sources are, of the incidents since a time, how many came from each
// place: Written, written in Lapilli (the UI, the API, a firing said to be
// real, an impact window confirmed), and LastWritten the latest of those
// ever written, by its start; Copied, those copied in from a tool that
// started since or changed since, maxCopied at most; File, those from the
// incidents file that started since; Impact, the impact windows found that
// started since.
type Sources struct {
	Written     int
	LastWritten *Incident
	Copied      []Copied
	File        int
	Impact      int
}

// IncidentSources counts the incidents since a time by where they came from.
func (l *Ledger) IncidentSources(ctx context.Context, since time.Time) (Sources, error) {
	var out Sources
	at := since.Unix()
	rows, err := l.r.QueryContext(ctx, "SELECT origin, count(*) FROM incidents WHERE start_at >= ? GROUP BY origin", at)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var origin string
		var n int
		if err := rows.Scan(&origin, &n); err != nil {
			rows.Close()
			return out, err
		}
		switch origin {
		case "ui":
			out.Written = n
		case "file":
			out.File = n
		case "impact":
			out.Impact = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	last, err := l.incidents(ctx, `SELECT `+incidentCols+`
		FROM incidents i WHERE i.origin = 'ui' ORDER BY i.start_at DESC, i.key LIMIT 1`)
	if err != nil {
		return out, err
	}
	if len(last) > 0 {
		out.LastWritten = &last[0]
	}
	rows, err = l.r.QueryContext(ctx, "SELECT key, start_at, updated FROM incidents WHERE origin = 'copy' AND (start_at >= ? OR updated >= ?) ORDER BY start_at, key LIMIT ?",
		at, at, maxCopied)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Copied
		var start, updated int64
		if err := rows.Scan(&c.ID, &start, &updated); err != nil {
			return out, err
		}
		c.Start, c.Updated = unix(start), unix(updated)
		out.Copied = append(out.Copied, c)
	}
	return out, rows.Err()
}

// ImpactWindows are the impact windows found that started in [from, to),
// decided or not, the earliest first.
func (l *Ledger) ImpactWindows(ctx context.Context, from, to time.Time) ([]Incident, error) {
	all, err := l.Incidents(ctx, from, to, true)
	return slices.DeleteFunc(all, func(in Incident) bool { return in.Origin != "impact" }), err
}

// ImpactOrphan is an impact window's key that tasks or comments are kept
// on, which no window has any more: read again, nothing found now covers
// it (remapImpact). Tasks are their ids; Comments, how many.
type ImpactOrphan struct {
	Key      string
	Tasks    []int64
	Comments int
}

// OrphanedImpact are the impact windows' keys tasks or comments are kept
// on that no incident has, 100 at most, by key.
func (l *Ledger) OrphanedImpact(ctx context.Context) ([]ImpactOrphan, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT k, kind, id FROM (
		SELECT incident AS k, 'task' AS kind, id FROM tasks WHERE incident LIKE 'impact-%'
		UNION ALL SELECT on_key, 'comment', id FROM comments WHERE on_kind = 'incident' AND on_key LIKE 'impact-%') x
		WHERE NOT EXISTS (SELECT 1 FROM incidents i WHERE i.key = x.k) ORDER BY k, kind, id LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImpactOrphan
	for rows.Next() {
		var key, kind string
		var id int64
		if err := rows.Scan(&key, &kind, &id); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].Key != key {
			if n == 100 {
				break
			}
			out = append(out, ImpactOrphan{Key: key})
		}
		if x := &out[len(out)-1]; kind == "task" {
			x.Tasks = append(x.Tasks, id)
		} else {
			x.Comments++
		}
	}
	return out, rows.Err()
}

// DecidedImpact are the impact windows something was decided about that a
// read from from on would take the place of, or end there, as ReadAgain
// reads them: on from from on, one that started before it too; for a try,
// which sets what it finds beside them.
func (l *Ledger) DecidedImpact(ctx context.Context, from time.Time) ([]Incident, error) {
	return l.incidents(ctx, `SELECT `+incidentCols+`
		FROM incidents i WHERE i.origin = 'impact' AND i.signal = '' AND i.state != '' AND (i.start_at >= ? OR i.end_at > ?) ORDER BY i.start_at, i.key`, from.Unix(), from.Unix())
}
