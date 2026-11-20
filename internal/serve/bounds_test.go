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
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
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
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "PayDown", TeamID: checkout.ID}, ana); err != nil {
		t.Fatal(err)
	}
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "CartSlow", TeamID: payments.ID}, ana); err != nil {
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
