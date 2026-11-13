package serve

import (
	"net/url"
	"slices"
	"strings"
	"unicode"
)

// The rules' search box (docs/design/ui.md): words, each a part of a rule's
// name, and filters written key:value. The filters a facet has, and the
// team, are made the URL's parameters, so the facets show them and a chip
// takes each away; label: and group: stay in the box.

// queryKeys are the keys the search box takes, in the order its help names
// them.
var queryKeys = []string{"team", "severity", "is", "status", "has", "label", "group"}

// The facets' values: performance (judged), trust (the backtest), the
// rule's disposition (its note), and what is under way.
var (
	perfValues   = []string{"noisy", "ok", "silent", "spare"}
	trustValues  = []string{"differs", "unreplayed", "recorded"}
	statusValues = []string{"keep", "tune", "retire", "none"}
	hasValues    = []string{"proposal", "facts"}
)

// isValues are what is: takes, and the facet and value each is.
var isValues = map[string][2]string{
	"noisy": {"perf", "noisy"}, "ok": {"perf", "ok"}, "silent": {"perf", "silent"}, "spare": {"perf", "spare"},
	"differs": {"trust", "differs"}, "unreplayed": {"trust", "unreplayed"}, "recorded": {"trust", "recorded"},
	"proposed": {"has", "proposal"}, "facts": {"has", "facts"},
}

// qword is a word of the search box: Key "" for a part of a name. Bad says
// why it isn't understood: "key", a key the box doesn't take; "value", a
// value its key doesn't.
type qword struct {
	Key, Value string
	Quoted     bool
	Bad        string
}

// String is the word as written in the box.
func (t qword) String() string {
	switch {
	case t.Key != "":
		return t.Key + ":" + t.Value
	case t.Quoted || strings.ContainsFunc(t.Value, unicode.IsSpace):
		return `"` + t.Value + `"`
	}
	return t.Value
}

// tokens are the words of a search: "a phrase in quotes" is one, and a
// part of a name, whatever it holds; a word key:value is a filter when key
// is letters alone (a name such as job:rate5m is written in quotes).
func tokens(q string) []qword {
	var out []qword
	rs := []rune(q)
	for i := 0; i < len(rs); {
		switch {
		case unicode.IsSpace(rs[i]):
			i++
			continue
		case rs[i] == '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				j++
			}
			if v := strings.TrimSpace(string(rs[i+1 : j])); v != "" {
				out = append(out, qword{Value: v, Quoted: true})
			}
			i = j + 1
			continue
		}
		j := i
		for j < len(rs) && !unicode.IsSpace(rs[j]) {
			j++
		}
		out = append(out, word(string(rs[i:j])))
		i = j
	}
	return out
}

// word is a word of a search, unquoted.
func word(w string) qword {
	k, v, ok := strings.Cut(w, ":")
	if !ok || k == "" || strings.ContainsFunc(k, func(r rune) bool { return !unicode.IsLetter(r) }) {
		return qword{Value: w}
	}
	t := qword{Key: strings.ToLower(k), Value: v}
	switch {
	case !slices.Contains(queryKeys, t.Key):
		t.Bad = "key"
	case v == "":
		t.Bad = "value"
	case t.Key == "is" && isValues[strings.ToLower(v)] == [2]string{}:
		t.Bad = "value"
	case t.Key == "status" && !slices.Contains(statusValues, strings.ToLower(v)):
		t.Bad = "value"
	case t.Key == "has" && !slices.Contains(hasValues, strings.ToLower(v)):
		t.Bad = "value"
	case t.Key == "label":
		if lk, lv, ok := strings.Cut(v, "="); !ok || lk == "" || lv == "" {
			t.Bad = "value"
		}
	}
	return t
}

// filters reports whether a search has filters for the rules in it: what
// the search of everything hands to the rules page.
func filters(ts []qword) bool {
	return slices.ContainsFunc(ts, func(t qword) bool { return t.Key != "" && t.Bad != "key" })
}

// canonical is a rules page's parameters with the search box's filters for
// the facets, and the team, made parameters: the box keeps the words, the
// labels and groups, and what it doesn't understand, to say so. changed is
// false when there was nothing to move. teams are the teams' names: a team
// no one has stays in the box.
func canonical(q url.Values, teams []string) (out url.Values, changed bool) {
	out = url.Values{}
	for k, vs := range q {
		out[k] = slices.Clone(vs)
	}
	add := func(k, v string) {
		if !slices.Contains(out[k], v) {
			out[k] = append(out[k], v)
		}
	}
	var kept []string
	for _, t := range tokens(q.Get("q")) {
		if t.Bad != "" {
			kept = append(kept, t.String())
			continue
		}
		switch t.Key {
		case "team":
			if !slices.Contains(teams, t.Value) {
				kept = append(kept, t.String())
				continue
			}
			out.Set("team", t.Value)
		case "severity":
			add("severity", t.Value)
		case "is":
			fv := isValues[strings.ToLower(t.Value)]
			add(fv[0], fv[1])
		case "status", "has":
			add(t.Key, strings.ToLower(t.Value))
		default:
			kept = append(kept, t.String())
			continue
		}
		changed = true
	}
	if !changed {
		return q, false
	}
	if len(kept) > 0 {
		out.Set("q", strings.Join(kept, " "))
	} else {
		out.Del("q")
	}
	return out, true
}
