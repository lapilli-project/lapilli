package serve

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// Roles: who may change what. Without --editors or --admins, everyone may
// change everything, as before; with either, whoever the sign-in proxy
// names (--user-header), or a group it names them in (--groups-header),
// gets the role the lists give them, and everyone else may only look.
//
//	viewer  looks
//	editor  writes incidents, tasks, comments, quiet windows, notes on rules
//	admin   also settings and teams
type role int

const (
	viewer role = iota
	editor
	admin
)

func (r role) String() string { return [...]string{"viewer", "editor", "admin"}[r] }

// groups are the groups the sign-in proxy says the reader is in.
func (s *Server) groups(r *http.Request) []string {
	if s.cfg.GroupsHeader == "" {
		return nil
	}
	return strings.FieldsFunc(r.Header.Get(s.cfg.GroupsHeader), func(c rune) bool { return c == ',' || c == ' ' || c == ';' })
}

// role is what the reader may do.
func (s *Server) role(r *http.Request) role {
	switch {
	case s.cfg.ReadOnly:
		return viewer
	case len(s.cfg.Editors) == 0 && len(s.cfg.Admins) == 0:
		return admin
	}
	// Whoever the proxy didn't name may only look, whatever the lists say.
	user := s.user(r)
	if user == "anonymous" {
		return viewer
	}
	who := append([]string{user}, s.groups(r)...)
	in := func(list []string) bool {
		return slices.Contains(list, "*") || slices.ContainsFunc(who, func(w string) bool { return slices.Contains(list, w) })
	}
	switch {
	case in(s.cfg.Admins):
		return admin
	case in(s.cfg.Editors):
		return editor
	}
	return viewer
}

// need is a handler that changes the ledger, for those of a role or above:
// refused with --read-only, its body limited.
func (s *Server) need(at role, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.ReadOnly {
			http.Error(w, "lapilli serve is read-only (--read-only)", http.StatusForbidden)
			return
		}
		if s.role(r) < at {
			s.oops(w, r, forbidden{at})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		h(w, r)
	}
}

// forbidden is a change the reader's role doesn't allow.
type forbidden struct{ need role }

func (f forbidden) Error() string {
	if f.need == admin {
		return "only an admin can change this"
	}
	return "only an editor can change this"
}

// me says who the reader is, and what they may do.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"user": s.user(r), "groups": s.groups(r), "role": s.role(r).String()})
}
