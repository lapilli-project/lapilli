package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
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
	if warn < 0 || table < warn || !strings.Contains(body[warn:table], `As proposed, it misses 1 incidents it catches now: <a href="/incidents/INC-1"><bdi>INC-1</bdi></a>.`) {
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
	code, _, hd := ui(t, h, http.MethodPost, path+"/undo", ana, "same-origin", url.Values{"key": {keyFor(t, h, id, ana, "undo")}})
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
	if !strings.Contains(body, "An undo is approved by the approver who asks for it.") || !strings.Contains(body, "1 of 1 approvals") ||
		!strings.Contains(body, `<th scope="col">As the change left it</th>`) && !strings.Contains(body, `<span class="ins">`) {
		t.Error("the undo's page doesn't say who approved it")
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
	for _, want := range []string{offButton("Drop it", "drop-why", "Editors only"), offButton("Make it a task", "task-why", "Editors only")} {
		if !strings.Contains(body, want) {
			t.Errorf("a viewer: no %s", want)
		}
	}
	if strings.Contains(body, path+"/drop") || strings.Contains(body, path+"/task") {
		t.Error("a viewer is given a form")
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
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
		cs, err := s.l.ProposalSteps(ctx, c.id, 10)
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
