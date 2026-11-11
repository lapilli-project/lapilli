package content

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// What the checks need of an expression is read from its tokens, not a
// full parse: where a shape isn't one of the few looked for, nothing is
// guessed.

type token struct {
	kind byte // i an identifier, n a number, s a string, r a range ([...]), o anything else
	text string
}

// tokens splits an expression into PromQL's tokens; comments and spaces
// are dropped.
func tokens(expr string) []token {
	var out []token
	for i := 0; i < len(expr); {
		c := expr[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '#':
			for i < len(expr) && expr[i] != '\n' {
				i++
			}
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for j < len(expr) && expr[j] != c {
				if expr[j] == '\\' && c != '`' {
					j++
				}
				j++
			}
			j = min(j+1, len(expr))
			out = append(out, token{'s', expr[i:j]})
			i = j
		case c == '[':
			j := strings.IndexByte(expr[i:], ']')
			if j < 0 {
				j = len(expr) - i - 1
			}
			out = append(out, token{'r', expr[i+1 : i+j]})
			i += j + 1
		case isLetter(c):
			j := i
			for j < len(expr) && (isLetter(expr[j]) || isDigit(expr[j])) {
				j++
			}
			out = append(out, token{'i', expr[i:j]})
			i = j
		case isDigit(c) || c == '.' && i+1 < len(expr) && isDigit(expr[i+1]):
			j := i
			for j < len(expr) && (isLetter(expr[j]) || isDigit(expr[j]) || expr[j] == '.') {
				j++
			}
			out = append(out, token{'n', expr[i:j]})
			i = j
		default:
			n := 1
			if i+1 < len(expr) {
				switch expr[i : i+2] {
				case "==", "!=", ">=", "<=", "=~", "!~":
					n = 2
				}
			}
			out = append(out, token{'o', expr[i : i+n]})
			i += n
		}
	}
	return out
}

func isLetter(c byte) bool {
	return c == '_' || c == ':' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// Squeeze is an expression with its spaces and comments aside, for telling
// two alike.
func Squeeze(expr string) string {
	ts := tokens(expr)
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = t.text
		if t.kind == 'r' {
			parts[i] = "[" + strings.Join(strings.Fields(t.text), "") + "]"
		}
	}
	return strings.Join(parts, " ")
}

// closing is the index of the bracket that closes the one at i, or -1.
func closing(ts []token, i int) int {
	depth := 0
	for j := i; j < len(ts); j++ {
		if ts[j].kind != 'o' {
			continue
		}
		switch ts[j].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// unwrap drops the parentheses around a whole expression.
func unwrap(ts []token) []token {
	for len(ts) > 2 && ts[0].text == "(" && closing(ts, 0) == len(ts)-1 {
		ts = ts[1 : len(ts)-1]
	}
	return ts
}

var binaryOps = map[string]bool{"+": true, "-": true, "*": true, "/": true, "%": true, "^": true,
	"==": true, "!=": true, ">": true, "<": true, ">=": true, "<=": true, "and": true, "or": true, "unless": true, "atan2": true}

var comparisons = map[string]bool{"==": true, "!=": true, ">": true, "<": true, ">=": true, "<=": true}

// groupedBy are the aggregations whose result has exactly the labels they
// are grouped by: not topk, bottomk, limitk or count_values.
var groupedBy = map[string]bool{"sum": true, "min": true, "max": true, "avg": true, "count": true, "group": true,
	"stddev": true, "stdvar": true, "quantile": true}

// Gives is the labels an expression's alerts can have, when it is an
// aggregation grouped by labels (sum by (service) (...)), with or without a
// comparison to a number; ok is false for any other expression.
func Gives(expr string) (labels map[string]bool, ok bool) {
	ts := unwrap(tokens(expr))
	// One comparison to a number, at the top, and nothing else.
	cut := -1
	for i := 0; i < len(ts); i++ {
		t := ts[i]
		if t.kind == 'o' && t.text == "(" {
			if i = closing(ts, i); i < 0 {
				return nil, false
			}
			continue
		}
		if t.kind == 'o' && t.text == "{" {
			for i < len(ts) && ts[i].text != "}" {
				i++
			}
			continue
		}
		if (t.kind == 'o' || t.kind == 'i') && binaryOps[t.text] {
			if cut >= 0 || !comparisons[t.text] {
				return nil, false
			}
			cut = i
		}
	}
	if cut >= 0 {
		rest := ts[cut+1:]
		if len(rest) > 0 && rest[0].kind == 'i' && rest[0].text == "bool" {
			rest = rest[1:]
		}
		if len(rest) > 0 && rest[0].text == "-" {
			rest = rest[1:]
		}
		if len(rest) != 1 || rest[0].kind != 'n' {
			return nil, false
		}
		ts = unwrap(ts[:cut])
	}
	if len(ts) < 3 || ts[0].kind != 'i' || !groupedBy[ts[0].text] {
		return nil, false
	}
	ts = ts[1:]
	labels = map[string]bool{}
	grouping := func() bool {
		if len(ts) == 0 || ts[0].kind != 'i' {
			return true
		}
		switch ts[0].text {
		case "without":
			return false
		case "by":
		default:
			return true
		}
		if len(ts) < 2 || ts[1].text != "(" {
			return false
		}
		end := closing(ts, 1)
		if end < 0 {
			return false
		}
		for _, t := range ts[2:end] {
			switch {
			case t.kind == 'i':
				labels[t.text] = true
			case t.kind == 's':
				labels[strings.Trim(t.text, "\"'`")] = true
			case t.text != ",":
				return false
			}
		}
		ts = ts[end+1:]
		return true
	}
	if !grouping() || len(ts) == 0 || ts[0].text != "(" {
		return nil, false
	}
	end := closing(ts, 0)
	if end < 0 {
		return nil, false
	}
	ts = ts[end+1:]
	if !grouping() || len(ts) != 0 {
		return nil, false
	}
	return labels, true
}

// A RangeUse is a range a rate-like function is taken over.
type RangeUse struct {
	Func  string
	Text  string // as written
	Range time.Duration
}

var rateLike = map[string]bool{"rate": true, "increase": true, "irate": true, "delta": true}

// Ranges are the ranges rate, increase, irate and delta are taken over in
// an expression, but subqueries'.
func Ranges(expr string) []RangeUse {
	ts := tokens(expr)
	var out []RangeUse
	for i := 0; i+1 < len(ts); i++ {
		if ts[i].kind != 'i' || !rateLike[ts[i].text] || ts[i+1].text != "(" {
			continue
		}
		end := closing(ts, i+1)
		if end < 0 {
			break
		}
		depth := 0
		for _, t := range ts[i+2 : end] {
			switch {
			case t.kind == 'o' && t.text == "(":
				depth++
			case t.kind == 'o' && t.text == ")":
				depth--
			case t.kind == 'r' && depth == 0 && !strings.Contains(t.text, ":"):
				text := strings.Join(strings.Fields(t.text), "")
				if d, ok := promParse(text); ok {
					out = append(out, RangeUse{Func: ts[i].text, Text: text, Range: d})
				}
			}
		}
	}
	return out
}

// Absent is whether an expression is absent(...), and only that.
func Absent(expr string) bool {
	ts := unwrap(tokens(expr))
	return len(ts) >= 3 && ts[0].kind == 'i' && ts[0].text == "absent" && ts[1].text == "(" && closing(ts, 1) == len(ts)-1
}

var promDur = regexp.MustCompile(`^(?:(\d+)y)?(?:(\d+)w)?(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?(?:(\d+)ms)?$`)

// promParse reads a Prometheus duration: 5m, 1h30m, 2d.
func promParse(s string) (time.Duration, bool) {
	m := promDur.FindStringSubmatch(s)
	if s == "" || m == nil {
		return 0, false
	}
	units := []time.Duration{365 * 24 * time.Hour, 7 * 24 * time.Hour, 24 * time.Hour, time.Hour, time.Minute, time.Second, time.Millisecond}
	var d time.Duration
	for i, u := range units {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		if err != nil || n > int64(200*365*24*time.Hour/u) {
			return 0, false
		}
		d += time.Duration(n) * u
	}
	return d, true
}

// promDuration writes a duration as Prometheus does.
func promDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}
