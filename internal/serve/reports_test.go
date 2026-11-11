package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// receiver takes webhooks, by path; those in down fail.
type receiver struct {
	sync.Mutex
	got  map[string][]string
	down map[string]bool
	srv  *httptest.Server
	dir  string
}

func newReceiver(t *testing.T) *receiver {
	rv := &receiver{got: map[string][]string{}, down: map[string]bool{}, dir: t.TempDir()}
	rv.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rv.Lock()
		defer rv.Unlock()
		if rv.down[r.URL.Path] {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		rv.got[r.URL.Path] = append(rv.got[r.URL.Path], string(b))
	}))
	t.Cleanup(rv.srv.Close)
	return rv
}

// file is a file holding the URL of path.
func (rv *receiver) file(path string) string {
	f := filepath.Join(rv.dir, strings.Trim(path, "/"))
	os.WriteFile(f, []byte(rv.srv.URL+path), 0o600)
	return f
}

// texts are the texts posted to path, Slack-style.
func (rv *receiver) texts(path string) []string {
	rv.Lock()
	defer rv.Unlock()
	var out []string
	for _, b := range rv.got[path] {
		var m struct{ Text string }
		json.Unmarshal([]byte(b), &m)
		out = append(out, m.Text)
	}
	return out
}

// events are the webhook's notices of an event.
func (rv *receiver) events(event string) []map[string]any {
	rv.Lock()
	defer rv.Unlock()
	var out []map[string]any
	for _, b := range rv.got["/hook"] {
		var m map[string]any
		json.Unmarshal([]byte(b), &m)
		if m["event"] == event {
			out = append(out, m)
		}
	}
	return out
}

// at sets the server's clock and runs its jobs (and runs, with run), then
// sends what the outbox holds.
func at(t *testing.T, s *Server, when time.Time, run bool) {
	t.Helper()
	ctx := context.Background()
	s.now = func() time.Time { return when }
	s.jobs(ctx, run)
	for _, d := range s.sendsTo() {
		if err := s.sendDue(ctx, d, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
}

func reportServer(t *testing.T, rv *receiver, change func(*Config)) *Server {
	t.Helper()
	s, _ := server(t, func(c *Config) {
		c.ExternalURL = "https://lapilli.example.com"
		c.NotifyWebhook, c.Slack = rv.file("/hook"), rv.file("/slack")
		c.TeamDigests = map[string]string{"shop": rv.file("/shop")}
		c.ReportDests = map[string]string{"ops": rv.file("/ops"), "broken": rv.file("/broken")}
		if change != nil {
			change(c)
		}
	})
	raw := ""
	if err := s.look(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(context.Background(), true)
	return s
}

// A report falls due by its schedule and goes once for its span to each
// destination it names, by name: its text Slack-style, report.ready to the
// webhook; one destination down holds up none of the others, and is tried
// again; the next span is sent at the next time it falls due.
func TestReportsSent(t *testing.T) {
	rv := newReceiver(t)
	s := reportServer(t, rv, nil)
	h := s.Handler()
	if code, body := api(t, h, "POST", "/api/v1/teams", `{"name": "shop", "rules": "^Errors$"}`); code != http.StatusCreated {
		t.Fatalf("team: %d %s", code, body)
	}
	code, body := api(t, h, "POST", "/api/v1/reports", `{"name": "shop daily", "team_id": 1, "schedule": "daily@09:00",
		"sections": ["woken", "summary", "noise", "missed", "tasks"], "dests": ["broken", "ops", "webhook", "slack", "team:shop"]}`)
	if code != http.StatusCreated {
		t.Fatalf("a report: %d %s", code, body)
	}
	var made reportLine
	json.Unmarshal([]byte(body), &made)
	if made.ID != 1 || made.TeamName != "shop" || !made.Enabled || made.Sections[0] != "summary" || made.Updated != now {
		t.Errorf("made: %s", body)
	}
	if code, body := api(t, h, "POST", "/api/v1/reports", `{"name": "everyone", "schedule": "daily@09:00", "sections": ["summary", "missed"], "dests": ["ops"]}`); code != http.StatusCreated {
		t.Fatalf("a report: %d %s", code, body)
	}
	rv.down["/broken"] = true
	// Made after today's: nothing until tomorrow's.
	at(t, s, now, false)
	if n := len(rv.texts("/ops")); n != 0 {
		t.Fatalf("sent for a time it fell due before it was made: %d", n)
	}
	day := time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)
	at(t, s, day, true)
	ops := rv.texts("/ops")
	if len(ops) != 2 {
		t.Fatalf("ops: %d %v", len(ops), ops)
	}
	var shop, all string
	for _, x := range ops {
		if strings.Contains(x, "shop daily") {
			shop = x
		} else {
			all = x
		}
	}
	if !strings.HasPrefix(shop, "```\nshop daily, for shop: 2026-11-03 00:00 → 2026-11-04 00:00 (UTC)") || !strings.Contains(shop, "Firings: ") ||
		!strings.Contains(shop, "Tasks: ") || strings.Index(shop, "Firings: ") > strings.Index(shop, "Tasks: ") {
		t.Errorf("shop's report, its sections in order: %s", shop)
	}
	if !strings.Contains(all, "everyone: 2026-11-03 00:00 → 2026-11-04 00:00") || !strings.Contains(all, "no alert caught 1") || !strings.Contains(all, "impact") {
		t.Errorf("everyone's, with the incident nothing caught: %s", all)
	}
	if got := rv.texts("/shop"); len(got) != 1 || got[0] != shop {
		t.Errorf("team:shop: %v", got)
	}
	slackHas := 0
	for _, x := range rv.texts("/slack") {
		if strings.Contains(x, "shop daily") {
			slackHas++
		}
	}
	if slackHas != 1 {
		t.Errorf("slack: %d of %v", slackHas, rv.texts("/slack"))
	}
	ready := rv.events("report.ready")
	if len(ready) != 1 {
		t.Fatalf("report.ready: %v", ready)
	}
	r := ready[0]
	nums, _ := r["numbers"].(map[string]any)
	if r["report"] != "shop daily" || r["team"] != "shop" || r["url"] != "https://lapilli.example.com/reports/1" || r["from"] != "2026-11-03T00:00:00Z" ||
		nums["summary"] == nil || nums["tasks"] == nil || !strings.HasPrefix(r["slack_text"].(string), "```\n") || r["text"] == "" {
		t.Errorf("report.ready: %v", r)
	}
	// Once for the span; the one that was down, tried again.
	rv.Lock()
	rv.down["/broken"] = false
	rv.Unlock()
	at(t, s, day.Add(10*time.Minute), true)
	if n := len(rv.texts("/ops")); n != 2 || len(rv.events("report.ready")) != 1 {
		t.Errorf("sent twice for a span: %d", n)
	}
	if got := rv.texts("/broken"); len(got) != 1 || got[0] != shop {
		t.Errorf("the one that was down: %v", got)
	}
	// The next day's.
	at(t, s, day.Add(24*time.Hour), true)
	if n := len(rv.texts("/ops")); n != 4 {
		t.Errorf("the next day: %d", n)
	}
	// The pages say when each was last sent.
	if code, body, _ := get(t, h, "/reports"); code != http.StatusOK || !strings.Contains(body, "2026-11-05 00:00") || !strings.Contains(body, "weekly digest") {
		t.Errorf("/reports: %d %s", code, body)
	}
}

// A report waits for the ledger to have its span; six hours after it fell
// due (or half its span, when shorter), it goes with what there is, and
// says how far that is.
func TestReportWaits(t *testing.T) {
	rv := newReceiver(t)
	s := reportServer(t, rv, nil)
	ctx := context.Background()
	cursor, err := s.l.Cursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	due := cursor.Truncate(time.Hour).Add(time.Hour)
	if _, err := s.l.PutScheduled(ctx, ledger.Scheduled{Name: "behind", Schedule: fmt.Sprintf("%d %d * * *", due.Minute(), due.Hour()),
		Sections: []string{"summary"}, Dests: []string{"ops"}, Enabled: true}, "ana", due.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	at(t, s, due.Add(time.Minute), false)
	at(t, s, due.Add(5*time.Hour), false)
	if n := len(rv.texts("/ops")); n != 0 {
		t.Fatalf("sent before the ledger had the span: %d", n)
	}
	at(t, s, due.Add(6*time.Hour), false)
	got := rv.texts("/ops")
	if len(got) != 1 || !strings.Contains(got[0], "The ledger had firings up to "+cursor.Format("2006-01-02 15:04")) {
		t.Fatalf("sent with what there is: %v", got)
	}
	// An hourly one waits half an hour.
	if _, err := s.l.PutScheduled(ctx, ledger.Scheduled{Name: "hourly", Schedule: "0 * * * *", Sections: []string{"summary"}, Dests: []string{"ops"}, Enabled: true},
		"ana", due.Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	next := due.Add(7 * time.Hour)
	at(t, s, next.Add(29*time.Minute), false)
	if n := len(rv.texts("/ops")); n != 1 {
		t.Fatalf("an hourly report sent before the ledger had it: %d", n)
	}
	at(t, s, next.Add(30*time.Minute), false)
	if n := len(rv.texts("/ops")); n != 2 {
		t.Errorf("an hourly report half an hour on: %d", n)
	}
}

// After a restart, a report due while serve was down goes once, for the
// last span due, if that span ended within its own length.
func TestReportAfterRestart(t *testing.T) {
	rv := newReceiver(t)
	s := reportServer(t, rv, nil)
	ctx := context.Background()
	if _, err := s.l.PutScheduled(ctx, ledger.Scheduled{Name: "daily", Schedule: "daily@09:00", Sections: []string{"summary"}, Dests: []string{"ops"}, Enabled: true},
		"ana", now.Add(-4*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	at(t, s, now, false)
	got := rv.texts("/ops")
	if len(got) != 1 || !strings.Contains(got[0], "2026-11-02 00:00 → 2026-11-03 00:00") {
		t.Fatalf("three days missed: %v", got)
	}
	// Down until after the next span had ended a day ago: not sent.
	at(t, s, time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC), true)
	if n := len(rv.texts("/ops")); n != 1 {
		t.Errorf("a span that ended over its length ago: %d", n)
	}
	at(t, s, time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC), true)
	if got := rv.texts("/ops"); len(got) != 2 || !strings.Contains(got[1], "2026-11-04 00:00 → 2026-11-05 00:00") {
		t.Errorf("the next: %v", got)
	}
}

// A team's report is of its rules alone, its times in its zone when it says
// so; one whose team is removed stops, and the pages say why.
func TestReportOfATeam(t *testing.T) {
	rv := newReceiver(t)
	s := reportServer(t, rv, nil)
	ctx := context.Background()
	h := s.Handler()
	shop, err := s.l.PutTeam(ctx, ledger.Team{Name: "shop", Rules: "^Nothing$", TZ: "Asia/Seoul"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutScheduled(ctx, ledger.Scheduled{Name: "shop", TeamID: shop, Zone: "team", Schedule: "daily@09:00", Sections: []string{"summary"},
		Dests: []string{"ops"}, Enabled: true}, "ana", now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	at(t, s, now, false)
	got := rv.texts("/ops")
	// 09:00 in Seoul is 00:00 UTC; the day before it, Seoul's.
	if len(got) != 1 || !strings.Contains(got[0], "shop, for shop: 2026-11-02 00:00 → 2026-11-03 00:00 (Asia/Seoul)") || !strings.Contains(got[0], "Firings: 0;") {
		t.Fatalf("a team's, in its zone, of its rules alone: %v", got)
	}
	if err := s.l.DeleteTeam(ctx, shop, "ana"); err != nil {
		t.Fatal(err)
	}
	at(t, s, now.Add(24*time.Hour), true)
	if n := len(rv.texts("/ops")); n != 1 {
		t.Errorf("sent for a team removed: %d", n)
	}
	if _, body, _ := get(t, h, "/runs"); !strings.Contains(body, "Reports stopped") || !strings.Contains(body, "aren&#39;t sent until they name another: shop.") {
		t.Errorf("/runs doesn't say why: %s", body)
	}
	if _, body, _ := get(t, h, "/reports"); !strings.Contains(body, "its team was removed") {
		t.Errorf("/reports doesn't say why: %s", body)
	}
	if code, _ := api(t, h, "GET", "/api/v1/reports/1/preview", ""); code != http.StatusBadRequest {
		t.Errorf("a preview of a report stopped: %d", code)
	}
}

// What reaches Slack from outside, an incident's title, is escaped, as the
// digest's is.
func TestReportEscaped(t *testing.T) {
	rv := newReceiver(t)
	s := reportServer(t, rv, nil)
	ctx := context.Background()
	h := s.Handler()
	if code, body := api(t, h, "POST", "/api/v1/incidents", `{"id": "INC-9", "title": "<!channel> <https://evil.example|x> down", "start": "2026-11-02T20:00:00Z", "labels": {"service": ["nothing"]}}`); code != http.StatusCreated {
		t.Fatalf("incident: %d %s", code, body)
	}
	if _, err := s.l.PutScheduled(ctx, ledger.Scheduled{Name: "missed", Schedule: "daily@09:00", Sections: []string{"missed"}, Dests: []string{"ops"}, Enabled: true},
		"ana", now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	at(t, s, now, false)
	got := rv.texts("/ops")
	if len(got) != 1 || strings.Contains(got[0], "<!channel>") || strings.Contains(got[0], "<https") || !strings.Contains(got[0], "&lt;!channel&gt;") {
		t.Errorf("escaped: %v", got)
	}
}

// Admins make, change and remove reports; everyone sees them and their
// previews; a destination is a name lapilli serve gave, never a URL; the
// weekly digest is shown as the flags define it; --read-only refuses all.
func TestReportsRoles(t *testing.T) {
	rv := newReceiver(t)
	s := reportServer(t, rv, func(c *Config) {
		c.UserHeader, c.Editors, c.Admins = "X-Forwarded-Email", []string{"ed@example.com"}, []string{"boss@example.com"}
	})
	h := s.Handler()
	as := func(who, method, path, body string) (int, string) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Forwarded-Email", who)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	report := `{"name": "r", "schedule": "weekly mon@09:00", "dests": ["ops"]}`
	if code, _ := as("ed@example.com", "POST", "/api/v1/reports", report); code != http.StatusForbidden {
		t.Errorf("an editor made a report: %d", code)
	}
	if code, body := as("boss@example.com", "POST", "/api/v1/reports", report); code != http.StatusCreated {
		t.Fatalf("an admin: %d %s", code, body)
	}
	for _, dests := range []string{`["https://hooks.example.com/x"]`, `["nope"]`, `["team:nobody"]`} {
		if code, body := as("boss@example.com", "PUT", "/api/v1/reports/1", `{"name": "r", "schedule": "weekly mon@09:00", "dests": `+dests+`}`); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", dests, code, body)
		}
	}
	if code, _ := as("ed@example.com", "DELETE", "/api/v1/reports/1", ""); code != http.StatusForbidden {
		t.Errorf("an editor removed a report: %d", code)
	}
	if code, body := as("viewer@example.com", "GET", "/api/v1/reports", ""); code != http.StatusOK || !strings.Contains(body, `"set_by": "flags"`) ||
		!strings.Contains(body, `"schedule": "weekly mon@09:00"`) || !strings.Contains(body, `"team:shop"`) {
		t.Errorf("everyone sees them, the digest too: %d %s", code, body)
	}
	if code, body := as("viewer@example.com", "GET", "/api/v1/reports/1/preview", ""); code != http.StatusOK || !strings.Contains(body, `"numbers"`) ||
		!strings.Contains(body, `"from": "2026-10-26T00:00:00Z"`) {
		t.Errorf("and previews: %d %s", code, body)
	}
	if code, body, _ := get(t, h, "/reports/1"); code != http.StatusOK || !strings.Contains(body, "As it would be sent now") || !strings.Contains(body, "r: 2026-10-26 00:00") {
		t.Errorf("the preview's page: %d %s", code, body)
	}
	if code, _ := as("viewer@example.com", "GET", "/api/v1/reports/0", ""); code != http.StatusNotFound {
		t.Errorf("the digest by id: %d", code)
	}
	// The form, an admin's.
	form := url.Values{"name": {"form"}, "schedule": {"0 8 * * 1-5"}, "span": {"24h"}, "sections": {"summary", "tasks"}, "dests": {"ops"}, "enabled": {"1"}}
	r := httptest.NewRequest(http.MethodPost, "/reports", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Forwarded-Email", "boss@example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/reports/2" {
		t.Errorf("the form: %d %s", w.Code, w.Body.String())
	}
	if code, body := as("boss@example.com", "GET", "/api/v1/reports/2", ""); !strings.Contains(body, `"span": "24h"`) || !strings.Contains(body, `"tasks"`) {
		t.Errorf("made by the form: %d %s", code, body)
	}
	if code, _ := as("boss@example.com", "DELETE", "/api/v1/reports/2", ""); code != http.StatusNoContent {
		t.Errorf("removed: %d", code)
	}
	cs, _ := s.l.Changes(context.Background(), 5)
	if len(cs) == 0 || cs[0].What != "report form deleted" || cs[0].By != "boss@example.com" {
		t.Errorf("the change noted: %v", cs)
	}

	ro, _ := server(t, func(c *Config) { c.ReadOnly, c.ReportDests = true, map[string]string{"ops": rv.file("/ops")} })
	if code, _ := api(t, ro.Handler(), "POST", "/api/v1/reports", report); code != http.StatusForbidden {
		t.Errorf("--read-only: %d", code)
	}
}
