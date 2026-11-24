package rules

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Covers reports whether selector a, as Selectors gives one, reads every
// series selector b does: the same offset and @, and each of a's matchers,
// its metric's name among them, one of b's, so that b only narrows a. A
// selector that doesn't read as one is covered by none.
func Covers(a, b string) bool { return readSel(a).covers(readSel(b)) }

// selectorParts are a selector's matchers, its metric's name as
// __name__="name", each written one way, and what follows it: its offset
// and @, spaces made one.
func selectorParts(sel string) (matchers []string, mods string, ok bool) {
	sel = strings.TrimSpace(sel)
	rest := ""
	if i := strings.IndexByte(sel, '{'); i >= 0 {
		end := closingBrace(sel, i)
		if end < 0 {
			return nil, "", false
		}
		rest = sel[end+1:]
		name := strings.TrimSpace(sel[:i])
		if name != "" {
			matchers = append(matchers, `__name__="`+name+`"`)
		}
		// Each once, looked up in a set: a selector can hold thousands.
		seen := map[string]bool{}
		for _, m := range matchers {
			seen[m] = true
		}
		for _, m := range splitMatchers(sel[i+1 : end]) {
			norm, ok := matcher(m)
			if !ok {
				return nil, "", false
			}
			if norm != "" && !seen[norm] {
				seen[norm] = true
				matchers = append(matchers, norm)
			}
		}
	} else {
		body := sel
		if i := strings.IndexAny(sel, " \t\n\r"); i >= 0 {
			body, rest = sel[:i], sel[i:]
		}
		if body == "" {
			return nil, "", false
		}
		matchers = append(matchers, `__name__="`+body+`"`)
	}
	return matchers, strings.Join(strings.Fields(rest), " "), true
}

// closingBrace is where the brace that opens at i closes, outside quoted
// strings; -1 for nowhere.
func closingBrace(s string, i int) int {
	var quote byte
	for j := i + 1; j < len(s); j++ {
		c := s[j]
		switch {
		case quote != 0 && c == '\\' && quote != '`':
			j++
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '}':
			return j
		}
	}
	return -1
}

// splitMatchers are the matchers between a selector's braces, split at
// the commas outside quoted strings.
func splitMatchers(s string) []string {
	var out []string
	var quote byte
	start := 0
	for j := 0; j < len(s); j++ {
		c := s[j]
		switch {
		case quote != 0 && c == '\\' && quote != '`':
			j++
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == ',':
			out = append(out, s[start:j])
			start = j + 1
		}
	}
	return append(out, s[start:])
}

// matcher is one matcher written one way, label, operator and value with
// no spaces between; a metric's name alone in the braces, as Prometheus 3
// takes one ({"http.requests"}), as __name__'s; "" for none, a trailing
// comma's.
func matcher(m string) (string, bool) {
	m = strings.TrimSpace(m)
	if m == "" {
		return "", true
	}
	i := strings.IndexAny(m, "=!")
	if q := m[0]; q == '"' || q == '\'' || q == '`' {
		// A quoted label's name, or a metric's name alone.
		end := strings.IndexByte(m[1:], q)
		if end < 0 {
			return "", false
		}
		if strings.TrimSpace(m[end+2:]) == "" {
			return `__name__=` + m, true
		}
		i = end + 2 + strings.IndexAny(m[end+2:], "=!")
		if i < end+2 {
			return "", false
		}
	}
	if i <= 0 {
		return "", false
	}
	label := strings.TrimSpace(m[:i])
	op := "="
	for _, o := range []string{"=~", "!~", "!=", "="} {
		if strings.HasPrefix(m[i:], o) {
			op = o
			break
		}
	}
	value := strings.TrimSpace(m[i+len(op):])
	if label == "" || value == "" {
		return "", false
	}
	return label + op + value, true
}

// MaxStandIns is how many stand-ins an expression has at most (StandIns).
const MaxStandIns = 32

// maxStandInSelectors is how many selectors an expression's stand-ins are
// looked for among at most: each is held against each other, and an impact
// expression reads 32 at most (serve.CheckImpact).
const maxStandInSelectors = 256

// A StandIn is a selector read for another's sake (For), which only
// narrows one (Covers): of the series that one reads, those with each
// label For narrows them by with a matcher the empty value doesn't match,
// any value. Where its data begins is where For's would, had nothing it
// matches happened yet: http_requests_total{code!=""} for
// http_requests_total{code=~"5.."}, whose series a client library makes
// at the first 5xx, beside http_requests_total. Its data beginning later
// than all requests' says the code label came later, through a rename, a
// relabelling, a library's upgrade: nothing could be matched before it.
type StandIn struct {
	Sel, For string
	// Of is the selector For narrows it was made of: one with no data of
	// its own says nothing of For's labels.
	Of string
}

// StandIns are, of expr's selectors, the stand-ins of each that only
// narrows another (Covers), made of the widest of those it narrows, the
// ones no other selector of expr is wider than: the expression reads
// those, and they see what it would. In the order they come, MaxStandIns
// at most; not of one an absent() or absent_over_time() reads, at any
// depth, which says users are hurt while it has no data.
//
// Each selector is read once, and whether it is of the widest once: the
// time it takes grows with the expression's length, and with the square
// of its selectors, of which more than maxStandInSelectors are refused.
func StandIns(expr string) ([]StandIn, error) {
	sels, absent, err := selectorsRead(expr)
	if err != nil {
		return nil, err
	}
	if len(sels) > maxStandInSelectors {
		return nil, fmt.Errorf("%d selectors: stand-ins are looked for among %d at most", len(sels), maxStandInSelectors)
	}
	parts := make([]selParts, len(sels))
	for i, s := range sels {
		parts[i] = readSel(s)
	}
	// Of the widest: no other covers it without being covered by it.
	wide := make([]bool, len(sels))
	for a := range sels {
		wide[a] = true
		for c := range sels {
			if c != a && parts[c].covers(parts[a]) && !parts[a].covers(parts[c]) {
				wide[a] = false
				break
			}
		}
	}
	var out []StandIn
	for j, b := range sels {
		if absent[b] {
			continue
		}
		for i, a := range sels {
			if i == j || !wide[i] || !parts[i].covers(parts[j]) {
				continue
			}
			sel, ok := standIn(parts[i], parts[j])
			if !ok {
				continue
			}
			x := StandIn{Sel: sel, For: b, Of: a}
			if !slices.Contains(out, x) {
				out = append(out, x)
			}
			if len(out) == MaxStandIns {
				return out, nil
			}
		}
	}
	return out, nil
}

// selParts is a selector as selectorParts reads it, read once, with its
// matchers to look each one up in.
type selParts struct {
	matchers []string
	has      map[string]bool
	mods     string
	ok       bool
}

func readSel(s string) selParts {
	var p selParts
	p.matchers, p.mods, p.ok = selectorParts(s)
	p.has = make(map[string]bool, len(p.matchers))
	for _, m := range p.matchers {
		p.has[m] = true
	}
	return p
}

// covers is Covers of two selectors read.
func (a selParts) covers(b selParts) bool {
	if !a.ok || len(a.matchers) == 0 || !b.ok || a.mods != b.mods {
		return false
	}
	for _, m := range a.matchers {
		if !b.has[m] {
			return false
		}
	}
	return true
}

// selectorsRead are expr's selectors, as Selectors gives them, and those
// of them an absent() or absent_over_time() reads, at one place at least.
func selectorsRead(expr string) ([]string, map[string]bool, error) {
	var x scanned
	if _, err := scopeScan(expr, "", nil, &x); err != nil {
		return nil, nil, err
	}
	in := inAbsent(x.text)
	var out []string
	absent, seen := map[string]bool{}, map[string]bool{}
	for i, s := range x.sels {
		start := x.ends[i] - len(s)
		s += selectorModifiers(x.text[x.ends[i]:])
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
		if start >= 0 && start < len(in) && in[start] {
			absent[s] = true
		}
	}
	return out, absent, nil
}

// absentBefore reports whether s ends in absent or absent_over_time, a
// name of its own, not the end of a longer one, then spaces: what opens
// the parenthesis after it. It looks back over those alone, never over
// all of s, so that an expression is read in time linear in its length
// however many parentheses it opens.
func absentBefore(s string) bool {
	s = strings.TrimRight(s, " \t\n\f\r")
	j := len(s)
	for j > 0 && len(s)-j <= len("absent_over_time") && nameByte(s[j-1]) {
		j--
	}
	name := s[j:]
	return (name == "absent" || name == "absent_over_time") && (j == 0 || !nameByte(s[j-1]))
}

// nameByte reports whether c is of a metric's or a function's name.
func nameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == ':'
}

// inAbsent says, of each byte of expr, whether it is within the
// parentheses of an absent() or absent_over_time(), at any depth; quoted
// strings are passed over. expr is read with its comments blanked
// (scanned.text).
func inAbsent(expr string) []bool {
	out := make([]bool, len(expr))
	var stack []bool // of each open parenthesis, whether an absent opened it
	depth := 0       // how many of them did
	var quote byte
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		out[i] = depth > 0
		switch {
		case quote != 0 && c == '\\' && quote != '`':
			i++
			if i < len(expr) {
				out[i] = depth > 0
			}
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '(':
			a := absentBefore(expr[:i])
			stack = append(stack, a)
			if a {
				depth++
			}
		case c == ')' && len(stack) > 0:
			if stack[len(stack)-1] {
				depth--
			}
			stack = stack[:len(stack)-1]
		}
	}
	return out
}

// standIn is the stand-in a gives for b, which only narrows it: a's
// matchers, b's metric's name, and label!="" of each other label b
// narrows by with a matcher the empty value doesn't match.
func standIn(a, b selParts) (string, bool) {
	if !a.ok || !b.ok {
		return "", false
	}
	out, in := slices.Clone(a.matchers), maps.Clone(a.has)
	for _, m := range b.matchers {
		if a.has[m] {
			continue
		}
		label, op, value, ok := splitMatcher(m)
		if !ok || matchesEmpty(op, value) {
			continue
		}
		if label == "__name__" {
			// Its metric, as it is: a name doesn't come later.
			out = append(out, m)
			continue
		}
		if has := label + `!=""`; !in[has] {
			out, in[has] = append(out, has), true
		}
	}
	return renderSelector(out, a.mods), true
}

// splitMatcher is a matcher as matcher writes it, in its parts.
func splitMatcher(m string) (label, op, value string, ok bool) {
	i := strings.IndexAny(m, "=!")
	if q := m[0]; q == '"' || q == '\'' || q == '`' {
		end := strings.IndexByte(m[1:], q)
		if end < 0 {
			return "", "", "", false
		}
		i = end + 2
	}
	if i <= 0 || i >= len(m) {
		return "", "", "", false
	}
	label = m[:i]
	for _, o := range []string{"=~", "!~", "!=", "="} {
		if strings.HasPrefix(m[i:], o) {
			return label, o, m[i+len(o):], true
		}
	}
	return "", "", "", false
}

// matchesEmpty reports whether a matcher matches a series without its
// label, whose value is then empty; false when that can't be told, so
// that the label is asked for.
func matchesEmpty(op, quoted string) bool {
	v, ok := unquote(quoted)
	if !ok {
		return false
	}
	switch op {
	case "=":
		return v == ""
	case "!=":
		return v != ""
	}
	re, err := regexp.Compile("^(?:" + v + ")$")
	if err != nil {
		return false
	}
	return re.MatchString("") == (op == "=~")
}

// unquote is a PromQL string's value: double-quoted, single-quoted with
// the same escapes, or raw in backticks; false for what isn't one string,
// a quote of its kind inside it not escaped ('a' or b='c', `a` or `b`),
// which ends it where PromQL reads it.
func unquote(q string) (string, bool) {
	if len(q) < 2 || q[0] != q[len(q)-1] {
		return "", false
	}
	inner := q[1 : len(q)-1]
	switch q[0] {
	case '`':
		return inner, !strings.Contains(inner, "`")
	case '\'':
		for i := 0; i < len(inner); i++ {
			switch inner[i] {
			case '\\':
				i++
			case '\'':
				return "", false
			}
		}
		inner = strings.ReplaceAll(inner, `\'`, `'`)
		q = `"` + strings.ReplaceAll(inner, `"`, `\"`) + `"`
	}
	v, err := strconv.Unquote(q)
	return v, err == nil
}

var bareName = regexp.MustCompile(`^"([a-zA-Z_:][a-zA-Z0-9_:]*)"$`)

// renderSelector writes matchers as a selector, the metric's name before
// its braces when it is one PromQL takes bare, and then mods.
func renderSelector(matchers []string, mods string) string {
	name := ""
	var rest []string
	for _, m := range matchers {
		if v, ok := strings.CutPrefix(m, "__name__="); ok && name == "" && bareName.MatchString(v) {
			name = bareName.FindStringSubmatch(v)[1]
			continue
		}
		rest = append(rest, m)
	}
	out := name
	if len(rest) > 0 || name == "" {
		out += "{" + strings.Join(rest, ",") + "}"
	}
	if mods != "" {
		out += " " + mods
	}
	return out
}
