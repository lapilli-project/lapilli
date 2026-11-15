package rules

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A rule that counts events compares how many times something happened over
// a range with a number one event alone passes: increase(restarts[30m]) >
// 0.5. After one event its condition holds for that range at most, so a
// "for" as long as the range, or longer, can never be waited out by one
// event: the rule fires only when events keep coming for the whole of its
// "for", which is another rule than the one written.

// eventFuncs are the functions of a range that count what happened in it.
var eventFuncs = map[string]bool{"increase": true, "changes": true, "resets": true, "delta": true, "idelta": true}

// eventAggs are the aggregations that keep one event's count: the sum of
// the alerts it is over, and the most of them.
var eventAggs = map[string]bool{"sum": true, "max": true}

// EventWindow is, for a Prometheus rule whose condition counts events, the
// range it counts them over, which one event holds it for at most: its
// value is increase, changes, resets, delta or idelta of a range or a
// subquery, summed or maxed by some labels or not, compared with > a number
// under 1, or with >= one of 1 at most, so that one event passes. Of a
// condition of comparisons joined by and, each of them that counts events,
// the longest range. ok is false for any other rule, and when what its
// condition is can't be told: nothing is said then.
func (r Rule) EventWindow() (time.Duration, bool) {
	if r.Graph != nil || r.Logs != "" || r.Unsupported != "" || len(r.Expr) > maxExpr {
		return 0, false
	}
	parts, ok := andParts(r.Expr, 0)
	if !ok {
		return 0, false
	}
	var out time.Duration
	for _, p := range parts {
		c, ok := compared(unwrap(p))
		if !ok || !(c.op == ">" && c.n >= 0 && c.n < 1 || c.op == ">=" && c.n > 0 && c.n <= 1) {
			continue
		}
		if w, ok := eventRange(c.value, 0); ok {
			out = max(out, w)
		}
	}
	return out, out > 0
}

// ForOutlasts is, for a rule whose condition counts events (EventWindow),
// the range it counts them over, when its "for" is as long or longer: one
// event alone can never fire it. ok is false otherwise.
func (r Rule) ForOutlasts() (time.Duration, bool) {
	w, ok := r.EventWindow()
	return w, ok && r.For >= w
}

// andParts are the parts of a condition each of which must hold for it to:
// the whole, or, of operands joined by and alone, each. ok is false for
// one with or or unless, which hold apart from a part.
func andParts(expr string, depth int) ([]string, bool) {
	expr = unwrap(expr)
	if depth > maxDepth {
		return nil, false
	}
	ops, ok := setOps(expr)
	if !ok {
		return nil, false
	}
	if len(ops) == 0 {
		return []string{expr}, true
	}
	var out []string
	start := 0
	for _, o := range ops {
		if o.word != "and" {
			return nil, false
		}
		out = append(out, expr[start:o.at])
		start = o.end
	}
	return append(out, expr[start:]), true
}

// unwrap is an expression without comments and spaces around it, and
// without brackets around the whole of it.
func unwrap(expr string) string {
	for range maxDepth {
		lo, hi, ok := trimmed(expr)
		if !ok || lo >= hi {
			return ""
		}
		expr = expr[lo:hi]
		if expr[0] != '(' || closing(expr, 0) != len(expr)-1 {
			return expr
		}
		expr = expr[1 : len(expr)-1]
	}
	return ""
}

// eventRange is, of the value a condition compares, the range it counts
// events over: increase(x[30m]), sum by (ns) (changes(x[1h:1m])).
func eventRange(value string, depth int) (time.Duration, bool) {
	value = unwrap(value)
	if depth > maxDepth || value == "" {
		return 0, false
	}
	name, rest := leadingWord(value)
	switch {
	case eventAggs[strings.ToLower(name)]:
		// sum by (ns) (x), sum (x) by (ns), sum without (pod) (x)
		rest = skipGrouping(rest)
		inner, after, ok := call(rest)
		if !ok || !blank(skipGrouping(after)) {
			return 0, false
		}
		return eventRange(inner, depth+1)
	case eventFuncs[name]:
		arg, after, ok := call(rest)
		if !ok || !blank(after) {
			return 0, false
		}
		return rangeOf(arg)
	}
	return 0, false
}

// leadingWord is the word an expression starts with, and what follows it.
func leadingWord(expr string) (string, string) {
	j := 0
	for j < len(expr) && isWordByte(expr[j]) {
		j++
	}
	return expr[:j], expr[j:]
}

// skipGrouping is what follows by (...) or without (...) at the start of
// expr; expr itself when it starts with neither.
func skipGrouping(expr string) string {
	at := skipSpace(expr, 0)
	w, rest := leadingWord(expr[at:])
	if w := strings.ToLower(w); w != "by" && w != "without" {
		return expr
	}
	at = skipSpace(rest, 0)
	if at >= len(rest) || rest[at] != '(' {
		return expr
	}
	end := closing(rest[at:], 0)
	if end < 0 {
		return expr
	}
	return rest[at+end+1:]
}

// call is, of an expression that starts with brackets, comments and spaces
// aside, what is in them and what follows them.
func call(expr string) (inner, after string, ok bool) {
	at := skipSpace(expr, 0)
	if at >= len(expr) || expr[at] != '(' {
		return "", "", false
	}
	end := closing(expr[at:], 0)
	if end < 0 {
		return "", "", false
	}
	return expr[at+1 : at+end], expr[at+end+1:], true
}

// blank reports whether expr holds nothing but comments and spaces.
func blank(expr string) bool {
	_, hi, ok := trimmed(expr)
	return ok && hi == 0
}

// rangeOf is the range a function's argument is taken over: a selector's
// range, x[30m], or a subquery's, (x)[30m:1m], with an offset or an @
// after it or not.
func rangeOf(arg string) (time.Duration, bool) {
	open, shut := -1, -1
	ok := walk(arg, func(i, depth int) {
		if depth != 0 {
			return
		}
		switch arg[i] {
		case '[':
			open, shut = i, -1
		case ']':
			if open >= 0 && shut < 0 {
				shut = i
			}
		}
	})
	if !ok || open <= 0 || shut < 0 || blank(arg[:open]) || !modifiers(arg[shut+1:]) {
		return 0, false
	}
	text := strings.Join(strings.Fields(arg[open+1:shut]), "")
	text, _, _ = strings.Cut(text, ":")
	d, ok := readRange(text)
	return d, ok && d > 0
}

// modifiers reports whether what follows a range is nothing but an offset
// and an @, as PromQL writes them after a selector or a subquery.
func modifiers(after string) bool {
	fs := strings.Fields(stripComments(after))
	for i := 0; i < len(fs); i++ {
		switch f := strings.ToLower(fs[i]); {
		case f == "offset" && i+1 < len(fs):
			if _, ok := readRange(strings.TrimPrefix(fs[i+1], "-")); !ok {
				return false
			}
			i++
		case f == "@" && i+1 < len(fs):
			i++
		case strings.HasPrefix(f, "@") && len(f) > 1:
		default:
			return false
		}
	}
	return true
}

// stripComments is expr without its comments.
func stripComments(expr string) string {
	var b strings.Builder
	for _, line := range strings.Split(expr, "\n") {
		c, _, _ := strings.Cut(line, "#")
		b.WriteString(c + "\n")
	}
	return b.String()
}

var rangeUnits = regexp.MustCompile(`^(?:(\d+)y)?(?:(\d+)w)?(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?(?:(\d+)ms)?$`)

// readRange reads a duration as PromQL writes a range: 30m, 1h30m, 2d.
func readRange(s string) (time.Duration, bool) {
	m := rangeUnits.FindStringSubmatch(s)
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
