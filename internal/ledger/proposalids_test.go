package ledger

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

// TestProposalsByID: the proposals list picks the proposals named by id
// alone, with the rules and the facets as before, and counts them alone;
// none named is every one.
func TestProposalsByID(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	var ids []int64
	for i := range 12 {
		id, err := l.Propose(ctx, Proposal{Rule: fmt.Sprintf("R%02d", i), Kind: []string{KindFor, KindDisable}[i%2], Path: PathGit, MadeBy: fmt.Sprint("p", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := l.Move(ctx, ids[3], []string{StateProposed}, StateDropped, "bo", Step{}); err != nil {
		t.Fatal(err)
	}
	named := []int64{ids[1], ids[3], ids[4], ids[9]}
	heads := func(p ProposalPick) []int64 {
		t.Helper()
		got, err := l.ProposalList(ctx, p, 100)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, h := range got {
			out = append(out, h.ID)
		}
		slices.Sort(out)
		return out
	}
	if got := heads(ProposalPick{IDs: named}); !slices.Equal(got, named) {
		t.Errorf("by id: %v, want %v", got, named)
	}
	if got := heads(ProposalPick{IDs: named, Kinds: []string{KindFor}}); !slices.Equal(got, []int64{ids[4]}) {
		t.Errorf("by id and kind: %v", got)
	}
	if got := heads(ProposalPick{IDs: named, Rules: []string{"R01", "R09", "R10"}}); !slices.Equal(got, []int64{ids[1], ids[9]}) {
		t.Errorf("by id and rules: %v", got)
	}
	if got := heads(ProposalPick{}); len(got) != len(ids) {
		t.Errorf("none named: %d", len(got))
	}
	counts, err := l.ProposalCountsOf(ctx, []string{"R01", "R03", "R04", "R05"}, named)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]int{}
	for _, c := range counts {
		by[c.Kind+"/"+c.State] += c.N
	}
	if want := map[string]int{KindDisable + "/" + StateProposed: 1, KindDisable + "/" + StateDropped: 1, KindFor + "/" + StateProposed: 1}; fmt.Sprint(by) != fmt.Sprint(want) {
		t.Errorf("counted %v, want %v", by, want)
	}
	all, err := l.ProposalCountsOf(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range all {
		n += c.N
	}
	if n != len(ids) {
		t.Errorf("none named counts %d", n)
	}
}
