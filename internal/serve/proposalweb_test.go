package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
)

// A proposal's page and the proposals list (docs/design/ui.md, phase 4):
// each test here is a behaviour.

// withProposal is a server with changes on and a proposal of a new for for
// Errors, made by bo.
func withProposal(t *testing.T, more func(*Config)) (*Server, http.Handler, int64) {
	t.Helper()
	s, _ := changesServer(t, more)
	h := s.Handler()
	return s, h, proposeFor(t, h, "15m")
}

// TestProposalPage: a proposal's page has its id and what it changes in
// words, its state as a sign and a word, who made it, its way and when;
// the breadcrumb Fix › Proposals › #id; the change as a diff in the
// options asked, the file a pull request changes beside it; and the
// backtest as a table, now beside as proposed.
func TestProposalPage(t *testing.T) {
	_, h, id := withProposal(t, nil)
	path := "/proposals/" + itoa(id)
	_, body, _ := ui(t, h, http.MethodGet, path, cy, "", nil)
	for _, want := range []string{
		`<h1>#` + itoa(id) + ` <bdi>Errors</bdi>&#39;s for, <bdi>2m</bdi> → <bdi>15m</bdi></h1>`,
		`<title>#` + itoa(id) + ` Errors&#39;s for, 2m → 15m · Lapilli</title>`,
		`<li>Fix</li><li><a href="/proposals">Proposals</a></li><li><span aria-current="page"><bdi>#` + itoa(id) + `</bdi></span></li>`,
		`<span class="badge st-wait"><span aria-hidden="true">◷</span> awaiting approval</span>`,
		`made by bo@example.com`, `a pull request`,
		// The diff, unified, its lines alone.
		`<pre class="diff"><span class="same"> Errors (shop)</span>`, `<span class="del">-for: 2m</span>` + "\n" + `<span class="ins">&#43;for: 15m</span>`,
		`<a href="` + path + `?diff=split#change">Side by side</a>`, `<a href="` + path + `?context=all#change">Every line</a>`,
		// The file the pull request changes.
		`The file as the pull request changes it: <bdi>shop.yaml</bdi>`, `<span class="ins">&#43;        for: 15m</span>`,
		// The backtest: fired 21 → 1, caught 1 → 1.
		`<th scope="row">Fired</th><td class="n">21</td><td class="n">1</td><td class="n"> <span class="delta better">−20 better</span></td>`,
		`<th scope="row">Caught something</th><td class="n">1</td><td class="n">1</td><td class="n"> <span class="delta">±0</span></td>`,
		`<th scope="row">Incidents missed that the other catches</th><td class="n">0</td><td class="n">0</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s", want)
		}
	}
	if strings.Contains(body, `class="warn"`) {
		t.Error("a warning, missing nothing more")
	}
	// Side by side: a table, headed now and as proposed; every line.
	_, body, _ = ui(t, h, http.MethodGet, path+"?diff=split&context=all", cy, "", nil)
	if !strings.Contains(body, `<table class="diff-split">`+"\n"+`<thead><tr><th scope="col">Now</th><th scope="col">As proposed</th></tr></thead>`) ||
		!strings.Contains(body, `<tr><td class="del">-for: 2m</td><td class="ins">&#43;for: 15m</td></tr>`) || strings.Contains(body, `<pre class="diff"><span class="same"> Errors`) {
		t.Error("side by side isn't a table of the two")
	}
	if !strings.Contains(body, `<a href="`+path+`?context=all&amp;diff=split#change" aria-current="true">Side by side</a>`) {
		t.Error("side by side isn't the option shown")
	}
	// What else was asked is the default, and never said back.
	_, body, _ = ui(t, h, http.MethodGet, path+"?diff=%3Cscript%3E&context=12", cy, "", nil)
	if strings.Contains(body, "diff-split") || strings.Contains(body, "%3Cscript") || strings.Contains(body, "<script") || strings.Contains(body, "context=12") {
		t.Error("an unknown option isn't the default")
	}
}

// TestProposalMissesMore: a backtest that misses an incident the rule
// catches now has a warning above its table, naming it.
func TestProposalMissesMore(t *testing.T) {
	s, h, id := withProposal(t, nil)
	p := state(t, s, id)
	ev := proposalEvidence(p)
	ev.Backtest.Lost, ev.Backtest.After.White = []string{"INC-1"}, 0
	b, _ := json.Marshal(ev)
	if _, err := s.l.Move(context.Background(), id, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{}); err != nil {
		t.Fatal(err)
	}
	p.Evidence, p.ID = b, 0
	again, err := s.l.Propose(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	_, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(again), cy, "", nil)
	warn := strings.Index(body, `<p class="warn">`)
	table := strings.Index(body, `<table class="backtest">`)
	if warn < 0 || table < warn || !strings.Contains(body[warn:table], `As proposed, it misses 1 incident it catches now: <a href="/incidents/INC-1"><bdi>INC-1</bdi></a>.`) {
		t.Errorf("no warning above the table:\n%s", body[max(0, warn):max(0, table)])
	}
	if !strings.Contains(body, `<th scope="row">Incidents missed that the other catches</th><td class="n">0</td><td class="n">1</td><td class="n"> <span class="delta worse">&#43;1 worse</span></td>`) {
		t.Error("the table doesn't say one more is missed")
	}
}

// approveForm is the approval form a proposal's page gives someone.
var approveForm = regexp.MustCompile(`(?s)<form method="post" action="/proposals/\d+/approve">.*?</form>`)

// TestApproveReadFirst: approving takes a box ticked, that the approver
// read the backtest: required on the page, and checked again when the
// approval comes, refused with why without it.
func TestApproveReadFirst(t *testing.T) {
	s, h, id := withProposal(t, nil)
	_, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), cy, "", nil)
	f := approveForm.FindString(body)
	if !strings.Contains(f, `<label><input type="checkbox" name="read" value="1" required> I have read the backtest</label>`) {
		t.Errorf("no box to tick, required:\n%s", f)
	}
	key := keyFor(t, h, id, cy, "approve")
	for _, read := range []string{"", "0", "yes"} {
		code, body, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/approve", cy, "same-origin", url.Values{"key": {key}, "read": {read}})
		if code != http.StatusBadRequest || !strings.Contains(body, "tick the box on its page that says you have") {
			t.Errorf("read=%q: %d", read, code)
		}
	}
	if p := state(t, s, id); p.State != ledger.StateProposed || len(p.Approvals) != 0 {
		t.Fatalf("approved without the box: %s, %d", p.State, len(p.Approvals))
	}
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/approve", cy, "same-origin", url.Values{"key": {key}, "read": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("with the box: %d", code)
	}
	if p := state(t, s, id); p.State != ledger.StateApproved {
		t.Fatalf("with the box: %s", p.State)
	}
}

// offButton is a disabled button, with why, as the page draws it.
func offButton(label, id, why string) string {
	return `<button type="button" disabled aria-describedby="` + id + `">` + label + `</button> <span id="` + id + `" class="why">` + why + `</span>`
}

// TestApproveDisabledWithWhy: a reader who may not approve sees the button
// disabled, with why: an editor, not an approver; whoever made it; one
// who approved it already; no one signed in; changes off; a change for a
// person to make.
func TestApproveDisabledWithWhy(t *testing.T) {
	s, h, id := withProposal(t, func(c *Config) { c.Approvals = 2 })
	approve(t, h, id, cy)
	path := "/proposals/" + itoa(id)
	for who, why := range map[string]string{
		bo: "Approvers only", cy: "You approved it: it waits for another approver.", "": "Read-only: no one is signed in",
	} {
		_, body, _ := ui(t, h, http.MethodGet, path, who, "", nil)
		if !strings.Contains(body, offButton("Approve", "approve-why", why)) || strings.Contains(body, `action="`+path+`/approve"`) {
			t.Errorf("%q: not disabled with %q", who, why)
		}
	}
	// One who may approves: no button disabled.
	if _, body, _ := ui(t, h, http.MethodGet, path, ana, "", nil); strings.Contains(body, `aria-describedby="approve-why"`) || !strings.Contains(body, "Approve (2 of 2)") {
		t.Error("ana may approve it")
	}
	// A change for a person to make is approved by no one.
	p := state(t, s, id)
	s.l.Move(context.Background(), id, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{})
	p.ID, p.Path, p.Document, p.DocHash = 0, ledger.PathNone, "", ""
	none, err := s.l.Propose(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(none), ana, "", nil); !strings.Contains(body,
		offButton("Approve", "approve-why", "For a person to make where the rule is kept: nothing to approve here")) {
		t.Error("a change for a person: not disabled with why")
	}
	// An approver's own proposal.
	s2, _ := changesServer(t, nil)
	h2 := s2.Handler()
	own, err := s2.propose(context.Background(), ask{Rule: "Errors", Kind: ledger.KindFor, For: 15 * time.Minute}, ana, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, body, _ := ui(t, h2, http.MethodGet, "/proposals/"+itoa(own), ana, "", nil); !strings.Contains(body, offButton("Approve", "approve-why", "You proposed this: someone else approves it.")) {
		t.Error("one's own: not disabled with why")
	}
	// Changes off: no one approves.
	s3, _ := changesServer(t, func(c *Config) { c.Changes = false })
	h3 := s3.Handler()
	off := proposeFor(t, h3, "15m")
	if _, body, _ := ui(t, h3, http.MethodGet, "/proposals/"+itoa(off), ana, "", nil); !strings.Contains(body, offButton("Approve", "approve-why", "Changes are off: lapilli serve was started without --changes")) {
		t.Error("changes off: not disabled with why")
	}
}

// TestProposalHistory: what happened to a proposal, in order, each with
// who and when: made, approved, carried out and handed to a recipe, its
// pull request opened (with its link), merged, applied; an undo asked for,
// linked to the proposal it is; and the proposal undone.
func TestProposalHistory(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, nil)
	h, port := s.Handler(), s.IncidentsHandler()
	id := proposeFor(t, h, "15m")
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	openThenMerge(t, port, id, true)
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(state(t, s, id).Document), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	path := "/proposals/" + itoa(id)
	_, body, _ := ui(t, h, http.MethodGet, path, ana, "", nil)
	// Undoing it is asked for here, as a proposal of its own.
	if !strings.Contains(body, "Undoing it is a change of its own: a new proposal, which you approve as you ask for it") ||
		!strings.Contains(body, `<button class="quiet-button">Ask to undo it</button>`) {
		t.Error("undoing it isn't offered as a proposal of its own")
	}
	code, _, hd := ui(t, h, http.MethodPost, path+"/undo", ana, "same-origin", url.Values{"key": {keyFor(t, h, id, ana, "undo")}, "read": {"1"}})
	if code != http.StatusSeeOther {
		t.Fatalf("undo: %d", code)
	}
	var uid int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &uid)
	u := state(t, s, uid)
	if u.Kind != ledger.KindUndo || u.State != ledger.StateApproved || u.Follows != id || len(u.Approvals) != 1 || u.Approvals[0].By != ana {
		t.Fatalf("the undo isn't a proposal approved by whoever asked: %+v", u)
	}
	s.jobs(ctx, false)
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(state(t, s, uid).Document), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	_, body, _ = ui(t, h, http.MethodGet, path, cy, "", nil)
	i := strings.Index(body, `<ol class="steps">`)
	j := strings.Index(body[max(i, 0):], "</ol>")
	if i < 0 || j < 0 {
		t.Fatal("no history")
	}
	var steps []string
	for _, li := range regexp.MustCompile(`(?s)<li><span class="t">[^<]*</span> (.*?)</li>`).FindAllStringSubmatch(body[i:i+j], -1) {
		steps = append(steps, regexp.MustCompile(`<[^>]+>`).ReplaceAllString(li[1], ""))
	}
	want := []string{"Made by bo@example.com", "Approved by ana@example.com: 1 of 1", "↻ being applied (checked again, waiting for a recipe) by Lapilli"}
	if len(steps) != 8 {
		t.Fatalf("steps: %q", steps)
	}
	for k, w := range want {
		if steps[k] != w {
			t.Errorf("step %d: %q, want %q", k, steps[k], w)
		}
	}
	for _, w := range []string{"↻ being applied (its pull request is open) by a change recipe Pull request ↗",
		"↻ being applied (merged, not yet read back) by a change recipe", "● applied by Lapilli", "ana@example.com asked to undo it, as #" + itoa(uid), "↶ undone by Lapilli"} {
		if !slices.Contains(steps, w) {
			t.Errorf("no step %q in %q", w, steps)
		}
	}
	if !strings.Contains(body[i:i+j], `<a href="https://git.example/pull/3" rel="noopener noreferrer">Pull request`) || !strings.Contains(body[i:i+j], `<a href="/proposals/`+itoa(uid)+`">#`+itoa(uid)+`</a>`) {
		t.Error("the pull request's link, or the undo's, isn't in the history")
	}
	// The undo's own page: approved by whoever asked, one of one.
	_, body, _ = ui(t, h, http.MethodGet, "/proposals/"+itoa(uid), cy, "", nil)
	if strings.Contains(body, "aren&#39;t shown") {
		t.Error("the undo's page says its older steps are gone")
	}
	if !strings.Contains(body, "An undo is approved by the approver who asks for it.") || !strings.Contains(body, "<p>1 of 1 approval <span class=\"quiet\">") ||
		!strings.Contains(body, `<th scope="col">As the change left it</th>`) && !strings.Contains(body, `<span class="ins">`) {
		t.Error("the undo's page doesn't say who approved it")
	}
}

// TestProposalStepsGone: a proposal whose making the ledger no longer
// keeps (--keep pruned its changes) says its older steps aren't shown;
// one whose making is kept, nothing of it.
func TestProposalStepsGone(t *testing.T) {
	s, h, id := withProposal(t, nil)
	path := "/proposals/" + itoa(id)
	if _, body, _ := ui(t, h, http.MethodGet, path, cy, "", nil); strings.Contains(body, "aren&#39;t shown") {
		t.Error("a proposal's whole history said to be cut")
	}
	ledgerExec(t, s, "DELETE FROM changes")
	s.cache.Lock()
	s.cache.gen = "" // the page worked out afresh: a write to the database isn't the ledger's
	s.cache.Unlock()
	if _, body, _ := ui(t, h, http.MethodGet, path, cy, "", nil); !strings.Contains(body, "Steps older than the changes the ledger keeps (--keep) aren&#39;t shown.") ||
		!strings.Contains(body, "Made by <bdi>bo@example.com</bdi>") {
		t.Error("a proposal whose changes were pruned doesn't say so, or its making")
	}
}

// TestProposalActionsDisabled: what a reader may not do on a proposal's
// page is a button, disabled, with why: asking for an applied change to be
// undone (an approver's), dropping one (an editor's or an approver's),
// closing one past Lapilli's hands (an admin's), making it a task (an
// editor's). None is a form to post.
func TestProposalActionsDisabled(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, func(c *Config) { c.Editors = []string{bo} })
	h, port := s.Handler(), s.IncidentsHandler()
	id := proposeFor(t, h, "15m")
	path := "/proposals/" + itoa(id)
	_, body, _ := ui(t, h, http.MethodGet, path, "dee@example.com", "", nil)
	for _, want := range []string{offButton("Drop it", "drop-why", "Editors and approvers only"), offButton("Make it a task", "task-why", "Editors only")} {
		if !strings.Contains(body, want) {
			t.Errorf("a viewer: no %s", want)
		}
	}
	if strings.Contains(body, path+"/drop") || strings.Contains(body, path+"/task") {
		t.Error("a viewer is given a form")
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	// Checked again and waiting for a recipe: an approver's or an admin's
	// to drop, not an editor's, as the handler has it.
	if _, body, _ := ui(t, h, http.MethodGet, path, bo, "", nil); !strings.Contains(body, offButton("Drop it", "drop-why", "Approvers and admins only")) || strings.Contains(body, path+"/drop") {
		t.Error("an editor: dropping one ready isn't disabled with why")
	}
	if code, _, _ := ui(t, h, http.MethodPost, path+"/drop", bo, "same-origin", url.Values{}); code < 400 || state(t, s, id).State != ledger.StateReady {
		t.Errorf("an editor dropped one ready: %d", code)
	}
	if _, body, _ := ui(t, h, http.MethodGet, path, cy, "", nil); !strings.Contains(body, `action="`+path+`/drop"`) {
		t.Error("an approver may drop one ready")
	}
	openThenMerge(t, port, id, false)
	if _, body, _ := ui(t, h, http.MethodGet, path, cy, "", nil); !strings.Contains(body, offButton("Close it", "drop-why", "Admins only")) || strings.Contains(body, "/drop") {
		t.Error("an approver, not an admin: closing it isn't disabled with why")
	}
	recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "merged"})
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(state(t, s, id).Document), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := ui(t, h, http.MethodGet, path, bo, "", nil); !strings.Contains(body, offButton("Ask to undo it", "undo-why", "Approvers only")) || strings.Contains(body, "/undo") {
		t.Error("an editor: undoing it isn't disabled with why")
	}
	if code, _, _ := ui(t, h, http.MethodPost, path+"/undo", bo, "same-origin", url.Values{"key": {s.formKey(state(t, s, id), bo, "undo")}}); code != http.StatusForbidden {
		t.Errorf("an editor undoing it: %d", code)
	}
	// Read-only, every one of them says so.
	s.cfg.ReadOnly = true
	if _, body, _ := ui(t, s.Handler(), http.MethodGet, path, ana, "", nil); !strings.Contains(body, offButton("Ask to undo it", "undo-why", "Read-only: lapilli serve was started with --read-only")) {
		t.Error("read-only: undoing it isn't disabled with why")
	}
}

// TestProposalSteps: a proposal's steps are its own, not another's whose
// number starts the same (#1, #12).
func TestProposalSteps(t *testing.T) {
	s, _ := server(t, nil)
	ctx := context.Background()
	var ids []int64
	for i := range 12 {
		id, err := s.l.Propose(ctx, ledger.Proposal{Rule: fmt.Sprint("R", i), Kind: ledger.KindFor, Path: ledger.PathNone, MadeBy: bo})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := s.l.Move(ctx, ids[0], []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id   int64
		want []string
	}{{ids[0], []string{"proposal #1 made", "proposal #1 dropped"}}, {ids[11], []string{"proposal #12 made"}}, {ids[9], []string{"proposal #10 made"}}} {
		cs, _, err := s.l.ProposalSteps(ctx, c.id, 10)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, x := range cs {
			got = append(got, x.What)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("#%d: %q, want %q", c.id, got, c.want)
		}
	}

}

// historyOf is a proposal's history as a page lists it to cy, a step a
// line, its tags taken out; the page worked out afresh.
func historyOf(t *testing.T, s *Server, id int64) []string {
	t.Helper()
	s.cache.Lock()
	s.cache.gen = "" // a write to the database isn't the ledger's
	s.cache.Unlock()
	_, body, _ := ui(t, s.Handler(), http.MethodGet, "/proposals/"+itoa(id), cy, "", nil)
	i := strings.Index(body, `<ol class="steps">`)
	j := strings.Index(body[max(i, 0):], "</ol>")
	if i < 0 || j < 0 {
		t.Fatal("no history")
	}
	var steps []string
	for _, li := range regexp.MustCompile(`(?s)<li><span class="t">[^<]*</span> (.*?)</li>`).FindAllStringSubmatch(body[i:i+j], -1) {
		text := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(strings.ReplaceAll(li[1], "<br>", " "), "")
		steps = append(steps, strings.Join(strings.Fields(text), " "))
	}
	return steps
}

// waitingDeletion is a deletion of Errors, approved by ana, that waited
// while its rule fired and was then handed to a recipe, its pull request
// opened and merged: each step as the ledger keeps it.
func waitingDeletion(t *testing.T) (*Server, int64) {
	t.Helper()
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	id, err := s.l.Propose(ctx, ledger.Proposal{Rule: "Errors", Kind: ledger.KindDelete, Path: ledger.PathGit, MadeBy: bo, Before: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.Approve(ctx, id, ana, 1); err != nil {
		t.Fatal(err)
	}
	if _, waits, err := s.waitsWhileFiring(ctx, state(t, s, id), true); err != nil || !waits {
		t.Fatalf("doesn't wait: %v", err)
	}
	for _, m := range []struct {
		from, to string
		step     ledger.Step
	}{
		{ledger.StateApproved, ledger.StateReady, ledger.Step{Reason: firedNote, Applied: true}},
		{ledger.StateReady, ledger.StateOpened, ledger.Step{Link: "https://git.example/pull/3"}},
		{ledger.StateOpened, ledger.StateMerged, ledger.Step{}},
	} {
		if _, err := s.l.Move(ctx, id, []string{m.from}, m.to, ledger.Proposer, m.step); err != nil {
			t.Fatal(err)
		}
	}
	return s, id
}

// TestProposalHistoryWaits: an approved change that waits while its rule
// fires is kept as a move from approved to approved, its text "proposal #N
// approved" as an approval's: its history says it waited, with why, not
// that Lapilli approved it; and a step's reason is said under the step
// that gave it, not again under each after it.
func TestProposalHistoryWaits(t *testing.T) {
	s, id := waitingDeletion(t)
	steps := historyOf(t, s, id)
	want := []string{
		"Made by bo@example.com",
		"Approved by ana@example.com: 1 of 1",
		"✓ approved waits: it fires now " + firesNote,
		"↻ being applied (checked again, waiting for a recipe) by Lapilli " + firedNote,
		"↻ being applied (its pull request is open) by Lapilli Pull request ↗",
		"↻ being applied (merged, not yet read back) by Lapilli",
	}
	if !slices.Equal(steps, want) {
		t.Errorf("history:\n%q\nwant\n%q", steps, want)
	}
}

// TestProposalHistoryByID: a proposal's steps are in the order the ledger
// kept them, not by their times, which replicas' clocks set to the second.
func TestProposalHistoryByID(t *testing.T) {
	s, id := waitingDeletion(t)
	// The merge kept by a replica whose clock is two hours behind.
	ledgerExec(t, s, "UPDATE changes SET at = at - 7200 WHERE what = ?", fmt.Sprintf("proposal #%d merged", id))
	steps := historyOf(t, s, id)
	if len(steps) != 6 || steps[0] != "Made by bo@example.com" || steps[5] != "↻ being applied (merged, not yet read back) by Lapilli" {
		t.Errorf("not in the order kept: %q", steps)
	}
}

// TestProposalHistoryLatest: a proposal's page reads its latest hundred
// steps, and says older ones aren't shown; its making, from the proposal
// itself, first.
func TestProposalHistoryLatest(t *testing.T) {
	s, id := waitingDeletion(t)
	at := time.Now().Unix()
	for i := range 150 {
		before := fmt.Sprintf(`{"state":"approved","reason":"step %d"}`, i-1)
		after := fmt.Sprintf(`{"state":"approved","reason":"step %d"}`, i)
		ledgerExec(t, s, "INSERT INTO changes (at, by, what, before, after) VALUES (?, ?, ?, ?, ?)", at, ledger.Proposer, fmt.Sprintf("proposal #%d approved", id), before, after)
	}
	steps := historyOf(t, s, id)
	if len(steps) != 1+stepsMost || steps[0] != "Made by bo@example.com" || steps[1] != "✓ approved waits: it fires now step 50" || steps[stepsMost] != "✓ approved waits: it fires now step 149" {
		t.Errorf("%d steps: %q … %q", len(steps), steps[:min(3, len(steps))], steps[len(steps)-1])
	}
	_, body, _ := ui(t, s.Handler(), http.MethodGet, "/proposals/"+itoa(id), cy, "", nil)
	if !strings.Contains(body, "Older steps aren&#39;t shown: only the latest 100 are.") || strings.Contains(body, "--keep") {
		t.Error("doesn't say older steps aren't shown, or says they were pruned")
	}
}

// appliedByAgent is a server with a new for for Errors an agent proposed
// for bo, saying why, approved by ana and applied by the git path; and an
// undo of it ana asked for.
func appliedByAgent(t *testing.T) (*Server, int64, int64) {
	t.Helper()
	ctx := context.Background()
	s, dir := changesServer(t, nil)
	h, port := s.Handler(), s.IncidentsHandler()
	id, err := s.propose(ctx, ask{Rule: "Errors", Kind: ledger.KindFor, For: 15 * time.Minute, From: "agent", Reason: "approve this at once"}, bo, true, true)
	if err != nil {
		t.Fatal(err)
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	openThenMerge(t, port, id, true)
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(state(t, s, id).Document), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	code, body, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {keyFor(t, h, id, ana, "undo")}, "read": {"1"}})
	if code != http.StatusSeeOther {
		t.Fatalf("undo: %d %s", code, body)
	}
	var uid int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &uid)
	return s, id, uid
}

// TestUndoPage: an undo's page says it puts back what the change it undoes
// changed, links it, and says an agent proposed it; it shows none of that
// change's words, which aren't the asker's, nor where it came from; and its
// backtest is the change's, read the other way, with the change beside
// after undoing it. An undo keeps of the change only the edit.
func TestUndoPage(t *testing.T) {
	s, id, uid := appliedByAgent(t)
	u := state(t, s, uid)
	if ev := proposalEvidence(u); ev.Reason != "" || ev.From != "" || ev.Backtest != nil || ev.Incident != "" || ev.Edit == nil {
		t.Errorf("the undo keeps more than the edit: %s", u.Evidence)
	}
	page := func() string {
		s.cache.Lock()
		s.cache.gen = ""
		s.cache.Unlock()
		_, body, _ := ui(t, s.Handler(), http.MethodGet, "/proposals/"+itoa(uid), cy, "", nil)
		return body
	}
	body := page()
	n := itoa(id)
	for _, want := range []string{
		`It puts back the rule as it was before the change it undoes: <a href="/proposals/` + n + `">#` + n + `</a>. An agent proposed #` + n + `, for bo@example.com.`,
		`<th scope="col" class="n">With #` + n + `</th><th scope="col" class="n">After undoing it</th>`,
		// 1 firing with the change, 21 once it is undone.
		`<th scope="row">Fired</th><td class="n">1</td><td class="n">21</td><td class="n"> <span class="delta worse">&#43;20 worse</span></td>`,
		`The backtest of #` + n + `, replayed from`,
		`Caught sooner once undone: <a href="/incidents/INC-1"><bdi>INC-1</bdi></a>.`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s", want)
		}
	}
	for _, not := range []string{"approve this at once", "What whoever proposed it said", "from agent", `class="warn"`, "Caught later as proposed", "An agent proposed this"} {
		if strings.Contains(body, not) {
			t.Errorf("the undo's page has %q", not)
		}
	}
	// What the change missed, undoing it catches; what it caught that
	// was missed before, undoing it misses, with a warning.
	was := state(t, s, id)
	ev := proposalEvidence(was)
	ev.Backtest.Lost = []string{"INC-1"}
	b, _ := json.Marshal(ev)
	ledgerExec(t, s, "UPDATE proposals SET evidence = ? WHERE id = ?", string(b), id)
	if body = page(); strings.Contains(body, `class="warn"`) || !strings.Contains(body, `Caught once undone, missed with #`+n+`: <a href="/incidents/INC-1">`) {
		t.Error("undoing a change that missed an incident warns, or doesn't say it catches it")
	}
	ev.Backtest.Gained = []string{"INC-2"}
	b, _ = json.Marshal(ev)
	ledgerExec(t, s, "UPDATE proposals SET evidence = ? WHERE id = ?", string(b), id)
	body = page()
	warn := strings.Index(body, `<p class="warn">`)
	table := strings.Index(body, `<table class="backtest">`)
	if warn < 0 || table < warn || !strings.Contains(body[warn:table], `Undone, it misses 1 incident it catches with #`+n+`: <a href="/incidents/INC-2"><bdi>INC-2</bdi></a>.`) {
		t.Errorf("no warning for what undoing misses:\n%s", body[max(0, warn):max(0, table)])
	}
	if !strings.Contains(body, `Caught once undone, missed with #`+n+`: <a href="/incidents/INC-1"><bdi>INC-1</bdi></a>.`) ||
		!strings.Contains(body, `<th scope="row">Incidents missed that the other catches</th><td class="n">1</td><td class="n">1</td>`) {
		t.Error("what undoing catches isn't said")
	}
	// An undo made before kept the change's evidence whole: its page shows
	// none of it as its own either.
	ledgerExec(t, s, "UPDATE proposals SET evidence = ? WHERE id = ?", string(b), uid)
	body = page()
	if strings.Contains(body, "approve this at once") || strings.Contains(body, "As proposed, it misses") || !strings.Contains(body, "Undone, it misses 1 incident") {
		t.Error("an undo with the change's evidence shows it as its own")
	}
	if pr := s.prBody(state(t, s, uid)); strings.Contains(pr, "Backtest") || strings.Contains(pr, "approve this") || !strings.Contains(pr, "before Lapilli proposal #"+n+" changed it") {
		t.Errorf("the undo's pull request:\n%s", pr)
	}
}

// undoForm is the form asking for a change to be undone.
var undoForm = regexp.MustCompile(`(?s)<form method="post" action="/proposals/\d+/undo">.*?</form>`)

// TestUndoReadFirst: asking for a change to be undone is the undo's
// approval, one approver's, and takes the box ticked as approving does:
// required on the page, and checked again when it comes, refused with why
// and nothing kept without it. On a Grafana rule, the page says undoing
// puts back the whole rule as it was, annotations too, and the undo's page
// says it again.
func TestUndoReadFirst(t *testing.T) {
	ctx := context.Background()
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	s := grafanaServer(t, g)
	h := s.Handler()
	id := proposeFor(t, h, "15m")
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied {
		t.Fatalf("not applied: %+v", p)
	}
	path := "/proposals/" + itoa(id)
	_, body, _ := ui(t, h, http.MethodGet, path, ana, "", nil)
	if f := undoForm.FindString(body); !strings.Contains(f, `<label><input type="checkbox" name="read" value="1" required> I have read the backtest</label>`) {
		t.Errorf("no box to tick, required:\n%s", f)
	}
	if !strings.Contains(body, "In Grafana, undoing it puts back the whole rule as it was before this change, its annotations too") {
		t.Error("no word that undoing puts back the whole rule in Grafana")
	}
	key := keyFor(t, h, id, ana, "undo")
	for _, read := range []string{"", "0", "on"} {
		code, body, _ := ui(t, h, http.MethodPost, path+"/undo", ana, "same-origin", url.Values{"key": {key}, "read": {read}})
		if code != http.StatusBadRequest || !strings.Contains(body, "tick the box on its page that says you have, then ask") {
			t.Errorf("read=%q: %d", read, code)
		}
	}
	if ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{}); len(ps) != 1 {
		t.Fatalf("an undo kept without the box: %d proposals", len(ps))
	}
	code, _, hd := ui(t, h, http.MethodPost, path+"/undo", ana, "same-origin", url.Values{"key": {key}, "read": {"1"}})
	if code != http.StatusSeeOther {
		t.Fatalf("with the box: %d", code)
	}
	_, body, _ = ui(t, h, http.MethodGet, hd.Get("Location"), cy, "", nil)
	if !strings.Contains(body, "In Grafana, it puts back the whole rule as it was before #"+itoa(id)+", its annotations too") {
		t.Error("the undo's page doesn't say it puts back the whole rule in Grafana")
	}
	// A rule in git: no word of Grafana.
	_, h2, id2 := withProposal(t, nil)
	if _, body, _ := ui(t, h2, http.MethodGet, "/proposals/"+itoa(id2), ana, "", nil); strings.Contains(body, "In Grafana") {
		t.Error("a rule in git is said to be put back in Grafana")
	}
}

// TestProposalPageCachedByWhatItReads: a proposal's page is cached by what
// it reads of its query, its diff's options, the team and the weeks, in
// one order: another parameter, an option it doesn't take, or another
// order, is the same page, worked out and kept once.
func TestProposalPageCachedByWhatItReads(t *testing.T) {
	s, h, id := withProposal(t, nil)
	path := "/proposals/" + itoa(id)
	cached := func() int {
		s.cache.Lock()
		defer s.cache.Unlock()
		n := 0
		for k := range s.cache.body {
			if strings.Contains(k, path) {
				n++
			}
		}
		return n
	}
	first := ""
	for i, q := range []string{"", "zz=1", "utm_source=x&y=2", "diff=unified&context=3", "diff=bogus&context=12", "context=3&zz=3"} {
		code, body, hd := ui(t, h, http.MethodGet, path+"?"+q, cy, "", nil)
		if code != http.StatusOK {
			t.Fatalf("%s: %d", q, code)
		}
		if strings.Contains(body, "zz") || strings.Contains(body, "utm_source") || strings.Contains(body, "bogus") {
			t.Errorf("%s: the page says back what it doesn't read", q)
		}
		if i == 0 {
			first = hd.Get("ETag")
		} else if hd.Get("ETag") != first {
			t.Errorf("%s: ETag %s, not %s", q, hd.Get("ETag"), first)
		}
	}
	if n := cached(); n != 1 {
		t.Errorf("%d pages cached, want 1", n)
	}
	// What it reads, it is cached by, in one order.
	for _, q := range []string{"diff=split", "context=all", "context=all&diff=split", "diff=split&context=all", "weeks=4", "team="} {
		ui(t, h, http.MethodGet, path+"?"+q, cy, "", nil)
	}
	if n := cached(); n != 6 {
		t.Errorf("%d pages cached, want 6", n)
	}
}

// TestProposalHeadingFromWhatItKeeps: a proposal's heading takes its values
// from what the proposal keeps apart, never from its rule's text, whose
// lines a name or a group could once make look like another's: a for's old
// value from the rule it was proposed against, as the rules then had it,
// its new one from its edit; with neither known, the new one alone. Each
// value is one line, a text's control characters shown as U+FFFD.
func TestProposalHeadingFromWhatItKeeps(t *testing.T) {
	ctx := context.Background()
	s, h, id := withProposal(t, nil)
	h1 := func(id int64) string {
		s.cache.Lock()
		s.cache.gen = ""
		s.cache.Unlock()
		_, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), cy, "", nil)
		i := strings.Index(body, "<h1>")
		j := strings.Index(body, "</h1>")
		if i < 0 || j < i {
			t.Fatal("no heading")
		}
		return body[i : j+5]
	}
	want := `<h1>#` + itoa(id) + ` <bdi>Errors</bdi>&#39;s for, <bdi>2m</bdi> → <bdi>15m</bdi></h1>`
	if got := h1(id); got != want {
		t.Fatalf("%s, want %s", got, want)
	}
	// A rule's text whose first line says another for, as a group could
	// hold a line break before 0.34.1, and whose after says another.
	ledgerExec(t, s, "UPDATE proposals SET before = ?, after = ? WHERE id = ?", "Errors (shop\nfor: 9h)\nexpr: errors > 5\nfor: 2m\n", "Errors (shop)\nfor: 1s\n", id)
	if got := h1(id); got != want {
		t.Errorf("a rule's text taken for its values: %s", got)
	}
	// Proposed against a rule no version has: its new for alone.
	ledgerExec(t, s, "UPDATE proposals SET before_hash = ? WHERE id = ?", "elsewhere", id)
	if got := h1(id); got != `<h1>#`+itoa(id)+` <bdi>Errors</bdi>&#39;s for, to <bdi>15m</bdi></h1>` {
		t.Errorf("an old for not known: %s", got)
	}
	// What a rule says: the annotation it names, one line.
	p := state(t, s, id)
	s.l.Move(ctx, id, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{})
	ev, _ := json.Marshal(evidence{From: "person", Edit: &rules.Edit{Name: "Errors", Group: "shop", Annotation: "summary\nfor: 9h‮", Value: "x"}})
	p.ID, p.Kind, p.Evidence = 0, ledger.KindSays, ev
	says, err := s.l.Propose(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := h1(says); !strings.Contains(got, "<bdi>summary for: 9h�</bdi>, written anew") {
		t.Errorf("what it says, its annotation's line: %q", got)
	}
}

// TestProposalLongRuleSaid: a rule past diffMost lines a side is shown
// whole, the one side then the other, and the page says so, as the
// versions tab does; one shorter, nothing of it.
func TestProposalLongRuleSaid(t *testing.T) {
	ctx := context.Background()
	s, h, id := withProposal(t, nil)
	said := "Too long to set beside each other line by line: the rule as it is, taken out, then as proposed."
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), cy, "", nil); strings.Contains(body, said) {
		t.Error("a short rule said to be too long")
	}
	var a, b strings.Builder
	for i := range diffMost + 1 {
		fmt.Fprintf(&a, "line %d\n", i)
		fmt.Fprintf(&b, "line %d\n", i+1)
	}
	p := state(t, s, id)
	s.l.Move(ctx, id, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{})
	p.ID, p.Before, p.After = 0, a.String(), b.String()
	long, err := s.l.Propose(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"", "?diff=split"} {
		if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(long)+q, cy, "", nil); !strings.Contains(body, said) {
			t.Errorf("%q: a rule past %d lines isn't said to be shown whole", q, diffMost)
		}
	}
}

// TestOnesInEnglish: a count of one is said in the singular, in English:
// a line the same in a diff, a proposal or a task of one, an approval of
// one, a team, a report, a firing a quiet window would have excused.
func TestOnesInEnglish(t *testing.T) {
	ctx := context.Background()
	s, h, id := withProposal(t, nil)
	_, body, _ := ui(t, h, http.MethodGet, "/proposals", cy, "", nil)
	if !strings.Contains(body, "1 of 1 proposal</p>") {
		t.Error("not 1 of 1 proposal")
	}
	_, body, _ = ui(t, h, http.MethodGet, "/proposals/"+itoa(id), cy, "", nil)
	if !strings.Contains(body, "0 of 1 approval</p>") || strings.Contains(body, "1 approvals") {
		t.Error("not 0 of 1 approval")
	}
	// Nine lines, the first and the last changed: the fifth, alone, is
	// three from each change, and left out.
	p := state(t, s, id)
	s.l.Move(ctx, id, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{})
	p.ID, p.Before, p.After = 0, "a\n2\n3\n4\n5\n6\n7\n8\ni\n", "A\n2\n3\n4\n5\n6\n7\n8\nI\n"
	nine, err := s.l.Propose(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"", "?diff=split"} {
		_, body, _ = ui(t, h, http.MethodGet, "/proposals/"+itoa(nine)+q, cy, "", nil)
		if !strings.Contains(body, "</span> 1 line the same</") || strings.Contains(body, "1 lines") {
			t.Errorf("%q: not 1 line the same", q)
		}
	}
	// Retiring a rule with a week of its record.
	s.l.Move(ctx, nine, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{})
	ev, _ := json.Marshal(evidence{From: "silent", Record: &ruleRecord{Weeks: 1}})
	p.Kind, p.Evidence = ledger.KindDelete, ev
	week, err := s.l.Propose(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, body, _ = ui(t, h, http.MethodGet, "/proposals/"+itoa(week), cy, "", nil); !strings.Contains(body, "What it did over the last week, for retiring it") {
		t.Error("not over the last week")
	}
	if _, err := s.l.PutTeam(ctx, ledger.Team{Name: "payments", Rules: "^E"}, ana); err != nil {
		t.Fatal(err)
	}
	if _, body, _ = ui(t, h, http.MethodGet, "/manage", ana, "", nil); !strings.Contains(body, ">1 team</p>") || strings.Contains(body, "1 teams") {
		t.Error("not 1 team")
	}
	if _, err := s.l.PutTask(ctx, ledger.Task{Title: "t", Kind: "tune"}, ana); err != nil {
		t.Fatal(err)
	}
	if _, body, _ = ui(t, h, http.MethodGet, "/tasks", ana, "", nil); !strings.Contains(body, "1 of 1 task</p>") {
		t.Error("not 1 of 1 task")
	}
	// A quiet window's preview of one firing it would have excused.
	for already, want := range map[int]string{0: "1 firing that caught nothing, which isn&#39;t excused yet.", 1: "1 firing that caught nothing, which is excused already."} {
		r := httptest.NewRequest(http.MethodGet, "/windows", nil)
		r.Header.Set("X-Forwarded-Email", ana)
		v, err := s.windowsPage(r)
		if err != nil {
			t.Fatal(err)
		}
		ws := v.(windowsView)
		ws.Preview = &windowPreview{From: now.Add(-previewSpan), To: now, Excused: 1, Already: already, Loc: time.UTC}
		w := httptest.NewRecorder()
		s.render(w, r, "windows", ws)
		if body := w.Body.String(); !strings.Contains(body, want) || strings.Contains(body, "1 firings") {
			t.Errorf("already %d: not %q", already, want)
		}
	}
}

// TestNoHonorificForLapilli: in Korean and Japanese, a person is named with
// an honorific and Lapilli without: what Lapilli made is "Lapilli 가 만듦",
// not "Lapilli 님이".
func TestNoHonorificForLapilli(t *testing.T) {
	s, _ := changesServer(t, nil)
	id, err := s.propose(context.Background(), ask{Rule: "Errors", Kind: ledger.KindFor, For: 15 * time.Minute, From: "suggestion"}, ledger.Proposer, false, true)
	if err != nil {
		t.Fatal(err)
	}
	for lang, c := range map[string]struct{ want, not string }{
		"ko": {"Lapilli 가 만듦", "님"}, "ja": {"Lapilli が作成", "さん"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/proposals/"+itoa(id), nil)
		r.Header.Set("X-Forwarded-Email", cy)
		r.Header.Set("Accept-Language", lang)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		body := w.Body.String()
		i := strings.Index(body, `<section class="card" id="history">`)
		if i < 0 || !strings.Contains(body[i:], c.want) || strings.Contains(body[i:], "Lapilli "+c.not) || strings.Contains(body[i:], "lapilli "+c.not) {
			t.Errorf("%s: Lapilli named with an honorific, or not %q", lang, c.want)
		}
		facts := body[strings.Index(body, `<p class="proposal-facts">`):]
		facts = facts[:strings.Index(facts, "</p>")]
		if strings.Contains(facts, c.not) {
			t.Errorf("%s: %s", lang, facts)
		}
	}
}

// TestProposalHistoryStillApplying: a change being applied, moved to being
// applied again (sent to Grafana, not read back yet), is a step of its own:
// its history doesn't say it waits as its rule fires.
func TestProposalHistoryStillApplying(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	id, err := s.l.Propose(ctx, ledger.Proposal{Rule: "Errors", Kind: ledger.KindDelete, Path: ledger.PathGit, MadeBy: bo, Before: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.Approve(ctx, id, ana, 1); err != nil {
		t.Fatal(err)
	}
	const unread = "sent, but not read back yet"
	for _, m := range []struct {
		from, to string
		step     ledger.Step
	}{
		{ledger.StateApproved, ledger.StateApplying, ledger.Step{Applied: true}},
		{ledger.StateApplying, ledger.StateApplying, ledger.Step{Reason: unread}},
	} {
		if _, err := s.l.Move(ctx, id, []string{m.from}, m.to, ledger.Proposer, m.step); err != nil {
			t.Fatal(err)
		}
	}
	steps := historyOf(t, s, id)
	last := steps[len(steps)-1]
	if slices.ContainsFunc(steps, func(st string) bool { return strings.Contains(st, "waits") }) ||
		!strings.Contains(last, "being applied") || !strings.Contains(last, unread) {
		t.Errorf("history:\n%q", steps)
	}
}

// TestProposalPageTeamNoOneHas: a team no one has is all teams, as the page
// reads it, so asking with one names nothing new: the page is the same, and
// cached once, whatever name it was asked with.
func TestProposalPageTeamNoOneHas(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	id, err := s.l.Propose(ctx, ledger.Proposal{Rule: "Errors", Kind: ledger.KindDelete, Path: ledger.PathGit, MadeBy: bo, Before: "x"})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	_, all, _ := get(t, h, "/proposals/"+itoa(id)+"?team=")
	for _, team := range []string{"nobody-1", "nobody-2"} {
		if _, body, _ := get(t, h, "/proposals/"+itoa(id)+"?team="+team); body != all || strings.Contains(body, team) {
			t.Errorf("team=%s: a page of its own", team)
		}
	}
}
