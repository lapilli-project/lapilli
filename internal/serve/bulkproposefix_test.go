package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// TestProposeTries: a reader asks for triesAnHour proposals an hour at most
// that read the rules again, made or refused (each refused here, the for
// it has already, after the read), with the single form and in bulk alike;
// past them, the single form answers 429 and a bulk proposal counts every
// rule left so, reading none. Lapilli's own aren't counted.
func TestProposeTries(t *testing.T) {
	s := proposeServer(t, nil)
	h := s.Handler()
	same := url.Values{"rule": {"BulkB"}, "kind": {"for"}, "for": {"15m"}}
	for i := range triesAnHour {
		if code, body, _ := postAs(t, h, "/proposals", same, bo, "same-origin"); code != http.StatusBadRequest || !strings.Contains(body, "that is its for already") {
			t.Fatalf("try %d: %d", i, code)
		}
	}
	code, body, _ := postAs(t, h, "/proposals", url.Values{"rule": {"BulkD"}, "kind": {"for"}, "for": {"10m"}}, bo, "same-origin")
	if code != http.StatusTooManyRequests || !strings.Contains(body, "made or refused") {
		t.Errorf("past the tries: %d", code)
	}
	_, _, hd := postAs(t, h, "/rules/bulk", url.Values{"do": {"propose"}, "propose": {"for"}, "for": {"10m"}, "rule": {"BulkA", "BulkD", "Errors"}, "back": {"/rules"}}, bo, "same-origin")
	if res := bulkOf(mustQuery(t, hd.Get("Location"))); res == nil || res.Tries != 3 || res.Done != 0 {
		t.Errorf("a bulk past the tries: %s", hd.Get("Location"))
	}
	// Another reader's are their own.
	if code, _, _ := postAs(t, h, "/proposals", url.Values{"rule": {"BulkD"}, "kind": {"for"}, "for": {"10m"}}, ana, "same-origin"); code != http.StatusSeeOther {
		t.Errorf("another reader: %d", code)
	}
	// Lapilli's own aren't counted.
	for range 3 {
		if _, err := s.propose(context.Background(), ask{Rule: "BulkB", Kind: ledger.KindFor, For: 15 * time.Minute}, ledger.Proposer, false, true); err == nil || strings.Contains(err.Error(), "made or refused") {
			t.Errorf("Lapilli's own: %v", err)
		}
	}
}

// TestProposeLimitRace: proposals asked for at once, past the hour's
// twenty, are refused 429 whichever check finds it, MayPropose or the
// ledger's write: never 400.
func TestProposeLimitRace(t *testing.T) {
	s := proposeServer(t, nil)
	ctx := context.Background()
	var b strings.Builder
	b.WriteString("groups:\n  - name: race\n    rules:\n")
	for i := range 12 {
		fmt.Fprintf(&b, "      - {alert: Race%d, expr: errors > %d, for: 1m}\n", i, 10+i)
	}
	os.WriteFile(filepath.Join(s.cfg.RulesPath, "race.yaml"), []byte(b.String()), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	for i := range ledger.ProposalsAnHour - 1 {
		if _, err := s.l.Propose(ctx, ledger.Proposal{Rule: fmt.Sprint("Other", i), Kind: ledger.KindFor, Path: ledger.PathNone, MadeBy: bo}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.propose(ctx, ask{Rule: fmt.Sprint("Race", i), Kind: ledger.KindFor, For: 5 * time.Minute, From: "person"}, bo, false, true)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	made := 0
	for err := range errs {
		if err == nil {
			made++
			continue
		}
		w := httptest.NewRecorder()
		s.oops(w, httptest.NewRequest(http.MethodPost, "/proposals", nil), err)
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("past the hour's twenty: %d %v", w.Code, err)
		}
	}
	if made != 1 {
		t.Errorf("%d made, not the one left", made)
	}
}

// TestBulkProposeSpareOverWeeks: retiring in bulk says a rule comes from
// being spare as its page, opened from the list, does: over the weeks the
// list showed. Errors2 caught both of the last week's incidents with
// Errors, and alone one six weeks back: spare over four weeks, not twelve.
func TestBulkProposeSpareOverWeeks(t *testing.T) {
	ctx := context.Background()
	mk := func() *Server {
		// As changesServer, with the second rule there before the first
		// run, which replays it over the weeks before.
		s, rules := server(t, func(c *Config) {
			c.UserHeader, c.Editors, c.Admins, c.Approvers, c.Changes = "X-Forwarded-Email", []string{bo}, []string{ana}, []string{ana, cy}, true
			c.Backfill = 28 * 24 * time.Hour
			c.Options.NoisyFirings = 1000
		})
		os.WriteFile(filepath.Join(rules, "more.yaml"), []byte("groups:\n  - name: more\n    rules:\n      - {alert: Errors2, expr: errors > 1, for: 2m, labels: {severity: page}}\n"), 0o644)
		if err := s.look(ctx, new(string)); err != nil {
			t.Fatal(err)
		}
		s.jobs(ctx, true)
		o := s.cfg.Options
		at := now.Add(-6 * 7 * 24 * time.Hour)
		if err := s.l.Record(ctx, ledger.Found{From: at.Add(-time.Hour), To: at.Add(time.Hour), Run: ledger.Run{Kind: "backfill", Started: at, Finished: at},
			Firings: []firing.Firing{{Rule: "Errors2", Labels: map[string]string{"service": "checkout"}, Start: at, End: at.Add(10 * time.Minute)}}}, o); err != nil {
			t.Fatal(err)
		}
		h := s.Handler()
		for in, start := range map[string]string{"I-1": "2026-11-02T06:01:00Z", "I-2": "2026-11-02T06:01:00Z", "I-0": at.Add(5 * time.Minute).Format(time.RFC3339)} {
			if code, body := api(t, h, "PUT", "/api/v1/incidents/"+in, `{"title": "x", "start": "`+start+`", "labels": {"service": ["checkout"]}}`); code != http.StatusOK {
				t.Fatalf("incident: %d %s", code, body)
			}
		}
		return s
	}
	for _, weeks := range []string{"4", "12"} {
		bulk, one := mk(), mk()
		f := pageForm(t, one.Handler(), "/rules/Errors2?tab=proposals&weeks="+weeks, ledger.KindDelete, false)
		if f == nil {
			t.Fatalf("weeks %s: no retiring form", weeks)
		}
		if code, body, _ := postAs(t, one.Handler(), "/proposals", f, bo, "same-origin"); code != http.StatusSeeOther {
			t.Fatalf("weeks %s one: %d %s", weeks, code, body)
		}
		code, _, hd := postAs(t, bulk.Handler(), "/rules/bulk", url.Values{"do": {"propose"}, "propose": {"retire"}, "rule": {"Errors2"},
			"back": {"/rules?weeks=" + weeks}}, bo, "same-origin")
		if res := bulkOf(mustQuery(t, hd.Get("Location"))); code != http.StatusSeeOther || res == nil || res.Done != 1 {
			t.Fatalf("weeks %s bulk: %d %s", weeks, code, hd.Get("Location"))
		}
		b, o := proposalsKept(t, bulk), proposalsKept(t, one)
		if strings.Join(b, "\n") != strings.Join(o, "\n") {
			t.Errorf("weeks %s: proposals\n%s\none\n%s", weeks, strings.Join(b, "\n"), strings.Join(o, "\n"))
		}
		spare := strings.Contains(strings.Join(o, "\n"), `"from":"spare"`)
		if spare != (weeks == "4") {
			t.Errorf("weeks %s: spare %v: the fixture doesn't tell four weeks from twelve", weeks, spare)
		}
	}
}

// TestAgentRetires: an agent's retire is turned off where the rule can be,
// deleted where it can't, as propose finds the rule after checking the
// person it acts for may propose.
func TestAgentRetires(t *testing.T) {
	g := grafanaServer(t, &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}})
	g.cfg.MCPPropose = "editors"
	f, _ := changesServer(t, func(c *Config) { c.MCPPropose = "editors" })
	for _, c := range []struct {
		s    *Server
		rule string
		kind string
	}{{g, "Other", ledger.KindDisable}, {f, "Errors", ledger.KindDelete}} {
		ts := httptest.NewServer(c.s.Handler())
		var out proposeOut
		if failed, text := call(t, connect(t, ts.URL, bo), "propose", map[string]any{"rule": c.rule, "kind": "retire", "reason": "spare"}, &out); failed {
			ts.Close()
			t.Fatalf("%s: %s", c.rule, text)
		}
		ts.Close()
		if p := state(t, c.s, out.ID); p.Kind != c.kind || !p.Agent {
			t.Errorf("%s: %s, agent %v", c.rule, p.Kind, p.Agent)
		}
	}
}

// TestBulkProposeNothingPicked: nothing picked says so first, reading
// nothing, whatever else the form says.
func TestBulkProposeNothingPicked(t *testing.T) {
	s := proposeServer(t, nil)
	code, _, hd := postAs(t, s.Handler(), "/rules/bulk", url.Values{"do": {"propose"}, "propose": {"bogus"}, "back": {"/rules"}}, bo, "same-origin")
	if res := bulkOf(mustQuery(t, hd.Get("Location"))); code != http.StatusSeeOther || res == nil || !res.None {
		t.Errorf("%d %s", code, hd.Get("Location"))
	}
}

// TestProposalsPickedNone: the proposals list asked for proposals by id
// that aren't there says so, with a way out, not that there are none.
func TestProposalsPickedNone(t *testing.T) {
	s, _ := changesServer(t, nil)
	_, body := getAs(t, s.Handler(), "/proposals?state=all&id=999999", bo)
	if !strings.Contains(body, "None of the proposals picked is here.") || !strings.Contains(body, `>Clear filters</a>`) || strings.Contains(body, "No proposals yet.") {
		t.Error("the proposals picked, not there, aren't said so")
	}
}

// TestLanguagesWhole: each language's name at the sidebar's foot is kept
// whole, as the stylesheet says: 한국어 wasn't, broken between its
// syllables where the sidebar is narrow.
func TestLanguagesWhole(t *testing.T) {
	css, err := files.ReadFile("static/lapilli.css")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`\.langs a, \.langs strong \{[^}]*white-space:\s*nowrap`).Match(css) {
		t.Error("a language's name may be broken")
	}
	_, body := getAs(t, pagesServer(t).Handler(), "/", "")
	for _, w := range []string{`class="langs"`, `lang="ko" hreflang="ko">한국어</a>`} {
		if !strings.Contains(body, w) {
			t.Errorf("no %s", w)
		}
	}
}

// TestTriesApart: an agent asking again and again uses up its own tries,
// not those of the person it acts for: where no one is named, everyone is
// "anonymous", and an agent's refusals would otherwise lock every person
// out of proposing for an hour, with nothing in the ledger to show for it.
func TestTriesApart(t *testing.T) {
	s := proposeServer(t, nil)
	ctx := context.Background()
	same := ask{Rule: "BulkB", Kind: ledger.KindFor, For: 15 * time.Minute, From: "agent"}
	for i := range triesAnHour {
		if _, err := s.propose(ctx, same, "anonymous", true, true); err == nil || !strings.Contains(err.Error(), "that is its for already") {
			t.Fatalf("an agent's try %d: %v", i, err)
		}
	}
	if _, err := s.propose(ctx, same, "anonymous", true, true); err != errTooManyTries {
		t.Errorf("past its tries, an agent: %v", err)
	}
	if _, err := s.propose(ctx, ask{Rule: "BulkD", Kind: ledger.KindFor, For: 10 * time.Minute, From: "person"}, "anonymous", false, true); err != nil {
		t.Errorf("the person, after their agent's tries: %v", err)
	}
}

// TestTriesAtTheRead: a try is counted only when the rules are read again:
// a proposal refused before (a rule not in effect, one waiting already)
// costs none.
func TestTriesAtTheRead(t *testing.T) {
	s := proposeServer(t, nil)
	h := s.Handler()
	for i := range triesAnHour + 5 {
		if code, _, _ := postAs(t, h, "/proposals", url.Values{"rule": {"Gone"}, "kind": {"for"}, "for": {"10m"}}, bo, "same-origin"); code != http.StatusBadRequest {
			t.Fatalf("try %d: %d", i, code)
		}
	}
	if code, body, _ := postAs(t, h, "/proposals", url.Values{"rule": {"BulkD"}, "kind": {"for"}, "for": {"10m"}}, bo, "same-origin"); code != http.StatusSeeOther {
		t.Errorf("after refusals that read nothing again: %d %.200s", code, body)
	}
}

// TestRefusedWithoutWaiting: a reader past the hour's twenty, or past
// their tries, is refused before waiting for the heavy turn, by the bulk
// proposal and the single form alike.
func TestRefusedWithoutWaiting(t *testing.T) {
	s := proposeServer(t, nil)
	ctx := context.Background()
	was, wasTime := busyWait, proposeTime
	defer func() { busyWait, proposeTime = was, wasTime }()
	busyWait, proposeTime = 10*time.Second, 10*time.Second
	for i := range ledger.ProposalsAnHour {
		if _, err := s.l.Propose(ctx, ledger.Proposal{Rule: fmt.Sprint("Other", i), Kind: ledger.KindFor, Path: ledger.PathNone, MadeBy: bo}); err != nil {
			t.Fatal(err)
		}
	}
	for range triesAnHour {
		s.tries.take(ana, false, time.Now())
	}
	s.heavy <- struct{}{} // another heavy request's
	defer func() { <-s.heavy }()
	h := s.Handler()
	for _, c := range []struct {
		who, want string
	}{{bo, "limit=3"}, {ana, "tries=3"}} {
		began := time.Now()
		_, _, hd := postAs(t, h, "/rules/bulk", url.Values{"do": {"propose"}, "propose": {"for"}, "for": {"10m"}, "rule": {"BulkA", "BulkB", "BulkD"}, "back": {"/rules"}}, c.who, "same-origin")
		if took := time.Since(began); took > 2*time.Second || !strings.Contains(hd.Get("Location"), c.want) {
			t.Errorf("%s's bulk: %s after %s", c.who, hd.Get("Location"), took)
		}
		began = time.Now()
		code, _, _ := postAs(t, h, "/proposals", url.Values{"rule": {"BulkD"}, "kind": {"for"}, "for": {"10m"}}, c.who, "same-origin")
		if took := time.Since(began); took > 2*time.Second || code != http.StatusTooManyRequests {
			t.Errorf("%s's single: %d after %s", c.who, code, took)
		}
	}
}

// TestRetireNotAFormKind: retiring as propose decides it is the bulk
// proposal's and MCP's; a rule's page's form names its kind, and one
// sending "retire" is refused, nothing made.
func TestRetireNotAFormKind(t *testing.T) {
	s := proposeServer(t, nil)
	before := len(proposalsKept(t, s))
	if code, _, _ := postAs(t, s.Handler(), "/proposals", url.Values{"rule": {"BulkD"}, "kind": {"retire"}}, bo, "same-origin"); code != http.StatusBadRequest {
		t.Errorf("kind=retire: %d", code)
	}
	if n := len(proposalsKept(t, s)); n != before {
		t.Errorf("%d proposals, not %d", n, before)
	}
}
