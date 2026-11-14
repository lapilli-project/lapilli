package serve

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"lapilli.dev/internal/ledger"
)

// Many at once (docs/design/ui.md): a form on the rules list that makes a
// task for each rule picked, or sets their team or what is to become of
// them, and one on the firings that writes each one picked down as real.
// Each item is the write the same form makes for one, with its checks,
// in turn: what one by one would refuse is refused, and counted, and the
// changes log has a line for each.

// maxBulk is how many items one bulk write takes.
const maxBulk = 100

func (s *Server) bulkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /rules/bulk", s.teamWrite(s.bulkRules))
	mux.HandleFunc("POST /firings/real", s.write(s.bulkReal))
}

// bulkKinds are what a bulk write does, as its result says: a task for
// each rule, a team for each, a disposition for each, an incident from
// each firing.
var bulkKinds = []string{"task", "team", "status", "real"}

// bulkResult is what a bulk write did, as the page it goes back to says it
// once: how many were done; of the rest, how many weren't the reader's
// team's, how many were refused for what they are, and, of firings, how
// many had caught something already. None says nothing was picked.
type bulkResult struct {
	Done, Theirs, Refused, Already int
	None                           bool
}

// bulkParams are the parameters a bulk write's result is said by.
var bulkParams = []string{"bulk", "done", "theirs", "refused", "already"}

// bulkOf is the result a page was sent back with, read within bounds:
// nil when there is none, or what is there isn't one.
func bulkOf(q url.Values) *bulkResult {
	if !slices.Contains(bulkKinds, q.Get("bulk")) {
		return nil
	}
	n := func(k string) (int, bool) {
		v := q.Get(k)
		if v == "" {
			return 0, true
		}
		x, err := strconv.Atoi(v)
		return x, err == nil && x >= 0 && x <= maxBulk
	}
	var b bulkResult
	var ok [4]bool
	b.Done, ok[0] = n("done")
	b.Theirs, ok[1] = n("theirs")
	b.Refused, ok[2] = n("refused")
	b.Already, ok[3] = n("already")
	if slices.Contains(ok[:], false) || b.Done+b.Theirs+b.Refused+b.Already > maxBulk {
		return nil
	}
	b.None = b.Done+b.Theirs+b.Refused+b.Already == 0
	return &b
}

// bulkBack is where a bulk write goes back to: the page it came from,
// back, when it is a page of path on this server, else path, with what was
// done said in its query.
func bulkBack(back, path, kind string, b bulkResult) string {
	u, err := url.Parse(safeBack(back, path))
	if err != nil || u.Path != path {
		u = &url.URL{Path: path}
	}
	q := u.Query()
	for _, k := range bulkParams {
		q.Del(k)
	}
	q.Set("bulk", kind)
	for k, n := range map[string]int{"done": b.Done, "theirs": b.Theirs, "refused": b.Refused, "already": b.Already} {
		if n > 0 {
			q.Set(k, strconv.Itoa(n))
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// picked are the values a form picked, each once, at most maxBulk of them
// (ok false past that), the empty ones left out.
func picked(vs []string) ([]string, bool) {
	var out []string
	for _, v := range vs {
		if v != "" && !slices.Contains(out, v) {
			if len(out) == maxBulk {
				return nil, false
			}
			out = append(out, v)
		}
	}
	return out, true
}

// count adds a write's outcome to a result: done, the reader's team's not,
// or refused for what it is. An error of another kind is returned, to stop
// at, as the write alone would.
func (b *bulkResult) count(err error) error {
	var inv ledger.ErrInvalid
	switch {
	case err == nil:
		b.Done++
	case errors.Is(err, ledger.ErrNotYourTeam):
		b.Theirs++
	case errors.As(err, &inv), errors.Is(err, ledger.ErrNotFound):
		b.Refused++
	default:
		return err
	}
	return nil
}

// bulkRules is the rules list's bulk write: for each rule picked, the task
// its page's "Add a task" makes (to tune it, its team's), or its note with
// the team, or what is to become of it, set, the rest as it is: the
// note's form, with what the rule's page shows in it.
func (s *Server) bulkRules(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	rules, ok := picked(r.PostForm["rule"])
	if !ok {
		s.oops(w, r, bad("at most %d rules at a time: pick fewer", maxBulk))
		return
	}
	ctx := r.Context()
	do := r.PostFormValue("do")
	var team int64
	status := r.PostFormValue("status")
	switch do {
	case "task":
	case "team":
		team = formInt(r, "team")
		if team != 0 {
			if _, err := s.teamByID(ctx, team); err != nil {
				s.oops(w, r, bad("no such team"))
				return
			}
		}
	case "status":
		if status != "" && !slices.Contains(ledger.Statuses, status) {
			s.oops(w, r, bad("status %q: want keep, tune, retire or none", status))
			return
		}
	default:
		s.oops(w, r, bad("what to do with the rules picked: a task, a team or a disposition"))
		return
	}
	o, err := s.owners(ctx)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	notes, err := s.l.Notes(ctx)
	if err != nil {
		s.oops(w, r, err)
		return
	}
	var res bulkResult
	by, lg := s.user(r), lang(r)
	for _, rule := range rules {
		var err error
		switch do {
		case "task":
			// As "Add a task" on its page, and as the task's form takes it.
			_, err = s.l.PutTask(ctx, ledger.Task{Title: tr(lg, "Tune %s", rule), Kind: "tune", Rule: strings.TrimSpace(rule),
				TeamID: o.rule[strings.TrimSpace(rule)]}, by)
		default:
			n := notes[rule]
			n.Rule = rule
			if do == "team" {
				n.TeamID = team
			} else {
				n.Status = status
			}
			if err = s.ruleInScope(r, rule); err == nil {
				err = s.l.PutNote(ctx, n, by)
			}
		}
		if err := res.count(err); err != nil {
			s.oops(w, r, err)
			return
		}
	}
	if do != "task" && res.Done > 0 {
		s.poke() // who owns a rule may have changed: a team's windows are judged again
	}
	back(w, r, bulkBack(r.PostFormValue("back"), "/rules", do, res))
}

// bulkReal is the firings' bulk write: each firing picked that caught
// nothing written down as an incident, as its own "This was real" does;
// one that has caught something by its turn, one picked before it among
// them, is left as it is, and counted.
func (s *Server) bulkReal(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	vs, ok := picked(r.PostForm["firing"])
	if !ok {
		s.oops(w, r, bad("at most %d firings at a time: pick fewer", maxBulk))
		return
	}
	var ids []int64
	for _, v := range vs {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			s.oops(w, r, bad("no such firing"))
			return
		}
		ids = append(ids, id)
	}
	var res bulkResult
	for _, id := range ids {
		made, _, err := s.realOne(r, id)
		switch {
		case errors.Is(err, ledger.ErrNotFound):
			res.Refused++
		case err != nil:
			s.oops(w, r, err)
			return
		case made == "":
			res.Already++
		default:
			res.Done++
		}
	}
	back(w, r, bulkBack(r.PostFormValue("back"), "/firings", "real", res))
}

// bulkView is what a list's bulk form offers the reader: May, whether
// they may use it at all, and if not, Why; Team, whether they may set a
// rule's team, and if not, TeamWhy; Teams, the teams to pick from; Result,
// what the last bulk write did, said once.
type bulkView struct {
	May          bool
	Why, TeamWhy string
	Team         bool
	Teams        []ledger.Team
	Result       *bulkResult
	Back         string
}

// bulkFor is what a bulk form offers the reader of r: all to an editor;
// to a team's editor, what their team's rules are, not their team.
// writers says who may use it: "editor", or "team" for a team's editors too.
func (s *Server) bulkFor(r *http.Request, writers string) bulkView {
	var v bulkView
	switch role := s.role(r); {
	case role >= editor:
		v.May, v.Team = true, true
	case writers == "team" && len(s.editsTeams(r)) > 0:
		v.May, v.TeamWhy = true, "Editors only"
	default:
		v.Why = cmpOr(s.why(r), "Editors only")
		v.TeamWhy = v.Why
	}
	v.Result = bulkOf(r.URL.Query())
	return v
}
