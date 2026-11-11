package ledger

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// One person, however the sign-in proxy spells them: in any letter case,
// Unicode's too, they neither approve their own proposal nor approve one
// twice.
func TestNamesFolded(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	p := proposal("HighErrors", KindFor)
	p.MadeBy = "Ǆemal@example.com"
	id, err := l.Propose(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, own := range []string{"ǆemal@example.com", "ǅEMAL@EXAMPLE.COM"} {
		if _, err := l.Approve(ctx, id, own, 2); !errors.Is(err, ErrOwnProposal) {
			t.Errorf("their own, as %s: %v", own, err)
		}
	}
	if _, err := l.Approve(ctx, id, "ÉMILE@example.com", 2); err != nil {
		t.Fatal(err)
	}
	for _, again := range []string{"émile@example.com", "ÉMILE@EXAMPLE.COM", "Émile@Example.Com"} {
		if _, err := l.Approve(ctx, id, again, 2); !errors.Is(err, ErrApproved) {
			t.Errorf("twice, as %s: %v", again, err)
		}
	}
	got, _ := l.ProposalByID(ctx, id)
	if got.State != StateProposed || len(got.Approvals) != 1 || got.MadeBy != "ǆemal@example.com" {
		t.Errorf("one approval of two, by one person: %+v", got)
	}
	for _, c := range [][2]string{{"K", "K"}, {"s", "ſ"}, {"Σ", "ς"}, {"ana", "ANA"}} {
		if FoldName(c[0]) != FoldName(c[1]) || !strings.EqualFold(c[0], c[1]) {
			t.Errorf("%q and %q: %q, %q", c[0], c[1], FoldName(c[0]), FoldName(c[1]))
		}
	}
}

// A person, with their agents, makes ProposalsAnHour proposals an hour at
// most; Lapilli's own aren't counted with theirs.
func TestProposalsAnHour(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	for i := range ProposalsAnHour {
		p := proposal("R"+strconv.Itoa(i), KindSays)
		p.MadeBy = "ana"
		if i%2 == 0 {
			p.MadeBy, p.Agent = "ANA", true // her agent, as the proxy spells her
		}
		if _, err := l.Propose(ctx, p); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	p := proposal("One more", KindFor)
	p.MadeBy = "ana"
	if _, err := l.Propose(ctx, p); !errors.Is(err, ErrTooManyProposals) {
		t.Errorf("past the hour's: %v", err)
	}
	p.MadeBy = "bo"
	if _, err := l.Propose(ctx, p); err != nil {
		t.Errorf("someone else: %v", err)
	}
	if _, err := l.Propose(ctx, proposal("Lapilli's", KindFor)); err != nil {
		t.Errorf("Lapilli's own: %v", err)
	}
}

// --approvals lowered: a proposal with the approvals now needed is moved
// on, and told; one for a person to make isn't.
func TestPromote(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	l.TellTo([]string{"webhook"})
	a, _ := l.Propose(ctx, proposal("A", KindFor))
	b, _ := l.Propose(ctx, proposal("B", KindFor))
	none := proposal("C", KindFor)
	none.Path = PathNone
	c, _ := l.Propose(ctx, none)
	for _, id := range []int64{a, c} {
		if _, err := l.Approve(ctx, id, "ana", 2); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := l.Promote(ctx, 2); err != nil || n != 0 {
		t.Fatalf("none has two: %d %v", n, err)
	}
	if n, err := l.Promote(ctx, 1); err != nil || n != 1 {
		t.Fatalf("one has one: %d %v", n, err)
	}
	for id, want := range map[int64]string{a: StateApproved, b: StateProposed, c: StateProposed} {
		if p, _ := l.ProposalByID(ctx, id); p.State != want {
			t.Errorf("#%d: %s, want %s", id, p.State, want)
		}
	}
	if got := strings.Join(toldEvents(t, l), " "); !strings.HasSuffix(got, "proposal.approved") {
		t.Errorf("told %q", got)
	}
}

// An undo waits for what else is unfinished for its rule, and a proposal
// for the undo.
func TestUndoOnePerRule(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	id, _ := l.Propose(ctx, proposal("HighErrors", KindFor))
	l.Approve(ctx, id, "ana", 1)
	l.Move(ctx, id, []string{StateApproved}, StateApplied, Proposer, Step{Applied: true})
	other, err := l.Propose(ctx, proposal("HighErrors", KindThreshold))
	if err != nil {
		t.Fatal(err)
	}
	undo := Proposal{Rule: "HighErrors", Kind: KindUndo, Path: PathGit, Follows: id, Before: "for: 15m", After: "for: 5m"}
	if _, err := l.Undo(ctx, undo, "ana"); !errors.Is(err, ErrProposed) {
		t.Errorf("undone beside another: %v", err)
	}
	l.Move(ctx, other, []string{StateProposed}, StateDropped, "bo", Step{})
	if _, err := l.Undo(ctx, undo, "ana"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Propose(ctx, proposal("HighErrors", KindSays)); !errors.Is(err, ErrProposed) {
		t.Errorf("proposed beside an undo: %v", err)
	}
}
