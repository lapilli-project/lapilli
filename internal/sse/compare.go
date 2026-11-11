package sse

import (
	"math"
	"strconv"
	"strings"
)

// Format writes an expression back as math, with brackets where its
// operators need them: $A, abs($A - $B) > 5 && $C < 1.
func Format(e Expr) string {
	var b strings.Builder
	format(&b, e, 0)
	return b.String()
}

func format(b *strings.Builder, e Expr, outer int) {
	switch x := e.(type) {
	case number:
		v := float64(x)
		switch {
		case math.IsInf(v, 1):
			b.WriteString("(1/0)")
		case math.IsInf(v, -1):
			b.WriteString("(-1/0)")
		case math.IsNaN(v):
			b.WriteString("(0/0)")
		case v < 0:
			b.WriteString("(" + strconv.FormatFloat(v, 'g', -1, 64) + ")")
		default:
			b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
		}
	case ref:
		if isPlain(string(x)) {
			b.WriteString("$" + string(x))
		} else {
			b.WriteString("${" + string(x) + "}")
		}
	case unaryOp:
		b.WriteString(x.op)
		format(b, x.e, 7)
	case call:
		b.WriteString(x.name + "(")
		format(b, x.arg, 0)
		b.WriteString(")")
	case binary:
		p := prec[x.op]
		if p < outer {
			b.WriteString("(")
		}
		// The side that binds the other way round gets brackets: a - (b - c),
		// (a ^ b) ^ c.
		left, right := p, p+1
		if x.op == "^" {
			left, right = p+1, p
		}
		format(b, x.l, left)
		b.WriteString(" " + x.op + " ")
		format(b, x.r, right)
		if p < outer {
			b.WriteString(")")
		}
	case forced:
		format(b, x.e, outer)
	default:
		b.WriteString("?")
	}
}

// isPlain is a ref that $ alone can be written with: ASCII letters,
// digits and _, as the lexer reads a ref byte by byte.
func isPlain(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x80 || !isWord(rune(c)) {
			return false
		}
	}
	return s != ""
}

// A Cmp is a comparison of an expression with a number in a condition.
type Cmp struct {
	Value Expr
	Op    string // >, >=, < or <=, as if the number were on the right
	N     float64
}

// Comparisons are the comparisons of an expression with a number that
// decide a condition, in order: the condition's own, when it is one, or,
// of a condition joining others with && and ||, each one's, and so on
// down. A comparison under ! or inside arithmetic isn't one: it decides
// the other way round, or not alone.
func Comparisons(e Expr) []Cmp {
	var out []Cmp
	walkCmp(e, func(c Cmp, _ []bool) { out = append(out, c) })
	return out
}

// walkCmp calls f with each comparison Comparisons finds, and the way to
// it: false for the left of each && or || on the way, true for the right.
func walkCmp(e Expr, f func(Cmp, []bool)) {
	var walk func(e Expr, path []bool)
	walk = func(e Expr, path []bool) {
		if v, op, n, ok := Compared(e); ok {
			f(Cmp{v, op, n}, path)
			return
		}
		if b, ok := e.(binary); ok && (b.op == "&&" || b.op == "||") {
			walk(b.l, append(path[:len(path):len(path)], false))
			walk(b.r, append(path[:len(path):len(path)], true))
		}
	}
	walk(e, nil)
}

// at replaces the comparison at the end of path with what with makes of
// it.
func at(e Expr, path []bool, with func(Expr) Expr) Expr {
	if len(path) == 0 {
		return with(e)
	}
	b := e.(binary)
	if path[0] {
		b.r = at(b.r, path[1:], with)
	} else {
		b.l = at(b.l, path[1:], with)
	}
	return b
}

// pathOf is the way to comparison i, as Comparisons counts them.
func pathOf(e Expr, i int) ([]bool, Cmp, bool) {
	n := 0
	var found []bool
	var cmp Cmp
	ok := false
	walkCmp(e, func(c Cmp, path []bool) {
		if n == i {
			found, cmp, ok = path, c, true
		}
		n++
	})
	return found, cmp, ok
}

// SetNumber is the expression with comparison i's number, as Comparisons
// counts them, set to v; the expression itself when it has no comparison
// i.
func SetNumber(e Expr, i int, v float64) Expr {
	path, c, ok := pathOf(e, i)
	if !ok {
		return e
	}
	return at(e, path, func(Expr) Expr { return Compare(c.Value, c.Op, v) })
}

// Decides is an expression giving, for each series of a condition, the
// value comparison i compares where the condition turns on it: holding with
// the comparison holding, and not without. Where the condition holds
// either way it gives +Inf for a comparison that holds above its number,
// -Inf for one below; where it holds neither way, NaN. ok is false when e
// has no comparison i.
func Decides(e Expr, i int) (Expr, bool) {
	path, c, ok := pathOf(e, i)
	if !ok {
		return nil, false
	}
	force := func(v float64) Expr {
		return at(e, path, func(cmp Expr) Expr { return forced{cmp, v} })
	}
	return decides{yes: force(1), no: force(0), value: c.Value, above: c.Op == ">" || c.Op == ">="}, true
}

// forced is a comparison that gives v for each series it compares,
// whatever its values: the labels it gives, without what it says.
type forced struct {
	e Expr
	v float64
}

func (f forced) Eval(refs func(string) (Set, bool)) Set {
	in := f.e.Eval(refs)
	out := make(Set, len(in))
	for i, it := range in {
		out[i] = Item{Labels: it.Labels, Key: it.Key, V: f.v}
	}
	return out
}
func (f forced) Refs() []string { return f.e.Refs() }

type decides struct {
	yes, no, value Expr
	above          bool
}

func (d decides) Refs() []string { return append(d.yes.Refs(), d.value.Refs()...) }

func (d decides) Eval(refs func(string) (Set, bool)) Set {
	// yes and no have the same series, in the same order: they are the
	// condition with only what one comparison says changed.
	yes, no := d.yes.Eval(refs), d.no.Eval(refs)
	both := make(Set, 0, len(yes))
	for i, it := range yes {
		if i >= len(no) {
			break
		}
		both = append(both, Item{Labels: it.Labels, Key: it.Key, V: b2f(it.V != 0)*2 + b2f(no[i].V != 0)})
	}
	best := math.Inf(1)
	if !d.above {
		best = math.Inf(-1)
	}
	return Join(both, d.value.Eval(refs), func(c, v float64) float64 {
		switch c {
		case 3: // either way
			return best
		case 2: // only with it
			return v
		}
		return math.NaN()
	})
}
