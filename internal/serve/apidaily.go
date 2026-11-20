package serve

import (
	"net/http"
	"net/url"
	"strings"
)

// The API for daily writes (docs/api.md): what a person did each day only
// through a form, deciding an impact window and saying a firing was real,
// through /api/v1 too, with the role, the checks and the limits of the
// form, as the ledger holds every write to them. Proposing stays MCP's
// for agents (behind --mcp-propose) and the rule's page's for people, and
// approving the proposal's page's alone (docs/design/changes.md).

// apiDecideIn is POST /api/v1/incidents/{id}/decide's body.
type apiDecideIn struct {
	// State is confirmed, dismissed, or "" to reopen.
	State string `json:"state"`
	// Title is the incident's a window confirmed becomes: its own when
	// left out.
	Title string `json:"title"`
	// Together dismisses with it the windows of the other named signals
	// on at some time in it, those nothing was decided of yet.
	Together bool `json:"together"`
}

// apiDecide is POST /api/v1/incidents/{id}/decide: an impact window
// confirmed as an incident, dismissed, or reopened, as its form decides it
// (decide), an editor's. It gives the window's id, its state, and the
// incident it was confirmed as.
func (s *Server) apiDecide(w http.ResponseWriter, r *http.Request) {
	var in apiDecideIn
	if err := readJSON(r, &in); err != nil {
		s.oops(w, r, err)
		return
	}
	o, err := s.options(r.Context())
	if err != nil {
		s.oops(w, r, err)
		return
	}
	id := r.PathValue("id")
	title := strings.TrimSpace(in.Title)
	if _, same := s.sameTitle(r, id, title); same {
		title = ""
	}
	made, err := s.l.Decide(r.Context(), id, in.State, title, s.user(r), o)
	if err != nil {
		s.oops(w, r, wrapBad(err))
		return
	}
	if in.State == "dismissed" && in.Together {
		if err := s.dismissWith(r, o); err != nil {
			s.oops(w, r, wrapBad(err))
			return
		}
	}
	out := struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		Incident string `json:"incident,omitempty"`
	}{id, in.State, made}
	writeJSON(w, out)
}

// apiReal is POST /api/v1/firings/real: the firing a report gives (rule,
// labels, start) written down as an incident it caught, as "This was
// real" writes it (real), an editor's: the last 14 days' firings, any
// firing an admin's. It gives the incident written, 201, or, with one
// written from it already or caught since, none, 200.
func (s *Server) apiReal(w http.ResponseWriter, r *http.Request) {
	var in apiFiring
	if err := readJSON(r, &in); err != nil {
		s.oops(w, r, err)
		return
	}
	k, err := in.firingKey()
	if err != nil {
		s.oops(w, r, err)
		return
	}
	made, _, err := s.realOne(r, k)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	out := struct {
		Incident string `json:"incident,omitempty"`
	}{made}
	if made != "" {
		w.Header().Set("Location", "/incidents/"+url.PathEscape(made))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, out)
		return
	}
	writeJSON(w, out)
}
