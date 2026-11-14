package ledger

import (
	"context"
	"database/sql"
	"errors"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// Real writes a firing that caught nothing down as an incident, as the
// firings' "This was real" does: from its start and end, titled by its
// rule, with its labels that o.Match matches on. It is one write: the
// firing is read again in it, and nothing is written when it has caught
// something by then, or an incident written from it is there already, the
// same title, times and labels. So two at once, by one person or two, write
// one incident and tell of it once. It returns the incident's id, "" when
// none was written, and the firing as it was read; ErrNotFound for no such
// firing, and an ErrInvalid for one Put would refuse (one that lasted
// longer than an incident may). A write of nothing is rolled back, not
// counted: what pages keep, and other replicas, stand.
func (l *Ledger) Real(ctx context.Context, id int64, by string, o verdict.Options) (string, Firing, error) {
	var f Firing
	var made string
	err := l.write(ctx, func(t *txn) error {
		made = ""
		var err error
		if f, err = l.firingIn(t, id); err != nil {
			return err
		}
		if f.White {
			return errNothing
		}
		in := realOf(f, o.Match)
		if err := checkIncident(in); err != nil {
			return err
		}
		if ok, err := l.writtenFrom(t, in); err != nil {
			return err
		} else if ok {
			return errNothing
		}
		if err := l.putIn(t, &in, originOf(by), by, o); err != nil {
			return err
		}
		made = in.ID
		return nil
	})
	if err != nil && !errors.Is(err, errNothing) {
		return "", f, err
	}
	return made, f, nil
}

// errNothing ends a write that finds nothing to write: rolled back, it
// isn't counted as a change.
var errNothing = errors.New("nothing to write")

// realOf is the incident a firing is written down as.
func realOf(f Firing, match []string) incident.Incident {
	in := incident.Incident{Title: "caught by " + f.Rule, Start: f.Start, End: f.End}
	for _, m := range match {
		if v, ok := f.Labels[m]; ok {
			if in.Labels == nil {
				in.Labels = incident.Labels{}
			}
			in.Labels[m] = []string{v}
		}
	}
	return in
}

// firingIn is FiringByID, read in t.
func (l *Ledger) firingIn(t *txn, id int64) (Firing, error) {
	var f Firing
	var sid, start int64
	var end sql.NullInt64
	var labels string
	err := t.QueryRowContext(t.ctx, `SELECT f.id, r.name, s.id, s.labels, f.start_at, f.end_at, f.white, f.refired, f.excused, f.deploy
		FROM firings f JOIN series s ON s.id = f.series_id JOIN rules r ON r.id = f.rule_id WHERE f.id = ?`, id).
		Scan(&f.ID, &f.Rule, &sid, &labels, &start, &end, &f.White, &f.Refired, &f.Excused, &f.Deploy)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	if err != nil {
		return f, err
	}
	if f.Labels, err = l.labels(sid, labels); err != nil {
		return f, err
	}
	f.Start, f.End = unix(start), fromNull(end)
	return f, nil
}

// writtenFrom reports whether an incident written here is in as a firing
// writes it down: its title, times and labels.
func (l *Ledger) writtenFrom(t *txn, in incident.Incident) (bool, error) {
	rows, err := t.QueryContext(t.ctx, "SELECT id, key, origin, title, start_at, end_at, labels FROM incidents WHERE start_at = ? AND origin = 'ui'", in.Start.Unix())
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanIncident(rows)
		if err != nil {
			return false, err
		}
		in.ID = r.ID
		if same(l.d, r.Incident, in) {
			return true, nil
		}
	}
	return false, rows.Err()
}
