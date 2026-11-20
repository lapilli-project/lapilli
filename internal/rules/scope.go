package rules

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// keywords are PromQL words that are never a metric name.
var keywords = map[string]bool{
	"and": true, "or": true, "unless": true, "atan2": true, "bool": true, "offset": true,
	"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true,
	"inf": true, "nan": true,
}

// labelLists are the keywords a list of label names in parentheses follows.
var labelLists = map[string]bool{
	"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true,
}

// Scope adds label matchers to every series selector in a PromQL
// expression, as a managed Prometheus that scopes a rule to a namespace or
// a cluster does before it evaluates it: errors > 5 becomes
// errors{namespace="shop"} > 5.
func Scope(expr string, labels map[string]string) (string, error) {
	return scopeWith(expr, matchersOf(labels), -1)
}

// matchersOf are labels as matchers, as Scope adds them, in their names'
// order: written once for every expression they are added to.
func matchersOf(labels map[string]string) string {
	var m []string
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		m = append(m, k+"="+strconv.Quote(labels[k]))
	}
	return strings.Join(m, ",")
}

// errNoRoom is an expression that would grow past the room scopeWith has.
var errNoRoom = errors.New("no room")

// scopeWith is Scope, with the matchers written: the expression grows by
// them at each selector, and past room bytes more (room >= 0), it is given
// up, errNoRoom, before anything is made. What it grows by is worked out
// first, without writing it (scopeScan), and the expression written into
// as much: written as it grew, a builder grows a quarter at a time, five
// times what it holds made by the end.
func scopeWith(expr, matchers string, room int) (string, error) {
	grows, err := scopeScan(expr, matchers, nil, nil)
	if err != nil {
		return "", err
	}
	if room >= 0 && grows > room {
		return "", errNoRoom
	}
	var b strings.Builder
	b.Grow(len(expr) + grows)
	if _, err := scopeScan(expr, matchers, &b, nil); err != nil {
		return "", err
	}
	return b.String(), nil
}

// scopeOut is what scopeScan writes into; nil, nothing.
type scopeOut struct{ b *strings.Builder }

func (o scopeOut) byte(c byte) {
	if o.b != nil {
		o.b.WriteByte(c)
	}
}

func (o scopeOut) str(s string) {
	if o.b != nil {
		o.b.WriteString(s)
	}
}

// Selectors are the series selectors of a PromQL expression, as written,
// each once, in the order they come: a metric's name with its matchers,
// or matchers alone (http_requests_total{code=~"5.."}, up, {job="x"}),
// with the offset and the @ written after them, past a range
// (x offset 1w, x @ 1700000000), as they move where the data is read; not
// the range itself. An expression that doesn't read as Scope reads it is
// an error.
func Selectors(expr string) ([]string, error) {
	var x scanned
	if _, err := scopeScan(expr, "", nil, &x); err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for i, s := range x.sels {
		s += selectorModifiers(x.text[x.ends[i]:])
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

// selectorModifiers are the offset and the @ a selector's text is followed
// by in rest, past its range, as " offset 1w @ 1700000000": rest read as
// scopeScan reads it, its comments blanked (scanned.text).
func selectorModifiers(rest string) string {
	space := func() { rest = strings.TrimLeft(rest, " \t\n\r") }
	token := func() string {
		space()
		j := 0
		for j < len(rest) && (rest[j] == '-' || rest[j] == '+' || rest[j] == '.' || rest[j] == '_' || rest[j] == '(' || rest[j] == ')' ||
			rest[j] >= '0' && rest[j] <= '9' || rest[j] >= 'a' && rest[j] <= 'z' || rest[j] >= 'A' && rest[j] <= 'Z') {
			if rest[j] == ')' && !strings.HasSuffix(rest[:j], "(") {
				break // the call it is in closes
			}
			j++
		}
		t := rest[:j]
		rest = rest[j:]
		return t
	}
	space()
	if strings.HasPrefix(rest, "[") {
		if end := strings.IndexByte(rest, ']'); end >= 0 {
			rest = rest[end+1:]
		}
	}
	// Looked at no further than each modifier, and written once, so that
	// an expression's selectors are read in time linear in its length.
	var out strings.Builder
	for {
		space()
		switch {
		case len(rest) >= 6 && strings.EqualFold(rest[:6], "offset") && (len(rest) == 6 || !isWordByte(rest[6])):
			rest = rest[6:]
			out.WriteString(" offset " + token())
		case strings.HasPrefix(rest, "@"):
			rest = rest[1:]
			out.WriteString(" @ " + token())
		default:
			return out.String()
		}
	}
}

// Ranges are what is in the brackets of an expression's range selectors and
// subqueries, as written: 5m, 1h:5m.
func Ranges(expr string) ([]string, error) {
	var x scanned
	_, err := scopeScan(expr, "", nil, &x)
	return x.ranges, err
}

// scanned is what scopeScan found: the selectors, where each ends, and the
// ranges, in text, the expression as it was read, its comments blanked.
type scanned struct {
	sels, ranges []string
	ends         []int
	text         string
}

// scopeScan reads expr as Scope does, writing it with matchers added into
// b, unless b is nil, and says how much they add: the matchers, and the
// commas and brackets around them. Each selector it adds them to, and each
// range, is put in found, as read, unless found is nil.
//
// It reads expr with its comments blanked (blankComments), as Prometheus
// reads past them, in braces and brackets as anywhere outside a string,
// to a \r or a \n: what a comment holds is never a selector's end, a
// range's, or a string's start. What it writes into b is expr as written,
// its comments kept, the matchers added where they would be had the
// comments been spaces: never within one, as a comment ends before the
// } or the word they are added at.
func scopeScan(src, matchers string, b *strings.Builder, found *scanned) (int, error) {
	expr := blankComments(src) // the same length as src, read in its place
	if found != nil {
		found.text = expr
	}
	w := scopeOut{b}
	added := 0
	// A metric's name just read, which matchers may follow: where it
	// starts and ends, nameEnd -1 for none.
	nameStart, nameEnd := 0, -1
	var parens []bool // for each open parenthesis, whether it holds label names
	listNext := false // a label list's parenthesis comes next
	i := 0
	next := func() byte { // the next character that isn't space
		for j := i; j < len(expr); j++ {
			if c := expr[j]; c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				return c
			}
		}
		return 0
	}
	str := func() error { // copy a quoted string
		q := expr[i]
		w.byte(q)
		for i++; i < len(expr); i++ {
			w.byte(expr[i])
			switch {
			case expr[i] == '\\' && q != '`' && i+1 < len(expr):
				i++
				w.byte(expr[i])
			case expr[i] == q:
				i++
				return nil
			}
		}
		return errors.New("a string doesn't end")
	}
	for i < len(expr) {
		c := expr[i]
		if listNext && c != '(' && c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			listNext = false // group_left with no labels
		}
		switch {
		case c == '"' || c == '\'' || c == '`':
			if err := str(); err != nil {
				return 0, err
			}
		case c == '{':
			// A selector's matchers: copy them, then add ours.
			open := i
			w.byte(c)
			for i++; ; {
				if i >= len(expr) {
					return 0, errors.New("a { isn't closed")
				}
				if q := expr[i]; q == '"' || q == '\'' || q == '`' {
					if err := str(); err != nil {
						return 0, err
					}
					continue
				}
				if expr[i] == '}' {
					break
				}
				w.byte(src[i])
				i++
			}
			if found != nil {
				from := open
				if nameEnd >= 0 && strings.TrimSpace(expr[nameEnd:open]) == "" {
					from = nameStart
				}
				found.sels, found.ends = append(found.sels, expr[from:i+1]), append(found.ends, i+1)
			}
			nameEnd = -1
			// What is in it is copied as written.
			inner := strings.TrimSpace(expr[open+1 : i])
			added += len(matchers)
			if inner != "" && !strings.HasSuffix(inner, ",") {
				added++
				w.byte(',')
			}
			w.str(matchers)
			w.byte('}')
			i++
		case c == '[':
			// A range or a subquery's window and step.
			end := strings.IndexByte(expr[i:], ']')
			if end < 0 {
				return 0, errors.New("a [ isn't closed")
			}
			w.str(src[i : i+end+1])
			if found != nil {
				found.ranges = append(found.ranges, strings.TrimSpace(expr[i+1:i+end]))
			}
			i += end + 1
		case c == '(':
			parens = append(parens, listNext)
			listNext = false
			w.byte(c)
			i++
		case c == ')':
			if len(parens) == 0 {
				return 0, errors.New("a ) wasn't opened")
			}
			parens = parens[:len(parens)-1]
			w.byte(c)
			i++
		case c >= '0' && c <= '9' || c == '.':
			// A number or a duration: 5, 0.5, 1e-3, 0x1f, 5m, 1h30m.
			start := i
			// Its first two bytes alone: lowering the rest of the
			// expression at every number took time with its square.
			hex := expr[i] == '0' && i+1 < len(expr) && expr[i+1]|0x20 == 'x'
			for i < len(expr) {
				d := expr[i]
				alnum := d >= '0' && d <= '9' || d >= 'a' && d <= 'z' || d >= 'A' && d <= 'Z' || d == '.' || d == '_'
				exp := i > start && (d == '+' || d == '-') && (expr[i-1] == 'e' || expr[i-1] == 'E') && !hex
				if !alnum && !exp {
					break
				}
				w.byte(d)
				i++
			}
		case c == '_' || c == ':' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(expr) && (expr[j] == '_' || expr[j] == ':' || expr[j] >= 'a' && expr[j] <= 'z' ||
				expr[j] >= 'A' && expr[j] <= 'Z' || expr[j] >= '0' && expr[j] <= '9') {
				j++
			}
			word, wordStart := expr[i:j], i
			w.str(word)
			i = j
			lower := strings.ToLower(word)
			after := next()
			switch {
			case len(parens) > 0 && parens[len(parens)-1]:
				// a label name in by (...), on (...)
			case keywords[lower]:
				listNext = labelLists[lower]
			case after == '(':
				// a function or an aggregation
			case nextWord(expr[i:]) == "by" || nextWord(expr[i:]) == "without":
				// sum by (...) (...), sum without (...) (...)
			case after != '{':
				added += len(matchers) + len("{}")
				w.byte('{')
				w.str(matchers)
				w.byte('}')
				if found != nil {
					found.sels, found.ends = append(found.sels, word), append(found.ends, i)
				}
			default:
				nameStart, nameEnd = wordStart, i // its matchers come next
			}
		default:
			w.byte(src[i])
			i++
		}
	}
	if len(parens) > 0 {
		return 0, fmt.Errorf("%d ( not closed", len(parens))
	}
	return added, nil
}

// nextWord is the word s starts with, after spaces, in lower case.
func nextWord(s string) string {
	s = strings.TrimLeft(s, " \t\n\r")
	j := 0
	for j < len(s) && (s[j] == '_' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
		j++
	}
	return strings.ToLower(s[:j])
}

// scoping puts matchers into each selector of the expressions of a
// document's rules, as a managed Prometheus does (a Google Rules'
// namespace, an Azure rule group's cluster), what that adds to each charged
// to the input before it is made: what and value name the label.
type scoping struct {
	in                    *input
	matchers, what, value string
}

// scope is Scope with s's matchers; past what the input may make, a
// MadeError.
func (s scoping) scope(expr string) (string, error) {
	by := func() string {
		return fmt.Sprintf("the %s %.40q, %d bytes, put into each selector of each rule's expression", s.what, s.value, len(s.value))
	}
	room := s.in.room()
	out, err := scopeWith(expr, s.matchers, room)
	switch {
	case errors.Is(err, errNoRoom):
		return "", s.in.charge(room+1, by)
	case err != nil:
		return "", err
	}
	if err := s.in.charge(len(out)-len(expr), by); err != nil {
		return "", err
	}
	return out, nil
}
