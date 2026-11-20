package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// A proposal adding a rule (docs/design/expectations.md, step 5) is kept
// with what it is held to, and only it carries any; once it is applied,
// what it is held to is kept as a draft accepted by someone other than its
// proposer, once, however many drafts are let go, and not counted toward
// the proposer's hour. In SQLite or, with LAPILLI_TEST_POSTGRES, Postgres.
func TestAddedExpectations(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	text := "expectations:\n  - rule: ErrorsLow\n    budget:\n      week: 60\n"
	x, _ := json.Marshal(AddedExpect{Text: text, Tests: map[string]string{"tests/low.yaml": "tests: []\n"}})
	p := Proposal{Rule: "ErrorsLow", Kind: KindAdd, Path: PathGit, MadeBy: "Bo", Expect: string(x)}
	for _, bad := range []Proposal{
		{Rule: "ErrorsLow", Kind: KindAdd, Path: PathGit, MadeBy: "Bo"},
		{Rule: "ErrorsLow", Kind: KindAdd, Path: PathGit, MadeBy: "Bo", Expect: "{"},
		{Rule: "ErrorsLow", Kind: KindAdd, Path: PathGit, MadeBy: "Bo", Expect: `{"text": ""}`},
		{Rule: "Errors", Kind: KindFor, Path: PathGit, MadeBy: "Bo", Expect: string(x)},
	} {
		if _, err := l.Propose(ctx, bad); err == nil {
			t.Errorf("kept: %+v", bad)
		}
	}
	id, err := l.Propose(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := l.ProposalExpect(ctx, id); err != nil || got.Text != text || got.Tests["tests/low.yaml"] != "tests: []\n" || got.Kept != 0 {
		t.Fatalf("what it is held to: %+v %v", got, err)
	}
	d := ExpectDraft{Rule: "ErrorsLow", MadeBy: "Bo", Text: text, Target: DraftToNone, Proposal: id}
	if _, err := l.KeepAddedDraft(ctx, d, "ana", "", ""); err == nil {
		t.Error("kept before it was applied")
	}
	if ids, err := l.AddsUnkept(ctx, 10); err != nil || len(ids) != 0 {
		t.Errorf("unkept before applied: %v %v", ids, err)
	}
	if _, err := l.Approve(ctx, id, "ana", 1, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Move(ctx, id, []string{StateApproved}, StateApplied, Proposer, Step{}); err != nil {
		t.Fatal(err)
	}
	if ids, err := l.AddsUnkept(ctx, 10); err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("unkept: %v %v", ids, err)
	}
	if _, err := l.KeepAddedDraft(ctx, d, "BO", "", ""); !errors.Is(err, ErrOwnDraft) {
		t.Errorf("accepted by its proposer: %v", err)
	}
	did, err := l.KeepAddedDraft(ctx, d, "ana", "", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.AddedDraft(ctx, id)
	if err != nil || got.ID != did || got.State != DraftAccepted || got.Origin != DraftProposal || got.DecidedBy != "ana" || got.MadeBy != "bo" {
		t.Fatalf("kept: %+v %v", got, err)
	}
	if _, err := l.KeepAddedDraft(ctx, d, "ana", "", ""); !errors.Is(err, ErrMoved) {
		t.Errorf("kept twice: %v", err)
	}
	if ids, _ := l.AddsUnkept(ctx, 10); len(ids) != 0 {
		t.Errorf("still unkept: %v", ids)
	}
	// Let go, it isn't kept again.
	if _, err := l.w.ExecContext(ctx, "DELETE FROM expectation_drafts"); err != nil {
		t.Fatal(err)
	}
	if ids, _ := l.AddsUnkept(ctx, 10); len(ids) != 0 {
		t.Errorf("unkept once let go: %v", ids)
	}
	// The proposal counted toward bo's hour, the draft didn't: one each
	// way kept, two counted would be the add and the draft.
	if n, err := madeInHour(ctx, l.r, "bo", time.Now()); err != nil || n != 1 {
		t.Errorf("bo's hour: %d %v", n, err)
	}
}

// What a rule added reads along an index, in either database: the applied
// adds whose expectations aren't kept, by the proposals' state.
func TestAddsUnkeptIndexed(t *testing.T) {
	l := open(t, time.UTC)
	q := `SELECT id FROM proposals WHERE state = ?1 AND kind = ?2 AND expect NOT LIKE ?3 ORDER BY updated, id LIMIT ?4`
	if p := plan(t, l, q, StateApplied, KindAdd, `%"kept":%`, 20); scansAll(l, p, "proposals") {
		t.Errorf("reads proposals whole: %s", p)
	}
}
