// Package sse evaluates Grafana's server-side expressions — math, threshold
// and their joins — on numbers lapilli already fetched, the way Grafana does
// when a rule is evaluated: a comparison gives 1 or 0, and two sets of series
// are paired by their labels.
package sse

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Item is one series' value at one evaluation.
type Item struct {
	Labels map[string]string
	V      float64
	// Key is Labels as LabelKey writes them, made once per series so that
	// pairing items needn't; empty, it is worked out when needed.
	Key string
}

// LabelKey writes a label set as one string, equal for equal sets.
func LabelKey(l map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(l)) {
		b.WriteString(k + "=" + l[k] + "\x00")
	}
	return b.String()
}

func (it Item) key() string {
	if it.Key == "" && len(it.Labels) > 0 {
		return LabelKey(it.Labels)
	}
	return it.Key
}

// Set is the values of every series a query or expression returned.
type Set []Item

// scalar is a number with no labels, which pairs with every item.
func scalar(v float64) Set { return Set{{V: v}} }

// Expr is a parsed math expression.
type Expr interface {
	Eval(refs func(string) (Set, bool)) Set
	Refs() []string
}

// ParseMath parses a Grafana math expression such as
// "$A * ($B >= 20)" or "$D > 10 && ${F} > ($H + 5)".
func ParseMath(s string) (Expr, error) {
	p := &parser{toks: nil}
	if err := p.lex(s); err != nil {
		return nil, fmt.Errorf("math expression %q: %w", s, err)
	}
	e, err := p.expr(0)
	if err != nil {
		return nil, fmt.Errorf("math expression %q: %w", s, err)
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("math expression %q: unexpected %q", s, p.toks[p.pos].s)
	}
	return e, nil
}

type tok struct {
	kind byte // n number, r ref, o operator, f function, ( )
	s    string
	v    float64
}

type parser struct {
	toks []tok
	pos  int
}

var twoChar = []string{">=", "<=", "==", "!=", "&&", "||"}

func (p *parser) lex(s string) error {
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '$':
			j := i + 1
			var name string
			if j < len(s) && s[j] == '{' {
				k := strings.IndexByte(s[j:], '}')
				if k < 0 {
					return fmt.Errorf("unclosed ${")
				}
				name, i = s[j+1:j+k], j+k+1
			} else {
				for j < len(s) && (isWord(rune(s[j]))) {
					j++
				}
				name, i = s[i+1:j], j
			}
			if name == "" {
				return fmt.Errorf("a $ with no name")
			}
			p.toks = append(p.toks, tok{kind: 'r', s: name})
		case c >= '0' && c <= '9' || c == '.':
			j := i
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.' || s[j] == 'e' || s[j] == 'E' ||
				(j > i && (s[j] == '-' || s[j] == '+') && (s[j-1] == 'e' || s[j-1] == 'E'))) {
				j++
			}
			v, err := strconv.ParseFloat(s[i:j], 64)
			if err != nil {
				return fmt.Errorf("number %q", s[i:j])
			}
			p.toks = append(p.toks, tok{kind: 'n', s: s[i:j], v: v})
			i = j
		case c == '(' || c == ')' || c == ',':
			p.toks = append(p.toks, tok{kind: c, s: string(c)})
			i++
		case unicode.IsLetter(rune(c)):
			j := i
			for j < len(s) && isWord(rune(s[j])) {
				j++
			}
			p.toks = append(p.toks, tok{kind: 'f', s: s[i:j]})
			i = j
		default:
			op := string(c)
			for _, t := range twoChar {
				if strings.HasPrefix(s[i:], t) {
					op = t
				}
			}
			if !strings.Contains("+-*/%^><!", op[:1]) && len(op) == 1 {
				return fmt.Errorf("unexpected %q", op)
			}
			p.toks = append(p.toks, tok{kind: 'o', s: op})
			i += len(op)
		}
	}
	return nil
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

// Binding powers, loosest first, as in Grafana's (Go-like) grammar.
var prec = map[string]int{
	"||": 1, "&&": 2,
	"==": 3, "!=": 3, "<": 3, "<=": 3, ">": 3, ">=": 3,
	"+": 4, "-": 4,
	"*": 5, "/": 5, "%": 5,
	"^": 6,
}

func (p *parser) peek() *tok {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

// expr parses operators binding at least as tightly as minPrec, by
// precedence climbing.
func (p *parser) expr(minPrec int) (Expr, error) {
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t == nil || t.kind != 'o' {
			return left, nil
		}
		bp, ok := prec[t.s]
		if !ok {
			return nil, fmt.Errorf("unexpected %q", t.s)
		}
		if bp < minPrec {
			return left, nil
		}
		p.pos++
		next := bp + 1
		if t.s == "^" {
			next = bp // right-associative
		}
		right, err := p.expr(next)
		if err != nil {
			return nil, err
		}
		left = binary{op: t.s, l: left, r: right}
	}
}

func (p *parser) unary() (Expr, error) {
	t := p.peek()
	if t == nil {
		return nil, fmt.Errorf("ends too soon")
	}
	switch {
	case t.kind == 'o' && (t.s == "-" || t.s == "!"):
		p.pos++
		e, err := p.unary()
		if err != nil {
			return nil, err
		}
		return unaryOp{op: t.s, e: e}, nil
	case t.kind == 'n':
		p.pos++
		return number(t.v), nil
	case t.kind == 'r':
		p.pos++
		return ref(t.s), nil
	case t.kind == '(':
		p.pos++
		e, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		if t := p.peek(); t == nil || t.kind != ')' {
			return nil, fmt.Errorf("missing )")
		}
		p.pos++
		return e, nil
	case t.kind == 'f':
		fn, ok := funcs[t.s]
		if !ok {
			return nil, fmt.Errorf("function %s", t.s)
		}
		p.pos++
		if t := p.peek(); t == nil || t.kind != '(' {
			return nil, fmt.Errorf("%s needs (", t.s)
		}
		p.pos++
		arg, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		if t := p.peek(); t == nil || t.kind != ')' {
			return nil, fmt.Errorf("missing )")
		}
		p.pos++
		return call{name: t.s, fn: fn, arg: arg}, nil
	}
	return nil, fmt.Errorf("unexpected %q", t.s)
}

var funcs = map[string]func(float64) float64{
	"abs": math.Abs, "ceil": math.Ceil, "floor": math.Floor, "round": math.Round, "log": math.Log, "exp": math.Exp,
	"is_nan":  func(v float64) float64 { return b2f(math.IsNaN(v)) },
	"is_inf":  func(v float64) float64 { return b2f(math.IsInf(v, 0)) },
	"is_null": func(v float64) float64 { return b2f(math.IsNaN(v)) },
}

type number float64

func (n number) Eval(func(string) (Set, bool)) Set { return scalar(float64(n)) }
func (number) Refs() []string                      { return nil }

type ref string

func (r ref) Eval(refs func(string) (Set, bool)) Set {
	s, _ := refs(string(r))
	return s
}
func (r ref) Refs() []string { return []string{string(r)} }

type unaryOp struct {
	op string
	e  Expr
}

func (u unaryOp) Eval(refs func(string) (Set, bool)) Set {
	in := u.e.Eval(refs)
	out := make(Set, len(in))
	for i, it := range in {
		v := -it.V
		if u.op == "!" {
			v = b2f(it.V == 0)
		}
		out[i] = Item{Labels: it.Labels, Key: it.Key, V: v}
	}
	return out
}
func (u unaryOp) Refs() []string { return u.e.Refs() }

type call struct {
	name string
	fn   func(float64) float64
	arg  Expr
}

func (c call) Eval(refs func(string) (Set, bool)) Set {
	in := c.arg.Eval(refs)
	out := make(Set, len(in))
	for i, it := range in {
		out[i] = Item{Labels: it.Labels, Key: it.Key, V: c.fn(it.V)}
	}
	return out
}
func (c call) Refs() []string { return c.arg.Refs() }

type binary struct {
	op   string
	l, r Expr
}

func (b binary) Refs() []string { return append(b.l.Refs(), b.r.Refs()...) }

func (b binary) Eval(refs func(string) (Set, bool)) Set {
	return Join(b.l.Eval(refs), b.r.Eval(refs), func(x, y float64) float64 { return apply(b.op, x, y) })
}

func apply(op string, x, y float64) float64 {
	switch op {
	case "+":
		return x + y
	case "-":
		return x - y
	case "*":
		return x * y
	case "/":
		return x / y
	case "%":
		return math.Mod(x, y)
	case "^":
		return math.Pow(x, y)
	case ">":
		return b2f(x > y)
	case ">=":
		return b2f(x >= y)
	case "<":
		return b2f(x < y)
	case "<=":
		return b2f(x <= y)
	case "==":
		return b2f(x == y)
	case "!=":
		return b2f(x != y)
	case "&&":
		return b2f(x != 0 && y != 0)
	case "||":
		return b2f(x != 0 || y != 0)
	}
	return math.NaN()
}

// Join pairs two sets the way Grafana's math does: items with the same
// labels; a lone item with no labels goes with every item on the other side;
// otherwise items whose labels are a subset of the other's.
func Join(l, r Set, f func(x, y float64) float64) Set {
	switch {
	case len(l) == 0 || len(r) == 0:
		return nil
	case len(r) == 1 && len(r[0].Labels) == 0:
		out := make(Set, len(l))
		for i, it := range l {
			out[i] = Item{Labels: it.Labels, Key: it.Key, V: f(it.V, r[0].V)}
		}
		return out
	case len(l) == 1 && len(l[0].Labels) == 0:
		out := make(Set, len(r))
		for i, it := range r {
			out[i] = Item{Labels: it.Labels, Key: it.Key, V: f(l[0].V, it.V)}
		}
		return out
	}
	// When every label set has as many labels, one can only be a subset of
	// another by being equal: pair them by key, without comparing each
	// with each. This is the usual case, two queries by the same labels.
	n, even := len(l[0].Labels), true
	for _, s := range [2]Set{l, r} {
		for _, it := range s {
			even = even && len(it.Labels) == n
		}
	}
	if even {
		byKey := make(map[string][]int, len(r))
		for i, b := range r {
			k := b.key()
			byKey[k] = append(byKey[k], i)
		}
		var out Set
		for _, a := range l {
			for _, i := range byKey[a.key()] {
				out = append(out, Item{Labels: a.Labels, Key: a.Key, V: f(a.V, r[i].V)})
			}
		}
		return out
	}
	var out Set
	for _, a := range l {
		for _, b := range r {
			switch {
			case maps.Equal(a.Labels, b.Labels), subset(b.Labels, a.Labels):
				out = append(out, Item{Labels: a.Labels, Key: a.Key, V: f(a.V, b.V)})
			case subset(a.Labels, b.Labels):
				out = append(out, Item{Labels: b.Labels, Key: b.Key, V: f(a.V, b.V)})
			}
		}
	}
	return out
}

// subset reports whether every label of a is in b with the same value.
func subset(a, b map[string]string) bool {
	if len(a) > len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
