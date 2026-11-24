package serve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/source"
)

// A team as the boundary of change (bounds.go): each guard here is what a
// team's editor or approver, or a race, would try past it
// (CONTRIBUTING.md), with a test that fails without the guard.

const (
	pat   = "pat@example.com"   // an editor of payments alone
	quinn = "quinn@example.com" // an approver of payments alone
	rob   = "rob@example.com"   // an approver of checkout alone
)

// payRules are two teams' rules beside the shop's Errors, which no team
// owns: PayDown is payments's, CartSlow checkout's, by their labels.
const payRules = `groups:
  - name: pay
    labels: {team: payments}
    rules:
      - alert: PayDown
        expr: up == 0
        for: 2m
  - name: cart
    labels: {team: checkout}
    rules:
      - alert: CartSlow
        expr: up == 0
        for: 2m
`

// teamsConfig declares the two teams, pat as payments's editor, quinn as
// its approver and rob as checkout's; more is added at its end.
const teamsConfig = `teams:
  - name: payments
    labels: {team: payments}
    approvers: [user:quinn@example.com]
  - name: checkout
    labels: {team: checkout}
    approvers: [user:rob@example.com]
grants:
  - {user: pat@example.com, team: payments}
`

// boundsServer is a server with changes on, the two teams' rules read,
// bo the only editor of every team, and what config declares kept.
func boundsServer(t *testing.T, config string, more func(*Config)) (*Server, string) {
	t.Helper()
	s, dir := changesServer(t, func(c *Config) {
		c.Editors = []string{bo}
		if more != nil {
			more(c)
		}
	})
	os.WriteFile(filepath.Join(dir, "pay.yaml"), []byte(payRules), 0o644)
	ctx := context.Background()
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	declare(t, s, config)
	return s, dir
}

// proposeSaysAs proposes a summary for a rule, as who: the proposal's id,
// or 0 and the code it was refused with.
func proposeSaysAs(t *testing.T, h http.Handler, who, rule string) (int64, int) {
	t.Helper()
	code, _, hd := ui(t, h, http.MethodPost, "/proposals", who, "same-origin",
		url.Values{"rule": {rule}, "kind": {"says"}, "annotation": {"summary"}, "text": {"x"}})
	if code != http.StatusSeeOther {
		return 0, code
	}
	return locID(hd), code
}

// approveAs approves with the key the page would give who, made here: what
// a person who saw a form once, or made one up, would send.
func approveAs(t *testing.T, s *Server, id int64, who string) (int, string) {
	t.Helper()
	key := s.formKey(state(t, s, id), who, "approve")
	code, body, _ := ui(t, s.Handler(), http.MethodPost, "/proposals/"+itoa(id)+"/approve", who, "same-origin", url.Values{"key": {key}, "read": {"1"}})
	return code, body
}

// A team's editor proposes for their team's rules alone: another team's
// rule, and a rule no team owns, are refused before the rules are read
// again (no try spent); what they propose keeps whose it was.
func TestTeamEditorProposes(t *testing.T) {
	parallel(t)
	s, _ := boundsServer(t, teamsConfig, nil)
	h := s.Handler()
	id, code := proposeSaysAs(t, h, pat, "PayDown")
	if id == 0 {
		t.Fatalf("pat for PayDown: %d", code)
	}
	if w := proposalEvidence(state(t, s, id)).ownedBy; !w.Owned || len(w.Owners) != 1 || w.Owners[0].Name != "payments" {
		t.Errorf("kept as owned by %+v", w)
	}
	tried := len(s.tries.at[triesKey(pat, false)])
	for _, rule := range []string{"CartSlow", "Errors"} {
		if _, code := proposeSaysAs(t, h, pat, rule); code != http.StatusForbidden {
			t.Errorf("pat for %s: %d, want 403", rule, code)
		}
	}
	if n := len(s.tries.at[triesKey(pat, false)]); n != tried {
		t.Errorf("a refused team's editor spent %d tries reading the rules again", n-tried)
	}
	// An editor of every team, as before.
	if id, code := proposeSaysAs(t, h, bo, "CartSlow"); id == 0 {
		t.Errorf("bo for CartSlow: %d", code)
	}
	// A viewer, as before.
	if _, code := proposeSaysAs(t, h, "vic@example.com", "PayDown"); code != http.StatusForbidden {
		t.Errorf("a viewer: %d", code)
	}
	// The rule's page offers pat the forms of PayDown's, and, of
	// CartSlow, says whose editors may.
	if _, body, _ := ui(t, h, http.MethodGet, "/rules/PayDown?tab=proposals", pat, "", nil); !strings.Contains(body, `<form method="post" action="/proposals" class="grid">`) {
		t.Error("PayDown's page offers pat no form")
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/rules/CartSlow?tab=proposals", pat, "", nil); strings.Contains(body, `action="/proposals" class="grid"`) ||
		!strings.Contains(body, "Only <bdi>checkout</bdi>&#39;s editors") {
		t.Error("CartSlow's page offers pat its forms, or doesn't say why not")
	}
}

// Who owns a rule is asked of the rule as it is read again and as
// proposed, not only of its name as the ledger last read it: a rule whose
// group's labels made it another team's since, or a change that would
// rename it or relabel it into another team's, isn't a team's editor's.
func TestTeamEditorBothTexts(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, dir := boundsServer(t, teamsConfig, nil)
	// Read again, PayDown is checkout's: the ledger's last version still
	// says payments.
	os.WriteFile(filepath.Join(dir, "pay.yaml"), []byte(strings.Replace(payRules, "{team: payments}", "{team: checkout}", 1)), 0o644)
	if _, code := proposeSaysAs(t, s.Handler(), pat, "PayDown"); code != http.StatusForbidden {
		t.Errorf("a rule checkout's as read again: %d, want 403", code)
	}
	os.WriteFile(filepath.Join(dir, "pay.yaml"), []byte(payRules), 0o644)
	pay, err := s.current(ctx, "PayDown", false)
	if err != nil {
		t.Fatal(err)
	}
	o, _ := s.owners(ctx)
	inPay := ledger.WithTeams(ctx, []int64{o.owner("PayDown")})
	relabelled := pay.rule
	relabelled.GroupLabels = map[string]string{"team": "checkout"}
	if _, err := s.proposedOwners(inPay, pay, pay.rule, relabelled); !errors.Is(err, ledger.ErrNotYourTeam) {
		t.Errorf("relabelled into checkout's: %v", err)
	}
	// Renamed into the pattern checkout owns by.
	declare(t, s, strings.Replace(teamsConfig, "labels: {team: checkout}", "rules: ^Cart", 1))
	o, _ = s.owners(ctx)
	inPay = ledger.WithTeams(ctx, []int64{o.owner("PayDown")})
	renamed := pay.rule
	renamed.Name = "CartAgain"
	if _, err := s.proposedOwners(inPay, pay, pay.rule, renamed); !errors.Is(err, ledger.ErrNotYourTeam) {
		t.Errorf("renamed into checkout's pattern: %v", err)
	}
	// As it is, payments's alone.
	w, err := s.proposedOwners(inPay, pay, pay.rule, pay.rule)
	if err != nil || len(w.Owners) != 1 || w.Owners[0].Name != "payments" {
		t.Errorf("as it is: %+v %v", w, err)
	}
}

// A team's approver approves what one team, theirs, owns, now and as it
// was proposed; not another team's, not no team's, and not with a form
// made for them: the server asks, whatever the page offered.
func TestTeamApprover(t *testing.T) {
	parallel(t)
	s, _ := boundsServer(t, teamsConfig, nil)
	h := s.Handler()
	pay, _ := proposeSaysAs(t, h, bo, "PayDown")
	cart, _ := proposeSaysAs(t, h, bo, "CartSlow")
	errs, _ := proposeSaysAs(t, h, bo, "Errors")
	if keyFor(t, h, cart, quinn, "approve") != "" || keyFor(t, h, errs, quinn, "approve") != "" {
		t.Error("quinn is offered a form for another team's, or no team's")
	}
	for _, id := range []int64{cart, errs} {
		if code, _ := approveAs(t, s, id, quinn); code != http.StatusForbidden {
			t.Errorf("quinn approving #%d: %d, want 403", id, code)
		}
		if p := state(t, s, id); p.State != ledger.StateProposed || len(p.Approvals) != 0 {
			t.Errorf("#%d: %+v", id, p)
		}
	}
	// Who may approve it, to an admin: payments's approvers too.
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(pay), ana, "", nil); !strings.Contains(body, "And <bdi>payments</bdi>&#39;s approvers, as <code>--config</code> declares them: <bdi>user:quinn@example.com</bdi>.") {
		t.Error("the page doesn't say payments's approvers may approve it")
	}
	if code, body := approve(t, h, pay, quinn); code != http.StatusSeeOther {
		t.Fatalf("quinn approving payments's: %d %s", code, zzMsg(body))
	}
	if p := state(t, s, pay); p.State != ledger.StateApproved || p.Approvals[0].By != quinn {
		t.Errorf("approved: %+v", p)
	}
	// rob, checkout's, approves CartSlow.
	if code, _ := approve(t, h, cart, rob); code != http.StatusSeeOther {
		t.Errorf("rob approving checkout's: %d", code)
	}
	// Someone who approves nothing is refused before the proposal is read.
	if code, _ := approveAs(t, s, errs, "vic@example.com"); code != http.StatusForbidden {
		t.Errorf("a viewer: %d", code)
	}
}

// Two approvals, as --approvals 2 asks: two people, each allowed, one of
// them a team's approver; the same person, listed both for every team and
// for theirs, is one.
func TestTeamApproverTwoApprovals(t *testing.T) {
	parallel(t)
	s, _ := boundsServer(t, strings.Replace(teamsConfig, "[user:quinn@example.com]", "[user:quinn@example.com, user:cy@example.com]", 1),
		func(c *Config) { c.Approvals = 2 })
	h := s.Handler()
	id, _ := proposeSaysAs(t, h, bo, "PayDown")
	if code, _ := approve(t, h, id, cy); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	if code, _ := approveAs(t, s, id, cy); code == http.StatusSeeOther {
		t.Error("cy approved twice, once as an approver of every team, once as payments's")
	}
	if p := state(t, s, id); p.State != ledger.StateProposed {
		t.Fatalf("one of two: %s", p.State)
	}
	if code, _ := approve(t, h, id, quinn); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	if p := state(t, s, id); p.State != ledger.StateApproved || len(p.Approvals) != 2 {
		t.Errorf("two of two: %+v", p)
	}
}

// A team's approver is who the sign-in says, and their groups as the
// proxy's header says them only where --groups-header trusts it.
func TestTeamApproverGroup(t *testing.T) {
	parallel(t)
	config := strings.Replace(teamsConfig, "[user:quinn@example.com]", "[group:pay-leads]", 1)
	for _, trusted := range []bool{true, false} {
		s, _ := boundsServer(t, config, func(c *Config) {
			if trusted {
				c.GroupsHeader = "X-Forwarded-Groups"
			}
		})
		id, _ := proposeSaysAs(t, s.Handler(), bo, "PayDown")
		key := s.formKey(state(t, s, id), "lee@example.com", "approve")
		r := httptest.NewRequest(http.MethodPost, "/proposals/"+itoa(id)+"/approve", strings.NewReader(url.Values{"key": {key}, "read": {"1"}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("X-Forwarded-Email", "lee@example.com")
		r.Header.Set("X-Forwarded-Groups", "pay-leads")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if want := map[bool]int{true: http.StatusSeeOther, false: http.StatusForbidden}[trusted]; w.Code != want {
			t.Errorf("--groups-header given %v: %d, want %d", trusted, w.Code, want)
		}
	}
}

// A rule another team owns now than when it was proposed: no one approves
// it, its page says why; one approved by its team's approver before the
// rule moved is refused as it is carried out.
func TestTeamMovedBetween(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := boundsServer(t, teamsConfig, nil)
	h := s.Handler()
	waiting, _ := proposeSaysAs(t, h, bo, "PayDown")
	o, _ := s.owners(ctx)
	checkout, _ := o.team("checkout")
	// Approved by payments's approver, then the rule is moved to
	// checkout, before it is carried out.
	declare(t, s, teamsConfig)
	approved, _ := proposeSaysAs(t, h, bo, "CartSlow")
	if code, _ := approve(t, h, approved, rob); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	payments, _ := o.team("payments")
	// Moved by an admin: with the boundaries declared, an admin's alone.
	if err := s.l.PutNote(ledger.AsAdmin(ctx), ledger.Note{Rule: "PayDown", TeamID: checkout.ID}, ana); err != nil {
		t.Fatal(err)
	}
	if err := s.l.PutNote(ledger.AsAdmin(ctx), ledger.Note{Rule: "CartSlow", TeamID: payments.ID}, ana); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{quinn, rob, ana} {
		if code, _ := approveAs(t, s, waiting, who); code == http.StatusSeeOther {
			t.Errorf("%s approved a rule another team owns now", who)
		}
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(waiting), ana, "", nil); !strings.Contains(body, "Its rule was <bdi>payments</bdi>&#39;s when it was proposed and is <bdi>checkout</bdi>&#39;s now") {
		t.Error("the page doesn't say its rule moved")
	}
	s.jobs(ctx, false)
	if p := state(t, s, approved); p.State != ledger.StateRefused || !strings.Contains(p.Reason, "was checkout's when it was proposed and is payments's now") {
		t.Errorf("carried out after its rule moved: %s %q", p.State, p.Reason)
	}
}

// A team's changes_in: a rule of its kept outside it is for a person to
// change, said why; one approved before changes_in narrowed is refused as
// it is carried out; a file that names more than --changes-in is refused.
func TestTeamChangesIn(t *testing.T) {
	ctx := context.Background()
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	s := mimirServer(t, m, func(c *Config) { c.Editors = []string{bo} })
	team := func(in string) string {
		return "teams:\n  - name: shop\n    rules: ^Errors\n    approvers: [user:quinn@example.com]\n    changes_in: " + in + "\n"
	}
	declare(t, s, team("[solo]"))
	h := s.Handler()
	id := proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
	if p := state(t, s, id); p.Path != ledger.PathNone || proposalEvidence(p).NoPath != noPathTeamIn {
		t.Fatalf("outside its team's changes_in: %s %q", p.Path, proposalEvidence(p).NoPath)
	}
	ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/drop", bo, "same-origin", url.Values{})
	declare(t, s, team("[shop]"))
	id = proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
	if p := state(t, s, id); p.Path != ledger.PathRuler {
		t.Fatalf("within its team's changes_in: %s %q", p.Path, proposalEvidence(p).NoPath)
	}
	if code, _ := approve(t, h, id, quinn); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	declare(t, s, team("[]"))
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateRefused || p.Reason != noPathTeamIn || len(m.written()) != 0 {
		t.Errorf("carried out outside its team's changes_in now: %s %q, written %v", p.State, p.Reason, m.written())
	}
	// Within --changes-in, or not.
	for in, ok := range map[string]bool{"[shop]": true, "[solo, shop]": true, "[other]": false, "[shop*]": false, "['*']": false} {
		d, err := ParseConfig([]byte(team(in)), time.UTC)
		if err == nil {
			err = s.boundsOK(d)
		}
		if (err == nil) != ok {
			t.Errorf("changes_in %s: %v", in, err)
		}
	}
	s.cfg.ChangesIn = []string{"team-*"}
	if !s.withinChangesIn("team-pay*") || !s.withinChangesIn("team-pay") || s.withinChangesIn("team*") || s.withinChangesIn("other") {
		t.Error("a prefix's narrower prefix and names")
	}
}

// A change freeze: approving and asking to undo are refused, the freeze
// named, the buttons disabled with why; proposing goes on; one approved
// before waits, said once, and isn't handed out; once it ends, at its end
// and not before, it is carried out.
func TestFreeze(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	s, _ := boundsServer(t, teamsConfig, nil)
	s.wall = func() time.Time { return at }
	h, port := s.Handler(), s.IncidentsHandler()
	before, _ := proposeSaysAs(t, h, bo, "PayDown")
	if code, _ := approve(t, h, before, quinn); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	declare(t, s, teamsConfig+"freezes:\n  - {title: Black Friday, start: 2026-11-27T00:00, end: 2026-11-30T00:00}\n")
	during, code := proposeSaysAs(t, h, bo, "CartSlow")
	if during == 0 {
		t.Fatalf("proposing during a freeze: %d", code)
	}
	for _, who := range []string{ana, rob} {
		code, body := approveAs(t, s, during, who)
		if code != http.StatusConflict || !strings.Contains(body, "Black Friday") {
			t.Errorf("%s approving during the freeze: %d %s", who, code, zzMsg(body))
		}
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(during), ana, "", nil); !strings.Contains(body, `disabled aria-describedby="approve-why"`) ||
		!strings.Contains(body, "Held by a change freeze, <bdi>Black Friday</bdi>, until it ends") || !strings.Contains(body, "2026-11-30 00:00") {
		t.Error("the page doesn't say the freeze holds it, or offers the form")
	}
	s.jobs(ctx, false)
	p := state(t, s, before)
	if p.State != ledger.StateApproved || !frozeNote(p.Reason) || !strings.Contains(p.Reason, "Black Friday") {
		t.Fatalf("approved before it, it waits: %s %q", p.State, p.Reason)
	}
	steps, _, _ := s.l.ProposalSteps(ctx, before, 100)
	s.jobs(ctx, false)
	if again, _, _ := s.l.ProposalSteps(ctx, before, 100); len(again) != len(steps) {
		t.Error("the wait said again each round")
	}
	// At its end, still frozen; past it, carried out.
	s.wall = func() time.Time { return time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC) }
	s.jobs(ctx, false)
	if p := state(t, s, before); p.State != ledger.StateApproved {
		t.Errorf("at the freeze's end: %s", p.State)
	}
	s.wall = func() time.Time { return time.Date(2026, 11, 30, 0, 0, 1, 0, time.UTC) }
	s.jobs(ctx, false)
	if p := state(t, s, before); p.State != ledger.StateReady || p.Reason != thawedNote {
		t.Errorf("after the freeze: %s %q", p.State, p.Reason)
	}
	// Ready, a freeze from then on holds it from the recipe.
	declare(t, s, teamsConfig+"freezes:\n  - {title: Payments month end, team: payments, start: 2026-11-30T00:00, end: 2026-12-01T00:00}\n")
	if n := ready(t, port); n != 0 {
		t.Errorf("%d handed out during a freeze of its team", n)
	}
	// Another team's freeze holds nothing of checkout's.
	if code, _ := approve(t, h, during, rob); code != http.StatusSeeOther {
		t.Errorf("checkout's during payments's freeze: %d", code)
	}
}

// A freeze holds what --approvals lowered would approve, and an undo asked
// for.
func TestFreezeHoldsPromoteAndUndo(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	s, dir := boundsServer(t, teamsConfig, func(c *Config) { c.Approvals = 2 })
	s.wall = func() time.Time { return at }
	h := s.Handler()
	id, _ := proposeSaysAs(t, h, bo, "PayDown")
	if code, _ := approve(t, h, id, quinn); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	declare(t, s, teamsConfig+"freezes:\n  - {title: Black Friday, team: payments, start: 2026-11-27T00:00, end: 2026-11-30T00:00}\n")
	s.cfg.Approvals = 1
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateProposed {
		t.Fatalf("approved as fewer are needed, during a freeze: %s", p.State)
	}
	// Applied before a freeze, its undo isn't asked for during one.
	declare(t, s, teamsConfig)
	s.jobs(ctx, false)
	p := state(t, s, id)
	if p.State != ledger.StateReady {
		t.Fatalf("after the freeze was taken out: %s", p.State)
	}
	recipe(t, s.IncidentsHandler(), http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "opened", "link": "https://git.example/shop/pull/8"})
	recipe(t, s.IncidentsHandler(), http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "merged"})
	os.WriteFile(filepath.Join(dir, "pay.yaml"), []byte(p.Document), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if p = state(t, s, id); p.State != ledger.StateApplied {
		t.Fatalf("applied: %s", p.State)
	}
	declare(t, s, teamsConfig+"freezes:\n  - {title: Black Friday, start: 2026-11-27T00:00, end: 2026-11-30T00:00}\n")
	key := s.formKey(p, quinn, "undo")
	if code, body, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", quinn, "same-origin", url.Values{"key": {key}, "read": {"1"}}); code != http.StatusConflict {
		t.Errorf("an undo asked for during a freeze: %d %s", code, zzMsg(body))
	}
	if keyFor(t, h, id, ana, "undo") != "" {
		t.Error("the undo's form offered during a freeze")
	}
	// Once it ends, payments's approver asks for it, as its team's.
	s.wall = func() time.Time { return time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) }
	key = keyFor(t, h, id, quinn, "undo")
	code, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", quinn, "same-origin", url.Values{"key": {key}, "read": {"1"}})
	if code != http.StatusSeeOther {
		t.Fatalf("payments's approver asking to undo: %d", code)
	}
	if w := proposalEvidence(state(t, s, locID(hd))).ownedBy; len(w.Owners) != 1 || w.Owners[0].Name != "payments" {
		t.Errorf("the undo kept as %+v", w)
	}
}

// A team's editor drops what they proposed for their team, and nothing
// else; backtests again only their team's.
func TestTeamEditorDropsOwn(t *testing.T) {
	parallel(t)
	s, _ := boundsServer(t, teamsConfig, nil)
	h := s.Handler()
	own, _ := proposeSaysAs(t, h, pat, "PayDown")
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(own)+"/drop", pat, "same-origin", url.Values{}); code != http.StatusSeeOther {
		t.Errorf("pat dropping their own: %d", code)
	}
	if p := state(t, s, own); p.State != ledger.StateDropped {
		t.Errorf("dropped: %s", p.State)
	}
	others, _ := proposeSaysAs(t, h, bo, "PayDown")
	cart, _ := proposeSaysAs(t, h, bo, "CartSlow")
	for _, id := range []int64{others, cart} {
		if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/drop", pat, "same-origin", url.Values{}); code != http.StatusForbidden {
			t.Errorf("pat dropping #%d, not theirs: %d", id, code)
		}
	}
	// Backtesting again: another team's is refused for being theirs, their
	// own team's for being fresh.
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(cart)+"/backtest", pat, "same-origin", url.Values{}); code != http.StatusForbidden {
		t.Errorf("pat backtesting checkout's again: %d", code)
	}
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(others)+"/backtest", pat, "same-origin", url.Values{}); code != http.StatusBadRequest {
		t.Errorf("pat backtesting payments's again: %d", code)
	}
}

// A bulk proposal from the rules list, by a team's editor: their team's
// rules proposed, another team's counted as not theirs.
func TestTeamEditorBulkProposes(t *testing.T) {
	parallel(t)
	s, _ := boundsServer(t, teamsConfig, nil)
	form := url.Values{"do": {"propose"}, "propose": {"retire"}, "rule": {"CartSlow", "PayDown", "Errors"}, "back": {"/rules"}}
	code, _, hd := ui(t, s.Handler(), http.MethodPost, "/rules/bulk", pat, "same-origin", form)
	if code != http.StatusSeeOther {
		t.Fatalf("%d", code)
	}
	q, _ := url.ParseQuery(strings.SplitN(hd.Get("Location"), "?", 2)[1])
	if q.Get("theirs") != "2" || q.Get("failed") != "" {
		t.Errorf("went back to %s", hd.Get("Location"))
	}
	ps, _ := s.l.Proposals(context.Background(), ledger.ProposalQuery{})
	for _, p := range ps {
		if p.Rule != "PayDown" {
			t.Errorf("proposed for %s", p.Rule)
		}
	}
}

// What a file declares of the boundaries: read strictly, kept in the
// ledger in the write that keeps it to the file, the same file writing
// nothing again; given back as the export gives it; gone without a file.
func TestBoundsDeclared(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	for body, want := range map[string]string{
		"teams:\n  - {name: a, approvers: [ana]}\n":                                                                  "write user:NAME",
		"teams:\n  - {name: a, approvers: ['team:x']}\n":                                                             "write user:NAME",
		"teams:\n  - {name: a, approvers: ['*']}\n":                                                                  "everyone signed in",
		"teams:\n  - {name: a, approvers: []}\n":                                                                     "names no one",
		"teams:\n  - {name: a, approvers: [user:lapilli]}\n":                                                         "keeps for itself",
		"teams:\n  - name: a\n    approvers:\n":                                                                      "nothing after it",
		"teams:\n  - {name: a, changes_in: [a,b]}\n":                                                                 "",
		"teams:\n  - {name: a, changes_in: ['a,b']}\n":                                                               "changes_in a,b",
		"teams:\n  - {name: a}\nfreezes:\n  - {title: x, team: b, start: 2026-11-27T00:00, end: 2026-11-28T00:00}\n": "isn't one this file declares",
		"freezes:\n  - {title: x, start: 2026-11-27T00:00, end: 2026-12-30T00:00}\n":                                 "31 days",
		"freezes:\n  - {start: 2026-11-27T00:00, end: 2026-11-28T00:00}\n":                                           "has a title",
		"freezes:\n  - {title: x, team: '', start: 2026-11-27T00:00, end: 2026-11-28T00:00}\n":                       "every team",
		"freezes:\n  - {title: x, on: 1, start: 2026-11-27T00:00, end: 2026-11-28T00:00}\n":                          "unknown key",
	} {
		_, err := ParseConfig([]byte(body), time.UTC)
		if (err == nil) != (want == "") || err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", body, err, want)
		}
	}
	s, _ := boundsServer(t, teamsConfig, nil)
	file := teamsConfig + "freezes:\n  - {title: Nightly, team: payments, start: '2026-11-27T22:00:00+09:00', end: '2026-11-28T06:00:00+09:00', every: day, until: 2026-12-31}\n"
	d, err := ParseConfig([]byte(file), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if r, _, err := s.l.Reconcile(ctx, d, ledger.Leave{}, s.cfg.Options); err != nil || r.Changed != 1 {
		t.Fatalf("kept: %+v %v", r, err)
	}
	if r, _, err := s.l.Reconcile(ctx, d, ledger.Leave{}, s.cfg.Options); err != nil || r.Any() {
		t.Errorf("the same file again: %+v %v", r, err)
	}
	all, err := s.l.AllDeclarable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseConfig(exportConfig(all, s.cfg.Loc), s.cfg.Loc)
	if err != nil || back.Bounds != d.Bounds {
		t.Errorf("the export reads back as\n%s\nnot\n%s (%v)\n%s", back.Bounds, d.Bounds, err, exportConfig(all, s.cfg.Loc))
	}
	b, err := s.bounds(ctx)
	if err != nil || len(b.Freezes) != 1 || b.Teams["payments"].Approvers[0] != "user:quinn@example.com" {
		t.Fatalf("read back: %+v %v", b, err)
	}
	if _, _, err := s.l.Reconcile(ctx, ledger.Declared{}, ledger.Leave{NoFile: true}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if b, err := s.bounds(ctx); err != nil || len(b.Teams)+len(b.Freezes) != 0 {
		t.Errorf("without a file: %+v %v", b, err)
	}
}

// A repeating freeze spanning midnight, in --tz: covered and ended as the
// calendar there says, its end included; the next one after.
func TestFreezeSpan(t *testing.T) {
	parallel(t)
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skip(err)
	}
	w := ledger.Window{Title: "Nightly", Start: time.Date(2026, 11, 2, 22, 0, 0, 0, seoul), End: time.Date(2026, 11, 3, 6, 0, 0, 0, seoul), Every: "day"}
	if err := ledger.CheckSpan(&w, seoul); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		at  time.Time
		end time.Time
	}{
		{time.Date(2026, 11, 10, 23, 0, 0, 0, seoul), time.Date(2026, 11, 11, 6, 0, 0, 0, seoul)},
		{time.Date(2026, 11, 11, 5, 59, 0, 0, seoul), time.Date(2026, 11, 11, 6, 0, 0, 0, seoul)},
		{time.Date(2026, 11, 11, 6, 0, 0, 0, seoul), time.Date(2026, 11, 11, 6, 0, 0, 0, seoul)},
		{time.Date(2026, 11, 11, 6, 0, 1, 0, seoul), time.Time{}},
		{time.Date(2026, 11, 11, 21, 59, 0, 0, seoul), time.Time{}},
		{time.Date(2026, 11, 2, 21, 0, 0, 0, seoul), time.Time{}},
	} {
		end, ok := w.EndOf(c.at)
		if ok != !c.end.IsZero() || ok && !end.Equal(c.end) || ok != w.Covers(c.at) {
			t.Errorf("at %s: %v %s, covers %v", c.at, ok, end, w.Covers(c.at))
		}
	}
	if start, end, ok := w.NextAfter(time.Date(2026, 11, 11, 6, 0, 1, 0, seoul)); !ok || !start.Equal(time.Date(2026, 11, 11, 22, 0, 0, 0, seoul)) || !end.Equal(time.Date(2026, 11, 12, 6, 0, 0, 0, seoul)) {
		t.Errorf("next: %s %s %v", start, end, ok)
	}
	// By a cron expression, every Saturday 02:00 for two hours, until a day.
	c := ledger.Window{Title: "Sat", Start: time.Date(2026, 11, 7, 2, 0, 0, 0, seoul), End: time.Date(2026, 11, 7, 4, 0, 0, 0, seoul), Every: "cron", On: "0 2 * * 6",
		Until: time.Date(2026, 11, 21, 0, 0, 0, 0, seoul)}
	if err := ledger.CheckSpan(&c, seoul); err != nil {
		t.Fatal(err)
	}
	if end, ok := c.EndOf(time.Date(2026, 11, 14, 3, 0, 0, 0, seoul)); !ok || !end.Equal(time.Date(2026, 11, 14, 4, 0, 0, 0, seoul)) {
		t.Errorf("a Saturday: %s %v", end, ok)
	}
	if _, ok := c.EndOf(time.Date(2026, 11, 28, 3, 0, 0, 0, seoul)); ok {
		t.Error("past until")
	}
	if _, _, ok := c.NextAfter(time.Date(2026, 11, 21, 5, 0, 0, 0, seoul)); ok {
		t.Error("a next one past until")
	}
	// In UTC the same instants: the tz decides the calendar, not the clock.
	if !w.Covers(time.Date(2026, 11, 10, 14, 30, 0, 0, time.UTC)) || w.Covers(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)) {
		t.Error("in UTC")
	}
}

// (a) An editor of every team who approves for checkout moves payments's
// PayDown into checkout with a note, then approves its change as
// checkout's approver. With the boundaries declared, a rule's team is an
// admin's to move, by the note's form, the API, the rules list's bulk
// "team" and the Cleanup page's move alike; the note's other fields are
// still an editor's.
func TestNoteTeamAdminsOnly(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := boundsServer(t, teamsConfig, func(c *Config) { c.Editors = append(c.Editors, rob) })
	h := s.Handler()
	o, _ := s.owners(ctx)
	checkout, _ := o.team("checkout")
	into := itoa(checkout.ID)
	if code, body, _ := ui(t, h, http.MethodPost, "/notes", rob, "same-origin", url.Values{"rule": {"PayDown"}, "team": {into}}); code != http.StatusForbidden {
		t.Errorf("rob moving PayDown by the note's form: %d %s", code, zzMsg(body))
	}
	if code, body, _ := apiAs(t, h, http.MethodPut, "/api/v1/notes", ledger.Note{Rule: "PayDown", TeamID: checkout.ID}, rob, "", ""); code != http.StatusForbidden {
		t.Errorf("rob moving PayDown through the API: %d %s", code, body)
	}
	code, _, hd := ui(t, h, http.MethodPost, "/rules/bulk", rob, "same-origin", url.Values{"do": {"team"}, "team": {into}, "rule": {"PayDown"}, "back": {"/rules"}})
	if q, _ := url.ParseQuery(strings.SplitN(hd.Get("Location"), "?", 2)[1]); code != http.StatusSeeOther || q.Get("admins") != "1" || q.Get("done") != "" {
		t.Errorf("rob moving PayDown from the rules list: %d %s", code, hd.Get("Location"))
	}
	// A rule's records moved from a name no rule has any more, whose note
	// is checkout's: PayDown would take its team.
	if err := s.l.PutNote(ledger.AsAdmin(ctx), ledger.Note{Rule: "PayDownOld", TeamID: checkout.ID}, ana); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := ui(t, h, http.MethodPost, "/orphans/move", rob, "same-origin", url.Values{"from": {"PayDownOld"}, "to": {"PayDown"}}); code != http.StatusForbidden {
		t.Errorf("rob moving a note of checkout's onto PayDown: %d", code)
	}
	if o, _ := s.owners(ctx); o.byID[o.owner("PayDown")].Name != "payments" {
		t.Fatalf("PayDown is %s's", o.byID[o.owner("PayDown")].Name)
	}
	// Nor is the note deleted there, which takes PayDownOld out of
	// checkout: an admin's.
	if code, _, _ := ui(t, h, http.MethodPost, "/orphans/delete", rob, "same-origin", url.Values{"rule": {"PayDownOld"}}); code != http.StatusForbidden {
		t.Errorf("rob deleting a note of checkout's: %d", code)
	}
	if n, err := s.l.NoteOf(ctx, "PayDownOld"); err != nil || n.TeamID != checkout.ID {
		t.Errorf("deleted by rob: %+v %v", n, err)
	}
	// So bo's proposal is payments's, and rob doesn't approve it.
	id, _ := proposeSaysAs(t, h, bo, "PayDown")
	if code, _ := approveAs(t, s, id, rob); code != http.StatusForbidden {
		t.Errorf("rob approving PayDown's change: %d", code)
	}
	// The rest of a note is an editor's still; its team, an admin's.
	if code, _, _ := ui(t, h, http.MethodPost, "/notes", rob, "same-origin", url.Values{"rule": {"PayDown"}, "status": {"tune"}}); code != http.StatusSeeOther {
		t.Errorf("rob's note without moving it: %d", code)
	}
	if code, _, _ := ui(t, h, http.MethodPost, "/notes", ana, "same-origin", url.Values{"rule": {"CartSlow"}, "team": {into}}); code != http.StatusSeeOther {
		t.Errorf("an admin's note: %d", code)
	}
	// Without boundaries, as before: an editor's.
	declare(t, s, strings.ReplaceAll(strings.ReplaceAll(teamsConfig, "    approvers: [user:quinn@example.com]\n", ""), "    approvers: [user:rob@example.com]\n", ""))
	if code, _, _ := ui(t, h, http.MethodPost, "/notes", rob, "same-origin", url.Values{"rule": {"PayDown"}, "team": {into}}); code != http.StatusSeeOther {
		t.Errorf("without boundaries: %d", code)
	}
}

// (b) payments's own freeze, and its changes_in, hold PayDown however a
// note moves it: bo, an editor of every team, doesn't move it into
// checkout now; a note an admin wrote (or one written before the
// boundaries were) leaves it held by both teams' boundaries, and no one
// team's to approve.
func TestNoteMovedHeldByBoth(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	const cara = "cara@example.com" // an editor of checkout alone
	s, _ := boundsServer(t, strings.Replace(teamsConfig, "grants:\n", "grants:\n  - {user: cara@example.com, team: checkout}\n", 1), nil)
	s.wall = func() time.Time { return at }
	h := s.Handler()
	o, _ := s.owners(ctx)
	checkout, _ := o.team("checkout")
	if code, body, _ := ui(t, h, http.MethodPost, "/notes", bo, "same-origin", url.Values{"rule": {"PayDown"}, "team": {itoa(checkout.ID)}}); code != http.StatusForbidden {
		t.Fatalf("bo moving PayDown into checkout: %d %s", code, zzMsg(body))
	}
	if err := s.l.PutNote(ledger.AsAdmin(ctx), ledger.Note{Rule: "PayDown", TeamID: checkout.ID}, ana); err != nil {
		t.Fatal(err)
	}
	// Neither team's editor proposes for it alone.
	for _, who := range []string{cara, pat} {
		if _, code := proposeSaysAs(t, h, who, "PayDown"); code != http.StatusForbidden {
			t.Errorf("%s, an editor of one team, proposing for PayDown: %d", who, code)
		}
	}
	before, _ := proposeSaysAs(t, h, bo, "PayDown")
	if w := proposalEvidence(state(t, s, before)).ownedBy; len(w.Owners) != 2 {
		t.Errorf("kept as owned by %+v, not by both", w)
	}
	if code, _ := approve(t, h, before, ana); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	frozen := teamsConfig + "freezes:\n  - {title: Payments close, team: payments, start: 2026-11-27T00:00, end: 2026-11-30T00:00}\n"
	declare(t, s, frozen)
	s.jobs(ctx, false)
	if p := state(t, s, before); p.State != ledger.StateApproved || !frozeNote(p.Reason) {
		t.Errorf("carried out during payments's freeze: %s %q", p.State, p.Reason)
	}
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(before)+"/drop", ana, "same-origin", url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("dropping it: %d", code)
	}
	during, code := proposeSaysAs(t, h, bo, "PayDown")
	if during == 0 {
		t.Fatalf("proposing during the freeze: %d", code)
	}
	if code, body := approveAs(t, s, during, ana); code != http.StatusConflict {
		t.Errorf("approved during payments's freeze: %d %s", code, zzMsg(body))
	}
	// Once it ends, neither team's approver approves it: it isn't one
	// team's both ways.
	s.wall = func() time.Time { return time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) }
	for _, who := range []string{rob, quinn} {
		if code, _ := approveAs(t, s, during, who); code != http.StatusForbidden {
			t.Errorf("%s, a team's approver: %d", who, code)
		}
	}
	// payments's changes_in, nowhere: a rule of payments's moved into
	// checkout by a note is still written nowhere.
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	ms := mimirServer(t, m, func(c *Config) { c.Editors = []string{bo} })
	declare(t, ms, "teams:\n  - name: shop\n    rules: ^Errors\n    changes_in: []\n  - name: edge\n    rules: ^Edge\n")
	mo, _ := ms.owners(ctx)
	edge, _ := mo.team("edge")
	if err := ms.l.PutNote(ledger.AsAdmin(ctx), ledger.Note{Rule: "Errors [shop/api]", TeamID: edge.ID}, ana); err != nil {
		t.Fatal(err)
	}
	id := proposeRule(t, ms.Handler(), "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
	if p := state(t, ms, id); p.Path != ledger.PathNone || proposalEvidence(p).NoPath != noPathTeamIn {
		t.Errorf("moved out of shop by a note, written outside shop's changes_in: %s %q", p.Path, proposalEvidence(p).NoPath)
	}
}

// An applied change to payments's rule, moved into checkout by a note
// since: checkout's approver doesn't ask to undo it, payments's freeze
// holds its undo, and the undo an approver of every team asks for is no
// one team's.
func TestNoteMovedUndo(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, dir := boundsServer(t, teamsConfig, nil)
	s.wall = func() time.Time { return time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC) }
	h := s.Handler()
	id, _ := proposeSaysAs(t, h, bo, "PayDown")
	if code, _ := approve(t, h, id, quinn); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	s.jobs(ctx, false)
	p := state(t, s, id)
	recipe(t, s.IncidentsHandler(), http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "opened", "link": "https://git.example/shop/pull/9"})
	recipe(t, s.IncidentsHandler(), http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "merged"})
	os.WriteFile(filepath.Join(dir, "pay.yaml"), []byte(p.Document), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if p = state(t, s, id); p.State != ledger.StateApplied {
		t.Fatalf("applied: %s", p.State)
	}
	o, _ := s.owners(ctx)
	checkout, _ := o.team("checkout")
	if err := s.l.PutNote(ledger.AsAdmin(ctx), ledger.Note{Rule: "PayDown", TeamID: checkout.ID}, ana); err != nil {
		t.Fatal(err)
	}
	undo := func(who string) (int, int64) {
		key := s.formKey(state(t, s, id), who, "undo")
		code, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", who, "same-origin", url.Values{"key": {key}, "read": {"1"}})
		return code, locID(hd)
	}
	if code, _ := undo(rob); code != http.StatusForbidden {
		t.Errorf("checkout's approver asking to undo payments's change: %d", code)
	}
	declare(t, s, teamsConfig+"freezes:\n  - {title: Payments close, team: payments, start: 2026-11-27T00:00, end: 2026-11-30T00:00}\n")
	if code, _ := undo(ana); code != http.StatusConflict {
		t.Errorf("an undo asked for during payments's freeze: %d", code)
	}
	declare(t, s, teamsConfig)
	code, u := undo(ana)
	if code != http.StatusSeeOther {
		t.Fatalf("an approver of every team asking to undo it: %d", code)
	}
	if w := proposalEvidence(state(t, s, u)).ownedBy; len(w.Owners) != 2 {
		t.Errorf("the undo kept as owned by %+v, not by both", w)
	}
}

// A freeze that begins while an approved change is checked again (its
// backtest run again, up to --timeout): asked again just before the write,
// it holds it, approved, and nothing is handed out or sent.
func TestFreezeBeginsWhileChecked(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	freeze := "freezes:\n  - {title: Black Friday, start: 2026-11-27T00:00, end: 2026-11-30T00:00}\n"
	t.Run("git", func(t *testing.T) {
		s, _ := boundsServer(t, teamsConfig, nil)
		s.wall = func() time.Time { return at }
		h, port := s.Handler(), s.IncidentsHandler()
		id := proposeFor(t, h, "15m")
		approve(t, h, id, ana)
		fh := &freezingHistory{s: s, config: teamsConfig + freeze}
		fh.armed.Store(true)
		s.h = fh
		if err := s.carryOut(ctx); err != nil {
			t.Fatal(err)
		}
		if fh.err != nil {
			t.Fatal(fh.err)
		}
		if p := state(t, s, id); p.State != ledger.StateApproved || !frozeNote(p.Reason) || ready(t, port) != 0 {
			t.Errorf("a freeze begun during the backtest: %s %q, %d handed out", p.State, p.Reason, ready(t, port))
		}
	})
	t.Run("ruler", func(t *testing.T) {
		m := mimirRuler(t, "/prometheus/config/v1/rules")
		s := mimirServer(t, m, func(c *Config) { c.Editors = []string{bo} })
		s.wall = func() time.Time { return at }
		h := s.Handler()
		id := proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
		approve(t, h, id, ana)
		fh := &freezingHistory{s: s, config: freeze}
		fh.armed.Store(true)
		s.h = fh
		if err := s.carryOut(ctx); err != nil {
			t.Fatal(err)
		}
		if fh.err != nil {
			t.Fatal(fh.err)
		}
		if p := state(t, s, id); p.State != ledger.StateApproved || !frozeNote(p.Reason) || len(m.written()) != 0 {
			t.Errorf("a freeze begun during the backtest: %s %q, written %v", p.State, p.Reason, m.written())
		}
	})
}

// freezingHistory is history that keeps a file with a change freeze to
// the ledger when it is first asked once armed: a freeze declared while a
// change's backtest runs again.
type freezingHistory struct {
	history
	s      *Server
	config string
	armed  atomic.Bool
	err    error
}

func (h *freezingHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if h.armed.CompareAndSwap(true, false) {
		d, err := ParseConfig([]byte(h.config), time.UTC)
		if err == nil {
			_, _, err = h.s.l.Reconcile(context.Background(), d, ledger.Leave{}, h.s.cfg.Options)
		}
		h.err = err
	}
	return h.history.QueryRange(ctx, expr, start, end, step)
}

// A team's name as the ledger keeps it, without the spaces around it: its
// approvers and freezes are that team's. Two teams of one name in another
// letter case, or with spaces, are refused where boundaries name them; a
// team the ledger didn't keep leaves its boundaries holding nothing, and
// the Settings page says so.
func TestBoundsTeamNames(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	spaced := strings.Replace(teamsConfig, "name: payments", `name: "payments "`, 1) +
		"freezes:\n  - {title: Payments close, team: 'payments ', start: 2026-11-27T00:00, end: 2026-11-30T00:00}\n"
	s, _ := boundsServer(t, spaced, nil)
	s.wall = func() time.Time { return at }
	h := s.Handler()
	if b, err := s.bounds(ctx); err != nil || len(b.Teams["payments"].Approvers) != 1 || b.Freezes[0].Team != "payments" {
		t.Fatalf("kept as %+v %v", b, err)
	}
	id, _ := proposeSaysAs(t, h, bo, "PayDown")
	if code, body := approveAs(t, s, id, ana); code != http.StatusConflict {
		t.Errorf("approved during the freeze of \"payments \": %d %s", code, zzMsg(body))
	}
	for _, body := range []string{
		"teams:\n  - {name: payments, rules: ^Pay}\n  - {name: Payments, rules: ^Pay, approvers: [user:quinn@example.com]}\n",
		"teams:\n  - {name: payments, rules: ^Pay, approvers: [user:quinn@example.com]}\n  - {name: ' payments', rules: ^Pay}\n",
		"teams:\n  - {name: payments, rules: ^Pay}\n  - {name: PAYMENTS, rules: ^Pay}\nfreezes:\n  - {title: x, team: PAYMENTS, start: 2026-11-27T00:00, end: 2026-11-28T00:00}\n",
	} {
		if _, err := ParseConfig([]byte(body), time.UTC); err == nil || !strings.Contains(err.Error(), "in another letter case or with spaces around it") || !strings.Contains(err.Error(), "line ") {
			t.Errorf("%q: %v", body, err)
		}
	}
	// A team the ledger doesn't keep (its pattern doesn't read): its
	// boundaries are said to hold nothing.
	bad := "teams:\n  - {name: billing, rules: '(', approvers: [user:quinn@example.com]}\nfreezes:\n  - {title: Billing close, team: billing, start: 2026-11-27T00:00, end: 2026-11-28T00:00}\n"
	d, err := ParseConfig([]byte(bad), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if _, skipped, err := s.l.Reconcile(ctx, d, ledger.Leave{}, s.cfg.Options); err != nil || len(skipped) != 1 {
		t.Fatalf("%v %v", skipped, err)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/settings", ana, "", nil); !strings.Contains(body, "teams it didn&#39;t keep, so they hold nothing:") || !strings.Contains(body, "<code><bdi>billing</bdi></code>") {
		t.Error("the Settings page doesn't say billing's boundaries hold nothing")
	}
}

// Boundaries the ledger holds that can't be read: the proposal's page and
// the Settings page are drawn, saying so, approving and undoing taken
// away and dropping left; nothing they would hold goes ahead.
func TestBoundsUnreadPages(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := boundsServer(t, teamsConfig, nil)
	h := s.Handler()
	id, _ := proposeSaysAs(t, h, bo, "PayDown")
	if err := s.l.SetMeta(ctx, ledger.BoundsKey, "{not json"); err != nil {
		t.Fatal(err)
	}
	const said = "The change boundaries the ledger holds can&#39;t be read"
	code, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), ana, "", nil)
	if code != http.StatusOK || !strings.Contains(body, said) || keyFor(t, h, id, ana, "approve") != "" ||
		!strings.Contains(body, `action="/proposals/`+itoa(id)+`/drop"`) || !strings.Contains(body, "Held while the change boundaries can&#39;t be read") {
		t.Errorf("the proposal's page: %d\n%s", code, zzMsg(body))
	}
	if code, body, _ := ui(t, h, http.MethodGet, "/settings", ana, "", nil); code != http.StatusOK || !strings.Contains(body, said) {
		t.Errorf("the Settings page: %d %s", code, zzMsg(body))
	}
	if code, _ := approveAs(t, s, id, ana); code == http.StatusSeeOther {
		t.Error("approved while the boundaries can't be read")
	}
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/drop", bo, "same-origin", url.Values{}); code != http.StatusSeeOther {
		t.Errorf("dropping it: %d", code)
	}
}

// A team's changes_in within --changes-in, in the letter case its own
// source compares names in: an Azure alarm's in any, beside a ruler that
// isn't Azure's; a CloudWatch alarm's as written, beside an Azure ruler.
func TestWithinChangesInFoldsBySource(t *testing.T) {
	parallel(t)
	alarms := []*source.Alarms{{Source: "cw", Kind: source.AlarmsAWS}, {Source: "az", Kind: source.AlarmsAzure}}
	for _, c := range []struct {
		ruler string
		x     string
		in    bool
	}{
		{source.RulerConfig, "az:payments-high", true},
		{source.RulerConfig, "az:PAY*", true},
		{source.RulerConfig, "cw:payments", false},
		{source.RulerConfig, "cw:Payments", true},
		{source.RulerAzure, "cw:payments", false},
		{source.RulerAzure, "cw:PayDown", true},
		{source.RulerAzure, "rg/shop-api", true},
		{source.RulerConfig, "rg/shop-api", false},
		{source.RulerConfig, "rg/Shop-api", true},
		// Another source's entry is no source's but its own.
		{source.RulerConfig, "az2:Pay", false},
	} {
		s := &Server{ruler: &source.Ruler{Kind: c.ruler}, alarms: alarms}
		s.cfg.ChangesIn = []string{"cw:Pay*", "az:Pay*", "rg/Shop*"}
		if got := s.withinChangesIn(c.x); got != c.in {
			t.Errorf("%s beside a ruler of kind %s: %v, want %v", c.x, c.ruler, got, c.in)
		}
	}
}

// proposeAddAs proposes a rule to add into a group of a rule file, as who:
// errors > 1 under name, with labels (YAML lines under labels:, or none),
// held to a budget it meets.
func proposeAddAs(t *testing.T, s *Server, who, file, group, name, labels string) (int64, int, string) {
	t.Helper()
	rule := "alert: " + name + "\nexpr: errors > 1\nfor: 2m\n"
	if labels != "" {
		rule += "labels:\n" + labels
	}
	key := placeKeyOf(t, s, addPlace{Path: ledger.PathGit, File: file, Group: group})
	return proposeAdding(t, s.Handler(), who, url.Values{"place": {key}, "rule": {rule}, "expectations": {strings.Replace(heldTo(60), "ErrorsLow", name, 1)}})
}

// A rule added is held by the boundaries of the team its name and labels
// give it, as it will have them in effect: checkout's label makes it
// checkout's, which payments's approver doesn't approve, a freeze of
// checkout's holds and checkout's approver does; added in pay's group,
// whose labels are payments's, one with no label of its own is payments's.
func TestTeamBoundsAddByLabels(t *testing.T) {
	parallel(t)
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	s, _ := boundsServer(t, teamsConfig, nil)
	s.wall = func() time.Time { return at }
	h := s.Handler()
	cart, code, body := proposeAddAs(t, s, bo, "shop.yaml", "shop", "CartErrors", "  team: checkout\n")
	if cart == 0 {
		t.Fatalf("proposing checkout's: %d %s", code, zzMsg(body))
	}
	if w := proposalEvidence(state(t, s, cart)).ownedBy; !w.Owned || len(w.Owners) != 1 || w.Owners[0].Name != "checkout" {
		t.Errorf("kept as owned by %+v", w)
	}
	if code, _ := approveAs(t, s, cart, quinn); code != http.StatusForbidden {
		t.Errorf("quinn approving checkout's: %d, want 403", code)
	}
	pay, code, body := proposeAddAs(t, s, bo, "pay.yaml", "pay", "PayErrors", "")
	if pay == 0 {
		t.Fatalf("proposing in pay's group: %d %s", code, zzMsg(body))
	}
	if w := proposalEvidence(state(t, s, pay)).ownedBy; len(w.Owners) != 1 || w.Owners[0].Name != "payments" {
		t.Errorf("by its group's labels, owned by %+v", w)
	}
	if code, _ := approveAs(t, s, pay, rob); code != http.StatusForbidden {
		t.Errorf("rob approving payments's: %d, want 403", code)
	}
	declare(t, s, teamsConfig+"freezes:\n  - {title: Cart week, team: checkout, start: 2026-11-27T00:00, end: 2026-11-30T00:00}\n")
	if code, body := approveAs(t, s, cart, rob); code != http.StatusConflict || !strings.Contains(body, "Cart week") {
		t.Errorf("rob approving during checkout's freeze: %d %s", code, zzMsg(body))
	}
	if code, body := approve(t, h, pay, quinn); code != http.StatusSeeOther {
		t.Errorf("quinn approving payments's during checkout's freeze: %d %s", code, zzMsg(body))
	}
	declare(t, s, teamsConfig)
	if code, body := approve(t, h, cart, rob); code != http.StatusSeeOther {
		t.Errorf("rob approving checkout's: %d %s", code, zzMsg(body))
	}
	// One whose labels can't be read is held, and refused: its team can't
	// be told.
	ctx := context.Background()
	unread, err := s.l.Propose(ctx, ledger.Proposal{Rule: "Unread", Kind: ledger.KindAdd, Path: ledger.PathGit, MadeBy: "u@example.com", AfterHash: "h",
		Expect: `{"text":"expectations:\n  - rule: Unread\n    budget:\n      week: 5\n"}`, Evidence: []byte(`{"add": {"rule": "x"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.Approve(ctx, unread, ana, 1, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if held, err := s.heldByBounds(ctx, state(t, s, unread)); !held || err != nil {
		t.Errorf("a rule added whose labels can't be read: held %v, %v", held, err)
	}
	if p := state(t, s, unread); p.State != ledger.StateRefused {
		t.Errorf("a rule added whose labels can't be read: %s %q", p.State, p.Reason)
	}
}

// A team's editor adds rules their team would own by the name and labels
// given, and by the labels of the group it goes in: not one another
// team's, nor no team's, refused before the rules are read again (no try
// spent); and not one whose group's labels make it another team's, refused
// once it is read back there.
func TestTeamEditorAdds(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, dir := boundsServer(t, teamsConfig, nil)
	// checkout owns what has shop=cart, as the basket group does.
	os.WriteFile(filepath.Join(dir, "basket.yaml"), []byte("groups:\n  - name: basket\n    labels: {shop: cart}\n    rules:\n      - alert: BasketSlow\n        expr: up == 0\n"), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	declare(t, s, strings.Replace(teamsConfig, "labels: {team: checkout}", "labels: {shop: cart}", 1))
	tried := len(s.tries.at[triesKey(pat, false)])
	for name, labels := range map[string]string{"CartErrors": "  shop: cart\n", "AnyErrors": ""} {
		if _, code, body := proposeAddAs(t, s, pat, "shop.yaml", "shop", name, labels); code != http.StatusForbidden {
			t.Errorf("pat adding %s: %d %s, want 403", name, code, zzMsg(body))
		}
	}
	if n := len(s.tries.at[triesKey(pat, false)]); n != tried {
		t.Errorf("a refused team's editor spent %d tries reading the rules again", n-tried)
	}
	id, code, body := proposeAddAs(t, s, pat, "shop.yaml", "shop", "PayErrors", "  team: payments\n")
	if id == 0 {
		t.Fatalf("pat adding payments's: %d %s", code, zzMsg(body))
	}
	if w := proposalEvidence(state(t, s, id)).ownedBy; len(w.Owners) != 1 || w.Owners[0].Name != "payments" {
		t.Errorf("kept as owned by %+v", w)
	}
	if _, code, body := proposeAddAs(t, s, pat, "basket.yaml", "basket", "BasketErrors", "  team: payments\n"); code != http.StatusForbidden {
		t.Errorf("pat adding to checkout's group: %d %s, want 403", code, zzMsg(body))
	}
}

// A rule added, backtested again after its group's labels made it another
// team's, is that team's: its proposal, payments's as it was proposed, is
// approved by no one, and is proposed again.
func TestTeamAddMovedByItsGroup(t *testing.T) {
	ctx := context.Background()
	s, dir := boundsServer(t, teamsConfig, nil)
	id, code, body := proposeAddAs(t, s, bo, "pay.yaml", "pay", "PayErrors", "")
	if id == 0 {
		t.Fatalf("proposing: %d %s", code, zzMsg(body))
	}
	os.WriteFile(filepath.Join(dir, "pay.yaml"), []byte(strings.Replace(payRules, "{team: payments}", "{team: checkout}", 1)), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	was := semantics
	semantics++
	t.Cleanup(func() { semantics = was })
	if err := s.backtestStale(ctx); err != nil {
		t.Fatal(err)
	}
	if now, err := s.teamNow(ctx, state(t, s, id)); err != nil || now.Name != "checkout" {
		t.Fatalf("backtested again, its team: %+v %v", now, err)
	}
	if code, body := approveAs(t, s, id, quinn); code == http.StatusSeeOther || !strings.Contains(body, "propose it again") {
		t.Errorf("quinn approving what is checkout's now: %d %s", code, zzMsg(body))
	}
}

// Accepting a draft asks whether its person may approve its rule now:
// a team's approver, of the team that owns it, or of every team. The
// form's key, which says who saw the page, lets in no one an approver of
// another team: one demoted, or who saw the form before.
func TestDraftAcceptedByItsTeamsApprover(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	s, _ := boundsServer(t, teamsConfig, nil)
	h := s.Handler()
	id, err := s.l.KeepDraft(ctx, ledger.ExpectDraft{Rule: "PayDown", Origin: ledger.DraftAgent, MadeBy: pat, Agent: true,
		Text: "expectations:\n  - rule: PayDown\n    budget: {week: 10}\n", Target: ledger.DraftToNone})
	if err != nil {
		t.Fatal(err)
	}
	accept := func(who string) int {
		d, err := s.l.DraftByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		code, _, _ := ui(t, h, http.MethodPost, "/expectations/drafts/"+itoa(id)+"/accept", who, "same-origin", url.Values{"key": {s.draftFormKey(d, who)}})
		return code
	}
	if code := accept("rob@example.com"); code != http.StatusForbidden {
		t.Errorf("checkout's approver accepted payments' draft: %d", code)
	}
	if d, _ := s.l.DraftByID(ctx, id); d.State != ledger.DraftOpen {
		t.Fatalf("accepted by another team's approver: %+v", d)
	}
	if code := accept("quinn@example.com"); code != http.StatusSeeOther {
		t.Errorf("payments' approver: %d", code)
	}
	if d, _ := s.l.DraftByID(ctx, id); d.State != ledger.DraftAccepted || d.DecidedBy != "quinn@example.com" {
		t.Errorf("accepted: %+v", d)
	}
}
