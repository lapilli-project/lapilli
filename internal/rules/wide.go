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
// itself; so is a selector with a label's name that doesn't unquote, or
// __graphite__, which VictoriaMetrics reads as a pattern of names.
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
			// A label's name that doesn't unquote, a history may read
			// otherwise ("\__name__" is __name__ to MetricsQL); and
			// __graphite__ is VictoriaMetrics' pattern of names.
			l, bad := unquoteLabel(label)
			if bad || l == "__graphite__" {
				return sel, true
			}
			if l != "__name__" {
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
// request's rule backtested through lapilli serve may: a selector whose
// range and offset, with those of every subquery around it, reach further
// back than most in all; a subquery's step that makes it more than points
// points, or none given (the server's own would be the evaluation's); an
// offset or a range written other than as a duration (Prometheus's
// duration expressions, offset (700d)); an @, which reads a time of its
// own choosing. "" when there is none. An expression that doesn't read is
// said so. Comments are read past, as Prometheus does: what a # starts, to
// the line's end, outside a string, reaches nothing, and hides nothing.
func Reach(expr string, most time.Duration, points int) string {
	expr = blankComments(expr)
	type subquery struct {
		open, close int // the parentheses it ranges over
		reach       time.Duration
	}
	type read struct {
		at    int // where the selector ends
		reach time.Duration
	}
	var subs []subquery
	var reads []read
	var opens []int
	space := func(i int) int {
		for i < len(expr) && (expr[i] == ' ' || expr[i] == '\t' || expr[i] == '\n' || expr[i] == '\r') {
			i++
		}
		return i
	}
	// offsetAt reads an offset at i, if one is there: its duration, where
	// it ends, and why it can't be taken.
	offsetAt := func(i int) (time.Duration, int, string) {
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
		case c == '(':
			opens = append(opens, i)
		case c == ')':
			if len(opens) == 0 {
				return "an expression that doesn't read"
			}
			open := opens[len(opens)-1]
			opens = opens[:len(opens)-1]
			j := space(i + 1)
			if j < len(expr) && expr[j] == '[' {
				end := strings.IndexByte(expr[j:], ']')
				if end < 0 {
					return "an expression that doesn't read"
				}
				r := expr[j+1 : j+end]
				span, step, _ := strings.Cut(r, ":")
				d, ok := readRange(strings.TrimSpace(span))
				switch {
				case !ok:
					return "[" + r + "], a range written other than as a duration"
				case !strings.Contains(r, ":"):
					return "[" + r + "] after a parenthesis, which doesn't read"
				case strings.TrimSpace(step) == "":
					return "[" + r + "], a subquery with no step: give it one"
				}
				s, ok := readRange(strings.TrimSpace(step))
				if !ok || s <= 0 || int64(d/s) > int64(points) {
					return "[" + r + "], a subquery of more than " + strconv.Itoa(points) + " points"
				}
				off, after, why := offsetAt(j + end + 1)
				if why != "" {
					return why
				}
				subs = append(subs, subquery{open, i, d + absDuration(off)})
				i = after - 1
			}
		case c == '[':
			// A selector's range, or a subquery of a selector.
			end := strings.IndexByte(expr[i:], ']')
			if end < 0 {
				return "an expression that doesn't read"
			}
			r := expr[i+1 : i+end]
			span, step, sub := strings.Cut(r, ":")
			d, ok := readRange(strings.TrimSpace(span))
			switch {
			case !ok:
				return "[" + r + "], a range written other than as a duration"
			case sub && strings.TrimSpace(step) == "":
				return "[" + r + "], a subquery with no step: give it one"
			case sub:
				s, ok := readRange(strings.TrimSpace(step))
				if !ok || s <= 0 || int64(d/s) > int64(points) {
					return "[" + r + "], a subquery of more than " + strconv.Itoa(points) + " points"
				}
			}
			off, after, why := offsetAt(i + end + 1)
			if why != "" {
				return why
			}
			reads = append(reads, read{i, d + absDuration(off)})
			i = after - 1
		case c == '}' || isWordByte(c):
			// The end of a selector, or of a word: an offset after it
			// moves what it reads.
			j := i
			for j+1 < len(expr) && c != '}' && isWordByte(expr[j+1]) {
				j++
			}
			i = j
			next := space(j + 1)
			if next < len(expr) && expr[next] == '[' {
				continue // its range, read at the bracket
			}
			if off, after, why := offsetAt(j + 1); why != "" {
				return why
			} else if after > j+1 {
				reads = append(reads, read{j, absDuration(off)})
				i = after - 1
			}
		}
	}
	if quote != 0 || len(opens) > 0 {
		return "an expression that doesn't read"
	}
	// Each read, with the subqueries around it: what it reads back in all.
	for _, r := range reads {
		total := r.reach
		for _, s := range subs {
			if s.open < r.at && r.at < s.close {
				total += s.reach
			}
		}
		if total > most {
			return "a range and an offset, with the subqueries around them, reaching " + reachOf(total) + " back, past " + reachOf(most)
		}
	}
	// A subquery with nothing read inside it reaches its own range.
	for _, s := range subs {
		total := s.reach
		for _, t := range subs {
			if t.open < s.open && s.close < t.close {
				total += t.reach
			}
		}
		if total > most {
			return "subqueries reaching " + reachOf(total) + " back, past " + reachOf(most)
		}
	}
	return ""
}

// reachOf is a reach as a comment says it: in days when whole ones.
func reachOf(d time.Duration) string {
	if d > 0 && d%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	}
	return promDuration(d)
}

// blankComments is expr with each comment, from a # outside a string to
// the line's end, written as spaces, so that what is left is where it was
// and no quote in a comment starts a string: strings read as scopeScan
// reads them, \ escaping in "..." and '...', nothing in `...`. A line
// ends at a \r as at a \n, as Prometheus's lexer ends a comment
// (isEndOfLine); what follows a \r is read, not passed over. Prometheus
// reads a comment anywhere outside a string, in a selector's braces and a
// range's brackets too, and so does this.
func blankComments(expr string) string {
	if !strings.Contains(expr, "#") {
		return expr
	}
	b := []byte(expr)
	var quote byte
	for i := 0; i < len(b); i++ {
		switch c := b[i]; {
		case quote != 0 && c == '\\' && quote != '`':
			i++
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '#':
			for ; i < len(b) && b[i] != '\n' && b[i] != '\r'; i++ {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
