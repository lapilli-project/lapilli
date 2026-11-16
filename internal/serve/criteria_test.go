package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// criteriaServer is a server whose runs read the impact signal ("impact")
// over the three days before now, with ana an admin and bo an editor; no
// incidents file, unless incidents.
func criteriaServer(t *testing.T, incidents bool, change func(*Config)) *Server {
	t.Helper()
	s, _ := server(t, func(c *Config) {
		c.UserHeader, c.Admins, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}, []string{"bo@example.com"}
		if !incidents {
			c.Incidents = ""
		}
		if change != nil {
			change(c)
		}
	})
	ctx := context.Background()
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	s.ready.Store(true)
	return s
}

// askAs asks for a page as who, from a browser on site (Sec-Fetch-Site).
func askAs(t *testing.T, h http.Handler, method, path string, form url.Values, who, site string) (int, string) {
	t.Helper()
	var r *http.Request
	if method == http.MethodPost {
		r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Sec-Fetch-Site", site)
	r.Header.Set("X-Forwarded-Email", who)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// The incident criteria an admin saves are checked first: what doesn't
// read as PromQL, a for out of bounds and what Prometheus's parser refuses
// are refused, and nothing is saved; an editor may not; empty turns the
// command line's off; back to the start is the command line's again.
func TestImpactSaveChecked(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	h := s.Handler()
	for _, c := range []struct {
		name string
		form url.Values
		who  string
		code int
		want string
	}{
		{"an editor", url.Values{"impact": {"impact > 1"}}, "bo@example.com", http.StatusForbidden, ""},
		{"not PromQL", url.Values{"impact": {"sum(rate(x[5m])"}}, "ana@example.com", 400, "doesn&#39;t read as PromQL"},
		{"too short a for", url.Values{"impact": {"impact > 1"}, "impact-for": {"10s"}}, "ana@example.com", 400, "want from 1m to 24h"},
		{"too long a for", url.Values{"impact": {"impact > 1"}, "impact-for": {"2d"}}, "ana@example.com", 400, "want from 1m to 24h"},
		{"Prometheus refuses it", url.Values{"impact": {"nope(up)"}}, "ana@example.com", 400, "unknown function"},
	} {
		code, body := askAs(t, h, http.MethodPost, "/settings/impact", c.form, c.who, "same-origin")
		if code != c.code || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d\n%s", c.name, code, body)
		}
	}
	if set, err := s.l.Settings(ctx); err != nil || len(set) != 0 {
		t.Fatalf("saved: %v %v", set, err)
	}
	if code, body := askAs(t, h, http.MethodPost, "/settings/impact", url.Values{"impact": {"impact > 1"}, "impact-for": {"10m"}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("saved: %d %s", code, body)
	}
	if c, err := s.criterion(ctx); err != nil || c != (criterion{"impact > 1", 10 * time.Minute}) {
		t.Errorf("in effect: %+v %v", c, err)
	}
	if code, _ := askAs(t, h, http.MethodPost, "/settings/impact", url.Values{"impact": {""}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	if set, _ := s.l.Settings(ctx); set["impact"].Value != impactOff {
		t.Errorf("emptied: %+v", set)
	}
	if c, _ := s.criterion(ctx); c.Expr != "" || s.running(ctx) != true {
		t.Errorf("off: %+v", c) // rules still to replay
	}
	if code, _ := askAs(t, h, http.MethodPost, "/settings/impact", url.Values{"reset-impact": {"1"}, "reset-impact-for": {"1"}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	if c, _ := s.criterion(ctx); c != (criterion{s.cfg.Impact, s.cfg.ImpactFor}) {
		t.Errorf("back to the start: %+v", c)
	}
	_, body, _ := get(t, h, "/settings.yaml")
	if strings.Contains(body, "impact") {
		t.Errorf("settings.yaml with the command line's:\n%s", body)
	}
}

// Saved, a new criterion leaves the record as the ledger holds it, by the
// one before, until it is read again: the replica that leads reads it over
// the weeks the runs replayed (the last four at most), once, its windows in
// place of the one before's, and from then on the record is kept by it;
// settings.yaml gives it as Helm values.
func TestImpactSavedReadAgain(t *testing.T) {
	ctx := context.Background()
	// Read by the runs, the one before finds no window.
	s := criteriaServer(t, false, func(c *Config) { c.Impact = "impact > 9" })
	h := s.Handler()
	began, err := s.l.RunsFrom(ctx)
	if err != nil || began.IsZero() {
		t.Fatal(began, err)
	}
	kept := func() (verdict.Options, bool) {
		t.Helper()
		o, err := s.options(ctx)
		if err != nil {
			t.Fatal(err)
		}
		k, err := s.l.RecordKept(ctx, now.Add(-24*time.Hour), now, o)
		if err != nil {
			t.Fatal(err)
		}
		return o, k
	}
	if o, k := kept(); !o.ImpactFrom.Equal(began.Add(-time.Hour)) || !k {
		t.Fatalf("read by the runs from %s: %s %v", began, o.ImpactFrom, k)
	}
	if code, body := askAs(t, h, http.MethodPost, "/settings/impact", url.Values{"impact": {"impact > 1"}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	if o, k := kept(); !o.ImpactFrom.Equal(began.Add(-time.Hour)) || !k {
		t.Errorf("saved, not read yet: kept from %s: %v", o.ImpactFrom, k)
	}
	s.jobs(ctx, false)
	if o, k := kept(); !o.ImpactFrom.Equal(began) || !k {
		t.Errorf("read again from %s: %s %v", began, o.ImpactFrom, k)
	}
	ws, err := s.l.ImpactWindows(ctx, began, now)
	if err != nil || len(ws) != 3 {
		t.Errorf("windows: %+v %v", ws, err)
	}
	// Checked once as it was saved, read again once: not again at the
	// next round.
	s.jobs(ctx, false)
	if n := s.h.(*history).exprs["impact > 1"]; n != 2 {
		t.Errorf("read %d times", n)
	}
	_, body, _ := get(t, h, "/settings.yaml")
	if !strings.Contains(body, `impact: "impact > 1"`) {
		t.Errorf("settings.yaml:\n%s", body)
	}
}

// A criterion that fails to be read again is tried again later, not each
// round; meanwhile the runs read it from where they are, which keeps the
// record from their own start alone.
func TestImpactReadAgainFails(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	if err := s.l.SetSettings(ctx, map[string]string{"impact": "turned"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, false)
	k, err := s.l.ImpactKept(ctx)
	if err != nil || k.AgainDone("5m turned") {
		t.Fatalf("%+v %v", k, err)
	}
	seen := s.sourcesNow()
	if seen.Again == nil || seen.Again.Tries != 1 || !seen.Again.Next.Equal(now.Add(time.Hour)) || !strings.Contains(seen.Again.Err, "slow down") {
		t.Errorf("failed: %+v", seen.Again)
	}
	asked := s.h.(*history).asked
	s.jobs(ctx, false)
	if s.h.(*history).asked != asked {
		t.Error("tried again before its time")
	}
	_, body, _ := get(t, s.Handler(), "/settings")
	if !strings.Contains(body, "Reading it again over the last four weeks failed") {
		t.Errorf("the page doesn't say it failed:\n%s", body)
	}
}

// A try reads the criterion typed over the last four weeks, an admin's,
// from this site, and saves nothing: the windows it finds, which would
// count, and the verdict in effect and with them.
func TestImpactTry(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, true, nil)
	h := s.Handler()
	path := "/settings?impact-try=1&impact=impact+%3E+1&impact-for=5m"
	if code, _ := askAs(t, h, http.MethodGet, path, nil, "bo@example.com", "same-origin"); code != http.StatusForbidden {
		t.Errorf("an editor: %d", code)
	}
	if code, _ := askAs(t, h, http.MethodGet, path, nil, "ana@example.com", "cross-site"); code != 400 {
		t.Errorf("from another site: %d", code)
	}
	asked, counted := s.h.(*history).asked, s.h.(*history).exprs["count(impact)"]
	code, body := askAs(t, h, http.MethodGet, path, nil, "ana@example.com", "same-origin")
	// The expression, and where its one selector's data begins.
	if code != 200 || s.h.(*history).asked != asked+2 || s.h.(*history).exprs["count(impact)"] != counted+1 {
		t.Fatalf("%d, %d reads\n%s", code, s.h.(*history).asked-asked, body)
	}
	for _, want := range []string{"3 windows found; 2 of them would count", "no: written down as INC-1", "service=cart",
		`<th scope="row">Incidents judged against</th><td>2</td><td><strong>3</strong></td>`, "Nothing was saved."} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q:\n%s", want, body)
		}
	}
	if set, err := s.l.Settings(ctx); err != nil || len(set) != 0 {
		t.Errorf("saved: %v %v", set, err)
	}
	// What doesn't read is said, with the form as typed, and not read.
	asked = s.h.(*history).asked
	_, body = askAs(t, h, http.MethodGet, "/settings?impact-try=1&impact=sum(&impact-for=5m", nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, "doesn&#39;t read as PromQL") || !strings.Contains(body, ">sum(</textarea>") || s.h.(*history).asked != asked {
		t.Errorf("not PromQL:\n%s", body)
	}
}

// Tries are counted, impactTriesAnHour an admin an hour, made or refused
// once checked: past them, refused before anything is read.
func TestImpactTriesLimited(t *testing.T) {
	s := criteriaServer(t, false, nil)
	h := s.Handler()
	for i := range impactTriesAnHour + 1 {
		// Each its own, so that none is the page kept for another.
		path := fmt.Sprintf("/settings?impact-try=1&impact-for=5m&impact=impact+%%3E+%d", i+10)
		code, body := askAs(t, h, http.MethodGet, path, nil, "ana@example.com", "same-origin")
		switch {
		case i < impactTriesAnHour && code != 200:
			t.Fatalf("try %d: %d %s", i, code, body)
		case i == impactTriesAnHour && (code != 400 || !strings.Contains(body, "as many incident criteria")):
			t.Errorf("past the hour's: %d", code)
		}
	}
	if n := s.h.(*history).exprs[fmt.Sprintf("impact > %d", impactTriesAnHour+10)]; n != 0 {
		t.Errorf("read past the hour's: %d", n)
	}
}

// A proposal waiting for approval whose backtest had no record is held
// once a criterion saved is read over its weeks: they have a record now,
// and it is backtested again before it is approved. The try before said
// so.
func TestImpactStalesProposals(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, func(c *Config) { c.Impact = "" })
	yes := true
	five := 5 * time.Minute
	ev, _ := json.Marshal(evidence{From: "person", Edit: &rules.Edit{Name: "Errors", For: &five}, Semantics: semantics, Version: "test",
		Backtest: &backtest{From: now.Add(-12 * weekLen), To: now, Before: side{Firings: 21}, After: side{Firings: 2}, NoRecord: &yes}})
	id, err := s.l.Propose(ctx, ledger.Proposal{Rule: "Errors", Kind: ledger.KindFor, Path: ledger.PathGit, MadeBy: "bo",
		Before: "Errors ()\nfor: 2m\n", After: "Errors ()\nfor: 5m\n", Evidence: ev})
	if err != nil {
		t.Fatal(err)
	}
	stale := func() *staleWhy {
		t.Helper()
		ps, err := s.l.Proposals(ctx, ledger.ProposalQuery{States: []string{ledger.StateProposed}})
		if err != nil || len(ps) != 1 {
			t.Fatal(ps, err)
		}
		ls, err := s.linesNow(ctx, ps)
		if err != nil {
			t.Fatal(err)
		}
		return ls[0].StaleWhy
	}
	if w := stale(); w != nil {
		t.Fatalf("stale before: %+v", w)
	}
	_, body := askAs(t, s.Handler(), http.MethodGet, "/settings?impact-try=1&impact=impact+%3E+1", nil, "ana@example.com", "same-origin")
	if !strings.Contains(body, "1 waiting proposals would need a new backtest") || !strings.Contains(body, fmt.Sprintf("/proposals/%d", id)) {
		t.Errorf("the try doesn't say it:\n%s", body)
	}
	if code, body := askAs(t, s.Handler(), http.MethodPost, "/settings/impact", url.Values{"impact": {"impact > 1"}}, "ana@example.com", "same-origin"); code != http.StatusSeeOther {
		t.Fatalf("%d %s", code, body)
	}
	s.jobs(ctx, false)
	if w := stale(); w == nil || w.Format != staleRecorded {
		t.Errorf("after: %+v", w)
	}
}

// Saved in the UI with no rules and no --impact, the impact signal is a
// run to make: the runs begin.
func TestImpactRuns(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.RulesPath, c.Impact = "", "" })
	if s.running(ctx) {
		t.Fatal("running with nothing to read")
	}
	if err := s.l.SetSettings(ctx, map[string]string{"impact": "impact"}, "ana"); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if c, err := s.l.Cursor(ctx); err != nil || c.IsZero() {
		t.Errorf("no run: %s %v", c, err)
	}
	if k, err := s.l.ImpactKept(ctx); err != nil || k.Since.IsZero() || k.Criterion != "5m impact" {
		t.Errorf("%+v %v", k, err)
	}
}

// The judging settings and the incident criteria are kept apart: an
// incident criterion saved is no judging setting the UI couldn't parse.
func TestImpactNotAJudgingSetting(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, false, nil)
	if err := s.l.SetSettings(ctx, map[string]string{"impact": "impact > 1", "impact-for": "10m"}, "ana"); err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	if _, err := s.options(ctx); err != nil || strings.Contains(logged.String(), "doesn't parse") {
		t.Errorf("%v %s", err, logged.String())
	}
}
