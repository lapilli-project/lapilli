package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Proposals (docs/design/changes.md): a change to one rule, with the
// evidence for it, approved by a person before anything is applied.

// What a proposal changes, the way it goes, and where it is.
const (
	KindFor       = "for"
	KindThreshold = "threshold"
	KindDisable   = "disable"
	KindDelete    = "delete"
	// KindSays sets or takes out one annotation: what the rule says when
	// it fires, never when it fires.
	KindSays = "says"
	// KindUndo writes back the rule an applied proposal changed.
	KindUndo = "undo"

	PathGit     = "git"
	PathGrafana = "grafana"
	PathNone    = "none"

	StateProposed = "proposed"
	StateApproved = "approved"
	StateReady    = "ready"
	// StateApplying is a change sent to Grafana, kept with the rule as it
	// was and counted against the limits before it is sent, and not yet
	// read back: read again each round, never sent again.
	StateApplying = "applying"
	StateOpened   = "opened"
	StateMerged   = "merged"
	StateApplied  = "applied"
	StateRefused  = "refused"
	StateFailed   = "failed"
	StateUndone   = "undone"
	StateDropped  = "dropped"

	// Proposer is who the proposals Lapilli makes itself are made by, and
	// ChangesCopier who a recipe with the changes token acts as. Neither is
	// a person: neither approves, and outcomes don't count them as use.
	Proposer      = "lapilli"
	ChangesCopier = "changes-token"
)

// unfinished are the states of a proposal that isn't finished with: another
// for the same rule waits for it.
var unfinished = []string{StateProposed, StateApproved, StateReady, StateApplying, StateOpened, StateMerged}

// Unfinished are the states of a proposal that isn't finished with.
func Unfinished() []string { return slices.Clone(unfinished) }

// Proposal is a change to one rule.
type Proposal struct {
	ID     int64     `json:"id"`
	Rule   string    `json:"rule"`
	Kind   string    `json:"kind"`
	Path   string    `json:"path"`
	State  string    `json:"state"`
	Made   time.Time `json:"made"`
	MadeBy string    `json:"made_by"`
	// Agent says an agent made it, through MCP: what it says is data.
	Agent bool `json:"agent,omitempty"`
	// Source is the document the rule was read from, DocHash its hash, and
	// Document the document as changed, for the git path.
	Source   string `json:"source,omitempty"`
	DocHash  string `json:"doc_hash,omitempty"`
	Document string `json:"document,omitempty"`
	// Before is the rule as it was read when the proposal was made, and
	// BeforeHash its hash; After the rule as proposed, and AfterHash what
	// it hashes to in the rules read once it is applied; Diff the change.
	Before     string          `json:"before"`
	BeforeHash string          `json:"before_hash"`
	After      string          `json:"after"`
	AfterHash  string          `json:"after_hash,omitempty"`
	Diff       string          `json:"diff"`
	Evidence   json.RawMessage `json:"evidence"`
	// Reason is why it was refused, or failed; Link its pull request.
	Reason string `json:"reason,omitempty"`
	Link   string `json:"link,omitempty"`
	// Backup is the rule as it was kept, read just before it was changed.
	Backup  string `json:"-"`
	Follows int64  `json:"follows,omitempty"`
	TaskID  int64  `json:"task_id,omitempty"`
	// Updated is when it last moved: for an applied one, when it took
	// effect. Applied is when Lapilli carried it out (sent it to Grafana,
	// or handed it to a recipe), which the limits count; never, for a
	// change a person made.
	Updated   time.Time  `json:"updated"`
	Applied   time.Time  `json:"applied,omitzero"`
	Approvals []Approval `json:"approvals"`
}

// Approval is a person's approval of a proposal.
type Approval struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

// Open reports whether the proposal isn't finished with.
func (p Proposal) Open() bool { return slices.Contains(unfinished, p.State) }

// about is what a notice says of a proposal.
func (p Proposal) about() map[string]any {
	return map[string]any{"proposal": map[string]any{"id": p.ID, "rule": p.Rule, "kind": p.Kind, "path": p.Path, "state": p.State,
		"link": p.Link, "reason": p.Reason, "made_by": p.MadeBy, "agent": p.Agent}}
}

// ErrProposed is a proposal for a rule that has one of its kind unfinished.
var ErrProposed = errors.New("this rule has a proposal of this kind already")

// Propose keeps a proposal, as proposed.
func (l *Ledger) Propose(ctx context.Context, p Proposal) (int64, error) {
	switch {
	case p.Rule == "" || !plain(p.Rule):
		return 0, invalid("a proposal is for a rule")
	case !slices.Contains([]string{KindFor, KindThreshold, KindDisable, KindDelete, KindSays}, p.Kind):
		return 0, invalid("a proposal's kind is for, threshold, disable, delete or says")
	case !slices.Contains([]string{PathGit, PathGrafana, PathNone}, p.Path):
		return 0, invalid("a proposal's path is git, grafana or none")
	case len(p.Evidence) == 0:
		p.Evidence = json.RawMessage("{}")
	}
	now := time.Now()
	p.State, p.Made, p.Updated = StateProposed, now.Truncate(time.Second), now.Truncate(time.Second)
	var id int64
	err := l.write(ctx, func(t *txn) error {
		var n int
		if err := t.QueryRowContext(t.ctx, `SELECT count(*) FROM proposals WHERE rule = ? AND kind = ? AND state IN (`+marks(len(unfinished))+`)`,
			append([]any{p.Rule, p.Kind}, anys(unfinished)...)...).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrProposed
		}
		if err := t.QueryRowContext(t.ctx, `INSERT INTO proposals (rule, kind, path, state, made, made_by, agent, source, doc_hash, before, before_hash,
			after, after_hash, document, diff, evidence, follows, task_id, updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			p.Rule, p.Kind, p.Path, p.State, p.Made.Unix(), p.MadeBy, p.Agent, p.Source, p.DocHash, p.Before, p.BeforeHash,
			p.After, p.AfterHash, p.Document, p.Diff, string(p.Evidence), nullID(p.Follows), nullID(p.TaskID), p.Updated.Unix()).Scan(&id); err != nil {
			return err
		}
		p.ID = id
		if err := change(t, p.MadeBy, fmt.Sprintf("proposal #%d made", id), nil, p.summary()); err != nil {
			return err
		}
		return l.tell(t, "proposal.made", p.MadeBy, p.about())
	})
	return id, err
}

// summary is what changes keeps of a proposal.
func (p Proposal) summary() map[string]any {
	return map[string]any{"id": p.ID, "rule": p.Rule, "kind": p.Kind, "path": p.Path, "state": p.State, "agent": p.Agent,
		"reason": p.Reason, "link": p.Link}
}

const proposalColumns = `id, rule, kind, path, state, made, made_by, agent, source, doc_hash, before, before_hash, after, after_hash, document,
	diff, evidence, reason, link, backup, follows, task_id, updated, applied`

func scanProposal(row interface{ Scan(...any) error }) (Proposal, error) {
	var p Proposal
	var made, updated int64
	var follows, task, applied sql.NullInt64
	var evidence string
	if err := row.Scan(&p.ID, &p.Rule, &p.Kind, &p.Path, &p.State, &made, &p.MadeBy, &p.Agent, &p.Source, &p.DocHash, &p.Before,
		&p.BeforeHash, &p.After, &p.AfterHash, &p.Document, &p.Diff, &evidence, &p.Reason, &p.Link, &p.Backup, &follows, &task, &updated, &applied); err != nil {
		return p, err
	}
	p.Made, p.Updated, p.Applied = unix(made), unix(updated), fromNull(applied)
	p.Follows, p.TaskID, p.Evidence = follows.Int64, task.Int64, json.RawMessage(evidence)
	return p, nil
}

// ProposalByID is one proposal, with who approved it.
func (l *Ledger) ProposalByID(ctx context.Context, id int64) (Proposal, error) {
	p, err := scanProposal(l.r.QueryRowContext(ctx, "SELECT "+proposalColumns+" FROM proposals WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	p.Approvals, err = l.approvals(ctx, l.r, id)
	return p, err
}

func (l *Ledger) approvals(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id int64) ([]Approval, error) {
	rows, err := q.QueryContext(ctx, "SELECT by, at FROM approvals WHERE proposal_id = ? ORDER BY at, by", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		var a Approval
		var at int64
		if err := rows.Scan(&a.By, &at); err != nil {
			return nil, err
		}
		a.At = unix(at)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ProposalQuery picks proposals: of a rule, in some states, the latest
// first, Limit at most.
type ProposalQuery struct {
	Rule   string
	States []string
	Limit  int
}

// Proposals are the proposals a query picks, with who approved each.
func (l *Ledger) Proposals(ctx context.Context, q ProposalQuery) ([]Proposal, error) {
	sq := "SELECT " + proposalColumns + " FROM proposals WHERE 1 = 1"
	var args []any
	if q.Rule != "" {
		sq += " AND rule = ?"
		args = append(args, q.Rule)
	}
	if len(q.States) > 0 {
		sq += " AND state IN (" + marks(len(q.States)) + ")"
		args = append(args, anys(q.States)...)
	}
	sq += " ORDER BY updated DESC, id DESC"
	if q.Limit > 0 {
		sq += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	rows, err := l.r.QueryContext(ctx, sq, args...)
	if err != nil {
		return nil, err
	}
	var out []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Approvals, err = l.approvals(ctx, l.r, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ErrOwnProposal is an approval by whoever made the proposal.
var ErrOwnProposal = errors.New("a proposal isn't approved by whoever made it")

// ErrApproved is a second approval by the same person.
var ErrApproved = errors.New("you have approved this proposal already")

// ErrMoved is a proposal not in the state a step needs: another step, or
// another person, moved it first.
var ErrMoved = errors.New("this proposal has moved on since: read it again")

// Approve notes a person's approval of a proposal, and approves it once it
// has need of them. No one approves their own proposal, or one twice.
func (l *Ledger) Approve(ctx context.Context, id int64, by string, need int) (Proposal, error) {
	var p Proposal
	err := l.write(ctx, func(t *txn) error {
		var err error
		if p, err = scanProposal(t.QueryRowContext(t.ctx, "SELECT "+proposalColumns+" FROM proposals WHERE id = ?", id)); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		switch {
		case p.State != StateProposed:
			return ErrMoved
		case strings.EqualFold(p.MadeBy, by):
			return ErrOwnProposal
		}
		var had int
		// In any letter case: one person, however the proxy spells them.
		if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM approvals WHERE proposal_id = ? AND lower(by) = lower(?)", id, by).Scan(&had); err != nil {
			return err
		}
		if had > 0 {
			return ErrApproved
		}
		now := time.Now().Unix()
		if _, err := t.ExecContext(t.ctx, "INSERT INTO approvals (proposal_id, by, at) VALUES (?, ?, ?)", id, by, now); err != nil {
			return err
		}
		if p.Approvals, err = l.approvals(t.ctx, t, id); err != nil {
			return err
		}
		if err := change(t, by, fmt.Sprintf("proposal #%d approved", id), nil, map[string]any{"approvals": len(p.Approvals), "need": need}); err != nil {
			return err
		}
		if len(p.Approvals) < need {
			return nil
		}
		p.State, p.Updated = StateApproved, unix(now)
		if _, err := t.ExecContext(t.ctx, "UPDATE proposals SET state = ?, updated = ? WHERE id = ?", p.State, now, id); err != nil {
			return err
		}
		return l.tell(t, "proposal.approved", by, p.about())
	})
	return p, err
}

// Step is what a step sets on a proposal besides its state.
type Step struct {
	Reason, Link, Backup string
	// Applied notes it was carried out now, which the limits count: sent to
	// Grafana, or handed to a recipe. A step it took effect by, read back
	// applied, leaves it as it was.
	Applied bool
	// Document, when set, is the change made afresh in the document as it
	// now reads, with its hash and diff: the rule is as it was, and only
	// the rest of its file changed.
	Document, DocHash, Diff string
	// AfterHash, when set, is what the rule hashes to as it was applied,
	// read back from where it is kept.
	AfterHash string
}

// events are what a proposal moving into a state is told as.
var events = map[string]string{StateOpened: "change.opened", StateApplied: "change.applied", StateRefused: "change.refused",
	StateFailed: "change.failed", StateUndone: "change.undone"}

// Move moves a proposal from one of the states from to to, with what the
// step sets, and keeps and tells of it; ErrMoved when it isn't in one of
// them.
func (l *Ledger) Move(ctx context.Context, id int64, from []string, to, by string, s Step) (Proposal, error) {
	var p Proposal
	err := l.write(ctx, func(t *txn) error {
		var err error
		if p, err = scanProposal(t.QueryRowContext(t.ctx, "SELECT "+proposalColumns+" FROM proposals WHERE id = ?", id)); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if !slices.Contains(from, p.State) {
			return ErrMoved
		}
		was := p.summary()
		now := time.Now().Truncate(time.Second)
		p.State, p.Updated = to, now
		if s.Reason != "" {
			p.Reason = s.Reason
		}
		if s.Link != "" {
			p.Link = s.Link
		}
		if s.Backup != "" {
			p.Backup = s.Backup
		}
		if s.Applied {
			p.Applied = now
		}
		if s.Document != "" {
			p.Document, p.DocHash, p.Diff = s.Document, s.DocHash, s.Diff
		}
		if s.AfterHash != "" {
			p.AfterHash = s.AfterHash
		}
		if _, err := t.ExecContext(t.ctx, `UPDATE proposals SET state = ?, updated = ?, reason = ?, link = ?, backup = ?, applied = ?,
			document = ?, doc_hash = ?, diff = ?, after_hash = ? WHERE id = ?`,
			p.State, now.Unix(), p.Reason, p.Link, p.Backup, nullable(p.Applied), p.Document, p.DocHash, p.Diff, p.AfterHash, id); err != nil {
			return err
		}
		if err := change(t, by, fmt.Sprintf("proposal #%d %s", id, to), was, p.summary()); err != nil {
			return err
		}
		if e := events[to]; e != "" {
			return l.tell(t, e, by, p.about())
		}
		return nil
	})
	return p, err
}

// Undo keeps the proposal that undoes an applied one, approved by whoever
// asked for it: an undo writes back what was, and is an approver's to ask
// for, as the change it undoes was. The applied proposal stays applied
// until its undo is.
func (l *Ledger) Undo(ctx context.Context, undo Proposal, by string) (int64, error) {
	if undo.Follows == 0 {
		return 0, invalid("an undo follows the change it undoes")
	}
	if len(undo.Evidence) == 0 {
		undo.Evidence = json.RawMessage("{}")
	}
	now := time.Now().Truncate(time.Second)
	var id int64
	err := l.write(ctx, func(t *txn) error {
		var state string
		if err := t.QueryRowContext(t.ctx, "SELECT state FROM proposals WHERE id = ?", undo.Follows).Scan(&state); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if state != StateApplied {
			return ErrMoved
		}
		var n int
		if err := t.QueryRowContext(t.ctx, `SELECT count(*) FROM proposals WHERE follows = ? AND kind = ? AND state IN (`+marks(len(unfinished))+`)`,
			append([]any{undo.Follows, undo.Kind}, anys(unfinished)...)...).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrProposed
		}
		undo.State, undo.Made, undo.Updated, undo.MadeBy = StateApproved, now, now, by
		if err := t.QueryRowContext(t.ctx, `INSERT INTO proposals (rule, kind, path, state, made, made_by, agent, source, doc_hash, before, before_hash,
			after, after_hash, document, diff, evidence, backup, follows, updated) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			undo.Rule, undo.Kind, undo.Path, undo.State, now.Unix(), by, undo.Source, undo.DocHash, undo.Before, undo.BeforeHash,
			undo.After, undo.AfterHash, undo.Document, undo.Diff, string(undo.Evidence), undo.Backup, undo.Follows, now.Unix()).Scan(&id); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "INSERT INTO approvals (proposal_id, by, at) VALUES (?, ?, ?)", id, by, now.Unix()); err != nil {
			return err
		}
		undo.ID = id
		if err := change(t, by, fmt.Sprintf("proposal #%d asked to be undone, as #%d", undo.Follows, id), nil, undo.summary()); err != nil {
			return err
		}
		return l.tell(t, "proposal.approved", by, undo.about())
	})
	return id, err
}

// SetProposalTask notes the task a proposal was made into.
func (l *Ledger) SetProposalTask(ctx context.Context, id, task int64) error {
	return l.writeAside(ctx, func(t *txn) error {
		_, err := t.ExecContext(t.ctx, "UPDATE proposals SET task_id = ? WHERE id = ?", task, id)
		return err
	})
}

// Carried are how many proposals Lapilli carried out (sent to Grafana, or
// handed to a recipe) since a time; of the kind given, when one is. Each is
// counted once, when it was carried out: a change a person made, its path
// none, never.
func (l *Ledger) Carried(ctx context.Context, since time.Time, kind string) (int, error) {
	q := "SELECT count(*) FROM proposals WHERE applied >= ? AND path != 'none'"
	args := []any{since.Unix()}
	if kind != "" {
		q += " AND kind = ?"
		args = append(args, kind)
	}
	var n int
	err := l.r.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
