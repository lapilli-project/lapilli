package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// bodyOf is a proposal's pull request's description, with what its
// evidence says now, as its page and the recipes are given it.
func bodyOf(t *testing.T, s *Server, p ledger.Proposal) string {
	t.Helper()
	ls, err := s.linesNow(context.Background(), []ledger.Proposal{p})
	if err != nil {
		t.Fatal(err)
	}
	return s.prBody(ls[0])
}

// restartsFile is the restart rule of 0.41.0's deployment: a condition that
// counts events over 30m, its for a minute.
const restartsFile = `groups:
  - name: pods
    rules:
      - alert: RestartsNow
        expr: increase(restarts[30m]) > 0.5
        for: 1m
        labels: {severity: page}
`

// unrecordedFrom and unrecordedTo are a period with no incident in it or in
// the two weeks before it, long before the test servers' INC-1.
var (
	unrecordedFrom = time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	unrecordedTo   = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
)

// keptAs keeps a proposal of RestartsNow's for, 1m → 1h, with evidence as
// another lapilli kept it: semantics and version (0 and "" for one before
// they were kept), and its backtest over [from, to), 141 firings → 3,
// none catching anything, noRecord as it kept it (nil, not kept). It keeps
// no for_outlasts, as 0.40 didn't.
func keptAs(t *testing.T, s *Server, semantics int, version string, from, to time.Time, noRecord *bool) int64 {
	t.Helper()
	return keptFor(t, s, "RestartsNow", semantics, version, from, to, noRecord)
}

// keptFor is keptAs of another rule of the pods group, by its name.
func keptFor(t *testing.T, s *Server, rule string, semantics int, version string, from, to time.Time, noRecord *bool) int64 {
	t.Helper()
	ctx := context.Background()
	f, err := s.current(ctx, rule, false)
	if err != nil {
		t.Fatal(err)
	}
	hour := time.Hour
	want := f.rule
	want.For = hour
	ev := evidence{From: "suggestion", Edit: &rules.Edit{Name: rule, Group: "pods", For: &hour}, For: hour, Semantics: semantics, Version: version,
		Backtest: &backtest{From: from, To: to, Before: side{Firings: 141}, After: side{Firings: 3}, Lost: []string{}, Gained: []string{}, Later: []string{}, NoRecord: noRecord}}
	raw, _ := json.Marshal(ev)
	id, err := s.l.Propose(ctx, ledger.Proposal{Rule: rule, Kind: ledger.KindFor, Path: ledger.PathGit, MadeBy: ledger.Proposer,
		Source: f.doc.Source, DocHash: rules.Hash(f.doc.Data), Before: ruleText(f.rule), BeforeHash: ruleHash(f.rule),
		After: ruleText(want), AfterHash: ruleHash(want), Diff: rules.Diff("restarts.yaml", []byte(ruleText(f.rule)), []byte(ruleText(want))), Evidence: raw})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// restartsServer is a changes server with the restart rule beside Errors.
func restartsServer(t *testing.T, more func(*Config)) (*Server, string) {
	t.Helper()
	s, dir := changesServer(t, more)
	if err := os.WriteFile(filepath.Join(dir, "restarts.yaml"), []byte(restartsFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.look(context.Background(), new(string)); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func apiProposalOf(t *testing.T, h http.Handler, id int64) map[string]any {
	t.Helper()
	code, body, _ := ui(t, h, http.MethodGet, "/api/v1/proposals/"+itoa(id), bo, "", nil)
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); code != 200 || err != nil {
		t.Fatalf("the API: %d %v %s", code, err, body)
	}
	return out
}

// TestOldProposalReadNow: a proposal 0.40 made of the restart rule's for,
// 1m → 1h, kept no for_outlasts, no version and no record of its period,
// which had no incident written down. Read now, its page, the proposals
// list, the API, its pull request's description and MCP say its for
// outlasts one event; its backtest counts its firings, 141 → 3, and calls
// that neither better nor worse, with "–" for what it caught or missed;
// and it is stale, its approval held until it is backtested again, on the
// page and by the server.
func TestOldProposalReadNow(t *testing.T) {
	ctx := context.Background()
	s, _ := restartsServer(t, nil)
	h := s.Handler()
	id := keptAs(t, s, 0, "", unrecordedFrom, unrecordedTo, nil)
	page := func(who string) string {
		t.Helper()
		code, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), who, "", nil)
		if code != 200 {
			t.Fatalf("its page: %d", code)
		}
		return html.UnescapeString(body)
	}
	body := page(cy)
	outlast := "As proposed, its for (1h) is as long as the 30m its condition counts over, or longer: one event alone could never fire it; only events that keep coming for 1h would."
	noRecord := "No incident was written down, or found by --impact, in the period or the two weeks before it: the firings are counted, not judged. Fewer firings aren't better, nor more worse: fewer may be events that happened, hidden."
	stale := "It is stale, so it isn't approved until it is backtested again: it was backtested by an earlier lapilli, before backtests were numbered by how they replay and judge."
	for _, want := range []string{outlast, noRecord, stale, offButton("Approve", "approve-why", "Held until it is backtested again: it is stale"),
		`<tr><th scope="row">Fired</th><td class="n">141</td><td class="n">3</td><td class="n"> <span class="delta">−138</span></td></tr>`,
		`<tr><th scope="row">Caught something</th><td class="n">–</td><td class="n">–</td><td class="n">–</td></tr>`} {
		if !strings.Contains(body, want) {
			t.Errorf("its page doesn't say %q", want)
		}
	}
	if i := strings.Index(body, `id="backtest"`); i < 0 || strings.Contains(body[i:strings.Index(body, `id="approval"`)], "delta better") ||
		strings.Contains(body[i:strings.Index(body, `id="approval"`)], "delta worse") {
		t.Error("its backtest calls fewer firings better")
	}
	if keyFor(t, h, id, cy, "approve") != "" {
		t.Error("a stale proposal can be approved from its page")
	}
	// An editor is offered to backtest it again; a viewer sees why not.
	if !strings.Contains(page(bo), `action="/proposals/`+itoa(id)+`/backtest"`) {
		t.Error("an editor isn't offered to backtest it again")
	}
	// An approval with a key from before it was found stale is refused.
	p := state(t, s, id)
	code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/approve", cy, "same-origin", url.Values{"key": {s.formKey(p, cy, "approve")}, "read": {"1"}})
	if p = state(t, s, id); code != http.StatusBadRequest || p.State != ledger.StateProposed || len(p.Approvals) != 0 {
		t.Errorf("approving it: %d, %s with %d approvals", code, p.State, len(p.Approvals))
	}
	// The list: a badge, and the for that outlasts one event.
	_, rows, _ := ui(t, h, http.MethodGet, "/proposals", cy, "", nil)
	for _, want := range []string{"stale: backtest it again", "as proposed, one event alone can&#39;t fire it"} {
		if !strings.Contains(rows, want) {
			t.Errorf("the list doesn't say %q", want)
		}
	}
	// The API, one and all.
	one := apiProposalOf(t, h, id)
	if one["stale"] != staleNoVersion || one["no_record"] != true || mustJSON(one["for_outlasts"]) != `{"for_ns":3600000000000,"window_ns":1800000000000}` {
		t.Errorf("the API: stale %v, no_record %v, for_outlasts %v", one["stale"], one["no_record"], one["for_outlasts"])
	}
	_, all, _ := ui(t, h, http.MethodGet, "/api/v1/proposals", bo, "", nil)
	var list struct{ Proposals []map[string]any }
	if json.Unmarshal([]byte(all), &list); len(list.Proposals) != 1 || list.Proposals[0]["stale"] != staleNoVersion || list.Proposals[0]["no_record"] != true {
		t.Errorf("the API's list: %s", all)
	}
	// Its pull request's description: counted, not judged, as --compare
	// says of such a period, and what its for means.
	pr := bodyOf(t, s, p)
	if !strings.Contains(pr, "fired 141 times as it is, 3 as proposed. "+verdict.NoRecordCompared+" "+noRecordFewer) || strings.Contains(pr, "catching an incident") ||
		!strings.Contains(pr, outlast) {
		t.Errorf("its description:\n%s", pr)
	}
	// MCP.
	_, out, err := s.toolRule(ctx, nil, ruleIn{Name: "RestartsNow"})
	if err != nil || len(out.Proposals) != 1 {
		t.Fatalf("MCP: %+v %v", out.Proposals, err)
	}
	x := out.Proposals[0]
	if x.Stale != staleNoVersion || x.Outlasts != outlast || x.Backtest == nil || !x.Backtest.NoRecord || x.Backtest.Caught != nil || x.Backtest.Fired != [2]int{141, 3} {
		t.Errorf("MCP's proposal: %+v %+v", x, x.Backtest)
	}
}

// TestBacktestAgain: an editor backtests a stale proposal again, with this
// lapilli, against the rule in effect, over the twelve weeks to now. Its
// evidence is replaced, with this version and whether its period had a
// record; the old backtest is in its history, with who backtested it
// again; the approval it had, given to the old one, is taken back; and it
// is no longer stale, so it is approved. A fresh one isn't backtested
// again.
func TestBacktestAgain(t *testing.T) {
	ctx := context.Background()
	s, _ := restartsServer(t, func(c *Config) { c.Approvals = 2 })
	h := s.Handler()
	id := keptAs(t, s, 0, "", unrecordedFrom, unrecordedTo, nil)
	// Approved once before it was found stale, as before an upgrade.
	if _, err := s.l.Approve(ctx, id, ana, 2, time.Time{}); err != nil {
		t.Fatal(err)
	}
	code, body, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/backtest", bo, "same-origin", url.Values{})
	if code != http.StatusSeeOther || hd.Get("Location") != "/proposals/"+itoa(id)+"#backtest" {
		t.Fatalf("backtesting it again: %d %s %v", code, body, hd)
	}
	p := state(t, s, id)
	ev := proposalEvidence(p)
	if p.State != ledger.StateProposed || len(p.Approvals) != 0 || ev.Version != "test" || ev.Backtest == nil || ev.Backtest.NoRecord == nil ||
		!ev.Backtest.To.After(unrecordedTo) || ev.Outlasts == nil || ev.Outlasts.Window != 30*time.Minute || ev.From != "suggestion" {
		t.Fatalf("backtested again: %+v %s", p, p.Evidence)
	}
	// Its history keeps the old backtest, and who ran it again.
	_, page, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), cy, "", nil)
	page = html.UnescapeString(page)
	for _, want := range []string{"Backtested again by <bdi>bo@example.com</bdi>, with lapilli <bdi>test</bdi>",
		"The backtest it replaced, from 2026-07-06 to 2026-09-28: fired 141 times as it was, 3 as proposed.",
		"Approvals withdrawn, given to the backtest it replaced: <bdi>ana@example.com</bdi>. Approve it again."} {
		if !strings.Contains(page, want) {
			t.Errorf("its history doesn't say %q", want)
		}
	}
	if strings.Contains(page, "It is stale") {
		t.Error("still stale once backtested again")
	}
	// Fresh, it is approved, by two.
	if code, body := approve(t, h, id, ana); code != http.StatusSeeOther {
		t.Fatalf("approving: %d %s", code, body)
	}
	// A fresh one isn't backtested again, and the page offers it no form.
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/backtest", bo, "same-origin", url.Values{}); code != http.StatusBadRequest {
		t.Errorf("a fresh proposal backtested again: %d", code)
	}
	if _, page, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), bo, "", nil); strings.Contains(page, "/backtest\"") {
		t.Error("a fresh proposal offers to be backtested again")
	}
}

// TestBacktestAgainRefused: backtesting again is an editor's, from this
// site, as proposing is; never a viewer's, anyone's who isn't signed in,
// another site's, or a read-only server's; counted against the reader's
// tries an hour; and refused when the rule changed since, which the page
// says, as the API does.
func TestBacktestAgainRefused(t *testing.T) {
	ctx := context.Background()
	s, dir := restartsServer(t, func(c *Config) { c.Editors = []string{bo} })
	h := s.Handler()
	id := keptAs(t, s, 0, "", unrecordedFrom, unrecordedTo, nil)
	path := "/proposals/" + itoa(id) + "/backtest"
	for name, c := range map[string]struct {
		who, site string
		want      int
	}{
		"a viewer":          {"viewer@example.com", "same-origin", http.StatusForbidden},
		"no one signed in":  {"", "same-origin", http.StatusForbidden},
		"from another site": {bo, "cross-site", http.StatusForbidden},
	} {
		if code, _, _ := ui(t, h, http.MethodPost, path, c.who, c.site, url.Values{}); code != c.want {
			t.Errorf("%s: %d, want %d", name, code, c.want)
		}
	}
	if _, page, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), "viewer@example.com", "", nil); !strings.Contains(page, offButton("Backtest again", "again-why", "Editors only")) {
		t.Error("a viewer isn't shown why they can't backtest it again")
	}
	// The reader's tries an hour, as proposing counts them.
	for range triesAnHour {
		s.tries.take(bo, false, time.Now())
	}
	if code, _, _ := ui(t, h, http.MethodPost, path, bo, "same-origin", url.Values{}); code != http.StatusTooManyRequests {
		t.Errorf("past the tries an hour: %d", code)
	}
	s.tries = tries{}
	// Read-only: no one's.
	s.cfg.ReadOnly = true
	if code, _, _ := ui(t, h, http.MethodPost, path, bo, "same-origin", url.Values{}); code != http.StatusForbidden {
		t.Errorf("read-only: %d", code)
	}
	s.cfg.ReadOnly = false
	if p := state(t, s, id); proposalEvidence(p).Version != "" {
		t.Fatalf("backtested again by one refused: %s", p.Evidence)
	}
	// The rule changed since: refused, and said so, its approval held.
	if err := os.WriteFile(filepath.Join(dir, "restarts.yaml"), []byte(strings.Replace(restartsFile, "> 0.5", "> 1.5", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	code, body, _ := ui(t, h, http.MethodPost, path, bo, "same-origin", url.Values{})
	if code != http.StatusBadRequest || !strings.Contains(html.UnescapeString(body), "the rule changed since it was proposed, so the proposal no longer applies to the rule as it is") {
		t.Errorf("the rule changed since: %d %s", code, body)
	}
	_, page, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), ana, "", nil)
	if !strings.Contains(html.UnescapeString(page), "it changed since, so this is refused when checked again. Propose it again from the rule's page.") {
		t.Error("its page doesn't say the rule changed since")
	}
	if one := apiProposalOf(t, h, id); one["stale"] != staleRule {
		t.Errorf("the API: %v", one["stale"])
	}
	// The rule changed and nothing else: refused, as it says, not as
	// fresh.
	if p := state(t, s, id); proposalEvidence(p).Version != "" {
		t.Fatalf("backtested again: %s", p.Evidence)
	}
	s.l.Move(ctx, id, []string{ledger.StateProposed}, ledger.StateDropped, "ana", ledger.Step{})
	os.WriteFile(filepath.Join(dir, "restarts.yaml"), []byte(restartsFile), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	fresh := keptAs(t, s, verdict.Semantics, "test", unrecordedFrom, unrecordedTo, nil)
	os.WriteFile(filepath.Join(dir, "restarts.yaml"), []byte(strings.Replace(restartsFile, "> 0.5", "> 1.5", 1)), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(fresh)+"/backtest", bo, "same-origin", url.Values{}); code != http.StatusBadRequest ||
		!strings.Contains(html.UnescapeString(body), "the rule changed since it was proposed") {
		t.Errorf("only the rule changed since: %d %s", code, body)
	}
}

// TestStaleWhy: a proposal waiting for approval is stale when the lapilli
// that made it is of another minor version, or when its period's record
// changed since: an incident written down in it, or none keeping it any
// more. One of the same minor version, of its period's record as it is,
// isn't; nor is one approved, or one with no backtest.
func TestStaleWhy(t *testing.T) {
	ctx := context.Background()
	s, _ := restartsServer(t, nil)
	why := func(id int64) string {
		t.Helper()
		ls, err := s.linesNow(ctx, []ledger.Proposal{state(t, s, id)})
		if err != nil {
			t.Fatal(err)
		}
		if ls[0].StaleWhy == nil {
			return ""
		}
		return ls[0].StaleWhy.Text("en")
	}
	yes, no := true, false
	drop := func(id int64) {
		s.l.Move(ctx, id, []string{ledger.StateProposed}, ledger.StateDropped, "ana", ledger.Step{})
	}
	s.cfg.Version = "v0.42.0"
	id := keptAs(t, s, verdict.Semantics+1, "v0.43.0", unrecordedFrom, unrecordedTo, &yes)
	if got := why(id); got != "it was backtested by lapilli v0.43.0, which replays or judges otherwise than this one" {
		t.Errorf("another number for how it backtests: %q", got)
	}
	drop(id)
	// Another version that replays and judges alike: not stale, whatever
	// its number, as an upgrade that changes neither leaves it.
	id = keptAs(t, s, verdict.Semantics, "v0.40.1", unrecordedFrom, unrecordedTo, &yes)
	if got := why(id); got != "" {
		t.Errorf("another version, backtesting alike: %q", got)
	}
	// An incident written down in its period since.
	if _, err := s.l.Put(ctx, incident.Incident{ID: "INC-8", Title: "Written down late", Start: unrecordedFrom.Add(30 * 24 * time.Hour),
		End: unrecordedFrom.Add(30*24*time.Hour + time.Hour)}, "ana", s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if got := why(id); got != staleRecorded {
		t.Errorf("an incident written down since: %q", got)
	}
	drop(id)
	// Kept then, none keeping it now.
	id = keptAs(t, s, verdict.Semantics, "v0.41.0", unrecordedFrom.AddDate(0, 0, -100), unrecordedFrom.AddDate(0, 0, -16), &no)
	if got := why(id); got != staleUnrecorded {
		t.Errorf("no record any more: %q", got)
	}
	// Approved, it isn't held: it is backtested again when carried out.
	s.l.Move(ctx, id, []string{ledger.StateProposed}, ledger.StateApproved, "ana", ledger.Step{})
	if got := why(id); got != "" {
		t.Errorf("approved: %q", got)
	}
}

// TestStaleNotByVersion: a proposal made by a lapilli of no version, or of
// one that backtests as this one does, isn't stale: what decides is how it
// was backtested (verdict.Semantics), the version kept only to be shown,
// as dev when there is none. The demo's proposal, made by one of no
// version, isn't either.
func TestStaleNotByVersion(t *testing.T) {
	ctx := context.Background()
	s, _ := restartsServer(t, func(c *Config) { c.Version = "" })
	id, err := s.propose(ctx, ask{Rule: "RestartsNow", Kind: ledger.KindFor, For: 10 * time.Minute, From: "person"}, bo, false, true)
	if err != nil {
		t.Fatal(err)
	}
	p := state(t, s, id)
	ls, err := s.linesNow(ctx, []ledger.Proposal{p})
	if ev := proposalEvidence(p); err != nil || ls[0].StaleWhy != nil || ev.Version != "dev" || ev.Semantics != verdict.Semantics {
		t.Errorf("made with no version: %v %+v %s", err, ls[0].StaleWhy, p.Evidence)
	}
	s.cfg.Version = "v9.9.9"
	if ls, _ := s.linesNow(ctx, []ledger.Proposal{p}); ls[0].StaleWhy != nil {
		t.Errorf("upgraded, backtesting alike: %+v", ls[0].StaleWhy)
	}
	d := demoServer(t)
	ps, err := d.l.Proposals(ctx, ledger.ProposalQuery{States: []string{ledger.StateProposed}})
	if err != nil || len(ps) == 0 {
		t.Fatalf("the demo's proposals: %v %v", ps, err)
	}
	if ls, err := d.linesNow(ctx, ps); err != nil || ls[0].StaleWhy != nil || ls[0].Stale {
		t.Errorf("the demo's proposal is stale: %v %+v", err, ls[0].StaleWhy)
	}
}

// TestProposalsStaleFilter: stale=1 lists the proposals waiting for
// approval that are stale alone, counted, with a chip that takes it away,
// carried by the form and the other links; with none, says so.
func TestProposalsStaleFilter(t *testing.T) {
	s, _ := restartsServer(t, nil)
	h := s.Handler()
	fresh := proposeFor(t, h, "15m")
	old := keptAs(t, s, 0, "", unrecordedFrom, unrecordedTo, nil)
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals", cy, "", nil); !strings.Contains(body, `<a href="/proposals?stale=1">Show the stale ones alone</a>`) {
		t.Error("the list doesn't link to its stale ones")
	}
	_, body, _ := ui(t, h, http.MethodGet, "/proposals?stale=1", cy, "", nil)
	if !strings.Contains(body, `href="/proposals/`+itoa(old)+`"`) || strings.Contains(body, `href="/proposals/`+itoa(fresh)+`"`) {
		t.Errorf("stale=1 lists #%d alone", old)
	}
	for _, want := range []string{"1 of 1 proposal", `<input type="hidden" name="stale" value="1">`, `Proposals: stale`} {
		if !strings.Contains(html.UnescapeString(body), want) {
			t.Errorf("stale=1 doesn't have %q", want)
		}
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals?stale=1&kind=threshold", cy, "", nil); !strings.Contains(body, "No proposal matches these filters.") {
		t.Error("stale=1 with another kind")
	}
	s.l.Move(context.Background(), old, []string{ledger.StateProposed}, ledger.StateDropped, "ana", ledger.Step{})
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals?stale=1", cy, "", nil); !strings.Contains(body, "No proposal waiting for approval is stale.") {
		t.Error("stale=1 with none stale doesn't say so")
	}
}

// TestBacktestAgainByAgent: an agent backtests a stale proposal again only
// where --mcp-propose lets it propose, for the person it acts for, its
// tries counted apart from theirs; without it, there is no such tool.
func TestBacktestAgainByAgent(t *testing.T) {
	s, _ := restartsServer(t, func(c *Config) { c.MCPPropose = "editors" })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	id := keptAs(t, s, 0, "", unrecordedFrom, unrecordedTo, nil)
	cs := connect(t, ts.URL, bo)
	// Its tries used up: refused, the person's own left as they were.
	for range triesAnHour {
		s.tries.take(bo, true, time.Now())
	}
	var out backtestAgainOut
	if failed, text := call(t, cs, "backtest_again", map[string]any{"id": id}, &out); !failed || !strings.Contains(text, "as many proposals in the last hour") {
		t.Errorf("past its tries: %v %s", failed, text)
	}
	if !s.tries.may(bo, false, time.Now()) {
		t.Error("an agent's tries counted against the person's")
	}
	s.tries = tries{}
	if failed, text := call(t, cs, "backtest_again", map[string]any{"id": id}, &out); failed {
		t.Fatalf("backtest_again: %s", text)
	}
	p := state(t, s, id)
	if proposalEvidence(p).Version != "test" {
		t.Errorf("not backtested again: %s", p.Evidence)
	}
	steps, _, _ := s.l.ProposalSteps(context.Background(), id, 10)
	if len(steps) == 0 || steps[len(steps)-1].By != bo || !strings.HasSuffix(steps[len(steps)-1].What, "backtested again") ||
		!strings.Contains(steps[len(steps)-1].After, `"agent":true`) {
		t.Errorf("its history: %+v", steps)
	}
	// An agent's, for whom, on its page and through MCP.
	if _, page, _ := ui(t, s.Handler(), http.MethodGet, "/proposals/"+itoa(id), cy, "", nil); !strings.Contains(html.UnescapeString(page),
		"Backtested again by an agent, for <bdi>bo@example.com</bdi>, with lapilli <bdi>test</bdi>") {
		t.Error("its page doesn't say an agent backtested it again")
	}
	var rule ruleDetailOut
	if failed, text := call(t, cs, "rule", map[string]any{"name": "RestartsNow"}, &rule); failed || len(rule.Proposals) != 1 || rule.Proposals[0].Again == nil ||
		!rule.Proposals[0].Again.Agent || rule.Proposals[0].Again.By != bo {
		t.Errorf("MCP's rule: %s %+v", text, rule.Proposals)
	}
	// A fresh one: refused.
	if failed, _ := call(t, cs, "backtest_again", map[string]any{"id": id}, &out); !failed {
		t.Error("a fresh one backtested again")
	}
	// Without --mcp-propose, or for a role it doesn't give, no tool.
	for _, c := range []struct {
		more func(*Config)
		who  string
	}{{nil, ana}, {func(c *Config) { c.MCPPropose = "admins" }, bo}} {
		s2, _ := restartsServer(t, c.more)
		ts2 := httptest.NewServer(s2.Handler())
		defer ts2.Close()
		tools, err := connect(t, ts2.URL, c.who).ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range tools.Tools {
			if x.Name == "backtest_again" {
				t.Errorf("backtest_again for %s with --mcp-propose %q", c.who, s2.cfg.MCPPropose)
			}
		}
	}
}

// TestOutlastsReadOnce: what a proposal's for proposes is worked out once
// while serve runs: a rule found only in the version in effect when it was
// made is read once, not on every list.
func TestOutlastsReadOnce(t *testing.T) {
	ctx := context.Background()
	s, dir := restartsServer(t, nil)
	id := keptAs(t, s, 0, "", unrecordedFrom, unrecordedTo, nil)
	// Made when the version it was made against was in effect, as the
	// tests' clock has it.
	ledgerExec(t, s, "UPDATE proposals SET made = ? WHERE id = ?", now.Unix(), id)
	// The rule changed since: the rule it was made against is only in the
	// version before.
	os.WriteFile(filepath.Join(dir, "restarts.yaml"), []byte(strings.Replace(restartsFile, "for: 1m", "for: 2m", 1)), 0o644)
	s.now = func() time.Time { return now.Add(time.Minute) }
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	p := state(t, s, id)
	for i := 0; i < 3; i++ {
		before := namedReads.Load()
		ls, err := s.linesNow(ctx, []ledger.Proposal{p})
		if err != nil {
			t.Fatal(err)
		}
		if ls[0].Outlasts == nil || ls[0].Outlasts.Window != 30*time.Minute {
			t.Fatalf("read %d: %+v", i, ls[0].Outlasts)
		}
		// The latest, for whether it is stale; the version it was made
		// against, the first time alone.
		if n, most := namedReads.Load()-before, map[bool]int64{true: 2, false: 1}[i == 0]; n > most {
			t.Errorf("read %d: %d versions parsed, want %d at most", i, n, most)
		}
	}
}

// TestStalePagesTranslated: the pages a stale proposal, one backtested
// again and the stale filter are shown on are in every language, and
// accessible, as an approver, an editor and an admin see them.
func TestStalePagesTranslated(t *testing.T) {
	ctx := context.Background()
	s, dir := restartsServer(t, func(c *Config) { c.Approvals = 2 })
	h := s.Handler()
	stale := keptAs(t, s, 0, "", unrecordedFrom, unrecordedTo, nil)
	s.l.Approve(ctx, stale, ana, 2, time.Time{})
	if code, body, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(stale)+"/backtest", bo, "same-origin", url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("backtesting again: %d %s", code, body)
	}
	// Backtested again, then stale by the rule.
	again := stale
	s.l.Move(ctx, again, []string{ledger.StateProposed}, ledger.StateDropped, "ana", ledger.Step{})
	other := keptAs(t, s, verdict.Semantics+1, "v0.40.1", unrecordedFrom, unrecordedTo, nil)
	paths := []string{"/proposals", "/proposals?stale=1", "/proposals?state=all", "/proposals/" + itoa(again), "/proposals/" + itoa(other), "/rules/RestartsNow?tab=proposals"}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	for _, who := range []string{ana, bo, cy} {
		pageA11y(t, h, who, paths...)
	}
	// One whose rule changed since.
	s.l.Move(ctx, other, []string{ledger.StateProposed}, ledger.StateDropped, "ana", ledger.Step{})
	changed := keptAs(t, s, 0, "", unrecordedFrom, unrecordedTo, nil)
	os.WriteFile(filepath.Join(dir, "restarts.yaml"), []byte(strings.Replace(restartsFile, "> 0.5", "> 1.5", 1)), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{ana, bo, cy} {
		pageA11y(t, h, who, "/proposals/"+itoa(changed), "/proposals?stale=1")
	}
	for _, x := range []string{staleRule, staleNoVersion, staleVersion, staleRecorded, staleUnrecorded} {
		for lang := range catalogs {
			tr(lang, x)
		}
	}
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
}

// TestOutlastsFinishedKept: a proposal finished with (applied, dropped,
// refused, failed) says the for_outlasts it kept, and no version is read
// for it: its rule is as it was, or gone, and its page records what was.
func TestOutlastsFinishedKept(t *testing.T) {
	ctx := context.Background()
	s, dir := restartsServer(t, nil)
	ids := againstVersions(t, s, dir, 3, 5, ledger.StateApplied)
	// One kept none, though its for outlasts one event: it says none.
	var ps []ledger.Proposal
	for _, id := range ids {
		ps = append(ps, state(t, s, id))
	}
	before := namedReads.Load()
	ls, err := s.linesNow(ctx, ps)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		if l.Outlasts != nil {
			t.Errorf("#%d, applied: worked out anew: %+v", l.ID, l.Outlasts)
		}
	}
	if n := namedReads.Load() - before; n != 1 {
		t.Errorf("%d versions parsed, want the latest alone", n)
	}
}

// TestOutlastsOldVersionsBounded: open proposals made against older
// versions have them parsed oldVersionsMost at a time: the rest say what
// they kept, and are worked out by the reads after.
func TestOutlastsOldVersionsBounded(t *testing.T) {
	ctx := context.Background()
	s, dir := restartsServer(t, nil)
	ids := againstVersions(t, s, dir, oldVersionsMost+2, oldVersionsMost+3, ledger.StateApproved)
	var ps []ledger.Proposal
	for _, id := range ids {
		ps = append(ps, state(t, s, id))
	}
	worked := func() int {
		t.Helper()
		before := namedReads.Load()
		ls, err := s.linesNow(ctx, ps)
		if err != nil {
			t.Fatal(err)
		}
		if n := namedReads.Load() - before; n > 1+oldVersionsMost {
			t.Errorf("%d versions parsed in one read, want %d at most", n, 1+oldVersionsMost)
		}
		k := 0
		for _, l := range ls {
			if l.Outlasts != nil && l.Outlasts.Window == 30*time.Minute {
				k++
			}
		}
		return k
	}
	if k := worked(); k != oldVersionsMost {
		t.Errorf("the first read worked out %d, want %d", k, oldVersionsMost)
	}
	if k := worked(); k != len(ids) {
		t.Errorf("the second read worked out %d, want all %d", k, len(ids))
	}
}

// TestOutlastsByEvidence: what a proposal's for_outlasts says is kept by
// its evidence too: evidence kept anew, as backtesting again keeps it, is
// read anew.
func TestOutlastsByEvidence(t *testing.T) {
	ctx := context.Background()
	s, _ := restartsServer(t, nil)
	id := keptAs(t, s, verdict.Semantics, "test", unrecordedFrom, unrecordedTo, nil)
	read := func() *verdict.Outlast {
		t.Helper()
		ls, err := s.linesNow(ctx, []ledger.Proposal{state(t, s, id)})
		if err != nil {
			t.Fatal(err)
		}
		return ls[0].Outlasts
	}
	if read() == nil {
		t.Fatal("a for of 1h over 30m doesn't outlast")
	}
	ten := 10 * time.Minute
	ev, _ := json.Marshal(evidence{From: "suggestion", Edit: &rules.Edit{Name: "RestartsNow", Group: "pods", For: &ten}, For: ten, Semantics: verdict.Semantics, Version: "test"})
	ledgerExec(t, s, "UPDATE proposals SET evidence = ? WHERE id = ?", string(ev), id)
	if o := read(); o != nil {
		t.Errorf("evidence kept anew, read as before: %+v", o)
	}
}

// TestFlightsOnce: those who ask for the same while it is worked out wait
// for it, worked out once; asked once it is done, it is worked out again.
func TestFlightsOnce(t *testing.T) {
	var f flights[int]
	var released atomic.Bool
	release, started := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.do(context.Background(), 7, func() (int, error) {
			close(started)
			<-release
			return 42, nil
		})
	}()
	<-started
	got := make([]int, 4)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = f.do(context.Background(), 7, func() (int, error) {
				if !released.Load() {
					t.Error("worked out again while it was being worked out")
				}
				return 42, nil
			})
		}()
	}
	time.Sleep(20 * time.Millisecond) // the others ask meanwhile
	released.Store(true)
	close(release)
	wg.Wait()
	for i, v := range got {
		if v != 42 {
			t.Errorf("%d got %d", i, v)
		}
	}
	if v, _ := f.do(context.Background(), 7, func() (int, error) { return 1, nil }); v != 1 {
		t.Errorf("asked again once done: %d", v)
	}
}

// TestOldNoRecordJudged: a backtest made before no_record was kept that
// caught an incident, or would miss one, judged its firings: its period
// had a record then, whatever the ledger says of it now, and its page
// says what it would miss, never that its firings were counted, not
// judged.
func TestOldNoRecordJudged(t *testing.T) {
	ctx := context.Background()
	s, _ := restartsServer(t, nil)
	f, err := s.current(ctx, "RestartsNow", false)
	if err != nil {
		t.Fatal(err)
	}
	hour := time.Hour
	for _, bt := range []backtest{
		{Before: side{Firings: 141, White: 2}, After: side{Firings: 3, White: 1}, Lost: []string{"INC-9"}, Gained: []string{}, Later: []string{}},
		{Before: side{Firings: 141}, After: side{Firings: 3}, Lost: []string{}, Gained: []string{"INC-9"}, Later: []string{}},
	} {
		bt.From, bt.To = unrecordedFrom, unrecordedTo
		ev, _ := json.Marshal(evidence{From: "suggestion", Edit: &rules.Edit{Name: "RestartsNow", Group: "pods", For: &hour}, For: hour, Backtest: &bt})
		id, err := s.l.Propose(ctx, ledger.Proposal{Rule: "RestartsNow", Kind: ledger.KindFor, Path: ledger.PathNone, MadeBy: ledger.Proposer,
			Before: ruleText(f.rule), BeforeHash: ruleHash(f.rule), After: "for: 1h\n", Evidence: ev})
		if err != nil {
			t.Fatal(err)
		}
		ls, err := s.linesNow(ctx, []ledger.Proposal{state(t, s, id)})
		if err != nil || ls[0].NoRecord {
			t.Errorf("%+v: taken as having no record: %v", bt, err)
		}
		if _, body, _ := ui(t, s.Handler(), http.MethodGet, "/proposals/"+itoa(id), cy, "", nil); strings.Contains(body, "counted, not judged") {
			t.Errorf("%+v: its page says it was counted, not judged", bt)
		}
		if no, err := s.spanNoRecord(ctx, &bt); err != nil || no {
			t.Errorf("%+v: an undo's page would take it as having no record", bt)
		}
		s.l.Move(ctx, id, []string{ledger.StateProposed}, ledger.StateDropped, "ana", ledger.Step{})
	}
}

// TestEarlierMissKept: a backtest run again over the twelve weeks to now
// can leave out an incident the one before it said the change misses, as
// the weeks move on. The proposal keeps it, and says so on its page, in
// its pull request's description and through MCP; its history says what
// the backtest replaced named; and what is checked again when it is
// carried out isn't refused for missing it, which was said.
func TestEarlierMissKept(t *testing.T) {
	ctx := context.Background()
	s, _ := restartsServer(t, nil)
	h := s.Handler()
	id := keptAs(t, s, 0, "", unrecordedFrom, unrecordedTo, nil)
	p := state(t, s, id)
	ev := proposalEvidence(p)
	ev.Backtest.Before.White, ev.Backtest.Lost, ev.Backtest.Later = 3, []string{"INC-OLD"}, []string{"INC-LATE"}
	raw, _ := json.Marshal(ev)
	ledgerExec(t, s, "UPDATE proposals SET evidence = ? WHERE id = ?", string(raw), id)
	if code, body, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/backtest", bo, "same-origin", url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("backtesting again: %d %s", code, body)
	}
	p = state(t, s, id)
	if got := proposalEvidence(p); len(got.EarlierLost) != 1 || got.EarlierLost[0] != "INC-OLD" || slices.Contains(got.Backtest.Lost, "INC-OLD") {
		t.Fatalf("kept: %s", p.Evidence)
	}
	_, page, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), cy, "", nil)
	for _, want := range []string{"An earlier backtest of it, before it was backtested again, said that as proposed it misses: <a href=\"/incidents/INC-OLD\"><bdi>INC-OLD</bdi></a>.",
		"It said that as proposed it misses: <a href=\"/incidents/INC-OLD\"><bdi>INC-OLD</bdi></a>.", "Caught later as proposed: <a href=\"/incidents/INC-LATE\"><bdi>INC-LATE</bdi></a>."} {
		if !strings.Contains(page, want) {
			t.Errorf("its page doesn't say %q", want)
		}
	}
	if pr := bodyOf(t, s, p); !strings.Contains(pr, "An earlier backtest said that as proposed it misses: `INC-OLD`.") {
		t.Errorf("its description:\n%s", pr)
	}
	_, out, err := s.toolRule(ctx, nil, ruleIn{Name: "RestartsNow"})
	if err != nil || len(out.Proposals) != 1 || out.Proposals[0].Backtest == nil || !slices.Equal(out.Proposals[0].Backtest.EarlierMissed, []string{"INC-OLD"}) {
		t.Errorf("MCP: %+v %v", out.Proposals, err)
	}
	// Checked again: an incident an earlier backtest named was approved
	// as missed; one none named is refused.
	was := proposalEvidence(p)
	if why := worse(was, evidence{Backtest: &backtest{Lost: []string{"INC-OLD"}}}); why != "" {
		t.Errorf("missing what an earlier backtest said: %q", why)
	}
	if why := worse(was, evidence{Backtest: &backtest{Lost: []string{"INC-NEW"}}}); why == "" {
		t.Error("missing what none said: not refused")
	}
}

// TestBacktestedTold: backtesting again is told as proposal.backtested, to
// the webhook and Slack, naming whose approvals were withdrawn and whether
// an agent asked for it; Lapilli's own too.
func TestBacktestedTold(t *testing.T) {
	ctx := context.Background()
	s, _ := restartsServer(t, func(c *Config) { c.Approvals = 2 })
	s.l.TellTo([]string{"webhook"})
	id := keptAs(t, s, 0, "", unrecordedFrom, unrecordedTo, nil)
	if _, err := s.l.Approve(ctx, id, ana, 2, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := ui(t, s.Handler(), http.MethodPost, "/proposals/"+itoa(id)+"/backtest", bo, "same-origin", url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("backtesting again: %d %s", code, body)
	}
	ns, err := s.l.Due(ctx, "webhook", time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ns, func(n ledger.Notice) bool { return n.Event == "proposal.backtested" })
	if i < 0 {
		t.Fatalf("not told: %v", ns)
	}
	var n notice
	if err := json.Unmarshal(ns[i].Body, &n); err != nil || n.Proposal == nil || !slices.Equal(n.Proposal.Withdrawn, []string{ana}) || n.Proposal.ByAgent || n.By != bo {
		t.Errorf("told: %s", ns[i].Body)
	}
	for _, c := range []struct {
		agent     bool
		withdrawn []string
		want      string
	}{
		{false, []string{ana}, "lapilli: bo@example.com backtested #" + itoa(id) + ", a new for for RestartsNow, again, and withdrew the approvals of ana@example.com"},
		{true, nil, "lapilli: an agent, for bo@example.com, backtested #" + itoa(id) + ", a new for for RestartsNow, again"},
	} {
		n.Proposal.ByAgent, n.Proposal.Withdrawn = c.agent, c.withdrawn
		if got := s.line(n); got != c.want {
			t.Errorf("its line: %q, want %q", got, c.want)
		}
	}
}

// TestBacktestAgainLedgerBound: what bounds backtesting again is kept in
// the ledger, not a replica's memory: once one ran, the same proposal,
// stale again (an incident written down in its weeks and taken away, say),
// isn't backtested again for ten minutes, with this replica's tries
// forgotten as another's would be, and the reader is told why.
func TestBacktestAgainLedgerBound(t *testing.T) {
	s, _ := restartsServer(t, nil)
	h := s.Handler()
	id := keptAs(t, s, 0, "", unrecordedFrom, unrecordedTo, nil)
	path := "/proposals/" + itoa(id) + "/backtest"
	if code, body, _ := ui(t, h, http.MethodPost, path, bo, "same-origin", url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("backtesting again: %d %s", code, body)
	}
	// Stale again, as an earlier lapilli's would be.
	ev := proposalEvidence(state(t, s, id))
	ev.Semantics = 0
	raw, _ := json.Marshal(ev)
	ledgerExec(t, s, "UPDATE proposals SET evidence = ? WHERE id = ?", string(raw), id)
	s.tries = tries{}
	code, body, _ := ui(t, h, http.MethodPost, path, cy, "same-origin", url.Values{})
	if code != http.StatusTooManyRequests || !strings.Contains(body, "less than ten minutes ago") {
		t.Errorf("again within ten minutes, by another: %d %s", code, body)
	}
	if got := proposalEvidence(state(t, s, id)); got.Semantics != 0 {
		t.Errorf("backtested again within ten minutes: %+v", got)
	}
}

// TestPromoteHoldsStale: with --approvals lowered, a proposal that has
// the approvals it now needs is approved, unless it waits to be backtested
// again: held, as an approval of it is.
func TestPromoteHoldsStale(t *testing.T) {
	ctx := context.Background()
	s, _ := restartsServer(t, func(c *Config) { c.Approvals = 2 })
	stale := keptAs(t, s, 0, "", unrecordedFrom, unrecordedTo, nil)
	fresh := proposeFor(t, s.Handler(), "15m")
	for _, id := range []int64{stale, fresh} {
		if _, err := s.l.Approve(ctx, id, ana, 2, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	s.cfg.Approvals = 1
	if err := s.carryOut(ctx); err != nil {
		t.Fatal(err)
	}
	if p := state(t, s, stale); p.State != ledger.StateProposed {
		t.Errorf("a stale one approved as fewer approvals are needed: %s", p.State)
	}
	if p := state(t, s, fresh); p.State == ledger.StateProposed {
		t.Errorf("a fresh one not approved as fewer approvals are needed: %s", p.State)
	}
}

// TestLapilliBacktestsStale: Lapilli backtests stale proposals waiting for
// approval again itself, as Lapilli, in its work, a few a round, the
// longest unmoved first: its own, which no one would ask for, too. One
// whose rule changed since is left for a person; one it can't is left for
// an hour.
func TestLapilliBacktestsStale(t *testing.T) {
	ctx := context.Background()
	s, dir := restartsServer(t, nil)
	var b strings.Builder
	b.WriteString(restartsFile)
	for i := 0; i < againARun+2; i++ {
		fmt.Fprintf(&b, "      - {alert: Restarts%d, expr: 'increase(restarts[30m]) > 0.5', for: 1m}\n", i)
	}
	os.WriteFile(filepath.Join(dir, "restarts.yaml"), []byte(b.String()), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i := 0; i < againARun+2; i++ {
		ids = append(ids, keptFor(t, s, fmt.Sprintf("Restarts%d", i), 0, "", unrecordedFrom, unrecordedTo, nil))
	}
	// The rule of the first changed since: left for a person.
	changed := keptAs(t, s, 0, "", unrecordedFrom, unrecordedTo, nil)
	os.WriteFile(filepath.Join(dir, "restarts.yaml"), []byte(strings.Replace(b.String(), "> 0.5\n        for: 1m", "> 1.5\n        for: 1m", 1)), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	backtested := func() (n int) {
		for _, id := range ids {
			if proposalEvidence(state(t, s, id)).Semantics == verdict.Semantics {
				n++
			}
		}
		return n
	}
	s.jobs(ctx, false)
	if n := backtested(); n != againARun {
		t.Errorf("one round backtested %d again, want %d", n, againARun)
	}
	if proposalEvidence(state(t, s, changed)).Semantics != 0 {
		t.Error("one whose rule changed since was backtested again")
	}
	steps, _, _ := s.l.ProposalSteps(ctx, ids[0], 10)
	if len(steps) == 0 || steps[len(steps)-1].By != ledger.Proposer || !strings.HasSuffix(steps[len(steps)-1].What, "backtested again") {
		t.Errorf("not as Lapilli: %+v", steps)
	}
	s.jobs(ctx, false)
	if n := backtested(); n != len(ids) {
		t.Errorf("two rounds backtested %d again, want all %d", n, len(ids))
	}
	// Not while it reads no rules of its own, nor read-only.
	s.cfg.ReadOnly = true
	if err := s.backtestStale(ctx); err != nil {
		t.Fatal(err)
	}
}
