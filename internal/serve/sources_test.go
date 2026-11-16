package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// putAs writes an incident through the incidents port, with the token.
func putAs(t *testing.T, s *Server, token, id, start string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/incidents/"+id, strings.NewReader(`{"title": "t", "start": "`+start+`"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.IncidentsHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", id, w.Code, w.Body.String())
	}
}

// sourceRowOf is the row of the Incident sources table a source names.
func sourceRowOf(body, name string) string {
	i := strings.Index(body, `id="incident-sources"`)
	if i < 0 {
		return ""
	}
	body = body[i:]
	j := strings.Index(body, name)
	if j < 0 {
		return ""
	}
	start := strings.LastIndex(body[:j], "<tr>")
	end := strings.Index(body[j:], "</tr>")
	return body[start : j+end]
}

// What a tool's job copies in is listed by source, its ids' prefix: when
// it last wrote, unchanged or not, and how many incidents in four weeks;
// quiet when nothing came for two days. Only an admin sees the port and
// the token's file; no one, the token.
func TestSourcesIntake(t *testing.T) {
	token := "an-incidents-token-0123"
	file := tokenFile(t, token)
	s := criteriaServer(t, false, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", file })
	h := s.Handler()
	putAs(t, s, token, "PD-1", "2026-11-01T00:00:00Z")
	putAs(t, s, token, "pd-2", "2026-10-01T00:00:00Z") // over four weeks ago: not counted
	putAs(t, s, token, "OPS-3", "2026-11-02T00:00:00Z")
	_, body := askAs(t, h, http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	pd := sourceRowOf(body, "PD-")
	if !strings.Contains(pd, "(PagerDuty)") || !strings.Contains(pd, "✓ working") || !strings.Contains(pd, "last received 2026-11-03 12:00") ||
		!strings.Contains(pd, `<td class="n">1</td>`) || !strings.Contains(pd, "port 127.0.0.1:0, token in "+file) {
		t.Errorf("PD:\n%s", pd)
	}
	if ops := sourceRowOf(body, "OPS-"); !strings.Contains(ops, "✓ working") || strings.Contains(ops, "(") {
		t.Errorf("OPS:\n%s", ops)
	}
	if strings.Contains(body, token) {
		t.Error("the token is shown")
	}
	_, body = askAs(t, h, http.MethodGet, "/settings", nil, "bo@example.com", "same-origin")
	if strings.Contains(body, file) || strings.Contains(body, "127.0.0.1:0") {
		t.Error("an editor sees the port and the token's file")
	}
	// Two days on, OPS's job wrote again, PD's didn't.
	s.now = func() time.Time { return now.Add(49 * time.Hour) }
	putAs(t, s, token, "OPS-3", "2026-11-02T00:00:00Z")
	_, body = askAs(t, h, http.MethodGet, "/settings", nil, "ana@example.com", "same-origin")
	if pd := sourceRowOf(body, "PD-"); !strings.Contains(pd, "⚠&#xFE0E; quiet") && !strings.Contains(pd, "⚠︎ quiet") || !strings.Contains(pd, "is its job still running?") {
		t.Errorf("PD, two days on:\n%s", pd)
	}
	if ops := sourceRowOf(body, "OPS-"); !strings.Contains(ops, "✓ working") || !strings.Contains(ops, "last received 2026-11-05 13:00") {
		t.Errorf("OPS, written again:\n%s", ops)
	}
}

// A source's writes are kept once in receivedEvery by a replica, not each
// write: a job writes every incident each hour.
func TestSourcesReceivedSeldom(t *testing.T) {
	ctx := context.Background()
	token := "an-incidents-token-0123"
	s := criteriaServer(t, false, func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", tokenFile(t, token) })
	for _, after := range []time.Duration{0, time.Minute, receivedEvery + time.Minute} {
		s.now = func() time.Time { return now.Add(after) }
		putAs(t, s, token, "PD-1", "2026-11-01T00:00:00Z")
		got, err := s.l.Received(ctx)
		want := now
		if after > receivedEvery {
			want = now.Add(after)
		}
		if err != nil || !got["PD"].Equal(want) {
			t.Errorf("after %s: %s %v", after, got["PD"], err)
		}
	}
}

// The incidents file and the impact signal: read, or failing with why; a
// source not set up says so, in no colour.
func TestSourcesReadOrFailing(t *testing.T) {
	ctx := context.Background()
	s := criteriaServer(t, true, nil)
	h := s.Handler()
	_, body, _ := get(t, h, "/settings")
	if row := sourceRowOf(body, "Incidents file"); !strings.Contains(row, "✓ working") || !strings.Contains(row, "last read 2026-11-03 12:00") {
		t.Errorf("file:\n%s", row)
	}
	if row := sourceRowOf(body, "Impact signal"); !strings.Contains(row, "✓ working") || !strings.Contains(row, `<td class="n">1</td>`) {
		t.Errorf("impact:\n%s", row)
	}
	if row := sourceRowOf(body, "Intake API"); !strings.Contains(row, `status-unknown">? not set up`) {
		t.Errorf("intake:\n%s", row)
	}
	os.WriteFile(s.cfg.Incidents, []byte("not: [incidents"), 0o644)
	if err := s.l.SetSettings(ctx, map[string]string{"impact": "turned"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now.Add(2 * time.Hour) } // a run due
	s.jobs(ctx, true)
	_, body, _ = get(t, h, "/settings")
	if row := sourceRowOf(body, "Incidents file"); !strings.Contains(row, `status-bad">✕ failing`) || !strings.Contains(row, "yaml") {
		t.Errorf("file failing:\n%s", row)
	}
	if row := sourceRowOf(body, "Impact signal"); !strings.Contains(row, `status-bad">✕ failing`) || !strings.Contains(row, "slow down") {
		t.Errorf("impact failing:\n%s", row)
	}
}

// sourcesStates are deployments whose Incident sources draw each state
// besides those of a11yServer's, which has no incidents port: under
// /sources, a tool's job gone quiet, the incidents file failing and the
// impact signal read; under /intake, a port that nothing came through yet,
// and nothing written in four weeks; under /nodata, an impact signal one of
// whose selectors has no data; under /floor, a criterion saved and read by
// a run, not yet read again. For TestStatusKinds and TestA11yServe.
func sourcesStates(t *testing.T) map[string]*Server {
	t.Helper()
	ctx := context.Background()
	token := "an-incidents-token-0123"
	intake := func(c *Config) { c.IncidentsListen, c.IncidentsToken = "127.0.0.1:0", tokenFile(t, token) }
	quiet := criteriaServer(t, true, intake)
	quiet.now = func() time.Time { return now.Add(-72 * time.Hour) }
	putAs(t, quiet, token, "PD-1", "2026-10-30T00:00:00Z")
	os.WriteFile(quiet.cfg.Incidents, []byte("not: [incidents"), 0o644)
	quiet.now = func() time.Time { return now.Add(2 * time.Hour) }
	quiet.jobs(ctx, true)
	// An impact signal one of whose selectors has no data.
	none := criteriaServer(t, false, nil)
	hist(none).begins = map[string][]time.Time{"probe_failed": {}}
	if err := none.l.SetSettings(ctx, map[string]string{"impact": "impact or probe_failed == 1"}, "ana@example.com"); err != nil {
		t.Fatal(err)
	}
	none.jobs(ctx, false)
	// A criterion saved, its read again failing, and a run reading it: a
	// floor.
	floor := criteriaServer(t, false, nil)
	saveFailing(t, floor, "impact > 1")
	runDue(floor, 2)
	floor.jobs(ctx, true)
	return map[string]*Server{"/sources": quiet, "/intake": criteriaServer(t, false, intake), "/nodata": none, "/floor": floor}
}
