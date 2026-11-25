package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

const (
	respIncTok = "an-incidents-token-long-enough"
	respTok    = "a-responses-token-long-enough"
	respWeek   = "?from=2026-11-02T00:00:00Z&to=2026-11-09T00:00:00Z"
)

// Two firings of a rule copied in, the pages of the first sent for it.
func respFirings() []firing.Firing {
	return []firing.Firing{
		{Rule: "checkout-5xx", Labels: map[string]string{"service": "checkout", "namespace": "shop"}, Start: utc("2026-11-03T02:10:00Z"), End: utc("2026-11-03T02:40:00Z")},
		{Rule: "checkout-5xx", Labels: map[string]string{"service": "checkout", "namespace": "shop", "pod": "x"}, Start: utc("2026-11-04T02:10:00Z"), End: utc("2026-11-04T02:40:00Z")},
	}
}

const respBody = `{"responses": [
 {"id": "PD-1", "alerts": [{"rule": "checkout-5xx", "labels": {"service": "checkout", "namespace": "shop"}}],
  "sent": "2026-11-03T02:11:00Z", "resolved": "2026-11-03T02:40:00Z", "resolved_by": "alert"}]}`

// PUT /api/v1/responses/SOURCE takes what a paging tool says of the pages
// sent in a period with the responses token and no other, replaces that
// source's pages sent in it, and refuses a period or a page that can't be;
// the firings it answers are judged again, and the summary, the report, the
// metrics and MCP say what was done about them.
func TestResponsesPort(t *testing.T) {
	parallel(t)
	s, _ := server(t, func(c *Config) {
		c.IncidentsListen, c.IncidentsToken, c.ResponsesToken = "127.0.0.1:0", tokenFile(t, respIncTok), tokenFile(t, respTok)
		c.UserHeader, c.Editors = "X-Forwarded-Email", []string{"ana@example.com"}
	})
	ctx := context.Background()
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.l.PutFirings(ctx, "cw", utc("2026-11-02T00:00:00Z"), utc("2026-11-09T00:00:00Z"), respFirings(), "job", o); err != nil {
		t.Fatal(err)
	}
	port, ui := s.IncidentsHandler(), s.Handler()
	send := func(h http.Handler, method, path, tok, body string) (int, string) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		r.Header.Set("X-Forwarded-Email", "ana@example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	for _, c := range []struct {
		method, path, tok, body string
		want                    int
	}{
		{"PUT", "/api/v1/responses/pd" + respWeek, "", respBody, http.StatusUnauthorized},
		{"PUT", "/api/v1/responses/pd" + respWeek, respIncTok, respBody, http.StatusUnauthorized},
		{"PUT", "/api/v1/incidents/A-1", respTok, `{"start": "2026-11-03T02:00:00Z"}`, http.StatusUnauthorized},
		{"PUT", "/api/v1/responses/pd?from=2026-11-02T00:00:00Z", respTok, respBody, http.StatusBadRequest},
		{"PUT", "/api/v1/responses/pd?from=2026-11-02&to=2026-11-09", respTok, respBody, http.StatusBadRequest},
		{"PUT", "/api/v1/responses/pd?from=2026-11-09T00:00:00Z&to=2026-11-02T00:00:00Z", respTok, respBody, http.StatusBadRequest},
		// The page was sent at 02:11 on the 3rd, which this period doesn't hold.
		{"PUT", "/api/v1/responses/pd?from=2026-11-04T00:00:00Z&to=2026-11-09T00:00:00Z", respTok, respBody, http.StatusBadRequest},
		{"PUT", "/api/v1/responses/PD" + respWeek, respTok, respBody, http.StatusBadRequest},
		{"PUT", "/api/v1/responses/file" + respWeek, respTok, respBody, http.StatusBadRequest},
		{"PUT", "/api/v1/responses/pd" + respWeek, respTok, "not json", http.StatusBadRequest},
		{"PUT", "/api/v1/responses/pd" + respWeek, respTok, `[]`, http.StatusBadRequest},
		// A person named, or a key that isn't known: refused.
		{"PUT", "/api/v1/responses/pd" + respWeek, respTok, `{"responses": [{"id": "P", "alerts": [{"rule": "r"}], "sent": "2026-11-03T02:11:00Z", "acknowledged_by": "bo"}]}`, http.StatusBadRequest},
		{"PUT", "/api/v1/responses/pd" + respWeek, respTok, strings.Repeat(" ", 65<<20), http.StatusRequestEntityTooLarge},
	} {
		if code, msg := send(port, c.method, c.path, c.tok, c.body); code != c.want {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.path, code, msg, c.want)
		}
	}
	if pages, _ := s.l.ResponsePages(ctx, utc("2026-10-01T00:00:00Z"), utc("2026-12-01T00:00:00Z")); len(pages) != 0 {
		t.Fatalf("refused writes kept %d pages", len(pages))
	}
	code, msg := send(port, "PUT", "/api/v1/responses/pd"+respWeek, respTok, respBody)
	var wrote responseWrite
	if json.Unmarshal([]byte(msg), &wrote); code != http.StatusOK || wrote.Added != 1 || !wrote.Differs || wrote.Pages != 1 || wrote.Source != "pd" {
		t.Fatalf("written: %d %s", code, msg)
	}
	// The same again changes nothing.
	if code, msg := send(port, "PUT", "/api/v1/responses/pd"+respWeek, respTok, respBody); code != http.StatusOK || !strings.Contains(msg, `"changed_ledger": false`) {
		t.Errorf("again: %d %s", code, msg)
	}
	// Noted as the token, not anyone's use of Lapilli.
	if cs, _ := s.l.ChangesSince(ctx, time.Unix(0, 0), 10); len(cs) > 0 {
		for _, c := range cs {
			if c.By != ledger.ResponsesCopier && c.By != ledger.FiringsCopier && c.By != "job" {
				t.Errorf("changes: %+v", c)
			}
		}
	}
	// The UI's port has no such route.
	if code, _ := send(ui, "PUT", "/api/v1/responses/pd"+respWeek, respTok, respBody); code != http.StatusNotFound && code != http.StatusMethodNotAllowed && code != http.StatusForbidden {
		t.Errorf("pages on the UI's port: %d", code)
	}

	// The report says what was done: the first firing's page nobody answered,
	// the second none.
	code, msg = send(ui, "GET", "/api/v1/report"+respWeek, "", "")
	var rep verdict.Report
	if err := json.Unmarshal([]byte(msg), &rep); err != nil || code != http.StatusOK {
		t.Fatalf("report: %d %v", code, err)
	}
	if rep.Responses == nil || rep.Responses.Pages != 1 || rep.Settings == nil || rep.Settings.NoisyBy != "caught" || rep.Settings.Routine != 0.5 {
		t.Errorf("report responses %+v settings %+v", rep.Responses, rep.Settings)
	}
	kinds := map[string]string{}
	for _, j := range rep.Firings {
		if j.Response == nil {
			t.Fatalf("a firing with no response: %+v", j)
		}
		kinds[j.Start.UTC().Format("02")] = j.Response.Kind
		if j.Start.Day() == 3 && (j.Response.Page == nil || j.Response.Page.ID != "PD-1" || j.Response.Page.ResolvedBy != "alert" || j.Response.Useful == nil || *j.Response.Useful) {
			t.Errorf("the answered firing: %+v", j.Response)
		}
	}
	if kinds["03"] != verdict.ResponseUnanswered || kinds["04"] != verdict.ResponseNone {
		t.Errorf("responses by day: %v", kinds)
	}
	if len(rep.Rules) != 1 || rep.Rules[0].Responses == nil || rep.Rules[0].Responses.Unanswered != 1 || rep.Rules[0].Responses.None != 1 || rep.Rules[0].Responses.NotUseful != 1 {
		t.Errorf("the rule's responses: %+v", rep.Rules)
	}
	// The summary: the week's responses and its actionable share.
	code, msg = send(ui, "GET", "/api/v1/summary?from=2026-11-02&to=2026-11-03", "", "")
	var sum struct {
		Rows []struct {
			Team       string
			Responses  *summaryResponses
			Actionable *summaryActionable
		}
	}
	if err := json.Unmarshal([]byte(msg), &sum); err != nil || code != http.StatusOK || len(sum.Rows) == 0 {
		t.Fatalf("summary: %d %v\n%s", code, err, msg)
	}
	row := sum.Rows[0]
	if row.Responses == nil || row.Responses.Unanswered != 1 || row.Actionable == nil || row.Actionable.NotUseful != 1 {
		t.Errorf("the summary's responses %+v, actionable %+v", row.Responses, row.Actionable)
	}
	// /metrics.
	if _, page, _ := get(t, ui, "/metrics"); !strings.Contains(page, "lapilli_rule_responses") {
		t.Errorf("/metrics has no lapilli_rule_responses")
	}
	// MCP's firings tool.
	_, out, err := s.toolFirings(ctx, nil, firingsIn{Days: 7})
	if err != nil {
		t.Fatal(err)
	}
	_ = out

	// Sent again with none, the source's page in the period goes, and the
	// firing has none.
	code, msg = send(port, "PUT", "/api/v1/responses/pd"+respWeek, respTok, `{"responses": []}`)
	if code != http.StatusOK || !strings.Contains(msg, `"removed": 1`) {
		t.Fatalf("replaced with none: %d %s", code, msg)
	}
	rep2, err := s.l.Report(ctx, utc("2026-11-02T00:00:00Z"), utc("2026-11-09T00:00:00Z"), o, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range rep2.Firings {
		// No page kept: nothing is said of what was done.
		if j.Response != nil && j.Response.Kind != verdict.ResponseNone {
			t.Errorf("after the page went: %+v", j.Response)
		}
	}
	// The same token in both files: neither is taken.
	os.WriteFile(s.cfg.IncidentsToken, []byte(respTok+"\n"), 0o600)
	if code, _ := send(port, "PUT", "/api/v1/responses/pd"+respWeek, respTok, respBody); code != http.StatusUnauthorized {
		t.Errorf("pages with the token both files hold: %d", code)
	}
	os.WriteFile(s.cfg.IncidentsToken, []byte(respIncTok), 0o600)
	// Read-only refuses them.
	s.cfg.ReadOnly = true
	if code, _ := send(port, "PUT", "/api/v1/responses/pd"+respWeek, respTok, respBody); code != http.StatusForbidden {
		t.Errorf("read-only: %d", code)
	}
}

// --responses is read again each run: pages added, changed and taken away
// are kept as the file has them, a file that hasn't changed is not read
// again, a file that is broken keeps what the ledger had, and what it says
// of a page shows in the firings.
func TestResponsesFile(t *testing.T) {
	parallel(t)
	dir := t.TempDir()
	file := dir + "/responses.yaml"
	os.WriteFile(file, []byte(`responses:
  - id: PD-1
    alerts: [{rule: checkout-5xx, labels: {service: checkout, namespace: shop}}]
    sent: 2026-11-03T02:11:00Z
    acknowledged: 2026-11-03T02:15:00Z
`), 0o600)
	s, _ := server(t, func(c *Config) { c.Responses = file })
	ctx := context.Background()
	o, err := s.options(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s.responsesOn() {
		t.Fatal("responses aren't on with --responses")
	}
	if o.Within == nil {
		t.Error("no window for a page to be tied to a firing in")
	}
	if _, err := s.l.PutFirings(ctx, "cw", utc("2026-11-02T00:00:00Z"), utc("2026-11-09T00:00:00Z"), respFirings(), "job", o); err != nil {
		t.Fatal(err)
	}
	if err := s.syncResponses(ctx, o); err != nil {
		t.Fatal(err)
	}
	pages, _ := s.l.ResponsePages(ctx, utc("2026-11-01T00:00:00Z"), utc("2026-11-30T00:00:00Z"))
	if len(pages) != 1 || pages[0].Acknowledged.IsZero() {
		t.Fatalf("pages: %+v", pages)
	}
	kind := func() string {
		rep, err := s.l.Report(ctx, utc("2026-11-02T00:00:00Z"), utc("2026-11-09T00:00:00Z"), o, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range rep.Firings {
			if j.Start.Day() == 3 && j.Response == nil {
				return verdict.ResponseNone // no page kept: nothing is said
			} else if j.Start.Day() == 3 {
				return j.Response.Kind
			}
		}
		return ""
	}
	if k := kind(); k != verdict.ResponseAcknowledged {
		t.Errorf("the page was acknowledged: %s", k)
	}
	// Unchanged: not read again (a page the ledger's own write took away
	// stays away until the file changes).
	if _, err := s.l.SyncResponses(ctx, ledger.ResponsesFile, "", nil, o); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	if err := s.syncResponses(ctx, o); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.l.ResponsePages(ctx, utc("2026-11-01T00:00:00Z"), utc("2026-11-30T00:00:00Z")); len(p) != 0 {
		t.Errorf("a file as it was was read again: %d pages", len(p))
	}
	// Changed: the page was resolved by its alert instead.
	os.WriteFile(file, []byte(strings.Replace(strings.Replace(string(b), "    acknowledged: 2026-11-03T02:15:00Z\n", "    resolved: 2026-11-03T02:40:00Z\n    resolved_by: alert\n", 1), "PD-1", "PD-1", 1)), 0o600)
	if err := s.syncResponses(ctx, o); err != nil {
		t.Fatal(err)
	}
	if k := kind(); k != verdict.ResponseUnanswered {
		t.Errorf("after the page changed: %s", k)
	}
	// Broken: refused, and what the ledger had stays.
	os.WriteFile(file, []byte("responses:\n  - id: PD-2\n"), 0o600)
	if err := s.syncResponses(ctx, o); err == nil {
		t.Error("a broken file was taken")
	}
	if k := kind(); k != verdict.ResponseUnanswered {
		t.Errorf("after a broken file: %s", k)
	}
	// Empty list: every page goes.
	os.WriteFile(file, []byte("responses: []\n"), 0o600)
	if err := s.syncResponses(ctx, o); err != nil {
		t.Fatal(err)
	}
	if k := kind(); k != verdict.ResponseNone {
		t.Errorf("with no pages: %s", k)
	}
}

// What a rule is found noisy by: caught as ever, or by what was done about
// its firings once the Settings page, or --noisy-by, says useful.
func TestNoisyBySetting(t *testing.T) {
	parallel(t)
	s, _ := server(t, nil)
	ctx := context.Background()
	if o, err := s.options(ctx); err != nil || o.NoisyBy != "" || show(o, "noisy-by") != "caught" {
		t.Fatalf("the default: %q %v", o.NoisyBy, err)
	}
	if err := s.l.SetSettings(ctx, map[string]string{"noisy-by": "useful"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if o, err := s.options(ctx); err != nil || o.NoisyBy != verdict.NoisyByUseful || show(o, "noisy-by") != "useful" {
		t.Errorf("set: %q %v", o.NoisyBy, err)
	}
	var o verdict.Options
	if err := apply(&o, "noisy-by", "both"); err == nil {
		t.Error("a value that isn't caught or useful was taken")
	}
	// Judged by what was done about it, a rule's firings with nothing known are not useful where
	// the record judges them.
	s.cfg.Options.NoisyBy = verdict.NoisyByUseful
	if k := responsesKey(verdict.Options{Routine: 0.7}); k == "" || strings.ContainsAny(k, "1m") {
		t.Errorf("the key of --routine: %q", k)
	}
	if responsesKey(verdict.Options{}) != "" || responsesKey(verdict.Options{Routine: 0.5}) != "" {
		t.Error("the defaults add to what judged the ledger")
	}
	if linkedSaid(linkedText(verdict.Options{LinkedOnly: true, Routine: 0.7, RolloutMatch: []string{"service"}})) != "on" {
		t.Error("--linked-only isn't read past the key of the responses")
	}
}
