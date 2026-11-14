package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// What the proposals' pages read beside the proposals themselves
// (docs/design/ui.md): each proposal's steps, as the changes keep them, and
// the proposals in brief, for the list's facets.

// ProposalSteps are the latest changes kept of a proposal, most at most,
// in the order they were made (by the changes' ids, not their times, which
// replicas' clocks set to the second): its making, each approval, each
// state it moved to, an undo asked for. They are those whose what starts
// "proposal #ID ", read by the changes' index on what; those older than
// --keep are gone. more says there are older ones than those given.
func (l *Ledger) ProposalSteps(ctx context.Context, id int64, most int) (steps []Change, more bool, err error) {
	// Every what of one proposal, and none of another's (#1 is not #12):
	// the space after its number, up to the character after the space.
	from := fmt.Sprintf("proposal #%d ", id)
	to := from[:len(from)-1] + "!"
	rows, err := l.r.QueryContext(ctx, "SELECT at, by, what, before, after FROM changes WHERE what >= ? AND what < ? ORDER BY id DESC LIMIT ?", from, to, most+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Change
		var at int64
		if err := rows.Scan(&at, &c.By, &c.What, &c.Before, &c.After); err != nil {
			return nil, false, err
		}
		c.At = unix(at)
		steps = append(steps, c)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(steps) > most {
		steps, more = steps[:most], true
	}
	slices.Reverse(steps)
	return steps, more, nil
}

// ProposalHead is a proposal in brief, as the proposals list shows and
// counts it: none of its documents, its evidence only for an undo (whether
// it undoes a change to what a rule says), and how many approved it.
type ProposalHead struct {
	Proposal
	Approved int
}

// ProposalHeads are the latest proposals in brief, most at most, the
// latest moved first.
func (l *Ledger) ProposalHeads(ctx context.Context, most int) ([]ProposalHead, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT p.id, p.rule, p.kind, p.path, p.state, p.made, p.made_by, p.agent, p.before_hash,
		CASE WHEN p.kind = ? THEN p.evidence ELSE '' END, p.updated, (SELECT count(*) FROM approvals a WHERE a.proposal_id = p.id)
		FROM proposals p ORDER BY p.updated DESC, p.id DESC LIMIT ?`, KindUndo, most)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProposalHead
	for rows.Next() {
		var h ProposalHead
		var made, updated int64
		var evidence string
		if err := rows.Scan(&h.ID, &h.Rule, &h.Kind, &h.Path, &h.State, &made, &h.MadeBy, &h.Agent, &h.BeforeHash, &evidence, &updated, &h.Approved); err != nil {
			return nil, err
		}
		h.Made, h.Updated = unix(made), unix(updated)
		if evidence != "" {
			h.Evidence = json.RawMessage(evidence)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// TasksByStatus are how many tasks there are in each status, of one team when
// team isn't 0.
func (l *Ledger) TasksByStatus(ctx context.Context, team int64) (map[string]int, error) {
	q := "SELECT status, count(*) FROM tasks"
	var args []any
	if team != 0 {
		q += " WHERE team_id = ?"
		args = append(args, team)
	}
	rows, err := l.r.QueryContext(ctx, q+" GROUP BY status", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}
