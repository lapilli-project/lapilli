package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Past DraftsKept, making room for a draft lets go the oldest decided,
// but never one accepted whose pull request is ready for a recipe or
// opened, as KeepAddedDraft lets none go: its recipe would find it gone,
// and its merge untold. With only those, and open ones, none is kept.
func TestKeepDraftKeepsPullRequests(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	was := DraftsKept
	DraftsKept = 4
	defer func() { DraftsKept = was }()
	doc, hash := "expectations: []\n", strings.Repeat("c", 64)
	keep := func(i int) int64 {
		t.Helper()
		d := draftOf(fmt.Sprintf("R%d", i))
		d.MadeBy = fmt.Sprintf("p%d", i)
		id, err := l.KeepDraft(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// The oldest two accepted by the git path: one ready, one opened.
	ready, opened := keep(1), keep(2)
	for _, id := range []int64{ready, opened} {
		if _, err := l.AcceptDraft(ctx, id, "bo", doc, hash); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.MoveDraftPR(ctx, opened, []string{DraftReady}, DraftOpened, "https://git.example/pr/1", ""); err != nil {
		t.Fatal(err)
	}
	// Then one dropped, and one accepted without a pull request.
	dropped, plain := keep(3), keep(4)
	if _, err := l.DropDraft(ctx, dropped, "bo"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.AcceptDraft(ctx, plain, "bo", "", ""); err != nil {
		t.Fatal(err)
	}
	// Two more: the dropped and the plain go, not the older two.
	keep(5)
	keep(6)
	for _, id := range []int64{ready, opened} {
		if d, err := l.DraftByID(ctx, id); err != nil || d.PR == "" {
			t.Errorf("draft %d, its pull request waiting, let go: %+v %v", id, d, err)
		}
	}
	for _, id := range []int64{dropped, plain} {
		if _, err := l.DraftByID(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("draft %d kept: %v", id, err)
		}
	}
	// Two waiting for their pull requests, two open: no room.
	d := draftOf("R7")
	d.MadeBy = "p7"
	if _, err := l.KeepDraft(ctx, d); !errors.Is(err, ErrDraftsWaiting) {
		t.Errorf("kept past them: %v", err)
	}
	// Merged, the pull request no longer waits: its draft may go.
	if _, err := l.MoveDraftPR(ctx, opened, []string{DraftOpened}, DraftMerged, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := l.KeepDraft(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := l.DraftByID(ctx, opened); !errors.Is(err, ErrNotFound) {
		t.Errorf("a merged draft kept: %v", err)
	}
}
