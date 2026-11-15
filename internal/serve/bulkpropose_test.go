package serve

import (
	"context"
	"errors"
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
		}, "bulk=propose&done=1&nosuggest=5"},
		{"retire", "retiring", url.Values{"propose": {"retire"}}, func(t *testing.T, h http.Handler, rule string) url.Values {
			for _, k := range []string{ledger.KindDisable, ledger.KindDelete} {
				if f := pageForm(t, h, "/rules/"+rule+"?tab=proposals", k, false); f != nil {
					return f
				}
			}
			return nil
		}, "bulk=propose&done=3&already=2&absent=1"},
	} {
		bulk := proposeServer(t, nil)
		one := proposeServer(t, nil)
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

// TestBulkProposeTurns: each proposal waits for the heavy turn as the
// single form's does, holding no turn to work out a page: with every page
// turn taken, it is made; with the heavy turn held past the wait, the first
// fails and the rest aren't tried, as a bulk write stops on an error of
// another kind.
func TestBulkProposeTurns(t *testing.T) {
	s := proposeServer(t, nil)
	h := s.Handler()
	was := busyWait
	defer func() { busyWait = was }()
	busyWait = 300 * time.Millisecond
	form := url.Values{"do": {"propose"}, "propose": {"for"}, "for": {"10m"}, "rule": {"BulkA", "BulkB", "BulkD"}, "back": {"/rules"}}
	// Every page's turn taken: it needs none.
	for range pageRenders {
		s.renders <- struct{}{}
	}
	s.heavy <- struct{}{} // another heavy request's
	done := make(chan http.Header)
	go func() {
		_, _, hd := postAs(t, h, "/rules/bulk", form, bo, "same-origin")
		done <- hd
	}()
	for began := time.Now(); heavyWaiting.Load() < 1; time.Sleep(5 * time.Millisecond) {
		if time.Since(began) > 5*time.Second {
			t.Fatal("not waiting for the heavy turn")
		}
	}
	if len(s.renders) != pageRenders {
		t.Errorf("%d page turns while it waits", len(s.renders))
	}
	hd := <-done
	if res := bulkOf(mustQuery(t, hd.Get("Location"))); res == nil || res.Failed != 1 || res.Untried != 2 || res.Done != 0 {
		t.Errorf("with the heavy turn held: %s", hd.Get("Location"))
	}
	<-s.heavy
	go func() {
		_, _, hd := postAs(t, h, "/rules/bulk", form, bo, "same-origin")
		done <- hd
	}()
	hd = <-done
	if res := bulkOf(mustQuery(t, hd.Get("Location"))); res == nil || res.Done != 3 {
		t.Errorf("with every page turn taken: %s", hd.Get("Location"))
	}
	for range pageRenders {
		<-s.renders
	}
	if len(s.heavy) != 0 {
		t.Error("the heavy turn isn't given back")
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

// TestBulkProposeResult: what a bulk proposal did is read within bounds:
// the proposals made only a bulk proposal's, as many as were made at most,
// each a number; and none of the page's links or forms carries it on.
func TestBulkProposeResult(t *testing.T) {
	s := proposeServer(t, nil)
	h := s.Handler()
	for q, want := range map[string]string{
		"bulk=propose&done=2&made=7&made=9&already=1&nosuggest=1": `2 proposed; 1 had a proposal not finished with already; 1 had no for suggested for them <a href="/proposals?id=7&amp;id=9&amp;state=all&amp;weeks=12">The proposals made`,
		"bulk=propose&done=1&absent=1&unreplayed=1&same=1&refused=1&limit=2&failed=1&untried=3": "1 proposed; 1 had that for already; 1 not replayed: what a change would do to them can&#39;t be checked; " +
			"1 not in effect here: nothing to change; 2 refused: past the 20 proposals an hour anyone may make; 1 refused: propose each from its page to see why; 1 failed: try again; stopped there: 3 not tried</p>",
		"bulk=propose&done=1&made=7&made=8":   "",
		"bulk=task&done=1&made=7":             "",
		"bulk=propose&done=1&made=x":          "",
		"bulk=propose&done=1&made=-3":         "",
		"bulk=propose&done=101":               "",
		"bulk=propose&editors=3":              "0 proposed; 3 refused: editors only</p>",
		"bulk=propose&done=3&made=7&made=7&q": "3 proposed",
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
}

// TestProposeWhyKept: each refusal a bulk proposal counts apart is one
// propose refuses with, as written: a text changed there would be counted
// as refused, without why.
func TestProposeWhyKept(t *testing.T) {
	var src strings.Builder
	for _, f := range []string{"changes.go", "changesweb.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src.Write(b)
	}
	for text := range proposeWhy {
		if !strings.Contains(src.String(), "bad("+fmt.Sprintf("%q", text)) {
			t.Errorf("propose refuses with no %q", text)
		}
	}
	// And each is counted by its why.
	s, _ := server(t, nil)
	r, _ := http.NewRequest(http.MethodPost, "/rules/bulk", nil)
	for _, c := range []struct {
		err  error
		want string
	}{
		{bad("no rule in effect is called %q", "X"), "absent=1"},
		{bad("it isn't replayed (%s): no backtest could check a change to it", "y"), "unreplayed=1"},
		{bad("that is its for already"), "same=1"},
		{ledger.ErrProposed, "already=1"},
		{ledger.ErrTooManyProposals, "limit=1"},
		{bad("it has been off since %s: four weeks aren't up", "z"), "refused=1"},
		{bad("another comparison is running: try again in a minute"), "failed=1"},
		{errBusy, "failed=1"},
		{errors.New("disk on fire"), "failed=1"},
	} {
		var b bulkResult
		stop := s.proposeCount(&b, r, c.err)
		got := bulkBack("/rules", "/rules", "propose", b)
		if !strings.Contains(got, c.want) || stop != strings.Contains(c.want, "failed") {
			t.Errorf("%v: %s, stop %v", c.err, got, stop)
		}
	}
}
