package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
)

// The Orphans page lists the names no rule in effect has that the ledger
// keeps something of, a rule renamed (Errors, now ErrorsRate) among them,
// and not the rules in effect; an editor moves what one kept to a rule in
// effect, or deletes it, each noted; a viewer sees the actions off, with
// why. A name in effect isn't moved, nor to a name none has.
func TestOrphans(t *testing.T) {
	ctx := context.Background()
	s, _, _ := rolesServer(t, nil)
	// What people kept of Errors and of Gone, while they were in effect.
	for _, name := range []string{"Errors", "Gone"} {
		if err := s.l.PutNote(ctx, ledger.Note{Rule: name, Status: "tune", Note: "noisy at night"}, "ed"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.l.PutTask(ctx, ledger.Task{Title: "tune " + name, Rule: name}, "ed"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.l.PutComment(ctx, ledger.Comment{On: "rule", Key: "Errors", Body: "seen again"}, "ed"); err != nil {
		t.Fatal(err)
	}
	pid, err := s.l.Propose(ctx, ledger.Proposal{Rule: "Errors", Kind: ledger.KindFor, Path: ledger.PathNone, MadeBy: "ed",
		Before: "for: 2m\n", After: "for: 10m\n", Diff: "-for: 2m\n+for: 10m\n", BeforeHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// Renamed: ErrorsRate in effect, Errors no more.
	os.WriteFile(filepath.Join(s.cfg.RulesPath, "shop.yaml"), []byte(strings.Replace(ruleFile, "alert: Errors", "alert: ErrorsRate", 1)), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "ErrorsRate", Note: "the rule as it is"}, "ed"); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	code, body := getAs(t, h, "/orphans", "ed@example.com")
	if code != http.StatusOK || !strings.Contains(body, "<bdi>Errors</bdi>") || !strings.Contains(body, "<bdi>Gone</bdi>") ||
		strings.Contains(body, "<bdi>ErrorsRate</bdi>") || !strings.Contains(body, fmt.Sprintf(`/proposals/%d"`, pid)) || !strings.Contains(body, `<option value="ErrorsRate">`) {
		t.Fatalf("/orphans: %d\n%s", code, body)
	}
	if !strings.Contains(body, "1 to 2 of 2") || !strings.Contains(body, "--rules-select and --rule narrow it") {
		t.Errorf("the count, and what it is compared with: %s", body)
	}
	_, body = getAs(t, h, "/orphans", "viewer@example.com")
	if !strings.Contains(body, `id="orphan-why"`) || strings.Count(body, `aria-describedby="orphan-why"`) != 4 || strings.Contains(body, `action="/orphans/move"`) {
		t.Errorf("a viewer's actions aren't off with why:\n%s", body)
	}
	post := func(path string, v url.Values) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("X-Forwarded-Email", "ed@example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	long := strings.Repeat("x", 900<<10)
	for _, c := range []struct {
		path string
		v    url.Values
		code int
	}{
		{"/orphans/move", url.Values{"from": {"Errors"}, "to": {"Nope"}}, http.StatusBadRequest},
		{"/orphans/move", url.Values{"from": {"ErrorsRate"}, "to": {"ErrorsRate"}}, http.StatusBadRequest},
		{"/orphans/move", url.Values{"from": {"Errors"}}, http.StatusBadRequest},
		{"/orphans/delete", url.Values{"rule": {"ErrorsRate"}}, http.StatusBadRequest},
		// A name a note can't be on, or nothing is kept of.
		{"/orphans/delete", url.Values{"rule": {long}}, http.StatusBadRequest},
		{"/orphans/move", url.Values{"from": {long}, "to": {"ErrorsRate"}}, http.StatusBadRequest},
		{"/orphans/delete", url.Values{"rule": {"a\nb"}}, http.StatusBadRequest},
		{"/orphans/delete", url.Values{"rule": {"NothingKept"}}, http.StatusNotFound},
		{"/orphans/move", url.Values{"from": {"NothingKept"}, "to": {"ErrorsRate"}}, http.StatusNotFound},
	} {
		if code := post(c.path, c.v); code != c.code {
			t.Errorf("%s %.40v: %d, want %d", c.path, c.v, code, c.code)
		}
	}
	if changes, _ := s.l.Changes(ctx, 100); slices.ContainsFunc(changes, func(c ledger.Change) bool { return len(c.What) > 1000 || strings.Contains(c.What, "NothingKept") }) {
		t.Error("the activity log holds a refused name")
	}
	if code := post("/orphans/move", url.Values{"from": {"Errors"}, "to": {"ErrorsRate"}}); code != http.StatusSeeOther {
		t.Fatalf("moving: %d", code)
	}
	if code := post("/orphans/delete", url.Values{"rule": {"Gone"}}); code != http.StatusSeeOther {
		t.Fatalf("deleting: %d", code)
	}
	_, body = getAs(t, h, "/orphans", "ed@example.com")
	if strings.Contains(body, "<bdi>Gone</bdi>") || !strings.Contains(body, "<bdi>Errors</bdi>") || !strings.Contains(body, "Proposals alone") {
		t.Errorf("after moving and deleting, the proposal's name alone:\n%s", body)
	}
	notes, _ := s.l.Notes(ctx)
	if n := notes["ErrorsRate"]; n.Note != "the rule as it is" || n.Status != "tune" {
		t.Errorf("ErrorsRate's note after moving: %+v", n)
	}
	ts, _ := s.l.Tasks(ctx, ledger.TaskQuery{Rule: "ErrorsRate"})
	cs, _ := s.l.Comments(ctx, "rule", "ErrorsRate")
	if len(ts) != 1 || len(cs) != 1 {
		t.Errorf("ErrorsRate's tasks and comments after moving: %+v %+v", ts, cs)
	}
	if p, _ := s.l.ProposalByID(ctx, pid); p.Rule != "Errors" || p.State != ledger.StateProposed {
		t.Errorf("the proposal after moving: %+v", p)
	}
	changes, _ := s.l.Changes(ctx, 50)
	var said []string
	for _, c := range changes {
		said = append(said, c.By+": "+c.What)
	}
	for _, want := range []string{"ed@example.com: what rule Errors kept moved to ErrorsRate", "ed@example.com: what rule Gone kept deleted"} {
		if !strings.Contains(strings.Join(said, "\n"), want) {
			t.Errorf("the activity log hasn't %q:\n%s", want, strings.Join(said, "\n"))
		}
	}
}

// With 4,000 rules in effect and 3,000 names gone, the page reads the
// names one at a time: it shows the first hundred, counts them all, pages
// to the next, and allocates a bounded amount however many there are.
func TestOrphansScale(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	ctx := context.Background()
	s, rules := server(t, nil)
	var b strings.Builder
	b.WriteString("groups:\n  - name: many\n    rules:\n")
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&b, "      - {alert: Rule%04d, expr: 'up == %d'}\n", i, i)
	}
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(b.String()), 0o644)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		if err := s.l.PutNote(ctx, ledger.Note{Rule: fmt.Sprintf("Gone%04d", i), Note: "n"}, "ana"); err != nil {
			t.Fatal(err)
		}
	}
	// And a note of a rule in effect, which isn't listed.
	if err := s.l.PutNote(ctx, ledger.Note{Rule: "Rule0001", Note: "n"}, "ana"); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	code, body, _ := get(t, h, "/orphans")
	runtime.ReadMemStats(&m1)
	alloc := m1.TotalAlloc - m0.TotalAlloc
	t.Logf("/orphans allocated %d MB", alloc>>20)
	if code != http.StatusOK || !strings.Contains(body, "1 to 100 of 3000") || strings.Count(body, "<tr>") != 101 || strings.Contains(body, "<bdi>Rule0001</bdi>") {
		t.Fatalf("/orphans: %d %.2000s", code, body)
	}
	// 4,000 names to move to, past datalistMost: typed, not listed.
	if strings.Contains(body, "<datalist") {
		t.Errorf("a datalist of 4,000 rules")
	}
	const budget = 64 << 20
	if alloc > budget {
		t.Errorf("/orphans allocated %d MB, over %d", alloc>>20, budget>>20)
	}
	_, body, _ = get(t, h, "/orphans?after=Gone0099")
	if !strings.Contains(body, "101 to 200 of 3000") || !strings.Contains(body, "<bdi>Gone0100</bdi>") {
		t.Errorf("the next hundred: %.2000s", body)
	}
}
