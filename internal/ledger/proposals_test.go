package ledger

import (
	"context"
	"errors"
	"fmt"
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
	if _, err := l.Approve(ctx, id, Proposer, 1, time.Time{}); !errors.Is(err, ErrOwnProposal) {
		t.Errorf("own: %v", err)
	}
	p, err := l.Approve(ctx, id, "ana", 2, time.Time{})
	if err != nil || p.State != StateProposed || len(p.Approvals) != 1 {
		t.Fatalf("first of two: %+v %v", p, err)
	}
	for _, again := range []string{"ana", "ANA"} {
		if _, err := l.Approve(ctx, id, again, 2, time.Time{}); !errors.Is(err, ErrApproved) {
			t.Errorf("twice, as %s: %v", again, err)
		}
	}
	if p, err = l.Approve(ctx, id, "bo", 2, time.Time{}); err != nil || p.State != StateApproved {
		t.Fatalf("second: %+v %v", p, err)
	}
	if _, err := l.Approve(ctx, id, "cy", 2, time.Time{}); !errors.Is(err, ErrMoved) {
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
	if _, err := l.Approve(ctx, id, "ana", 1, time.Time{}); err != nil {
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

// TestBacktestAgain: a proposal waiting for approval is given its evidence
// worked out again in place of what it had, the approvals given to that
// taken back, and both kept in its history, with who asked; one that
// moved on, or whose evidence changed meanwhile, isn't.
func TestBacktestAgain(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	p := proposal("HighErrors", KindFor)
	p.Evidence = []byte(`{"from":"suggestion","backtest":{"before":{"firings":141}}}`)
	id, err := l.Propose(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Approve(ctx, id, "ana", 2, time.Time{}); err != nil {
		t.Fatal(err)
	}
	was := []byte(`{"from":"suggestion","backtest":{"before":{"firings":141}}}`)
	now := []byte(`{"from":"suggestion","version":"v0.42.0","backtest":{"before":{"firings":140},"no_record":true}}`)
	if _, _, err := l.BacktestAgain(ctx, id, "bo", false, []byte(`{"other":1}`), now, nil, nil); !errors.Is(err, ErrMoved) {
		t.Errorf("its evidence changed meanwhile: %v", err)
	}
	l.TellTo([]string{"webhook"})
	got, withdrawn, err := l.BacktestAgain(ctx, id, "Bo", true, was, now, map[string]any{"version": ""}, map[string]any{"version": "v0.42.0"})
	if err != nil || len(withdrawn) != 1 || withdrawn[0] != "ana" || string(got.Evidence) != string(now) || got.State != StateProposed {
		t.Fatalf("backtested again: %+v %v %v", got, withdrawn, err)
	}
	// Told, naming whose approvals were withdrawn, and that an agent asked.
	ns, err := l.Due(ctx, "webhook", time.Now().Add(time.Hour), 10)
	if err != nil || len(ns) != 1 || ns[0].Event != "proposal.backtested" || !strings.Contains(string(ns[0].Body), `"approvals_withdrawn":["ana"]`) ||
		!strings.Contains(string(ns[0].Body), `"by_agent":true`) || !strings.Contains(string(ns[0].Body), `"by":"bo"`) {
		t.Errorf("told: %v %v", ns, err)
	}
	if p, err := l.ProposalByID(ctx, id); err != nil || len(p.Approvals) != 0 || string(p.Evidence) != string(now) {
		t.Errorf("kept: %+v %v", p, err)
	}
	steps, _, err := l.ProposalSteps(ctx, id, 10)
	if err != nil || len(steps) == 0 {
		t.Fatal(steps, err)
	}
	last := steps[len(steps)-1]
	if last.What != "proposal #"+itoa(id)+" backtested again" || last.By != "bo" || !strings.Contains(last.After, `"approvals_taken_back":1`) ||
		!strings.Contains(last.After, `"approvals_withdrawn":["ana"]`) || !strings.Contains(last.After, `"agent":true`) ||
		!strings.Contains(last.After, `"version":"v0.42.0"`) || last.Before != `{"version":""}` {
		t.Errorf("its step: %+v", last)
	}
	// Moved on: not.
	if _, err := l.Move(ctx, id, []string{StateProposed}, StateDropped, "ana", Step{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.BacktestAgain(ctx, id, "bo", false, now, was, nil, nil); !errors.Is(err, ErrMoved) {
		t.Errorf("dropped: %v", err)
	}
}

// TestProposalListEvidence: the proposals list reads, in brief, the
// evidence of an undo, of a new for, and of one waiting for approval, and
// no other's.
func TestProposalListEvidence(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	for _, c := range []struct {
		rule, kind, to string
	}{{"A", KindFor, StateApplied}, {"B", KindThreshold, ""}, {"C", KindThreshold, StateApplied}, {"D", KindSays, StateDropped}} {
		p := proposal(c.rule, c.kind)
		p.Evidence, p.AfterHash = []byte(`{"from":"`+c.rule+`"}`), "after-"+c.rule
		id, err := l.Propose(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if c.to != "" {
			if _, err := l.Move(ctx, id, []string{StateProposed}, c.to, "ana", Step{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	hs, err := l.ProposalList(ctx, ProposalPick{}, 10)
	if err != nil || len(hs) != 4 {
		t.Fatal(hs, err)
	}
	for _, h := range hs {
		want := map[string]bool{"A": true, "B": true}[h.Rule]
		if got := len(h.Evidence) > 0; got != want || h.AfterHash != "after-"+h.Rule {
			t.Errorf("%s (%s, %s): evidence %q, after %q", h.Rule, h.Kind, h.State, h.Evidence, h.AfterHash)
		}
	}
}

// TestBacktestAgainBounded: a proposal backtested again isn't again for
// BacktestCooldown, by anyone; a person backtests BacktestsAnHour again in
// an hour, and the agents acting for them as many apart, counted by the
// changes the ledger keeps; Lapilli's own aren't counted.
func TestBacktestAgainBounded(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	ev := []byte(`{"backtest":{}}`)
	again := func(rule, by string, agent bool) error {
		t.Helper()
		p := proposal(rule, KindFor)
		p.Evidence = ev
		id, err := l.Propose(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.MayBacktestAgain(ctx, id, by, agent); err != nil {
			return err
		}
		_, _, err = l.BacktestAgain(ctx, id, by, agent, ev, []byte(`{"backtest":{"semantics":1}}`), nil, nil)
		return err
	}
	for i := 0; i < BacktestsAnHour; i++ {
		if err := again(fmt.Sprint("P", i), "bo", false); err != nil {
			t.Fatalf("the %dth: %v", i+1, err)
		}
	}
	if err := again("Over", "BO", false); !errors.Is(err, ErrTooManyBacktests) {
		t.Errorf("one past the hour's: %v", err)
	}
	// Agents acting for bo: their own count, as many.
	for i := 0; i < BacktestsAnHour; i++ {
		if err := again(fmt.Sprint("A", i), "bo", true); err != nil {
			t.Fatalf("the agents' %dth: %v", i+1, err)
		}
	}
	if err := again("AgentOver", "bo", true); !errors.Is(err, ErrTooManyBacktests) {
		t.Errorf("the agents', one past: %v", err)
	}
	// Lapilli's own: not counted.
	if err := again("Own", Proposer, false); err != nil {
		t.Errorf("Lapilli's own: %v", err)
	}
	// One proposal, again within the cooldown: no one, Lapilli neither.
	p := proposal("Twice", KindFor)
	p.Evidence = ev
	id, err := l.Propose(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	now := []byte(`{"backtest":{"semantics":1}}`)
	if _, _, err := l.BacktestAgain(ctx, id, "cy", false, ev, now, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"cy", "dee", Proposer} {
		if err := l.MayBacktestAgain(ctx, id, who, false); !errors.Is(err, ErrBacktestedJustNow) {
			t.Errorf("%s, within the cooldown: %v", who, err)
		}
		if _, _, err := l.BacktestAgain(ctx, id, who, false, now, ev, nil, nil); !errors.Is(err, ErrBacktestedJustNow) {
			t.Errorf("%s, written within the cooldown: %v", who, err)
		}
	}
}

// TestApproveAsShown: an approval of a proposal as it was shown is refused
// once it moved since, backtested again meanwhile: in the write itself,
// with nothing kept.
func TestApproveAsShown(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	p := proposal("HighErrors", KindFor)
	p.Evidence = []byte(`{"backtest":{}}`)
	id, err := l.Propose(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	shown, err := l.ProposalByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.BacktestAgain(ctx, id, "bo", false, shown.Evidence, []byte(`{"backtest":{"semantics":1}}`), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Approve(ctx, id, "ana", 1, shown.Updated); !errors.Is(err, ErrMoved) {
		t.Errorf("approved as it was before it was backtested again: %v", err)
	}
	if p, _ := l.ProposalByID(ctx, id); len(p.Approvals) != 0 || p.State != StateProposed {
		t.Errorf("kept: %+v", p)
	}
	now, _ := l.ProposalByID(ctx, id)
	if p, err := l.Approve(ctx, id, "ana", 1, now.Updated); err != nil || p.State != StateApproved {
		t.Errorf("approved as it is: %+v %v", p, err)
	}
}
