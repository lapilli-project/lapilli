package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// The second review of changing rules, before v0.22.0: each test fails on
// what it found, and passes on its fix.

// What a rule says calls only the functions its template language has:
// Prometheus refuses a whole file for one it hasn't, a misspelt humanize as
// much as one that doesn't exist; Grafana's has its own beside them.
func TestSaysOnlyItsFunctions(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	h := s.Handler()
	for text, ok := range map[string]bool{
		`{{ nosuchfunc $labels.instance }}`:                                                                 false,
		`{{ humanise $value }}`:                                                                             false,
		`Errors: {{ $value | humanise }}`:                                                                   false,
		`{{ removeLabels $labels "pod" }}`:                                                                  false, // Grafana's
		`{{ humanizePercentage $value }} on {{ $labels.instance | title | toUpper }}`:                       true,
		`{{ reReplaceAll "(.*):.*" "$1" $labels.instance }} {{ $externalURL }}`:                             true,
		`{{ $labels.instance | stripPort }} {{ index $labels "pod" }} {{ printf "%.1f" $value }}`:           true,
		`{{ humanizeDuration 3600 }} {{ toTime 1700000000 }} {{ $value | humanize1024 }} {{ .Labels.job }}`: true,
	} {
		id, code, body := proposeSays(t, h, url.Values{"annotation": {"summary"}, "text": {text}})
		if ok != (code == http.StatusSeeOther) {
			t.Errorf("%q: %d, want taken %v\n%s", text, code, ok, zzMsg(body))
		}
		if !ok && !strings.Contains(body, "not defined") {
			t.Errorf("%q: refused without naming the function: %s", text, zzMsg(body))
		}
		if id != 0 {
			s.l.Move(ctx, id, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{})
		}
	}
	// Grafana's own, in Grafana's language.
	g := grafanaServer(t, &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}})
	gh := g.Handler()
	if _, code, body := proposeSays(t, gh, url.Values{"annotation": {"summary"}, "text": {`{{ humanise $values.A.Value }}`}}); code != http.StatusBadRequest {
		t.Errorf("Grafana, misspelt: %d %s", code, zzMsg(body))
	}
	if _, code, body := proposeSays(t, gh, url.Values{"annotation": {"summary"}, "text": {`{{ $values.A.Value | humanize }} on {{ removeLabels $labels "pod" }}`}}); code != http.StatusSeeOther {
		t.Errorf("Grafana's own function: %d %s", code, zzMsg(body))
	}
}

// zzMsg is the message of an error page, or the start of a body.
func zzMsg(body string) string {
	if i := strings.Index(body, "</h1>"); i >= 0 {
		rest := body[i+5:]
		if j, k := strings.Index(rest, "<p>"), strings.Index(rest, "</p>"); j >= 0 && k > j {
			return rest[j+3 : k]
		}
	}
	return clip(body, 200)
}

// openThenMerge takes a ready proposal through its pull request, opened
// and merged, as a recipe tells it.
func openThenMerge(t *testing.T, port http.Handler, id int64, merge bool) {
	t.Helper()
	if code, body := recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "opened", "link": "https://git.example/pull/3"}); code != 200 {
		t.Fatalf("opened: %d %s", code, body)
	}
	if merge {
		if code, body := recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "merged"}); code != 200 {
			t.Fatalf("merged: %d %s", code, body)
		}
	}
}

// A pull request changed in review and merged so (15m made 20m) is settled
// as amended once the rules read say neither what was nor what was
// proposed: it no longer waits for ever, holding up its rule. Told before
// Lapilli read the file, or after, and for what a rule says too.
func TestMergedChangedInReview(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, nil)
	s.l.TellTo([]string{"webhook"})
	h, port := s.Handler(), s.IncidentsHandler()
	file := filepath.Join(dir, "shop.yaml")
	id := proposeFor(t, h, "15m")
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	p := state(t, s, id)
	openThenMerge(t, port, id, true)
	// Not read yet: it waits, merged.
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateMerged {
		t.Fatalf("merged, not read yet: %s", p.State)
	}
	os.WriteFile(file, []byte(strings.Replace(p.Document, "for: 15m", "for: 20m", 1)), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if p := state(t, s, id); p.State != ledger.StateAmended || !strings.Contains(p.Reason, "changed in review") {
		t.Fatalf("changed in review: %s %q", p.State, p.Reason)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), ana, "", nil); strings.Contains(body, "/undo") {
		t.Error("an amended change is offered to undo")
	}
	ns, _ := s.l.Due(ctx, "webhook", time.Now().Add(time.Hour), 100)
	if !slices.ContainsFunc(ns, func(n ledger.Notice) bool { return n.Event == "change.amended" }) {
		t.Error("not told change.amended")
	}
	// The rule takes another.
	if _, code, msg := proposeThreshold(t, h); code != http.StatusSeeOther {
		t.Fatalf("a new proposal for the rule: %d %s", code, msg)
	}

	// What it says, merged with other words, told after the file was read.
	s2, dir2 := changesServer(t, nil)
	h2, port2 := s2.Handler(), s2.IncidentsHandler()
	sid, code, body := proposeSays(t, h2, url.Values{"annotation": {"summary"}, "text": {"Errors on {{ $labels.instance }}"}})
	if code != http.StatusSeeOther {
		t.Fatalf("says: %d %s", code, zzMsg(body))
	}
	approve(t, h2, sid, ana)
	s2.jobs(ctx, false)
	sp := state(t, s2, sid)
	openThenMerge(t, port2, sid, false)
	os.WriteFile(filepath.Join(dir2, "shop.yaml"), []byte(strings.Replace(sp.Document, "Errors on", "Many errors on", 1)), 0o644)
	if err := s2.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if p := state(t, s2, sid); p.State != ledger.StateOpened {
		t.Fatalf("read while open: %s", p.State)
	}
	recipe(t, port2, http.MethodPatch, "/api/v1/changes/"+itoa(sid), changesToken, map[string]string{"state": "merged"})
	if p := state(t, s2, sid); p.State != ledger.StateAmended {
		t.Errorf("what it says, changed in review, merged after it was read: %s", p.State)
	}
}

func proposeThreshold(t *testing.T, h http.Handler) (int64, int, string) {
	t.Helper()
	code, body, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Errors"}, "kind": {"threshold"}, "bound": {"0"}, "value": {"7"}})
	return locID(hd), code, zzMsg(body)
}

// An admin closes, saying why, a proposal past Lapilli's hands: a pull
// request open or merged and never read back. No one else does, and not
// without a reason.
func TestAdminCloses(t *testing.T) {
	ctx := context.Background()
	for _, merge := range []bool{false, true} {
		s, _ := changesServer(t, nil)
		h, port := s.Handler(), s.IncidentsHandler()
		id := proposeFor(t, h, "15m")
		approve(t, h, id, ana)
		s.jobs(ctx, false)
		openThenMerge(t, port, id, merge)
		was := state(t, s, id).State
		if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), cy, "", nil); strings.Contains(body, "/drop") {
			t.Errorf("%s: an approver, not an admin, is offered to close it", was)
		}
		if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), ana, "", nil); !strings.Contains(body, `name="reason"`) {
			t.Errorf("%s: an admin isn't offered to close it", was)
		}
		for _, who := range []string{bo, cy} {
			if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/drop", who, "same-origin", url.Values{"reason": {"stuck"}}); code < 400 {
				t.Errorf("%s: %s closing it: %d", was, who, code)
			}
		}
		if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/drop", ana, "same-origin", url.Values{}); code != http.StatusBadRequest {
			t.Errorf("%s: closed with no reason: %d", was, code)
		}
		if p := state(t, s, id); p.State != was {
			t.Fatalf("%s: moved to %s", was, p.State)
		}
		if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/drop", ana, "same-origin", url.Values{"reason": {strings.Repeat("이유", 201)}}); code != http.StatusBadRequest {
			t.Errorf("%s: a reason of 402 characters: %d", was, code)
		}
		// 400 characters, in Korean: more bytes than that.
		if code, body, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/drop", ana, "same-origin", url.Values{"reason": {"merged elsewhere " + strings.Repeat("이유", 191)}}); code != http.StatusSeeOther {
			t.Fatalf("%s: an admin closing it: %d %s", was, code, zzMsg(body))
		}
		if p := state(t, s, id); p.State != ledger.StateDropped || !strings.Contains(p.Reason, "merged elsewhere") {
			t.Errorf("%s: closed: %s %q", was, p.State, p.Reason)
		}
		if n := len(handed(t, port)); n != 0 {
			t.Errorf("%s: still handed to the recipe: %d", was, n)
		}
		if _, code, msg := proposeThreshold(t, h); code != http.StatusSeeOther {
			t.Errorf("%s: the rule is still held up: %d %s", was, code, msg)
		}
	}
}

// A change sent to Grafana and not read back is closed by an admin only
// once it is read back again and still can't be, and the admin says it
// didn't reach Grafana: failed. Read back, it is whatever Grafana says.
func TestAdminClosesApplying(t *testing.T) {
	ctx := context.Background()
	f := &grafanaFlaky{g: &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}}
	f.unreadable.Store(true)
	s := flakyServer(t, f, nil)
	h := s.Handler()
	id := proposeFor(t, h, "15m")
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplying {
		t.Fatalf("applying: %s", p.State)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), ana, "", nil); !strings.Contains(body, `name="unwritten"`) {
		t.Error("an admin isn't asked whether it reached Grafana")
	}
	if code, body, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/drop", ana, "same-origin", url.Values{"reason": {"Grafana is gone"}}); code != http.StatusBadRequest || !strings.Contains(body, "can&#39;t be read back") {
		t.Errorf("closed without saying it didn't reach Grafana: %d %s", code, zzMsg(body))
	}
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/drop", ana, "same-origin", url.Values{"reason": {"Grafana is gone"}, "unwritten": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("closed: %d", code)
	}
	if p := state(t, s, id); p.State != ledger.StateFailed || !strings.Contains(p.Reason, "Grafana is gone") {
		t.Errorf("closed, unwritten: %s %q", p.State, p.Reason)
	}
	// Readable: read back, it is applied whatever the admin says.
	f2 := &grafanaFlaky{g: &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}}
	f2.unreadable.Store(true)
	s2 := flakyServer(t, f2, nil)
	h2 := s2.Handler()
	id2 := proposeFor(t, h2, "15m")
	approve(t, h2, id2, ana)
	s2.jobs(ctx, false)
	f2.unreadable.Store(false)
	if code, _, _ := ui(t, h2, http.MethodPost, "/proposals/"+itoa(id2)+"/drop", ana, "same-origin", url.Values{"reason": {"stuck"}, "unwritten": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("closing one Grafana can read: %d", code)
	}
	if p := state(t, s2, id2); p.State != ledger.StateApplied {
		t.Errorf("read back when closed: %s", p.State)
	}
}

// A proposal whose pull request opened before it was made into a task
// gives the task none of its links: the task gets a tracker's issue, and
// shows the pull request through the proposal.
func TestTaskOfOpenedProposal(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	h, port := s.Handler(), s.IncidentsHandler()
	id := proposeFor(t, h, "15m")
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	openThenMerge(t, port, id, false)
	code, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/task", bo, "same-origin", url.Values{})
	if code != http.StatusSeeOther {
		t.Fatalf("made into a task: %d", code)
	}
	task, err := s.l.TaskByID(ctx, state(t, s, id).TaskID)
	if err != nil || task.Link != "" {
		t.Fatalf("the task took the pull request's link: %+v %v", task, err)
	}
	if _, body, _ := ui(t, h, http.MethodGet, hd.Get("Location"), bo, "", nil); !strings.Contains(body, `href="https://git.example/pull/3"`) {
		t.Errorf("the task's page doesn't show the pull request")
	}
	// A task made elsewhere carries no proposal.
	other, _ := s.l.PutTask(ctx, ledger.Task{Title: "t", Kind: "tune", Rule: "Errors"}, bo)
	if code, body, _ := ui(t, h, http.MethodGet, "/tasks/"+itoa(other), bo, "", nil); code != 200 || strings.Contains(body, "/proposals/") {
		t.Errorf("a task of its own: %d", code)
	}
}

// --approvals raised from 1 to 2: a proposal one person approved, not yet
// carried out or not yet taken by a recipe, isn't carried out or handed
// out; it waits for a second approval. An undo, approved by the approver
// who asks for it, goes on.
func TestApprovalsRaised(t *testing.T) {
	ctx := context.Background()
	stop := filepath.Join(t.TempDir(), "paused")
	s, dir := changesServer(t, func(c *Config) { c.ChangesPaused = stop })
	h, port := s.Handler(), s.IncidentsHandler()
	// Approved while paused; ready, not yet taken.
	os.WriteFile(stop, []byte("1"), 0o644)
	waiting := proposeFor(t, h, "15m")
	approve(t, h, waiting, cy)
	s.jobs(ctx, false)
	if p := state(t, s, waiting); p.State != ledger.StateApproved {
		t.Fatalf("approved while paused: %s", p.State)
	}
	os.Remove(stop)
	s.cfg.Approvals = 2
	if n := ready(t, port); n != 0 {
		t.Errorf("%d handed out", n)
	}
	s.jobs(ctx, false)
	if p := state(t, s, waiting); p.State != ledger.StateProposed {
		t.Fatalf("raised to two, approved by one: %s", p.State)
	}
	if n := ready(t, port); n != 0 {
		t.Errorf("%d handed out with one approval of two", n)
	}
	approve(t, h, waiting, ana)
	s.jobs(ctx, false)
	p := state(t, s, waiting)
	if p.State != ledger.StateReady || ready(t, port) != 1 {
		t.Fatalf("approved by two: %s", p.State)
	}
	// Its undo, asked for by one approver, goes on.
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(p.Document), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	if p := state(t, s, waiting); p.State != ledger.StateApplied {
		t.Fatalf("applied: %s", p.State)
	}
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(waiting)+"/undo", ana, "same-origin", url.Values{"key": {keyFor(t, h, waiting, ana, "undo")}, "read": {"1"}})
	s.jobs(ctx, false)
	if u := state(t, s, locID(hd)); u.State != ledger.StateReady || ready(t, port) != 1 {
		t.Errorf("the undo, under two approvals: %s", u.State)
	}

	// Ready under one approval, raised before a recipe took it.
	s2, _ := changesServer(t, nil)
	h2, port2 := s2.Handler(), s2.IncidentsHandler()
	id := proposeFor(t, h2, "15m")
	approve(t, h2, id, cy)
	s2.jobs(ctx, false)
	if ready(t, port2) != 1 {
		t.Fatal("not ready")
	}
	s2.cfg.Approvals = 2
	if n := ready(t, port2); n != 0 {
		t.Errorf("ready with one approval of two, handed out: %d", n)
	}
	s2.jobs(ctx, false)
	if p := state(t, s2, id); p.State != ledger.StateProposed || len(p.Approvals) != 1 {
		t.Errorf("ready, raised: %s with %d approvals", p.State, len(p.Approvals))
	}
}

// The proposals' pages, in each language, find a translation for every
// text they show, with proposals in every state there is a form for: to
// approve, to drop, to close one opened, merged or applying, and the task
// a proposal was made into.
func TestProposalPagesInEachLanguage(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, nil)
	h, port := s.Handler(), s.IncidentsHandler()
	file := filepath.Join(dir, "shop.yaml")
	os.WriteFile(filepath.Join(dir, "more.yaml"), []byte("groups:\n  - name: more\n    rules:\n"+
		"      - alert: A\n        expr: errors > 1\n      - alert: B\n        expr: errors > 2\n      - alert: C\n        expr: errors > 3\n"), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	propose := func(rule string) int64 {
		code, body, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {rule}, "kind": {"for"}, "for": {"15m"}})
		if code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", rule, code, zzMsg(body))
		}
		return locID(hd)
	}
	amended := proposeFor(t, h, "15m")
	approve(t, h, amended, ana)
	s.jobs(ctx, false)
	p := state(t, s, amended)
	openThenMerge(t, port, amended, true)
	os.WriteFile(file, []byte(strings.Replace(p.Document, "for: 15m", "for: 20m", 1)), 0o644)
	if err := s.look(ctx, new(string)); err != nil {
		t.Fatal(err)
	}
	opened, merged, proposed := propose("A"), propose("B"), propose("C")
	for _, id := range []int64{opened, merged} {
		approve(t, h, id, ana)
	}
	s.jobs(ctx, false)
	openThenMerge(t, port, opened, false)
	openThenMerge(t, port, merged, true)
	ui(t, h, http.MethodPost, "/proposals/"+itoa(merged)+"/task", bo, "same-origin", url.Values{})
	if st := []string{state(t, s, amended).State, state(t, s, opened).State, state(t, s, merged).State, state(t, s, proposed).State}; !slices.Equal(st,
		[]string{ledger.StateAmended, ledger.StateOpened, ledger.StateMerged, ledger.StateProposed}) {
		t.Fatalf("states: %v", st)
	}
	// One sent to Grafana and not read back.
	f := &grafanaFlaky{g: &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}}
	f.unreadable.Store(true)
	g := flakyServer(t, f, nil)
	gh := g.Handler()
	applying := proposeFor(t, gh, "15m")
	approve(t, gh, applying, ana)
	g.jobs(ctx, false)
	missing.Lock()
	clear(missing.m)
	missing.Unlock()
	task := state(t, s, merged).TaskID
	for _, c := range []struct {
		h     http.Handler
		paths []string
	}{
		{h, []string{"/proposals", "/proposals?state=all", "/proposals/" + itoa(amended), "/proposals/" + itoa(opened), "/proposals/" + itoa(merged),
			"/proposals/" + itoa(proposed), "/rules/Errors", "/tasks/" + itoa(task)}},
		{gh, []string{"/proposals", "/proposals/" + itoa(applying)}},
	} {
		for _, n := range names {
			for _, who := range []string{ana, bo, cy} {
				for _, path := range c.paths {
					r := httptest.NewRequest(http.MethodGet, path, nil)
					r.Header.Set("Accept-Language", n.Code)
					r.Header.Set("X-Forwarded-Email", who)
					w := httptest.NewRecorder()
					c.h.ServeHTTP(w, r)
					if w.Code != 200 || !strings.Contains(w.Body.String(), `<html lang="`+n.Code+`">`) {
						t.Errorf("%s %s as %s: %d", n.Code, path, who, w.Code)
					}
				}
			}
		}
	}
	missing.Lock()
	for k := range missing.m {
		lang, msg, _ := strings.Cut(k, "\x00")
		t.Errorf("%s has no translation for %q", lang, msg)
	}
	missing.Unlock()
}
