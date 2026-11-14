package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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

// ProposalHead is a proposal in brief, as the proposals list shows it:
// none of its documents, its evidence only for an undo (whether it undoes
// a change to what a rule says), and how many approved it.
type ProposalHead struct {
	Proposal
	Approved int
}

// Makers are who made a proposal, as the proposals list tells them apart:
// a person, an agent for one, or Lapilli itself.
const (
	MakerPerson  = "person"
	MakerAgent   = "agent"
	MakerLapilli = "lapilli"
)

// ProposalPick is what the proposals list picks: of the rules named (every
// rule's when Rules is nil, none's when it is empty), any of the states,
// kinds, ways and makers given in each (any at all for one given none).
type ProposalPick struct {
	Rules                        []string
	States, Kinds, Paths, Makers []string
}

// ProposalCount is how many proposals there are of a kind, a way, a state
// and a maker.
type ProposalCount struct {
	Kind, Path, State, Maker string
	N                        int
}

// ruleChunk is how many rules' names a read of the proposals names at
// once: a team's rules can be thousands, and a statement takes some
// thousands of values at most.
const ruleChunk = 2000

// maker is a proposal's maker, worked out by the database, of the
// proposals' table as at names it ("p." or "").
func maker(at string) (string, []any) {
	return "CASE WHEN " + at + "agent != 0 THEN '" + MakerAgent + "' WHEN " + at + "made_by = ? THEN '" + MakerLapilli + "' ELSE '" + MakerPerson + "' END",
		[]any{FoldName(Proposer)}
}

// byRules runs a read for each chunk of rules (once, for every rule's, when
// rules is nil; never, for none's), with where saying them.
func byRules(rules []string, read func(where string, args []any) error) error {
	if rules == nil {
		return read("", nil)
	}
	for len(rules) > 0 {
		n := min(len(rules), ruleChunk)
		if err := read("rule IN ("+marks(n)+")", anys(rules[:n])); err != nil {
			return err
		}
		rules = rules[n:]
	}
	return nil
}

// ProposalCounts are how many proposals of the rules named (every rule's
// when nil) there are of each kind, way, state and maker, counted by the
// database: the list's facets, whatever the proposals' number.
func (l *Ledger) ProposalCounts(ctx context.Context, rules []string) ([]ProposalCount, error) {
	m, margs := maker("")
	sum := map[ProposalCount]int{}
	err := byRules(rules, func(where string, args []any) error {
		q := "SELECT kind, path, state, " + m + ", count(*) FROM proposals"
		if where != "" {
			q += " WHERE " + where
		}
		rows, err := l.r.QueryContext(ctx, q+" GROUP BY 1, 2, 3, 4", append(margs, args...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c ProposalCount
			if err := rows.Scan(&c.Kind, &c.Path, &c.State, &c.Maker, &c.N); err != nil {
				return err
			}
			n := c.N
			c.N = 0
			sum[c] += n
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	out := make([]ProposalCount, 0, len(sum))
	for c, n := range sum {
		c.N = n
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b ProposalCount) int {
		return strings.Compare(a.Kind+"\x00"+a.Path+"\x00"+a.State+"\x00"+a.Maker, b.Kind+"\x00"+b.Path+"\x00"+b.State+"\x00"+b.Maker)
	})
	return out, nil
}

// ProposalList are, in brief, the proposals a pick picks, most at most,
// picked by the database: those waiting for approval first, then those
// not finished with, then the rest, each the latest moved first.
func (l *Ledger) ProposalList(ctx context.Context, pick ProposalPick, most int) ([]ProposalHead, error) {
	m, margs := maker("p.")
	rank := "CASE WHEN p.state = ? THEN 0 WHEN p.state IN (" + marks(len(unfinished)) + ") THEN 1 ELSE 2 END"
	rargs := append([]any{StateProposed}, anys(unfinished)...)
	var out []ProposalHead
	ranks := map[int64]int{}
	err := byRules(pick.Rules, func(where string, args []any) error {
		q := `SELECT p.id, p.rule, p.kind, p.path, p.state, p.made, p.made_by, p.agent, p.before_hash,
			CASE WHEN p.kind = ? THEN p.evidence ELSE '' END, p.updated, (SELECT count(*) FROM approvals a WHERE a.proposal_id = p.id), ` + rank + `
			FROM proposals p WHERE 1 = 1`
		qargs := append([]any{KindUndo}, rargs...)
		if where != "" {
			q += " AND p." + where
			qargs = append(qargs, args...)
		}
		for _, f := range []struct {
			col string
			vs  []string
		}{{"p.state", pick.States}, {"p.kind", pick.Kinds}, {"p.path", pick.Paths}} {
			if len(f.vs) > 0 {
				q += " AND " + f.col + " IN (" + marks(len(f.vs)) + ")"
				qargs = append(qargs, anys(f.vs)...)
			}
		}
		if len(pick.Makers) > 0 {
			q += " AND " + m + " IN (" + marks(len(pick.Makers)) + ")"
			qargs = append(append(qargs, margs...), anys(pick.Makers)...)
		}
		q += " ORDER BY 13, p.updated DESC, p.id DESC LIMIT ?"
		rows, err := l.r.QueryContext(ctx, q, append(qargs, most)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h ProposalHead
			var made, updated int64
			var evidence string
			var rank int
			if err := rows.Scan(&h.ID, &h.Rule, &h.Kind, &h.Path, &h.State, &made, &h.MadeBy, &h.Agent, &h.BeforeHash, &evidence, &updated, &h.Approved, &rank); err != nil {
				return err
			}
			h.Made, h.Updated = unix(made), unix(updated)
			if evidence != "" {
				h.Evidence = json.RawMessage(evidence)
			}
			ranks[h.ID] = rank
			out = append(out, h)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	// Each chunk's in order: together, in the same order, the first most.
	slices.SortFunc(out, func(a, b ProposalHead) int {
		if r := ranks[a.ID] - ranks[b.ID]; r != 0 {
			return r
		}
		if c := b.Updated.Compare(a.Updated); c != 0 {
			return c
		}
		return int(b.ID - a.ID)
	})
	return out[:min(len(out), most)], nil
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

