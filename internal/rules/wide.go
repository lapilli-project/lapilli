package rules

import (
	"strconv"
	"strings"
	"time"
)

// Wide is the first series selector of a PromQL expression that reads past
// one metric: matchers with no metric's name ({job="api"}), or a name
// matched as a pattern or a negation (__name__=~"api_.+",
// {__name__!="up"}). A pull request's rule backtested through lapilli serve
// reads with serve's access, of a Prometheus others share, and is held to
// one metric a selector: what it reads is what it names. An expression
// that doesn't read as Selectors reads it is wide as a whole, said as
// itself.
func Wide(expr string) (string, bool) {
	sels, err := Selectors(expr)
	if err != nil {
		return expr, true
	}
	for _, sel := range sels {
		ms, _, ok := selectorParts(sel)
		if !ok {
			return sel, true
		}
		named := false
		for _, m := range ms {
			label, op, value, ok := splitMatcher(m)
			if !ok {
				return sel, true
			}
			if l, err := unquoteLabel(label); err || l != "__name__" {
				continue
			}
			if op != "=" {
				return sel, true
			}
			if v, ok := unquote(value); !ok || v == "" {
				return sel, true
			}
			named = true
		}
		if !named {
			return sel, true
		}
	}
	return "", false
}

// unquoteLabel is a label's name as written, bare or quoted (Prometheus
// 3's {"http.requests", "le"="1"}); err says a quoted one didn't unquote.
func unquoteLabel(label string) (string, bool) {
	label = strings.TrimSpace(label)
	if label != "" && (label[0] == '"' || label[0] == '\'' || label[0] == '`') {
		v, ok := unquote(label)
		return v, !ok
	}
	return label, false
}

// Reach says the first thing an expression reads past where a pull
// request's rule backtested through lapilli serve may: a range, or a
// subquery's, past most; a subquery's step that makes it more than points
// points; an offset past offset, either way; an @, which reads a time of
// its own choosing. "" when there is none. A range written so that
// Lapilli doesn't read it is said as itself.
func Reach(expr string, most, offset time.Duration, points int) string {
	rs, err := Ranges(expr)
	if err != nil {
		return "an expression that doesn't read"
	}
	for _, r := range rs {
		span, step, sub := strings.Cut(r, ":")
		d, ok := readRange(strings.TrimSpace(span))
		switch {
		case !ok:
			return "[" + r + "], a range Lapilli doesn't read"
		case d > most:
			return "[" + r + "], a range past " + promDuration(most)
		case sub && strings.TrimSpace(step) != "":
			s, ok := readRange(strings.TrimSpace(step))
			if !ok || s <= 0 || int64(d/s) > int64(points) {
				return "[" + r + "], a subquery of more than " + strconv.Itoa(points) + " points"
			}
		}
	}
	// What follows the selectors and subqueries, outside quoted strings.
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
		case c == '@':
			return "@, which reads a time of its own"
		case (c == 'o' || c == 'O') && (i == 0 || !isWordByte(expr[i-1])) && len(expr) >= i+6 && strings.EqualFold(expr[i:i+6], "offset") &&
			(len(expr) == i+6 || !isWordByte(expr[i+6])):
			j := i + 6
			for j < len(expr) && (expr[j] == ' ' || expr[j] == '\t' || expr[j] == '\n' || expr[j] == '\r') {
				j++
			}
			k := j
			if k < len(expr) && (expr[k] == '-' || expr[k] == '+') {
				k++
			}
			end := k
			for end < len(expr) && (isWordByte(expr[end]) || expr[end] == '.') {
				end++
			}
			if end == k {
				continue // a label or a function named so
			}
			d, ok := readRange(expr[k:end])
			if !ok {
				return "offset " + expr[j:end] + ", which Lapilli doesn't read"
			}
			if d > offset {
				return "offset " + expr[j:end] + ", past " + promDuration(offset)
			}
			i = end - 1
		}
	}
	return ""
}
