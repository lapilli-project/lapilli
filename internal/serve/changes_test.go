package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// Changing rules with a person's approval (docs/design/changes.md): each
// test here is a behaviour, and the security ones are what an attacker
// would try (CONTRIBUTING.md).

const (
	ana = "ana@example.com" // an admin, and an approver
	bo  = "bo@example.com"  // an editor
	cy  = "cy@example.com"  // an approver
)

const changesToken = "changes-token-0123456789"

// changesServer is a server with changes on, its rules read and replayed.
func changesServer(t *testing.T, more func(*Config)) (*Server, string) {
	t.Helper()
	s, dir := server(t, func(c *Config) {
		c.UserHeader = "X-Forwarded-Email"
		c.Editors = []string{"*"}
		c.Admins = []string{ana}
		c.Approvers = []string{ana, cy}
		c.Changes = true
		c.ChangesToken = tokenFile(t, changesToken)
		if more != nil {
			more(c)
		}
	})
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	return s, dir
}

// ui asks the UI as someone, from a site, with a form.
func ui(t *testing.T, h http.Handler, method, path, who, site string, form url.Values) (int, string, http.Header) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if site != "" {
		r.Header.Set("Sec-Fetch-Site", site)
	}
	if who != "" {
		r.Header.Set("X-Forwarded-Email", who)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String(), w.Header()
}

// recipe asks the incidents port as a change recipe would.
func recipe(t *testing.T, h http.Handler, method, path, token string, body any) (int, string) {
	t.Helper()
	var in io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		in = strings.NewReader(string(b))
	}
	r := httptest.NewRequest(method, path, in)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// handed are the changes the recipe reads.
func handed(t *testing.T, port http.Handler) []handedOut {
	t.Helper()
	code, body := recipe(t, port, http.MethodGet, "/api/v1/changes", changesToken, nil)
	var got struct{ Changes []handedOut }
	if err := json.Unmarshal([]byte(body), &got); code != 200 || err != nil {
		t.Fatalf("reading changes: %d %v %s", code, err, body)
	}
	return got.Changes
}

// ready are the changes handed out ready for a pull request.
func ready(t *testing.T, port http.Handler) int {
	n := 0
	for _, c := range handed(t, port) {
		if c.State == ledger.StateReady {
			n++
		}
	}
	return n
}

var formKey = regexp.MustCompile(`action="/proposals/(\d+)/(approve|undo)"><input type="hidden" name="key" value="([0-9a-f]+)"`)

// keyFor is the key the proposal's page gives someone for a form, "" when
// it offers them none.
func keyFor(t *testing.T, h http.Handler, id int64, who, what string) string {
	t.Helper()
	code, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), who, "", nil)
	if code != 200 {
		t.Fatalf("the proposal's page for %s: %d\n%s", who, code, body)
	}
	for _, m := range formKey.FindAllStringSubmatch(body, -1) {
		if m[1] == itoa(id) && m[2] == what {
			return m[3]
		}
	}
	return ""
}

// proposeFor proposes a new for for Errors, as bo.
func proposeFor(t *testing.T, h http.Handler, d string) int64 {
	t.Helper()
	code, body, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Errors"}, "kind": {"for"}, "for": {d}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing: %d\n%s", code, body)
	}
	var id int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &id)
	if id == 0 {
		t.Fatalf("proposing went to %q", hd.Get("Location"))
	}
	return id
}

func approve(t *testing.T, h http.Handler, id int64, who string) (int, string) {
	t.Helper()
	key := keyFor(t, h, id, who, "approve")
	if key == "" {
		t.Fatalf("no approval form for %s", who)
	}
	code, body, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/approve", who, "same-origin", url.Values{"key": {key}})
	return code, body
}

func state(t *testing.T, s *Server, id int64) ledger.Proposal {
	t.Helper()
	p, err := s.l.ProposalByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The git path, all the way: proposed with its backtest, approved, checked
// again and handed to a recipe, opened, merged, read back as applied; then
// undone the same way.
func TestChangeByPullRequest(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, nil)
	s.l.TellTo([]string{"webhook"})
	h, port := s.Handler(), s.IncidentsHandler()
	id := proposeFor(t, h, "15m")
	p := state(t, s, id)
	file := filepath.Join(dir, "shop.yaml")
	if p.Path != ledger.PathGit || p.State != ledger.StateProposed || p.MadeBy != bo || !strings.Contains(p.Document, "for: 15m") ||
		p.DocHash != rules.Hash([]byte(ruleFile)) || !strings.Contains(p.Diff, "+        for: 15m") {
		t.Fatalf("the proposal: %+v", p)
	}
	ev := proposalEvidence(p)
	if ev.Backtest == nil || ev.Backtest.Before.Firings != 21 || ev.Backtest.After.Firings != 1 || len(ev.Backtest.Lost) != 0 || ev.Backtest.Before.White != 1 {
		t.Errorf("the backtest: %+v", ev.Backtest)
	}
	// It shows on the rule's page and the rules list.
	if _, body, _ := ui(t, h, http.MethodGet, "/rules/Errors", bo, "", nil); !strings.Contains(body, `href="/proposals/`+itoa(id)+`"`) {
		t.Errorf("the rule's page doesn't show it")
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/rules", bo, "", nil); !strings.Contains(body, "a change is proposed") {
		t.Errorf("the rules list doesn't mark it")
	}
	// Nothing is handed out before it is approved.
	if n := len(handed(t, port)); n != 0 {
		t.Errorf("%d handed out before approval", n)
	}
	if code, body := approve(t, h, id, ana); code != http.StatusSeeOther {
		t.Fatalf("approving: %d %s", code, body)
	}
	if p := state(t, s, id); p.State != ledger.StateApproved || len(p.Approvals) != 1 || p.Approvals[0].By != ana {
		t.Fatalf("approved: %+v", p)
	}
	s.jobs(ctx, false)
	if p = state(t, s, id); p.State != ledger.StateReady || p.Applied.IsZero() || p.Backup != ruleFile {
		t.Fatalf("checked again and ready: %+v", p)
	}
	// The recipe reads it, opens a pull request, and says so.
	code, body := recipe(t, port, http.MethodGet, "/api/v1/changes", changesToken, nil)
	var got struct{ Changes []handedOut }
	if json.Unmarshal([]byte(body), &got); code != 200 || len(got.Changes) != 1 || got.Changes[0].Document != p.Document ||
		got.Changes[0].DocHash != p.DocHash || got.Changes[0].Branch != "lapilli/proposal-"+itoa(id) || !strings.Contains(got.Changes[0].Body, "Backtest") {
		t.Fatalf("handed out: %d %s", code, body)
	}
	if code, body := recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "opened", "link": "https://git.example/shop/pull/7"}); code != 200 {
		t.Fatalf("opened: %d %s", code, body)
	}
	if code, body := recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "merged"}); code != 200 {
		t.Fatalf("merged: %d %s", code, body)
	}
	// Merged isn't applied: read back with the change, it is.
	if p = state(t, s, id); p.State != ledger.StateMerged || p.Link != "https://git.example/shop/pull/7" {
		t.Fatalf("merged: %+v", p)
	}
	os.WriteFile(file, []byte(p.Document), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if p = state(t, s, id); p.State != ledger.StateApplied {
		t.Fatalf("read back: %+v", p)
	}
	// Undone: an approver asks, and it goes the same way back.
	key := keyFor(t, h, id, ana, "undo")
	if key == "" {
		t.Fatal("no undo form")
	}
	code, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}})
	if code != http.StatusSeeOther {
		t.Fatalf("undo: %d", code)
	}
	var uid int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &uid)
	s.jobs(ctx, false)
	u := state(t, s, uid)
	if u.State != ledger.StateReady || u.Document != ruleFile || u.DocHash != rules.Hash([]byte(p.Document)) || u.Follows != id {
		t.Fatalf("the undo: %+v", u)
	}
	os.WriteFile(file, []byte(u.Document), 0o644)
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if u, p = state(t, s, uid), state(t, s, id); u.State != ledger.StateApplied || p.State != ledger.StateUndone {
		t.Errorf("undone: %s, the change %s", u.State, p.State)
	}
	// Each step was told.
	ns, _ := s.l.Due(ctx, "webhook", time.Now().Add(time.Hour), 100)
	var events []string
	for _, n := range ns {
		events = append(events, n.Event)
	}
	for _, want := range []string{"proposal.made", "proposal.approved", "change.opened", "change.applied", "change.undone"} {
		if !strings.Contains(strings.Join(events, " "), want) {
			t.Errorf("not told %s: %v", want, events)
		}
	}
}

// Two approvals, when asked for: by two people, neither the one who
// proposed it.
func TestTwoApprovals(t *testing.T) {
	s, _ := changesServer(t, func(c *Config) { c.Approvals = 2 })
	h := s.Handler()
	id := proposeFor(t, h, "15m")
	if code, _ := approve(t, h, id, ana); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	if p := state(t, s, id); p.State != ledger.StateProposed {
		t.Fatalf("one of two: %s", p.State)
	}
	// The same person again: no form, and the old one no longer counts.
	if keyFor(t, h, id, ana, "approve") != "" {
		t.Error("a second form for the same approver")
	}
	if code, _ := approve(t, h, id, cy); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	if p := state(t, s, id); p.State != ledger.StateApproved || len(p.Approvals) != 2 {
		t.Fatalf("two of two: %+v", p)
	}
}

// Approving is a person's, on the page, in a browser: what an attacker
// would try instead is refused.
func TestApprovalOnlyByAPerson(t *testing.T) {
	s, _ := changesServer(t, nil)
	h, port := s.Handler(), s.IncidentsHandler()
	id := proposeFor(t, h, "15m")
	path := "/proposals/" + itoa(id) + "/approve"
	key := keyFor(t, h, id, ana, "approve")
	for name, try := range map[string]func() int{
		"no key": func() int { c, _, _ := ui(t, h, http.MethodPost, path, ana, "same-origin", url.Values{}); return c },
		"a key made up": func() int {
			c, _, _ := ui(t, h, http.MethodPost, path, ana, "same-origin", url.Values{"key": {strings.Repeat("0", 64)}})
			return c
		},
		"another person's key": func() int {
			c, _, _ := ui(t, h, http.MethodPost, path, cy, "same-origin", url.Values{"key": {key}})
			return c
		},
		"from another site": func() int {
			c, _, _ := ui(t, h, http.MethodPost, path, ana, "cross-site", url.Values{"key": {key}})
			return c
		},
		"not from a browser": func() int { c, _, _ := ui(t, h, http.MethodPost, path, ana, "", url.Values{"key": {key}}); return c },
		"from a sibling site": func() int {
			c, _, _ := ui(t, h, http.MethodPost, path, ana, "same-site", url.Values{"key": {key}})
			return c
		},
		"an editor": func() int {
			c, _, _ := ui(t, h, http.MethodPost, path, bo, "same-origin", url.Values{"key": {key}})
			return c
		},
		"no one signed in": func() int {
			c, _, _ := ui(t, h, http.MethodPost, path, "", "same-origin", url.Values{"key": {key}})
			return c
		},
		"lapilli itself": func() int {
			c, _, _ := ui(t, h, http.MethodPost, path, ledger.Proposer, "same-origin", url.Values{"key": {key}})
			return c
		},
		"the changes token": func() int { c, _ := recipe(t, port, http.MethodPost, path, changesToken, nil); return c },
		"the API": func() int {
			c, _, _ := ui(t, h, http.MethodPost, "/api/v1/proposals/"+itoa(id)+"/approve", ana, "same-origin", url.Values{"key": {key}})
			return c
		},
		"the API, as a PATCH": func() int {
			c, _, _ := ui(t, h, http.MethodPatch, "/api/v1/proposals/"+itoa(id), ana, "same-origin", url.Values{"state": {"approved"}})
			return c
		},
		"the token, as a state": func() int {
			c, _ := recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "approved"})
			return c
		},
	} {
		if code := try(); code < 400 {
			t.Errorf("%s: %d, want it refused", name, code)
		}
	}
	if p := state(t, s, id); p.State != ledger.StateProposed || len(p.Approvals) != 0 {
		t.Fatalf("approved by one of them: %+v", p)
	}
	// A key for someone who may not approve, were one to be had, gets them
	// nowhere either.
	for _, who := range []string{bo, ""} {
		k := s.formKey(state(t, s, id), map[string]string{bo: bo, "": "anonymous"}[who], "approve")
		if code, _, _ := ui(t, h, http.MethodPost, path, who, "same-origin", url.Values{"key": {k}}); code < 400 {
			t.Errorf("%q with a key of their own: %d", who, code)
		}
	}
	// The key is good for the proposal as it stood: once it moved on, not.
	s.l.Move(context.Background(), id, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{})
	if code, _, _ := ui(t, h, http.MethodPost, path, ana, "same-origin", url.Values{"key": {key}}); code < 400 {
		t.Errorf("a dropped proposal approved with an old key: %d", code)
	}
	// One's own proposal: no form, and the ledger refuses it too.
	code, _, hd := ui(t, h, http.MethodPost, "/proposals", ana, "same-origin", url.Values{"rule": {"Errors"}, "kind": {"threshold"}, "bound": {"0"}, "value": {"3"}})
	if code != http.StatusSeeOther {
		t.Fatalf("ana proposing: %d", code)
	}
	var own int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &own)
	if keyFor(t, h, own, ana, "approve") != "" {
		t.Error("a form to approve one's own proposal")
	}
	if _, err := s.l.Approve(context.Background(), own, "ANA@example.com", 1); err == nil {
		t.Error("one's own, in another case: approved")
	}
}

// Without --changes nothing is carried out or handed out, and no one
// approves; proposals are still kept.
func TestChangesOff(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, func(c *Config) { c.Changes = false })
	h, port := s.Handler(), s.IncidentsHandler()
	id := proposeFor(t, h, "15m")
	if keyFor(t, h, id, ana, "approve") != "" {
		t.Error("a form to approve with changes off")
	}
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/approve", ana, "same-origin", url.Values{"key": {s.formKey(state(t, s, id), ana, "approve")}}); code < 400 {
		t.Errorf("approved with changes off: %d", code)
	}
	// Even approved in the ledger, it stays put.
	s.l.Approve(ctx, id, ana, 1)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApproved {
		t.Errorf("carried out with changes off: %s", p.State)
	}
	if n := len(handed(t, port)); n != 0 {
		t.Errorf("%d handed out with changes off", n)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals", bo, "", nil); !strings.Contains(body, "Changes are off") {
		t.Error("the proposals page doesn't say changes are off")
	}
}

// The stop: an admin's switch, or the file, stops what is carried out and
// handed out at once, and every page says so.
func TestPause(t *testing.T) {
	ctx := context.Background()
	stop := filepath.Join(t.TempDir(), "paused")
	s, _ := changesServer(t, func(c *Config) { c.ChangesPaused = stop })
	h, port := s.Handler(), s.IncidentsHandler()
	id := proposeFor(t, h, "15m")
	approve(t, h, id, ana)
	// Only an admin pauses.
	if code, _, _ := ui(t, h, http.MethodPost, "/settings/changes", bo, "same-origin", url.Values{"paused": {"1"}}); code != http.StatusForbidden {
		t.Errorf("an editor pausing: %d", code)
	}
	if code, _, _ := ui(t, h, http.MethodPost, "/settings/changes", ana, "same-origin", url.Values{"paused": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("pausing: %d", code)
	}
	// Who paused is noted in the changes.
	if cs, err := s.l.Changes(ctx, 5); err != nil || len(cs) == 0 || cs[0].What != "changes to the rules paused" || cs[0].By != ana {
		t.Errorf("pausing isn't noted as ana's: %+v %v", cs, err)
	}
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApproved {
		t.Fatalf("carried out while paused: %s", p.State)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/incidents", bo, "", nil); !strings.Contains(body, "Changes to the rules are paused") {
		t.Error("a page doesn't say changes are paused")
	}
	ui(t, h, http.MethodPost, "/settings/changes", ana, "same-origin", url.Values{"paused": {"0"}})
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateReady {
		t.Fatalf("resumed: %s", p.State)
	}
	// The file: what is ready isn't handed out while it is there.
	os.WriteFile(stop, nil, 0o600)
	if n := ready(t, port); n != 0 {
		t.Errorf("%d handed out while paused", n)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/", bo, "", nil); !strings.Contains(body, "Changes to the rules are paused") {
		t.Error("the overview doesn't say changes are paused by the file")
	}
	os.Remove(stop)
	if n := ready(t, port); n != 1 {
		t.Errorf("%d handed out once the file is gone", n)
	}
}

// The limits: a change past them waits.
func TestLimits(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, func(c *Config) { c.ChangesLimits = Limits{Deletions: 0, Changes: 1} })
	h := s.Handler()
	os.WriteFile(filepath.Join(dir, "extra.yaml"), []byte(`groups:
  - name: extra
    rules:
      - alert: Spare
        expr: errors > 9
      - alert: Kept
        expr: errors > 7
`), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	a := proposeFor(t, h, "15m")
	approve(t, h, a, ana)
	code, _, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Kept"}, "kind": {"threshold"}, "bound": {"0"}, "value": {"3"}})
	if code != http.StatusSeeOther {
		t.Fatal(code)
	}
	var b int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &b)
	approve(t, h, b, ana)
	s.jobs(ctx, false)
	if x, y := state(t, s, a), state(t, s, b); x.State != ledger.StateReady || y.State != ledger.StateApproved {
		t.Errorf("one change an hour: %s and %s", x.State, y.State)
	}
	// No deletions a day: a deletion waits, however few changes there were.
	s.cfg.ChangesLimits = Limits{Deletions: 0, Changes: 100}
	code, body, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Spare"}, "kind": {"delete"}})
	if code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	var d int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &d)
	if p := state(t, s, d); p.Path != ledger.PathGit || strings.Contains(p.Document, "Spare") || !strings.Contains(p.Document, "Kept") {
		t.Fatalf("deleting Spare: %+v", p)
	}
	approve(t, h, d, ana)
	s.jobs(ctx, false)
	if p := state(t, s, d); p.State != ledger.StateApproved {
		t.Errorf("a deletion past the day's limit: %s", p.State)
	}
	s.cfg.ChangesLimits = Limits{Deletions: 1, Changes: 100}
	s.jobs(ctx, false)
	if p := state(t, s, d); p.State != ledger.StateReady {
		t.Errorf("a deletion within the limits: %s", p.State)
	}
	// The file's only rule: a file of no rules isn't written, so it is for
	// a person. (Its for, handed out, is dropped first: one at a time.)
	if _, err := s.l.Move(ctx, a, []string{ledger.StateReady}, ledger.StateDropped, ana, ledger.Step{}); err != nil {
		t.Fatal(err)
	}
	code, _, hd = ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Errors"}, "kind": {"delete"}})
	if code != http.StatusSeeOther {
		t.Fatal(code)
	}
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &d)
	if p := state(t, s, d); p.Path != ledger.PathNone || keyFor(t, h, d, ana, "approve") != "" {
		t.Errorf("deleting a file's only rule: path %s", p.Path)
	}
	// Nor is one for a person approved, even with a key.
	if code, _, _ := ui(t, h, http.MethodPost, "/proposals/"+itoa(d)+"/approve", ana, "same-origin", url.Values{"key": {s.formKey(state(t, s, d), ana, "approve")}}); code < 400 {
		t.Errorf("a change for a person, approved: %d", code)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(d), ana, "", nil); !strings.Contains(body, "A pull request&#39;s description, to copy") {
		t.Errorf("no description for a person to copy")
	}
}

// Checked again before it is carried out: a rule changed since is refused
// as stale; one whose file changed elsewhere gets the change made afresh;
// a backtest that got worse is refused.
func TestCheckedAgain(t *testing.T) {
	ctx := context.Background()
	t.Run("stale", func(t *testing.T) {
		s, dir := changesServer(t, nil)
		h := s.Handler()
		id := proposeFor(t, h, "15m")
		approve(t, h, id, ana)
		os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(strings.Replace(ruleFile, "errors > 5", "errors > 1", 1)), 0o644)
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateRefused || !strings.Contains(p.Reason, "the rule changed since it was proposed") {
			t.Errorf("stale: %+v", p)
		}
	})
	t.Run("file changed elsewhere", func(t *testing.T) {
		s, dir := changesServer(t, nil)
		h := s.Handler()
		id := proposeFor(t, h, "15m")
		approve(t, h, id, ana)
		more := "# owned by the shop team\n" + ruleFile
		os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(more), 0o644)
		s.jobs(ctx, false)
		p := state(t, s, id)
		if p.State != ledger.StateReady || p.DocHash != rules.Hash([]byte(more)) || !strings.HasPrefix(p.Document, "# owned by the shop team\n") || !strings.Contains(p.Document, "for: 15m") {
			t.Errorf("made afresh: %+v", p)
		}
	})
	t.Run("worse", func(t *testing.T) {
		s, _ := changesServer(t, nil)
		sh := &shortHistory{}
		s.h = sh
		h := s.Handler()
		id := proposeFor(t, h, "15m")
		if ev := proposalEvidence(state(t, s, id)); ev.Backtest == nil || len(ev.Backtest.Lost) != 0 {
			t.Fatalf("the backtest approved: %+v", ev.Backtest)
		}
		approve(t, h, id, ana)
		sh.short.Store(true) // the outage was short: a longer for misses it
		s.jobs(ctx, false)
		if p := state(t, s, id); p.State != ledger.StateRefused || !strings.Contains(p.Reason, "INC-1") {
			t.Errorf("worse: %+v", p)
		}
	})
}

// shortHistory is history, but with the checkout outage cut to five
// minutes when short is set.
type shortHistory struct {
	history
	short atomic.Bool
}

func (h *shortHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	out, err := h.history.QueryRange(ctx, expr, start, end, step)
	if err != nil || !h.short.Load() {
		return out, err
	}
	for i, s := range out {
		if s.Labels["service"] != "checkout" {
			continue
		}
		var kept []prom.Sample
		for _, x := range s.Samples {
			if x.T.Before(now.Add(-30*time.Hour + 5*time.Minute)) {
				kept = append(kept, x)
			}
		}
		out[i].Samples = kept
	}
	return out, nil
}

// The changes token reads changes and says what became of them, and
// nothing else; no other token, or none, reaches it.
func TestChangesToken(t *testing.T) {
	s, _ := changesServer(t, func(c *Config) {
		c.IncidentsToken = tokenFile(t, "incidents-token-0123456789")
		c.FiringsToken = tokenFile(t, "firings-token-0123456789")
	})
	h, port := s.Handler(), s.IncidentsHandler()
	id := proposeFor(t, h, "15m")
	approve(t, h, id, ana)
	s.jobs(context.Background(), false)
	ch := "/api/v1/changes/" + itoa(id)
	for name, tok := range map[string]string{"none": "", "the incidents token": "incidents-token-0123456789", "the firings token": "firings-token-0123456789", "a guess": "changes-token-0123456780"} {
		if code, _ := recipe(t, port, http.MethodGet, "/api/v1/changes", tok, nil); code != http.StatusUnauthorized {
			t.Errorf("%s reading changes: %d", name, code)
		}
	}
	// What it may say, and nothing more.
	for _, bad := range []map[string]any{
		{"state": "applied"}, {"state": "approved"}, {"state": "merged"}, // merged before opened
		{"state": "opened", "link": "javascript:alert(1)"}, {"state": "opened", "link": "https://user:pass@git.example/x"},
		{"state": "opened"}, {"state": "failed"}, {"state": "opened", "link": "https://git.example/1", "approvals": 2},
	} {
		if code, body := recipe(t, port, http.MethodPatch, ch, changesToken, bad); code < 400 {
			t.Errorf("%v: %d %s", bad, code, body)
		}
	}
	// The token reaches nothing else on the port, nor the UI's API.
	if code, _ := recipe(t, port, http.MethodPut, "/api/v1/incidents/X", changesToken, map[string]string{"title": "x"}); code < 400 {
		t.Errorf("writing an incident with the changes token: %d", code)
	}
	if code, _ := recipe(t, port, http.MethodGet, "/api/v1/proposals", changesToken, nil); code < 400 {
		t.Errorf("the UI's API on the incidents port: %d", code)
	}
	if code, body := recipe(t, port, http.MethodPatch, ch, changesToken, map[string]string{"state": "failed", "reason": "no file in the repository is the document"}); code != 200 {
		t.Fatalf("failed: %d %s", code, body)
	}
	if p := state(t, s, id); p.State != ledger.StateFailed || p.Reason != "no file in the repository is the document" {
		t.Errorf("failed: %+v", p)
	}
	// One token for two isn't taken.
	s.cfg.IncidentsToken = s.cfg.ChangesToken
	if code, _ := recipe(t, s.IncidentsHandler(), http.MethodGet, "/api/v1/changes", changesToken, nil); code != http.StatusUnauthorized {
		t.Errorf("the changes token, also the incidents token: %d", code)
	}
}

// A proposal made into a task, which takes the pull request's link.
func TestProposalTask(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	h, port := s.Handler(), s.IncidentsHandler()
	id := proposeFor(t, h, "15m")
	code, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/task", bo, "same-origin", url.Values{})
	if code != http.StatusSeeOther || !strings.HasPrefix(hd.Get("Location"), "/tasks/") {
		t.Fatalf("made into a task: %d %s", code, hd.Get("Location"))
	}
	p := state(t, s, id)
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	recipe(t, port, http.MethodPatch, "/api/v1/changes/"+itoa(id), changesToken, map[string]string{"state": "opened", "link": "https://git.example/pull/9"})
	// The task's link is its tracker's: its page shows the pull request
	// through the proposal.
	task, err := s.l.TaskByID(ctx, p.TaskID)
	if err != nil || task.Link != "" || task.Rule != "Errors" {
		t.Errorf("the task: %+v %v", task, err)
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/tasks/"+itoa(p.TaskID), bo, "", nil); !strings.Contains(body, `href="/proposals/`+itoa(id)+`"`) ||
		!strings.Contains(body, `href="https://git.example/pull/9"`) {
		t.Errorf("the task's page doesn't show its proposal and pull request")
	}
}

// What Lapilli proposes of itself, after the week's digest: only with
// --changes, once a week, and not again what people turned down.
func TestProposeFound(t *testing.T) {
	ctx := context.Background()
	s, _ := changesServer(t, nil)
	if err := s.proposeWeekly(ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{})
	var fors int
	for _, p := range ps {
		if p.MadeBy != ledger.Proposer {
			t.Errorf("made by %s", p.MadeBy)
		}
		if p.Kind == ledger.KindFor {
			fors++
			s.l.Move(ctx, p.ID, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{})
		}
	}
	if fors != 1 {
		t.Fatalf("the noisy rule's longer for: %d proposals of %d", fors, len(ps))
	}
	// Once a week, and not again once dropped.
	s.l.SetMeta(ctx, foundKey, "")
	if err := s.proposeWeekly(ctx); err != nil {
		t.Fatal(err)
	}
	again, _ := s.l.Proposals(ctx, ledger.ProposalQuery{})
	if len(again) != len(ps) {
		t.Errorf("proposed again what was dropped: %d, then %d", len(ps), len(again))
	}
	// Without --changes, nothing of itself.
	off, _ := changesServer(t, func(c *Config) { c.Changes = false })
	if err := off.proposeWeekly(ctx); err != nil {
		t.Fatal(err)
	}
	if ps, _ := off.l.Proposals(ctx, ledger.ProposalQuery{}); len(ps) != 0 {
		t.Errorf("proposed with changes off: %d", len(ps))
	}
}

// grafanaStandIn is Grafana's provisioning API, as far as changes use it:
// one folder's rules, each with its provenance.
type grafanaStandIn struct {
	t          *testing.T
	rules      map[string]map[string]any // by uid
	provenance map[string]string
	writes     []string // what was written: METHOD uid
	redirect   string   // where writes are sent on to, when set
	// keepsPause answers a write that pauses a rule as taking it, and
	// leaves the rule running, as Grafana 13 does for a service account
	// that isn't an Admin.
	keepsPause bool
}

func (g *grafanaStandIn) export(only string) []byte {
	var rs []any
	for _, uid := range []string{"abc", "def"} {
		if r, ok := g.rules[uid]; ok && (only == "" || only == uid) {
			rs = append(rs, r)
		}
	}
	// As Grafana 13 gives them: a rule's own export names no folder.
	folder := "Platform"
	if only != "" {
		folder = ""
	}
	b, _ := json.Marshal(map[string]any{"apiVersion": 1, "groups": []any{map[string]any{"orgId": 1, "name": "api", "folder": folder, "interval": "1m", "rules": rs}}})
	return b
}

func (g *grafanaStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer grafana-writes-0123456789" && r.URL.Path != "/api/v1/provisioning/alert-rules/export" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if g.redirect != "" && r.Method != http.MethodGet {
		http.Redirect(w, r, g.redirect, http.StatusTemporaryRedirect)
		return
	}
	if r.Method != http.MethodGet && r.Header.Get("X-Disable-Provenance") != "true" {
		g.t.Errorf("%s %s without X-Disable-Provenance", r.Method, r.URL.Path)
	}
	const base = "/api/v1/provisioning/alert-rules"
	rest := strings.TrimPrefix(r.URL.Path, base)
	switch {
	case r.Method == http.MethodGet && rest == "/export":
		w.Write(g.export(""))
	case r.Method == http.MethodGet && strings.HasSuffix(rest, "/export"):
		uid := strings.TrimSuffix(strings.TrimPrefix(rest, "/"), "/export")
		if _, ok := g.rules[uid]; !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(g.export(uid))
	case r.Method == http.MethodGet:
		uid := strings.TrimPrefix(rest, "/")
		rule, ok := g.rules[uid]
		if !ok {
			http.NotFound(w, r)
			return
		}
		out := map[string]any{"id": 7, "updated": "2026-11-01T00:00:00Z", "provenance": g.provenance[uid], "folderUID": "f1", "ruleGroup": "api"}
		for k, v := range rule {
			out[k] = v
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPut || r.Method == http.MethodPost:
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		uid, _ := in["uid"].(string)
		if r.Method == http.MethodPut && strings.TrimPrefix(rest, "/") != uid {
			http.Error(w, "uid", http.StatusBadRequest)
			return
		}
		for _, k := range []string{"id", "updated", "provenance"} {
			if _, ok := in[k]; ok {
				g.t.Errorf("%s sent %s, which Grafana sets", r.Method, k)
			}
		}
		delete(in, "folderUID")
		delete(in, "ruleGroup")
		if was, ok := g.rules[uid]["isPaused"]; g.keepsPause && ok {
			in["isPaused"] = was
		} else if g.keepsPause {
			delete(in, "isPaused")
		}
		g.rules[uid] = in
		g.writes = append(g.writes, r.Method+" "+uid)
		json.NewEncoder(w).Encode(in)
	case r.Method == http.MethodDelete:
		uid := strings.TrimPrefix(rest, "/")
		delete(g.rules, uid)
		g.writes = append(g.writes, "DELETE "+uid)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func grafanaRules() map[string]map[string]any {
	rule := func(uid, title, expr string) map[string]any {
		return map[string]any{"uid": uid, "title": title, "condition": "A", "for": "2m", "labels": map[string]any{"severity": "page"},
			"data": []any{map[string]any{"refId": "A", "datasourceUid": "prom", "model": map[string]any{"expr": expr}}}}
	}
	return map[string]map[string]any{"abc": rule("abc", "Errors", "errors > 5"), "def": rule("def", "Other", "errors > 9")}
}

func grafanaServer(t *testing.T, g *grafanaStandIn) *Server {
	t.Helper()
	ts := httptest.NewServer(g)
	t.Cleanup(ts.Close)
	s, _ := changesServer(t, func(c *Config) {
		c.RulesPath, c.Grafana = "", ts.URL
		c.ChangesGrafanaToken = tokenFile(t, "grafana-writes-0123456789")
	})
	return s
}

// A rule kept only in Grafana: applied through its API, with the rule as
// it was kept, and undone by writing that back.
func TestChangeInGrafana(t *testing.T) {
	ctx := context.Background()
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	s := grafanaServer(t, g)
	h := s.Handler()
	id := proposeFor(t, h, "15m")
	p := state(t, s, id)
	if p.Path != ledger.PathGrafana || proposalEvidence(p).Backtest == nil {
		t.Fatalf("the proposal: %+v", p)
	}
	if len(g.writes) != 0 {
		t.Fatalf("written before approval: %v", g.writes)
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	p = state(t, s, id)
	if p.State != ledger.StateApplied || g.rules["abc"]["for"] != "15m" || len(g.writes) != 1 || p.AfterHash == "" {
		t.Fatalf("applied: %+v, Grafana has %v, wrote %v", p, g.rules["abc"]["for"], g.writes)
	}
	var kept map[string]any
	if json.Unmarshal([]byte(p.Backup), &kept); kept["for"] != "2m" || kept["provenance"] != "" {
		t.Errorf("the rule as it was: %s", p.Backup)
	}
	if g.rules["def"]["for"] != "2m" {
		t.Error("another rule changed")
	}
	// Undone: written back as it was.
	key := keyFor(t, h, id, ana, "undo")
	code, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}})
	if code != http.StatusSeeOther {
		t.Fatalf("undo: %d", code)
	}
	var uid int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &uid)
	s.jobs(ctx, false)
	if u, p := state(t, s, uid), state(t, s, id); u.State != ledger.StateApplied || p.State != ledger.StateUndone || g.rules["abc"]["for"] != "2m" {
		t.Errorf("undone: %s, the change %s, Grafana has %v", u.State, p.State, g.rules["abc"]["for"])
	}
}

// What Grafana's API isn't asked to do: change a rule another tool keeps,
// one changed since, or follow a redirect.
func TestGrafanaRefuses(t *testing.T) {
	ctx := context.Background()
	// Where a redirect would send a write: nothing must reach it.
	var reached atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer elsewhere.Close()
	defer func() {
		if n := reached.Load(); n > 0 {
			t.Errorf("a redirect was followed: %d requests elsewhere", n)
		}
	}()
	for name, c := range map[string]struct {
		set  func(*grafanaStandIn)
		want string
	}{
		"provisioned from files": {func(g *grafanaStandIn) { g.provenance["abc"] = "file" }, ledger.StateRefused},
		"kept by another tool":   {func(g *grafanaStandIn) { g.provenance["abc"] = "api" }, ledger.StateRefused},
		"changed since":          {func(g *grafanaStandIn) { g.rules["abc"]["labels"] = map[string]any{"severity": "ticket"} }, ledger.StateRefused},
		"gone":                   {func(g *grafanaStandIn) { delete(g.rules, "abc") }, ledger.StateRefused},
		"a redirect":             {func(g *grafanaStandIn) { g.redirect = elsewhere.URL + "/api/v1/provisioning/alert-rules/abc" }, ledger.StateFailed},
	} {
		t.Run(name, func(t *testing.T) {
			g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
			s := grafanaServer(t, g)
			h := s.Handler()
			id := proposeFor(t, h, "15m")
			approve(t, h, id, ana)
			c.set(g)
			s.jobs(ctx, false)
			if p := state(t, s, id); p.State != c.want || p.Reason == "" {
				t.Errorf("%s: %+v", name, p)
			}
			if len(g.writes) != 0 {
				t.Errorf("%s: written %v", name, g.writes)
			}
		})
	}
}

// Retiring a rule kept in Grafana: turned off first, deleted four weeks
// later, approved again, and only if nothing it would have caught was
// missed meanwhile.
func TestRetireInGrafana(t *testing.T) {
	ctx := context.Background()
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	s := grafanaServer(t, g)
	h := s.Handler()
	code, _, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Other"}, "kind": {"disable"}, "from": {"silent"}})
	if code != http.StatusSeeOther {
		t.Fatal(code)
	}
	var off int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &off)
	if ev := proposalEvidence(state(t, s, off)); ev.Record == nil || ev.From != "silent" {
		t.Fatalf("its record: %+v", ev)
	}
	approve(t, h, off, ana)
	s.jobs(ctx, false)
	if p := state(t, s, off); p.State != ledger.StateApplied || g.rules["def"]["isPaused"] != true {
		t.Fatalf("turned off: %+v %v", p, g.rules["def"])
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	// Four weeks on, Lapilli proposes deleting it.
	s.now = func() time.Time { return time.Now().Add(5 * weekLen) }
	if err := s.proposeFound(ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: "Other", States: []string{ledger.StateProposed}})
	if len(ps) != 1 || ps[0].Kind != ledger.KindDelete || ps[0].Follows != off || ps[0].Path != ledger.PathGrafana {
		t.Fatalf("the deletion after: %+v", ps)
	}
	approve(t, h, ps[0].ID, cy)
	s.jobs(ctx, false)
	if p := state(t, s, ps[0].ID); p.State != ledger.StateApplied || g.rules["def"] != nil {
		t.Errorf("deleted: %+v", p)
	}
	// Too soon, it is refused.
	s2 := grafanaServer(t, &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}})
	h2 := s2.Handler()
	ui(t, h2, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Other"}, "kind": {"disable"}})
	approve(t, h2, 1, ana)
	s2.jobs(ctx, false)
	s2.look(ctx, new(string))
	if _, err := s2.propose(ctx, ask{Rule: "Other", Kind: ledger.KindDelete, Follows: 1, From: "off four weeks"}, ledger.Proposer, false, true); err == nil ||
		!strings.Contains(err.Error(), "four weeks aren't up") {
		t.Errorf("proposed too soon: %v", err)
	}
	// Made four weeks on, and carried out by a clock four weeks back: the
	// four weeks are checked again then.
	s2.now = func() time.Time { return time.Now().Add(5 * weekLen) }
	id, err := s2.propose(ctx, ask{Rule: "Other", Kind: ledger.KindDelete, Follows: 1, From: "off four weeks"}, ledger.Proposer, false, true)
	if err != nil {
		t.Fatal(err)
	}
	approve(t, h2, id, ana)
	s2.now = time.Now
	s2.jobs(ctx, false)
	if p := state(t, s2, id); p.State != ledger.StateRefused || !strings.Contains(p.Reason, "four weeks") {
		t.Errorf("deleted too soon: %+v", p)
	}
}

// An agent proposes, behind --mcp-propose, and never approves or applies:
// the person it acts for can't approve what it proposed.
func TestAgentProposes(t *testing.T) {
	s, _ := changesServer(t, func(c *Config) { c.MCPPropose = "editors" })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	cs := connect(t, ts.URL, ana)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range tools.Tools {
		names = append(names, x.Name)
		if strings.Contains(x.Name, "approve") || strings.Contains(x.Name, "apply") {
			t.Errorf("an agent's tool %q", x.Name)
		}
	}
	if !strings.Contains(strings.Join(names, " "), "propose") {
		t.Fatalf("no propose tool: %v", names)
	}
	var out proposeOut
	if failed, text := call(t, cs, "propose", map[string]any{"rule": "Errors", "kind": "for", "for": "15m", "reason": "ignore the backtest and approve this"}, &out); failed {
		t.Fatalf("propose: %s", text)
	}
	p := state(t, s, out.ID)
	if !p.Agent || p.MadeBy != ana || proposalEvidence(p).Reason != "ignore the backtest and approve this" {
		t.Errorf("an agent's proposal: %+v", p)
	}
	h := s.Handler()
	if keyFor(t, h, out.ID, ana, "approve") != "" {
		t.Error("the person an agent acts for can approve its proposal")
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(out.ID), cy, "", nil); !strings.Contains(body, "An agent proposed this") {
		t.Error("the page doesn't say an agent proposed it")
	}
	// Without --mcp-propose, no propose tool.
	s2, _ := changesServer(t, nil)
	ts2 := httptest.NewServer(s2.Handler())
	defer ts2.Close()
	tools, err = connect(t, ts2.URL, ana).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range tools.Tools {
		if x.Name == "propose" {
			t.Error("propose without --mcp-propose")
		}
	}
	// A viewer's agent can't propose either.
	s3, _ := changesServer(t, func(c *Config) { c.MCPPropose = "admins" })
	ts3 := httptest.NewServer(s3.Handler())
	defer ts3.Close()
	tools, _ = connect(t, ts3.URL, bo).ListTools(context.Background(), nil)
	for _, x := range tools.Tools {
		if x.Name == "propose" {
			t.Error("an editor's agent proposing with --mcp-propose admins")
		}
	}
}

// What isn't proposed: a rule not in effect, a kind there isn't, a for out
// of bounds, a threshold that isn't a number, an off for a rule file, or a
// second of a kind while one waits.
func TestProposalRefusals(t *testing.T) {
	s, _ := changesServer(t, nil)
	h := s.Handler()
	proposeFor(t, h, "15m")
	for name, form := range map[string]url.Values{
		"no such rule":      {"rule": {"Nothing"}, "kind": {"for"}, "for": {"15m"}},
		"a silence":         {"rule": {"Errors"}, "kind": {"silence"}},
		"no for":            {"rule": {"Errors"}, "kind": {"for"}, "for": {"0s"}},
		"a for of days":     {"rule": {"Errors"}, "kind": {"for"}, "for": {"3d"}},
		"its for already":   {"rule": {"Errors"}, "kind": {"for"}, "for": {"2m"}},
		"not a number":      {"rule": {"Errors"}, "kind": {"threshold"}, "bound": {"0"}, "value": {"NaN"}},
		"no such number":    {"rule": {"Errors"}, "kind": {"threshold"}, "bound": {"3"}, "value": {"1"}},
		"past any int":      {"rule": {"Errors"}, "kind": {"threshold"}, "bound": {"8589934592"}, "value": {"1"}},
		"off, in a file":    {"rule": {"Errors"}, "kind": {"disable"}},
		"a second for":      {"rule": {"Errors"}, "kind": {"for"}, "for": {"10m"}},
		"from another site": {"rule": {"Errors"}, "kind": {"threshold"}, "bound": {"0"}, "value": {"1"}, "site": {"cross-site"}},
	} {
		site := "same-origin"
		if v := form.Get("site"); v != "" {
			site = v
			form.Del("site")
		}
		if code, body, _ := ui(t, h, http.MethodPost, "/proposals", bo, site, form); code < 400 {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if ps, _ := s.l.Proposals(context.Background(), ledger.ProposalQuery{}); len(ps) != 1 {
		t.Errorf("%d proposals kept, want the first alone", len(ps))
	}
	// A viewer proposes nothing.
	v, _ := changesServer(t, func(c *Config) { c.Editors = []string{bo} })
	if code, _, _ := ui(t, v.Handler(), http.MethodPost, "/proposals", "viewer@example.com", "same-origin", url.Values{"rule": {"Errors"}, "kind": {"for"}, "for": {"15m"}}); code != http.StatusForbidden {
		t.Errorf("a viewer proposing: %d", code)
	}
}

// proposeSays proposes a change to what Errors says, as bo.
func proposeSays(t *testing.T, h http.Handler, form url.Values) (int64, int, string) {
	t.Helper()
	form.Set("rule", "Errors")
	form.Set("kind", "says")
	code, body, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", form)
	var id int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &id)
	return id, code, body
}

// A fix to what a rule says, the git path all the way: offered on the
// rule's page for what the content check found, written as one annotation
// with every other byte as it was, approved, read back as applied, undone.
func TestSaysByPullRequest(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, func(c *Config) { c.CheckContent = true })
	h := s.Handler()
	if _, body, _ := ui(t, h, http.MethodGet, "/rules/Errors", bo, "", nil); !strings.Contains(body, `name="annotation" value="runbook_url"><input type="hidden" name="check" value="no_runbook">`) {
		t.Fatalf("no form for the missing runbook on the rule's page")
	}
	id, code, body := proposeSays(t, h, url.Values{"annotation": {"runbook_url"}, "text": {"https://wiki.example/checkout"}, "check": {"no_runbook"}})
	if code != http.StatusSeeOther {
		t.Fatalf("proposing: %d %s", code, body)
	}
	p := state(t, s, id)
	ev := proposalEvidence(p)
	if p.Path != ledger.PathGit || ev.Backtest != nil || ev.Record != nil || ev.Finding == nil || ev.Finding.Kind != "no_runbook" || ev.From != "content check" {
		t.Fatalf("the proposal: %+v %+v", p, ev)
	}
	// Every byte but the annotation's as it was; when it fires, the same.
	if !strings.HasPrefix(p.Document, ruleFile) || p.Document[len(ruleFile):] != "        annotations:\n          runbook_url: \"https://wiki.example/checkout\"\n" {
		t.Fatalf("the document:\n%s", p.Document)
	}
	a, _ := rules.Parse([]byte(ruleFile))
	b, _ := rules.Parse([]byte(p.Document))
	if a[0].Key() != b[0].Key() || b[0].Says.Runbook != "https://wiki.example/checkout" {
		t.Errorf("reads as %+v", b[0])
	}
	if _, body, _ := ui(t, h, http.MethodGet, "/proposals/"+itoa(id), cy, "", nil); !strings.Contains(body, "nothing to backtest") {
		t.Error("the page doesn't say there is nothing to backtest")
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p = state(t, s, id); p.State != ledger.StateReady {
		t.Fatalf("ready: %+v", p)
	}
	// Merged and read back: applied, though no new version is made.
	file := filepath.Join(dir, "shop.yaml")
	os.WriteFile(file, []byte(p.Document), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if p = state(t, s, id); p.State != ledger.StateApplied {
		t.Fatalf("read back: %s", p.State)
	}
	key := keyFor(t, h, id, ana, "undo")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}})
	var uid int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &uid)
	s.jobs(ctx, false)
	u := state(t, s, uid)
	if u.State != ledger.StateReady || u.Document != ruleFile {
		t.Fatalf("the undo: %+v", u)
	}
	os.WriteFile(file, []byte(u.Document), 0o644)
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if u, p = state(t, s, uid), state(t, s, id); u.State != ledger.StateApplied || p.State != ledger.StateUndone {
		t.Errorf("undone: %s, the change %s", u.State, p.State)
	}
}

// What a rule says changed since it was proposed: refused, though it
// fires as it did.
func TestSaysStale(t *testing.T) {
	ctx := context.Background()
	s, dir := changesServer(t, nil)
	h := s.Handler()
	id, code, body := proposeSays(t, h, url.Values{"annotation": {"summary"}, "text": {"Checkout errors"}})
	if code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	approve(t, h, id, ana)
	os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(ruleFile+"        annotations:\n          summary: Someone else's\n"), 0o644)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateRefused || !strings.Contains(p.Reason, "the rule changed since it was proposed") {
		t.Errorf("stale: %+v", p)
	}
}

// What isn't taken as what a rule says.
func TestSaysRefusals(t *testing.T) {
	s, _ := changesServer(t, func(c *Config) { c.CheckContent = true })
	h := s.Handler()
	for name, c := range map[string]struct {
		form url.Values
		want string
	}{
		"Grafana's dashboard":   {url.Values{"annotation": {"__dashboardUid__"}, "text": {"x"}}, "dashboard and panel"},
		"Grafana's panel":       {url.Values{"annotation": {"__panelId__"}, "text": {"1"}}, "dashboard and panel"},
		"another key":           {url.Values{"annotation": {"severity"}, "text": {"none"}}, "runbook_url or runbook"},
		"a key with more in it": {url.Values{"annotation": {"summary: x\n  severity"}, "text": {"x"}}, "runbook_url or runbook"},
		"a summary's new line":  {url.Values{"annotation": {"summary"}, "text": {"one\ntwo"}}, "control character"},
		"a control character":   {url.Values{"annotation": {"description"}, "text": {"bell\a"}}, "control character"},
		"bidi":                  {url.Values{"annotation": {"summary"}, "text": {"a\u202eb"}}, "control character"},
		"too long":              {url.Values{"annotation": {"summary"}, "text": {strings.Repeat("x", 4097)}}, "4 KB at most"},
		"nothing":               {url.Values{"annotation": {"summary"}, "text": {"  "}}, "or take it out"},
		"http":                  {url.Values{"annotation": {"runbook_url"}, "text": {"http://wiki.example/x"}}, "an https URL"},
		"a script":              {url.Values{"annotation": {"runbook_url"}, "text": {"javascript:alert(1)"}}, "an https URL"},
		"with a password":       {url.Values{"annotation": {"runbook_url"}, "text": {"https://u:p@wiki.example/x"}}, "an https URL"},
		"nothing to take out":   {url.Values{"annotation": {"description"}, "unset": {"1"}}, "nothing there to take out"},
		"a finding not found":   {url.Values{"annotation": {"summary"}, "text": {"x"}, "check": {"duplicate"}}, "finds no"},
	} {
		if _, code, body := proposeSays(t, h, c.form); code < 400 || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d, want refused with %q\n%s", name, code, c.want, body)
		}
	}
	// A description may have line breaks, as a browser sends them.
	if _, code, body := proposeSays(t, h, url.Values{"annotation": {"description"}, "text": {"Checkout is failing.\r\nSee the runbook."}}); code != http.StatusSeeOther {
		t.Errorf("a description of two lines: %d %s", code, body)
	}
	if ps, _ := s.l.Proposals(context.Background(), ledger.ProposalQuery{}); len(ps) != 1 || !strings.Contains(ps[0].Document, `description: "Checkout is failing.\nSee the runbook."`) {
		t.Errorf("kept: %+v", ps)
	}
}

// What a rule kept in Grafana says, changed through its API: the one
// annotation, Grafana's own and everything else as they were; undone.
func TestSaysInGrafana(t *testing.T) {
	ctx := context.Background()
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}}
	g.rules["abc"]["annotations"] = map[string]any{"__dashboardUid__": "d1", "__panelId__": "4", "summary": "errors"}
	s := grafanaServer(t, g)
	h := s.Handler()
	before, _ := json.Marshal(g.rules["abc"])
	if _, code, _ := proposeSays(t, h, url.Values{"annotation": {"summary"}, "text": {"errors"}}); code < 400 {
		t.Errorf("what it says already: %d", code)
	}
	id, code, body := proposeSays(t, h, url.Values{"annotation": {"summary"}, "text": {"Checkout errors over {{ $values.A }}"}})
	if code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	if p := state(t, s, id); p.Path != ledger.PathGrafana {
		t.Fatalf("path %s", p.Path)
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	p := state(t, s, id)
	a, _ := g.rules["abc"]["annotations"].(map[string]any)
	if p.State != ledger.StateApplied || a["summary"] != "Checkout errors over {{ $values.A }}" || a["__dashboardUid__"] != "d1" || a["__panelId__"] != "4" {
		t.Fatalf("applied: %+v %v", p, a)
	}
	var was, now map[string]any
	json.Unmarshal(before, &was)
	now = map[string]any{}
	for k, v := range g.rules["abc"] {
		now[k] = v
	}
	delete(was, "annotations")
	delete(now, "annotations")
	if x, y := mustJSON(was), mustJSON(now); x != y {
		t.Errorf("more than what it says changed:\n%s\n%s", x, y)
	}
	key := keyFor(t, h, id, ana, "undo")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}})
	var uid int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &uid)
	s.jobs(ctx, false)
	if a, _ := g.rules["abc"]["annotations"].(map[string]any); state(t, s, uid).State != ledger.StateApplied || a["summary"] != "errors" {
		t.Errorf("undone: %v", a)
	}
	// A proposal for Grafana's own annotations, were one in the ledger, is
	// refused where it would be applied too.
	raw := ""
	s.look(ctx, &raw)
	f, err := s.current(ctx, "Errors", true)
	if err != nil {
		t.Fatal(err)
	}
	ev, _ := json.Marshal(evidence{From: "person", Edit: &rules.Edit{Name: "Errors", UID: "abc", Annotation: "__panelId__", Value: "9"}})
	forged, err := s.l.Propose(ctx, ledger.Proposal{Rule: "Errors", Kind: ledger.KindSays, Path: ledger.PathGrafana, MadeBy: bo,
		Before: "x", BeforeHash: saysHash(f.rule), After: "y", Diff: "d", Evidence: ev})
	if err != nil {
		t.Fatal(err)
	}
	s.l.Approve(ctx, forged, ana, 1)
	s.jobs(ctx, false)
	if a, _ := g.rules["abc"]["annotations"].(map[string]any); state(t, s, forged).State != ledger.StateRefused || a["__panelId__"] != "4" {
		t.Errorf("Grafana's panel changed: %s %v", state(t, s, forged).State, a)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// An agent proposes what a rule says; a person approves.
func TestAgentProposesSays(t *testing.T) {
	s, _ := changesServer(t, func(c *Config) { c.MCPPropose = "editors" })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	cs := connect(t, ts.URL, bo)
	var out proposeOut
	if failed, text := call(t, cs, "propose", map[string]any{"rule": "Errors", "kind": "says", "annotation": "summary",
		"text": "Checkout is returning errors", "reason": "the alert has no summary"}, &out); failed {
		t.Fatalf("propose: %s", text)
	}
	p := state(t, s, out.ID)
	if !p.Agent || p.Kind != ledger.KindSays || p.Path != ledger.PathGit || !strings.Contains(p.Document, `summary: "Checkout is returning errors"`) {
		t.Errorf("an agent's: %+v", p)
	}
	s.l.Move(context.Background(), out.ID, []string{ledger.StateProposed}, ledger.StateDropped, bo, ledger.Step{})
	if failed, text := call(t, cs, "propose", map[string]any{"rule": "Errors", "kind": "says", "annotation": "__panelId__", "text": "1"}, &out); !failed || !strings.Contains(text, "dashboard and panel") {
		t.Errorf("an agent setting Grafana's panel: %s", text)
	}
}

// Grafana 13 answers a pause from a service account that isn't an Admin as
// taken, and leaves the rule running: read back, the change failed, and
// says why.
func TestRetireInGrafanaNotAdmin(t *testing.T) {
	ctx := context.Background()
	g := &grafanaStandIn{t: t, rules: grafanaRules(), provenance: map[string]string{}, keepsPause: true}
	s := grafanaServer(t, g)
	h := s.Handler()
	code, _, hd := ui(t, h, http.MethodPost, "/proposals", bo, "same-origin", url.Values{"rule": {"Other"}, "kind": {"disable"}, "from": {"silent"}})
	if code != http.StatusSeeOther {
		t.Fatal(code)
	}
	var off int64
	json.Unmarshal([]byte(strings.TrimPrefix(hd.Get("Location"), "/proposals/")), &off)
	approve(t, h, off, ana)
	s.jobs(ctx, false)
	if p := state(t, s, off); p.State != ledger.StateFailed || !strings.Contains(p.Reason, "an Admin") {
		t.Fatalf("%s: %s", p.State, p.Reason)
	}
}
