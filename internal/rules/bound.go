package rules

import (
	"maps"
	"math"
	"strconv"
	"strings"
	"unicode"

	"lapilli.dev/internal/sse"
)

// A Bound is the number a rule's condition compares a value with: the
// condition holds while the value is above it (> or >=) or below it (< or
// <=).
type Bound struct {
	Op    string  `json:"op"`
	Value float64 `json:"value"`
}

// Holds reports whether v meets the bound.
func (b Bound) Holds(v float64) bool {
	switch b.Op {
	case ">":
		return v > b.Value
	case ">=":
		return v >= b.Value
	case "<":
		return v < b.Value
	case "<=":
		return v <= b.Value
	}
	return false
}

// Above reports whether the condition holds above the bound.
func (b Bound) Above() bool { return b.Op == ">" || b.Op == ">=" }

// Bound finds the number a rule's condition compares a value with, and the
// rule whose condition returns that value instead, wherever it has one. ok
// is false when the condition isn't one such comparison:
//
//   - a Prometheus rule whose expression, outside brackets, compares an
//     expression with a number, once, without bool and without and, or or
//     unless;
//   - a Grafana rule decided by a threshold with one number, or by a math
//     expression that compares an expression with a number.
//
// A Grafana value that is 0 isn't returned, as a condition's isn't.
func (r Rule) Bound() (b Bound, value Rule, ok bool) {
	if r.Unsupported != "" {
		return Bound{}, Rule{}, false
	}
	value = r
	value.Labels = maps.Clone(r.Labels)
	if r.Graph == nil {
		c, ok := compared(r.Expr)
		if !ok {
			return Bound{}, Rule{}, false
		}
		value.Expr = c.value
		return Bound{c.op, c.n}, value, true
	}
	g := r.Graph
	st, ok := g.Steps[g.Cond]
	switch {
	case !ok:
		return Bound{}, Rule{}, false
	case st.Threshold != nil:
		op, one := thresholdOps[st.Threshold.Type]
		if !one {
			return Bound{}, Rule{}, false
		}
		vg := *g
		vg.Cond = st.Threshold.Input
		value.Graph = &vg
		return Bound{op, st.Threshold.A}, value, true
	case st.Math != nil:
		e, op, n, ok := sse.Compared(st.Math)
		if !ok {
			return Bound{}, Rule{}, false
		}
		vg := *g
		vg.Steps = maps.Clone(g.Steps)
		vg.Steps[g.Cond] = Step{Math: e}
		value.Graph = &vg
		return Bound{op, n}, value, true
	}
	return Bound{}, Rule{}, false
}

// WithBound is the rule with the number its condition compares with set to
// v; the rule itself when it has no Bound.
func (r Rule) WithBound(v float64) Rule {
	b, _, ok := r.Bound()
	if !ok {
		return r
	}
	out := r
	if r.Graph == nil {
		c, _ := compared(r.Expr)
		out.Expr = r.Expr[:c.at] + strconv.FormatFloat(v, 'g', -1, 64) + r.Expr[c.end:]
		return out
	}
	g := *r.Graph
	g.Steps = maps.Clone(r.Graph.Steps)
	st := g.Steps[g.Cond]
	if st.Threshold != nil {
		t := *st.Threshold
		t.A = v
		st.Threshold = &t
	} else {
		e, _, _, _ := sse.Compared(st.Math)
		st.Math = sse.Compare(e, b.Op, v)
	}
	g.Steps[g.Cond] = st
	out.Graph = &g
	// Grafana starts a rule's alerts afresh when it changes; a version with
	// the new number is another rule.
	out.Digest = r.Digest + " " + b.Op + strconv.FormatFloat(v, 'g', -1, 64)
	return out
}

var thresholdOps = map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<="}

// comparison is a PromQL expression's one comparison with a number: the
// other side, the operator as if the number were on the right, the number,
// and where the number is written.
type comparison struct {
	value   string
	op      string
	n       float64
	at, end int
}

// compared finds a PromQL expression's comparison with a number outside
// brackets, if it is one. It reads only as much PromQL as that takes:
// brackets, strings, comments and the words and, or, unless and bool.
func compared(expr string) (comparison, bool) {
	type op struct{ at, end int }
	var ops []op
	depth := 0
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		switch {
		case c == '#':
			for i < len(expr) && expr[i] != '\n' {
				i++
			}
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for ; j < len(expr) && expr[j] != c; j++ {
				if expr[j] == '\\' && c != '`' {
					j++
				}
			}
			if j >= len(expr) {
				return comparison{}, false
			}
			i = j
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			if depth--; depth < 0 {
				return comparison{}, false
			}
		case depth > 0:
		case c == '>' || c == '<' || c == '=' || c == '!':
			end := i + 1
			if end < len(expr) && expr[end] == '=' {
				end++
			}
			switch s := expr[i:end]; s {
			case ">", ">=", "<", "<=", "==", "!=":
				ops = append(ops, op{i, end})
			default:
				return comparison{}, false // "=" or "!" alone isn't PromQL here
			}
			i = end - 1
		case isWordByte(c):
			j := i
			for j < len(expr) && isWordByte(expr[j]) {
				j++
			}
			switch strings.ToLower(expr[i:j]) {
			case "and", "or", "unless":
				return comparison{}, false
			}
			i = j - 1
		}
	}
	if depth != 0 || len(ops) != 1 {
		return comparison{}, false
	}
	o := ops[0]
	s := expr[o.at:o.end]
	flip := map[string]string{">": "<", ">=": "<=", "<": ">", "<=": ">="}
	if _, ok := flip[s]; !ok {
		return comparison{}, false
	}
	left, right := expr[:o.at], expr[o.end:]
	if w := strings.Fields(right); len(w) > 0 && strings.EqualFold(w[0], "bool") {
		return comparison{}, false
	}
	ln, lok := number(left)
	rn, rok := number(right)
	switch {
	case rok && !lok:
		at, end := span(right)
		return comparison{strings.TrimSpace(left), s, rn, o.end + at, o.end + end}, true
	case lok && !rok:
		at, end := span(left)
		return comparison{strings.TrimSpace(right), flip[s], ln, at, end}, true
	}
	return comparison{}, false
}

// number reads a side of a comparison that is a plain number, with any
// comment after it.
func number(s string) (float64, bool) {
	at, end := span(s)
	if at < 0 || strings.TrimSpace(s[end:]) != "" && !strings.HasPrefix(strings.TrimSpace(s[end:]), "#") {
		return 0, false
	}
	n, err := strconv.ParseFloat(s[at:end], 64)
	if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
		return 0, false
	}
	return n, true
}

// span is where the first word of s is, -1 when there is none; a sign
// before it is part of it.
func span(s string) (int, int) {
	at := strings.IndexFunc(s, func(r rune) bool { return !unicode.IsSpace(r) })
	if at < 0 {
		return -1, -1
	}
	end := at
	for end < len(s) && (isWordByte(s[end]) || s[end] == '.' || end == at && (s[end] == '-' || s[end] == '+') ||
		(s[end] == '-' || s[end] == '+') && end > at && (s[end-1] == 'e' || s[end-1] == 'E')) {
		end++
	}
	return at, end
}

func isWordByte(c byte) bool {
	return c == '_' || c == ':' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
