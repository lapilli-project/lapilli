package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
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

// A window confirmed, reopened and confirmed again, through the API or its
// form, is the incident it was confirmed as before: each gives that
// incident, and the ledger keeps the one. The title the form or the API
// gives is its title; the form's sent back as the window's own, or none,
// leaves it as it was.
func TestDecideAgainThroughTheAPIAndTheForm(t *testing.T) {
	s := verdictServer(t, nil)
	h := s.Handler()
	ctx := context.Background()
	path := "/api/v1/incidents/" + impactWindow + "/decide"
	decide := func(state, title string) string {
		t.Helper()
		code, body, _ := apiAs(t, h, http.MethodPost, path, map[string]any{"state": state, "title": title}, vEditor, "", "")
		var out struct{ Incident string }
		if code != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("%q: %d %s", state, code, body)
		}
		return out.Incident
	}
	first := decide("confirmed", "checkout slow")
	if !strings.HasPrefix(first, "L-") {
		t.Fatalf("confirmed as %q", first)
	}
	decide("", "")
	if again := decide("confirmed", "checkout slow, again"); again != first {
		t.Errorf("confirmed again through the API: %q, want %q", again, first)
	}
	decide("", "")
	code, _, hd := ui(t, h, http.MethodPost, "/incidents/"+impactWindow+"/decide", vEditor, "same-origin", url.Values{"state": {"confirmed"}, "title": {"by the form"}})
	if code != http.StatusSeeOther || hd.Get("Location") != "/incidents/"+first {
		t.Errorf("confirmed again by the form: %d %s, want the incident written before, %s", code, hd.Get("Location"), first)
	}
	in, err := s.l.IncidentByID(ctx, impactWindow)
	if err != nil {
		t.Fatal(err)
	}
	incs, err := s.l.Incidents(ctx, in.Start.Add(-time.Hour), in.Start.Add(time.Hour), true)
	if err != nil {
		t.Fatal(err)
	}
	var written []string
	for _, x := range incs {
		if strings.HasPrefix(x.ID, "L-") {
			written = append(written, x.ID+" "+x.Title)
		}
	}
	if len(written) != 1 || written[0] != first+" by the form" {
		t.Errorf("written down: %q, want %s alone, titled as the form said", written, first)
	}
	// The form as the page gives it, the window's own title in it, and
	// the API with no title: the incident's title stays.
	decide("", "")
	if code, _, hd := ui(t, h, http.MethodPost, "/incidents/"+impactWindow+"/decide", vEditor, "same-origin", url.Values{"state": {"confirmed"}, "title": {in.Title}}); code != http.StatusSeeOther ||
		hd.Get("Location") != "/incidents/"+first {
		t.Errorf("confirmed again by the form as the page gives it: %d %s", code, hd.Get("Location"))
	}
	decide("", "")
	decide("confirmed", "")
	if now, err := s.l.IncidentByID(ctx, first); err != nil || now.Title != "by the form" {
		t.Errorf("the incident's title, none given: %q (%v), want it as it was", now.Title, err)
	}
	// Deleted through the API and written again under its id by an
	// editor, of another day: another incident, which the window doesn't
	// take, confirmed again.
	decide("", "")
	if code, body, _ := apiAs(t, h, http.MethodDelete, "/api/v1/incidents/"+first, nil, vEditor, "", ""); code != http.StatusNoContent {
		t.Fatalf("deleted: %d %s", code, body)
	}
	other := map[string]any{"title": "search down", "start": in.Start.Add(48 * time.Hour).Format(time.RFC3339), "end": in.Start.Add(49 * time.Hour).Format(time.RFC3339),
		"labels": map[string][]string{"service": {"search"}}}
	if code, body, _ := apiAs(t, h, http.MethodPut, "/api/v1/incidents/"+first, other, vEditor, "", ""); code >= 300 {
		t.Fatalf("written again under %s: %d %s", first, code, body)
	}
	again := decide("confirmed", "")
	if again == "" || again == first {
		t.Errorf("confirmed again after %s was deleted and written again: %q, want an incident of its own", first, again)
	}
	if now, err := s.l.IncidentByID(ctx, first); err != nil || now.Title != "search down" || !now.Start.Equal(in.Start.Add(48*time.Hour)) {
		t.Errorf("the editor's incident: %+v %v, want it as they wrote it", now, err)
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
