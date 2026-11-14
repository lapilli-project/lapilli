package serve

import (
	"context"
	"encoding/json"
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
	"lapilli.dev/internal/rules"
)

// What an attacker, or bad luck, tried against changing rules, each fixed
// (CONTRIBUTING.md): the review before v0.22.0.

// grafanaFlaky is Grafana's stand-in, failing as Grafana and what is in
// front of it can.
type grafanaFlaky struct {
	g *grafanaStandIn
	// unreadable fails every read of a rule once one was written.
	unreadable atomic.Bool
	// deleteAnswer answers a DELETE, done, with this status.
	deleteAnswer int
	// pauseOn is made, once armed, when the rule abc is read: an admin
	// pausing changes between the read and the write.
	pauseOn string
	armed   atomic.Bool
}

func (f *grafanaFlaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/provisioning/alert-rules")
	if f.pauseOn != "" && r.Method == http.MethodGet && rest == "/abc" && f.armed.CompareAndSwap(true, false) {
		os.WriteFile(f.pauseOn, []byte("1"), 0o644)
	}
	if f.deleteAnswer != 0 && r.Method == http.MethodDelete {
		f.g.ServeHTTP(httptest.NewRecorder(), r)
		http.Error(w, "upstream timeout", f.deleteAnswer)
		return
	}
	if f.unreadable.Load() && r.Method == http.MethodGet && rest != "/export" && len(f.g.writes) > 0 {
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	f.g.ServeHTTP(w, r)
}

func flakyServer(t *testing.T, f *grafanaFlaky, more func(*Config)) *Server {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	s, _ := changesServer(t, func(c *Config) {
		c.RulesPath, c.Grafana = "", ts.URL
		c.ChangesGrafanaToken = tokenFile(t, "grafana-writes-0123456789")
		if more != nil {
			more(c)
		}
	})
	return s
}

func locID(hd http.Header) int64 {
	var id int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &id)
	return id
}

// Grafana takes a change, then can't be read: the change is kept as
// applying, with the rule as it was, and counted against the limits; it is
// read back later, and never sent again.
func TestGrafanaReadBackFails(t *testing.T) {
	ctx := context.Background()
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	f := &grafanaFlaky{g: g}
	s := flakyServer(t, f, func(c *Config) { c.ChangesLimits = Limits{Deletions: 5, Changes: 1} })
	h := s.Handler()
	a := proposeFor(t, h, "15m")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Other"}, "kind": {"for"}, "for": {"20m"}})
	b := locID(hd)
	approve(t, h, a, ana)
	approve(t, h, b, ana)
	f.unreadable.Store(true)
	s.jobs(ctx, false)
	pa, pb := state(t, s, a), state(t, s, b)
	if pa.State != ledger.StateApplying || pa.Backup == "" || pa.Applied.IsZero() || !strings.Contains(pa.Reason, "not read back yet") {
		t.Fatalf("written, not read back: %+v", pa)
	}
	if pb.State != ledger.StateApproved || len(g.writes) != 1 || g.rules["abc"]["for"] != "15m" {
		t.Fatalf("one change an hour: %s, written %v", pb.State, g.writes)
	}
	// Read through the export meanwhile, the rule changed: by this change,
	// so the proposal isn't marked stale.
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(a), cy, "", nil); strings.Contains(body, "isn&#39;t the one this was proposed against") {
		t.Error("a change being applied is marked stale by itself")
	}
	// It blocks another for the rule, as any change under way does.
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Errors"}, "kind": {"for"}, "for": {"30m"}}); code < 400 {
		t.Errorf("another proposal for a rule being changed: %d", code)
	}
	// Read later: applied, and its undo has what it needs.
	f.unreadable.Store(false)
	s.jobs(ctx, false)
	if pa = state(t, s, a); pa.State != ledger.StateApplied || pa.AfterHash == "" || len(g.writes) != 1 || strings.Contains(pa.Reason, "not read back yet") {
		t.Fatalf("read back: %+v, written %v", pa, g.writes)
	}
	if keyFor(t, h, a, ana, "undo") == "" {
		t.Error("no undo for a change read back late")
	}
}

// Grafana deletes a rule and answers 504: the deletion is applied, the
// rule as it was kept, and it can be undone.
func TestGrafanaDeleteAnswersBadly(t *testing.T) {
	ctx := context.Background()
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	f := &grafanaFlaky{g: g}
	s := flakyServer(t, f, nil)
	h := s.Handler()
	_, _, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Other"}, "kind": {"disable"}})
	off := locID(hd)
	approve(t, h, off, ana)
	s.jobs(ctx, false)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(5 * weekLen) }
	id, err := s.propose(ctx, ask{Rule: "Other", Kind: ledger.KindDelete, Follows: off, From: "off four weeks"}, ledger.Proposer, false, true)
	if err != nil {
		t.Fatal(err)
	}
	approve(t, h, id, cy)
	f.deleteAnswer = http.StatusGatewayTimeout
	s.jobs(ctx, false)
	p := state(t, s, id)
	if p.State != ledger.StateApplied || p.Backup == "" || g.rules["def"] != nil || !strings.Contains(p.Reason, "504") {
		t.Fatalf("deleted, answered 504: %+v, Grafana has %v", p, g.rules["def"])
	}
	key := keyFor(t, h, id, ana, "undo")
	_, _, hd = ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}, "read": {"1"}})
	s.jobs(ctx, false)
	if u := state(t, s, locID(hd)); u.State != ledger.StateApplied || g.rules["def"] == nil {
		t.Errorf("undone: %+v", u)
	}
}

// Paused while a change is checked again: nothing is sent to Grafana, or
// handed to a recipe; it waits, approved.
func TestPausedWhileChecked(t *testing.T) {
	ctx := context.Background()
	t.Run("grafana", func(t *testing.T) {
		stop := filepath.Join(t.TempDir(), "paused")
		g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
		f := &grafanaFlaky{g: g, pauseOn: stop}
		s := flakyServer(t, f, func(c *Config) { c.ChangesPaused = stop })
		h := s.Handler()
		id := proposeFor(t, h, "15m")
		approve(t, h, id, ana)
		f.armed.Store(true)
		if err := s.carryOut(ctx); err != nil {
			t.Fatal(err)
		}
		if p := state(t, s, id); p.State != ledger.StateApproved || len(g.writes) != 0 {
			t.Fatalf("paused before the write: %s, written %v", p.State, g.writes)
		}
		os.Remove(stop)
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateApplied || len(g.writes) != 1 {
			t.Errorf("resumed: %s, written %v", p.State, g.writes)
		}
	})
	t.Run("git", func(t *testing.T) {
		stop := filepath.Join(t.TempDir(), "paused")
		s, _ := changesServer(t, func(c *Config) { c.ChangesPaused = stop })
		h, port := s.Handler(), s.IncidentsHandler()
		id := proposeFor(t, h, "15m")
		approve(t, h, id, ana)
		ph := &pausingHistory{stop: stop}
		ph.armed.Store(true)
		s.h = ph
		if err := s.carryOut(ctx); err != nil {
			t.Fatal(err)
		}
		os.Remove(stop)
		if p := state(t, s, id); p.State != ledger.StateApproved || ready(t, port) != 0 {
			t.Fatalf("paused during the backtest: %s", p.State)
		}
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateReady {
			t.Errorf("resumed: %s", p.State)
		}
	})
}

// pausingHistory is history, making the pause file when it is first asked
// once armed: an admin pausing changes during a backtest.
type pausingHistory struct {
	history
	stop  string
	armed atomic.Bool
}

func (h *pausingHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if h.armed.CompareAndSwap(true, false) {
		os.WriteFile(h.stop, []byte("1"), 0o644)
	}
	return h.history.QueryRange(ctx, expr, start, end, step)
}

// The limits count a change once, when it is handed out or sent, never
// again when it takes effect; and a change a person made never.
func TestLimitsCountOnce(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, nil)
	h, port := s.Handler(), s.IncidentsHandler()
	id := proposeFor(t, h, "15m")
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	handedOut := state(t, s, id).Applied
	if handedOut.IsZero() {
		t.Fatal("not counted when handed out")
	}
	time.Sleep(1100 * time.Millisecond) // the ledger keeps seconds
	mark := time.Now()
	recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "opened", "link": "https://git.example/pull/1"})
	recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "merged"})
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(state(t, s, id).Document), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	p := state(t, s, id)
	if n, _ := s.l.Carried(ctx, mark, ""); p.State != ledger.StateApplied || n != 0 || !p.Applied.Equal(handedOut) {
		t.Errorf("counted again when it took effect: %s, %d since, applied %v (handed out %v)", p.State, n, p.Applied, handedOut)
	}
	// A deletion for a person to make, made: applied, never counted.
	code, _, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Errors"}, "kind": {"delete"}})
	if code != http.StatusSeeOther {
		t.Fatal(code)
	}
	d := locID(hd)
	if p := state(t, s, d); p.Path != ledger.PathNone {
		t.Fatalf("path %s", p.Path)
	}
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte("groups:\n  - name: shop\n    rules:\n      - alert: Kept\n        expr: errors > 7\n"), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if p := state(t, s, d); p.State != ledger.StateApplied || !p.Applied.IsZero() {
		t.Errorf("a person's change: %s, counted at %v", p.State, p.Applied)
	}
}

// One proposal for a rule at a time, of any kind: a fix to what it says
// beside a new for would leave neither as it expects the rule once both
// were merged, and neither would be seen applied.
func TestOneProposalPerRule(t *testing.T) {
	s, _ := changesServer(t, nil)
	h := s.Handler()
	id := proposeFor(t, h, "15m")
	for name, form := range map[string]url.Values{
		"says":      {"rule": {"Errors"}, "kind": {"says"}, "annotation": {"summary"}, "text": {"Checkout errors"}},
		"threshold": {"rule": {"Errors"}, "kind": {"threshold"}, "bound": {"0"}, "value": {"7"}},
		"delete":    {"rule": {"Errors"}, "kind": {"delete"}},
	} {
		if code, body, _ := ui(t, h, http.MethodPost, "/proposals", cy, "same-origin", form); code < 400 || !strings.Contains(body, "one at a time") {
			t.Errorf("a %s beside a for: %d", name, code)
		}
	}
	s.l.Move(context.Background(), id, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{})
	if _, code, body := proposeSays(t, h, url.Values{"annotation": {"summary"}, "text": {"Checkout errors"}}); code != http.StatusSeeOther {
		t.Errorf("once the for was dropped: %d %s", code, body)
	}
}

// A person proposes twenty an hour at most, with their agents: one agent
// can't make a hundred proposals, and a hundred notices, at once.
func TestProposalRate(t *testing.T) {
	s, dir := changesServer(t, func(c *Config) { c.MCPPropose = "editors" })
	var b strings.Builder
	b.WriteString("groups:\n  - name: shop\n    rules:\n")
	for i := range 30 {
		b.WriteString("      - alert: R" + itoa(int64(i)) + "\n        expr: errors > 5\n        labels: {severity: page}\n")
	}
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(b.String()), 0o644)
	if err := s.look(context.Background(), new(string)); err != nil {
		t.Fatal(err)
	}
	s.l.TellTo([]string{"webhook"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	cs := connect(t, ts.URL, bo)
	made := 0
	var last string
	for i := range 25 {
		var out proposeOut
		failed, text := call(t, cs, "propose", map[string]any{"rule": "R" + itoa(int64(i)), "kind": "says", "annotation": "summary", "text": "x"}, &out)
		if !failed {
			made++
		}
		last = text
	}
	if made != ledger.ProposalsAnHour || !strings.Contains(last, "in the last hour") {
		t.Errorf("an agent made %d proposals in a row (%s)", made, last)
	}
	// The person, in the UI, is past it too; someone else isn't.
	if code, _, _ := ui(t, s.Handler(), http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"R29"}, "kind": {"for"}, "for": {"15m"}}); code < 400 {
		t.Errorf("the person whose agent made them: %d", code)
	}
	if code, body, _ := ui(t, s.Handler(), http.MethodPost, "/proposals", cy, "same-origin", url.Values{"rule": {"R29"}, "kind": {"for"}, "for": {"15m"}}); code != http.StatusSeeOther {
		t.Errorf("someone else: %d %s", code, body)
	}
	ns, _ := s.l.Due(context.Background(), "webhook", time.Now().Add(time.Hour), 1000)
	if len(ns) > ledger.ProposalsAnHour+1 {
		t.Errorf("%d notices", len(ns))
	}
}

// One person approves once, in any letter case, Unicode's too.
func TestApprovedOnceInAnyCase(t *testing.T) {
	s, _ := changesServer(t, func(c *Config) {
		c.Approvals = 2
		c.GroupsHeader = "X-Forwarded-Groups"
		c.Approvers = []string{"group:sre"}
	})
	h := s.Handler()
	id := proposeFor(t, h, "15m")
	as := func(who string) int {
		page := httptest.NewRequest(http.MethodGet, "/proposals/"+itoa(id), nil)
		page.Header.Set("X-Forwarded-Email", who)
		page.Header.Set("X-Forwarded-Groups", "sre")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, page)
		key := ""
		for _, m := range formKey.FindAllStringSubmatch(w.Body.String(), -1) {
			if m[2] == "approve" {
				key = m[3]
			}
		}
		if key == "" {
			return 0 // no form offered
		}
		r := httptest.NewRequest(http.MethodPost, "/proposals/"+itoa(id)+"/approve", strings.NewReader(url.Values{"key": {key}, "read": {"1"}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("X-Forwarded-Email", who)
		r.Header.Set("X-Forwarded-Groups", "sre")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := as("ÉMILE@example.com"); code != http.StatusSeeOther {
		t.Fatalf("first: %d", code)
	}
	if code := as("émile@example.com"); code == http.StatusSeeOther {
		t.Errorf("the same person again, in lower case: %d", code)
	}
	if p := state(t, s, id); p.State != ledger.StateProposed || len(p.Approvals) != 1 {
		t.Errorf("one person's approval: %+v", p)
	}
}

// --approvals lowered from 2 to 1: a proposal one person approved moves on.
func TestApprovalsLowered(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, func(c *Config) { c.Approvals = 2 })
	h := s.Handler()
	id := proposeFor(t, h, "15m")
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateProposed {
		t.Fatalf("one of two: %s", p.State)
	}
	s.cfg.Approvals = 1
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateReady {
		t.Errorf("one of one: %s", p.State)
	}
}

// A change ready for a recipe that no recipe takes can be dropped, by an
// approver or an admin, not an editor: it would hold up its rule.
func TestDropReady(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, func(c *Config) { c.Editors = []string{bo} })
	h, port := s.Handler(), s.IncidentsHandler()
	id := proposeFor(t, h, "15m")
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), bo, "", nil); strings.Contains(body, "/drop") {
		t.Error("an editor is offered to drop a change handed out")
	}
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/drop", bo, "same-origin", url.Values{}); code < 400 {
		t.Errorf("an editor dropping it: %d", code)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), cy, "", nil); !strings.Contains(body, "/drop") {
		t.Error("an approver isn't offered to drop it")
	}
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/drop", cy, "same-origin", url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("an approver, not an editor, dropping it: %d", code)
	}
	if p := state(t, s, id); p.State != ledger.StateDropped || ready(t, port) != 0 {
		t.Errorf("dropped: %s", p.State)
	}
	// Its rule takes another.
	proposeFor(t, h, "20m")
	// No one else drops anything.
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id+1)+"/drop", "viewer@example.com", "same-origin", url.Values{}); code != http.StatusForbidden {
		t.Errorf("a viewer dropping: %d", code)
	}
}

// The incident a proposal is made for is one the ledger has, by its ID,
// and goes into a pull request as code: an editor's text never does.
func TestIncidentNamed(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	h, port := s.Handler(), s.IncidentsHandler()
	threshold := func(incident string) (int, string, http.Header) {
		return ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Errors"}, "kind": {"threshold"}, "bound": {"0"},
			"value": {"3"}, "from": {"near miss"}, "incident": {incident}})
	}
	for _, bad := range []string{"INC-1\n\n@acme/everyone [approve now](https://evil.example/login) ![](https://evil.example/p.png)",
		"INC-9", "`INC-1`", strings.Repeat("A", 900<<10)} {
		if code, _, _ := threshold(bad); code != http.StatusBadRequest {
			t.Errorf("%.40q: %d", bad, code)
		}
	}
	code, body, hd := threshold("INC-1")
	if code != http.StatusSeeOther {
		t.Fatalf("INC-1: %d %s", code, body)
	}
	id := locID(hd)
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	for _, c := range handed(t, port) {
		if c.ID == id && !strings.Contains(c.Body, "Made for the incident `INC-1`, which nothing caught.") {
			t.Errorf("the pull request's body:\n%s", c.Body)
		}
	}
}

// A proposal's task linked to its tracker's issue keeps that link when the
// pull request opens: the tracker job keeps the task in step by it.
func TestTaskLinkKept(t *testing.T) {
	ctx := context.Background()
	const tasksTok = "tasks-token-0123456789abc"
	s, _ := changesServer(t, func(c *Config) { c.TasksToken = tokenFile(t, tasksTok) })
	h, port := s.Handler(), s.IncidentsHandler()
	id := proposeFor(t, h, "15m")
	ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/task", bo, "same-origin", url.Values{})
	p := state(t, s, id)
	if code, body := recipe(t, port, http.MethodPatch, "/api/v1/tasks/"+itoa(p.TaskID), tasksTok, map[string]string{"link": "https://github.com/example/alerts/issues/99"}); code != 200 {
		t.Fatalf("the tracker's link: %d %s", code, body)
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "opened", "link": "https://github.com/example/rules/pull/12"})
	task, _ := s.l.TaskByID(ctx, p.TaskID)
	if task.Link != "https://github.com/example/alerts/issues/99" || state(t, s, id).Link != "https://github.com/example/rules/pull/12" {
		t.Errorf("the task's link %q, the proposal's %q", task.Link, state(t, s, id).Link)
	}
}

// What a rule says is written only as a template that parses in its
// language, calling no query: Prometheus refuses a whole file for one that
// doesn't parse, and query runs PromQL each time the alert fires.
func TestSaysTemplates(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	h := s.Handler()
	for text, want := range map[string]string{
		`Errors on {{ $labels.instance`: "template that parses",
		`{{ range query "count by (__name__)({__name__=~\".+\"})" }}{{ .Labels.__name__ }} {{ end }}`: "calls query",
		`{{ "up" | query | first | value }}`:                                 "calls query",
		`{{ define "x" }}{{ with query "up" }}{{ . }}{{ end }}{{ end }}done`: "calls query",
		`{{ $values.A }} errors`:                                             "template that parses", // Grafana's, not Prometheus's
	} {
		if _, code, body := proposeSays(t, h, url.Values{"annotation": {"summary"}, "text": {text}}); code != http.StatusBadRequest || !strings.Contains(body, want) {
			t.Errorf("%q: %d, want refused with %q", text, code, want)
		}
	}
	if _, code, _ := proposeSays(t, h, url.Values{"annotation": {"runbook_url"}, "text": {"https://wiki.example/{{$labels.job"}}); code != http.StatusBadRequest {
		t.Errorf("a runbook that doesn't parse: %d", code)
	}
	id, code, body := proposeSays(t, h, url.Values{"annotation": {"summary"}, "text": {`Errors on {{ $labels.instance }}: {{ $value | humanize }}, see {{ .Labels.query }}`}})
	if code != http.StatusSeeOther {
		t.Fatalf("a template that parses: %d %s", code, body)
	}
	s.l.Move(ctx, id, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{})
	// Kept in the ledger anyway, it is read back when checked again, and
	// refused.
	f, err := s.current(ctx, "Errors", true)
	if err != nil {
		t.Fatal(err)
	}
	ev, _ := json.Marshal(evidence{From: "person", Edit: &rules.Edit{Name: "Errors", Group: "shop", Annotation: "summary", Value: "Errors on {{ $labels.instance"}})
	forged, err := s.l.Propose(ctx, ledger.Proposal{Rule: "Errors", Kind: ledger.KindSays, Path: ledger.PathGit, MadeBy: bo, Source: f.doc.Source,
		DocHash: "another", Before: "x", BeforeHash: saysHash(f.rule), After: "y", Diff: "d", Evidence: ev})
	if err != nil {
		t.Fatal(err)
	}
	s.l.Approve(ctx, forged, ana, 1)
	s.jobs(ctx, false)
	if p := state(t, s, forged); p.State != ledger.StateRefused {
		t.Errorf("a template that doesn't parse, kept in the ledger: %s", p.State)
	}
	// So is one for Grafana.
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	gs := grafanaServer(t, g)
	gf, err := gs.current(ctx, "Errors", true)
	if err != nil {
		t.Fatal(err)
	}
	ev, _ = json.Marshal(evidence{From: "person", Edit: &rules.Edit{Name: "Errors", UID: "abc", Annotation: "summary", Value: `{{ query "up" }}`}})
	forged, _ = gs.l.Propose(ctx, ledger.Proposal{Rule: "Errors", Kind: ledger.KindSays, Path: ledger.PathGrafana, MadeBy: bo,
		Before: "x", BeforeHash: saysHash(gf.rule), After: "y", Diff: "d", Evidence: ev})
	gs.l.Approve(ctx, forged, ana, 1)
	gs.jobs(ctx, false)
	if p := state(t, gs, forged); p.State != ledger.StateRefused || len(g.writes) != 0 {
		t.Errorf("a query, kept in the ledger, for Grafana: %s, written %v", p.State, g.writes)
	}
}

// grafanaFile is a Grafana alerting provisioning file, as kept in git.
const grafanaFile = `apiVersion: 1
groups:
  - orgId: 1
    name: api
    folder: Platform
    interval: 1m
    rules:
      - uid: ghi
        title: Panel errors
        condition: A
        for: 2m
        labels:
          severity: page
        data:
          - refId: A
            datasourceUid: prom
            model:
              expr: errors > 5
`

// What a rule in a Grafana provisioning file says is changed by a pull
// request: its digest, which covers what it says, isn't what it is checked
// by.
func TestSaysInGrafanaFile(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, nil)
	file := filepath.Join(dir, "grafana.yaml")
	os.WriteFile(file, []byte(grafanaFile), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	code, body, hd := ui(t, s.Handler(), http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Panel errors"}, "kind": {"says"},
		"annotation": {"summary"}, "text": {"Errors on {{ $labels.instance }}"}})
	if code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	id := locID(hd)
	p := state(t, s, id)
	if p.Path != ledger.PathGit || !strings.HasPrefix(p.Document, grafanaFile) || !strings.Contains(p.Document, `summary: "Errors on {{ $labels.instance }}"`) {
		t.Fatalf("the proposal: path %s\n%s", p.Path, p.Document)
	}
	approve(t, s.Handler(), id, ana)
	s.jobs(ctx, false)
	if p = state(t, s, id); p.State != ledger.StateReady {
		t.Fatalf("ready: %+v", p)
	}
	os.WriteFile(file, []byte(p.Document), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if p = state(t, s, id); p.State != ledger.StateApplied {
		t.Errorf("read back: %s", p.State)
	}
}

// A deletion merged after Lapilli read the merged file, while its pull
// request was open, is still seen applied.
func TestDeletionMergedLate(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, nil)
	h, port := s.Handler(), s.IncidentsHandler()
	extra := filepath.Join(dir, "extra.yaml")
	os.WriteFile(extra, []byte("groups:\n  - name: extra\n    rules:\n      - alert: Spare\n        expr: errors > 9\n      - alert: Kept\n        expr: errors > 7\n"), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	_, _, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Spare"}, "kind": {"delete"}})
	id := locID(hd)
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	p := state(t, s, id)
	if p.State != ledger.StateReady {
		t.Fatalf("ready: %+v", p)
	}
	recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "opened", "link": "https://git.example/pull/3"})
	os.WriteFile(extra, []byte(p.Document), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if p := state(t, s, id); p.State != ledger.StateOpened {
		t.Fatalf("read while open: %s", p.State)
	}
	code, body := recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "merged"})
	var told ledger.Proposal
	if json.Unmarshal([]byte(body), &told); code != 200 || told.State != ledger.StateApplied {
		t.Errorf("merged: %d %s", code, told.State)
	}
	if p := state(t, s, id); p.State != ledger.StateApplied {
		t.Errorf("merged after it was read: %s", p.State)
	}
}

// A Grafana rule is deleted only after it was turned off and stayed off:
// never outright, and never once turned on again by hand.
func TestGrafanaRetirement(t *testing.T) {
	ctx := context.Background()
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	s := grafanaServer(t, g)
	h := s.Handler()
	if code, body, _ := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Other"}, "kind": {"delete"}}); code != http.StatusBadRequest || !strings.Contains(body, "turned off first") {
		t.Errorf("deleted outright: %d", code)
	}
	// Kept in the ledger anyway, it is refused when checked again.
	of, err := s.current(ctx, "Other", true)
	if err != nil {
		t.Fatal(err)
	}
	ev, _ := json.Marshal(evidence{From: "person", Edit: &rules.Edit{Name: "Other", UID: "def", Delete: true}, Record: &ruleRecord{}})
	forged, err := s.l.Propose(ctx, ledger.Proposal{Rule: "Other", Kind: ledger.KindDelete, Path: ledger.PathGrafana, MadeBy: bo,
		Before: "x", BeforeHash: ruleHash(of.rule), Diff: "d", Evidence: ev})
	if err != nil {
		t.Fatal(err)
	}
	s.l.Approve(ctx, forged, ana, 1)
	s.jobs(ctx, false)
	if p := state(t, s, forged); p.State != ledger.StateRefused || g.rules["def"] == nil {
		t.Fatalf("a deletion with no disable before it: %s", p.State)
	}
	_, _, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Other"}, "kind": {"disable"}})
	off := locID(hd)
	approve(t, h, off, ana)
	s.jobs(ctx, false)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	// Off, its page offers to delete it, following the disable.
	if _, body, _ := ui(t, h, http.MethodGet, "/rules/Other?tab=proposals", bo, "", nil); !strings.Contains(body, `name="kind" value="delete"`) {
		t.Error("the page of a rule off doesn't offer to delete it")
	}
	if _, err := s.propose(ctx, ask{Rule: "Other", Kind: ledger.KindDelete, From: "person"}, bo, false, true); err == nil ||
		!strings.Contains(err.Error(), "four weeks aren't up") {
		t.Errorf("proposed the day it was turned off: %v", err)
	}
	s.now = func() time.Time { return time.Now().Add(5 * weekLen) }
	id, err := s.propose(ctx, ask{Rule: "Other", Kind: ledger.KindDelete, From: "person"}, bo, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if p := state(t, s, id); p.Follows != off {
		t.Errorf("follows #%d, want #%d", p.Follows, off)
	}
	approve(t, h, id, ana)
	// Turned on again by hand, before it is checked again: refused.
	g.rules["def"]["isPaused"] = false
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(5 * weekLen) }
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateRefused || g.rules["def"] == nil {
		t.Fatalf("deleted once on again: %+v", p)
	}
	// Nor proposed again, four weeks on.
	if err := s.proposeFound(ctx); err != nil {
		t.Fatal(err)
	}
	if ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: "Other", States: ledger.Unfinished()}); len(ps) != 0 {
		t.Errorf("proposed for a rule on again: %+v", ps)
	}
	if _, err := s.propose(ctx, ask{Rule: "Other", Kind: ledger.KindDelete, Follows: off}, ledger.Proposer, false, true); err == nil || !strings.Contains(err.Error(), "turned on again") {
		t.Errorf("proposed by hand: %v", err)
	}
}

// A Grafana rule turned off by hand, not by a proposal, isn't deleted from
// here, and the refusal says where it is.
func TestGrafanaOffByHand(t *testing.T) {
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	g.rules["def"]["isPaused"] = true
	s := grafanaServer(t, g)
	if code, body, _ := ui(t, s.Handler(), http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Other"}, "kind": {"delete"}}); code != http.StatusBadRequest || !strings.Contains(body, "delete it in Grafana") {
		t.Errorf("deleting a rule off by hand: %d", code)
	}
}
