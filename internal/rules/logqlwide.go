package rules

import (
	"strings"
	"time"
)

// LogQLWide is the first stream selector of a LogQL expression that names
// no stream by a label's value: none of its matchers a label matched with
// = to a value that isn't empty ({app=~".+"}, {app!="x"}, {}), as Wide is
// of PromQL's selectors. A pull request's rule backtested through lapilli
// serve reads with serve's access, of a Loki others share, and is held to
// the streams it names. A selector that doesn't read is wide, said as
// itself; an expression whose strings don't close, as a whole.
func LogQLWide(expr string) (string, bool) {
	expr = blankComments(expr)
	var quote byte
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		switch {
		case quote != 0 && c == '\\' && quote != '`':
			i++
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '{':
			end := closingBrace(expr, i)
			if end < 0 {
				return expr, true
			}
			sel := expr[i : end+1]
			named := false
			for _, m := range splitMatchers(expr[i+1 : end]) {
				m = strings.TrimSpace(m)
				if m == "" {
					continue
				}
				label, op, value, ok := splitMatcher(m)
				if !ok {
					return sel, true
				}
				if _, bad := unquoteLabel(label); bad {
					return sel, true
				}
				v, ok := unquote(strings.TrimSpace(value))
				if !ok {
					return sel, true
				}
				if op == "=" && v != "" {
					named = true
				}
			}
			if !named {
				return sel, true
			}
			i = end
		}
	}
	if quote != 0 {
		return expr, true
	}
	return "", false
}

// LogQLReach says the first thing a LogQL expression reads past where a
// pull request's rule backtested through lapilli serve may, as Reach says
// of PromQL: a range with its offset reaching further back than most; a
// range or an offset written other than as a duration; a subquery, which
// LogQL hasn't; an offset after no range; an @. "" when there is none. A
// stream selector's matchers, between its braces, are passed over, and
// comments read past, as Loki reads them.
func LogQLReach(expr string, most time.Duration) string {
	expr = blankComments(expr)
	var quote byte
	braces := 0
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		switch {
		case quote != 0 && c == '\\' && quote != '`':
			i++
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '{':
			braces++
		case c == '}':
			if braces == 0 {
				return "an expression that doesn't read"
			}
			braces--
		case braces > 0:
		case c == '@':
			return "@, which reads a time of its own"
		case c == '[':
			end := strings.IndexByte(expr[i:], ']')
			if end < 0 {
				return "an expression that doesn't read"
			}
			r := strings.TrimSpace(expr[i+1 : i+end])
			if strings.Contains(r, ":") {
				return "[" + r + "], a subquery, which LogQL hasn't"
			}
			d, ok := readRange(r)
			if !ok {
				return "[" + r + "], a range written other than as a duration"
			}
			off, after, why := offsetAfter(expr, i+end+1)
			if why != "" {
				return why
			}
			if total := d + absDuration(off); total > most {
				return "a range and its offset reaching " + reachOf(total) + " back, past " + reachOf(most)
			}
			i = after - 1
		case isWordByte(c):
			j := i
			for j < len(expr) && isWordByte(expr[j]) {
				j++
			}
			if strings.EqualFold(expr[i:j], "offset") {
				return "an offset after no range"
			}
			i = j - 1
		}
	}
	if quote != 0 || braces > 0 {
		return "an expression that doesn't read"
	}
	return ""
}

// offsetAfter reads an offset at i of expr, if one is there, past spaces:
// its duration, where it ends, and why it can't be taken; i when there is
// none.
func offsetAfter(expr string, i int) (time.Duration, int, string) {
	space := func(i int) int {
		for i < len(expr) && (expr[i] == ' ' || expr[i] == '\t' || expr[i] == '\n' || expr[i] == '\r') {
			i++
		}
		return i
	}
	j := space(i)
	if len(expr) < j+6 || !strings.EqualFold(expr[j:j+6], "offset") || len(expr) > j+6 && isWordByte(expr[j+6]) {
		return 0, i, ""
	}
	k := space(j + 6)
	if k < len(expr) && (expr[k] == '-' || expr[k] == '+') {
		k++
	}
	end := k
	for end < len(expr) && (isWordByte(expr[end]) || expr[end] == '.') {
		end++
	}
	d, ok := readRange(expr[k:end])
	if end == k || !ok {
		return 0, end, "an offset written other than as a duration"
	}
	return d, end, ""
}
