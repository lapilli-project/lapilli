package serve

import (
	"net/http"
	"net/url"
	"slices"

	"lapilli.dev/internal/ledger"
)

// The Cleanup page, /orphans (docs/design/ui.md, Cleanup), under
// Administration: the rule names no rule in effect has any more, a rule
// deleted or renamed, that the ledger still keeps a note, tasks, comments
// or open proposals of, which no other list shows. An editor moves what one kept to a rule in effect (for a
// rename), or deletes it; its proposals stay, each made against the rule
// as it was, to be dropped on its page.

func (s *Server) orphansRoutes(mux *http.ServeMux, page func(string, func(*http.Request) (any, error)) http.HandlerFunc) {
	mux.HandleFunc("GET /orphans", page("orphans", s.orphansPage))
	mux.HandleFunc("POST /orphans/move", s.write(s.postOrphanMove))
	mux.HandleFunc("POST /orphans/delete", s.write(s.postOrphanDelete))
}

// orphansShown is how many names the page shows at once, the next after a
// link; datalistMost, how many rules in effect it offers to move to, past
// which the name is typed.
const (
	orphansShown = 100
	datalistMost = 2000
)

type orphansView struct {
	// Known says which rules are in effect is known: a version of them is
	// recorded.
	Known bool
	Lines []ledger.RuleKept
	// Total is how many names there are, From and Upto the first and the
	// last shown (1 on), and Next the page after, "" for none.
	Total, From, Upto int
	Next              string
	// Rules are the rules in effect to move to, when there are no more
	// than datalistMost.
	Rules []string
	// Moved and Deleted are what the last write did, from the query.
	Moved, To, Deleted string
	// ImpactNotes are the impact windows read again with another
	// criterion, and covered by nothing found now, that tasks or comments
	// are kept on (ledger.OrphanedImpact).
	ImpactNotes []ledger.ImpactOrphan
	// Verdicts are people's verdicts to look at again
	// (docs/design/judging.md, "Through judging again"): the latest first.
	Verdicts []ledger.Verdict
}

// inEffectNames are the names of the rules in effect, as the latest
// version records them; ok is false before one is.
func (s *Server) inEffectNames(r *http.Request) (map[string]bool, []string, bool, error) {
	latest, ok, err := s.l.Latest(r.Context())
	if err != nil || !ok {
		return nil, nil, false, err
	}
	set := make(map[string]bool, len(latest.Names))
	var names []string
	for _, n := range latest.Names {
		if n != "" && !set[n] {
			set[n] = true
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return set, names, true, nil
}

func (s *Server) orphansPage(r *http.Request) (any, error) {
	ctx := r.Context()
	q := r.URL.Query()
	v := orphansView{Moved: q.Get("moved"), To: q.Get("to"), Deleted: q.Get("deleted")}
	var err error
	if v.ImpactNotes, err = s.l.OrphanedImpact(ctx); err != nil {
		return v, err
	}
	if v.Verdicts, _, err = s.l.MovedVerdicts(ctx, ledger.MaxMovedShown, s.now()); err != nil {
		return v, err
	}
	set, names, ok, err := s.inEffectNames(r)
	if err != nil || !ok {
		return v, err
	}
	v.Known = true
	if len(names) <= datalistMost {
		v.Rules = names
	}
	// The names read one at a time, in order: each counted, the page's
	// kept, the rest let go.
	after := q.Get("after")
	var page []string
	more := false
	err = s.l.KeptNames(ctx, func(name string) bool {
		if set[name] {
			return true
		}
		v.Total++
		switch {
		case after != "" && name <= after:
			v.From = v.Total + 1
		case len(page) < orphansShown:
			page = append(page, name)
		default:
			more = true
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if v.From == 0 && len(page) > 0 {
		v.From = 1
	}
	if more {
		v.Next = page[len(page)-1]
	}
	if len(page) > 0 {
		v.Upto = v.From + len(page) - 1
	}
	v.Lines, err = s.l.KeptOf(ctx, page)
	return v, err
}

// errInEffect is a name a rule in effect has: what it keeps is its own;
// errNotInEffect, a name to move to no rule in effect has.
var (
	errInEffect    = bad("a rule in effect has that name: what it keeps is its own")
	errNotInEffect = bad("no rule in effect has that name: move to one of the rules in effect")
)

// orphanOf checks a name is no rule's in effect, and, when to is given,
// that to is one's.
func (s *Server) orphanOf(r *http.Request, from, to string) error {
	// A name the ledger could keep a note of: 500 bytes at most, nothing in
	// it that doesn't show; the ledger refuses one it keeps nothing of.
	if len(from) > 500 || !ledger.Plain(from) || len(to) > 500 || !ledger.Plain(to) {
		return bad("a rule's name is 500 bytes at most, with nothing in it that doesn't show")
	}
	set, _, ok, err := s.inEffectNames(r)
	switch {
	case err != nil:
		return err
	case !ok:
		return bad("the rules haven't been read yet: which names are gone isn't known")
	case from == "" || set[from]:
		return errInEffect
	case to != "" && !set[to]:
		return errNotInEffect
	}
	return nil
}

func (s *Server) postOrphanMove(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	from, to := r.PostFormValue("from"), r.PostFormValue("to")
	if to == "" {
		s.oops(w, r, errNotInEffect)
		return
	}
	err := s.orphanOf(r, from, to)
	if err == nil {
		err = s.l.MoveRuleKept(r.Context(), from, to, s.user(r))
		s.poke() // who owns a rule may have changed: a team's windows are judged again
	}
	s.written(w, r, err, "/orphans?"+url.Values{"moved": {from}, "to": {to}}.Encode())
}

func (s *Server) postOrphanDelete(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		s.oops(w, r, err)
		return
	}
	rule := r.PostFormValue("rule")
	err := s.orphanOf(r, rule, "")
	if err == nil {
		err = s.l.DeleteRuleKept(r.Context(), rule, s.user(r))
		s.poke()
	}
	s.written(w, r, err, "/orphans?"+url.Values{"deleted": {rule}}.Encode())
}
