package rules

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Parsed is a rule as Whole has it, its expression read as PromQL and
// written again in one form: two writings of the same rule are one. A rule
// in its file and the same rule as an API that prints its expression again
// gives it (Prometheus's rules API, Mimir's, Thanos') have one Parsed, where
// their Whole differ. Its for, keep_firing_for, interval and the rest are
// Whole's, by value already; its labels are the labels its alerts carry
// (AlertLabels), as one map, its group's among them whether they were
// written apart (a file) or not (an API); and its group's limit. A Grafana
// rule is read by parsedGrafana, from its graph. ok is false
// when its expression isn't PromQL that Lapilli reads so (a Loki ruler's, a
// cloud's alarm, or an expression parsedExpr refuses), or, for a Grafana
// rule, when parsedGrafana can't hold its graph: Whole is all there is to
// compare it by, and a text read otherwise is another.
//
// Two rules whose Parsed are one fire alike; two that may fire otherwise
// never have one Parsed. What can't be told so is refused, never guessed.
func (r Rule) Parsed() (string, bool) {
	if r.Graph != nil {
		return r.parsedGrafana()
	}
	if r.Logs != "" || r.Cloud != "" {
		return "", false
	}
	e, ok := parsedExpr(r.Expr)
	if !ok {
		return "", false
	}
	// Its labels apart from Key's, each name and value in quotes: Key's
	// a=b=c is a="b=c" and, of a name Prometheus 3 takes, "a=b"="c".
	labels := r.AlertLabels()
	var l strings.Builder
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		l.WriteString(strconv.Quote(k) + "=" + strconv.Quote(labels[k]) + ",")
	}
	w := r
	w.Expr, w.Labels, w.GroupLabels = e, nil, nil
	return fmt.Sprintf("parsed %s limit %d labels %s", w.Key(), r.Limit, l.String()), true
}

// parsedExpr is a PromQL expression in one form, ok false when it isn't
// read. Each form below is one, and nothing else is: comments and space
// aside; a number by its value, its signs with it (5, 5.0, 5e0, +5; - 5 as
// -5; but 0x10 and 010, which MetricsQL may read otherwise, each only as
// written, and 1_000 not at all); a duration by
// its value (300s, 5m); a string by its value, whatever its quotes ('a',
// "a", `a`); a selector's matchers in any order, its metric's name written
// before its braces or as __name__="name"; by and without before or after
// an aggregation's parentheses, by () as none; the labels of by, without,
// on, ignoring, group_left and group_right in any order, ignoring () with no
// group_left or group_right as none, group_left as group_left (); the words
// of PromQL (sum, and, by, offset, bool...) in any case; @ before or after
// an offset, an offset of 0 as none; and parentheses that change nothing:
// around the whole expression, around a whole argument, around one term
// with no sign (a selector, a number, a call...) where nothing follows
// them, but never around a range or a subquery. Every form is how
// Prometheus 3.5.0 reads and prints it (promql/parser), and VictoriaMetrics'
// MetricsQL reads it alike: so what MetricsQL reads otherwise (a
// backtick's string ending in \, a label's name in quotes with a \ in it, a
// \r in a comment, a duration outside brackets and offsets, 1d-1y) isn't
// read at all.
//
// It is held as Lapilli reads every expression (ReadExpr): 16 KiB, 32
// levels of brackets at most.
func parsedExpr(expr string) (string, bool) {
	if len(expr) > maxExpr || !utf8.ValidString(expr) {
		return "", false
	}
	p := &pq{s: expr}
	e := p.expr()
	p.space(true)
	if p.bad || e == nil || p.i != len(p.s) {
		return "", false
	}
	var out pqOut
	e.normalize(true).write(&out)
	return out.b.String(), true
}

// pq reads an expression: s, read as far as i; depth, how many brackets
// deep; bad, that it isn't read.
type pq struct {
	s     string
	i     int
	depth int
	bad   bool
}

// A pqExpr is terms between binary operators, as written: no operator's
// precedence is worked out, so nothing is moved across one.
type pqExpr struct {
	terms []pqTerm
	ops   []pqOp
}

// A pqTerm is the signs before it (+ and -, in order), what it is, and what
// follows it: ranges, subqueries, offsets and @.
type pqTerm struct {
	signs string
	prim  pqPrim
	post  []pqPost
}

// A pqPrim is one of: a number ('n', text its value written as one, or
// verbatim as written when not byValue), a string ('s', text its value), a
// selector ('v', its matchers, each written as one), a call ('f', text its
// function), an aggregation ('a', text its operator, grouped by labels, or
// without them) or a parenthesis ('p', in).
type pqPrim struct {
	kind     byte
	text     string
	num      float64
	byValue  bool
	matchers []string
	args     []*pqExpr
	grouped  bool
	without  bool
	labels   []string
	in       *pqExpr
}

// A pqOp is a binary operator: op, in lower case, its bool, and its
// matching: on or ignoring and their labels, group_left or group_right
// and theirs.
type pqOp struct {
	op, match, group string
	boolean          bool
	on, include      []string
}

// A pqPost follows a term: '[' a range, ':' a subquery, as text; 'o' an
// offset, '@' an @, text their value.
type pqPost struct {
	kind byte
	text string
}

// The words PromQL reads as its own, in any case (promql/parser's key):
// the aggregations, and the rest, which are no name of a metric or a
// function here.
var (
	pqAggregations = map[string]bool{"sum": true, "avg": true, "count": true, "min": true, "max": true, "group": true,
		"stddev": true, "stdvar": true, "topk": true, "bottomk": true, "count_values": true, "quantile": true,
		"limitk": true, "limit_ratio": true}
	pqWords = map[string]bool{"and": true, "or": true, "unless": true, "atan2": true, "offset": true, "by": true,
		"without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true, "bool": true,
		"start": true, "end": true}
)

func (p *pq) peek() byte {
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

// space reads past spaces, and comments where they may be: not in a
// range's brackets, which Prometheus refuses one in. A comment ends at a
// newline. Prometheus ends one at a \r too, and MetricsQL (v0.87.6) goes
// on past it: a \r in a comment but before a newline isn't read, as what
// follows it is read by one and not by the other.
func (p *pq) space(comments bool) {
	for p.i < len(p.s) {
		switch c := p.s[p.i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			p.i++
		case c == '#' && comments:
			for p.i < len(p.s) && p.s[p.i] != '\n' {
				if p.s[p.i] == '\r' && (p.i+1 == len(p.s) || p.s[p.i+1] != '\n') {
					p.bad = true
				}
				p.i++
			}
		default:
			return
		}
	}
}

func pqLetter(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func pqDigit(c byte) bool  { return c >= '0' && c <= '9' }

// word is the name at i, as Prometheus reads one outside braces: a letter,
// _ or :, then letters, digits, _ and :. "" for none; nothing is read.
func (p *pq) word() string {
	j := p.i
	for j < len(p.s) && (pqLetter(p.s[j]) || p.s[j] == ':' || j > p.i && pqDigit(p.s[j])) {
		j++
	}
	return p.s[p.i:j]
}

// expr reads terms between binary operators, to what ends them: the end,
// a ) or a ,.
func (p *pq) expr() *pqExpr {
	if p.depth++; p.depth > maxDepth {
		p.bad = true
	}
	defer func() { p.depth-- }()
	e := &pqExpr{}
	for !p.bad {
		t := p.term()
		if p.bad {
			return nil
		}
		e.terms = append(e.terms, t)
		p.space(true)
		op, ok := p.op()
		if !ok {
			return e
		}
		e.ops = append(e.ops, op)
	}
	return nil
}

// op reads a binary operator and its bool, on or ignoring, group_left or
// group_right, if one is at i.
func (p *pq) op() (pqOp, bool) {
	var o pqOp
	rest := p.s[p.i:]
	switch {
	case rest == "":
		return o, false
	case strings.HasPrefix(rest, "==") || strings.HasPrefix(rest, "!=") || strings.HasPrefix(rest, ">=") || strings.HasPrefix(rest, "<="):
		o.op = rest[:2]
	case strings.IndexByte("+-*/%^<>", rest[0]) >= 0:
		o.op = rest[:1]
	default:
		w := strings.ToLower(p.word())
		if w != "and" && w != "or" && w != "unless" && w != "atan2" {
			return o, false
		}
		o.op = w
	}
	p.i += len(o.op)
	p.space(true)
	if w := p.word(); strings.EqualFold(w, "bool") {
		p.i += len(w)
		o.boolean = true
		p.space(true)
	}
	if w := strings.ToLower(p.word()); w == "on" || w == "ignoring" {
		p.i += len(w)
		o.match = w
		p.space(true)
		o.on = p.labels()
		p.space(true)
		if w := strings.ToLower(p.word()); w == "group_left" || w == "group_right" {
			p.i += len(w)
			o.group, o.include = w, []string{}
			p.space(true)
			if p.peek() == '(' {
				o.include = p.labels()
			}
		}
	}
	return o, !p.bad
}

// term reads a term's signs, what it is and what follows it.
func (p *pq) term() pqTerm {
	var t pqTerm
	var signs []byte
	for p.space(true); p.peek() == '+' || p.peek() == '-'; p.space(true) {
		signs = append(signs, p.s[p.i])
		p.i++
	}
	t.signs = string(signs)
	t.prim = p.primary()
	if !p.bad {
		t.post = p.postfixes(t.prim.kind == 'v')
	}
	return t
}

func (p *pq) primary() pqPrim {
	var x pqPrim
	p.space(true)
	c := p.peek()
	switch {
	case c == '(':
		p.i++
		x.kind, x.in = 'p', p.expr()
		p.space(true)
		p.expect(')')
	case c == '{':
		x.kind, x.matchers = 'v', p.braces("")
	case c == '"' || c == '\'' || c == '`':
		x.kind = 's'
		x.text, _ = p.str()
	case pqDigit(c) || c == '.' && p.i+1 < len(p.s) && pqDigit(p.s[p.i+1]):
		// A number: a duration outside brackets and offsets is read
		// otherwise by MetricsQL (1d-1y), and by Prometheus only with
		// its experimental durations.
		var bad bool
		x.kind = 'n'
		x.text, x.num, x.byValue, bad = pqNumber(p.token())
		p.bad = p.bad || bad
	case pqLetter(c) || c == ':':
		w := p.word()
		p.i += len(w)
		lw := strings.ToLower(w)
		switch {
		case pqAggregations[lw]:
			return p.aggregation(lw)
		case lw == "inf" || lw == "nan":
			x.kind, x.byValue = 'n', true
			x.num, _ = strconv.ParseFloat(lw, 64)
			return x
		case pqWords[lw]:
			// A word of PromQL's where a term is: a metric of that name
			// (Prometheus takes "start" or "by" so), or nothing it reads.
			p.bad = true
			return x
		}
		p.space(true)
		switch p.peek() {
		case '(':
			if strings.Contains(w, ":") {
				p.bad = true
				return x
			}
			p.i++
			x.kind, x.text, x.args = 'f', w, p.args()
		case '{':
			x.kind, x.matchers = 'v', p.braces(w)
		default:
			x.kind, x.matchers = 'v', []string{strconv.Quote("__name__") + "=" + strconv.Quote(w)}
		}
	default:
		p.bad = true
	}
	return x
}

func (p *pq) expect(c byte) {
	if p.peek() != c {
		p.bad = true
		return
	}
	p.i++
}

// aggregation reads an aggregation after its operator: by or without
// before its parentheses or after them, and its arguments.
func (p *pq) aggregation(op string) pqPrim {
	x := pqPrim{kind: 'a', text: op}
	grouping := func() bool {
		p.space(true)
		w := strings.ToLower(p.word())
		if w != "by" && w != "without" {
			return false
		}
		p.i += len(w)
		p.space(true)
		x.grouped, x.without, x.labels = true, w == "without", p.labels()
		return true
	}
	before := grouping()
	p.space(true)
	p.expect('(')
	if p.bad {
		return x
	}
	x.args = p.args()
	if !p.bad && !before {
		grouping()
	}
	return x
}

// args reads a call's or an aggregation's arguments, after its (, to its
// ): none, or expressions between commas, with none after the last.
func (p *pq) args() []*pqExpr {
	out := []*pqExpr{}
	p.space(true)
	if p.peek() == ')' {
		p.i++
		return out
	}
	for !p.bad {
		e := p.expr()
		if p.bad {
			return nil
		}
		out = append(out, e)
		p.space(true)
		switch p.peek() {
		case ',':
			p.i++
			p.space(true)
			if p.peek() == ')' {
				p.bad = true // Prometheus refuses a comma after the last
			}
		case ')':
			p.i++
			return out
		default:
			p.bad = true
		}
	}
	return nil
}

// labels reads the labels of by, without, on, ignoring, group_left and
// group_right, in their parentheses: names Prometheus reads outside braces,
// PromQL's words among them as written, or strings; a comma after the last
// or not. Each is written as one, and they are sorted so: in any order,
// they are the same labels.
func (p *pq) labels() []string {
	p.expect('(')
	out := []string{}
	for !p.bad {
		p.space(true)
		if p.peek() == ')' {
			p.i++
			slices.Sort(out)
			return out
		}
		out = append(out, p.labelName(true))
		p.space(true)
		switch p.peek() {
		case ',':
			p.i++
		case ')':
		default:
			p.bad = true
		}
	}
	return nil
}

// labelName reads a label's name: a name, or a string. A name in quotes
// stays one apart from the same name without them, which Prometheus 2
// doesn't read, nor MetricsQL by the same escapes. colons says a : may be
// in it, as outside braces.
func (p *pq) labelName(colons bool) string {
	switch c := p.peek(); {
	case c == '"' || c == '\'' || c == '`':
		v, raw := p.str()
		if strings.ContainsRune(raw, '\\') {
			p.bad = true
		}
		return "q" + strconv.Quote(v)
	case pqLetter(c):
		j := p.i
		for j < len(p.s) && (pqLetter(p.s[j]) || pqDigit(p.s[j]) || colons && p.s[j] == ':') {
			j++
		}
		w := p.s[p.i:j]
		p.i = j
		return strconv.Quote(w)
	}
	p.bad = true
	return ""
}

// braces reads a selector's braces, for the metric name, "" for none
// before them: its matchers, each written as one, the name's among them,
// sorted so. A name given twice (before the braces and in them) isn't
// read: it would stand for one once written as one.
func (p *pq) braces(name string) []string {
	p.expect('{')
	var out []string
	names := 0
	if name != "" {
		out = append(out, strconv.Quote("__name__")+"="+strconv.Quote(name))
	}
	for !p.bad {
		p.space(true)
		if p.peek() == '}' {
			p.i++
			break
		}
		label := p.labelName(false)
		p.space(true)
		op := ""
		for _, o := range []string{"=~", "!~", "!=", "="} {
			if strings.HasPrefix(p.s[p.i:], o) {
				op = o
				break
			}
		}
		switch {
		case p.bad:
		case op == "" && strings.HasPrefix(label, "q"):
			// A metric's name, alone in quotes: {"my.metric"}.
			out = append(out, label)
			names++
		case op == "":
			p.bad = true
		default:
			p.i += len(op)
			p.space(true)
			if c := p.peek(); c != '"' && c != '\'' && c != '`' {
				p.bad = true
				break
			}
			v, _ := p.str()
			out = append(out, label+op+strconv.Quote(v))
			if label == strconv.Quote("__name__") || label == "q"+strconv.Quote("__name__") {
				names++
			}
		}
		p.space(true)
		switch p.peek() {
		case ',':
			p.i++
		case '}':
		default:
			p.bad = true
		}
	}
	if name != "" && names > 0 {
		p.bad = true
	}
	slices.Sort(out)
	return out
}

// str reads a string, as Prometheus does (promql/parser's lexString,
// lexRawString and strutil.Unquote): its value, and what is between its
// quotes. In backticks, nothing is escaped, and one ending in \ isn't
// read, which MetricsQL reads as going on; in quotes, Go's escapes, and no
// newline. Neither has an invalid byte, or U+FFFD written as one.
func (p *pq) str() (string, string) {
	q := p.s[p.i]
	start := p.i + 1
	if q == '`' {
		k := strings.IndexByte(p.s[start:], '`')
		if k < 0 {
			p.bad = true
			return "", ""
		}
		raw := p.s[start : start+k]
		p.i = start + k + 1
		if strings.HasSuffix(raw, `\`) || strings.ContainsRune(raw, utf8.RuneError) {
			p.bad = true
		}
		return raw, raw
	}
	var b strings.Builder
	j := start
	for {
		if j >= len(p.s) || p.s[j] == '\n' {
			p.bad = true
			return "", ""
		}
		if p.s[j] == q {
			break
		}
		r, multibyte, tail, err := strconv.UnquoteChar(p.s[j:], q)
		if err != nil || r == utf8.RuneError && p.s[j] != '\\' {
			p.bad = true
			return "", ""
		}
		if r < utf8.RuneSelf || !multibyte {
			b.WriteByte(byte(r))
		} else {
			b.WriteRune(r)
		}
		j = len(p.s) - len(tail)
	}
	p.i = j + 1
	return b.String(), p.s[start:j]
}

// token is the number or duration at i, as far as Prometheus's lexer reads
// one: letters, digits, _ and ., and a sign after an exponent's e (1e-3,
// which Prometheus reads as one, a hexadecimal's 0x1e+5 too).
func (p *pq) token() string {
	j := p.i
	for j < len(p.s) {
		c := p.s[j]
		switch {
		case pqLetter(c) || pqDigit(c) || c == '.':
		case (c == '+' || c == '-') && j > p.i && (p.s[j-1] == 'e' || p.s[j-1] == 'E') && j+1 < len(p.s) && pqDigit(p.s[j+1]):
		default:
			tok := p.s[p.i:j]
			p.i = j
			return tok
		}
		j++
	}
	tok := p.s[p.i:j]
	p.i = j
	return tok
}

// pqNumber is a number token's value, written as one; or, for a
// hexadecimal or an integer with a leading 0 (010, which Prometheus reads
// as 8 and MetricsQL may read as 10), its text as written. bad for what
// isn't a number Prometheus reads alike, or reads at all: 1e400, 1_000.
func pqNumber(tok string) (text string, v float64, byValue, bad bool) {
	switch {
	case pqHex(tok):
		return "n=" + tok, 0, false, false
	case !pqDecimal(tok):
		return "", 0, false, true
	case len(tok) > 1 && tok[0] == '0' && strings.Trim(tok, "0123456789") == "":
		return "n=" + tok, 0, false, false
	}
	// As promql/parser's number: an integer, then a float.
	if n, err := strconv.ParseInt(tok, 0, 64); err == nil {
		return "", float64(n), true, false
	}
	f, err := strconv.ParseFloat(tok, 64)
	if err != nil {
		return "", 0, false, true
	}
	return "", f, true, false
}

// pqDecimal: 5, 5., 5.5, .5, with an exponent or not (1e3, 2.5E-7).
func pqDecimal(s string) bool {
	i, digits := 0, 0
	for ; i < len(s) && pqDigit(s[i]); i++ {
		digits++
	}
	if i < len(s) && s[i] == '.' {
		for i++; i < len(s) && pqDigit(s[i]); i++ {
			digits++
		}
	}
	if digits == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		n := i
		for ; i < len(s) && pqDigit(s[i]); i++ {
		}
		if i == n {
			return false
		}
	}
	return i == len(s)
}

func pqHex(s string) bool {
	if len(s) < 3 || s[0] != '0' || s[1] != 'x' && s[1] != 'X' {
		return false
	}
	return strings.Trim(s[2:], "0123456789abcdefABCDEF") == ""
}

// pqDuration is a duration's value, as Prometheus reads one
// (model.ParseDuration): numbers each with a unit, the largest first, of
// y, w, d, h, m, s and ms; within time.Duration.
func pqDuration(s string) (time.Duration, bool) {
	var total uint64
	last := -1
	for s != "" {
		i := 0
		for i < len(s) && pqDigit(s[i]) {
			i++
		}
		v, err := strconv.ParseUint(s[:i], 10, 64)
		if i == 0 || err != nil {
			return 0, false
		}
		s = s[i:]
		j := 0
		for j < len(s) && !pqDigit(s[j]) {
			j++
		}
		u := slices.Index(pqUnits, s[:j])
		if u <= last {
			return 0, false
		}
		last, s = u, s[j:]
		mult := uint64(pqUnitOf[u])
		if v > (1<<63-1)/mult {
			return 0, false
		}
		if total += v * mult; total > 1<<63-1 {
			return 0, false
		}
	}
	return time.Duration(total), last >= 0
}

// A duration's units, the largest first, and what each is.
var (
	pqUnits  = []string{"y", "w", "d", "h", "m", "s", "ms"}
	pqUnitOf = []time.Duration{365 * 24 * time.Hour, 7 * 24 * time.Hour, 24 * time.Hour, time.Hour, time.Minute, time.Second, time.Millisecond}
)

// duration reads a duration, where a range, a subquery or an offset has
// one: never a number, which Prometheus 3 reads as seconds and Prometheus
// 2 not at all, nor one with a sign right after it, which MetricsQL reads
// as one duration (1d-1y).
func (p *pq) duration() time.Duration {
	if !pqDigit(p.peek()) {
		p.bad = true
		return 0
	}
	d, ok := pqDuration(p.token())
	if c := p.peek(); !ok || c == '+' || c == '-' {
		p.bad = true
	}
	return d
}

// postfixes reads what follows a term, a selector or not: ranges,
// subqueries, offsets and @. Between two of the brackets, one offset and
// one @ at most, as Prometheus refuses another. An offset of 0 is none, as
// Prometheus prints it, where Prometheus takes one: on a selector, or
// after a range or a subquery; elsewhere (sum(x) offset 0s) it isn't
// read, as an expression Prometheus refuses would be one it runs.
func (p *pq) postfixes(selector bool) []pqPost {
	var out []pqPost
	bracket, offset, at := false, false, false
	for !p.bad {
		p.space(true)
		switch {
		case p.peek() == '[':
			bracket, offset, at = true, false, false
			p.i++
			p.space(false)
			d := p.duration()
			p.space(false)
			text := "[d:" + strconv.FormatInt(int64(d), 10)
			kind := byte('[')
			if p.peek() == ':' {
				p.i++
				p.space(false)
				kind, text = ':', text+":"
				if p.peek() != ']' {
					text += "d:" + strconv.FormatInt(int64(p.duration()), 10)
					p.space(false)
				}
			}
			p.expect(']')
			out = append(out, pqPost{kind, text + "]"})
		case p.peek() == '@':
			if at {
				p.bad = true
				break
			}
			at = true
			p.i++
			p.space(true)
			if w := strings.ToLower(p.word()); w == "start" || w == "end" {
				p.i += len(w)
				p.space(true)
				p.expect('(')
				p.space(true)
				p.expect(')')
				out = append(out, pqPost{'@', w + "()"})
				continue
			}
			neg := false
			if c := p.peek(); c == '+' || c == '-' {
				neg = c == '-'
				p.i++
				p.space(true)
			}
			if c := p.peek(); !pqDigit(c) && c != '.' {
				p.bad = true
				break
			}
			text, v, byValue, bad := pqNumber(p.token())
			switch {
			case bad:
				p.bad = true
			case !byValue && neg:
				text = "-" + text
			case byValue && neg:
				text = pqFloat(-v)
			case byValue:
				text = pqFloat(v)
			}
			out = append(out, pqPost{'@', text})
		default:
			w := p.word()
			if !strings.EqualFold(w, "offset") {
				return out
			}
			if offset {
				p.bad = true
				break
			}
			offset = true
			p.i += len(w)
			p.space(true)
			neg := p.peek() == '-'
			if neg {
				p.i++
				p.space(true)
			}
			switch d := p.duration(); {
			case d != 0:
				if neg {
					d = -d
				}
				out = append(out, pqPost{'o', "d:" + strconv.FormatInt(int64(d), 10)})
			case !selector && !bracket:
				p.bad = true
			}
		}
	}
	return out
}

func pqFloat(v float64) string { return "n:" + strconv.FormatFloat(v, 'g', -1, 64) }

// normalize drops the parentheses that change nothing: around the whole of
// e when whole (the whole expression, a whole argument, what is in
// parentheses), and around one term with no sign, range or subquery where
// nothing follows them.
func (e *pqExpr) normalize(whole bool) *pqExpr {
	for whole && len(e.terms) == 1 {
		t := e.terms[0]
		if t.signs != "" || len(t.post) > 0 || t.prim.kind != 'p' || t.prim.in.ranged() {
			break
		}
		e = t.prim.in
	}
	for i := range e.terms {
		t := &e.terms[i]
		for t.prim.kind == 'p' && len(t.post) == 0 {
			in := t.prim.in
			if len(in.terms) != 1 || in.terms[0].signs != "" || in.ranged() {
				break
			}
			t.prim, t.post = in.terms[0].prim, in.terms[0].post
		}
		switch t.prim.kind {
		case 'p':
			t.prim.in = t.prim.in.normalize(true)
		case 'f', 'a':
			for j, a := range t.prim.args {
				t.prim.args[j] = a.normalize(true)
			}
		}
	}
	return e
}

// ranged says e is one term with a range or a subquery after it.
func (e *pqExpr) ranged() bool {
	return len(e.terms) == 1 && slices.ContainsFunc(e.terms[0].post, func(x pqPost) bool { return x.kind == '[' || x.kind == ':' })
}

// pqOut is what is written: tokens with a space between each. None has a
// space in it but a string's, which its quotes delimit.
type pqOut struct{ b strings.Builder }

func (o *pqOut) add(s string) {
	if o.b.Len() > 0 {
		o.b.WriteByte(' ')
	}
	o.b.WriteString(s)
}

func (e *pqExpr) write(o *pqOut) {
	for i, t := range e.terms {
		if i > 0 {
			e.ops[i-1].write(o)
		}
		t.write(o)
	}
}

func (x pqOp) write(o *pqOut) {
	o.add(x.op)
	if x.boolean {
		o.add("bool")
	}
	if x.match == "ignoring" && len(x.on) == 0 && x.group == "" {
		return // as none, as Prometheus prints it
	}
	if x.match != "" {
		o.add(x.match)
		o.add("(" + strings.Join(x.on, ",") + ")")
	}
	if x.group != "" {
		o.add(x.group)
		o.add("(" + strings.Join(x.include, ",") + ")")
	}
}

func (t pqTerm) write(o *pqOut) {
	if t.prim.kind == 'n' && t.prim.byValue && len(t.post) == 0 {
		// A number's signs are its own, as Prometheus reads -5 and +Inf.
		v := t.prim.num
		for _, s := range t.signs {
			if s == '-' {
				v = -v
			}
		}
		o.add(pqFloat(v))
		return
	}
	for _, s := range t.signs {
		o.add(string(s))
	}
	t.prim.write(o)
	// Each run of offsets and @, @ first, as Prometheus prints them.
	for i := 0; i < len(t.post); {
		if k := t.post[i].kind; k == '[' || k == ':' {
			o.add(t.post[i].text)
			i++
			continue
		}
		j := i
		for j < len(t.post) && (t.post[j].kind == 'o' || t.post[j].kind == '@') {
			j++
		}
		for _, kind := range []byte{'@', 'o'} {
			for _, x := range t.post[i:j] {
				if x.kind == kind {
					o.add(map[byte]string{'@': "@", 'o': "offset"}[kind])
					o.add(x.text)
				}
			}
		}
		i = j
	}
}

func (x pqPrim) write(o *pqOut) {
	switch x.kind {
	case 'n':
		if x.byValue {
			o.add(pqFloat(x.num))
		} else {
			o.add(x.text)
		}
	case 's':
		o.add(strconv.Quote(x.text))
	case 'v':
		o.add("{" + strings.Join(x.matchers, ",") + "}")
	case 'p':
		o.add("(")
		x.in.write(o)
		o.add(")")
	case 'f', 'a':
		o.add(x.text)
		switch {
		case x.without:
			o.add("without")
			o.add("(" + strings.Join(x.labels, ",") + ")")
		case x.grouped && len(x.labels) > 0:
			o.add("by")
			o.add("(" + strings.Join(x.labels, ",") + ")")
		}
		o.add("(")
		for i, a := range x.args {
			if i > 0 {
				o.add(",")
			}
			a.write(o)
		}
		o.add(")")
	}
}
