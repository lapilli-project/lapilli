package serve

import (
	"fmt"
	"net/http"
	"strings"

	"lapilli.dev/internal/ledger"
)

// The roles admins give in the UI (docs/design/serveops.md): an editor's,
// of every team or of one, on top of the flags. Only admins see who holds
// a role, and give or take one away.
func (s *Server) rolesRoutes(mux *http.ServeMux, page func(string, func(*http.Request) (any, error)) http.HandlerFunc) {
	mux.HandleFunc("GET /roles", page("roles", s.rolesPage))
	mux.HandleFunc("POST /roles", s.admin(s.postRole))
	mux.HandleFunc("POST /roles/{id}/delete", s.admin(s.deleteRole))
	mux.HandleFunc("GET /api/v1/roles", s.cached(s.apiRoles))
	mux.HandleFunc("POST /api/v1/roles", s.admin(s.apiPutRole))
	mux.HandleFunc("DELETE /api/v1/roles/{id}", s.admin(s.apiDelete(func(r *http.Request, id int64) error {
		if err := s.rolesOn(); err != nil {
			return err
		}
		return s.l.DeleteGrant(r.Context(), id, s.user(r))
	})))
}

// errRolesOff is a role given where roles aren't kept in the ledger.
var errRolesOff = bad("roles are given in the UI only with --admins and --user-header: without them there is no one to give them, or everyone may change everything")

func (s *Server) rolesOn() error {
	if !s.rolesKept() {
		return errRolesOff
	}
	return nil
}

// roleLine is a role given in the UI, as the page shows it.
type roleLine struct {
	ledger.Grant
	Team string `json:"team,omitempty"`
}

type rolesView struct {
	Kept bool // roles can be given here
	// The flags' lists, which the UI doesn't change.
	Admins, Editors, Approvers []string
	Lines                      []roleLine
	Teams                      []ledger.Team
	Most                       int
}

// roleLines are the roles given in the UI, with their teams' names.
func (s *Server) roleLines(r *http.Request) ([]roleLine, []ledger.Team, error) {
	gs, err := s.l.Grants(r.Context())
	if err != nil {
		return nil, nil, err
	}
	o, err := s.owners(r.Context())
	if err != nil {
		return nil, nil, err
	}
	out := []roleLine{}
	for _, g := range gs {
		out = append(out, roleLine{Grant: g, Team: o.byID[g.TeamID].Name})
	}
	return out, o.teams, nil
}

func (s *Server) rolesPage(r *http.Request) (any, error) {
	if s.role(r) < admin {
		return nil, forbidden{admin}
	}
	lines, teams, err := s.roleLines(r)
	if err != nil {
		return nil, err
	}
	approvers := s.cfg.Approvers
	if len(approvers) == 0 && s.cfg.Changes {
		approvers = []string{"(" + tr(lang(r), "the admins") + ")"}
	}
	return rolesView{Kept: s.rolesKept(), Admins: s.cfg.Admins, Editors: s.cfg.Editors, Approvers: approvers, Lines: lines, Teams: teams,
		Most: ledger.MaxGrants}, nil
}

// grantFrom reads a role to give: who, as user:NAME or group:NAME, and
// the team, 0 for every team.
func grantFrom(kind, name string, team int64) ledger.Grant {
	return ledger.Grant{Who: kind + ":" + strings.TrimSpace(name), TeamID: team}
}

func (s *Server) postRole(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	err := s.rolesOn()
	if err == nil {
		_, err = s.l.PutGrant(r.Context(), grantFrom(r.PostFormValue("kind"), r.PostFormValue("name"), formInt(r, "team")), s.user(r))
	}
	s.written(w, r, err, "/roles")
}

func (s *Server) deleteRole(w http.ResponseWriter, r *http.Request) {
	err := s.rolesOn()
	if err == nil {
		err = s.l.DeleteGrant(r.Context(), pathID(r), s.user(r))
	}
	s.written(w, r, err, "/roles")
}

func (s *Server) apiRoles(w http.ResponseWriter, r *http.Request) {
	if s.role(r) < admin {
		s.oops(w, r, forbidden{admin})
		return
	}
	lines, _, err := s.roleLines(r)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"kept": s.rolesKept(), "flags": map[string][]string{"admins": orNoneOf(s.cfg.Admins),
		"editors": orNoneOf(s.cfg.Editors), "approvers": orNoneOf(s.cfg.Approvers)}, "given": lines})
}

func (s *Server) apiPutRole(w http.ResponseWriter, r *http.Request) {
	if err := s.rolesOn(); err != nil {
		s.oops(w, r, err)
		return
	}
	var g ledger.Grant
	if err := readJSON(r, &g); err != nil {
		s.oops(w, r, err)
		return
	}
	id, err := s.l.PutGrant(r.Context(), ledger.Grant{Who: g.Who, Role: g.Role, TeamID: g.TeamID}, s.user(r))
	if err != nil {
		s.oops(w, r, err)
		return
	}
	lines, _, err := s.roleLines(r)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	for _, l := range lines {
		if l.ID == id {
			answer(w, r, fmt.Sprintf("/api/v1/roles/%d", id), l)
			return
		}
	}
	s.oops(w, r, ledger.ErrNotFound)
}
