package rules

import (
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"lapilli.dev/internal/sse"
)

// NearBy is how close a rule's value has to come to the number its
// condition compares it with, as a share of that number, to have nearly
// caught an incident.
const NearBy = 0.2

// A Bound is a number a rule's condition compares a value with: the
// comparison holds while the value is above it (> or >=) or below it (< or
// <=).
type Bound struct {
	Op    string
	Value float64
	// At is which of the condition's numbers it is, as Bounds counts them,
	// those it leaves out too.
	At int
	// Of is the expression compared, cut short, when the comparison is an
	// operand of and, or or unless; "" when it is the whole condition.
	Of string
	// Unless is a comparison on the right of unless: the condition holds the
	// more, the less it does.
	Unless bool
	// Rest is a Grafana range's other end: the condition holds when the value
	// meets both ends, or, when Either, one of them.
	Rest   *Bound
	Either bool
	// Values is whether ValueOf reads the values it compares.
	Values bool
}

// Holds reports whether v meets the bound's own comparison, whatever the
// rest of the condition says.
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

// Whole reports whether the condition, as far as it compares with b,
// holds for v with b's number at n: for a range, with its other end as it
// is.
func (b Bound) Whole(n, v float64) bool {
	own := Bound{Op: b.Op, Value: n}.Holds(v)
	switch {
	case b.Rest == nil:
		return own
	case b.Either:
		return own || b.Rest.Holds(v)
	}
	return own && b.Rest.Holds(v)
}

// Above reports whether the comparison holds above the bound.
func (b Bound) Above() bool { return b.Op == ">" || b.Op == ">=" }

// Lowers reports whether the condition holds more with a lower number:
// above it, or, on the right of unless, below it.
func (b Bound) Lowers() bool { return b.Above() != b.Unless }

// Bounds are the numbers a rule's condition compares a value with:
//
//   - in a Prometheus rule's expression, a comparison of an expression with
//     a number, without bool, outside brackets; of a set operation (and, or,
//     unless), each operand's, and so on down, whole operands in brackets
//     too;
//   - a Grafana rule decided by a threshold: its one number, or a range's
//     two; or by a math expression that compares an expression with a
//     number.
//
// A number past what a float holds to the full is none: there is no share
// of it to come within.
func (r Rule) Bounds() []Bound {
	if r.Unsupported != "" {
		return nil
	}
	var out []Bound
	if r.Graph == nil {
		cs := comparisons(r.Expr)
		for i, c := range cs {
			b := Bound{Op: c.op, Value: c.n, At: i, Unless: c.unless, Values: c.values}
			if c.part {
				b.Of = short(c.value)
			}
			out = append(out, b)
		}
	} else if st, ok := r.Graph.Steps[r.Graph.Cond]; ok {
		switch {
		case st.Threshold != nil:
			t := st.Threshold
			if op, one := thresholdOps[t.Type]; one {
				out = append(out, Bound{Op: op, Value: t.A, Values: true})
			} else if rg, ok := rangeOps[t.Type]; ok && fine(t.A) && fine(t.B) {
				out = append(out,
					Bound{Op: rg.lo, Value: t.A, Rest: &Bound{Op: rg.hi, Value: t.B}, Either: rg.either, Values: true},
					Bound{Op: rg.hi, Value: t.B, At: 1, Rest: &Bound{Op: rg.lo, Value: t.A}, Either: rg.either, Values: true})
			}
		case st.Math != nil:
			cs := sse.Comparisons(st.Math)
			for i, c := range cs {
				b := Bound{Op: c.Op, Value: c.N, At: i, Values: true}
				if len(cs) > 1 {
					b.Of = short(sse.Format(c.Value))
				}
				out = append(out, b)
			}
		case st.Classic != nil:
			out = append(out, st.Classic.bounds()...)
		}
	}
	return slices.DeleteFunc(out, func(b Bound) bool { return !fine(b.Value) })
}

// bounds are the numbers of classic conditions that compare above or below
// one, by their place among all the conditions' numbers: each named by its
// reducer and query, as avg(A); a range's two ends in order, compared
// again by replaying.
func (c *Classic) bounds() []Bound {
	var out []Bound
	at := 0
	for _, cd := range c.Conds {
		of := cd.Reducer + "(" + cd.Query + ")"
		if cd.Instant {
			of = cd.Query
		}
		if op, one := thresholdOps[cd.Type]; one && len(cd.Params) > 0 {
			out = append(out, Bound{Op: op, Value: cd.Params[0], At: at, Of: of, Values: true})
		} else if rg, ok := rangeOps[cd.Type]; ok && len(cd.Params) == 2 && cd.Params[0] <= cd.Params[1] {
			a, b := cd.Params[0], cd.Params[1]
			out = append(out,
				Bound{Op: rg.lo, Value: a, At: at, Of: of, Rest: &Bound{Op: rg.hi, Value: b}, Either: rg.either},
				Bound{Op: rg.hi, Value: b, At: at + 1, Of: of, Rest: &Bound{Op: rg.lo, Value: a}, Either: rg.either})
		}
		at += len(cd.Params)
	}
	return out
}

// param is which condition and which of its numbers a bound's At is.
func (c *Classic) param(at int) (cond, i int, ok bool) {
	for j, cd := range c.Conds {
		if at < len(cd.Params) {
			return j, at, true
		}
		at -= len(cd.Params)
	}
	return 0, 0, false
}

// fine is a number a float holds to the full, or nothing.
func fine(v float64) bool {
	return v == 0 || math.Abs(v) >= 0x1p-1022 && math.Abs(v) <= math.MaxFloat64
}

// ValueOf is, for a bound with Values, the rule whose condition returns the
// values it compares, as far as any could come within NearBy of it: a
// Prometheus rule with the number at its Furthest, so the history returns
// those past that where the rest of the expression holds; a Grafana rule
// decided by its threshold's input, or by its math expression without the
// comparison. ok is false for a bound without Values.
func (r Rule) ValueOf(b Bound) (Rule, bool) {
	if !b.Values || r.Unsupported != "" {
		return Rule{}, false
	}
	if r.Graph == nil {
		return r.WithBound(b, b.Furthest()), true
	}
	value := r
	value.Labels = maps.Clone(r.Labels)
	g := *r.Graph
	st := g.Steps[g.Cond]
	switch {
	case st.Threshold != nil:
		g.Cond = st.Threshold.Input
	case st.Math != nil && len(sse.Comparisons(st.Math)) == 1:
		e, _, _, ok := sse.Compared(st.Math)
		if !ok {
			return Rule{}, false
		}
		g.Steps = maps.Clone(g.Steps)
		g.Steps[g.Cond] = Step{Math: e}
	case st.Math != nil:
		e, ok := sse.Decides(st.Math, b.At)
		if !ok {
			return Rule{}, false
		}
		g.Steps = maps.Clone(g.Steps)
		g.Steps[g.Cond] = Step{Math: e}
	case st.Classic != nil:
		cond, _, ok := st.Classic.param(b.At)
		if !ok {
			return Rule{}, false
		}
		c := *st.Classic
		c.Of = cond + 1
		g.Steps = maps.Clone(g.Steps)
		g.Steps[g.Cond] = Step{Classic: &c}
	default:
		return Rule{}, false
	}
	value.Graph = &g
	return value, true
}

// WithBound is the rule with b's number set to v; the rule itself when b
// isn't one of its Bounds.
func (r Rule) WithBound(b Bound, v float64) Rule {
	if !slices.ContainsFunc(r.Bounds(), func(x Bound) bool { return x.At == b.At && x.Op == b.Op }) {
		return r
	}
	n := strconv.FormatFloat(v, 'g', -1, 64)
	out := r
	if r.Graph == nil {
		c := comparisons(r.Expr)[b.At]
		// A number written up against a word, as in and+5, stays apart from
		// it without its sign.
		if c.at > 0 && isWordByte(r.Expr[c.at-1]) {
			n = " " + n
		}
		if c.end < len(r.Expr) && (isWordByte(r.Expr[c.end]) || r.Expr[c.end] == '.') {
			n += " "
		}
		out.Expr = r.Expr[:c.at] + n + r.Expr[c.end:]
		return out
	}
	g := *r.Graph
	g.Steps = maps.Clone(r.Graph.Steps)
	st := g.Steps[g.Cond]
	switch {
	case st.Threshold != nil:
		t := *st.Threshold
		if b.At == 1 {
			t.B = v
		} else {
			t.A = v
		}
		st.Threshold = &t
	case st.Classic != nil:
		cond, i, _ := st.Classic.param(b.At)
		c := *st.Classic
		c.Conds = slices.Clone(c.Conds)
		c.Conds[cond].Params = slices.Clone(c.Conds[cond].Params)
		c.Conds[cond].Params[i] = v
		st.Classic = &c
	default:
		st.Math = sse.SetNumber(st.Math, b.At, v)
	}
	g.Steps[g.Cond] = st
	out.Graph = &g
	// Grafana starts a rule's alerts afresh when it changes; a version with
	// the new number is another rule.
	out.Digest = r.Digest + " " + strconv.Itoa(b.At) + b.Op + n
	return out
}

var thresholdOps = map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<="}

// rangeOps are how a range's two ends compare: lo the first value's, hi the
// second's, and whether the value need meet only one of them.
var rangeOps = map[string]struct {
	lo, hi string
	either bool
}{
	"within_range": {">", "<", false}, "within_range_included": {">=", "<=", false},
	"outside_range": {"<", ">", true}, "outside_range_included": {"<=", ">=", true},
}

// comparison is a comparison with a number in a PromQL expression: the
// other side, the operator as if the number were on the right, the number,
// where the number is written, whether it is on the right of unless,
// whether the expression returns the other side's values where it holds,
// and whether it is an operand of a set operation.
type comparison struct {
	value                string
	op                   string
	n                    float64
	at, end              int
	unless, values, part bool
}

// How much of an expression is read for its comparisons: each level of
// brackets reads the text inside again, so a long or deep one isn't.
const (
	maxExpr  = 16 << 10
	maxDepth = 32
)

// comparisons are a PromQL expression's comparisons with a number: its
// own, when it is one, or else each operand's of a set operation, and so on
// down, so far. A comparison in brackets otherwise is part of a value.
func comparisons(expr string) []comparison {
	if len(expr) > maxExpr {
		return nil
	}
	var out []comparison
	operands(expr, 0, 0, false, true, false, &out)
	return out
}

// operands adds the comparisons of expr, which starts at off in the whole
// expression; unless says it is on the right of unless, values that the
// whole returns its values, and part that it is an operand.
func operands(expr string, off, depth int, unless, values, part bool, out *[]comparison) {
	lo, hi, ok := trimmed(expr)
	if !ok || lo >= hi || depth > maxDepth {
		return
	}
	depth++
	if expr[lo] == '(' && closing(expr, lo) == hi-1 {
		operands(expr[lo+1:hi-1], off+lo+1, depth, unless, values, part, out)
		return
	}
	ops, ok := setOps(expr)
	if !ok {
		return
	}
	if len(ops) == 0 {
		if c, ok := compared(expr); ok {
			c.at, c.end, c.unless, c.values, c.part = c.at+off, c.end+off, unless, values, part
			*out = append(*out, c)
		}
		return
	}
	// or binds least, and returns the values of either side.
	if slices.ContainsFunc(ops, func(o setOp) bool { return o.word == "or" }) {
		start := 0
		for _, o := range ops {
			if o.word == "or" {
				operands(expr[start:o.at], off+start, depth, unless, false, true, out)
				start = o.end
			}
		}
		operands(expr[start:], off+start, depth, unless, false, true, out)
		return
	}
	// and and unless return the values of their left.
	start, against := 0, unless
	for i, o := range ops {
		operands(expr[start:o.at], off+start, depth, against, values && i == 0, true, out)
		start, against = o.end, unless != (o.word == "unless")
	}
	operands(expr[start:], off+start, depth, against, false, true, out)
}

// walk calls f with the offset of each byte of expr outside comments and
// spaces, and the depth of brackets it is at: a bracket at the depth
// outside it, and a string as its two quotes. ok is false when a string
// doesn't end or a bracket isn't matched.
func walk(expr string, f func(i, depth int)) bool {
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
				return false
			}
			f(i, depth)
			f(j, depth)
			i = j
		case c == '(' || c == '[' || c == '{':
			f(i, depth)
			depth++
		case c == ')' || c == ']' || c == '}':
			if depth--; depth < 0 {
				return false
			}
			f(i, depth)
		case c < 0x80 && unicode.IsSpace(rune(c)):
		default:
			f(i, depth)
		}
	}
	return depth == 0
}

// trimmed is where expr's text is, comments and spaces aside.
func trimmed(expr string) (lo, hi int, ok bool) {
	lo = -1
	ok = walk(expr, func(i, _ int) {
		if lo < 0 {
			lo = i
		}
		hi = i + 1
	})
	return max(lo, 0), hi, ok
}

// closing is where the bracket at open closes; -1 when it doesn't.
func closing(expr string, open int) int {
	at, depth := -1, -1
	walk(expr, func(i, d int) {
		switch {
		case i == open:
			depth = d
		case at < 0 && depth >= 0 && i > open && d == depth && strings.IndexByte(")]}", expr[i]) >= 0:
			at = i
		}
	})
	return at
}

// A setOp is and, or or unless outside brackets, with its on(...) or
// ignoring(...).
type setOp struct {
	word    string
	at, end int
}

// setOps are expr's set operators outside brackets, in order.
func setOps(expr string) ([]setOp, bool) {
	var ops []setOp
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
				return nil, false
			}
			i = j
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			if depth--; depth < 0 {
				return nil, false
			}
		case depth == 0 && isWordByte(c):
			j := i
			for j < len(expr) && isWordByte(expr[j]) {
				j++
			}
			word := strings.ToLower(expr[i:j])
			if word != "and" && word != "or" && word != "unless" {
				i = j - 1
				continue
			}
			op := setOp{word, i, j}
			// A matching modifier belongs to the operator.
			k := skipSpace(expr, j)
			m := k
			for m < len(expr) && isWordByte(expr[m]) {
				m++
			}
			if w := strings.ToLower(expr[k:m]); w == "on" || w == "ignoring" {
				p := skipSpace(expr, m)
				if p < len(expr) && expr[p] == '(' {
					if q := strings.IndexByte(expr[p:], ')'); q >= 0 {
						op.end = p + q + 1
					}
				}
			}
			ops = append(ops, op)
			i = op.end - 1
		}
	}
	return ops, depth == 0
}

// skipSpace is where the text after i starts, spaces and comments aside.
func skipSpace(expr string, i int) int {
	for i < len(expr) {
		switch c := expr[i]; {
		case c == '#':
			for i < len(expr) && expr[i] != '\n' {
				i++
			}
		case c < 0x80 && unicode.IsSpace(rune(c)):
			i++
		default:
			return i
		}
	}
	return i
}

// short is an expression as a person reads it in a line: without comments,
// its spaces one, and cut at forty characters.
func short(expr string) string {
	var b strings.Builder
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		switch {
		case c == '#':
			for i < len(expr) && expr[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
			continue
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for ; j < len(expr) && expr[j] != c; j++ {
				if expr[j] == '\\' && c != '`' {
					j++
				}
			}
			j = min(j, len(expr)-1)
			b.WriteString(expr[i : j+1])
			i = j
			continue
		}
		b.WriteByte(c)
	}
	s := []rune(strings.Join(strings.Fields(b.String()), " "))
	if len(s) > 40 {
		return string(s[:39]) + "…"
	}
	return string(s)
}

// compared reads a PromQL expression with no set operation outside
// brackets as a comparison with a number, if it is one. It reads only as
// much PromQL as that takes: brackets, strings, comments and the words
// and, or, unless and bool.
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
		return comparison{value: strings.TrimSpace(left), op: s, n: rn, at: o.end + at, end: o.end + end}, true
	case lok && !rok:
		at, end := span(left)
		return comparison{value: strings.TrimSpace(right), op: flip[s], n: ln, at: at, end: end}, true
	}
	return comparison{}, false
}

// number reads a side of a comparison that is a plain number, with nothing
// after it but comments.
func number(s string) (float64, bool) {
	at, end := span(s)
	if at < 0 {
		return 0, false
	}
	for _, line := range strings.Split(s[end:], "\n") {
		if c, _, _ := strings.Cut(line, "#"); strings.TrimSpace(c) != "" {
			return 0, false
		}
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

// Round is the round number nearest v that v meets, on the bound's side: in
// steps of a tenth of the bound's leading figure, 0.87 short of a bound of
// 0.9, 480 short of 500.
func Round(v float64, b Bound) float64 {
	at, x := b.grid(v)
	k := math.Floor(x)
	if !b.Above() {
		k = math.Ceil(x)
	}
	step := 1.0
	if b.Above() {
		step = -1
	}
	for i := 0; i < 3 && !(Bound{Op: b.Op, Value: at(k)}).Holds(v); i++ {
		k += step
	}
	return at(k)
}

// Furthest is the round number furthest from b, the way it would hold
// more, that is within NearBy of it: 0.72 for > 0.9, 120 for < 100.
func (b Bound) Furthest() float64 {
	edge := b.Value - NearBy*math.Abs(b.Value)
	if !b.Lowers() {
		edge = b.Value + NearBy*math.Abs(b.Value)
	}
	at, x := b.grid(edge)
	if b.Lowers() {
		return at(math.Ceil(x))
	}
	return at(math.Floor(x))
}

// Steps are the round numbers from b, not b's own, to its Furthest, the
// nearest first: 0.89, 0.88 … 0.72 for > 0.9.
func (b Bound) Steps() []float64 {
	at, x := b.grid(b.Value)
	_, far := b.grid(b.Furthest())
	far = math.Round(far)
	var out []float64
	if b.Lowers() {
		for k := math.Ceil(x) - 1; k >= far && len(out) < 30; k-- {
			out = append(out, at(k))
		}
	} else {
		for k := math.Floor(x) + 1; k <= far && len(out) < 30; k++ {
			out = append(out, at(k))
		}
	}
	return out
}

// grid is the bound's round numbers, k of them from nothing at(k), and v
// in them.
func (b Bound) grid(v float64) (at func(k float64) float64, x float64) {
	exp := 0
	if b.Value != 0 {
		exp = int(math.Floor(math.Log10(math.Abs(b.Value)))) - 1
	}
	at = func(k float64) float64 {
		if exp < 0 {
			return k / math.Pow10(-exp)
		}
		return k * math.Pow10(exp)
	}
	x = v / at(1)
	if r := math.Round(x); math.Abs(x-r) < 1e-9*math.Max(1, math.Abs(x)) {
		x = r // 0.29 / 0.01 is 28.999999999999996
	}
	return at, x
}
