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
	var m []string
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		m = append(m, k+"="+strconv.Quote(labels[k]))
	}
	matchers := strings.Join(m, ",")
	var b strings.Builder
	var parens []bool // for each open parenthesis, whether it holds label names
	listNext := false // a label list's parenthesis comes next
	i := 0
	next := func() byte { // the next character that isn't space or a comment
		for j := i; j < len(expr); j++ {
			switch c := expr[j]; {
			case c == '#':
				for j < len(expr) && expr[j] != '\n' {
					j++
				}
			case c != ' ' && c != '\t' && c != '\n' && c != '\r':
				return c
			}
		}
		return 0
	}
	str := func() error { // copy a quoted string
		q := expr[i]
		b.WriteByte(q)
		for i++; i < len(expr); i++ {
			b.WriteByte(expr[i])
			switch {
			case expr[i] == '\\' && q != '`' && i+1 < len(expr):
				i++
				b.WriteByte(expr[i])
			case expr[i] == q:
				i++
				return nil
			}
		}
		return errors.New("a string doesn't end")
	}
	for i < len(expr) {
		c := expr[i]
		if listNext && c != '(' && c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != '#' {
			listNext = false // group_left with no labels
		}
		switch {
		case c == '"' || c == '\'' || c == '`':
			if err := str(); err != nil {
				return "", err
			}
		case c == '#':
			for i < len(expr) && expr[i] != '\n' {
				b.WriteByte(expr[i])
				i++
			}
		case c == '{':
			// A selector's matchers: copy them, then add ours.
			start := b.Len()
			b.WriteByte(c)
			for i++; ; {
				if i >= len(expr) {
					return "", errors.New("a { isn't closed")
				}
				if q := expr[i]; q == '"' || q == '\'' || q == '`' {
					if err := str(); err != nil {
						return "", err
					}
					continue
				}
				if expr[i] == '}' {
					break
				}
				b.WriteByte(expr[i])
				i++
			}
			inner := strings.TrimSpace(b.String()[start+1:])
			if inner != "" && !strings.HasSuffix(inner, ",") {
				b.WriteByte(',')
			}
			b.WriteString(matchers + "}")
			i++
		case c == '[':
			// A range or a subquery's window and step.
			end := strings.IndexByte(expr[i:], ']')
			if end < 0 {
				return "", errors.New("a [ isn't closed")
			}
			b.WriteString(expr[i : i+end+1])
			i += end + 1
		case c == '(':
			parens = append(parens, listNext)
			listNext = false
			b.WriteByte(c)
			i++
		case c == ')':
			if len(parens) == 0 {
				return "", errors.New("a ) wasn't opened")
			}
			parens = parens[:len(parens)-1]
			b.WriteByte(c)
			i++
		case c >= '0' && c <= '9' || c == '.':
			// A number or a duration: 5, 0.5, 1e-3, 0x1f, 5m, 1h30m.
			start := i
			hex := strings.HasPrefix(strings.ToLower(expr[i:]), "0x")
			for i < len(expr) {
				d := expr[i]
				alnum := d >= '0' && d <= '9' || d >= 'a' && d <= 'z' || d >= 'A' && d <= 'Z' || d == '.' || d == '_'
				exp := i > start && (d == '+' || d == '-') && (expr[i-1] == 'e' || expr[i-1] == 'E') && !hex
				if !alnum && !exp {
					break
				}
				b.WriteByte(d)
				i++
			}
		case c == '_' || c == ':' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(expr) && (expr[j] == '_' || expr[j] == ':' || expr[j] >= 'a' && expr[j] <= 'z' ||
				expr[j] >= 'A' && expr[j] <= 'Z' || expr[j] >= '0' && expr[j] <= '9') {
				j++
			}
			word := expr[i:j]
			b.WriteString(word)
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
				b.WriteString("{" + matchers + "}")
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	if len(parens) > 0 {
		return "", fmt.Errorf("%d ( not closed", len(parens))
	}
	return b.String(), nil
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
