package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// draftOf is an agent's draft of a rule's budget, for the person ana.
func draftOf(rule string) ExpectDraft {
	return ExpectDraft{Rule: rule, Origin: DraftAgent, MadeBy: "Ana", Agent: true, Text: "expectations:\n  - rule: " + rule + "\n    budget:\n      week: 5\n",
		Reason: "it pages <b>too</b> often", Found: json.RawMessage(`[{"what":"budget week","state":"broken"}]`), Target: DraftToFile, DocHash: strings.Repeat("a", 64)}
}

// A draft is kept open, told, and read back; whoever proposed it, or the
// person whose agent did, can't accept it, in any letter case; another
// person can, once; and dropping is anyone's who may drop, its proposer's
// too. In SQLite or, with LAPILLI_TEST_POSTGRES, in Postgres. A rule's
// name longer than a btree holds is kept and found again.
func TestExpectDrafts(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	l.TellTo([]string{"webhook"})
	defer l.TellTo(nil)
	d := draftOf("Restarts")
	d.Tests = map[string]string{"tests/restarts.yaml": "tests: []\n"}
	id, err := l.KeepDraft(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.DraftByID(ctx, id)
	if err != nil || got.State != DraftOpen || got.MadeBy != "ana" || !got.Agent || got.Text != d.Text || got.Tests["tests/restarts.yaml"] != "tests: []\n" ||
		got.Reason != d.Reason || string(got.Found) != string(d.Found) || got.Made.IsZero() {
		t.Fatalf("%+v %v", got, err)
	}
	for _, own := range []string{"ana", "ANA", "Ana"} {
		if _, err := l.AcceptDraft(ctx, id, own, "", ""); !errors.Is(err, ErrOwnDraft) {
			t.Errorf("accepted by %s, who proposed it: %v", own, err)
		}
	}
	a, err := l.AcceptDraft(ctx, id, "Bo", "", "")
	if err != nil || a.State != DraftAccepted || a.DecidedBy != "bo" || a.Decided.IsZero() || a.PR != "" {
		t.Fatalf("accepted: %+v %v", a, err)
	}
	for _, again := range []func() error{
		func() error { _, err := l.AcceptDraft(ctx, id, "cy", "", ""); return err },
		func() error { _, err := l.DropDraft(ctx, id, "cy"); return err },
	} {
		if err := again(); !errors.Is(err, ErrMoved) {
			t.Errorf("decided already: %v", err)
		}
	}
	long := strings.Repeat("ルール", 1200) // 10,800 bytes
	id2, err := l.KeepDraft(ctx, draftOf(long))
	if err != nil {
		t.Fatal(err)
	}
	if dr, err := l.DropDraft(ctx, id2, "ana"); err != nil || dr.State != DraftDropped || dr.DecidedBy != "ana" {
		t.Errorf("dropped by its proposer: %+v %v", dr, err)
	}
	if ds, err := l.Drafts(ctx, DraftQuery{Rule: long, HasRule: true}); err != nil || len(ds) != 1 || ds[0].Rule != long {
		t.Errorf("a long name's: %+v %v", ds, err)
	}
	if ds, err := l.Drafts(ctx, DraftQuery{States: []string{DraftAccepted}}); err != nil || len(ds) != 1 || ds[0].ID != id {
		t.Errorf("accepted: %+v %v", ds, err)
	}
	if _, err := l.DraftByID(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("none: %v", err)
	}
	var events []string
	ns, err := l.Due(ctx, "webhook", time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range ns {
		events = append(events, n.Event)
		// What a notice says of a draft is never its text or its reason.
		if strings.Contains(string(n.Body), "often") || strings.Contains(string(n.Body), "budget") {
			t.Errorf("%s tells the proposer's words: %s", n.Event, n.Body)
		}
	}
	if fmt.Sprint(events) != "[expectation.drafted expectation.accepted expectation.drafted expectation.dropped]" {
		t.Errorf("told: %v", events)
	}
}

// A draft is bounded, and so are those kept: its text, test files and
// reason; ten open a rule; and past DraftsKept, the oldest decided let go,
// none kept while as many are open.
func TestExpectDraftsBounded(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	big := func(f func(*ExpectDraft)) ExpectDraft {
		d := draftOf("R")
		f(&d)
		return d
	}
	for name, d := range map[string]ExpectDraft{
		"text":    big(func(d *ExpectDraft) { d.Text = strings.Repeat("x", MaxDraftText+1) }),
		"no text": big(func(d *ExpectDraft) { d.Text = "" }),
		"tests":   big(func(d *ExpectDraft) { d.Tests = map[string]string{"t.yaml": strings.Repeat("x", MaxDraftTests)} }),
		"tests n": big(func(d *ExpectDraft) {
			d.Tests = map[string]string{}
			for i := range MaxDraftTestsN + 1 {
				d.Tests[fmt.Sprint(i)] = ""
			}
		}),
		"reason":   big(func(d *ExpectDraft) { d.Reason = strings.Repeat("x", MaxDraftReason+1) }),
		"found":    big(func(d *ExpectDraft) { d.Found = json.RawMessage(`"` + strings.Repeat("x", MaxDraftFound) + `"`) }),
		"origin":   big(func(d *ExpectDraft) { d.Origin = "model" }),
		"target":   big(func(d *ExpectDraft) { d.Target = "cluster" }),
		"rule":     big(func(d *ExpectDraft) { d.Rule = "a‮b" }),
		"made by":  big(func(d *ExpectDraft) { d.MadeBy = "" }),
		"doc hash": big(func(d *ExpectDraft) { d.DocHash = strings.Repeat("a", 65) }),
	} {
		if _, err := l.KeepDraft(ctx, d); !errors.As(err, new(ErrInvalid)) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	if _, err := l.AcceptDraft(ctx, 1, "bo", strings.Repeat("x", MaxDraftDocument+1), strings.Repeat("b", 64)); !errors.As(err, new(ErrInvalid)) {
		t.Errorf("a document too big: %v", err)
	}
	// Ten open a rule, each by another person, as the hour's limit is a
	// person's.
	for i := range DraftsOpenARule {
		d := draftOf("R")
		d.MadeBy = fmt.Sprintf("p%d", i)
		if _, err := l.KeepDraft(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.KeepDraft(ctx, draftOf("R")); !errors.Is(err, ErrDraftsWaiting) {
		t.Errorf("an eleventh open: %v", err)
	}
	// Kept in all: past it, the oldest decided go; all open, none is kept.
	was := DraftsKept
	DraftsKept = 11
	defer func() { DraftsKept = was }()
	if _, err := l.DropDraft(ctx, 1, "bo"); err != nil {
		t.Fatal(err)
	}
	for i, rule := range []string{"S", "T"} {
		d := draftOf(rule)
		d.MadeBy = fmt.Sprintf("q%d", i)
		if _, err := l.KeepDraft(ctx, d); err != nil {
			t.Fatalf("%s: %v", rule, err)
		}
	}
	if _, err := l.DraftByID(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("the oldest decided wasn't let go: %v", err)
	}
	d := draftOf("U")
	d.MadeBy = "q9"
	if _, err := l.KeepDraft(ctx, d); !errors.Is(err, ErrDraftsWaiting) {
		t.Errorf("all open, one more kept: %v", err)
	}
	if ds, err := l.Drafts(ctx, DraftQuery{}); err != nil || len(ds) != 11 {
		t.Errorf("kept: %d %v", len(ds), err)
	}
}

// Drafts and proposals are one limit an hour, a person's with their
// agents': ProposalsAnHour of both together.
func TestDraftsCountWithProposals(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	for i := range ProposalsAnHour - 1 {
		p := proposal(fmt.Sprintf("P%d", i), KindFor)
		p.MadeBy = "ana"
		if _, err := l.Propose(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.MayDraft(ctx, "ANA"); err != nil {
		t.Fatalf("one left: %v", err)
	}
	if _, err := l.KeepDraft(ctx, draftOf("D")); err != nil {
		t.Fatal(err)
	}
	if err := l.MayDraft(ctx, "ana"); !errors.Is(err, ErrTooManyProposals) {
		t.Errorf("may draft past the hour's: %v", err)
	}
	if _, err := l.KeepDraft(ctx, draftOf("E")); !errors.Is(err, ErrTooManyProposals) {
		t.Errorf("a draft past the hour's: %v", err)
	}
	p := proposal("Q", KindFor)
	p.MadeBy = "Ana"
	if _, err := l.Propose(ctx, p); !errors.Is(err, ErrTooManyProposals) {
		t.Errorf("a proposal past the hour's, a draft among them: %v", err)
	}
	if err := l.MayPropose(ctx, "Q", "ana"); !errors.Is(err, ErrTooManyProposals) {
		t.Errorf("may propose past the hour's: %v", err)
	}
	// Another person's are theirs.
	other := draftOf("E")
	other.MadeBy = "bo"
	if _, err := l.KeepDraft(ctx, other); err != nil {
		t.Errorf("another person's: %v", err)
	}
}

// An accepted draft by the git path is ready for a recipe, its file kept
// until it is handed out; its pull request moves on only from where it
// is.
func TestDraftPullRequest(t *testing.T) {
	ctx := context.Background()
	l := open(t, time.UTC)
	id, err := l.KeepDraft(ctx, draftOf("Restarts"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.MoveDraftPR(ctx, id, []string{DraftReady}, DraftOpened, "https://git.example/pr/1", ""); !errors.Is(err, ErrMoved) {
		t.Errorf("opened while open: %v", err)
	}
	d, err := l.AcceptDraft(ctx, id, "bo", "expectations: []\n", strings.Repeat("b", 64))
	if err != nil || d.PR != DraftReady || d.Document == "" {
		t.Fatalf("%+v %v", d, err)
	}
	if ds, err := l.Drafts(ctx, DraftQuery{PRs: []string{DraftReady, DraftOpened}}); err != nil || len(ds) != 1 || ds[0].Document != "expectations: []\n" {
		t.Fatalf("ready: %+v %v", ds, err)
	}
	if _, err := l.MoveDraftPR(ctx, id, []string{DraftOpened}, DraftMerged, "", ""); !errors.Is(err, ErrMoved) {
		t.Errorf("merged before opened: %v", err)
	}
	d, err = l.MoveDraftPR(ctx, id, []string{DraftReady}, DraftOpened, "https://git.example/pr/1", "")
	if err != nil || d.PR != DraftOpened || d.Link != "https://git.example/pr/1" || d.Document != "" {
		t.Fatalf("opened: %+v %v", d, err)
	}
	if d, err = l.MoveDraftPR(ctx, id, []string{DraftOpened}, DraftMerged, "", ""); err != nil || d.PR != DraftMerged || d.Link == "" {
		t.Errorf("merged: %+v %v", d, err)
	}
}

// A draft's queries read along an index, in either database: a rule's,
// a person's last hour, a state's, a pull request's.
func TestDraftQueriesIndexed(t *testing.T) {
	l := open(t, time.UTC)
	for _, c := range []struct {
		q    string
		args []any
	}{
		{"SELECT " + draftCols + " FROM expectation_drafts WHERE " + l.d.pick("rule = ?1", "md5(rule) = md5(?1) AND rule = ?1") + " ORDER BY id DESC LIMIT 20", []any{"Restarts"}},
		{"SELECT count(*) FROM expectation_drafts WHERE made_by = ?1 AND made > ?2", []any{"ana", int64(1)}},
		{"SELECT " + draftCols + " FROM expectation_drafts WHERE state IN (?1) ORDER BY id DESC LIMIT 50", []any{DraftOpen}},
		{"SELECT " + draftCols + " FROM expectation_drafts WHERE pr IN (?1, ?2) ORDER BY id DESC LIMIT 50", []any{DraftReady, DraftOpened}},
	} {
		if p := plan(t, l, c.q, c.args...); scansAll(l, p, "expectation_drafts") {
			t.Errorf("%s reads expectation_drafts whole: %s", c.q, p)
		}
	}
}
