package serve

import (
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
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

// String is the word as written in the box: what tokens reads as it.
func (t qword) String() string {
	v := t.Value
	if t.Quoted || v == "" && t.Key == "" || strings.ContainsFunc(v, unicode.IsSpace) {
		v = `"` + v + `"`
	}
	if t.Key != "" {
		return t.Key + ":" + v
	}
	return v
}

// The bounds of what a page is asked (docs/design/ui.md): what a page
// does with a query doesn't grow with it past them.
const (
	// maxQuery is a page's query, as sent, in bytes: past it, the page is
	// refused (414) before any work.
	maxQuery = 4096
	// maxSearch is a search's length, in characters: past it, the search
	// is left out whole, and the page says so.
	maxSearch = 500
	// maxWords are the words of a search used: those after are named, and
	// left out.
	maxWords = 20
	// maxValues are the values of a facet used, each once: those after are
	// left out.
	maxValues = 20
)

// searchText is a search box's text as a page reads it: every q= of the
// URL, joined, and its words, read once.
type searchText struct {
	// Q is the text, "" when it is too long: past maxSearch characters,
	// TooLong, and nothing of it is used.
	Q       string
	TooLong bool
	// All are its words; Words the first maxWords of them, those used,
	// and Over the rest.
	All, Words, Over []qword
}

func searchOf(qs []string) searchText {
	q := strings.TrimSpace(strings.Join(qs, " "))
	if utf8.RuneCountInString(q) > maxSearch {
		return searchText{TooLong: true}
	}
	st := searchText{Q: q, All: tokens(q)}
	st.Words = st.All[:min(len(st.All), maxWords)]
	st.Over = st.All[len(st.Words):]
	return st
}

// asWritten are words as written in the box, joined.
func asWritten(ts []qword) string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.String()
	}
	return strings.Join(out, " ")
}

// tokens are the words of a search: "a phrase in quotes" is one, and a
// part of a name, whatever it holds; a word key:value is a filter when key
// is letters alone (a name such as job:rate5m is written in quotes), and
// key:"a value in quotes" is one whose value has spaces.
func tokens(q string) []qword {
	var out []qword
	rs := []rune(q)
	// quoted is where the quote begun at i ends, and what it holds.
	quoted := func(i int) (int, string) {
		j := i + 1
		for j < len(rs) && rs[j] != '"' {
			j++
		}
		return j + 1, strings.TrimSpace(string(rs[i+1 : min(j, len(rs))]))
	}
	for i := 0; i < len(rs); {
		switch {
		case unicode.IsSpace(rs[i]):
			i++
			continue
		case rs[i] == '"':
			var v string
			if i, v = quoted(i); v != "" {
				out = append(out, qword{Value: v, Quoted: true})
			}
			continue
		}
		j := i
		for j < len(rs) && !unicode.IsSpace(rs[j]) {
			if rs[j] == '"' && j > i && rs[j-1] == ':' && isKey(rs[i:j-1]) {
				break
			}
			j++
		}
		if j < len(rs) && rs[j] == '"' {
			// key:"a value with spaces"
			end, v := quoted(j)
			out = append(out, keyed(string(rs[i:j-1]), v))
			i = end
			continue
		}
		out = append(out, word(string(rs[i:j])))
		i = j
	}
	return out
}

// isKey reports whether a word's part before its colon is a key: letters
// alone.
func isKey(k []rune) bool {
	return len(k) > 0 && !slices.ContainsFunc(k, func(r rune) bool { return !unicode.IsLetter(r) })
}

// word is a word of a search, unquoted.
func word(w string) qword {
	k, v, ok := strings.Cut(w, ":")
	if !ok || !isKey([]rune(k)) {
		return qword{Value: w}
	}
	return keyed(k, v)
}

// keyed is a filter, key:value, and whether it is understood.
func keyed(k, v string) qword {
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
// no one has stays in the box. A search too long is left as it is, for the
// page to say so; a facet keeps maxValues values.
func canonical(q url.Values, teams []string) (out url.Values, changed bool) {
	st := searchOf(q["q"])
	if st.TooLong {
		return q, false
	}
	out = url.Values{}
	for k, vs := range q {
		out[k] = slices.Clone(vs)
	}
	add := func(k, v string) {
		if !slices.Contains(out[k], v) && len(out[k]) < maxValues {
			out[k] = append(out[k], v)
		}
	}
	var kept []qword
	for _, t := range st.All {
		if t.Bad != "" {
			kept = append(kept, t)
			continue
		}
		switch t.Key {
		case "team":
			if !slices.Contains(teams, t.Value) {
				kept = append(kept, t)
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
			kept = append(kept, t)
			continue
		}
		changed = true
	}
	if !changed {
		return q, false
	}
	if len(kept) > 0 {
		out.Set("q", asWritten(kept))
	} else {
		out.Del("q")
	}
	return out, true
}
