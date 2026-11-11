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
// are dropped. One longer than maxExpr has none: it isn't read.
func tokens(expr string) []token {
	if len(expr) > maxExpr {
		return nil
	}
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
			// To the ], or to the end when there is none.
			j := strings.IndexByte(expr[i+1:], ']')
			if j < 0 {
				j = len(expr) - i - 1
			}
			out = append(out, token{'r', expr[i+1 : i+1+j]})
			i += j + 2
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
	if len(expr) > maxExpr {
		return expr
	}
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

// lexed is an expression's tokens, with its parentheses matched once, so
// each check reads it in one pass whatever its depth.
type lexed struct {
	ts []token
	// match is, for each (, the index of the ) that closes it; -1 for one
	// never closed, and for every other token.
	match []int
	// in is, for each token, the index of the ( it is directly inside; -1
	// at the top.
	in []int
}

func lex(expr string) *lexed {
	ts := tokens(expr)
	e := &lexed{ts: ts, match: make([]int, len(ts)), in: make([]int, len(ts))}
	var open []int
	for i, t := range ts {
		e.match[i], e.in[i] = -1, -1
		if len(open) > 0 {
			e.in[i] = open[len(open)-1]
		}
		if t.kind != 'o' {
			continue
		}
		switch t.text {
		case "(":
			open = append(open, i)
		case ")":
			if len(open) > 0 {
				e.match[open[len(open)-1]] = i
				open = open[:len(open)-1]
			}
		}
	}
	return e
}

// closing is the index of the ) that closes the ( at i, if it is before
// end; -1 otherwise.
func (e *lexed) closing(i, end int) int {
	if j := e.match[i]; j >= 0 && j < end {
		return j
	}
	return -1
}

// unwrap drops the parentheses around the whole of ts[lo:hi].
func (e *lexed) unwrap(lo, hi int) (int, int) {
	for hi-lo > 2 && e.ts[lo].text == "(" && e.closing(lo, hi) == hi-1 {
		lo, hi = lo+1, hi-1
	}
	return lo, hi
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
	e := lex(expr)
	ts := e.ts
	lo, hi := e.unwrap(0, len(ts))
	// One comparison to a number, at the top, and nothing else.
	cut := -1
	for i := lo; i < hi; i++ {
		t := ts[i]
		if t.kind == 'o' && t.text == "(" {
			if i = e.closing(i, hi); i < 0 {
				return nil, false
			}
			continue
		}
		if t.kind == 'o' && t.text == "{" {
			for i < hi && ts[i].text != "}" {
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
		rest := ts[cut+1 : hi]
		if len(rest) > 0 && rest[0].kind == 'i' && rest[0].text == "bool" {
			rest = rest[1:]
		}
		if len(rest) > 0 && rest[0].text == "-" {
			rest = rest[1:]
		}
		if len(rest) != 1 || rest[0].kind != 'n' {
			return nil, false
		}
		lo, hi = e.unwrap(lo, cut)
	}
	if hi-lo < 3 || ts[lo].kind != 'i' || !groupedBy[ts[lo].text] {
		return nil, false
	}
	lo++
	labels = map[string]bool{}
	grouping := func() bool {
		if lo >= hi || ts[lo].kind != 'i' {
			return true
		}
		switch ts[lo].text {
		case "without":
			return false
		case "by":
		default:
			return true
		}
		if hi-lo < 2 || ts[lo+1].text != "(" {
			return false
		}
		end := e.closing(lo+1, hi)
		if end < 0 {
			return false
		}
		for _, t := range ts[lo+2 : end] {
			switch {
			case t.kind == 'i':
				labels[t.text] = true
			case t.kind == 's':
				labels[strings.Trim(t.text, "\"'`")] = true
			case t.text != ",":
				return false
			}
		}
		lo = end + 1
		return true
	}
	if !grouping() || lo >= hi || ts[lo].text != "(" {
		return nil, false
	}
	end := e.closing(lo, hi)
	if end < 0 {
		return nil, false
	}
	lo = end + 1
	if !grouping() || lo != hi {
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
	e := lex(expr)
	ts := e.ts
	var out []RangeUse
	for j, t := range ts {
		// A range directly inside the parentheses of rate(...), closed.
		p := e.in[j]
		if t.kind != 'r' || p < 1 || e.match[p] < 0 || strings.Contains(t.text, ":") {
			continue
		}
		f := ts[p-1]
		if f.kind != 'i' || !rateLike[f.text] {
			continue
		}
		text := strings.Join(strings.Fields(t.text), "")
		if d, ok := ParseDuration(text); ok {
			out = append(out, RangeUse{Func: f.text, Text: text, Range: d})
		}
	}
	return out
}

// Absent is whether an expression is absent(...), and only that.
func Absent(expr string) bool {
	e := lex(expr)
	lo, hi := e.unwrap(0, len(e.ts))
	ts := e.ts
	return hi-lo >= 3 && ts[lo].kind == 'i' && ts[lo].text == "absent" && ts[lo+1].text == "(" && e.closing(lo+1, hi) == hi-1
}

var promDur = regexp.MustCompile(`^(?:(\d+)y)?(?:(\d+)w)?(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?(?:(\d+)ms)?$`)

// ParseDuration reads a Prometheus duration: 5m, 1h30m, 2d.
func ParseDuration(s string) (time.Duration, bool) {
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
