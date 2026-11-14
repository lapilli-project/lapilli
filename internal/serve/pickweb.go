package serve

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Facets with counts for the lists that aren't the rules (docs/design/ui.md):
// the proposals, the tasks. As the rules list has them: values of a facet
// ORed, facets ANDed, each value counted as the list would show it picked
// alone in its facet with what the others pick; a chip for each value in
// effect; one GET form, its result a URL to keep.

// pickFacet is a facet of such a list: its parameter, its name and its
// values with their labels (texts to translate). def are the values picked
// when the URL doesn't name the facet (the open ones, say); with def, "all"
// picks none, so that a form with none ticked is all of them, not def.
// aliases are values the URL may give for some of the values (open: the
// states not finished with), and raw, values the URL may give that the
// facet doesn't offer but its items have (a state of its own, from an
// older link), each offered as it is once picked.
type pickFacet struct {
	param, name    string
	values, labels []string
	def            []string
	aliases        map[string][]string
	raw            []string
}

// picksOf reads what a list's URL picks in each facet: values it offers,
// its aliases' values, and its raw values, each once, maxValues at most;
// def when the facet isn't named. What else it gives is left out, never
// shown.
func picksOf(q url.Values, fs []pickFacet) picks {
	p := picks{}
	for _, f := range fs {
		vs, named := q[f.param]
		if !named && f.def != nil {
			p[f.param] = slices.Clone(f.def)
			continue
		}
		var got []string
		add := func(x string) {
			if !slices.Contains(got, x) && len(got) < maxValues {
				got = append(got, x)
			}
		}
		for _, v := range vs {
			v = strings.ToLower(strings.TrimSpace(v))
			switch {
			case slices.Contains(f.values, v), slices.Contains(f.raw, v):
				add(v)
			case f.aliases[v] != nil:
				for _, x := range f.aliases[v] {
					add(x)
				}
			}
		}
		p[f.param] = got
	}
	return p
}

// pickURL is a list's page, path, for what is picked, with the page
// context: a facet picking its default left out, one with a default
// picking none given as all.
func pickURL(path string, c pageCtx, fs []pickFacet, p picks) string {
	q := url.Values{}
	if c.TeamSet {
		q.Set("team", c.Team)
	}
	if c.Weeks != 0 {
		q.Set("weeks", strconv.Itoa(c.Weeks))
	}
	for _, f := range fs {
		vs := p[f.param]
		switch {
		case f.def != nil && sameSet(vs, f.def):
		case f.def != nil && len(vs) == 0:
			q.Set(f.param, "all")
		default:
			q[f.param] = slices.Clone(vs)
		}
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

// pickView is what a list shows of its facets: each, counted; a chip for
// each value in effect; the form's hidden fields and where Clear goes.
type pickView struct {
	Action string
	Facets []facet
	Chips  []chip
	Hidden []field
	Clear  string
	// Active says a filter but the facets' defaults is in effect.
	Active bool
}

// pickItems works out a list's facets over its n items, values giving an
// item's values in a facet: the items shown, in their order, and the view
// of the facets, for the list at path with the page context c.
func pickItems(path string, c pageCtx, fs []pickFacet, p picks, n int, values func(i int, param string) []string) ([]int, pickView) {
	in := func(i int, f pickFacet) bool {
		want := p[f.param]
		return len(want) == 0 || slices.ContainsFunc(values(i, f.param), func(v string) bool { return slices.Contains(want, v) })
	}
	counts := map[string]map[string]int{}
	var shown []int
	for i := 0; i < n; i++ {
		all := true
		for _, f := range fs {
			all = all && in(i, f)
		}
		if all {
			shown = append(shown, i)
		}
		for _, f := range fs {
			but := true
			for _, g := range fs {
				if g.param != f.param && !in(i, g) {
					but = false
					break
				}
			}
			if !but {
				continue
			}
			if counts[f.param] == nil {
				counts[f.param] = map[string]int{}
			}
			vs := values(i, f.param)
			for k, v := range vs {
				if !slices.Contains(vs[:k], v) { // a value an item has twice counts once
					counts[f.param][v]++
				}
			}
		}
	}
	return shown, facetView(path, c, fs, p, counts)
}

// facetView is a list's facets, each value with its count, counts[param]
// [value], and a chip for each value in effect; the form's hidden fields
// and where Clear goes, for the list at path with the page context c.
func facetView(path string, c pageCtx, fs []pickFacet, p picks, counts map[string]map[string]int) pickView {
	v := pickView{Action: path, Hidden: c.hidden()}
	for _, f := range fs {
		fc := facet{Param: f.param, Name: f.name}
		values, labels := slices.Clone(f.values), slices.Clone(f.labels)
		// A raw value picked is offered as it is, so that it can be
		// unticked.
		for _, x := range p[f.param] {
			if !slices.Contains(values, x) {
				values, labels = append(values, x), append(labels, x)
			}
		}
		for i, x := range values {
			fc.Options = append(fc.Options, facetOption{Value: x, Label: labels[i], Count: counts[f.param][x], Checked: slices.Contains(p[f.param], x)})
		}
		v.Facets = append(v.Facets, fc)
		if f.def != nil {
			// A form with none ticked is all of them, not the default.
			v.Hidden = append(v.Hidden, field{f.param, "all"})
		}
	}
	// The team's chip gives all teams.
	if c.Team != "" {
		all := c
		all.Team, all.TeamSet = "", true
		v.Chips = append(v.Chips, chip{Group: "Team", Value: c.Team, Raw: true, Href: pickURL(path, all, fs, p)})
	}
	for _, f := range fs {
		if f.def != nil && sameSet(p[f.param], f.def) {
			continue // as the page is unless asked: no chip
		}
		for _, x := range p[f.param] {
			label := x
			if i := slices.Index(f.values, x); i >= 0 {
				label = f.labels[i]
			}
			q := picks{}
			for k, vs := range p {
				q[k] = vs
			}
			q[f.param] = slices.DeleteFunc(slices.Clone(p[f.param]), func(w string) bool { return w == x })
			v.Chips = append(v.Chips, chip{Group: f.name, Value: label, Href: pickURL(path, c, fs, q)})
		}
	}
	v.Active = len(v.Chips) > 0 && !(len(v.Chips) == 1 && c.Team != "")
	// Clear: every facet picking all, the context kept.
	none := picks{}
	for _, f := range fs {
		none[f.param] = nil
	}
	v.Clear = pickURL(path, c, fs, none)
	return v
}
