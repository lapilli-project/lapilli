package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
)

// impactWindow is the impact window the test history's signal makes.
const impactWindow = "impact-20261103T0600Z"

// windowState is an impact window's state as the ledger keeps it.
func windowState(t *testing.T, s *Server, key string) string {
	t.Helper()
	in, err := s.l.IncidentByID(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return in.State
}

// POST /api/v1/incidents/{id}/decide decides an impact window as its form
// does: an editor's, not a viewer's nor a team's editor's, not from
// another site, nor with --read-only; what the ledger refuses is refused.
func TestAPIDecide(t *testing.T) {
	s := verdictServer(t, nil)
	h := s.Handler()
	path := "/api/v1/incidents/" + impactWindow + "/decide"
	if st := windowState(t, s, impactWindow); st != "" {
		t.Fatalf("decided already: %q", st)
	}
	for _, c := range []struct {
		who, site, origin string
		body              any
		want              int
	}{
		{vViewer, "", "", map[string]any{"state": "dismissed"}, http.StatusForbidden},
		{vTeam, "", "", map[string]any{"state": "dismissed"}, http.StatusForbidden},
		{"", "", "", map[string]any{"state": "dismissed"}, http.StatusForbidden},
		{vEditor, "cross-site", "https://evil.example", map[string]any{"state": "dismissed"}, http.StatusForbidden},
		{vEditor, "", "", map[string]any{"state": "maybe"}, http.StatusBadRequest},
		{vEditor, "", "", map[string]any{"state": "dismissed", "title": strings.Repeat("x", 501)}, http.StatusBadRequest},
		{vEditor, "", "", "not an object", http.StatusBadRequest},
	} {
		if code, body, _ := apiAs(t, h, http.MethodPost, path, c.body, c.who, c.site, c.origin); code != c.want {
			t.Errorf("%s from %q, %v: %d, want %d: %s", c.who, c.site, c.body, code, c.want, body)
		}
	}
	if st := windowState(t, s, impactWindow); st != "" {
		t.Fatalf("decided by a refused request: %q", st)
	}
	if code, body, _ := apiAs(t, h, http.MethodPost, "/api/v1/incidents/INC-9/decide", map[string]any{"state": "dismissed"}, vEditor, "", ""); code != http.StatusBadRequest {
		t.Errorf("an incident that isn't an impact window: %d %s", code, body)
	}
	code, body, _ := apiAs(t, h, http.MethodPost, path, map[string]any{"state": "confirmed", "title": "checkout slow"}, vEditor, "", "")
	var out struct{ ID, State, Incident string }
	if code != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil || out.State != "confirmed" || !strings.HasPrefix(out.Incident, "L-") {
		t.Fatalf("confirmed: %d %s", code, body)
	}
	in, err := s.l.IncidentByID(context.Background(), out.Incident)
	if err != nil || in.Title != "checkout slow" {
		t.Errorf("the incident it became: %+v %v", in, err)
	}
	if st := windowState(t, s, impactWindow); st != "confirmed" {
		t.Errorf("the window: %q", st)
	}
	// As its form, noted as the editor's.
	cs, err := s.l.Changes(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cs[0].What, "impact window "+impactWindow+" confirmed") || cs[0].By != vEditor {
		t.Errorf("noted as %+v", cs[0])
	}
	s.cfg.ReadOnly = true
	if code, _, _ := apiAs(t, h, http.MethodPost, path, map[string]any{"state": ""}, vAdmin, "", ""); code != http.StatusForbidden {
		t.Errorf("--read-only: %d", code)
	}
}

// POST /api/v1/firings/real writes the firing a report gives down as an
// incident, as "This was real" does: an editor's of the last 14 days'
// firings, an admin's of any; once, then nothing more; not a viewer's
// nor a team's editor's.
func TestAPIReal(t *testing.T) {
	s := verdictServer(t, nil)
	h := s.Handler()
	f := searchFiring(t, s)
	body := apiFiringOf(f)
	for _, c := range []struct {
		who, site, origin string
		body              any
		want              int
	}{
		{vViewer, "", "", body, http.StatusForbidden},
		{vTeam, "", "", body, http.StatusForbidden},
		{vEditor, "cross-site", "https://evil.example", body, http.StatusForbidden},
		{vEditor, "", "", with(body, "rule", "Nothing"), http.StatusNotFound},
		{vEditor, "", "", map[string]any{"rule": f.Rule}, http.StatusBadRequest},
	} {
		if code, out, _ := apiAs(t, h, http.MethodPost, "/api/v1/firings/real", c.body, c.who, c.site, c.origin); code != c.want {
			t.Errorf("%s, %v: %d, want %d: %s", c.who, c.body, code, c.want, out)
		}
	}
	// Fifteen days on, an editor may no longer; an admin may.
	s.now = func() time.Time { return now.Add(15 * 24 * time.Hour) }
	if code, out, _ := apiAs(t, h, http.MethodPost, "/api/v1/firings/real", body, vEditor, "", ""); code != http.StatusForbidden {
		t.Errorf("an editor, of a firing older than 14 days: %d %s", code, out)
	}
	code, out, hd := apiAs(t, h, http.MethodPost, "/api/v1/firings/real", body, vAdmin, "", "")
	var made struct{ Incident string }
	if code != http.StatusCreated || json.Unmarshal([]byte(out), &made) != nil || made.Incident == "" || hd.Get("Location") != "/incidents/"+made.Incident {
		t.Fatalf("an admin: %d %s %v", code, out, hd)
	}
	in, err := s.l.IncidentByID(context.Background(), made.Incident)
	if err != nil || in.Origin != "ui" || !strings.Contains(in.Title, f.Rule) {
		t.Errorf("written as %+v, %v", in, err)
	}
	// Again: written already, nothing more.
	code, out, _ = apiAs(t, h, http.MethodPost, "/api/v1/firings/real", body, vAdmin, "", "")
	if code != http.StatusOK || strings.Contains(out, "L-") {
		t.Errorf("again: %d %s", code, out)
	}
	if n, err := s.l.Changes(context.Background(), 100); err != nil || !strings.Contains(changesText(n), "incident "+made.Incident+" written") {
		t.Errorf("not noted: %v", err)
	}
}

func changesText(cs []ledger.Change) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(c.By + ": " + c.What + "\n")
	}
	return b.String()
}
