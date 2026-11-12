package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"lapilli.dev/internal/ledger"
)

// Roles: who may change what. Without --editors or --admins, everyone may
// change everything when lapilli serve listens on loopback alone, or with
// --everyone-admin; listening beyond it, or with a sign-in of its own
// (--oidc-issuer, --proxy-jwt), everyone may only look. With either list,
// whoever the sign-in names (--user-header, a session, a bearer token, a
// proxy's signed header), or a group it names them in (--groups-header,
// --groups-claim), gets the role the lists give them, and everyone else
// may only look.
//
//	viewer  looks
//	editor  writes incidents, tasks, comments, quiet windows, notes on rules;
//	        proposes changes to rules
//	admin   also settings and teams
//
// Approving a change to a rule is a role of its own, --approvers, or the
// admins when it isn't given (see approver).
//
// With --admins, admins can also give an editor's role in the UI, of every
// team or of one (docs/design/serveops.md): kept in the ledger, on top of
// the flags, and never more than an editor's. A team's editors change only
// what is their team's: its tasks, quiet windows and reports, notes on its
// rules, comments on what is its.
type role int

const (
	viewer role = iota
	editor
	admin
)

func (r role) String() string { return [...]string{"viewer", "editor", "admin"}[r] }

// groups are the groups the sign-in proxy says the reader is in, separated
// by commas: a group's name can have spaces in it, and "Platform Admins"
// isn't "Admins".
func (s *Server) groups(r *http.Request) []string {
	if s.in != nil {
		if w, ok := whoOf(r); ok {
			return w.Groups
		}
		return nil
	}
	if s.cfg.GroupsHeader == "" || len(r.Header.Values(s.cfg.GroupsHeader)) != 1 {
		return nil
	}
	var out []string
	for _, g := range strings.Split(r.Header.Get(s.cfg.GroupsHeader), ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// role is what the reader may do.
func (s *Server) role(r *http.Request) role {
	switch {
	case s.cfg.ReadOnly:
		return viewer
	case len(s.cfg.Editors) == 0 && len(s.cfg.Admins) == 0:
		// No roles: what anyone who reaches it may do. A listener beyond
		// loopback is reached by whatever shares its network, every pod in
		// a cluster, which shouldn't change the ledger unasked.
		if s.open.Load() {
			return admin
		}
		return viewer
	}
	// Whoever the proxy didn't name may only look, whatever the lists say.
	if s.user(r) == "anonymous" {
		return viewer
	}
	switch {
	case s.listed(r, s.cfg.Admins):
		return admin
	case s.listed(r, s.cfg.Editors):
		return editor
	}
	if every, _ := s.granted(r); every {
		return editor
	}
	return viewer
}

// rolesKept reports whether admins can give roles in the UI: roles are on,
// with someone to give them (--admins), and the proxy names who is who.
func (s *Server) rolesKept() bool {
	return !s.cfg.ReadOnly && len(s.cfg.Admins) > 0 && s.namesPeople()
}

// grantsCache keeps the roles the ledger gives until it changes.
type grantsCache struct {
	sync.Mutex
	gen int64
	gs  []ledger.Grant
}

// grants are the roles the ledger gives; none when roles aren't kept, or
// they can't be read (the reader is then given none).
func (s *Server) grants(ctx context.Context) []ledger.Grant {
	if !s.rolesKept() {
		return nil
	}
	gen := s.l.Gen()
	s.grantsC.Lock()
	defer s.grantsC.Unlock()
	if s.grantsC.gs != nil && s.grantsC.gen == gen {
		return s.grantsC.gs
	}
	gs, err := s.l.Grants(ctx)
	if err != nil {
		s.log.Warn("the roles kept in the ledger can't be read: no one is given one", "err", err)
		return nil
	}
	if gs == nil {
		gs = []ledger.Grant{}
	}
	s.grantsC.gen, s.grantsC.gs = gen, gs
	return gs
}

// granted are the roles the ledger gives the reader: an editor of every
// team, and the teams they edit. Whoever the proxy didn't name has none.
func (s *Server) granted(r *http.Request) (every bool, teams []int64) {
	user := s.user(r)
	if user == "anonymous" || !s.rolesKept() {
		return false, nil
	}
	groups := s.groups(r)
	for _, g := range s.grants(r.Context()) {
		kind, name, _ := strings.Cut(g.Who, ":")
		if !(kind == "user" && name == user || kind == "group" && slices.Contains(groups, name)) {
			continue // another's, or not a grant the ledger should hold
		}
		if g.TeamID == 0 {
			every = true
		} else if !slices.Contains(teams, g.TeamID) {
			teams = append(teams, g.TeamID)
		}
	}
	slices.Sort(teams)
	return every, teams
}

// editsTeams are the teams the reader edits, when they are no editor of
// every team: nil for an editor or an admin, and for a viewer.
func (s *Server) editsTeams(r *http.Request) []int64 {
	if s.cfg.ReadOnly || s.role(r) >= editor {
		return nil
	}
	_, teams := s.granted(r)
	return teams
}

// listed reports whether the reader is in a list of users and groups.
// "user:NAME" is a person alone, "group:NAME" a group alone: one who could
// pick their own name can't take a group's. A name with neither is either,
// as before. "*@DOMAIN" is everyone whose name ends in @DOMAIN, in any
// letter case. Whoever the sign-in didn't name is in none.
func (s *Server) listed(r *http.Request, list []string) bool {
	user := s.user(r)
	if user == "anonymous" {
		return false
	}
	groups := s.groups(r)
	return slices.ContainsFunc(list, func(x string) bool {
		if name, ok := strings.CutPrefix(x, "user:"); ok {
			return name == user
		}
		if name, ok := strings.CutPrefix(x, "group:"); ok {
			return slices.Contains(groups, name)
		}
		if domain, ok := strings.CutPrefix(x, "*@"); ok {
			at := strings.LastIndexByte(user, '@')
			return domain != "" && at > 0 && strings.EqualFold(user[at+1:], domain)
		}
		return x == "*" || x == user || slices.Contains(groups, x)
	})
}

// letIn reports whether the reader may come in at all: with a sign-in of
// lapilli's own, or --viewers, only those a role list names (--viewers,
// --editors, --admins, --approvers) or the UI gave a role. An identity
// provider signs in whoever it knows, which for some (a public one, a
// directory with guests) is anyone.
func (s *Server) letIn(r *http.Request) bool {
	if s.in == nil && len(s.cfg.Viewers) == 0 {
		return true
	}
	if s.listed(r, s.cfg.Viewers) {
		return true
	}
	// '*' in a list that gives a role is everyone --viewers lets in, not
	// everyone signed in.
	for _, list := range [][]string{s.cfg.Editors, s.cfg.Admins, s.cfg.Approvers} {
		if s.listed(r, slices.DeleteFunc(slices.Clone(list), func(x string) bool { return x == "*" })) {
			return true
		}
	}
	every, teams := s.granted(r)
	return every || len(teams) > 0
}

// restricted reports whether --viewers names who may look, rather than
// everyone signed in.
func restricted(viewers []string) bool {
	return len(viewers) > 0 && !slices.Contains(viewers, "*")
}

// CheckRoles says what is wrong with the role lists, if anything: with a
// sign-in of lapilli's own, someone must be let in, and "*" (everyone the
// identity provider signs in) may give a role to change only when
// --viewers says who may come in at all.
func CheckRoles(c Config) error {
	if c.OIDC == nil && c.Proxy == nil {
		return nil
	}
	if len(c.Viewers)+len(c.Editors)+len(c.Admins)+len(c.Approvers) == 0 {
		return errors.New("a sign-in of lapilli's own lets in only those a role names: give --viewers (people, group:NAME or *@DOMAIN), --editors or --admins")
	}
	if !restricted(c.Viewers) {
		for flag, list := range map[string][]string{"--editors": c.Editors, "--admins": c.Admins, "--approvers": c.Approvers} {
			if slices.Contains(list, "*") {
				return fmt.Errorf("%s '*' gives a role to everyone the identity provider signs in: say who may come in with --viewers (people, group:NAME or *@DOMAIN) first", flag)
			}
		}
	}
	return nil
}

// notLetIn is a person signed in whom no role lets in. Who is said where
// the refusal is written for them (notLetInText), and never logged.
type notLetIn struct{}

func (notLetIn) Error() string { return "lapilli doesn't let you in: ask its admins to name you" }

// notLetInText is the refusal of a person no role lets in, in a language.
const notLetInText = "you are signed in as %s, whom lapilli doesn't let in: ask its admins to name you"

// need is a handler that changes the ledger, for those of a role or above:
// refused with --read-only, its body limited.
func (s *Server) need(at role, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.ReadOnly {
			http.Error(w, "lapilli serve is read-only (--read-only)", http.StatusForbidden)
			return
		}
		if s.role(r) < at {
			s.oops(w, r, s.forbidden(at))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		h(w, r)
	}
}

// teamWrite is a handler that changes the ledger, for editors, and for a
// team's editors limited to what is their team's: the ledger refuses, in
// the write itself, a row of another team's or of everyone's.
func (s *Server) teamWrite(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.ReadOnly {
			http.Error(w, "lapilli serve is read-only (--read-only)", http.StatusForbidden)
			return
		}
		if s.role(r) < editor {
			teams := s.editsTeams(r)
			if len(teams) == 0 {
				s.oops(w, r, s.forbidden(editor))
				return
			}
			r = r.WithContext(ledger.WithTeams(r.Context(), teams))
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		h(w, r)
	}
}

// scoped reports whether a request is a team's editor's: limited to their
// teams, which it gives.
func scoped(r *http.Request) ([]int64, bool) { return ledger.TeamsOf(r.Context()) }

// forbidden is a change the reader's role doesn't allow.
// forbidden is a change the reader's role doesn't allow; closed, one no
// one may make, as there are no roles and the server listens beyond
// loopback.
type forbidden struct {
	need             role
	closed, signedIn bool
}

func (f forbidden) Error() string {
	switch {
	case f.closed && f.signedIn:
		return "no one changes anything here: lapilli serve signs people in, with no roles. Name who may make changes in --editors or --admins"
	case f.closed:
		return "no one changes anything here: lapilli serve listens beyond this machine with no roles. Give roles behind a sign-in proxy (--user-header with --editors or --admins), or --everyone-admin"
	case f.need == admin:
		return "only an admin can change this"
	}
	return "only an editor can change this"
}

// forbidden is the refusal of a change that needs a role the reader hasn't.
func (s *Server) forbidden(at role) forbidden {
	return forbidden{need: at, closed: !s.cfg.ReadOnly && len(s.cfg.Editors)+len(s.cfg.Admins) == 0 && !s.open.Load(), signedIn: s.in != nil}
}

// LoopbackOnly reports whether an address to listen on is this machine's
// alone: a loopback address, or localhost. ":8080", any address, isn't.
func LoopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// listening is told where the UI's listener was bound. Without roles,
// everyone is an admin only when --listen said loopback and the address
// it was bound to is loopback: localhost is what the resolver says, and
// /etc/hosts can make it another. It then says who may change what.
func (s *Server) listening(a net.Addr) {
	if !s.cfg.EveryoneAdmin || s.in != nil {
		tcp, ok := a.(*net.TCPAddr)
		s.open.Store(s.open.Load() && ok && tcp.IP.IsLoopback() && s.in == nil)
	}
	s.logWho()
}

// logWho says at start who may change what.
func (s *Server) logWho() {
	switch {
	case s.cfg.ReadOnly:
		s.log.Info("--read-only: no one changes anything through the UI or the API")
	case len(s.cfg.Editors)+len(s.cfg.Admins) > 0:
		s.log.Info("roles: those --editors and --admins name, as the sign-in names them, change what their role allows; everyone else may only look")
	case s.in != nil:
		s.log.Info("a sign-in and only --viewers: those it names may look, and no one changes anything. To take changes, name who may make them in --editors or --admins")
	case s.cfg.EveryoneAdmin:
		s.log.Warn("--everyone-admin: everyone who reaches the UI and the API changes anything, as an admin, with no sign-in", "listen", s.cfg.Listen)
	case s.open.Load():
		s.log.Info("no roles: everyone who reaches the UI and the API changes anything, as an admin; it listens on this machine alone", "listen", s.cfg.Listen)
	default:
		s.log.Warn("no roles, listening beyond this machine: everyone may only look. To take changes, give roles behind a sign-in proxy (--user-header with --editors or --admins), or --everyone-admin", "listen", s.cfg.Listen)
	}
}

// me says who the reader is, and what they may do.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	me := map[string]any{"user": s.user(r), "groups": s.groups(r), "role": s.role(r).String()}
	if teams := s.editsTeams(r); len(teams) > 0 {
		names := []string{}
		if o, err := s.owners(r.Context()); err == nil {
			for _, id := range teams {
				if t, ok := o.byID[id]; ok {
					names = append(names, t.Name)
				}
			}
		}
		me["teams"] = names
	}
	json.NewEncoder(w).Encode(me)
}

// ruleInScope refuses a team's editor a note on a rule their team doesn't
// own, as the pages work owners out.
func (s *Server) ruleInScope(r *http.Request, rule string) error {
	teams, limited := scoped(r)
	if !limited {
		return nil
	}
	o, err := s.owners(r.Context())
	if err != nil {
		return err
	}
	if id := o.rule[rule]; id == 0 || !slices.Contains(teams, id) {
		return ledger.ErrNotYourTeam
	}
	return nil
}

// commentInScope refuses a team's editor a comment on what isn't their
// team's: a rule it doesn't own, another team's task, an incident it
// doesn't own. A comment changed or deleted is checked by what it is on.
func (s *Server) commentInScope(r *http.Request, id int64, on, key string) error {
	teams, limited := scoped(r)
	if !limited {
		return nil
	}
	ctx := r.Context()
	if id != 0 {
		c, err := s.l.CommentByID(ctx, id)
		if err != nil {
			return err
		}
		on, key = c.On, c.Key
	}
	o, err := s.owners(ctx)
	if err != nil {
		return err
	}
	switch on {
	case "rule":
		if t := o.rule[key]; t != 0 && slices.Contains(teams, t) {
			return nil
		}
	case "task":
		if n, err := strconv.ParseInt(key, 10, 64); err == nil {
			if t, err := s.l.TaskByID(ctx, n); err == nil && slices.Contains(teams, t.TeamID) {
				return nil
			}
		}
	case "incident":
		if in, err := s.l.IncidentByID(ctx, key); err == nil {
			for _, id := range teams {
				if t, ok := o.byID[id]; ok && o.ownsIncident(t, in) {
					return nil
				}
			}
		}
	}
	return ledger.ErrNotYourTeam
}

// changesSeen are the changes a reader may see: who was given a role, or
// had one taken away, is an admin's to read, as the roles page is.
func (s *Server) changesSeen(r *http.Request, cs []ledger.Change) []ledger.Change {
	if s.role(r) == admin {
		return cs
	}
	return slices.DeleteFunc(cs, func(c ledger.Change) bool {
		return strings.HasPrefix(c.What, ledger.RoleGiven) || strings.HasPrefix(c.What, ledger.RoleTaken)
	})
}
