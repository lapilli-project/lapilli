package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// What the impact signal was read with, and from when (docs/design/serve.md,
// "No record"), kept in meta: impact_since, from when the runs read it, all
// of it (verdict.Options.ImpactFrom); impact_criterion, what they read it
// with, the expression and --impact-for as lapilli serve keys them;
// impact_again, the criterion the weeks before the runs' first read of it
// were read again with (ReadAgain), or found needing none, againDue while
// they wait to be; and impact_data, when each selector the criterion reads
// first had data (ImpactData). A lapilli before 0.47 kept only the first.
const (
	metaImpactSince     = "impact_since"
	metaImpactCriterion = "impact_criterion"
	metaImpactAgain     = "impact_again"
	metaImpactData      = "impact_data"
	// againDue is impact_again while the weeks before a new criterion's
	// first read wait to be read again with it.
	againDue = "-"
)

// ImpactKept is what keeps the record by the impact signal, as the ledger
// has it: Since, from when the runs read it, all of it, zero for never, or
// not since a run that didn't; Criterion, what they read it with, "" when
// none is kept (never read, or read by a lapilli before 0.47); Again, the
// criterion the weeks before Since were read again with (ReadAgain), or
// found needing none (MarkAgain), "" for none and "-" while it waits; and
// Data, where the data of the selectors a criterion reads begins, nil
// before it was looked for.
type ImpactKept struct {
	Since     time.Time
	Criterion string
	Again     string
	Data      *ImpactData
}

// AgainDone reports whether the weeks before the runs read criterion were
// read again with it, or found needing none.
func (k ImpactKept) AgainDone(criterion string) bool { return k.Again == criterion }

// From is from when the record is kept by the impact signal: Since, but
// never before where the data of every selector the criterion read begins
// (ImpactData.Floor), and never when one of them has no data yet. Data of
// another criterion than the one read says nothing of it: Since was kept
// to that one's when it was read.
func (k ImpactKept) From() time.Time {
	if k.Since.IsZero() {
		return time.Time{}
	}
	if d := k.Data; d != nil && d.Criterion == k.Criterion {
		if d.NoData {
			return time.Time{}
		}
		if d.Floor.After(k.Since) {
			return d.Floor
		}
	}
	return k.Since
}

// ImpactData is where the data of the selectors a criterion reads begins,
// as lapilli serve looked for it from From to CheckedTo: when each first
// had a sample (Selectors). Floor is the first whole week of the ledger
// after the latest of them, of a selector that had none at From: the
// record is kept by the signal from there at the earliest, as only there
// did every selector it reads have data. Zero when each had data at From.
// NoData says one had none at all: the record isn't kept by it.
type ImpactData struct {
	Criterion string         `json:"criterion"`
	Selectors []SelectorData `json:"selectors"`
	From      time.Time      `json:"from"`
	CheckedTo time.Time      `json:"checked_to"`
	Floor     time.Time      `json:"floor,omitzero"`
	NoData    bool           `json:"no_data,omitempty"`
}

// SelectorData is a selector, as written in the expression, and when its
// first sample was found: zero for none yet; AtStart, where the look began,
// so it may have had data before.
type SelectorData struct {
	Sel     string    `json:"sel"`
	First   time.Time `json:"first,omitzero"`
	AtStart bool      `json:"at_start,omitempty"`
}

// Complete reports whether every selector had data.
func (d *ImpactData) Complete() bool {
	return !slices.ContainsFunc(d.Selectors, func(x SelectorData) bool { return x.First.IsZero() })
}

// Late is the selector whose data begins the latest, after the look began,
// and the others': what holds the record back, when more than one selector
// is read and they don't begin together. "" for none.
func (d *ImpactData) Late() string {
	late, at, earlier := "", time.Time{}, false
	for _, x := range d.Selectors {
		switch {
		case x.First.IsZero():
			return ""
		case x.First.After(at):
			if late != "" || x.AtStart {
				earlier = true
			}
			if !x.AtStart {
				late, at = x.Sel, x.First
			}
		default:
			earlier = true
		}
	}
	if !earlier {
		return ""
	}
	return late
}

// Floor works d's Floor and NoData out, by the ledger's weeks.
func (l *Ledger) Floor(d *ImpactData) { l.floor(d) }

func (l *Ledger) floor(d *ImpactData) {
	d.Floor, d.NoData = time.Time{}, !d.Complete()
	if d.NoData {
		return
	}
	for _, x := range d.Selectors {
		if !x.AtStart {
			d.Floor = later(d.Floor, l.WeekOnOrAfter(x.First))
		}
	}
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

// ImpactKept reads what keeps the record by the impact signal, in one read.
func (l *Ledger) ImpactKept(ctx context.Context) (ImpactKept, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT key, value FROM meta WHERE key IN (?, ?, ?, ?)", metaImpactSince, metaImpactCriterion, metaImpactAgain, metaImpactData)
	if err != nil {
		return ImpactKept{}, err
	}
	defer rows.Close()
	var k ImpactKept
	for rows.Next() {
		var key, v string
		if err := rows.Scan(&key, &v); err != nil {
			return ImpactKept{}, err
		}
		switch key {
		case metaImpactSince:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && v != impactNone {
				k.Since = unix(n)
			}
		case metaImpactCriterion:
			k.Criterion = v
		case metaImpactAgain:
			k.Again = v
		case metaImpactData:
			var d ImpactData
			if json.Unmarshal([]byte(v), &d) == nil {
				k.Data = &d
			}
		}
	}
	return k, rows.Err()
}

// impactData is the ImpactData kept, nil for none.
func (l *Ledger) impactData(ctx context.Context, q querier) (*ImpactData, error) {
	v, err := l.meta(ctx, q, metaImpactData)
	if err != nil || v == "" {
		return nil, err
	}
	var d ImpactData
	if err := json.Unmarshal([]byte(v), &d); err != nil {
		return nil, nil // looked for again
	}
	return &d, nil
}

// SetImpactData keeps where the data of the criterion the runs read begins,
// and moves impact_since forward to its Floor: never back. Of another
// criterion than the one read, it keeps nothing.
func (l *Ledger) SetImpactData(ctx context.Context, d ImpactData) error {
	l.floor(&d)
	return l.write(ctx, func(t *txn) error {
		crit, err := l.meta(t.ctx, t, metaImpactCriterion)
		if err != nil || crit != d.Criterion {
			return err
		}
		b, err := json.Marshal(d)
		if err != nil {
			return err
		}
		if err := setMeta(t.ctx, t, metaImpactData, string(b)); err != nil {
			return err
		}
		was, err := l.meta(t.ctx, t, metaImpactSince)
		if err != nil {
			return err
		}
		if n, err := strconv.ParseInt(was, 10, 64); err == nil && !d.Floor.IsZero() && n < d.Floor.Unix() {
			return setMeta(t.ctx, t, metaImpactSince, strconv.FormatInt(d.Floor.Unix(), 10))
		}
		return nil
	})
}

// criterionChanged reports whether a run read the impact signal with
// another criterion than the runs before it: not when the run says none
// (one from before 0.47), nor when the runs before didn't read it. A ledger
// that kept no criterion but was read (an earlier lapilli's) was read with
// the one the run takes it to (ImpactAssumed).
func (l *Ledger) criterionChanged(t *txn, f Found) (bool, error) {
	if !f.ImpactOn || f.ImpactCriterion == "" {
		return false, nil
	}
	was, err := l.meta(t.ctx, t, metaImpactCriterion)
	if err != nil || was != "" {
		return was != "" && was != f.ImpactCriterion, err
	}
	since, err := l.meta(t.ctx, t, metaImpactSince)
	if _, perr := strconv.ParseInt(since, 10, 64); err != nil || perr != nil {
		return false, err
	}
	return f.ImpactAssumed != f.ImpactCriterion, nil
}

// ImpactRead is the impact signal read again over [From, To], To where the
// runs have got to (the cursor), with Criterion: the windows it found, and
// where the data of the selectors it reads begins over the span (Data).
type ImpactRead struct {
	From, To  time.Time
	Criterion string
	Windows   []incident.Incident
	Data      ImpactData
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
// begins). Before r.From, the windows found before stay, as the incidents
// they were when they were read, and keep the record of the weeks near
// them as an incident does; the signal read with another criterion keeps
// none.
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
		lo, err := l.impact(t, Found{From: r.From, ImpactSince: r.From, Impact: r.Windows}, true)
		if err != nil {
			return err
		}
		since := later(r.From, r.Data.Floor)
		b, err := json.Marshal(r.Data)
		if err != nil {
			return err
		}
		for k, v := range map[string]string{metaImpactSince: strconv.FormatInt(since.Unix(), 10), metaImpactCriterion: r.Criterion,
			metaImpactAgain: r.Criterion, metaImpactData: string(b)} {
			if err := setMeta(t.ctx, t, k, v); err != nil {
				return err
			}
		}
		return pend(t, [][2]int64{{min(lo, from) - int64(o.Lead/time.Second), r.To.Unix()}})
	})
}

// MarkAgain keeps that the weeks before the runs read criterion need no
// read again: the runs read it from their first, or a lapilli before 0.47
// read it, which kept no criterion, and it is taken for the one read now.
func (l *Ledger) MarkAgain(ctx context.Context, criterion string) error {
	return l.write(ctx, func(t *txn) error {
		if err := setMeta(t.ctx, t, metaImpactCriterion, criterion); err != nil {
			return err
		}
		return setMeta(t.ctx, t, metaImpactAgain, criterion)
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
	last, err := l.incidents(ctx, `SELECT i.id, i.key, i.origin, i.title, i.start_at, i.end_at, i.labels, i.state, i.counts, i.updated, i.updated_by
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
