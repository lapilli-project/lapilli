package ledger

import (
	"context"
	"fmt"
)

// What the proposals' pages read beside the proposals themselves
// (docs/design/ui.md): each proposal's steps, as the changes keep them.

// ProposalSteps are the changes kept of a proposal, in the order they were
// made, most at most: its making, each approval, each state it moved to,
// an undo asked for. They are those whose what starts "proposal #ID ",
// read by the changes' index on what; those older than --keep are gone.
func (l *Ledger) ProposalSteps(ctx context.Context, id int64, most int) ([]Change, error) {
	// Every what of one proposal, and none of another's (#1 is not #12):
	// the space after its number, up to the character after the space.
	from := fmt.Sprintf("proposal #%d ", id)
	to := from[:len(from)-1] + "!"
	rows, err := l.r.QueryContext(ctx, "SELECT at, by, what, before, after FROM changes WHERE what >= ? AND what < ? ORDER BY id LIMIT ?", from, to, most)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var at int64
		if err := rows.Scan(&at, &c.By, &c.What, &c.Before, &c.After); err != nil {
			return nil, err
		}
		c.At = unix(at)
		out = append(out, c)
	}
	return out, rows.Err()
}
