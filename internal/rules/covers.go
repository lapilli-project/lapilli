package rules

import (
	"slices"
	"strings"
)

// Covers reports whether selector a, as Selectors gives one, reads every
// series selector b does: the same offset and @, and each of a's matchers,
// its metric's name among them, one of b's, so that b only narrows a. Of
// an impact signal's expression, a selector b with no data, covered by one
// with data, had nothing to match, not nothing to read: the 5xx requests
// of a service that served no 5xx (http_requests_total{code=~"5.."}, whose
// series a client library makes at the first one), beside all of its
// requests (http_requests_total). A selector that doesn't read as one is
// covered by none.
func Covers(a, b string) bool {
	ma, oa, ok := selectorParts(a)
	if !ok || len(ma) == 0 {
		return false
	}
	mb, ob, ok := selectorParts(b)
	if !ok || oa != ob {
		return false
	}
	for _, m := range ma {
		if !slices.Contains(mb, m) {
			return false
		}
	}
	return true
}

// selectorParts are a selector's matchers, its metric's name as
// __name__="name", each written one way, and what follows it: its offset
// and @, spaces made one.
func selectorParts(sel string) (matchers []string, mods string, ok bool) {
	sel = strings.TrimSpace(sel)
	body, rest := sel, ""
	if i := strings.IndexByte(sel, '{'); i >= 0 {
		end := closingBrace(sel, i)
		if end < 0 {
			return nil, "", false
		}
		body, rest = sel[:end+1], sel[end+1:]
		name := strings.TrimSpace(sel[:i])
		if name != "" {
			matchers = append(matchers, `__name__="`+name+`"`)
		}
		for _, m := range splitMatchers(sel[i+1 : end]) {
			norm, ok := matcher(m)
			if !ok {
				return nil, "", false
			}
			if norm != "" && !slices.Contains(matchers, norm) {
				matchers = append(matchers, norm)
			}
		}
	} else {
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
