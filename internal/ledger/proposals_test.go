package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func proposal(rule, kind string) Proposal {
	return Proposal{Rule: rule, Kind: kind, Path: PathGit, MadeBy: Proposer, Before: "for: 5m", BeforeHash: "b", After: "for: 15m", Diff: "-for: 5m\n+for: 15m\n"}
}

func toldEvents(t *testing.T, l *Ledger) []string {
	t.Helper()
	ns, err := l.Due(context.Background(), "webhook", time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range ns {
		out = append(out, n.Event)
	}
	return out
}

func TestProposals(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	l.TellTo([]string{"webhook"})
	id, err := l.Propose(ctx, proposal("HighErrors", KindFor))
	if err != nil {
		t.Fatal(err)
	}
	// One at a time, for a rule, of any kind: two merged would leave
	// neither as it expects the rule.
	for _, kind := range []string{KindFor, KindThreshold, KindSays, KindDelete} {
		if _, err := l.Propose(ctx, proposal("HighErrors", kind)); !errors.Is(err, ErrProposed) {
			t.Errorf("a %s beside it: %v", kind, err)
		}
	}
	for _, p := range []Proposal{proposal("", KindFor), proposal("X", "silence"), {Rule: "X", Kind: KindFor, Path: "cluster"}, proposal("X", KindUndo)} {
		if _, err := l.Propose(ctx, p); !errors.As(err, new(ErrInvalid)) {
			t.Errorf("%+v: %v, want refused", p, err)
		}
	}
	// No one approves their own; nobody twice; at need, approved.
	if _, err := l.Approve(ctx, id, Proposer, 1); !errors.Is(err, ErrOwnProposal) {
		t.Errorf("own: %v", err)
	}
	p, err := l.Approve(ctx, id, "ana", 2)
	if err != nil || p.State != StateProposed || len(p.Approvals) != 1 {
		t.Fatalf("first of two: %+v %v", p, err)
	}
	for _, again := range []string{"ana", "ANA"} {
		if _, err := l.Approve(ctx, id, again, 2); !errors.Is(err, ErrApproved) {
			t.Errorf("twice, as %s: %v", again, err)
		}
	}
	if p, err = l.Approve(ctx, id, "bo", 2); err != nil || p.State != StateApproved {
		t.Fatalf("second: %+v %v", p, err)
	}
	if _, err := l.Approve(ctx, id, "cy", 2); !errors.Is(err, ErrMoved) {
		t.Errorf("approved already: %v", err)
	}
	// Steps move it on only from where it is.
	if _, err := l.Move(ctx, id, []string{StateReady}, StateOpened, ChangesCopier, Step{Link: "https://git.example/pr/1"}); !errors.Is(err, ErrMoved) {
		t.Errorf("opened before ready: %v", err)
	}
	if p, err = l.Move(ctx, id, []string{StateApproved}, StateReady, Proposer, Step{Applied: true, Backup: "kept"}); err != nil || p.Applied.IsZero() {
		t.Fatalf("ready: %+v %v", p, err)
	}
	if n, err := l.Carried(ctx, time.Now().Add(-time.Hour), ""); err != nil || n != 1 {
		t.Errorf("carried: %d %v", n, err)
	}
	if n, err := l.Carried(ctx, time.Now().Add(-time.Hour), KindDelete); err != nil || n != 0 {
		t.Errorf("deletions carried: %d %v", n, err)
	}
	if _, err := l.Move(ctx, id, []string{StateReady}, StateOpened, ChangesCopier, Step{Link: "https://git.example/pr/1"}); err != nil {
		t.Fatal(err)
	}
	got, err := l.ProposalByID(ctx, id)
	if err != nil || got.State != StateOpened || got.Link != "https://git.example/pr/1" || got.Backup != "kept" || len(got.Approvals) != 2 {
		t.Errorf("read back: %+v %v", got, err)
	}
	ps, err := l.Proposals(ctx, ProposalQuery{Rule: "HighErrors", States: []string{StateOpened}})
	if err != nil || len(ps) != 1 || ps[0].ID != id {
		t.Errorf("by rule and state: %+v %v", ps, err)
	}
	if _, err := l.ProposalByID(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("none: %v", err)
	}
	want := "proposal.made proposal.approved change.opened"
	if got := strings.Join(toldEvents(t, l), " "); got != want {
		t.Errorf("told %q, want %q", got, want)
	}
}

func TestUndo(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	id, err := l.Propose(ctx, proposal("HighErrors", KindFor))
	if err != nil {
		t.Fatal(err)
	}
	undo := Proposal{Rule: "HighErrors", Kind: KindUndo, Path: PathGit, Follows: id, Before: "for: 15m", After: "for: 5m"}
	if _, err := l.Undo(ctx, undo, "ana"); !errors.Is(err, ErrMoved) {
		t.Errorf("undoing one not applied: %v", err)
	}
	if _, err := l.Approve(ctx, id, "ana", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Move(ctx, id, []string{StateApproved}, StateApplied, Proposer, Step{Applied: true}); err != nil {
		t.Fatal(err)
	}
	uid, err := l.Undo(ctx, undo, "bo")
	if err != nil {
		t.Fatal(err)
	}
	u, err := l.ProposalByID(ctx, uid)
	if err != nil || u.State != StateApproved || u.MadeBy != "bo" || len(u.Approvals) != 1 || u.Follows != id {
		t.Errorf("the undo: %+v %v", u, err)
	}
	if _, err := l.Undo(ctx, undo, "bo"); !errors.Is(err, ErrProposed) {
		t.Errorf("undone twice at once: %v", err)
	}
}
