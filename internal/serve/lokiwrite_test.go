package serve

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
)

// Changing a Loki ruler's rules through its configuration API
// (docs/design/rulers.md, "Loki's ruler"): Cortex's API under
// /loki/api/v1/rules, written as a Mimir's is, on a path of its own. A real
// Loki runs in test/e2e/loki.sh.

const lokiShop = `name: api
interval: 1m
rules:
  - alert: LogErrors
    expr: sum(count_over_time({app="shop"} |= "error" [5m])) > 9
    for: 5m
  - alert: LogSlow
    expr: max(max_over_time({app="shop"} | logfmt | unwrap latency_ms [1m])) > 500
    for: 2m
`

// lokiRulerStandIn is a Loki's ruler: Mimir's stand-in at Loki's path, the
// rules API Loki serves beside it, and a tenant every request must name.
type lokiRulerStandIn struct {
	*mimirStandIn
	tenant string
	mu     sync.Mutex
	asked  []string // each request, with the tenant it named
}

func newLokiRuler(t *testing.T) *lokiRulerStandIn {
	m := newMimir(t, "/loki/api/v1/rules")
	m.readToken, m.writeToken = "loki-reads-0123456789", "loki-writes-0123456789"
	if err := m.put("shop", []byte(lokiShop)); err != nil {
		t.Fatal(err)
	}
	return &lokiRulerStandIn{mimirStandIn: m, tenant: "acme"}
}

func (l *lokiRulerStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	l.asked = append(l.asked, r.Method+" "+r.URL.EscapedPath()+" "+r.Header.Get("X-Scope-OrgID"))
	l.mu.Unlock()
	if r.Header.Get("X-Scope-OrgID") != l.tenant {
		http.Error(w, "no org id", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/prometheus/api/v1/rules" {
		io.WriteString(w, `{"status":"success","data":{"groups":[]}}`)
		return
	}
	l.mimirStandIn.ServeHTTP(w, r)
}

func (l *lokiRulerStandIn) requests() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.asked)
}

// lokiServer reads a Mimir's rules with --ruler, shop's namespace changes
// allowed, and a Loki's with --loki-loaded-rules, with its tenant, a token
// to read and one to write; in are the --changes-in entries beside shop.
func lokiServer(t *testing.T, in []string, more func(*Config)) (*Server, *mimirStandIn, *lokiRulerStandIn) {
	t.Helper()
	lk := newLokiRuler(t)
	ts := httptest.NewServer(lk)
	t.Cleanup(ts.Close)
	m := mimirRuler(t, "/prometheus/config/v1/rules")
	s := mimirServer(t, m, func(c *Config) {
		c.Loki, c.LokiLoaded, c.LokiTenant = ts.URL, true, lk.tenant
		c.LokiToken, c.ChangesLokiToken = tokenFile(t, lk.readToken), tokenFile(t, lk.writeToken)
		c.ChangesIn = append(c.ChangesIn, in...)
		if more != nil {
			more(c)
		}
	})
	s.h = replay.WithLogs(s.h, func(rules.Rule) (replay.History, error) { return &lokiLogs{}, nil })
	if err := s.look(context.Background(), new(string)); err != nil {
		t.Fatal(err)
	}
	return s, m, lk
}

// A longer for for a Loki ruler's rule, in a namespace --changes-in names
// as loki:shop: proposed on Loki's own path with the group as it would be
// written, applied once approved, its group posted to Loki with the write
// token and the tenant, read back; undone, the rule put back. The Mimir,
// whose namespace shop --changes-in names too, isn't written to.
func TestLokiRulerChange(t *testing.T) {
	ctx := context.Background()
	s, m, lk := lokiServer(t, []string{"loki:shop"}, nil)
	h := s.Handler()
	id := proposeRule(t, h, "LogErrors", "for", url.Values{"for": {"15m"}})
	p := state(t, s, id)
	ev := proposalEvidence(p)
	if p.Path != ledger.PathLoki || ev.Edit == nil || ev.Edit.Namespace != "shop" || ev.Backtest == nil || ev.NoPath != "" ||
		!strings.HasSuffix(p.Source, "/loki/api/v1/rules/shop") {
		t.Fatalf("the proposal: path %q, source %s, why %q, %+v", p.Path, p.Source, ev.NoPath, ev.Edit)
	}
	if n := changedLines(p.Diff); n != 2 || !strings.Contains(p.Diff, "for: 15m") {
		t.Errorf("the diff changes more than the for:\n%s", p.Diff)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), ana, "", nil); !strings.Contains(body, "Loki&#39;s ruler&#39;s API") {
		t.Error("the page doesn't say it is applied through Loki's ruler's API")
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	p = state(t, s, id)
	if p.State != ledger.StateApplied || !slices.Equal(lk.written(), []string{"POST shop/api"}) || p.AfterHash == "" {
		t.Fatalf("applied: %s %s, written %v", p.State, p.Reason, lk.written())
	}
	if len(m.written()) != 0 {
		t.Errorf("the Mimir was written to: %v", m.written())
	}
	if g := lk.group("shop", "api"); !strings.Contains(g, "for: 15m") || !strings.Contains(g, "for: 2m") {
		t.Errorf("the group as written:\n%s", g)
	}
	for _, r := range lk.requests() {
		if !strings.HasSuffix(r, " acme") {
			t.Errorf("a request without the tenant: %s", r)
		}
	}
	key := keyFor(t, h, id, ana, "undo")
	code, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}, "read": {"1"}})
	if code != http.StatusSeeOther {
		t.Fatalf("undo: %d", code)
	}
	undo := locID(hd)
	if u := state(t, s, undo); u.Path != ledger.PathLoki {
		t.Errorf("the undo's path: %q", u.Path)
	}
	s.jobs(ctx, false)
	if u, p := state(t, s, undo), state(t, s, id); u.State != ledger.StateApplied || p.State != ledger.StateUndone {
		t.Fatalf("undone: %s %s, the change %s", u.State, u.Reason, p.State)
	}
	if g := lk.group("shop", "api"); !strings.Contains(g, "for: 5m") || strings.Contains(g, "for: 15m") {
		t.Errorf("undone, the group:\n%s", g)
	}
}

// Read again before it is written: a Loki rule changed in Loki since it was
// proposed is refused as stale; only another rule of its group changed, the
// change is made afresh, the other as Loki now holds it.
func TestLokiRulerChangedSince(t *testing.T) {
	ctx := context.Background()
	s, _, lk := lokiServer(t, []string{"loki:shop"}, nil)
	h := s.Handler()
	stale := proposeRule(t, h, "LogErrors", "for", url.Values{"for": {"15m"}})
	lk.mu.Lock()
	lk.put("shop", []byte(strings.Replace(lokiShop, "for: 5m", "for: 7m", 1)))
	lk.mu.Unlock()
	approve(t, h, stale, ana)
	s.jobs(ctx, false)
	if p := state(t, s, stale); p.State != ledger.StateRefused || !strings.Contains(p.Reason, "changed in Loki's ruler since it was proposed") || len(lk.written()) != 0 {
		t.Fatalf("the rule changed since: %s %q, written %v", p.State, p.Reason, lk.written())
	}
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	afresh := proposeRule(t, h, "LogErrors", "for", url.Values{"for": {"15m"}})
	lk.mu.Lock()
	lk.put("shop", []byte(strings.Replace(strings.Replace(lokiShop, "for: 5m", "for: 7m", 1), "> 500", "> 900", 1)))
	lk.mu.Unlock()
	approve(t, h, afresh, ana)
	s.jobs(ctx, false)
	p := state(t, s, afresh)
	if g := lk.group("shop", "api"); p.State != ledger.StateApplied || !strings.Contains(g, "for: 15m") || !strings.Contains(g, "> 900") {
		t.Fatalf("another rule changed since: %s %q\n%s", p.State, p.Reason, g)
	}
}

// A Loki ruler's rule is for a person while --changes-in names no Loki
// namespace of its: shop, which names the Mimir's, doesn't name Loki's; a
// Loki prefix names it. Without --loki-loaded-rules, loki:shop is a
// Mimir's namespace, as before; and a change approved on Loki's path that
// serve no longer reads Loki for is refused, nothing written.
func TestLokiRulerChangesIn(t *testing.T) {
	ctx := context.Background()
	s, m, lk := lokiServer(t, nil, nil)
	h := s.Handler()
	p := state(t, s, proposeRule(t, h, "LogErrors", "for", url.Values{"for": {"15m"}}))
	if p.Path != ledger.PathNone || proposalEvidence(p).NoPath != noPathLokiIn {
		t.Errorf("--changes-in shop,solo: path %q, why %q", p.Path, proposalEvidence(p).NoPath)
	}
	if !s.changesIn("shop", "api") || s.changesInLoki("shop") {
		t.Error("shop names Loki's namespace, or no longer the Mimir's")
	}
	s2, _, _ := lokiServer(t, []string{"loki:sh*"}, nil)
	if p := state(t, s2, proposeRule(t, s2.Handler(), "LogErrors", "for", url.Values{"for": {"15m"}})); p.Path != ledger.PathLoki {
		t.Errorf("loki:sh*: path %q, why %q", p.Path, proposalEvidence(p).NoPath)
	}
	if s2.changesIn("loki:shop", "api") {
		t.Error("loki:sh* names a Mimir namespace")
	}
	s.loki = nil
	if s.lokiEntry("loki:shop") || !lokiInOf([]string{"loki:shop"}, true, "shop") || lokiInOf([]string{"loki:shop"}, false, "shop") {
		t.Error("without --loki-loaded-rules, loki:shop is Loki's")
	}
	// Approved on Loki's path, then serve restarted without
	// --loki-loaded-rules: refused, nothing sent anywhere.
	s3, m3, lk3 := lokiServer(t, []string{"loki:shop"}, nil)
	id := proposeRule(t, s3.Handler(), "LogErrors", "for", url.Values{"for": {"15m"}})
	approve(t, s3.Handler(), id, ana)
	s3.loki = nil
	s3.jobs(ctx, false)
	if p := state(t, s3, id); p.State != ledger.StateRefused || len(lk3.written())+len(m3.written()) != 0 {
		t.Errorf("without Loki's ruler: %s %q", p.State, p.Reason)
	}
	if len(m.written())+len(lk.written()) != 0 {
		t.Error("written to")
	}
}

// What Loki's ruler is written with: a write token of its own, not the
// one that reads it, nor --ruler's; and nothing at all without --changes.
func TestLokiRulerWrites(t *testing.T) {
	s, _, lk := lokiServer(t, []string{"loki:shop"}, nil)
	if s.loki == nil || s.lokiWrites.TokenFile != s.cfg.ChangesLokiToken || s.lokiWrites.TokenFile == s.rulerWrites.TokenFile ||
		s.loki.Reads.TokenFile != s.cfg.LokiToken || s.loki.Tenant != lk.tenant {
		t.Errorf("Loki's ruler: %+v, writes %+v", s.loki, s.lokiWrites)
	}
	off, _ := server(t, func(c *Config) { c.Loki, c.LokiLoaded, c.ChangesLokiToken = "http://loki:3100", true, "/nowhere" })
	if off.loki == nil || off.lokiWrites.TokenFile != "" {
		t.Errorf("without --changes: %+v", off.lokiWrites)
	}
	files, _ := server(t, func(c *Config) { c.Loki, c.LokiRules = "http://loki:3100", t.TempDir() })
	if files.loki != nil {
		t.Error("--loki-rules alone reads Loki's ruler")
	}
	// An --alarms source named loki would have its alarms named by
	// loki:NAMESPACE too: refused.
	named := &Server{cfg: Config{Loki: "http://loki:3100", LokiLoaded: true, Changes: true, Alarms: []AlarmsAt{{Source: "loki"}}}}
	if err := named.newLoki(); err == nil || named.loki != nil {
		t.Errorf("--alarms loki= beside --loki-loaded-rules: %v", err)
	}
}

// A team's changes_in names Loki's namespaces as --changes-in does, within
// those --changes-in names: loki:shop within loki:sh*, not within shop.
func TestLokiTeamChangesIn(t *testing.T) {
	s, _, _ := lokiServer(t, []string{"loki:sh*"}, nil)
	for x, want := range map[string]bool{"loki:shop": true, "loki:sh*": true, "loki:solo": false, "shop": true, "loki:shop2*": true} {
		if got := s.withinChangesIn(x); got != want {
			t.Errorf("%s within --changes-in shop,solo,loki:sh*: %v, want %v", x, got, want)
		}
	}
	s2, _, _ := lokiServer(t, nil, nil)
	if s2.withinChangesIn("loki:shop") {
		t.Error("loki:shop within --changes-in shop,solo")
	}
}
