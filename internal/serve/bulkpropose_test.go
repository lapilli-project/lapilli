package serve

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// proposeServer is changesServer with more rules: BulkA, with a suggested
// for of 30m; BulkB, whose for is 15m already; BulkC, with a proposal not
// finished with, someone else's; BulkD; and Errors, as changesServer has
// it. bo is an editor, vi a viewer, pat an editor of BulkA's team alone.
func proposeServer(t *testing.T, more func(*Config)) *Server {
	t.Helper()
	ctx := context.Background()
	s, _ := changesServer(t, func(c *Config) {
		c.Editors = []string{bo}
		if more != nil {
			more(c)
		}
	})
	os.WriteFile(filepath.Join(s.cfg.RulesPath, "bulk.yaml"), []byte(`groups:
  - name: bulk
    rules:
      - {alert: BulkA, expr: errors > 1, for: 2m, labels: {team: a}}
      - {alert: BulkB, expr: errors > 2, for: 15m}
      - {alert: BulkC, expr: errors > 3, for: 1m}
      - {alert: BulkD, expr: errors > 4}
`), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	// BulkA fired, noisily, and the digest suggested a longer for for it:
	// its page offers "Propose it".
	at := now.Add(-30 * time.Hour)
	if err := s.l.Record(ctx, ledger.Found{From: at, To: now, Run: ledger.Run{Kind: "backfill", Started: at, Finished: at},
		Firings: []firing.Firing{{Rule: "BulkA", Labels: map[string]string{"service": "x"}, Start: at, End: at.Add(5 * time.Minute)}}}, s.cfg.Options); err != nil {
		t.Fatal(err)
	}
	week := s.l.Midnight(ledger.Week(s.l.Day(now)))
	if err := s.l.AddResult(ctx, "suggestions", now, week.Add(-7*24*time.Hour), week, []verdict.Suggestion{
		{Rule: "BulkA", For: 2 * time.Minute, NewFor: 30 * time.Minute}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.propose(ctx, ask{Rule: "BulkC", Kind: ledger.KindFor, For: 5 * time.Minute, From: "person"}, cy, false, true); err != nil {
		t.Fatal(err)
	}
	team, err := s.l.PutTeam(ctx, ledger.Team{Name: "a", Rules: "^BulkA$"}, ana)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutGrant(ctx, ledger.Grant{Who: "user:pat@example.com", TeamID: team}, ana); err != nil {
		t.Fatal(err)
	}
	return s
}

// pageForm is the form a rule's page proposes with, as a browser would
// send it: on the page at path, the form to /proposals whose kind is kind
// (and, with hidden, whose for is a hidden field, the summary's "Propose
// it"), its fields as they are, nil when the page has none.
func pageForm(t *testing.T, h http.Handler, path, kind string, hidden bool) url.Values {
	t.Helper()
	code, body := getAs(t, h, path, bo)
	if code == http.StatusNotFound {
		return nil // no such rule: no page, and no form
	}
	if code != http.StatusOK {
		t.Fatalf("%s: %d", path, code)
	}
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range allNodes(doc) {
		if f.DataAtom != atom.Form || nodeAttr(f, "action") != "/proposals" {
			continue
		}
		v := url.Values{}
		forHidden := false
		for _, n := range allNodes(f) {
			if n.DataAtom == atom.Input && nodeAttr(n, "name") != "" && nodeAttr(n, "type") != "checkbox" {
				v.Set(nodeAttr(n, "name"), nodeAttr(n, "value"))
				if nodeAttr(n, "name") == "for" && nodeAttr(n, "type") == "hidden" {
					forHidden = true
				}
			}
		}
		if v.Get("kind") == kind && forHidden == hidden {
			return v
		}
	}
	return nil
}

// proposalsKept are the proposals a ledger keeps, all they are but when, in
// the order they were made.
func proposalsKept(t *testing.T, s *Server) []string {
	t.Helper()
	ps, err := s.l.Proposals(context.Background(), ledger.ProposalQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range ps {
		out = append(out, fmt.Sprintf("#%d %s %s %s %s %s agent=%v before=%q %s after=%q %s doc=%s %q diff=%q follows=%d evidence=%s",
			p.ID, p.Rule, p.Kind, p.Path, p.State, p.MadeBy, p.Agent, p.Before, p.BeforeHash, p.After, p.AfterHash, p.DocHash, p.Document, p.Diff, p.Follows, p.Evidence))
	}
	slices.Sort(out)
	return out
}

// TestBulkPropose: a proposal for the rules picked makes, rule by rule,
// the proposal the rule's own page's form makes, posted one at a time on a
// twin ledger: a new for (from the suggestion where there is one), the
// for suggested for each, retiring each; the same proposals kept, evidence
// and backtest, path and diff, the same lines in the changes log, by whom;
// what is refused, counted by why, and the proposals made linked to.
func TestBulkPropose(t *testing.T) {
	picks := []string{"Errors", "BulkA", "BulkB", "BulkC", "BulkD", "Gone"}
	for _, c := range []struct {
		what, kind string
		form       url.Values
		// one is the form a rule's page sends, and the page it is on.
		one  func(t *testing.T, h http.Handler, rule string) url.Values
		want string // the result, but the proposals made
	}{
		{"for", "a new for", url.Values{"propose": {"for"}, "for": {"15m"}}, func(t *testing.T, h http.Handler, rule string) url.Values {
			f := pageForm(t, h, "/rules/"+rule+"?tab=proposals", ledger.KindFor, false)
			if f != nil {
				f.Set("for", "15m")
			}
			return f
		}, "bulk=propose&done=2&already=2&same=1&absent=1"},
		{"suggested", "the suggested for", url.Values{"propose": {"suggested"}}, func(t *testing.T, h http.Handler, rule string) url.Values {
			return pageForm(t, h, "/rules/"+rule, ledger.KindFor, true)
		}, "bulk=propose&done=1&absent=1&nosuggest=4"},
		{"retire", "retiring", url.Values{"propose": {"retire"}}, func(t *testing.T, h http.Handler, rule string) url.Values {
			for _, k := range []string{ledger.KindDisable, ledger.KindDelete} {
				if f := pageForm(t, h, "/rules/"+rule+"?tab=proposals", k, false); f != nil {
					return f
				}
			}
			return nil
		}, "bulk=propose&done=3&already=2&absent=1"},
	} {
		// The digest's week judged, its noisy rule suggested a longer for.
		bulk := proposeServer(t, keepRecord)
		one := proposeServer(t, keepRecord)
		form := url.Values{"do": {"propose"}, "rule": picks, "back": {"/rules?weeks=12&sort=refired"}}
		for k, v := range c.form {
			form[k] = v
		}
		code, body, hd := postAs(t, bulk.Handler(), "/rules/bulk", form, bo, "same-origin")
		if code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", c.kind, code, body)
		}
		loc, err := url.Parse(hd.Get("Location"))
		if err != nil || loc.Path != "/rules" || loc.Query().Get("sort") != "refired" || loc.Query().Get("weeks") != "12" {
			t.Errorf("%s: back to %q", c.kind, hd.Get("Location"))
		}
		t.Logf("%s: %s", c.kind, loc)
		res := bulkOf(loc.Query())
		if res == nil {
			t.Fatalf("%s: no result in %q", c.kind, loc)
		}
		// One at a time, each from its own page, as a browser would.
		h := one.Handler()
		made, refused := 0, 0
		for _, rule := range picks {
			f := c.one(t, h, rule)
			if f == nil {
				refused++ // the page offers none: nothing to send
				continue
			}
			switch code, body, _ := postAs(t, h, "/proposals", f, bo, "same-origin"); code {
			case http.StatusSeeOther:
				made++
			case http.StatusBadRequest, http.StatusConflict, http.StatusTooManyRequests:
				refused++
			default:
				t.Fatalf("%s %s one at a time: %d %s", c.kind, rule, code, body)
			}
		}
		if res.Done != made || len(res.Made) != made || res.Done+res.Already+res.Same+res.Absent+res.NoSuggest+res.Unreplayed+res.Refused+res.Limit != len(picks) {
			t.Errorf("%s: %+v, one at a time %d made, %d refused", c.kind, res, made, refused)
		}
		if c.want != "" {
			q := loc.Query()
			q.Del("made")
			q.Del("sort")
			q.Del("weeks")
			if got := q.Encode(); got != canonicalURL(t, "/?"+c.want)[2:] {
				t.Errorf("%s: %s, want %s", c.kind, got, c.want)
			}
		}
		if b, o := proposalsKept(t, bulk), proposalsKept(t, one); !slices.Equal(b, o) {
			t.Errorf("%s: proposals\n%s\none at a time\n%s", c.kind, strings.Join(b, "\n"), strings.Join(o, "\n"))
		}
		_, _, bl := worked(t, bulk)
		_, _, ol := worked(t, one)
		if !slices.Equal(bl, ol) {
			t.Errorf("%s: the changes log\n%v\none at a time\n%v", c.kind, bl, ol)
		}
		// The proposals made are bo's, as one at a time, counted against
		// the hour's.
		for _, s := range []*Server{bulk, one} {
			ps, _ := s.l.Proposals(context.Background(), ledger.ProposalQuery{Limit: 100})
			n := 0
			for _, p := range ps {
				if p.MadeBy == bo {
					n++
				}
			}
			if n != made {
				t.Errorf("%s: %d of bo's, not %d", c.kind, n, made)
			}
		}
		// The list says it once, and links to them.
		_, page := getAs(t, bulk.Handler(), hd.Get("Location"), bo)
		if n := strings.Count(page, `role="status"`); n != 1 {
			t.Errorf("%s: said %d times", c.kind, n)
		}
		if made > 0 {
			link := regexp.MustCompile(`<a href="(/proposals\?[^"]+)">The proposals made, on the proposals list</a>`).FindStringSubmatch(page)
			if link == nil {
				t.Fatalf("%s: no link to the proposals made in\n%s", c.kind, page[strings.Index(page, `role="status"`):][:600])
			}
			_, list := getAs(t, bulk.Handler(), html.UnescapeString(link[1]), bo)
			var listed []string
			for _, m := range regexp.MustCompile(`<td><a href="/proposals/(\d+)[^"]*">#`).FindAllStringSubmatch(list, -1) {
				listed = append(listed, m[1])
			}
			var want []string
			for _, id := range res.Made {
				want = append(want, fmt.Sprint(id))
			}
			slices.Sort(listed)
			slices.Sort(want)
			if !slices.Equal(listed, want) {
				t.Errorf("%s: the link lists %v, made %v", c.kind, listed, want)
			}
		}
	}
}

// TestBulkProposeOff: retiring rules kept where they can be turned off
// (Grafana's) proposes turning each off, as its page's form does, the
// same proposals as one at a time, approved each as one.
func TestBulkProposeOff(t *testing.T) {
	bulk := grafanaServer(t, &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}})
	one := grafanaServer(t, &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}})
	code, _, hd := postAs(t, bulk.Handler(), "/rules/bulk", url.Values{"do": {"propose"}, "propose": {"retire"}, "rule": {"Errors", "Other"}, "back": {"/rules"}}, bo, "same-origin")
	res := bulkOf(mustQuery(t, hd.Get("Location")))
	if code != http.StatusSeeOther || res == nil || res.Done != 2 {
		t.Fatalf("%d %s", code, hd.Get("Location"))
	}
	for _, rule := range []string{"Errors", "Other"} {
		f := pageForm(t, one.Handler(), "/rules/"+rule+"?tab=proposals", ledger.KindDisable, false)
		if f == nil {
			t.Fatalf("%s's page offers no turning off", rule)
		}
		if code, body, _ := postAs(t, one.Handler(), "/proposals", f, bo, "same-origin"); code != http.StatusSeeOther {
			t.Fatalf("%s one at a time: %d %s", rule, code, body)
		}
	}
	b, o := proposalsKept(t, bulk), proposalsKept(t, one)
	if !slices.Equal(b, o) {
		t.Errorf("proposals\n%s\none at a time\n%s", strings.Join(b, "\n"), strings.Join(o, "\n"))
	}
	for _, p := range b {
		if strings.Contains(p, bo) && !strings.Contains(p, " disable grafana proposed ") {
			t.Errorf("not turned off through Grafana: %s", p)
		}
	}
	_, _, bl := worked(t, bulk)
	_, _, ol := worked(t, one)
	if !slices.Equal(bl, ol) {
		t.Errorf("the changes log\n%v\none at a time\n%v", bl, ol)
	}
}

// TestBulkProposeRefused: a bulk proposal is refused whole past twenty
// rules, from another site, to a viewer, read-only, or for what it can't
// propose; a team's editor's are each refused, editors only, as the single
// form refuses them; nothing is made.
func TestBulkProposeRefused(t *testing.T) {
	s := proposeServer(t, nil)
	h := s.Handler()
	var many []string
	for i := range maxPropose + 1 {
		many = append(many, fmt.Sprint("R", i))
	}
	before := len(proposalsKept(t, s))
	base := url.Values{"do": {"propose"}, "propose": {"for"}, "for": {"15m"}, "rule": {"BulkA", "BulkD"}, "back": {"/rules"}}
	with := func(k string, v ...string) url.Values {
		f := url.Values{}
		for x, y := range base {
			f[x] = y
		}
		f[k] = v
		return f
	}
	for _, c := range []struct {
		what, who, site string
		form            url.Values
		want            int
	}{
		{"21 rules", bo, "same-origin", with("rule", many...), http.StatusBadRequest},
		{"from another site", bo, "cross-site", base, http.StatusForbidden},
		{"a viewer", "vi@example.com", "same-origin", base, http.StatusForbidden},
		{"no one signed in", "", "same-origin", base, http.StatusForbidden},
		{"no for", bo, "same-origin", with("for", ""), http.StatusBadRequest},
		{"a for that isn't one", bo, "same-origin", with("for", "soon"), http.StatusBadRequest},
		{"a for too long", bo, "same-origin", with("for", "72h"), http.StatusBadRequest},
		{"nothing to propose", bo, "same-origin", with("propose", "threshold"), http.StatusBadRequest},
	} {
		if code, body, _ := postAs(t, h, "/rules/bulk", c.form, c.who, c.site); code != c.want {
			t.Errorf("%s: %d, not %d: %.300s", c.what, code, c.want, body)
		}
	}
	// The single form refuses a team's editor; the bulk one counts each.
	if code, _, _ := postAs(t, h, "/proposals", url.Values{"rule": {"BulkA"}, "kind": {"for"}, "for": {"15m"}}, "pat@example.com", "same-origin"); code != http.StatusForbidden {
		t.Errorf("a team's editor's single proposal: %d", code)
	}
	code, _, hd := postAs(t, h, "/rules/bulk", base, "pat@example.com", "same-origin")
	if code != http.StatusSeeOther || !strings.Contains(hd.Get("Location"), "editors=2") || strings.Contains(hd.Get("Location"), "done=") {
		t.Errorf("a team's editor: %d %s", code, hd.Get("Location"))
	}
	if n := len(proposalsKept(t, s)); n != before {
		t.Errorf("%d proposals after refusals, not the %d there were", n, before)
	}
	// Read-only, no one may.
	ro := proposeServer(t, func(c *Config) { c.ReadOnly = true })
	if code, _, _ := postAs(t, ro.Handler(), "/rules/bulk", base, bo, "same-origin"); code != http.StatusForbidden {
		t.Errorf("read-only: %d", code)
	}
}

// TestBulkProposeLimit: the hour's twenty proposals count those made in
// bulk as one by one: past them, each is refused, and said so.
func TestBulkProposeLimit(t *testing.T) {
	s := proposeServer(t, nil)
	ctx := context.Background()
	for i := range ledger.ProposalsAnHour - 1 {
		if _, err := s.l.Propose(ctx, ledger.Proposal{Rule: fmt.Sprint("Other", i), Kind: ledger.KindFor, Path: ledger.PathNone, MadeBy: bo}); err != nil {
			t.Fatal(err)
		}
	}
	code, _, hd := postAs(t, s.Handler(), "/rules/bulk", url.Values{"do": {"propose"}, "propose": {"for"}, "for": {"10m"},
		"rule": {"BulkA", "BulkB", "BulkD"}, "back": {"/rules"}}, bo, "same-origin")
	res := bulkOf(mustQuery(t, hd.Get("Location")))
	if code != http.StatusSeeOther || res == nil || res.Done != 1 || res.Limit != 2 {
		t.Fatalf("%d %s: %+v", code, hd.Get("Location"), res)
	}
	// The single form, past them, says so too: 429, not a server's error.
	if code, body, _ := postAs(t, s.Handler(), "/proposals", url.Values{"rule": {"BulkD"}, "kind": {"for"}, "for": {"10m"}}, bo, "same-origin"); code != http.StatusTooManyRequests ||
		!strings.Contains(body, "as many proposals in the last hour as anyone may") {
		t.Errorf("the single form past them: %d", code)
	}
	_, page := getAs(t, s.Handler(), hd.Get("Location"), bo)
	if !strings.Contains(page, "1 proposed; 2 refused: past the 20 proposals an hour anyone may make") {
		t.Error("the list doesn't say the hour's proposals were made")
	}
}

func mustQuery(t *testing.T, u string) url.Values {
	t.Helper()
	x, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	return x.Query()
}

// TestBulkProposeTurns: what a rule's page reads for its forms (the
// suggestions, the spare rules) a bulk proposal reads under a page's turn,
// as the page does, waiting for one when every one is taken; and gives it
// back before any proposal waits for the heavy turn, which it waits for as
// the single form does: held past the wait, the first fails and the rest
// aren't tried, as a bulk write stops on an error of another kind.
func TestBulkProposeTurns(t *testing.T) {
	s := proposeServer(t, nil)
	h := s.Handler()
	was := busyWait
	defer func() { busyWait = was }()
	busyWait = 300 * time.Millisecond
	form := url.Values{"do": {"propose"}, "propose": {"retire"}, "rule": {"BulkA", "BulkB", "BulkD"}, "back": {"/rules"}}
	for range pageRenders {
		s.renders <- struct{}{}
	}
	s.heavy <- struct{}{} // another heavy request's
	done := make(chan http.Header)
	go func() {
		_, _, hd := postAs(t, h, "/rules/bulk", form, bo, "same-origin")
		done <- hd
	}()
	time.Sleep(100 * time.Millisecond)
	if heavyWaiting.Load() != 0 {
		t.Fatal("it waits for the heavy turn before it has read what the pages read")
	}
	<-s.renders // a page's turn given back: it takes it, reads, gives it back
	for began := time.Now(); heavyWaiting.Load() < 1; time.Sleep(5 * time.Millisecond) {
		if time.Since(began) > 5*time.Second {
			t.Fatal("not waiting for the heavy turn")
		}
	}
	if len(s.renders) != pageRenders-1 {
		t.Errorf("%d page turns while it waits for the heavy one, not the %d others hold", len(s.renders), pageRenders-1)
	}
	hd := <-done
	if res := bulkOf(mustQuery(t, hd.Get("Location"))); res == nil || res.Failed != 1 || res.Untried != 2 || res.Done != 0 {
		t.Errorf("with the heavy turn held: %s", hd.Get("Location"))
	}
	<-s.heavy
	for range pageRenders - 1 {
		<-s.renders
	}
	_, _, hd = postAs(t, h, "/rules/bulk", form, bo, "same-origin")
	if res := bulkOf(mustQuery(t, hd.Get("Location"))); res == nil || res.Done != 3 {
		t.Errorf("with every turn free: %s", hd.Get("Location"))
	}
	if len(s.heavy) != 0 || len(s.renders) != 0 {
		t.Error("a turn isn't given back")
	}
}

// TestBulkProposeTimeUp: a bulk proposal starts no rule once the time for
// its request is up (proposeTime), its wait for the heavy turn ending then
// too: the rest are "not tried: time's up", the answer comes back within
// it, and nothing is logged as an error.
func TestBulkProposeTimeUp(t *testing.T) {
	s := proposeServer(t, nil)
	var logs strings.Builder
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	was, wasBusy := proposeTime, busyWait
	defer func() { proposeTime, busyWait = was, wasBusy }()
	proposeTime, busyWait = 300*time.Millisecond, 10*time.Second
	s.heavy <- struct{}{} // another heavy request's, for longer than that
	form := url.Values{"do": {"propose"}, "propose": {"for"}, "for": {"10m"}, "rule": {"BulkA", "BulkB", "BulkD"}, "back": {"/rules"}}
	began := time.Now()
	_, _, hd := postAs(t, s.Handler(), "/rules/bulk", form, bo, "same-origin")
	took := time.Since(began)
	<-s.heavy
	if res := bulkOf(mustQuery(t, hd.Get("Location"))); res == nil || res.Late != 3 || res.Failed != 0 || res.Done != 0 {
		t.Errorf("time's up: %s", hd.Get("Location"))
	}
	if took > 3*time.Second {
		t.Errorf("answered after %s", took)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("logged: %s", logs.String())
	}
	// A request ended (a proxy's timeout, the reader gone) is logged.
	s.heavy <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/rules/bulk", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("X-Forwarded-Email", bo)
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	s.Handler().ServeHTTP(httptest.NewRecorder(), r)
	<-s.heavy
	if l := logs.String(); !strings.Contains(l, "a proposal for the rules picked stopped: its request ended") || !strings.Contains(l, "failed=") ||
		!strings.Contains(l, `"not tried, time up"=`) {
		t.Errorf("a request ended isn't logged, with what became of each: %s", l)
	}
}

// TestBulkProposeBar: the bar's proposal is an editor's; whoever may not
// sees it disabled, with why, said once: read-only, a viewer, a team's
// editor, and a lapilli serve with no rules of its own, to anyone.
func TestBulkProposeBar(t *testing.T) {
	s := proposeServer(t, nil)
	for who, want := range map[string]string{
		bo:                `<button class="quiet-button" name="do" value="propose">Propose</button>`,
		"vi@example.com":  `<button class="quiet-button" type="button" disabled aria-describedby="bulk-why">Propose</button>`,
		"pat@example.com": `<button class="quiet-button" type="button" disabled aria-describedby="bulk-team-why">Propose</button>`,
		"":                `<button class="quiet-button" type="button" disabled aria-describedby="bulk-why">Propose</button>`,
	} {
		// A team's editor sees their own team's rules unless they pick all.
		_, body := getAs(t, s.Handler(), "/rules?weeks=12&team=", who)
		if !strings.Contains(body, want) {
			t.Errorf("%q: no %s", who, want)
		}
		if who != bo && !strings.Contains(body, `<select name="propose" disabled`) {
			t.Errorf("%q: what to propose isn't disabled", who)
		}
	}
	s.cfg.RulesPath, s.cfg.Alerts = "", true // no rules of its own
	s.cache.Lock()
	s.cache.gen = ""
	s.cache.Unlock()
	_, body := getAs(t, s.Handler(), "/rules?weeks=12", bo)
	if !strings.Contains(body, `<button class="quiet-button" type="button" disabled aria-describedby="bulk-propose-why">Propose</button> <span id="bulk-propose-why" class="why">Nothing to propose: this lapilli serve reads no rules of its own</span>`) {
		t.Error("with no rules of its own, proposing isn't disabled with why")
	}
	if code, _, _ := postAs(t, s.Handler(), "/rules/bulk", url.Values{"do": {"propose"}, "propose": {"retire"}, "rule": {"BulkA"}}, bo, "same-origin"); code != http.StatusBadRequest {
		t.Errorf("proposing with no rules of its own: %d", code)
	}
}

// TestBulkProposeResult: what a bulk proposal did is read within bounds,
// and what it says was proposed is what the reader proposed: of the
// proposals made= names, those that are theirs, counted and linked, not
// another's nor what the address claims; twenty at most; and none of the
// page's links or forms carries it on.
func TestBulkProposeResult(t *testing.T) {
	s := proposeServer(t, nil)
	ctx := context.Background()
	mine := func(rule, by string, agent bool) string {
		id, err := s.l.Propose(ctx, ledger.Proposal{Rule: rule, Kind: ledger.KindFor, Path: ledger.PathNone, MadeBy: by, Agent: agent})
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(id)
	}
	a, b, other, agent := mine("Mine1", bo, false), mine("Mine2", bo, false), mine("Theirs", cy, false), mine("Agents", bo, true)
	h := s.Handler()
	for q, want := range map[string]string{
		"bulk=propose&done=2&made=" + a + "&made=" + b + "&already=1&nosuggest=1": `2 proposed; 1 had a proposal not finished with already; 1 had no for suggested for them <a href="/proposals?id=` + a + `&amp;id=` + b + `&amp;state=all&amp;weeks=12">The proposals made`,
		// What the address claims, and another's, or an agent's, aren't the reader's.
		"bulk=propose&done=20&made=" + other:                  "",
		"bulk=propose&done=20":                                "",
		"bulk=propose&done=3&same=1&made=" + other:            "0 proposed; 1 had that for already</p>",
		"bulk=propose&done=2&made=" + a + "&made=" + agent:    `1 proposed <a href="/proposals?id=` + a + `&amp;state=all&amp;weeks=12">`,
		"bulk=propose&done=3&made=" + a + "&made=" + a + "&q": `1 proposed <a href="/proposals?id=` + a + `&amp;state`,
		"bulk=propose&absent=1&unreplayed=1&same=1&refused=1&limit=2&tries=1&failed=1&untried=3&late=2": "0 proposed; 1 had that for already; 1 not replayed: what a change would do to them can&#39;t be checked; " +
			"1 not in effect here: nothing to change; 2 refused: past the 20 proposals an hour anyone may make; 1 refused: propose each from its page to see why; " +
			"1 not read: you have asked for as many proposals in the last hour as anyone may, made or refused; 1 failed: try again; stopped there: 3 not tried; " +
			"the time for one request was up: 2 not tried; pick them again</p>",
		"bulk=propose&done=1&made=7&made=8": "",
		"bulk=task&done=1&made=7":           "",
		"bulk=propose&done=1&made=x":        "",
		"bulk=propose&done=1&made=-3":       "",
		"bulk=propose&done=101":             "",
		"bulk=propose&editors=3":            "0 proposed; 3 refused: editors only</p>",
	} {
		_, body := getAs(t, h, "/rules?weeks=12&"+q, bo)
		said := strings.Contains(body, `role="status">`)
		if want == "" && said || want != "" && !strings.Contains(body, want) {
			t.Errorf("%s: says %v, want %q", q, said, want)
		}
		for _, carried := range []string{"made=", "made%3D", `name="made"`, "bulk=", "bulk%3D"} {
			if strings.Contains(body, carried) {
				t.Errorf("%s: a link or a form carries %s on", q, carried)
			}
		}
	}
	// Twenty at most, in code: twenty-one names say nothing.
	q := url.Values{"bulk": {"propose"}, "done": {"21"}}
	for i := range maxPropose + 1 {
		q.Add("made", fmt.Sprint(i+1))
	}
	if bulkOf(q) != nil {
		t.Error("twenty-one made= are read")
	}
}

// TestProposeWhyKept: every refusal propose gives, with what it calls,
// is one proposeWhy classifies, of the rule or stopping the bulk, and every
// one it classifies is one propose gives: a text added or changed there
// fails here until it is said which it is. And each is counted by its why.
func TestProposeWhyKept(t *testing.T) {
	reached := refusalsOf(t, "propose")
	for text, in := range reached {
		if _, ok := proposeWhy[text]; !ok {
			t.Errorf("propose (%v) refuses with %q, which a bulk proposal doesn't classify: of the rule, or stop", in, text)
		}
	}
	for text := range proposeWhy {
		if _, ok := reached[text]; !ok {
			t.Errorf("proposeWhy classifies %q, which propose doesn't refuse with", text)
		}
	}
	s, _ := server(t, nil)
	r, _ := http.NewRequest(http.MethodPost, "/rules/bulk", nil)
	for _, c := range []struct {
		err  error
		want string
		stop bool
	}{
		{bad("no rule in effect is called %q", "X"), "absent=1", false},
		{bad("it isn't replayed (%s): no backtest could check a change to it", "y"), "unreplayed=1", false},
		{bad("that is its for already"), "same=1", false},
		{ledger.ErrProposed, "already=1", false},
		{bad("it has been off since %s: four weeks aren't up", "z"), "refused=1", false},
		{bad("a text no one gave"), "refused=1", false},
		// The reader's: the rest are counted so, not read.
		{ledger.ErrTooManyProposals, "limit=3", true},
		{errTooManyTries, "tries=3", true},
		// The server's or the request's: the rest not tried.
		{bad("nothing has been replayed yet to backtest it against: try again after the first run"), "failed=1&untried=2", true},
		{bad("another comparison is running: try again in a minute"), "failed=1&untried=2", true},
		{errBusy, "failed=1&untried=2", true},
		{errors.New("disk on fire"), "failed=1&untried=2", true},
	} {
		var b bulkResult
		stop := s.proposeCount(&b, r, c.err, 3)
		got := bulkBack("/rules", "/rules", "propose", b)
		if !strings.Contains(got, c.want) || stop != c.stop {
			t.Errorf("%v: %s, stop %v", c.err, got, stop)
		}
	}
}

// refusalsOf are the texts bad is called with, as written, in the function
// or method of this package named root and what it calls of this package,
// or refers to as a method value, transitively (by name: a method of any
// type with the name), with where. A function passed by its bare name, as
// a value, isn't followed. A refusal it can't read the text of (bad
// called with other than a string as written, a badRequest made by hand)
// fails the test: say it in proposeWhy's terms, or write it as a string.
func refusalsOf(t *testing.T, root string) map[string][]string {
	t.Helper()
	fset := gotoken.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := map[string][]*ast.FuncDecl{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				funcs[fd.Name.Name] = append(funcs[fd.Name.Name], fd)
			}
		}
	}
	out := map[string][]string{}
	seen := map[string]bool{}
	var walk func(name string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for _, fd := range funcs[name] {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name != "bad" && funcs[id.Name] != nil {
						walk(id.Name)
					}
					// A method called on whatever else (a target, a value of
					// another type of this package).
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && funcs[sel.Sel.Name] != nil {
						walk(sel.Sel.Name)
					}
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "bad" && len(x.Args) > 0 {
						lit, ok := x.Args[0].(*ast.BasicLit)
						if !ok || lit.Kind != gotoken.STRING {
							t.Errorf("%s: %s refuses with bad and a text not written there", fset.Position(x.Pos()), name)
							return true
						}
						text, _ := strconv.Unquote(lit.Value)
						out[text] = append(out[text], name)
					}
				case *ast.CompositeLit:
					if id, ok := x.Type.(*ast.Ident); ok && id.Name == "badRequest" {
						t.Errorf("%s: %s makes a badRequest by hand", fset.Position(x.Pos()), name)
					}
				case *ast.SelectorExpr:
					// Called, or the server's method as a value.
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "s" && funcs[x.Sel.Name] != nil {
						walk(x.Sel.Name)
					}
				}
				return true
			})
		}
	}
	walk(root)
	return out
}

// TestBulkProposeWritesBack: twenty proposals, each waiting for the heavy
// turn and its backtest, may take longer than the server gives an answer
// to be written (two minutes): each moves that on, so the reader is sent
// back to the list, not shown an error for what was made.
func TestBulkProposeWritesBack(t *testing.T) {
	s := proposeServer(t, nil)
	srv := httptest.NewUnstartedServer(s.Handler())
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()
	s.heavy <- struct{}{} // another heavy request's, for longer than that
	go func() {
		time.Sleep(time.Second)
		<-s.heavy
	}()
	form := url.Values{"do": {"propose"}, "propose": {"for"}, "for": {"10m"}, "rule": {"BulkA", "BulkB"}, "back": {"/rules"}}
	r, err := http.NewRequest(http.MethodPost, srv.URL+"/rules/bulk", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	r.Host = "example.com"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("X-Forwarded-Email", bo)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(r)
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	resp.Body.Close()
	if res := bulkOf(mustQuery(t, resp.Header.Get("Location"))); resp.StatusCode != http.StatusSeeOther || res == nil || res.Done != 2 {
		t.Errorf("%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// TestFirstProposeWhileOneWaits: "What to do first" offers the digest's
// suggested for as "Propose it" until the rule has a proposal not
// finished with; then, as the ledger takes one a rule at a time, the
// button is drawn disabled for everyone, with why and a link to the one
// that waits, in each language, and no form makes another.
func TestFirstProposeWhileOneWaits(t *testing.T) {
	// Its week's record kept: a suggestion is shown for a week with one.
	s := proposeServer(t, nil)
	keepRecordIn(t, s)
	h := s.Handler()
	form := `<input type="hidden" name="from" value="suggestion"><input type="hidden" name="for" value="30m"><button>Propose it</button>`
	if _, body := getAs(t, h, "/rules/BulkA", bo); !strings.Contains(body, form) || strings.Contains(body, `id="first-propose-why"`) {
		t.Fatalf("before a proposal, BulkA's first step isn't its form:\n%s", firstStep(body))
	}
	code, _, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"BulkA"}, "kind": {"for"}, "for": {"30m"}, "from": {"suggestion"}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing BulkA's suggestion: %d", code)
	}
	id := strings.TrimPrefix(hd.Get("Location"), "/proposals/")
	for _, who := range []string{bo, "vi@example.com"} {
		for _, n := range names {
			r := httptest.NewRequest(http.MethodGet, "/rules/BulkA", nil)
			r.Header.Set("X-Forwarded-Email", who)
			r.Header.Set("Accept-Language", n.Code)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			first := firstStep(w.Body.String())
			off := `<span class="action-off"><button type="button" disabled aria-describedby="first-propose-why">` + tr(n.Code, "Propose it") + `</button> <span id="first-propose-why" class="why">` +
				tr(n.Code, "this rule has a proposal not yet finished with: one at a time") + `</span></span> <a href="/proposals/` + id + `">#` + id + `</a>`
			if !strings.Contains(first, off) || strings.Contains(first, `value="suggestion"`) {
				t.Errorf("%s, %s: with #%s waiting, the first step is\n%s", who, n.Code, id, first)
			}
		}
	}
}

// TestProposeFormsWhileOneWaits: with a proposal for the rule not finished
// with, every form on its page that would propose another (a new for, a
// threshold, what it says, retiring it on the Proposals tab; a fix to what
// it says on the summary) is drawn as it is, its fields and its button
// disabled, the button tied to why, said once above them with a link to
// the one that waits: for an editor and a viewer alike, in each language.
func TestProposeFormsWhileOneWaits(t *testing.T) {
	s := proposeServer(t, func(c *Config) { c.CheckContent = true })
	h := s.Handler()
	// Before: an editor's forms send.
	if _, body := getAs(t, h, "/rules/Errors?tab=proposals", bo); !strings.Contains(body, `<button>Propose it, with its backtest</button>`) || strings.Contains(body, `id="propose-open"`) {
		t.Fatal("before a proposal, the Proposals tab's forms aren't an editor's to send")
	}
	if _, body := getAs(t, h, "/rules/Errors", bo); !strings.Contains(body, `<button>Propose it, for a person to approve</button>`) {
		t.Fatal("before a proposal, the summary has no fix to what Errors says to send")
	}
	id := itoa(proposeFor(t, h, "15m"))
	for _, who := range []string{bo, "vi@example.com"} {
		for _, n := range names {
			why := tr(n.Code, "this rule has a proposal not yet finished with: one at a time")
			for _, c := range []struct{ path, section, said string }{
				{"/rules/Errors?tab=proposals", "propose", "propose-open"},
				{"/rules/Errors", "says", "says-open"},
			} {
				r := httptest.NewRequest(http.MethodGet, c.path, nil)
				r.Header.Set("X-Forwarded-Email", who)
				r.Header.Set("Accept-Language", n.Code)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				doc, err := html.Parse(w.Body)
				if err != nil {
					t.Fatal(err)
				}
				var sec *html.Node
				for _, x := range allNodes(doc) {
					if x.DataAtom == atom.Section && nodeAttr(x, "id") == c.section {
						sec = x
					}
				}
				if sec == nil {
					t.Fatalf("%s, %s, %s: no section #%s", who, n.Code, c.path, c.section)
				}
				var said string
				forms, buttons, fields := 0, 0, 0
				for _, x := range allNodes(sec) {
					switch {
					case nodeAttr(x, "id") == c.said:
						said = nodeText(x)
						if a := nodeAttr(x.LastChild, "href"); a != "/proposals/"+id {
							t.Errorf("%s, %s, %s: why links to %q, not #%s", who, n.Code, c.path, a, id)
						}
					case x.DataAtom == atom.Form:
						forms++
					case x.DataAtom == atom.Button:
						buttons++
						if !hasNodeAttr(x, "disabled") || nodeAttr(x, "aria-describedby") != c.said {
							t.Errorf("%s, %s, %s: the button %q isn't disabled and tied to why", who, n.Code, c.path, nodeText(x))
						}
					case (x.DataAtom == atom.Input && nodeAttr(x, "type") != "hidden") || x.DataAtom == atom.Select || x.DataAtom == atom.Textarea:
						fields++
						if !hasNodeAttr(x, "disabled") {
							t.Errorf("%s, %s, %s: the field %s isn't disabled", who, n.Code, c.path, nodeAttr(x, "name"))
						}
					}
				}
				if said != why+" #"+id {
					t.Errorf("%s, %s, %s: why says %q", who, n.Code, c.path, said)
				}
				// The tab's forms: a for, a threshold, what it says, and
				// retiring it; the summary's, a fix for each fact found
				// (the missing runbook's among them).
				if want := map[string]int{"propose": 4, "says": 1}[c.section]; buttons < want || fields < want || forms < want-1 {
					t.Errorf("%s, %s, %s: %d forms, %d buttons, %d fields drawn", who, n.Code, c.path, forms, buttons, fields)
				}
			}
		}
	}
}

// firstStep is a rule page's "What to do first", or the page when it has none.
func firstStep(body string) string {
	i := strings.Index(body, `id="first"`)
	if i < 0 {
		return body
	}
	j := strings.Index(body[i:], "</section>")
	return body[i : i+j]
}
