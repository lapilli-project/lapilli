package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
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
// adds whose expectations wait to be kept, by their kind, state and
// expect_kept; the draft one was kept as, by its proposal.
func TestAddsUnkeptIndexed(t *testing.T) {
	l := open(t, time.UTC)
	q := `SELECT id FROM proposals WHERE kind = ?1 AND state = ?2 AND expect_kept = 0 ORDER BY updated, id LIMIT ?3`
	if p := plan(t, l, q, KindAdd, StateApplied, 20); scansAll(l, p, "proposals") || !strings.Contains(p, "proposals_unkept") {
		t.Errorf("reads proposals whole, or not along proposals_unkept: %s", p)
	}
	q = "SELECT " + draftCols + " FROM expectation_drafts WHERE origin = ?1 AND proposal = ?2"
	if p := plan(t, l, q, DraftProposal, int64(7)); scansAll(l, p, "expectation_drafts") {
		t.Errorf("reads expectation_drafts whole: %s", p)
	}
}

// Only what can be kept waits to be: an add applied with no one's approval
// (its diff merged by hand) is never kept, and leaves the list as it is
// applied, so twenty such hold up no approved one; one refused as it is
// kept is marked never to be (SkipAddedExpect), and leaves it too. Which
// wait is told by expect_kept, not by what the JSON holds: a test file
// named kept is no marker. In SQLite or, with LAPILLI_TEST_POSTGRES,
// Postgres.
func TestAddsUnkeptOnlyKeepable(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	text := "expectations:\n  - rule: R\n    budget:\n      week: 5\n"
	add := func(rule, by string, tests map[string]string, approve bool) int64 {
		t.Helper()
		x, _ := json.Marshal(AddedExpect{Text: text, Tests: tests})
		id, err := l.Propose(ctx, Proposal{Rule: rule, Kind: KindAdd, Path: PathGit, MadeBy: by, Expect: string(x)})
		if err != nil {
			t.Fatal(err)
		}
		from := StateProposed
		if approve {
			if _, err := l.Approve(ctx, id, "ana", 1, time.Time{}); err != nil {
				t.Fatal(err)
			}
			from = StateApproved
		}
		if _, err := l.Move(ctx, id, []string{from}, StateApplied, Proposer, Step{}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i := range 20 {
		add("Hand"+strconv.Itoa(i), "u"+strconv.Itoa(i)+"@example.com", nil, false)
	}
	kept := add("Approved", "bo", map[string]string{"kept": "tests: []\n"}, true)
	refused := add("Refused", "cy", nil, true)
	ids, err := l.AddsUnkept(ctx, 20)
	if err != nil || len(ids) != 2 || ids[0] != kept || ids[1] != refused {
		t.Fatalf("waiting to be kept: %v %v", ids, err)
	}
	if x, err := l.ProposalExpect(ctx, ids[0]-1); err != nil || x.Kept != -1 {
		t.Errorf("an add made by hand: kept %d %v", x.Kept, err)
	}
	if err := l.SkipAddedExpect(ctx, refused); err != nil {
		t.Fatal(err)
	}
	d := ExpectDraft{Rule: "Approved", MadeBy: "bo", Text: text, Target: DraftToNone, Proposal: kept}
	did, err := l.KeepAddedDraft(ctx, d, "ana", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ids, _ := l.AddsUnkept(ctx, 20); len(ids) != 0 {
		t.Errorf("still waiting: %v", ids)
	}
	if x, _ := l.ProposalExpect(ctx, kept); x.Kept != did || x.Tests["kept"] != "tests: []\n" {
		t.Errorf("kept as %d, not %d: %+v", x.Kept, did, x)
	}
	if x, _ := l.ProposalExpect(ctx, refused); x.Kept != -1 {
		t.Errorf("refused, kept %d", x.Kept)
	}
	// Marked never to be kept, it isn't kept after.
	if _, err := l.KeepAddedDraft(ctx, ExpectDraft{Rule: "Refused", MadeBy: "cy", Text: text, Target: DraftToNone, Proposal: refused}, "ana", "", ""); !errors.Is(err, ErrMoved) {
		t.Errorf("kept once marked never to be: %v", err)
	}
}
