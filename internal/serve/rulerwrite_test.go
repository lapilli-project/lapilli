package serve

import (
	"context"
	"database/sql"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
)

// Changing rules through a managed Prometheus ruler (docs/design/rulers.md),
// each ruler against its stand-in: Mimir, Cortex at its own paths, Amazon
// and Azure. A real Mimir runs in test/e2e/changes.sh.

// rulerServer is a server reading the rules from a ruler's stand-in, with
// changes on, the namespaces or rule groups in may changed.
func rulerServer(t *testing.T, ruler string, h http.Handler, in []string, more func(*Config)) (*Server, *toStandIn) {
	t.Helper()
	fakeAuth(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	tr := &toStandIn{ts: ts}
	s, _ := changesServer(t, func(c *Config) {
		c.RulesPath, c.Ruler, c.RulerTransport, c.ChangesIn = "", ruler, tr, in
		if more != nil {
			more(c)
		}
	})
	return s, tr
}

// proposeRule proposes a change to a rule, as bo, from its page.
func proposeRule(t *testing.T, h http.Handler, rule, kind string, more url.Values) int64 {
	t.Helper()
	form := url.Values{"rule": {rule}, "kind": {kind}}
	for k, v := range more {
		form[k] = v
	}
	code, body, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", form)
	if code != http.StatusSeeOther {
		t.Fatalf("proposing %s for %s: %d %s", kind, rule, code, zzMsg(body))
	}
	return locID(hd)
}

const mimirShop = `name: api
interval: 1m
rules:
  # Pages the shop's on-call.
  - alert: Errors
    expr: errors > 5
    for: 2m
    labels: {severity: page}
  - record: job:errors:sum
    expr: sum(errors)
  - alert: Other
    expr: errors > 9
    for: 5m
    labels: {severity: page}
`

// mimirRuler is a Mimir with the shop's namespace (a group of two alert
// rules and a recording rule, and a group of one), a namespace a tool
// keeps with a rule of the same name, and a namespace of one rule.
func mimirRuler(t *testing.T, base string) *mimirStandIn {
	m := newMimir(t, base)
	for ns, g := range map[string]string{
		"shop":           mimirShop,
		"alloy/payments": "name: api\nrules:\n  - alert: Errors\n    expr: errors > 5\n    labels: {severity: page}\n",
		"solo":           "name: solo\nrules:\n  - alert: SoloDown\n    expr: up == 0\n",
	} {
		if err := m.put(ns, []byte(g)); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.put("shop", []byte("name: edge\nrules:\n  - alert: EdgeDown\n    expr: up == 0\n    for: 2m\n")); err != nil {
		t.Fatal(err)
	}
	return m
}

func mimirServer(t *testing.T, m *mimirStandIn, more func(*Config)) *Server {
	t.Helper()
	s, _ := rulerServer(t, "http://mimir:8080"+m.base, m, []string{"shop", "solo"}, func(c *Config) {
		c.RulerToken = tokenFile(t, m.readToken)
		c.ChangesRulerToken = tokenFile(t, m.writeToken)
		if more != nil {
			more(c)
		}
	})
	return s
}

// A rule in a Mimir's namespace that --changes-in names: proposed with the
// group as it would be written, applied once approved, its group posted
// with the write token alone, read back; undone, the rule put back as it
// was, the rest of the group as it was changed since. Cortex is the same
// API at its own paths.
func TestRulerMimir(t *testing.T) {
	for name, base := range map[string]string{"Mimir": "/prometheus/config/v1/rules", "Cortex": "/api/v1/rules"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			m := mimirRuler(t, base)
			s := mimirServer(t, m, nil)
			h := s.Handler()
			id := proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
			p := state(t, s, id)
			ev := proposalEvidence(p)
			if p.Path != ledger.PathRuler || ev.Edit == nil || ev.Edit.Namespace != "shop" || ev.Backtest == nil ||
				p.Source != "http://mimir:8080"+base+"/shop" {
				t.Fatalf("the proposal: %+v %+v", p, ev)
			}
			if n := changedLines(p.Diff); n != 2 || !strings.Contains(p.Diff, "+          for: 15m") {
				t.Errorf("the diff changes more than the for:\n%s", p.Diff)
			}
			if len(m.written()) != 0 {
				t.Fatalf("written before approval: %v", m.written())
			}
			// Its page says where it is kept.
			if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), ana, "", nil); !strings.Contains(body, "In the ruler: namespace shop, group api.") {
				t.Error("the page doesn't say where it is kept")
			}
			approve(t, h, id, ana)
			s.jobs(ctx, false)
			p = state(t, s, id)
			if p.State != ledger.StateApplied || !slices.Equal(m.written(), []string{"POST shop/api"}) || p.AfterHash == "" || p.Applied.IsZero() {
				t.Fatalf("applied: %+v, written %v", p, m.written())
			}
			if g := m.group("shop", "api"); !strings.Contains(g, "for: 15m") || !strings.Contains(g, "record: job:errors:sum") || !strings.Contains(g, "for: 5m") {
				t.Errorf("the group as written:\n%s", g)
			}
			if !strings.Contains(p.Backup, "for: 2m") || !strings.Contains(p.Backup, "name: api") {
				t.Errorf("the group as it was: %s", p.Backup)
			}
			// Someone changes another rule in the group; the change is
			// undone, and theirs stays.
			m.mu.Lock()
			m.put("shop", []byte(strings.Replace(strings.Replace(mimirShop, "for: 2m", "for: 15m", 1), "errors > 9", "errors > 11", 1)))
			m.mu.Unlock()
			if err := s.look(ctx, new(string)); err != nil {
				t.Fatal(err)
			}
			key := keyFor(t, h, id, ana, "undo")
			code, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}})
			if code != http.StatusSeeOther {
				t.Fatalf("undo: %d", code)
			}
			undo := locID(hd)
			s.jobs(ctx, false)
			if u, p := state(t, s, undo), state(t, s, id); u.State != ledger.StateApplied || p.State != ledger.StateUndone {
				t.Fatalf("undone: %s %s, the change %s", u.State, u.Reason, p.State)
			}
			if g := m.group("shop", "api"); !strings.Contains(g, "for: 2m") || !strings.Contains(g, "errors > 11") {
				t.Errorf("undone, the group:\n%s", g)
			}
			if m.group("shop", "edge") == "" || m.group("solo", "solo") == "" {
				t.Error("another group changed")
			}
		})
	}
}

// Every kind a Mimir takes: a threshold and what a rule says in place; a
// deletion of a group's last rule deletes the group, whatever else its
// namespace holds, and is undone by putting the group back; a namespace's
// last rule goes with its group. A rule file has no off.
func TestRulerMimirKinds(t *testing.T) {
	ctx := context.Background()
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	s := mimirServer(t, m, nil)
	h := s.Handler()
	carry := func(id int64) ledger.Proposal {
		t.Helper()
		approve(t, h, id, ana)
		s.jobs(ctx, false)
		if err := s.look(ctx, new(string)); err != nil {
			t.Fatal(err)
		}
		return state(t, s, id)
	}
	if p := carry(proposeRule(t, h, "Other", "threshold", url.Values{"bound": {"0"}, "value": {"11"}})); p.State != ledger.StateApplied ||
		!strings.Contains(m.group("shop", "api"), "errors > 11") {
		t.Errorf("a threshold: %s %s\n%s", p.State, p.Reason, m.group("shop", "api"))
	}
	if p := carry(proposeRule(t, h, "Other", "says", url.Values{"annotation": {"runbook_url"}, "text": {"https://wiki.example.com/other"}})); p.State != ledger.StateApplied ||
		!strings.Contains(m.group("shop", "api"), "runbook_url: https://wiki.example.com/other") {
		t.Errorf("what it says: %s %s\n%s", p.State, p.Reason, m.group("shop", "api"))
	}
	code, body, _ := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"EdgeDown"}, "kind": {"disable"}})
	if code == http.StatusSeeOther || !strings.Contains(zzMsg(body), "no off") {
		t.Errorf("turned off: %d %s", code, zzMsg(body))
	}
	del := proposeRule(t, h, "EdgeDown", "delete", nil)
	if p := carry(del); p.State != ledger.StateApplied || m.group("shop", "edge") != "" || m.group("shop", "api") == "" ||
		!slices.Contains(m.written(), "DELETE shop/edge") {
		t.Fatalf("a group's last rule: %s %s, written %v", p.State, p.Reason, m.written())
	}
	key := keyFor(t, h, del, ana, "undo")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(del)+"/undo", ana, "same-origin", url.Values{"key": {key}})
	s.jobs(ctx, false)
	if u := state(t, s, locID(hd)); u.State != ledger.StateApplied || !strings.Contains(m.group("shop", "edge"), "alert: EdgeDown") {
		t.Errorf("undone: %s %s\n%s", u.State, u.Reason, m.group("shop", "edge"))
	}
	if p := carry(proposeRule(t, h, "SoloDown", "delete", nil)); p.State != ledger.StateApplied || m.group("solo", "solo") != "" {
		t.Errorf("a namespace's last rule: %s %s", p.State, p.Reason)
	}
}

// A namespace --changes-in doesn't name is for a person, and its page
// says why, in each language; a name ending in * is a prefix.
func TestRulerNotNamed(t *testing.T) {
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	s := mimirServer(t, m, nil)
	h := s.Handler()
	id := proposeRule(t, h, "Errors [alloy%2Fpayments/api]", "for", url.Values{"for": {"15m"}})
	p := state(t, s, id)
	if p.Path != ledger.PathNone || proposalEvidence(p).NoPath != noPathIn {
		t.Fatalf("not named: %s, %q", p.Path, proposalEvidence(p).NoPath)
	}
	if keyFor(t, h, id, ana, "approve") != "" {
		t.Error("a change for a person offered for approval")
	}
	for lang, want := range map[string]string{"en": html.EscapeString(noPathIn), "ko": html.EscapeString(catalogs["ko"][noPathIn])} {
		r := httptest.NewRequest(http.MethodGet, "/proposals/"+itoa(id), nil)
		r.Header.Set("Accept-Language", lang)
		r.Header.Set("X-Forwarded-Email", ana)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: the page doesn't say why", lang)
		}
	}
	s2 := mimirServer(t, mimirRuler(t, "/prometheus/config/v1/rules"), func(c *Config) { c.ChangesIn = []string{"alloy/*"} })
	if p := state(t, s2, proposeRule(t, s2.Handler(), "Errors [alloy%2Fpayments/api]", "for", url.Values{"for": {"15m"}})); p.Path != ledger.PathRuler {
		t.Errorf("named by a prefix: %s %q", p.Path, proposalEvidence(p).NoPath)
	}
	for _, x := range noPaths {
		if _, ok := catalogs["ko"][x]; !ok {
			t.Errorf("ko has no translation for %q", x)
		}
	}
}

// What a ruler's change is checked against once approved, before anything
// is sent: the rule itself changed since is refused as stale; only other
// rules in its group changed since, it is made afresh, theirs as they now
// are; a namespace no longer named (a restart) is refused; and a unit
// that answers as another's is refused.
func TestRulerChecksAgain(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		set   func(*mimirStandIn, *Server)
		state string
		why   string
	}{
		"the rule changed": {func(m *mimirStandIn, _ *Server) {
			m.put("shop", []byte(strings.Replace(mimirShop, "errors > 5", "errors > 6", 1)))
		}, ledger.StateRefused, "the rule changed in the ruler"},
		"another rule changed": {func(m *mimirStandIn, _ *Server) {
			m.put("shop", []byte(strings.Replace(mimirShop, "errors > 9", "errors > 11", 1)))
		}, ledger.StateApplied, ""},
		"no longer named": {func(_ *mimirStandIn, s *Server) { s.cfg.ChangesIn = []string{"solo"} }, ledger.StateRefused, noPathIn},
		"the group gone":  {func(m *mimirStandIn, _ *Server) { delete(m.groups["shop"], "api") }, ledger.StateRefused, "gone"},
		"Mimir refuses it": {func(m *mimirStandIn, _ *Server) { m.reject = "errors > 5" }, ledger.StateFailed,
			"could not parse expression"},
	} {
		t.Run(name, func(t *testing.T) {
			m := mimirRuler(t, "/prometheus/config/v1/rules")
			s := mimirServer(t, m, nil)
			h := s.Handler()
			id := proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
			was := state(t, s, id)
			approve(t, h, id, ana)
			m.mu.Lock()
			c.set(m, s)
			m.mu.Unlock()
			s.jobs(ctx, false)
			p := state(t, s, id)
			if p.State != c.state || !strings.Contains(p.Reason, c.why) {
				t.Fatalf("%s: %+v", name, p)
			}
			switch c.state {
			case ledger.StateApplied:
				if g := m.group("shop", "api"); !strings.Contains(g, "for: 15m") || !strings.Contains(g, "errors > 11") ||
					p.DocHash == was.DocHash || !strings.Contains(p.Document, "errors > 11") {
					t.Errorf("made afresh:\n%s\n%s", g, p.Document)
				}
			case ledger.StateRefused:
				if w := m.written(); len(w) != 0 {
					t.Errorf("written %v", w)
				}
			}
		})
	}
}

// A ruler's write goes to --ruler's host alone: a redirect isn't followed,
// nothing reaches where it points, and the change fails.
func TestRulerRedirect(t *testing.T) {
	ctx := context.Background()
	var reached atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer elsewhere.Close()
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	m.redirect = elsewhere.URL + "/prometheus/config/v1/rules/shop"
	s := mimirServer(t, m, nil)
	h := s.Handler()
	id := proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateFailed || !strings.Contains(p.Reason, "redirect") {
		t.Errorf("a redirect: %+v", p)
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("%d requests elsewhere", n)
	}
}

// A change sent and then not read back stays applying, counted once, and
// is never sent again; read back later, it is applied. Paused just before
// the write, nothing is sent: it waits, approved.
func TestRulerReadBackAndPause(t *testing.T) {
	ctx := context.Background()
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	stop := filepath.Join(t.TempDir(), "paused")
	m.pauseOn = stop
	s := mimirServer(t, m, func(c *Config) { c.ChangesPaused = stop })
	h := s.Handler()
	id := proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
	approve(t, h, id, ana)
	m.armed.Store(true) // paused as the group is read again
	if err := s.carryOut(ctx); err != nil {
		t.Fatal(err)
	}
	if p := state(t, s, id); p.State != ledger.StateApproved || len(m.written()) != 0 {
		t.Fatalf("paused just before the write: %s, written %v", p.State, m.written())
	}
	os.Remove(stop)
	mark := time.Now().Add(-time.Second)
	// Written, then unreadable.
	s.ruler.Transport = &failAfterWrite{next: s.ruler.Transport, m: m}
	if err := s.carryOut(ctx); err != nil {
		t.Fatal(err)
	}
	p := state(t, s, id)
	if p.State != ledger.StateApplying || len(m.written()) != 1 || !strings.Contains(p.Reason, "not read back") {
		t.Fatalf("unreadable after the write: %+v, written %v", p, m.written())
	}
	if n, _ := s.l.Carried(ctx, mark, ""); n != 1 {
		t.Errorf("counted %d times", n)
	}
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplying || len(m.written()) != 1 {
		t.Fatalf("read again: %s, written %v", p.State, m.written())
	}
	m.failGets.Store(false)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied || len(m.written()) != 1 {
		t.Errorf("read back: %s %s, written %v", p.State, p.Reason, m.written())
	}
	if n, _ := s.l.Carried(ctx, mark, ""); n != 1 {
		t.Errorf("counted %d times", n)
	}
}

// changedLines are the lines a diff takes out or puts in.
func changedLines(diff string) int {
	n := 0
	for _, l := range strings.Split(diff, "\n") {
		if (strings.HasPrefix(l, "-") || strings.HasPrefix(l, "+")) && !strings.HasPrefix(l, "---") && !strings.HasPrefix(l, "+++") {
			n++
		}
	}
	return n
}

// failAfterWrite makes the stand-in fail every read once a write reached
// it.
type failAfterWrite struct {
	next http.RoundTripper
	m    *mimirStandIn
}

func (f *failAfterWrite) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := f.next.RoundTrip(r)
	if r.Method != http.MethodGet {
		f.m.failGets.Store(true)
	}
	return resp, err
}

const amazonShop = `# The shop's rules, kept here only.
groups:
  - name: api
    rules:
      - alert: Errors
        expr: errors > 5
        for: 2m
        labels:
          severity: page
      - alert: Other
        expr: errors > 9
  # The edge's.
  - name: edge
    rules:
      - alert: EdgeDown
        expr: up == 0
`

func amazonRuler(t *testing.T) *amazonStandIn {
	return &amazonStandIn{t: t, writer: "as arn:aws:iam::123456789012:role/lapilli-rules", slow: 1, ns: map[string]*amazonNS{
		"shop": {data: amazonShop, status: "ACTIVE", tags: map[string]string{"team": "shop"}, modified: 1},
		"stack": {data: "groups:\n  - name: s\n    rules:\n      - alert: Stacked\n        expr: up == 0\n", status: "ACTIVE", modified: 1,
			tags: map[string]string{"aws:cloudformation:stack-id": "arn:aws:cloudformation:us-east-1:123456789012:stack/s/1"}},
		"solo": {data: "groups:\n  - name: solo\n    rules:\n      - alert: SoloDown\n        expr: up == 0\n", status: "ACTIVE", modified: 1},
	}}
}

func amazonServer(t *testing.T, a *amazonStandIn, more func(*Config)) (*Server, *toStandIn) {
	return rulerServer(t, "https://aps.us-east-1.amazonaws.com/workspaces/ws-1", a, []string{"shop", "stack", "solo"}, func(c *Config) {
		c.Auth, c.ChangesAWSRole = "aws", "arn:aws:iam::123456789012:role/lapilli-rules"
		if more != nil {
			more(c)
		}
	})
}

// Amazon: the namespace written whole, with the change on the lines the
// rule is on, its comments and other groups byte for byte, signed for aps
// as the role given to write with; applying until the namespace is ACTIVE
// again. A namespace CloudFormation keeps, or tagged as --changes-managed
// says, is for a person, as is a namespace's last rule; a group's last
// rule is cut from the file, and put back by undo.
func TestRulerAmazon(t *testing.T) {
	ctx := context.Background()
	a := amazonRuler(t)
	s, tr := amazonServer(t, a, nil)
	h := s.Handler()
	id := proposeRule(t, h, "Errors", "for", url.Values{"for": {"15m"}})
	if p := state(t, s, id); p.Path != ledger.PathRuler || p.Source != "arn:aws:aps:us-east-1:123456789012:rulegroupsnamespace/ws-1/shop" {
		t.Fatalf("the proposal: %+v", p)
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplying || !strings.Contains(p.Reason, "updating") {
		t.Fatalf("while Amazon updates it: %+v", p)
	}
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied {
		t.Fatalf("once ACTIVE: %+v", p)
	}
	if got := a.data("shop"); got != strings.Replace(amazonShop, "for: 2m", "for: 15m", 1) {
		t.Errorf("the namespace as written:\n%s", got)
	}
	if len(a.tokens) != 1 || !strings.HasPrefix(a.tokens[0], "lapilli-"+itoa(id)+"-") {
		t.Errorf("client tokens %v", a.tokens)
	}
	for host := range tr.hosts {
		if host != "aps.us-east-1.amazonaws.com" {
			t.Errorf("asked %s", host)
		}
	}
	// What CloudFormation keeps, and a namespace's last rule.
	for rule, why := range map[string]string{"Stacked": noPathCFN, "SoloDown": noPathLastNS} {
		if p := state(t, s, proposeRule(t, h, rule, "delete", nil)); p.Path != ledger.PathNone || proposalEvidence(p).NoPath != why {
			t.Errorf("%s: %s %q", rule, p.Path, proposalEvidence(p).NoPath)
		}
	}
	// A group's last rule: cut from the file, and put back.
	del := proposeRule(t, h, "EdgeDown", "delete", nil)
	approve(t, h, del, ana)
	s.jobs(ctx, false)
	s.jobs(ctx, false)
	if p := state(t, s, del); p.State != ledger.StateApplied || strings.Contains(a.data("shop"), "edge") || !strings.Contains(a.data("shop"), "# The shop's rules") {
		t.Fatalf("a group's last rule: %+v\n%s", p, a.data("shop"))
	}
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	key := keyFor(t, h, del, ana, "undo")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(del)+"/undo", ana, "same-origin", url.Values{"key": {key}})
	s.jobs(ctx, false)
	s.jobs(ctx, false)
	if u := state(t, s, locID(hd)); u.State != ledger.StateApplied || !strings.Contains(a.data("shop"), "      - alert: EdgeDown\n        expr: up == 0\n") {
		t.Errorf("undone: %+v\n%s", u, a.data("shop"))
	}
	// --changes-managed names the shop's tag.
	s2, _ := amazonServer(t, amazonRuler(t), func(c *Config) { c.ChangesManaged = []string{"team=*"} })
	if p := state(t, s2, proposeRule(t, s2.Handler(), "Errors", "for", url.Values{"for": {"15m"}})); p.Path != ledger.PathNone || proposalEvidence(p).NoPath != noPathManaged {
		t.Errorf("tagged as managed: %s %q", p.Path, proposalEvidence(p).NoPath)
	}
}

// An Amazon namespace being written waits, approved and uncounted; one
// that failed is refused; a write Amazon fails is failed; a tag added
// since approval refuses it.
func TestRulerAmazonStates(t *testing.T) {
	ctx := context.Background()
	t.Run("updating", func(t *testing.T) {
		a := amazonRuler(t)
		s, _ := amazonServer(t, a, nil)
		h := s.Handler()
		id := proposeRule(t, h, "Errors", "for", url.Values{"for": {"15m"}})
		approve(t, h, id, ana)
		a.mu.Lock()
		a.ns["shop"].status, a.ns["shop"].updating = "UPDATING", 5
		a.mu.Unlock()
		mark := time.Now().Add(-time.Second)
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateApproved || len(a.writes) != 0 {
			t.Fatalf("while UPDATING: %+v", p)
		}
		if n, _ := s.l.Carried(ctx, mark, ""); n != 0 {
			t.Errorf("counted while it waited: %d", n)
		}
		a.mu.Lock()
		a.ns["shop"].status = "ACTIVE"
		a.mu.Unlock()
		s.jobs(ctx, false)
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateApplied || len(a.writes) != 1 {
			t.Errorf("once ACTIVE: %+v", p)
		}
	})
	for name, c := range map[string]struct {
		set   func(*amazonStandIn)
		state string
		why   string
	}{
		"failed before": {func(a *amazonStandIn) { a.ns["shop"].status, a.ns["shop"].reason = "UPDATE_FAILED", "bad file" }, ledger.StateRefused, "UPDATE_FAILED in Amazon: bad file"},
		"failed after":  {func(a *amazonStandIn) { a.fails = true }, ledger.StateFailed, "UPDATE_FAILED: the rules file has an error"},
		"tagged since":  {func(a *amazonStandIn) { a.ns["shop"].tags["aws:cloudformation:stack-id"] = "x" }, ledger.StateRefused, "CloudFormation"},
	} {
		t.Run(name, func(t *testing.T) {
			a := amazonRuler(t)
			s, _ := amazonServer(t, a, nil)
			h := s.Handler()
			id := proposeRule(t, h, "Errors", "for", url.Values{"for": {"15m"}})
			approve(t, h, id, ana)
			a.mu.Lock()
			c.set(a)
			a.mu.Unlock()
			s.jobs(ctx, false)
			if p := state(t, s, id); p.State != c.state || !strings.Contains(p.Reason, c.why) {
				t.Errorf("%s: %+v", name, p)
			}
		})
	}
}

func azureRuler(t *testing.T) *azureStandIn {
	errs := map[string]any{"alert": "Errors", "expression": "errors > 5", "for": "PT2M", "severity": 3, "labels": map[string]any{"team": "shop"}}
	other := map[string]any{"alert": "Other", "expression": "errors > 9", "for": "PT5M"}
	rec := map[string]any{"record": "job:errors:sum", "expression": "sum(errors)"}
	return &azureStandIn{t: t, writer: "as 11111111-2222-3333-4444-555555555555", groups: map[string]map[string]any{
		"rg-shop/shop":  azureGroupOf("rg-shop", "shop", map[string]any{"team": "shop"}, errs, rec, other),
		"rg-ops/solo":   azureGroupOf("rg-ops", "solo", map[string]any{}, map[string]any{"alert": "SoloDown", "expression": "up == 0"}),
		"rg-ops/kept":   azureGroupOf("rg-ops", "kept", map[string]any{"managed-by": "bicep"}, map[string]any{"alert": "Kept", "expression": "up == 0"}),
		"rg-other/else": azureGroupOf("rg-other", "else", map[string]any{}, map[string]any{"alert": "Else", "expression": "up == 0"}),
	}}
}

func azureServer(t *testing.T, z *azureStandIn, more func(*Config)) (*Server, *toStandIn) {
	return rulerServer(t, "https://management.azure.com/subscriptions/sub-1", z, []string{"rg-shop/shop", "RG-OPS/*"}, func(c *Config) {
		c.Auth, c.ChangesAzureClientID = "azure", "11111111-2222-3333-4444-555555555555"
		c.ChangesManaged = []string{"managed-by=bicep"}
		if more != nil {
			more(c)
		}
	})
}

// Azure: the rule group put whole, as it was read but for what Azure sets,
// with the one rule changed (a for as Azure writes it); turned off with
// the rule's own enabled, and deleted four weeks later, approved again; a
// rule group's last rule is for a person, as is one tagged as kept by a
// tool, or not named.
func TestRulerAzure(t *testing.T) {
	ctx := context.Background()
	z := azureRuler(t)
	s, tr := azureServer(t, z, nil)
	h := s.Handler()
	id := proposeRule(t, h, "Errors", "for", url.Values{"for": {"15m"}})
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied || z.rule("rg-shop/shop", "Errors")["for"] != "PT15M" {
		t.Fatalf("a for: %+v, %v", p, z.rule("rg-shop/shop", "Errors"))
	}
	body := z.bodies[0]
	if body["location"] != "koreacentral" || body["tags"].(map[string]any)["team"] != "shop" || len(body["properties"].(map[string]any)["rules"].([]any)) != 3 {
		t.Errorf("the rule group as written: %v", body)
	}
	for host := range tr.hosts {
		if host != "management.azure.com" {
			t.Errorf("asked %s", host)
		}
	}
	// Retired: off first, by the rule's own enabled, then deleted.
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	off := proposeRule(t, h, "Other", "disable", nil)
	approve(t, h, off, ana)
	s.jobs(ctx, false)
	if p := state(t, s, off); p.State != ledger.StateApplied || z.rule("rg-shop/shop", "Other")["enabled"] != false {
		t.Fatalf("turned off: %+v %v", p, z.rule("rg-shop/shop", "Other"))
	}
	if g := z.groups["rg-shop/shop"]["properties"].(map[string]any)["enabled"]; g != true {
		t.Errorf("the group's enabled touched: %v", g)
	}
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.propose(ctx, ask{Rule: "Other", Kind: ledger.KindDelete, From: "off four weeks"}, ledger.Proposer, false, true); err == nil ||
		!strings.Contains(err.Error(), "four weeks aren't up") {
		t.Errorf("deleted too soon: %v", err)
	}
	s.now = func() time.Time { return time.Now().Add(5 * weekLen) }
	del, err := s.propose(ctx, ask{Rule: "Other", Kind: ledger.KindDelete, From: "off four weeks"}, ledger.Proposer, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if p := state(t, s, del); p.Path != ledger.PathRuler || p.Follows != off {
		t.Fatalf("the deletion: %+v", p)
	}
	approve(t, h, del, cy)
	s.jobs(ctx, false)
	if p := state(t, s, del); p.State != ledger.StateApplied || z.rule("rg-shop/shop", "Other") != nil || z.rule("rg-shop/shop", "Errors") == nil {
		t.Errorf("deleted: %+v", p)
	}
	s.now = func() time.Time { return now }
	// For a person: a rule group's last rule, one a tool keeps, one not named.
	for rule, why := range map[string]string{"Kept": noPathManaged, "Else": noPathInAzure} {
		if p := state(t, s, proposeRule(t, h, rule, "disable", nil)); p.Path != ledger.PathNone || proposalEvidence(p).NoPath != why {
			t.Errorf("%s: %s %q", rule, p.Path, proposalEvidence(p).NoPath)
		}
	}
	solo := proposeRule(t, h, "SoloDown", "disable", nil)
	approve(t, h, solo, ana)
	s.jobs(ctx, false)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(5 * weekLen) }
	last, err := s.propose(ctx, ask{Rule: "SoloDown", Kind: ledger.KindDelete, From: "off four weeks"}, ledger.Proposer, false, true)
	if p := state(t, s, last); err != nil || p.Path != ledger.PathNone || proposalEvidence(p).NoPath != noPathLastGroup {
		t.Errorf("a rule group's last rule: %v %+v", err, p)
	}
}

// Azure answers a rule group with another's id: refused, nothing written.
func TestRulerAzureElsewhere(t *testing.T) {
	ctx := context.Background()
	z := azureRuler(t)
	s, _ := azureServer(t, z, nil)
	h := s.Handler()
	id := proposeRule(t, h, "Errors", "for", url.Values{"for": {"15m"}})
	approve(t, h, id, ana)
	z.mu.Lock()
	z.otherID = true
	z.mu.Unlock()
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateRefused || len(z.writes) != 0 {
		t.Errorf("another's id: %+v, written %v", p, z.writes)
	}
}

// --changes-in: names, prefixes, and Azure's RG/NAME, or NAME when
// --ruler names one resource group, in any letter case.
func TestChangesIn(t *testing.T) {
	for _, c := range []struct {
		ruler     string
		in        []string
		ns, group string
		ok        bool
	}{
		{"http://mimir/rules", []string{"shop"}, "shop", "g", true},
		{"http://mimir/rules", []string{"shop"}, "Shop", "g", false},
		{"http://mimir/rules", []string{"team-*"}, "team-a", "g", true},
		{"http://mimir/rules", []string{"team-*"}, "tea", "g", false},
		{"http://mimir/rules", []string{"alloy/payments"}, "alloy/payments", "g", true},
		{"https://aps.us-east-1.amazonaws.com/workspaces/ws-1", []string{"shop"}, "shop", "g", true},
		{"https://management.azure.com/subscriptions/s", []string{"RG-Shop/Shop"}, "rg-shop", "shop", true},
		{"https://management.azure.com/subscriptions/s", []string{"shop"}, "rg-shop", "shop", false},
		{"https://management.azure.com/subscriptions/s", []string{"rg-shop/*"}, "rg-shop", "any", true},
		{"https://management.azure.com/subscriptions/s/resourceGroups/rg-shop", []string{"shop"}, "rg-shop", "shop", true},
		{"https://management.azure.com/subscriptions/s/resourceGroups/rg-shop", []string{"sh*"}, "rg-shop", "Shop", true},
	} {
		r, err := source.NewRuler(c.ruler, "test")
		if err != nil {
			t.Fatal(err)
		}
		s := &Server{cfg: Config{ChangesIn: c.in}, ruler: r}
		if got := s.changesIn(c.ns, c.group); got != c.ok {
			t.Errorf("%s %v %s/%s: %v", c.ruler, c.in, c.ns, c.group, got)
		}
	}
}

// What a ruler holds is read again before every write, and what was there
// is kept: undo puts the rule back from it, never from the ledger's
// version of the rules.
func TestRulerBackupIsTheUnit(t *testing.T) {
	ctx := context.Background()
	a := amazonRuler(t)
	s, _ := amazonServer(t, a, nil)
	h := s.Handler()
	id := proposeRule(t, h, "Other", "says", url.Values{"annotation": {"summary"}, "text": {"Errors over nine"}})
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	s.jobs(ctx, false)
	p := state(t, s, id)
	if p.State != ledger.StateApplied || p.Backup != amazonShop {
		t.Fatalf("kept: %+v", p)
	}
	if !strings.Contains(a.data("shop"), `summary: "Errors over nine"`) {
		t.Errorf("as written:\n%s", a.data("shop"))
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/api/v1/proposals/"+itoa(id), ana, "", nil); strings.Contains(body, "backup") {
		t.Error("what it replaced is in the API's answer")
	}
	rs, err := rules.Parse([]byte(a.data("shop")))
	if err != nil || len(rs) != 3 {
		t.Errorf("the namespace reads as %d rules: %v", len(rs), err)
	}
}

// A ruler's proposals' pages, in each language, find a translation for
// every text they show: one sent and not read back, which an admin can
// close, and one for a person, with why.
func TestRulerPagesInEachLanguage(t *testing.T) {
	ctx := context.Background()
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	s := mimirServer(t, m, nil)
	h := s.Handler()
	person := proposeRule(t, h, "Errors [alloy%2Fpayments/api]", "for", url.Values{"for": {"15m"}})
	applying := proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
	approve(t, h, applying, ana)
	s.ruler.Transport = &failAfterWrite{next: s.ruler.Transport, m: m}
	s.jobs(ctx, false)
	if p := state(t, s, applying); p.State != ledger.StateApplying {
		t.Fatalf("not applying: %+v", p)
	}
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	for _, n := range names {
		for _, path := range []string{"/proposals", "/proposals/" + itoa(applying), "/proposals/" + itoa(person)} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Accept-Language", n.Code)
			r.Header.Set("X-Forwarded-Email", ana)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Errorf("%s %s: %d", n.Code, path, w.Code)
			}
			if path == "/proposals/"+itoa(applying) && !strings.Contains(w.Body.String(), `name="unwritten"`) {
				t.Errorf("%s: no way to close it", n.Code)
			}
		}
	}
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
	// Closed by an admin only once read back again, and still not.
	code, body, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(applying)+"/drop", ana, "same-origin", url.Values{"reason": {"the ruler was rebuilt"}})
	if code == http.StatusSeeOther || !strings.Contains(zzMsg(body), "reach the ruler") {
		t.Errorf("closed without saying it didn't reach the ruler: %d %s", code, zzMsg(body))
	}
	m.failGets.Store(false)
	ui(t, h, http.MethodPost, "/proposals/"+itoa(applying)+"/drop", ana, "same-origin", url.Values{"reason": {"the ruler was rebuilt"}, "unwritten": {"1"}})
	if p := state(t, s, applying); p.State != ledger.StateApplied {
		t.Errorf("read back when closed: %+v", p)
	}
}

// Where a change is written is worked out again from --ruler: one made
// against another ruler, before a restart changed the flag, is refused,
// and nothing is written anywhere.
func TestRulerWrittenWhereRead(t *testing.T) {
	ctx := context.Background()
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	s := mimirServer(t, m, nil)
	h := s.Handler()
	id := proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
	approve(t, h, id, ana)
	other, err := source.NewRuler("http://other-mimir:8080/prometheus/config/v1/rules", "test")
	if err != nil {
		t.Fatal(err)
	}
	other.Transport, other.Reads = s.ruler.Transport, s.ruler.Reads
	s.ruler = other
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateRefused || !strings.Contains(p.Reason, "another ruler") || len(m.written()) != 0 {
		t.Errorf("another ruler's: %+v, written %v", p, m.written())
	}
}

// Undo puts back only a rule as the change left it: one changed in the
// ruler since, or a deleted one of its name there again, is refused, for a
// person to undo in the ruler.
func TestRulerUndoRefused(t *testing.T) {
	ctx := context.Background()
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	s := mimirServer(t, m, nil)
	h := s.Handler()
	undo := func(id int64) ledger.Proposal {
		t.Helper()
		key := keyFor(t, h, id, ana, "undo")
		_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}})
		s.jobs(ctx, false)
		return state(t, s, locID(hd))
	}
	id := proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	m.mu.Lock()
	m.put("shop", []byte(strings.Replace(mimirShop, "for: 2m", "for: 20m", 1)))
	m.mu.Unlock()
	if u := undo(id); u.State != ledger.StateRefused || !strings.Contains(u.Reason, "changed in the ruler since the change") {
		t.Errorf("changed since: %+v", u)
	}
	del := proposeRule(t, h, "EdgeDown", "delete", nil)
	approve(t, h, del, ana)
	s.jobs(ctx, false)
	m.mu.Lock()
	m.put("shop", []byte("name: edge\nrules:\n  - alert: EdgeDown\n    expr: up == 1\n"))
	m.mu.Unlock()
	if u := undo(del); u.State != ledger.StateRefused || !strings.Contains(u.Reason, "in its group again") {
		t.Errorf("a deletion, its name back: %+v", u)
	}
	if w := m.written(); len(w) != 2 {
		t.Errorf("written %v", w)
	}
}

// Someone else writes the group between Lapilli's read and its write: it
// reads back as neither it was nor as proposed, and fails, with what it
// was kept for a person.
func TestRulerWrittenMeanwhile(t *testing.T) {
	ctx := context.Background()
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	m.meanwhile = func(m *mimirStandIn) {
		m.put("shop", []byte(strings.Replace(mimirShop, "errors > 9", "errors > 12", 1)))
	}
	s := mimirServer(t, m, nil)
	h := s.Handler()
	id := proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateFailed || !strings.Contains(p.Reason, "neither") || !strings.Contains(p.Backup, "errors > 9") {
		t.Errorf("written meanwhile: %+v", p)
	}
}

// What a rule is to say is checked again just before it is written, as it
// was when proposed: a text changed in the ledger since (by whoever could
// reach its file) to one that calls query is refused, and nothing is
// written.
func TestRulerSaysCheckedAgain(t *testing.T) {
	ctx := context.Background()
	a := amazonRuler(t)
	s, _ := amazonServer(t, a, nil)
	h := s.Handler()
	id := proposeRule(t, h, "Other", "says", url.Values{"annotation": {"summary"}, "text": {"Errors over nine"}})
	approve(t, h, id, ana)
	db, err := sql.Open("sqlite", filepath.Join(s.cfg.Ledger, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "UPDATE proposals SET evidence = replace(evidence, 'Errors over nine', '{{ query `up` }}') WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateRefused || !strings.Contains(p.Reason, "query") || len(a.writes) != 0 {
		t.Errorf("a text changed since: %+v, written %v", p, a.writes)
	}
}

// undoNow asks for an applied change to be undone, as ana, carries the
// undo out, and gives it.
func undoNow(t *testing.T, s *Server, id int64, rounds int) ledger.Proposal {
	t.Helper()
	h := s.Handler()
	key := keyFor(t, h, id, ana, "undo")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}})
	for range rounds {
		s.jobs(context.Background(), false)
	}
	return state(t, s, locID(hd))
}

// A deletion undone after a person deleted the rest of its group in the
// ruler puts back a group holding that one rule, not the group as it was:
// the rules the person deleted stay deleted. Mimir: the group was deleted
// whole; Amazon: cut from its namespace.
func TestRulerUndoPutsBackOneRule(t *testing.T) {
	ctx := context.Background()
	t.Run("Mimir", func(t *testing.T) {
		m := mimirRuler(t, "/prometheus/config/v1/rules")
		s := mimirServer(t, m, nil)
		h := s.Handler()
		del := proposeRule(t, h, "Other", "delete", nil)
		approve(t, h, del, ana)
		s.jobs(ctx, false)
		m.mu.Lock()
		delete(m.groups["shop"], "api")
		m.mu.Unlock()
		if err := s.look(ctx, new(string)); err != nil {
			t.Fatal(err)
		}
		u := undoNow(t, s, del, 1)
		g := m.group("shop", "api")
		if u.State != ledger.StateApplied || !strings.Contains(g, "alert: Other") || !strings.Contains(g, "interval: 1m") ||
			strings.Contains(g, "Errors") || strings.Contains(g, "job:errors:sum") {
			t.Errorf("undone: %s %q\n%s", u.State, u.Reason, g)
		}
	})
	t.Run("Amazon", func(t *testing.T) {
		a := amazonRuler(t)
		s, _ := amazonServer(t, a, nil)
		h := s.Handler()
		del := proposeRule(t, h, "Other", "delete", nil)
		approve(t, h, del, ana)
		s.jobs(ctx, false)
		s.jobs(ctx, false)
		a.mu.Lock()
		a.ns["shop"].data = "# The shop's rules, kept here only.\ngroups:\n  # The edge's.\n  - name: edge\n    rules:\n      - alert: EdgeDown\n        expr: up == 0\n"
		a.ns["shop"].modified++
		a.mu.Unlock()
		if err := s.look(ctx, new(string)); err != nil {
			t.Fatal(err)
		}
		u := undoNow(t, s, del, 2)
		want := "# The shop's rules, kept here only.\ngroups:\n  - name: api\n    rules:\n      - alert: Other\n        expr: errors > 9\n" +
			"  # The edge's.\n  - name: edge\n    rules:\n      - alert: EdgeDown\n        expr: up == 0\n"
		if got := a.data("shop"); u.State != ledger.StateApplied || got != want {
			t.Errorf("undone: %s %q\n%s", u.State, u.Reason, got)
		}
	})
}

// Undo puts back a rule only as the change left it, every key of it: one
// given an Azure action group since, or a Mimir runbook link, neither of
// which its hash covers, is refused, and what was set since stays.
func TestRulerUndoKeepsWhatChangedSince(t *testing.T) {
	ctx := context.Background()
	t.Run("Azure", func(t *testing.T) {
		z := azureRuler(t)
		s, _ := azureServer(t, z, nil)
		h := s.Handler()
		id := proposeRule(t, h, "Errors", "for", url.Values{"for": {"15m"}})
		approve(t, h, id, ana)
		s.jobs(ctx, false)
		z.mu.Lock()
		r := z.groups["rg-shop/shop"]["properties"].(map[string]any)["rules"].([]any)[0].(map[string]any)
		r["actions"] = []any{map[string]any{"actionGroupId": "/subscriptions/sub-1/resourceGroups/rg-shop/providers/microsoft.insights/actionGroups/oncall"}}
		z.mu.Unlock()
		if err := s.look(ctx, new(string)); err != nil {
			t.Fatal(err)
		}
		if u := undoNow(t, s, id, 1); u.State != ledger.StateRefused || !strings.Contains(u.Reason, "changed in the ruler since the change") ||
			z.rule("rg-shop/shop", "Errors")["actions"] == nil {
			t.Errorf("an action group set since: %s %q, %v", u.State, u.Reason, z.rule("rg-shop/shop", "Errors"))
		}
	})
	t.Run("Mimir", func(t *testing.T) {
		m := mimirRuler(t, "/prometheus/config/v1/rules")
		s := mimirServer(t, m, nil)
		h := s.Handler()
		id := proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
		approve(t, h, id, ana)
		s.jobs(ctx, false)
		m.mu.Lock()
		m.put("shop", []byte(strings.Replace(strings.Replace(mimirShop, "for: 2m", "for: 15m", 1),
			"labels: {severity: page}\n  - record", "labels: {severity: page}\n    annotations: {runbook_url: https://wiki/errors}\n  - record", 1)))
		m.mu.Unlock()
		if err := s.look(ctx, new(string)); err != nil {
			t.Fatal(err)
		}
		if u := undoNow(t, s, id, 1); u.State != ledger.StateRefused || !strings.Contains(m.group("shop", "api"), "runbook_url") {
			t.Errorf("a runbook link set since: %s %q\n%s", u.State, u.Reason, m.group("shop", "api"))
		}
	})
}

// Someone gives the rule just changed an action group in Azure's portal
// before Lapilli reads it back: the rule read back isn't, whole, the one
// written, though its hash is, so the change fails, as neither, rather
// than pass as applied.
func TestRulerReadBackWholeEntries(t *testing.T) {
	ctx := context.Background()
	z := azureRuler(t)
	z.meanwhile = func(z *azureStandIn) {
		r := z.groups["rg-shop/shop"]["properties"].(map[string]any)["rules"].([]any)[0].(map[string]any)
		r["actions"] = []any{map[string]any{"actionGroupId": "oncall"}}
	}
	s, _ := azureServer(t, z, nil)
	h := s.Handler()
	id := proposeRule(t, h, "Errors", "for", url.Values{"for": {"15m"}})
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateFailed || !strings.Contains(p.Reason, "neither") {
		t.Errorf("written meanwhile: %+v", p)
	}
}

// Amazon's unit is a namespace: a deletion is undone though another group
// in it has a rule of the same name.
func TestRulerAmazonUndoDeleteOtherGroupSameName(t *testing.T) {
	ctx := context.Background()
	a := amazonRuler(t)
	a.ns["shop"].data = amazonShop + "  - name: edge-eu\n    rules:\n      - alert: EdgeDown\n        expr: up{region=\"eu\"} == 0\n"
	s, _ := amazonServer(t, a, nil)
	h := s.Handler()
	del := proposeRule(t, h, "EdgeDown [shop/edge]", "delete", nil)
	approve(t, h, del, ana)
	s.jobs(ctx, false)
	s.jobs(ctx, false)
	if p := state(t, s, del); p.State != ledger.StateApplied {
		t.Fatalf("deleted: %+v", p)
	}
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if u := undoNow(t, s, del, 2); u.State != ledger.StateApplied || !strings.Contains(a.data("shop"), "  - name: edge\n") {
		t.Errorf("undone: %s %q\n%s", u.State, u.Reason, a.data("shop"))
	}
}

// Someone who may write namespace alloy makes a group payments/api with an
// Errors of their own: it isn't named as namespace alloy/payments' Errors
// in group api, and a proposal for that one is that one's.
func TestRulerNamesDontCollide(t *testing.T) {
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	if err := m.put("alloy", []byte("name: payments/api\nrules:\n  - alert: Errors\n    expr: vector(1)\n    labels: {severity: page}\n")); err != nil {
		t.Fatal(err)
	}
	s := mimirServer(t, m, func(c *Config) { c.ChangesIn = []string{"alloy*", "shop"} })
	h := s.Handler()
	for name, ns := range map[string]string{"Errors [alloy%2Fpayments/api]": "alloy/payments", "Errors [alloy/payments%2Fapi]": "alloy"} {
		ev := proposalEvidence(state(t, s, proposeRule(t, h, name, "for", url.Values{"for": {"15m"}})))
		if ev.Edit == nil || ev.Edit.Namespace != ns {
			t.Errorf("%s: proposed for %+v", name, ev.Edit)
		}
	}
}

// A namespace whose name has a .. segment isn't written to, though
// --changes-in names it by a prefix: it is for a person.
func TestRulerDotSegments(t *testing.T) {
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	if err := m.put("team/../shop", []byte("name: g\nrules:\n  - alert: Dotted\n    expr: up == 0\n")); err != nil {
		t.Fatal(err)
	}
	s := mimirServer(t, m, func(c *Config) { c.ChangesIn = []string{"team/*"} })
	if p := state(t, s, proposeRule(t, s.Handler(), "Dotted", "delete", nil)); p.Path != ledger.PathNone || proposalEvidence(p).NoPath != noPathName {
		t.Errorf("a .. segment: %s %q", p.Path, proposalEvidence(p).NoPath)
	}
}

// failOneReadAfterWrite fails the first read after a write, once.
type failOneReadAfterWrite struct {
	next  http.RoundTripper
	armed atomic.Bool
	done  atomic.Bool
}

func (f *failOneReadAfterWrite) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && f.armed.Load() && f.done.CompareAndSwap(false, true) {
		return nil, errors.New("read timed out")
	}
	resp, err := f.next.RoundTrip(r)
	if r.Method != http.MethodGet {
		f.armed.Store(true)
	}
	return resp, err
}

// One change to a unit at a time: one sent and not yet read back holds up
// another to the same group, which waits, approved and uncounted, and goes
// once the first is read back; both are then applied, neither taken for
// the other's.
func TestRulerOneChangeToAUnitAtATime(t *testing.T) {
	ctx := context.Background()
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	s := mimirServer(t, m, nil)
	h := s.Handler()
	a := proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
	b := proposeRule(t, h, "Other", "for", url.Values{"for": {"10m"}})
	c := proposeRule(t, h, "EdgeDown", "for", url.Values{"for": {"3m"}}) // another group
	for _, id := range []int64{a, b, c} {
		approve(t, h, id, ana)
	}
	mark := time.Now().Add(-time.Second)
	s.ruler.Transport = &failOneReadAfterWrite{next: s.ruler.Transport}
	if err := s.carryOut(ctx); err != nil {
		t.Fatal(err)
	}
	if pa, pb, pc := state(t, s, a), state(t, s, b), state(t, s, c); pa.State != ledger.StateApplying || pb.State != ledger.StateApproved || pc.State != ledger.StateApplied {
		t.Fatalf("while the first isn't read back: %s, %s, %s", pa.State, pb.State, pc.State)
	}
	if n, _ := s.l.Carried(ctx, mark, ""); n != 2 {
		t.Errorf("counted %d", n)
	}
	if err := s.carryOut(ctx); err != nil {
		t.Fatal(err)
	}
	g := m.group("shop", "api")
	if pa, pb := state(t, s, a), state(t, s, b); pa.State != ledger.StateApplied || pb.State != ledger.StateApplied ||
		!strings.Contains(g, "for: 15m") || !strings.Contains(g, "for: 10m") {
		t.Errorf("both: %s %q, %s %q\n%s", pa.State, pa.Reason, pb.State, pb.Reason, g)
	}
}

// A change left applying is read back only from the ruler it was sent to:
// with --ruler pointed elsewhere since, it stays applying, saying so, and
// nothing is asked of the other ruler; pointed back, it is read back.
func TestRulerSettledWhereSent(t *testing.T) {
	ctx := context.Background()
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	s := mimirServer(t, m, nil)
	h := s.Handler()
	id := proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
	approve(t, h, id, ana)
	tr := s.ruler.Transport.(*toStandIn)
	s.ruler.Transport = &failAfterWrite{next: tr, m: m}
	s.jobs(ctx, false)
	m.failGets.Store(false)
	was := s.ruler
	other, err := source.NewRuler("http://other-mimir:8080/prometheus/config/v1/rules", "test")
	if err != nil {
		t.Fatal(err)
	}
	other.Transport, other.Reads = tr, was.Reads
	s.ruler = other
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplying || !strings.Contains(p.Reason, "--ruler no longer names") || tr.hosts["other-mimir:8080"] != 0 {
		t.Fatalf("read back elsewhere: %+v, asked %v", p, tr.hosts)
	}
	s.ruler = was
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied {
		t.Errorf("pointed back: %+v", p)
	}
}

// A Mimir that caches its rule storage gives the group as it was for a
// while after it took a change: it is read again, each round, for two
// minutes from when it was sent by the server's clock, however soon the
// rounds come (approving another change starts one at once), and only
// then called failed; read back changed meanwhile, it is applied.
func TestRulerMimirReadBackAgain(t *testing.T) {
	ctx := context.Background()
	for stale, want := range map[int]string{1: ledger.StateApplied, 1000: ledger.StateFailed} {
		m := mimirRuler(t, "/prometheus/config/v1/rules")
		m.stale = stale
		s := mimirServer(t, m, nil)
		s.now = time.Now
		h := s.Handler()
		id := proposeRule(t, h, "Errors [shop/api]", "for", url.Values{"for": {"15m"}})
		approve(t, h, id, ana)
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateApplying || p.Reason != asWasYet {
			t.Fatalf("stale %d, read back as it was: %+v", stale, p)
		}
		if err := s.carryOut(ctx); err != nil { // a round at once
			t.Fatal(err)
		}
		if p := state(t, s, id); stale > 1 && p.State != ledger.StateApplying {
			t.Fatalf("stale %d, read again at once: %s %q", stale, p.State, p.Reason)
		}
		s.now = func() time.Time { return time.Now().Add(readBackWithin + time.Minute) }
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != want || len(m.written()) != 1 {
			t.Errorf("stale %d, read again: %s %q, written %v", stale, p.State, p.Reason, m.written())
		}
	}
}

// A group Mimir writes in a form that can't be read back (an expression
// whose first line starts with spaces, as Mimir 3.2.4 writes it) is passed
// over, and logged: the other groups, in its namespace and the rest, are
// read and replayed, and changed. A group read but not read back as it
// was written is for a person.
func TestRulerReadsPastAGroup(t *testing.T) {
	ctx := context.Background()
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	if err := m.put("shop", []byte("name: lead\nrules:\n  - alert: Lead\n    expr: \"  up\\n== 0\\n\"\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.group("shop", "lead"), "|4") {
		t.Fatalf("not written as Mimir writes it:\n%s", m.group("shop", "lead"))
	}
	s := mimirServer(t, m, nil)
	h := s.Handler()
	if p := state(t, s, proposeRule(t, h, "Other", "for", url.Values{"for": {"10m"}})); p.Path != ledger.PathRuler {
		t.Fatalf("a rule in the namespace beside it: %+v", p)
	}
	if _, err := s.current(ctx, "Lead", false); err == nil {
		t.Error("the group that can't be read was read")
	}
	m.answer = func(ns, group string) []byte {
		if group == "edge" {
			return []byte("name: edge\nrules:\n    - alert: EdgeDown\n      expr: |4\n          up\n        == 0\n")
		}
		return nil
	}
	if p := state(t, s, proposeRule(t, h, "EdgeDown", "for", url.Values{"for": {"10m"}})); p.Path != ledger.PathNone || proposalEvidence(p).NoPath != noPathUnread {
		t.Errorf("a group not read back: %s %q", p.Path, proposalEvidence(p).NoPath)
	}
}

// An alert named as Lapilli names another apart ("Errors [shop/api]") is
// named apart itself: a proposal for "Errors [shop/api]" is for the rule
// in shop/api. Rules alike in group and labels share a name, and a
// proposal for it is refused: which one it is for would be a guess.
func TestRulerOwnNameDoesntCollide(t *testing.T) {
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	for ns, g := range map[string]string{
		"a-team": "name: g\nrules:\n  - alert: \"Errors [shop/api]\"\n    expr: vector(1)\n    labels: {severity: page}\n",
		"twins":  "name: g\nrules:\n  - alert: Twin\n    expr: up == 0\n  - alert: Twin\n    expr: up == 1\n",
	} {
		if err := m.put(ns, []byte(g)); err != nil {
			t.Fatal(err)
		}
	}
	s := mimirServer(t, m, func(c *Config) { c.ChangesIn = []string{"a-team", "shop", "twins"} })
	h := s.Handler()
	for name, ns := range map[string]string{"Errors [shop/api]": "shop", "Errors [shop/api] [a-team/g]": "a-team"} {
		if ev := proposalEvidence(state(t, s, proposeRule(t, h, name, "for", url.Values{"for": {"15m"}}))); ev.Edit == nil || ev.Edit.Namespace != ns {
			t.Errorf("%s: proposed for %+v", name, ev.Edit)
		}
	}
	code, body, _ := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Twin"}, "kind": {"delete"}})
	if code == http.StatusSeeOther || !strings.Contains(zzMsg(body), "more than one rule") {
		t.Errorf("twins: %d %s", code, zzMsg(body))
	}
}

// Azure gives a rule group's id with its resource group in another letter
// case: proposals are made as before, and carried out.
func TestRulerAzureIDCase(t *testing.T) {
	ctx := context.Background()
	z := azureRuler(t)
	s, _ := azureServer(t, z, nil)
	h := s.Handler()
	z.mu.Lock()
	g := z.groups["rg-shop/shop"]
	g["id"] = strings.Replace(g["id"].(string), "/resourceGroups/rg-shop/", "/resourceGroups/RG-Shop/", 1)
	z.mu.Unlock()
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	id := proposeRule(t, h, "Errors", "for", url.Values{"for": {"15m"}})
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied {
		t.Errorf("after the id's case changed: %+v", p)
	}
}
