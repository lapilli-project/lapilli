package rules

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// parsedLogQL is a LogQL expression in one form, ok false when it isn't
// read: the LogQL expression of a Loki ruler's rule as Parsed has it, the
// way parsedExpr has PromQL's. Each form below is one, and nothing else is:
// comments (#) and space aside; a stream selector's matchers in any order;
// a string by its value, in quotes or backticks; a number by its value
// (1e+06 as 1000000); a duration by its value (300s, 5m, 5m0s); the words
// of LogQL (sum, by, json, offset, bool, or...) in any case, where Loki's
// lexer takes them so; by and without before or after an aggregation's
// parentheses, and no grouping as by () in a vector aggregation; the
// operators of a binary operation by their precedence, each written
// between parentheses as Loki prints it, so a pair of parentheses that
// changes nothing is none; a label filter's = as ==, on a number or a
// duration; and a range's unwrap, offset and pipeline in the order Loki's
// grammar lets them be written in (selector, pipeline, unwrap, range, and
// range, offset, pipeline, unwrap), which Loki prints as one. Every form is
// how Loki 3.7.9 reads and prints it (pkg/logql/syntax: syntax.y, lex.go
// and ast.go's String methods).
//
// It reads a metric query: a range aggregation (rate, count_over_time,
// quantile_over_time...) of a stream selector with line filters (|=, !=,
// |~, !~, |>, !>, one string each), parsers (json, logfmt, regexp, pattern,
// unpack), label_format, line_format, decolorize, drop, keep, label
// filters (a string, a number or a duration against one label) and unwrap
// (bytes(), duration() and duration_seconds() too) with the filters after
// it, a vector aggregation of it (sum, avg, min, max, count, stddev,
// stdvar, topk, bottomk, approx_topk, sort, sort_desc), label_replace,
// vector, a number, and binary operations between them, bool among them.
// Everything else Loki may read otherwise, or that Lapilli isn't sure of,
// isn't read: a log query, variants, on, ignoring, group_left and
// group_right, a line filter's or, ip, a parser's flags, several label
// filters in one stage (and, or, a comma, parentheses), a duration or a
// number whose kind depends on the lexer (bytes, 0x10, 010, 1_000), a sign
// before anything but a number, two numbers in a binary operation (which
// Loki works out as it reads), a label named as one of its words, a
// character in ' (which is no string in Loki), a // or /* comment (which
// its scanner skips), and any space or comment inside a range's brackets or
// between a function's name and its parenthesis, which Loki doesn't read
// as it does elsewhere.
//
// It is held as Lapilli holds every expression: 16 KiB, 32 levels of
// brackets at most.
func parsedLogQL(expr string) (string, bool) {
	if len(expr) > maxExpr || !utf8.ValidString(expr) {
		return "", false
	}
	p := &lq{s: expr}
	n := p.expr(0)
	p.space()
	if p.bad || p.i != len(p.s) {
		return "", false
	}
	return n.s, true
}

// lq reads an expression: s, read as far as i; depth, how many levels of
// brackets deep; bad, that it isn't read.
type lq struct {
	s     string
	i     int
	depth int
	bad   bool
}

// An lqNode is what was read, written in its one form; lit, that it is a
// number written as one, which Loki works out with another as it reads.
type lqNode struct {
	s   string
	lit bool
}

// The words Loki's lexer takes as its own wherever they are (tokens, in
// lex.go): no label is named so.
var lqWords = map[string]bool{
	"by": true, "without": true, "bool": true, "unwrap": true, "label_replace": true, "offset": true,
	"on": true, "ignoring": true, "group_left": true, "group_right": true, "or": true, "and": true,
	"unless": true, "json": true, "regexp": true, "logfmt": true, "unpack": true, "pattern": true,
	"label_format": true, "line_format": true, "ip": true, "decolorize": true, "drop": true, "keep": true,
	"variants": true, "of": true,
}

// The words it takes as its own where a ( or a by or without ( follows them
// (functionTokens), and a label's name where not.
var (
	lqRangeOps = map[string]bool{"rate": true, "rate_counter": true, "count_over_time": true, "bytes_rate": true,
		"bytes_over_time": true, "avg_over_time": true, "sum_over_time": true, "min_over_time": true,
		"max_over_time": true, "stdvar_over_time": true, "stddev_over_time": true, "quantile_over_time": true,
		"first_over_time": true, "last_over_time": true, "absent_over_time": true}
	lqVectorOps = map[string]bool{"sum": true, "avg": true, "max": true, "min": true, "count": true, "stddev": true,
		"stdvar": true, "bottomk": true, "topk": true, "sort": true, "sort_desc": true, "approx_topk": true}
	lqFuncWords = map[string]bool{"vector": true, "bytes": true, "duration": true, "duration_seconds": true}
)

func lqLetter(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// ws reads past the space Loki's scanner reads past: not comments.
func (p *lq) ws() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n' || p.s[p.i] == '\r') {
		p.i++
	}
}

// space reads past space and # comments, to the line's end. // and /*, which
// Loki's scanner skips as Go's, aren't read at all.
func (p *lq) space() {
	for {
		p.ws()
		if p.peek() != '#' {
			return
		}
		for p.i < len(p.s) && p.s[p.i] != '\n' {
			p.i++
		}
	}
}

func (p *lq) peek() byte {
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

func (p *lq) expect(c byte) {
	if p.peek() != c {
		p.bad = true
		return
	}
	p.i++
}

// word is the name at i, letters, digits and _, as Loki reads one (ASCII
// only): "" for none; nothing is read.
func (p *lq) word() string {
	j := p.i
	for j < len(p.s) && (lqLetter(p.s[j]) || j > p.i && pqDigit(p.s[j])) {
		j++
	}
	return p.s[p.i:j]
}

// fnAhead says a ( follows the word that ends at j, or by or without and
// then a (, past space but not comments, as Loki's lexer decides a word is
// a function (isFunction).
func (p *lq) fnAhead(j int) bool {
	q := &lq{s: p.s, i: j}
	q.ws()
	if q.peek() == '(' {
		return true
	}
	rest := strings.ToLower(q.s[q.i:])
	for _, w := range []string{"by", "without"} {
		if strings.HasPrefix(rest, w) {
			q.i += len(w)
			q.ws()
			return q.peek() == '('
		}
	}
	return false
}

// ident reads a label's name: not one of Loki's words, nor a function's
// where it would be read as one.
func (p *lq) ident() string {
	w := p.word()
	lw := strings.ToLower(w)
	if w == "" || pqDigit(w[0]) || lqWords[lw] || (lqRangeOps[lw] || lqVectorOps[lw] || lqFuncWords[lw]) && p.fnAhead(p.i+len(w)) {
		p.bad = true
		return ""
	}
	p.i += len(w)
	return w
}

// str reads a string in quotes or backticks, by its value, as Loki does
// (lex.go: text/scanner's, then strutil.Unquote). A ' isn't one in Loki. A
// \r in a backtick's is dropped by Unquote.
func (p *lq) str() string {
	if c := p.peek(); c != '"' && c != '`' {
		p.bad = true
		return ""
	}
	q := &pq{s: p.s, i: p.i}
	v, raw := q.str()
	p.i = q.i
	if q.bad || p.s[q.i-1] == '`' && strings.ContainsRune(raw, '\r') {
		p.bad = true
		return ""
	}
	return v
}

// token is what Loki's lexer reads as a number or duration at i: letters,
// digits, _ and ., and a sign after an exponent's e (1e+06).
func (p *lq) token() string {
	j := p.i
	for j < len(p.s) {
		c := p.s[j]
		switch {
		case lqLetter(c) || pqDigit(c) || c == '.':
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

// number is a decimal number at i, 5, 5.5, .5, 1e3, 1e+06, by its value
// written as one; not 0x10, 010 nor 1_000, nor one past a float64.
func (p *lq) number() (float64, string) {
	c := p.peek()
	if !pqDigit(c) && c != '.' {
		p.bad = true
		return 0, ""
	}
	tok := p.token()
	if !pqDecimal(tok) || len(tok) > 1 && tok[0] == '0' && pqDigit(tok[1]) {
		p.bad = true
		return 0, ""
	}
	v, err := strconv.ParseFloat(tok, 64)
	if err != nil || math.IsInf(v, 0) {
		p.bad = true
		return 0, ""
	}
	return v, lqFloat(v)
}

func lqFloat(v float64) string { return "n:" + strconv.FormatFloat(v, 'g', -1, 64) }

// lqDuration is a duration's value as Loki reads one outside brackets
// (lexer's parseDuration): Prometheus's, or else Go's (1.5s, 5m0s).
func lqDuration(tok string) (time.Duration, bool) {
	if d, ok := pqDuration(tok); ok {
		return d, true
	}
	if tok == "" || !pqDigit(tok[0]) && tok[0] != '.' {
		return 0, false
	}
	d, err := time.ParseDuration(tok)
	return d, err == nil && d >= 0
}

// expr reads terms between binary operators whose precedence is min or
// more, as Loki's grammar has them: or, then and and unless, then the
// comparisons, then + and -, then *, / and %, then ^, which goes right to
// left. Each is written between parentheses, as Loki prints it.
func (p *lq) expr(min int) lqNode {
	if p.depth++; p.depth > maxDepth {
		p.bad = true
	}
	defer func() { p.depth-- }()
	l := p.operand()
	for !p.bad {
		p.space()
		op, prec := p.binop()
		if op == "" || prec < min {
			break
		}
		p.i += len(op)
		p.space()
		out := op
		if w := p.word(); strings.EqualFold(w, "bool") {
			p.i += len(w)
			out += " bool"
		}
		next := prec + 1
		if op == "^" {
			next = prec
		}
		r := p.expr(next)
		if p.bad {
			break
		}
		// Loki works out a binary operation of two numbers as it reads
		// it, and refuses a number beside and, or or unless.
		if l.lit && r.lit || (op == "and" || op == "or" || op == "unless") && (l.lit || r.lit) {
			p.bad = true
			break
		}
		l = lqNode{s: "(" + l.s + " " + out + " " + r.s + ")"}
	}
	return l
}

// binop is the binary operator at i, in lower case, and its precedence;
// "" for none. A word is one only as a whole.
func (p *lq) binop() (string, int) {
	rest := p.s[p.i:]
	for _, o := range []string{"==", "!=", ">=", "<="} {
		if strings.HasPrefix(rest, o) {
			return o, 3
		}
	}
	switch c := p.peek(); c {
	case '>', '<':
		return string(c), 3
	case '+', '-':
		return string(c), 4
	case '*', '%':
		return string(c), 5
	case '/':
		if strings.HasPrefix(rest, "//") || strings.HasPrefix(rest, "/*") {
			p.bad = true
			return "", 0
		}
		return "/", 5
	case '^':
		return "^", 6
	}
	switch w := strings.ToLower(p.word()); w {
	case "or":
		return w, 1
	case "and", "unless":
		return w, 2
	}
	return "", 0
}

// operand reads a number with its sign, a parenthesis, or a function:
// a range aggregation, a vector aggregation, label_replace or vector.
func (p *lq) operand() lqNode {
	p.space()
	switch c := p.peek(); {
	case c == '(':
		p.i++
		n := p.expr(0)
		p.space()
		p.expect(')')
		return n
	case c == '+' || c == '-' || pqDigit(c) || c == '.':
		neg := false
		if c == '+' || c == '-' {
			neg = c == '-'
			p.i++
			p.space()
		}
		v, _ := p.number()
		if neg {
			v = -v
		}
		return lqNode{s: lqFloat(v), lit: true}
	case lqLetter(c):
		w := p.word()
		lw := strings.ToLower(w)
		if !p.fnAhead(p.i+len(w)) || !lqRangeOps[lw] && !lqVectorOps[lw] && lw != "label_replace" && lw != "vector" {
			p.bad = true
			return lqNode{}
		}
		p.i += len(w)
		p.ws()
		switch {
		case lqRangeOps[lw]:
			return lqNode{s: p.rangeAgg(lw)}
		case lqVectorOps[lw]:
			return lqNode{s: p.vectorAgg(lw)}
		case lw == "vector":
			p.expect('(')
			p.space()
			_, n := p.number()
			p.space()
			p.expect(')')
			return lqNode{s: "vector(" + n + ")"}
		}
		return lqNode{s: p.labelReplace()}
	}
	p.bad = true
	return lqNode{}
}

// grouping reads by or without and its labels, if one is at i; as written:
// none for by (), which is a vector's none; sorted, as a set of labels.
func (p *lq) grouping() (string, bool) {
	w := strings.ToLower(p.word())
	if w != "by" && w != "without" {
		return "", false
	}
	p.i += len(w)
	p.space()
	p.expect('(')
	var labels []string
	for !p.bad {
		p.space()
		if p.peek() == ')' {
			break
		}
		if len(labels) > 0 {
			p.expect(',')
			p.space()
		}
		labels = append(labels, p.ident())
	}
	p.expect(')')
	slices.Sort(labels)
	return " " + w + " (" + strings.Join(labels, ",") + ")", true
}

// rangeAgg reads a range aggregation's arguments, after its (: quantile's
// number, and the range, then its grouping, which isn't a vector's: none and
// by () are two.
func (p *lq) rangeAgg(op string) string {
	p.expect('(')
	p.space()
	param := ""
	if c := p.peek(); pqDigit(c) || c == '.' {
		_, param = p.number()
		p.space()
		p.expect(',')
		param += ","
	}
	if (op == "quantile_over_time") != (param != "") {
		p.bad = true
	}
	r := p.logRange()
	p.space()
	p.expect(')')
	g := ""
	if !p.bad {
		p.space()
		g, _ = p.grouping()
	}
	return op + "(" + param + r + ")" + g
}

// vectorAgg reads a vector aggregation after its operator: by or without
// before its parentheses or after them, and its arguments, topk's number
// among them.
func (p *lq) vectorAgg(op string) string {
	g, before := p.grouping()
	p.ws()
	p.expect('(')
	p.space()
	param := ""
	if op == "topk" || op == "bottomk" || op == "approx_topk" {
		tok := p.token()
		n, err := strconv.Atoi(tok)
		if err != nil || n <= 0 || strings.Trim(tok, "0123456789") != "" {
			p.bad = true
		}
		p.space()
		p.expect(',')
		param = strconv.Itoa(n) + ","
	}
	e := p.expr(0)
	p.space()
	p.expect(')')
	if !p.bad && !before {
		p.space()
		g, _ = p.grouping()
	}
	if g == " by ()" {
		g = ""
	}
	return op + g + "(" + param + e.s + ")"
}

// labelReplace reads label_replace's arguments, after its (.
func (p *lq) labelReplace() string {
	p.expect('(')
	e := p.expr(0)
	out := "label_replace(" + e.s
	for i := 0; i < 4 && !p.bad; i++ {
		p.space()
		p.expect(',')
		p.space()
		out += "," + strconv.Quote(p.str())
	}
	p.space()
	p.expect(')')
	return out + ")"
}

// logRange reads a stream selector with its pipeline, its unwrap and its
// range, as syntax.y has them: the selector, its pipeline, its unwrap,
// then the range and an offset; or the selector, the range and an offset,
// then its pipeline and its unwrap. Written as the first. After an unwrap,
// only label filters.
func (p *lq) logRange() string {
	p.space()
	out := p.selector()
	var stages []string
	unwrap, post := "", []string{}
	ranged, closed := "", false
	for !p.bad && !closed {
		p.space()
		switch c := p.peek(); {
		case c == '[':
			if ranged != "" {
				p.bad = true
				break
			}
			end := strings.IndexByte(p.s[p.i:], ']')
			d, ok := time.Duration(0), false
			if end > 0 {
				d, ok = pqDuration(p.s[p.i+1 : p.i+end])
			}
			if !ok {
				p.bad = true
				break
			}
			p.i += end + 1
			ranged = "[" + d.String() + "]"
			p.space()
			if w := p.word(); strings.EqualFold(w, "offset") {
				p.i += len(w)
				p.space()
				o, ok := lqDuration(p.token())
				p.bad = p.bad || !ok
				ranged += " offset " + o.String()
			}
			// A range written after the selector's pipeline or unwrap is
			// the last of it; before, the pipeline may follow.
			closed = len(stages) > 0 || unwrap != ""
		case c == '|' || c == '!':
			st := p.stage()
			switch {
			case p.bad:
			case st.kind == 'u' && unwrap == "":
				unwrap = st.s
			case unwrap != "" && st.kind == 'l':
				post = append(post, st.s)
			case unwrap != "":
				p.bad = true // only label filters follow an unwrap
			default:
				stages = append(stages, st.s)
			}
		default:
			closed = true
		}
	}
	if ranged == "" {
		p.bad = true
	}
	out += strings.Join(append([]string{""}, stages...), " ")
	if unwrap != "" {
		out += " " + unwrap + strings.Join(append([]string{""}, post...), " ")
	}
	return out + ranged
}

// selector reads a stream selector: matchers in any order, written sorted.
// A selector with no matcher isn't read.
func (p *lq) selector() string {
	p.expect('{')
	var ms []string
	for !p.bad {
		p.space()
		if len(ms) > 0 {
			p.expect(',')
			p.space()
		}
		ms = append(ms, p.matcher())
		p.space()
		if p.peek() == '}' {
			p.i++
			break
		}
	}
	slices.Sort(ms)
	return "{" + strings.Join(ms, ",") + "}"
}

// matcher reads a label, =, !=, =~ or !~ and a string.
func (p *lq) matcher() string {
	name := p.ident()
	p.space()
	op := p.matchOp()
	p.space()
	return name + op + strconv.Quote(p.str())
}

func (p *lq) matchOp() string {
	for _, o := range []string{"=~", "!~", "!=", "="} {
		if strings.HasPrefix(p.s[p.i:], o) {
			p.i += len(o)
			return o
		}
	}
	p.bad = true
	return ""
}

// An lqStage is a stage of a pipeline in its one form, and its kind: 'f' a
// line filter, 'l' a label filter, 'u' an unwrap, 'o' any other.
type lqStage struct {
	kind byte
	s    string
}

// stage reads a stage of a pipeline: a line filter, or a | and a parser, a
// format, a drop or keep, an unwrap or a label filter.
func (p *lq) stage() lqStage {
	for _, o := range []string{"|=", "|~", "|>", "!=", "!~", "!>"} {
		if strings.HasPrefix(p.s[p.i:], o) {
			p.i += len(o)
			p.space()
			return lqStage{'f', o + " " + strconv.Quote(p.str())}
		}
	}
	p.expect('|')
	p.space()
	w := strings.ToLower(p.word())
	switch w {
	case "json", "logfmt":
		p.i += len(w)
		p.space()
		out := "| " + w
		if lqLetter(p.peek()) {
			out += " " + p.extractions()
		}
		return lqStage{'o', out}
	case "regexp", "pattern", "line_format":
		p.i += len(w)
		p.space()
		return lqStage{'o', "| " + w + " " + strconv.Quote(p.str())}
	case "unpack", "decolorize":
		p.i += len(w)
		return lqStage{'o', "| " + w}
	case "label_format":
		p.i += len(w)
		p.space()
		return lqStage{'o', "| label_format " + p.labelFormats()}
	case "drop", "keep":
		p.i += len(w)
		p.space()
		return lqStage{'o', "| " + w + " " + p.namedMatchers()}
	case "unwrap":
		p.i += len(w)
		p.space()
		return lqStage{'u', p.unwrap()}
	}
	return lqStage{'l', p.labelFilter()}
}

// list reads one or more of item, between commas.
func (p *lq) list(item func() string) string {
	var out []string
	for !p.bad {
		out = append(out, item())
		p.space()
		if p.peek() != ',' {
			break
		}
		p.i++
		p.space()
	}
	return strings.Join(out, ",")
}

// extractions reads what json and logfmt take: labels, each with the
// expression it is taken by, or its own name.
func (p *lq) extractions() string {
	return p.list(func() string {
		name := p.ident()
		p.space()
		if p.peek() != '=' {
			return name + "=" + strconv.Quote(name)
		}
		p.i++
		p.space()
		return name + "=" + strconv.Quote(p.str())
	})
}

// labelFormats reads what label_format takes: a label renamed (a=b) or
// given a template (a="{{.b}}"), which are two.
func (p *lq) labelFormats() string {
	return p.list(func() string {
		name := p.ident()
		p.space()
		p.expect('=')
		p.space()
		if c := p.peek(); c == '"' || c == '`' {
			return name + "=" + strconv.Quote(p.str())
		}
		return name + "=" + p.ident()
	})
}

// namedMatchers reads what drop and keep take: labels, and matchers.
func (p *lq) namedMatchers() string {
	return p.list(func() string {
		name := p.ident()
		p.space()
		if c := p.peek(); c == '=' || c == '!' {
			p.space()
			op := p.matchOp()
			p.space()
			return name + op + strconv.Quote(p.str())
		}
		return name
	})
}

// unwrap reads an unwrap's label, as it is or in bytes(), duration() or
// duration_seconds().
func (p *lq) unwrap() string {
	w := p.word()
	if lw := strings.ToLower(w); lw == "bytes" || lw == "duration" || lw == "duration_seconds" {
		q := &lq{s: p.s, i: p.i + len(w)}
		if q.ws(); q.peek() == '(' {
			p.i = q.i + 1
			p.space()
			name := p.ident()
			p.space()
			p.expect(')')
			return "| unwrap " + lw + "(" + name + ")"
		}
	}
	return "| unwrap " + p.ident()
}

// labelFilter reads one filter of a label: a string against it (=, !=, =~,
// !~), or a number or a duration (=, ==, !=, >, >=, <, <=), where = and ==
// are one. Several in one stage (and, or, a comma, a space, parentheses)
// aren't read, nor ip() nor a size.
func (p *lq) labelFilter() string {
	name := p.ident()
	p.space()
	op := ""
	for _, o := range []string{"=~", "!~", "!=", "==", ">=", "<=", "=", ">", "<"} {
		if strings.HasPrefix(p.s[p.i:], o) {
			op = o
			break
		}
	}
	if op == "" {
		p.bad = true
		return ""
	}
	p.i += len(op)
	p.space()
	if c := p.peek(); c == '"' || c == '`' {
		if op == "==" || op[0] == '>' || op[0] == '<' {
			p.bad = true
		}
		return name + op + strconv.Quote(p.str())
	}
	if op == "=~" || op == "!~" {
		p.bad = true
		return ""
	}
	if op == "=" {
		op = "=="
	}
	signed := p.peek() == '+' || p.peek() == '-'
	neg := p.peek() == '-'
	if signed {
		p.i++
		p.space()
	}
	save := p.i
	if tok := p.token(); !pqDecimal(tok) {
		d, ok := lqDuration(tok)
		if signed || !ok {
			p.bad = true
		}
		return name + op + "d:" + d.String()
	}
	p.i = save
	v, _ := p.number()
	if neg {
		v = -v
	}
	return name + op + lqFloat(v)
}
