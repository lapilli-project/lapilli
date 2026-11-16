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
// with, the expression and --impact-for as lapilli serve keys them; and
// impact_again, the criterion the weeks before the runs' first read of it
// were read again with (ReadAgain), or found needing none, againDue while a
// run that read another criterion than the runs before waits for it. A
// lapilli before 0.47 kept neither of the last two: the criterion it read
// is taken to be the one read now.
const (
	metaImpactSince     = "impact_since"
	metaImpactCriterion = "impact_criterion"
	metaImpactAgain     = "impact_again"
	// againDue is impact_again while the weeks before a new criterion's
	// first read wait to be read again with it.
	againDue = "-"
)

// ImpactKept is what keeps the record by the impact signal, as the ledger
// has it: Since, from when the runs read it, all of it, zero for never, or
// not since a run that didn't; Criterion, what they read it with, "" when
// none is kept (never read, or read by a lapilli before 0.47); and Again,
// the criterion the weeks before Since were read again with (ReadAgain),
// or found needing none (MarkAgain), "" for none and "-" while it waits.
type ImpactKept struct {
	Since     time.Time
	Criterion string
	Again     string
}

// AgainDone reports whether the weeks before the runs read criterion were
// read again with it, or found needing none.
func (k ImpactKept) AgainDone(criterion string) bool { return k.Again == criterion }

// ImpactKept reads what keeps the record by the impact signal, in one read.
func (l *Ledger) ImpactKept(ctx context.Context) (ImpactKept, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT key, value FROM meta WHERE key IN (?, ?, ?)", metaImpactSince, metaImpactCriterion, metaImpactAgain)
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
		}
	}
	return k, rows.Err()
}

// criterionChanged reports whether a run read the impact signal with
// another criterion than the runs before it: not when either is "" (a run
// or a ledger from before 0.47), nor when the runs before didn't read it.
func (l *Ledger) criterionChanged(t *txn, f Found) (bool, error) {
	if !f.ImpactOn || f.ImpactCriterion == "" {
		return false, nil
	}
	was, err := l.meta(t.ctx, t, metaImpactCriterion)
	return was != "" && was != f.ImpactCriterion, err
}

// ImpactRead is the impact signal read again over [From, To], To where the
// runs have got to (the cursor), with Criterion: the windows it found.
type ImpactRead struct {
	From, To  time.Time
	Criterion string
	Windows   []incident.Incident
}

// ErrRunsMoved is an ImpactRead the runs went past while it was read: it is
// read again, to where they are.
var ErrRunsMoved = errors.New("the runs moved on while the impact signal was read again")

// ReadAgain puts what the impact signal read again over [r.From, r.To]
// found in place of the windows found before from r.From on, keeping what
// people decided about them (impact), ends a window found before on at
// r.From there, and gives the stones again over the span: from then on, the
// record is kept by r.Criterion, read over all of it (impact_since r.From).
// Before r.From, the windows found before stay, as the incidents they were
// when they were read, and keep the record of the weeks near them as an
// incident does; the signal read with another criterion keeps none.
func (l *Ledger) ReadAgain(ctx context.Context, r ImpactRead, o verdict.Options) error {
	return l.write(ctx, func(t *txn) error {
		cur, err := l.meta(t.ctx, t, "cursor")
		if err != nil {
			return err
		}
		if cur != strconv.FormatInt(r.To.Unix(), 10) {
			return ErrRunsMoved
		}
		from := r.From.Unix()
		lo := from
		var on int64
		err = t.QueryRowContext(t.ctx, "SELECT coalesce(min(start_at), ?) FROM incidents WHERE origin = 'impact' AND start_at < ? AND end_at > ?", from, from, from).Scan(&on)
		if err != nil {
			return err
		}
		lo = min(lo, on)
		if _, err := t.ExecContext(t.ctx, "UPDATE incidents SET end_at = ? WHERE origin = 'impact' AND start_at < ? AND end_at > ?", from, from, from); err != nil {
			return err
		}
		ilo, err := l.impact(t, Found{From: r.From, ImpactSince: r.From, Impact: r.Windows}, true)
		if err != nil {
			return err
		}
		lo = min(lo, ilo)
		for k, v := range map[string]string{metaImpactSince: strconv.FormatInt(from, 10), metaImpactCriterion: r.Criterion, metaImpactAgain: r.Criterion} {
			if err := setMeta(t.ctx, t, k, v); err != nil {
				return err
			}
		}
		return l.judge(t, lo-int64(o.Lead/time.Second), r.To.Unix(), o)
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
// the ledger keeps: the oldest is forgotten past it.
const MaxReceived = 64

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
		if at.Unix() > m[source] {
			m[source] = at.Unix()
		}
		for len(m) > MaxReceived {
			oldest := ""
			for k, v := range m {
				if oldest == "" || v < m[oldest] || v == m[oldest] && k < oldest {
					oldest = k
				}
			}
			delete(m, oldest)
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
