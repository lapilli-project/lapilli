package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// keepOf gives a rule's name a note, tasks (one done), a comment and a
// proposal, as the UI keeps them.
func keepOf(t *testing.T, l *Ledger, rule string, note Note) {
	t.Helper()
	ctx := context.Background()
	note.Rule = rule
	if err := l.PutNote(ctx, note, "ana"); err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{"open", "done"} {
		id, err := l.PutTask(ctx, Task{Title: "tune " + rule, Rule: rule}, "ana")
		if err != nil {
			t.Fatal(err)
		}
		if st == "done" {
			if _, err := l.PatchTask(ctx, id, []byte(`{"status":"done"}`), "ana"); err != nil {
				t.Fatal(err)
			}
		}
	}
	// As the UI keeps one on a rule replayed then, gone now.
	if _, err := l.w.ExecContext(ctx, "INSERT INTO comments (on_kind, on_key, body, created, created_by, updated) VALUES ('rule', ?, 'noisy at night', 1, 'ana', 1)", rule); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Propose(ctx, proposal(rule, KindFor)); err != nil {
		t.Fatal(err)
	}
}

// What the ledger keeps of each rule's name is read name by name, in
// order, and counted for those asked; moved to another name, as for a rule
// renamed, its note fills what the other's leaves empty, its proposals
// stay; deleted, its proposals stay too. Each is noted in the activity log.
func TestRuleKept(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	keepOf(t, l, "OldErrors", Note{Status: "tune", Runbook: "https://runbooks.example/old", Note: "the old one"})
	keepOf(t, l, "Gone", Note{Note: "gone"})
	if err := l.PutNote(ctx, Note{Rule: "NewErrors", Note: "the new one"}, "ana"); err != nil {
		t.Fatal(err)
	}
	var names []string
	if err := l.KeptNames(ctx, func(n string) bool { names = append(names, n); return true }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"Gone", "NewErrors", "OldErrors"}) {
		t.Errorf("names kept: %q", names)
	}
	// Stopped when asked.
	n := 0
	l.KeptNames(ctx, func(string) bool { n++; return false })
	if n != 1 {
		t.Errorf("read %d names after stopping", n)
	}
	kept, err := l.KeptOf(ctx, []string{"OldErrors", "NewErrors", "Nothing"})
	if err != nil {
		t.Fatal(err)
	}
	want := []RuleKept{{Rule: "OldErrors", Note: true, Tasks: 2, OpenTasks: 1, Comments: 1, Proposals: []int64{1}}, {Rule: "NewErrors", Note: true}, {Rule: "Nothing"}}
	if fmt.Sprint(kept) != fmt.Sprint(want) {
		t.Errorf("kept:\n got %+v\nwant %+v", kept, want)
	}

	if err := l.MoveRuleKept(ctx, "OldErrors", "OldErrors", "ana"); !errors.Is(err, ErrSameRule) {
		t.Errorf("moved to itself: %v", err)
	}
	if err := l.MoveRuleKept(ctx, "OldErrors", "NewErrors", "ana"); err != nil {
		t.Fatal(err)
	}
	kept, _ = l.KeptOf(ctx, []string{"OldErrors", "NewErrors"})
	want = []RuleKept{{Rule: "OldErrors", Proposals: []int64{1}}, {Rule: "NewErrors", Note: true, Tasks: 2, OpenTasks: 1, Comments: 1}}
	if fmt.Sprint(kept) != fmt.Sprint(want) {
		t.Errorf("kept after moving:\n got %+v\nwant %+v", kept, want)
	}
	notes, _ := l.Notes(ctx)
	if nn := notes["NewErrors"]; nn.Note != "the new one" || nn.Status != "tune" || nn.Runbook != "https://runbooks.example/old" {
		t.Errorf("the note moved onto another: %+v", nn)
	}
	if _, ok := notes["OldErrors"]; ok {
		t.Errorf("the note moved is still there")
	}

	if err := l.DeleteRuleKept(ctx, "Gone", "ana"); err != nil {
		t.Fatal(err)
	}
	kept, _ = l.KeptOf(ctx, []string{"Gone"})
	if fmt.Sprint(kept) != fmt.Sprint([]RuleKept{{Rule: "Gone", Proposals: []int64{2}}}) {
		t.Errorf("kept after deleting: %+v", kept)
	}
	logged := changesBy(t, l, "ana")
	for _, w := range []string{"what rule OldErrors kept moved to NewErrors", "what rule Gone kept deleted", "task 3 deleted", "task 4 deleted"} {
		if !slices.Contains(logged, w) {
			t.Errorf("the activity log hasn't %q: %q", w, logged)
		}
	}
	// A team's editor moves nothing: who may is an editor of every team.
	if err := l.MoveRuleKept(WithTeams(ctx, []int64{1}), "NewErrors", "Other", "ed"); !errors.Is(err, ErrNotYourTeam) {
		t.Errorf("a team's editor moving: %v", err)
	}
}

// Thousands of names are read one at a time: what is held at once is the
// one read, however many there are.
func TestKeptNamesScale(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	const n = 3000
	err := l.write(ctx, func(t *txn) error {
		for i := range n {
			if _, err := t.ExecContext(t.ctx, "INSERT INTO comments (on_kind, on_key, body, created, updated) VALUES ('rule', ?, 'x', 1, 1)", fmt.Sprintf("Rule%05d", i)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	seen, last, ordered := 0, "", true
	if err := l.KeptNames(ctx, func(name string) bool {
		ordered = ordered && name > last
		seen, last = seen+1, name
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if seen != n || !ordered {
		t.Errorf("read %d names, in order %v", seen, ordered)
	}
}

// The proposals waiting for approval made before a time are those expiry
// closes: not those approved, nor those made since; the earliest first.
func TestWaitingSince(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	var ids []int64
	for _, rule := range []string{"A", "B", "C"} {
		id, err := l.Propose(ctx, proposal(rule, KindFor))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for i, at := range []int64{100, 50, 200} {
		if _, err := l.w.ExecContext(ctx, "UPDATE proposals SET made = ? WHERE id = ?", at, ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.Move(ctx, ids[2], []string{StateProposed}, StateApproved, "ana", Step{}); err != nil {
		t.Fatal(err)
	}
	got, err := l.WaitingSince(ctx, time.Unix(150, 0), 10)
	if err != nil || !slices.Equal(got, []int64{ids[1], ids[0]}) {
		t.Errorf("waiting since: %v %v", got, err)
	}
	if got, _ := l.WaitingSince(ctx, time.Unix(150, 0), 1); !slices.Equal(got, []int64{ids[1]}) {
		t.Errorf("waiting since, one at most: %v", got)
	}
	// Expired, a proposal is finished with: another for its rule may be
	// made.
	if _, err := l.Move(ctx, ids[0], []string{StateProposed}, StateExpired, Proposer, Step{Reason: "waited"}); err != nil {
		t.Fatal(err)
	}
	if err := l.MayPropose(ctx, "A", "ana"); err != nil {
		t.Errorf("a rule whose proposal expired: %v", err)
	}
}

// A name moved or deleted is one a note could be on: 500 bytes at most,
// nothing in it that doesn't show; and one the ledger keeps something of.
// Nothing is noted otherwise.
func TestRuleKeptNames(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	keepOf(t, l, "Kept", Note{Note: "n"})
	before := len(changesBy(t, l, "ana"))
	long := strings.Repeat("x", 501)
	for name, err := range map[string]error{
		"move a long name":   l.MoveRuleKept(ctx, long, "Kept", "ana"),
		"move a line break":  l.MoveRuleKept(ctx, "a\nb", "Kept", "ana"),
		"delete a long name": l.DeleteRuleKept(ctx, long, "ana"),
		"delete a bidi mark": l.DeleteRuleKept(ctx, "a‮b", "ana"),
	} {
		if !invalidErr(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := l.MoveRuleKept(ctx, "Nothing", "Kept", "ana"); !errors.Is(err, ErrNotFound) {
		t.Errorf("moving what nothing is kept of: %v", err)
	}
	if err := l.DeleteRuleKept(ctx, "Nothing", "ana"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting what nothing is kept of: %v", err)
	}
	if after := len(changesBy(t, l, "ana")); after != before {
		t.Errorf("%d changes noted", after-before)
	}
}
