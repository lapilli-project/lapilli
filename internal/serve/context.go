package serve

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// pageCtx is what a page is looked at for: a team and a period, read from
// its URL once, and carried on every link from one page to another, so
// that they hold however the reader goes (docs/design/ui.md).
type pageCtx struct {
	// Team is the team in effect, "" for all teams. TeamSet says team= was
	// in the URL: empty, it says all teams, chosen. Mine says Team is the
	// reader's own team, taken because team= wasn't in the URL.
	Team          string
	TeamSet, Mine bool
	// Weeks is 1, 4 or 12 when weeks= was in the URL, else 0.
	Weeks int
	// Teams are the teams' names, for the bar to choose from.
	Teams []string
}

// ctxKey keeps a request's page context in its context.
type ctxKey struct{}

// The pages that show one team's, and the pages that show a period of 1,
// 4 or 12 weeks: the bar offers each only where the page honours it.
var (
	teamPages  = map[string]bool{"overview": true, "rules": true, "incidents": true, "tasks": true}
	weeksPages = map[string]bool{"rules": true, "incidents": true, "rule": true}
)

// pageContext reads a request's page context: the team and weeks of its
// URL, or, with no team= in it, the reader's own team when they edit one
// team and no more.
func (s *Server) pageContext(r *http.Request) pageCtx {
	if c, ok := r.Context().Value(ctxKey{}).(pageCtx); ok {
		return c
	}
	q := r.URL.Query()
	var c pageCtx
	o, err := s.owners(r.Context())
	if err != nil {
		o = &owners{}
	}
	for _, t := range o.teams {
		c.Teams = append(c.Teams, t.Name)
	}
	if vs, ok := q["team"]; ok {
		c.TeamSet = true
		// A team no one has means all teams, as teamFilter has it.
		if _, ok := o.team(vs[0]); ok {
			c.Team = vs[0]
		}
	} else if mine := s.myTeam(r, o); mine != "" {
		c.Team, c.Mine = mine, true
	}
	switch v := q.Get("weeks"); v {
	case "1", "4", "12":
		c.Weeks, _ = strconv.Atoi(v)
	}
	return c
}

// urlContext is the team and period a URL has, as they are: for a link
// that carries them on, as the page it goes to reads them.
func urlContext(q url.Values) pageCtx {
	var c pageCtx
	if vs, ok := q["team"]; ok {
		c.Team, c.TeamSet = vs[0], true
	}
	switch v := q.Get("weeks"); v {
	case "1", "4", "12":
		c.Weeks, _ = strconv.Atoi(v)
	}
	return c
}

// myTeam is the team the reader edits when it is the one team they edit,
// and they are no editor of every team, nor an admin: "" otherwise.
func (s *Server) myTeam(r *http.Request, o *owners) string {
	teams := s.editsTeams(r)
	if len(teams) != 1 {
		return ""
	}
	return o.byID[teams[0]].Name
}

// withContext keeps a request's page context in it, and gives the request
// its view reads: with the reader's own team as team=, on the pages that
// show one, when it was taken for them.
func (s *Server) withContext(r *http.Request, name string) (*http.Request, *http.Request) {
	c := s.pageContext(r)
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, c))
	if !c.Mine || !teamPages[name] {
		return r, r
	}
	vr := r.Clone(r.Context())
	q := vr.URL.Query()
	q.Set("team", c.Team)
	vr.URL.RawQuery = q.Encode()
	return r, vr
}

// Link is a page's URL, target, with the page context: team= and weeks=,
// unless target sets them. Each pair of kv sets a parameter; an empty value
// takes it out, but for team, where empty is all teams. target is always a
// path on this server, from the code or the templates; values are escaped.
func (c pageCtx) Link(target string, kv ...any) string {
	frag := ""
	if i := strings.IndexByte(target, '#'); i >= 0 {
		target, frag = target[:i], target[i:]
	}
	p, raw, _ := strings.Cut(target, "?")
	q, _ := url.ParseQuery(raw)
	if _, ok := q["team"]; !ok && c.TeamSet {
		q.Set("team", c.Team)
	}
	if _, ok := q["weeks"]; !ok && c.Weeks != 0 {
		q.Set("weeks", strconv.Itoa(c.Weeks))
	}
	for i := 0; i+1 < len(kv); i += 2 {
		k, v := fmt.Sprint(kv[i]), fmt.Sprint(kv[i+1])
		if v == "" && k != "team" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
	}
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	return p + frag
}

// Rule is a rule's page, with the context.
func (c pageCtx) Rule(name string) string { return c.Link("/rules/" + url.PathEscape(name)) }

// Incident is an incident's page, with the context.
func (c pageCtx) Incident(id string) string { return c.Link("/incidents/" + url.PathEscape(id)) }

// hidden are the page context as a form's hidden fields, for a form that
// goes to another page.
func (c pageCtx) hidden() []field {
	var out []field
	if c.TeamSet {
		out = append(out, field{"team", c.Team})
	}
	if c.Weeks != 0 {
		out = append(out, field{"weeks", strconv.Itoa(c.Weeks)})
	}
	return out
}

// field is a form's hidden field.
type field struct{ Name, Value string }

// keepParams are a request's parameters but those named, as a form's hidden
// fields, in order, for a form that sends the page again.
func keepParams(q url.Values, but ...string) []field {
	var out []field
	for _, k := range slices.Sorted(maps.Keys(q)) {
		if slices.Contains(but, k) {
			continue
		}
		for _, v := range q[k] {
			out = append(out, field{k, v})
		}
	}
	return out
}
