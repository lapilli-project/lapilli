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
	"unicode/utf8"
)

// Drafts of what a rule is held to (docs/design/expectations.md, step 3):
// an agent's, through MCP, or the history's, kept from a rule's tab, for
// a person to accept or drop (expectation_drafts, 027.sql). Lapilli
// accepts none itself; whoever proposed one, or whose agent did, can't
// accept it; and drafts count toward a person's proposals an hour.

// Where a draft came from.
const (
	DraftAgent   = "agent"
	DraftHistory = "history"
	// DraftProposal is what a proposal adding a rule held it to, kept
	// accepted once the rule is added (KeepAddedDraft).
	DraftProposal = "proposal"
)

// A draft's states.
const (
	DraftOpen     = "open"
	DraftAccepted = "accepted"
	DraftDropped  = "dropped"
)

// Where accepting a draft goes: the expectations file serve reads (by the
// git path when it can), serve's configuration, or nowhere serve knows.
const (
	DraftToFile   = "file"
	DraftToConfig = "config"
	DraftToNone   = "none"
)

// A draft's pull request, by the git path: ready for a recipe, opened,
// merged, closed without merging, or failed.
const (
	DraftReady  = "ready"
	DraftOpened = "opened"
	DraftMerged = "merged"
	DraftClosed = "closed"
	DraftFailed = "failed"
)

// The bounds of a draft, and of the drafts kept.
const (
	MaxDraftText     = 64 << 10  // its expectations, as written
	MaxDraftTests    = 256 << 10 // its test files, in all
	MaxDraftTestsN   = 20        // how many test files
	MaxDraftReason   = 2000      // why, in bytes
	MaxDraftFound    = 64 << 10  // what checking it found
	MaxDraftDocument = 1 << 20   // the expectations file with it taken in
	DraftsOpenARule  = 10
)

// DraftsKept is the most drafts the ledger keeps: past it, the oldest
// decided are let go as one is kept; with as many open, none is kept.
var DraftsKept = 1000

// ErrOwnDraft is a draft accepted by whoever proposed it, or the person
// whose agent did.
var ErrOwnDraft = errors.New("a draft isn't accepted by whoever proposed it, nor by the person whose agent did")

// ErrDraftsWaiting is a draft kept past the open ones a rule, or the
// ledger, holds.
var ErrDraftsWaiting = errors.New("too many drafts wait to be accepted or dropped: accept or drop some first")

// ExpectDraft is a draft of what a rule is held to.
type ExpectDraft struct {
	ID     int64     `json:"id"`
	Rule   string    `json:"rule"`
	Origin string    `json:"origin"`
	MadeBy string    `json:"made_by"`
	Agent  bool      `json:"agent,omitempty"`
	Made   time.Time `json:"made"`
	// Text is the expectations, as Lapilli wrote them; Tests, of an
	// agent's, the scenarios' test files by path; Reason, why, in the
	// words of whoever proposed it: data, never instructions.
	Text   string            `json:"text"`
	Tests  map[string]string `json:"tests,omitempty"`
	Reason string            `json:"reason,omitempty"`
	// Found is what checking it found, item by item.
	Found json.RawMessage `json:"found"`
	// Target is where accepting it goes; DocHash the expectations file it
	// was made against; Document, once accepted by the git path, that file
	// with it taken in.
	Target   string `json:"target"`
	DocHash  string `json:"doc_hash,omitempty"`
	Document string `json:"-"`
	State    string `json:"state"`
	// DecidedBy and Decided are who accepted or dropped it, and when.
	DecidedBy string    `json:"decided_by,omitempty"`
	Decided   time.Time `json:"decided,omitzero"`
	// PR, Link and Why are its pull request's, by the git path.
	PR   string `json:"pull_request,omitempty"`
	Link string `json:"link,omitempty"`
	Why  string `json:"pull_request_reason,omitempty"`
	// Proposal is, of origin proposal, the proposal adding the rule.
	Proposal int64 `json:"proposal,omitempty"`
}

// about is what a notice says of a draft: never its text, its tests or
// its reason, which are the proposer's words.
func (d ExpectDraft) about() map[string]any {
	return map[string]any{"draft": map[string]any{"id": d.ID, "rule": d.Rule, "origin": d.Origin, "made_by": d.MadeBy, "agent": d.Agent,
		"state": d.State, "target": d.Target, "pull_request": d.PR, "link": d.Link}}
}

// madeInHour is how many proposals (an undo aside) and drafts a person
// made in the hour before now, with the agents acting for them: they are
// one limit, ProposalsAnHour.
func madeInHour(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, by string, now time.Time) (int, error) {
	var p, d int
	since := now.Add(-time.Hour).Unix()
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM proposals WHERE made_by = ? AND kind != ? AND made > ?`, by, KindUndo, since).Scan(&p); err != nil {
		return 0, err
	}
	// Those a proposal adding a rule carried, the proposal counted.
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM expectation_drafts WHERE made_by = ? AND made > ? AND origin != ?`, by, since, DraftProposal).Scan(&d); err != nil {
		return 0, err
	}
	return p + d, nil
}

// MayDraft says whether KeepDraft would refuse a draft by a person now for
// the hour's limit, which proposals and drafts share: asked before the
// work a draft takes (a replay of twelve weeks), so one refused costs
// nothing. KeepDraft asks again as it keeps one.
func (l *Ledger) MayDraft(ctx context.Context, by string) error {
	n, err := madeInHour(ctx, l.r, FoldName(by), time.Now())
	if err != nil {
		return err
	}
	if n >= ProposalsAnHour {
		return ErrTooManyProposals
	}
	return nil
}

// KeepDraft keeps a draft, open: within its bounds, the hour's limit it
// shares with proposals, DraftsOpenARule open for its rule and DraftsKept
// in all, the oldest decided let go to keep it. It is told
// (expectation.drafted).
func (l *Ledger) KeepDraft(ctx context.Context, d ExpectDraft) (int64, error) {
	tests, err := json.Marshal(d.Tests)
	if err != nil {
		return 0, err
	}
	if d.Tests == nil {
		tests = []byte("{}")
	}
	testsLen := 0
	for p, t := range d.Tests {
		testsLen += len(p) + len(t)
	}
	switch {
	case d.Rule == "" || !plain(d.Rule):
		return 0, invalid("a draft is of a rule")
	case d.Origin != DraftAgent && d.Origin != DraftHistory:
		return 0, invalid("a draft is an agent's or the history's")
	case d.MadeBy == "" || !plain(d.MadeBy):
		return 0, invalid("a draft is made by someone")
	case !slices.Contains([]string{DraftToFile, DraftToConfig, DraftToNone}, d.Target):
		return 0, invalid("a draft goes to the expectations file, serve's configuration or none")
	case d.Text == "" || len(d.Text) > MaxDraftText || !utf8.ValidString(d.Text):
		return 0, invalid("a draft's expectations are %d bytes at most", MaxDraftText)
	case len(d.Tests) > MaxDraftTestsN || testsLen > MaxDraftTests:
		return 0, invalid("a draft holds %d test files, %d bytes in all, at most", MaxDraftTestsN, MaxDraftTests)
	case len(d.Reason) > MaxDraftReason || !utf8.ValidString(d.Reason):
		return 0, invalid("a draft's reason is %d bytes at most", MaxDraftReason)
	case len(d.Found) > MaxDraftFound:
		return 0, invalid("what checking a draft found is %d bytes at most", MaxDraftFound)
	case len(d.DocHash) > 64:
		return 0, invalid("a draft's file is named by its hash")
	case len(d.Found) == 0:
		d.Found = json.RawMessage("[]")
	}
	now := time.Now()
	d.State, d.Made, d.MadeBy = DraftOpen, now.Truncate(time.Second), FoldName(d.MadeBy)
	err = l.write(ctx, func(t *txn) error {
		n, err := madeInHour(t.ctx, t, d.MadeBy, now)
		if err != nil {
			return err
		}
		if n >= ProposalsAnHour {
			return ErrTooManyProposals
		}
		var open, kept int
		if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM expectation_drafts WHERE "+l.d.pick("rule = ?", "md5(rule) = md5(?) AND rule = ?")+" AND state = ?",
			ruleArgs(l.d, d.Rule, DraftOpen)...).Scan(&open); err != nil {
			return err
		}
		if open >= DraftsOpenARule {
			return ErrDraftsWaiting
		}
		if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM expectation_drafts").Scan(&kept); err != nil {
			return err
		}
		if kept >= DraftsKept {
			// The oldest decided, as many as keeping this one takes.
			res, err := t.ExecContext(t.ctx, `DELETE FROM expectation_drafts WHERE id IN (SELECT id FROM expectation_drafts WHERE state != ? ORDER BY id LIMIT ?)`,
				DraftOpen, kept-DraftsKept+1)
			if err != nil {
				return err
			}
			if gone, err := res.RowsAffected(); err != nil {
				return err
			} else if int(gone) < kept-DraftsKept+1 {
				return ErrDraftsWaiting
			}
		}
		if err := t.QueryRowContext(t.ctx, `INSERT INTO expectation_drafts (rule, origin, made_by, agent, made, text, tests, reason, found, target, doc_hash, state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			d.Rule, d.Origin, d.MadeBy, d.Agent, d.Made.Unix(), d.Text, string(tests), d.Reason, string(d.Found), d.Target, d.DocHash, d.State).Scan(&d.ID); err != nil {
			return err
		}
		if err := change(t, d.MadeBy, fmt.Sprintf("expectation draft #%d made", d.ID), nil, d.about()); err != nil {
			return err
		}
		return l.tell(t, "expectation.drafted", d.MadeBy, d.about())
	})
	return d.ID, err
}

// ruleArgs are a rule's name as a query on expectation_drafts asks it,
// then the other arguments: twice in Postgres, once by its md5.
func ruleArgs(d dialect, rule string, more ...any) []any {
	args := []any{rule}
	if d == postgres {
		args = append(args, rule)
	}
	return append(args, more...)
}

const draftCols = "id, rule, origin, made_by, agent, made, text, tests, reason, found, target, doc_hash, document, state, decided_by, decided, pr, link, pr_reason, proposal"

func scanDraft(row interface{ Scan(...any) error }) (ExpectDraft, error) {
	var d ExpectDraft
	var made, decided int64
	var tests, found string
	if err := row.Scan(&d.ID, &d.Rule, &d.Origin, &d.MadeBy, &d.Agent, &made, &d.Text, &tests, &d.Reason, &found, &d.Target, &d.DocHash, &d.Document,
		&d.State, &d.DecidedBy, &decided, &d.PR, &d.Link, &d.Why, &d.Proposal); err != nil {
		return d, err
	}
	d.Made, d.Found = unix(made), json.RawMessage(found)
	if decided != 0 {
		d.Decided = unix(decided)
	}
	if err := json.Unmarshal([]byte(tests), &d.Tests); err != nil {
		return d, err
	}
	if len(d.Tests) == 0 {
		d.Tests = nil
	}
	return d, nil
}

// DraftByID is one draft.
func (l *Ledger) DraftByID(ctx context.Context, id int64) (ExpectDraft, error) {
	d, err := scanDraft(l.r.QueryRowContext(ctx, "SELECT "+draftCols+" FROM expectation_drafts WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// DraftQuery picks drafts: of a rule (when HasRule), in some states, of
// a pull request's states, the latest first, Limit at most.
type DraftQuery struct {
	Rule    string
	HasRule bool
	States  []string
	PRs     []string
	Limit   int
}

// Drafts are the drafts a query picks, the latest first.
func (l *Ledger) Drafts(ctx context.Context, q DraftQuery) ([]ExpectDraft, error) {
	var where []string
	var args []any
	if q.HasRule {
		where = append(where, l.d.pick("rule = ?", "md5(rule) = md5(?) AND rule = ?"))
		args = ruleArgs(l.d, q.Rule)
	}
	if len(q.States) > 0 {
		where = append(where, "state IN ("+marks(len(q.States))+")")
		args = append(args, anys(q.States)...)
	}
	if len(q.PRs) > 0 {
		where = append(where, "pr IN ("+marks(len(q.PRs))+")")
		args = append(args, anys(q.PRs)...)
	}
	s := "SELECT " + draftCols + " FROM expectation_drafts"
	if len(where) > 0 {
		s += " WHERE " + strings.Join(where, " AND ")
	}
	s += " ORDER BY id DESC"
	limit := q.Limit
	if limit <= 0 || limit > DraftsKept {
		limit = DraftsKept
	}
	s += fmt.Sprintf(" LIMIT %d", limit)
	rows, err := l.r.QueryContext(ctx, s, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExpectDraft
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// KeepAddedDraft keeps what a proposal adding a rule held it to, as the
// rule is added: a draft of origin proposal, made by whoever proposed the
// rule, accepted by by (its first approver, never the proposer), with
// document, by the git path, ready for a recipe, as AcceptDraft makes one.
// Not counted toward the hour's limit (the proposal was), nor held to the
// drafts waiting for a rule: it waits for none. Kept once a proposal. It
// is told (expectation.accepted).
func (l *Ledger) KeepAddedDraft(ctx context.Context, d ExpectDraft, by, document, docHash string) (int64, error) {
	tests, err := json.Marshal(d.Tests)
	if err != nil {
		return 0, err
	}
	if d.Tests == nil {
		tests = []byte("{}")
	}
	by = FoldName(by)
	switch {
	case d.Proposal == 0 || d.Rule == "" || !plain(d.Rule) || d.MadeBy == "" || !plain(d.MadeBy):
		return 0, invalid("a rule added's expectations are of its proposal, its rule and who proposed it")
	case FoldName(d.MadeBy) == by:
		return 0, ErrOwnDraft
	case !slices.Contains([]string{DraftToFile, DraftToConfig, DraftToNone}, d.Target):
		return 0, invalid("a draft goes to the expectations file, serve's configuration or none")
	case d.Text == "" || len(d.Text) > MaxDraftText || !utf8.ValidString(d.Text):
		return 0, invalid("a draft's expectations are %d bytes at most", MaxDraftText)
	case len(document) > MaxDraftDocument:
		return 0, invalid("the expectations file with a draft taken in is %d bytes at most", MaxDraftDocument)
	case document != "" && (len(docHash) != 64 || strings.Trim(docHash, "0123456789abcdef") != ""):
		return 0, invalid("a draft's file is named by its hash")
	case len(d.Found) == 0:
		d.Found = json.RawMessage("[]")
	}
	now := time.Now().Truncate(time.Second)
	d.Origin, d.State, d.Made, d.MadeBy, d.DecidedBy, d.Decided = DraftProposal, DraftAccepted, now, FoldName(d.MadeBy), by, now
	if document != "" {
		d.PR, d.Document, d.DocHash = DraftReady, document, docHash
	}
	err = l.write(ctx, func(t *txn) error {
		// Kept once a proposal: its expectations say so, and say which,
		// however many drafts are let go since.
		var raw, kind, state string
		if err := t.QueryRowContext(t.ctx, "SELECT expect, kind, state FROM proposals WHERE id = ?", d.Proposal).Scan(&raw, &kind, &state); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var x AddedExpect
		if kind != KindAdd || state != StateApplied || json.Unmarshal([]byte(raw), &x) != nil {
			return invalid("a rule added's expectations are kept once it is added")
		}
		if x.Kept != 0 {
			return ErrMoved
		}
		var kept int
		if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM expectation_drafts").Scan(&kept); err != nil {
			return err
		}
		if kept >= DraftsKept {
			if _, err := t.ExecContext(t.ctx, `DELETE FROM expectation_drafts WHERE id IN (SELECT id FROM expectation_drafts WHERE state != ? AND pr NOT IN (?, ?) ORDER BY id LIMIT ?)`,
				DraftOpen, DraftReady, DraftOpened, kept-DraftsKept+1); err != nil {
				return err
			}
		}
		if err := t.QueryRowContext(t.ctx, `INSERT INTO expectation_drafts (rule, origin, made_by, agent, made, text, tests, reason, found, target, doc_hash, document,
			state, decided_by, decided, pr, proposal) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			d.Rule, d.Origin, d.MadeBy, d.Agent, d.Made.Unix(), d.Text, string(tests), d.Reason, string(d.Found), d.Target, d.DocHash, d.Document,
			d.State, d.DecidedBy, d.Decided.Unix(), d.PR, d.Proposal).Scan(&d.ID); err != nil {
			return err
		}
		x.Kept = d.ID
		marked, err := json.Marshal(x)
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE proposals SET expect = ? WHERE id = ?", string(marked), d.Proposal); err != nil {
			return err
		}
		if err := change(t, by, fmt.Sprintf("expectation draft #%d accepted, as proposal #%d adds its rule", d.ID, d.Proposal), nil, d.about()); err != nil {
			return err
		}
		return l.tell(t, "expectation.accepted", by, d.about())
	})
	return d.ID, err
}

// AddedDraft is the draft a proposal adding a rule was kept as
// (KeepAddedDraft); ErrNotFound when it wasn't, yet.
func (l *Ledger) AddedDraft(ctx context.Context, proposal int64) (ExpectDraft, error) {
	d, err := scanDraft(l.r.QueryRowContext(ctx, "SELECT "+draftCols+" FROM expectation_drafts WHERE origin = ? AND proposal = ?", DraftProposal, proposal))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// AcceptDraft notes a person's acceptance of an open draft: refused for
// whoever proposed it, or whose agent did (in any letter case, as
// proposals' approvers are told apart), and for one decided meanwhile.
// With document, it is accepted by the git path: ready for a recipe to
// open its pull request with that file, in place of the one whose hash is
// docHash. It is told (expectation.accepted).
func (l *Ledger) AcceptDraft(ctx context.Context, id int64, by, document, docHash string) (ExpectDraft, error) {
	return l.decideDraft(ctx, id, by, DraftAccepted, document, docHash)
}

// DropDraft drops an open draft: whoever proposed it may, as anyone who
// may drop a proposal. It is told (expectation.dropped).
func (l *Ledger) DropDraft(ctx context.Context, id int64, by string) (ExpectDraft, error) {
	return l.decideDraft(ctx, id, by, DraftDropped, "", "")
}

func (l *Ledger) decideDraft(ctx context.Context, id int64, by, to, document, docHash string) (ExpectDraft, error) {
	var d ExpectDraft
	by = FoldName(by)
	switch {
	case len(document) > MaxDraftDocument:
		return d, invalid("the expectations file with a draft taken in is %d bytes at most", MaxDraftDocument)
	case document != "" && (len(docHash) != 64 || strings.Trim(docHash, "0123456789abcdef") != ""):
		return d, invalid("a draft's file is named by its hash")
	}
	err := l.write(ctx, func(t *txn) error {
		var err error
		if d, err = scanDraft(t.QueryRowContext(t.ctx, "SELECT "+draftCols+" FROM expectation_drafts WHERE id = ?", id)); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		switch {
		case d.State != DraftOpen:
			return ErrMoved
		case to == DraftAccepted && FoldName(d.MadeBy) == by:
			return ErrOwnDraft
		}
		now := time.Now().Unix()
		d.State, d.DecidedBy, d.Decided = to, by, unix(now)
		if document != "" {
			d.PR, d.Document, d.DocHash = DraftReady, document, docHash
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE expectation_drafts SET state = ?, decided_by = ?, decided = ?, pr = ?, document = ?, doc_hash = ? WHERE id = ? AND state = ?",
			d.State, d.DecidedBy, now, d.PR, d.Document, d.DocHash, id, DraftOpen); err != nil {
			return err
		}
		if err := change(t, by, fmt.Sprintf("expectation draft #%d %s", id, to), nil, d.about()); err != nil {
			return err
		}
		return l.tell(t, "expectation."+to, by, d.about())
	})
	return d, err
}

// MoveDraftPR moves an accepted draft's pull request from one of from to
// to, as a recipe says: opened with its link, merged, closed or failed
// with why. ErrMoved when it isn't in one of from.
func (l *Ledger) MoveDraftPR(ctx context.Context, id int64, from []string, to, link, why string) (ExpectDraft, error) {
	var d ExpectDraft
	err := l.write(ctx, func(t *txn) error {
		var err error
		if d, err = scanDraft(t.QueryRowContext(t.ctx, "SELECT "+draftCols+" FROM expectation_drafts WHERE id = ?", id)); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if d.State != DraftAccepted || !slices.Contains(from, d.PR) {
			return ErrMoved
		}
		d.PR = to
		if link != "" {
			d.Link = link
		}
		d.Why = why
		if to != DraftReady {
			d.Document = "" // handed out: its pull request holds it
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE expectation_drafts SET pr = ?, link = ?, pr_reason = ?, document = ? WHERE id = ?",
			d.PR, d.Link, d.Why, d.Document, id); err != nil {
			return err
		}
		if err := change(t, ChangesCopier, fmt.Sprintf("expectation draft #%d pull request %s", id, to), nil, d.about()); err != nil {
			return err
		}
		return l.tell(t, "expectation.pull_request", ChangesCopier, d.about())
	})
	return d, err
}
