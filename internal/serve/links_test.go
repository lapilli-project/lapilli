package serve

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// An incident written through the API with the alerts its tool linked to
// it is caught by a firing of one, its labels disagreeing on --match, and
// the incident's page, the rule's and the API say the catch is linked. A
// start with --linked-only judges every firing again, once: an incident
// that lists alerts is then caught by them alone. The Settings page says
// which is in effect.
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

	// The Settings page, in each language, and its API say whether
	// --linked-only is in effect.
	says := func(on bool) {
		t.Helper()
		_, page, _ := get(t, h, "/settings")
		ko := pageIn(t, h, "/settings", "", "ko")
		_, js, _ := get(t, h, "/api/v1/settings")
		want := map[bool][3]string{
			true:  {"<code>--linked-only</code>: On: an incident that lists", "연결된 알림만 잡은 것으로", `"linked_only": true`},
			false: {"<code>--linked-only</code>: Off: an incident that lists", "연결된 알림과,", `"linked_only": false`},
		}
		for i, body := range []string{page, ko, js} {
			if !strings.Contains(body, want[on][i]) || strings.Contains(body, want[!on][i]) {
				t.Errorf("--linked-only %v: want %q, not %q, in %s", on, want[on][i], want[!on][i], body)
			}
		}
	}
	says(false)
	// While every firing is judged again with a new --linked-only, the page
	// and its API say so beside it; not before, nor once it is done.
	judging := func(on bool) {
		t.Helper()
		_, page, _ := get(t, h, "/settings")
		ko := pageIn(t, h, "/settings", "", "ko")
		_, js, _ := get(t, h, "/api/v1/settings")
		for i, body := range []string{page, ko, js} {
			want := []string{"Every firing is being judged again with it", "이 값으로 모든 발화를 다시 판정하는 중이에요", `"linked_only_judging": true`}[i]
			if strings.Contains(body, want) != on {
				t.Errorf("judging again %v: %q in %s", on, want, body)
			}
		}
		if !on && !strings.Contains(js, `"linked_only_judging": false`) {
			t.Errorf("no linked_only_judging: %s", js)
		}
	}
	judging(false)

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
	says(true)
	judging(true)
	s.jobs(ctx, false)
	judging(false)
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

// Replicas started with different --linked-only judge every firing again
// once for each change of value, not at each change of leader: one whose
// flag another replica's newer change has passed judges what it runs with
// its own and nothing again, and says so in its log, at start and as it
// leads, with both values. The leader's flag judges what it runs either
// way.
func TestLinkedOnlyReplicasDiffer(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, nil)
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	n := 0
	was := judgeAll
	judgeAll = func(l *ledger.Ledger, ctx context.Context, o verdict.Options, done func(float64)) error {
		n++
		return was(l, ctx, o, done)
	}
	t.Cleanup(func() { judgeAll = was })
	var logged strings.Builder
	s.log = slog.New(slog.NewTextHandler(&logged, nil))
	started := func() {
		// A process started afresh: what it knew is what it finds.
		s.linkedKnown.Lock()
		s.linkedKnown.read = false
		s.linkedKnown.Unlock()
		s.logLinked(ctx)
	}
	h := s.Handler()
	judging := func() bool {
		_, js, _ := get(t, h, "/api/v1/settings")
		return strings.Contains(js, `"linked_only_judging": true`)
	}

	// Replica A, with --linked-only, on a ledger judged without it.
	s.cfg.Options.LinkedOnly = true
	started()
	if !strings.Contains(logged.String(), "every firing is judged again with this replica's, once") || !strings.Contains(logged.String(), "linked_only=on ledger=off") {
		t.Errorf("at start: %s", logged.String())
	}
	if !judging() {
		t.Error("the Settings page doesn't say every firing is to be judged again")
	}
	s.jobs(ctx, false)
	if v, _ := s.l.Meta(ctx, linkedMeta); n != 1 || v != "1" || judging() {
		t.Fatalf("A leading: judged all %d times, the ledger says %q", n, v)
	}

	// Replica B, without it, started since, leads and judges every firing
	// again: A, leading again, doesn't undo it.
	if err := s.l.SetMeta(ctx, linkedMeta, ""); err != nil {
		t.Fatal(err)
	}
	logged.Reset()
	s.logLinked(ctx) // as lead does
	s.jobs(ctx, false)
	s.jobs(ctx, false)
	if v, _ := s.l.Meta(ctx, linkedMeta); n != 1 || v != "" {
		t.Errorf("A leading after B: judged all %d times, the ledger says %q; want once in all, B's left", n, v)
	}
	if !strings.Contains(logged.String(), "another replica judged every firing with since this one started") || !strings.Contains(logged.String(), "linked_only=on ledger=off") {
		t.Errorf("A leading after B: %s", logged.String())
	}
	if judging() {
		t.Error("A's Settings page says every firing is being judged again, which A doesn't do")
	}

	// A started again with it, after B: a change of value, judged once.
	logged.Reset()
	started()
	s.jobs(ctx, false)
	s.jobs(ctx, false)
	if v, _ := s.l.Meta(ctx, linkedMeta); n != 2 || v != "1" {
		t.Errorf("A started again: judged all %d times, the ledger says %q; want twice in all", n, v)
	}
	// Agreeing, it says nothing.
	logged.Reset()
	s.logLinked(ctx)
	if strings.Contains(logged.String(), "linked-only") {
		t.Errorf("agreeing: %s", logged.String())
	}
}
