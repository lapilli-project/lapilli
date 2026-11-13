package serve

import (
	"cmp"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
)

// The search in every page's bar (docs/design/ui.md): rules by name,
// incidents by id or title, teams by name. One found is opened; filters
// such as is:noisy are a search of the rules, on the rules page.

// searchMost is how many of each kind a search shows.
const searchMost = 50

type searchView struct {
	Q string
	// Rules, Incidents and Teams are those found, the closest first, up
	// to searchMost of each; the totals are of all found.
	Rules                                  []string
	Incidents                              []ledger.Incident
	Teams                                  []string
	RulesTotal, IncidentsTotal, TeamsTotal int
	// More is the rules page with the words, when more rules were found
	// than shown.
	More  string
	Empty *emptyView
	// TooLong says the search was too long to be read; Over are its words
	// past maxWords, left out.
	TooLong bool
	Over    []string
}

// Total is how many were found.
func (v searchView) Total() int { return v.RulesTotal + v.IncidentsTotal + v.TeamsTotal }

func (s *Server) searchPage(r *http.Request) (any, error) {
	ctx := r.Context()
	c := s.pageContext(r)
	st := searchOf(r.URL.Query()["q"])
	v := searchView{Q: st.Q, TooLong: st.TooLong}
	if st.Q == "" {
		return v, nil
	}
	if filters(st.All) {
		// A search of the rules, shown on the rules page with its filters
		// as the facets.
		p := url.Values{"q": {st.Q}}
		for _, f := range c.hidden() {
			p.Set(f.Name, f.Value)
		}
		p, _ = canonical(p, c.Teams)
		return nil, redirect{"/rules?" + p.Encode()}
	}
	// Each word is a part of a name; a key the rules' search doesn't take
	// (job:rate5m) is one too. Each is looked for once, in names each
	// lowered once.
	var words []string
	var asked []qword
	for _, t := range st.Words {
		w := t.Value
		if t.Key != "" {
			w = t.Key + ":" + t.Value
		}
		if lw := strings.ToLower(w); !slices.Contains(words, lw) {
			words = append(words, lw)
			asked = append(asked, qword{Value: w, Quoted: t.Quoted || strings.Contains(w, ":")})
		}
	}
	for _, t := range st.Over {
		v.Over = append(v.Over, t.String())
	}
	if len(words) == 0 {
		// Nothing to look for, such as a lone quote: no search.
		v.Q = ""
		return v, nil
	}
	found := func(lowered string) bool {
		for _, w := range words {
			if !strings.Contains(lowered, w) {
				return false
			}
		}
		return true
	}

	// The rules a rule's page knows: those that fired in the twelve weeks
	// kept, and those in effect.
	t, err := s.tallies(ctx, weeks)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, w := range t.weeks {
		for id := range t.by[w] {
			names[t.rules[id].Name] = true
		}
	}
	if latest, ok, err := s.l.Latest(ctx); err != nil {
		return nil, err
	} else if ok {
		for _, n := range latest.Names {
			if n != "" {
				names[n] = true
			}
		}
	}
	for n := range names {
		if found(strings.ToLower(n)) {
			v.Rules = append(v.Rules, n)
		}
	}
	v.RulesTotal = len(v.Rules)
	closest(v.Rules, st.Q)
	if len(v.Rules) > searchMost {
		v.Rules = v.Rules[:searchMost]
		// The rules page, for every team, reads the words as words.
		v.More = c.Link("/rules", "q", asWritten(asked), "team", "")
	}

	// The incidents of the twelve weeks, and any by its id.
	incs, err := s.l.Incidents(ctx, s.l.Midnight(t.weeks[0]), s.now().Add(24*time.Hour), true)
	if err != nil {
		return nil, err
	}
	if in, err := s.l.IncidentByID(ctx, st.Q); err == nil {
		if !slices.ContainsFunc(incs, func(x ledger.Incident) bool { return x.ID == in.ID }) {
			incs = append(incs, in)
		}
	} else if !errors.Is(err, ledger.ErrNotFound) {
		return nil, err
	}
	for _, in := range incs {
		in = shown(lang(r), in)
		if found(strings.ToLower(in.ID + " " + in.Title)) {
			v.Incidents = append(v.Incidents, in)
		}
	}
	v.IncidentsTotal = len(v.Incidents)
	lq, ranks := strings.ToLower(st.Q), map[string]int{}
	for _, in := range v.Incidents {
		ranks[in.ID] = rankLowered(strings.ToLower(in.ID), lq)
	}
	slices.SortStableFunc(v.Incidents, func(a, b ledger.Incident) int {
		return cmp.Or(cmp.Compare(ranks[a.ID], ranks[b.ID]), b.Start.Compare(a.Start))
	})
	v.Incidents = v.Incidents[:min(len(v.Incidents), searchMost)]

	for _, name := range c.Teams {
		if found(strings.ToLower(name)) {
			v.Teams = append(v.Teams, name)
		}
	}
	v.TeamsTotal = len(v.Teams)
	closest(v.Teams, st.Q)
	v.Teams = v.Teams[:min(len(v.Teams), searchMost)]

	switch {
	case v.Total() == 0:
		v.Empty = &emptyView{Kind: "notfound", Title: "Nothing found.",
			Text:  "No rule, incident or team has that in its name. Try fewer letters, or search the rules with a filter, such as is:noisy.",
			Links: []emptyLink{{"All rules", c.Link("/rules")}, {"All incidents", c.Link("/incidents")}}}
	case v.Total() == 1:
		// The one found, opened; but a page whose path a redirect would
		// read as . or .., which it would make another page's.
		switch {
		case len(v.Rules) == 1 && !dots(v.Rules[0]):
			return nil, redirect{c.Rule(v.Rules[0])}
		case len(v.Incidents) == 1 && !dots(v.Incidents[0].ID):
			return nil, redirect{c.Incident(v.Incidents[0].ID)}
		case len(v.Teams) == 1:
			return nil, redirect{c.Link("/", "team", v.Teams[0])}
		}
	}
	return v, nil
}

// dots reports whether a name is a path's . or .., which a path made of it
// would lose.
func dots(name string) bool { return name == "." || name == ".." }

// rankLowered is how close a name is to what was searched for, both
// lowered: the same, ahead, then anywhere.
func rankLowered(n, q string) int {
	switch {
	case n == q:
		return 0
	case strings.HasPrefix(n, q):
		return 1
	}
	return 2
}

// closest puts names in order of how close each is, then by name: each
// lowered once.
func closest(names []string, q string) {
	q = strings.ToLower(q)
	rank := make(map[string]int, len(names))
	for _, n := range names {
		rank[n] = rankLowered(strings.ToLower(n), q)
	}
	slices.SortFunc(names, func(a, b string) int { return cmp.Or(cmp.Compare(rank[a], rank[b]), strings.Compare(a, b)) })
}
