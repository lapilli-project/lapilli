package serve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// An incident written through the API with the alerts its tool linked to
// it is caught by a firing of one, its labels disagreeing on --match, and
// the incident's page, the rule's and the API say the catch is linked. A
// start with --linked-only judges every firing again, once: an incident
// that lists alerts is then caught by them alone.
func TestLinkedAlertsServe(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	h := s.Handler()
	// Errors fires about search at 22:02 and 23:02 that Sunday.
	if code, body := api(t, h, "PUT", "/api/v1/incidents/INC-9", `{"title": "Checkout down", "start": "2026-11-01T22:00:00Z", "end": "2026-11-01T22:30:00Z",
		"labels": {"service": ["checkout"]}, "alerts": [{"rule": "Errors", "labels": {"service": "search"}}]}`); code/100 != 2 || !strings.Contains(body, `"linked": true`) {
		t.Fatalf("incident: %d %s", code, body)
	}
	if code, body := api(t, h, "PUT", "/api/v1/incidents/INC-10", `{"title": "Search slow", "start": "2026-11-01T23:00:00Z", "end": "2026-11-01T23:30:00Z",
		"labels": {"service": ["search"]}, "alerts": [{"rule": "Latency"}]}`); code/100 != 2 || !strings.Contains(body, `"rule": "Errors"`) || strings.Contains(body, `"linked"`) {
		t.Fatalf("incident caught by the time: %d %s", code, body)
	}
	for _, bad := range []string{`[{"labels": {"service": "search"}}]`, `[{"rule": "Errors", "labels": {"code": 500}}]`, `[{"rule": "Errors", "labels": {"service": ""}}]`,
		// A misspelt key would have every Errors firing in its span link.
		`[{"rule": "Errors", "label": {"service": "checkout"}}]`, `[{"rule": "Errors", "Labels": {"service": "checkout"}}]`} {
		if code, body := api(t, h, "PUT", "/api/v1/incidents/INC-11", `{"start": "2026-11-01T23:00:00Z", "alerts": `+bad+`}`); code != 400 {
			t.Errorf("alerts %s: %d %s, want refused", bad, code, body)
		}
	}
	if _, err := s.l.IncidentByID(ctx, "INC-11"); err == nil {
		t.Error("INC-11 was written with alerts refused")
	}
	// The same alert 150 times is one: written (a week before any firing).
	many := strings.Repeat(`{"rule": "Errors", "labels": {"service": "search"}}, `, 149) + `{"rule": "Errors", "labels": {"service": "search"}}`
	if code, body := api(t, h, "PUT", "/api/v1/incidents/INC-12", `{"start": "2026-10-20T00:00:00Z", "alerts": [`+many+`]}`); code/100 != 2 || strings.Count(body, `"rule": "Errors"`) != 1 {
		t.Errorf("one alert 150 times: %d %s", code, body)
	}
	if _, body, _ := get(t, h, "/incidents/INC-9"); !strings.Contains(body, "(linked in the incident tool)") {
		t.Errorf("incident page: %s", body)
	}
	if _, body, _ := get(t, h, "/incidents/INC-10"); strings.Contains(body, "(linked in the incident tool)") {
		t.Errorf("incident page of one caught by the time: %s", body)
	}
	if _, body, _ := get(t, h, "/rules/Errors"); strings.Count(body, "(linked in the incident tool)") != 1 {
		t.Errorf("rule page: %s", body)
	}
	_, body, _ := get(t, h, "/api/v1/incidents?from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z")
	var incs []struct {
		ID       string          `json:"id"`
		Alerts   []any           `json:"alerts"`
		CaughtBy []verdict.Catch `json:"caught_by"`
	}
	if err := json.Unmarshal([]byte(body), &incs); err != nil || len(incs) != 2 || len(incs[0].Alerts) != 1 || len(incs[0].CaughtBy) != 1 || !incs[0].CaughtBy[0].Linked {
		t.Errorf("/api/v1/incidents: %v %s", err, body)
	}

	n := 0
	was := judgeAll
	judgeAll = func(l *ledger.Ledger, ctx context.Context, o verdict.Options, done func(float64)) error {
		n++
		return was(l, ctx, o, done)
	}
	t.Cleanup(func() { judgeAll = was })
	s.jobs(ctx, false)
	if n != 0 {
		t.Fatalf("judged all %d times with --linked-only as it was", n)
	}
	s.cfg.Options.LinkedOnly = true
	s.jobs(ctx, false)
	s.jobs(ctx, false)
	if v, err := s.l.Meta(ctx, linkedMeta); n != 1 || v != "1" || err != nil {
		t.Errorf("--linked-only given: judged all %d times, the ledger says %q (%v); want once", n, v, err)
	}
	ten, err := s.l.IncidentByID(ctx, "INC-10")
	if err != nil || len(ten.CaughtBy) != 0 {
		t.Errorf("INC-10 with --linked-only: caught by %+v (%v), want none, as it lists only Latency", ten.CaughtBy, err)
	}
	if nine, _ := s.l.IncidentByID(ctx, "INC-9"); len(nine.CaughtBy) != 1 || !nine.CaughtBy[0].Linked {
		t.Errorf("INC-9 with --linked-only: %+v", nine.CaughtBy)
	}
}
