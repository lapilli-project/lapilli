package ledger

import (
	"context"
	"database/sql"
	"errors"
)

// NoteOf is the note on a rule, as Notes has it: one with Rule alone when
// there is none.
func (l *Ledger) NoteOf(ctx context.Context, rule string) (Note, error) {
	n := Note{Rule: rule}
	var at int64
	err := l.r.QueryRowContext(ctx, "SELECT rule, coalesce(team_id, 0), status, runbook, note, updated, updated_by FROM notes WHERE rule = ?", rule).
		Scan(&n.Rule, &n.TeamID, &n.Status, &n.Runbook, &n.Note, &at, &n.By)
	if errors.Is(err, sql.ErrNoRows) {
		return Note{Rule: rule}, nil
	}
	if err != nil {
		return Note{}, err
	}
	n.Updated = unix(at)
	return n, nil
}
