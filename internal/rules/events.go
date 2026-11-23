package rules

import (
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/sse"
)

// A rule that counts events compares how many times something happened over
// a range with a number one event alone passes: increase(restarts[30m]) >
// 0.5, rate(restarts[10m]) * 60 * 5 > 0. After one event its condition holds
// for that range at most, so a "for" as long as the range, or longer, can
// never be waited out by one event: the rule fires only when events keep
// coming for the whole of its "for", which is another rule than the one
// written.

// eventFuncs are the functions of a range of a counter that count what
// happened in it. delta is a gauge's, which no event makes go up; idelta
// and irate hold for about a scrape after an event, not for the range.
var eventFuncs = map[string]bool{"increase": true, "changes": true, "resets": true, "rate": true}

// eventAggs are the aggregations that keep one event's count: the sum of
// the alerts it is over, and the most of them.
var eventAggs = map[string]bool{"sum": true, "max": true}

// logEventFuncs are LogQL's functions of a range of log lines that count
// what happened in it (docs/design/loki.md): each line is one event, and
// holds what it counts for that range at most, whatever the line. Of each,
// whether one line comes to one over the range's seconds: count_over_time
// counts lines, rate is them a second; bytes_over_time counts a line's
// bytes, one at least of a line that says anything, and bytes_rate is them
// a second.
var logEventFuncs = map[string]bool{"count_over_time": false, "rate": true, "bytes_over_time": false, "bytes_rate": true}

// continuous are the ends of the names of counters of an amount, not of
// events: an increase of them is time spent or bytes sent, not something
// that happened.
var continuous = []string{"_seconds_total", "_bytes_total", "_sum"}

// How much of an expression is read for whether it counts events: an alert
// rule that does is short, so a long or deep one isn't read, and costs the
// checks no more than this.
const (
	eventMaxExpr  = 4 << 10
	eventMaxDepth = 8
)

// EventWindow is, for a Prometheus rule whose condition counts events, the
// range it counts them over, which one event holds it for at most: its
// value is increase, changes, resets or rate of a counter's range, or of a
// subquery of one, summed or maxed by some labels or not, times or over
// numbers or not, compared with a number that no event doesn't pass and one
// event does: increase(x[30m]) > 0.5, rate(x[10m]) * 300 > 0. A counter of
// seconds or bytes, or a histogram's sum, is no count of events. Of a
// condition of comparisons joined by and, each that counts events, the
// longest range. Of a Grafana rule, as Graph.eventWindow reads it. ok is
// false for any other rule, and when what its condition is can't be told:
// nothing is said then.
//
// Of a Loki ruler's rule, written in LogQL, the same of count_over_time,
// rate, bytes_over_time or bytes_rate of a stream's lines over a range
// (logEventValue), its pipeline counting lines, not a label's values
// (unwrap): count_over_time({app="shop"} |= "error" [5m]) > 0.
func (r Rule) EventWindow() (time.Duration, bool) {
	if r.Unsupported != "" {
		return 0, false
	}
	if r.Graph != nil {
		return r.Graph.eventWindow()
	}
	if len(r.Expr) > eventMaxExpr {
		return 0, false
	}
	value := eventValue
	if r.Logs != "" {
		if !plainLogQL(r.Expr) {
			return 0, false
		}
		value = logEventValue
	}
	parts, ok := andParts(r.Expr, 0)
	if !ok {
		return 0, false
	}
	var out time.Duration
	for _, p := range parts {
		c, ok := compared(unwrap(p))
		if !ok {
			continue
		}
		w, one, ok := value(c.value, 0)
		if !ok || passes(c.op, 0, c.n) || !passes(c.op, one, c.n) {
			continue
		}
		out = max(out, w)
	}
	return out, out > 0
}

// eventWindow is EventWindow of a Grafana rule whose condition compares the
// value of its one PromQL data query with a number, as a Prometheus rule's
// does: a threshold of one number (gt or gte), or a math expression that is
// one comparison of a ref with a number, of that query's value as it is
// (an instant query's) or through reduces that leave it so (keeps). The
// query's PromQL is read as a Prometheus rule's value is; a Loki query's
// LogQL as a Loki ruler's rule's (logEventValue), of an instant query
// alike. ok is false for any other: a range query, whose points one event
// can hold for as long again as its range (a pod's series that went on
// being returned after the pod was gone), a classic condition, any other
// math, a reduce that changes the value or puts a number in place of NaN,
// two data queries; and for a rule that fires on no data (noDataState
// Alerting, or KeepLast), whose firings needn't be events.
func (g *Graph) eventWindow() (time.Duration, bool) {
	if len(g.Data) != 1 || len(g.Other) != 0 || g.NoData == StateAlerting || g.NoData == StateKeepLast {
		return 0, false
	}
	var in, op string
	var n float64
	switch st := g.Steps[g.Cond]; {
	case st.Threshold != nil:
		in, op, n = st.Threshold.Input, graphOps[st.Threshold.Type], st.Threshold.A
	case st.Math != nil:
		value, o, x, ok := sse.Compared(st.Math)
		ref, isRef := sse.Ref(value)
		if !ok || !isRef {
			return 0, false
		}
		in, op, n = ref, o, x
	}
	expr, ok := g.eventQuery(in, 0)
	if !ok || op == "" || len(expr) > eventMaxExpr {
		return 0, false
	}
	value := eventValue
	if g.Logs != "" {
		if !plainLogQL(expr) {
			return 0, false
		}
		value = logEventValue
	}
	w, one, ok := value(expr, 0)
	if !ok || passes(op, 0, n) || !passes(op, one, n) {
		return 0, false
	}
	return w, true
}

// graphOps are the thresholds of one number above which events are.
var graphOps = map[string]string{"gt": ">", "gte": ">="}

// eventQuery is the PromQL of the instant data query a ref of a Grafana rule
// reads, as it is or through reduces that leave its value so (keeps). ok is
// false for any other ref.
func (g *Graph) eventQuery(ref string, depth int) (string, bool) {
	if depth > eventMaxDepth {
		return "", false
	}
	if st, isStep := g.Steps[ref]; isStep {
		if r := st.Reduce; r == nil || r.Mode == "replaceNN" || !keeps(r.Fn, r.Window) {
			return "", false
		}
		return g.eventQuery(st.Reduce.Input, depth+1)
	}
	rd, isRead := g.reads[ref]
	if _, isData := g.Data[rd.query]; !isRead || !isData || !keeps(rd.fn, rd.window) {
		return "", false
	}
	expr, ok := g.Queries[rd.query]
	return expr, ok
}

// keeps reports whether a reduce leaves the value it reduces as it is: of
// an instant query's one value, last, max, min or mean, or the value as it
// is (fn ""). Nothing of a range query's.
func keeps(fn string, window time.Duration) bool {
	return window == 0 && (fn == "" || fn == "last" || fn == "max" || fn == "min" || fn == "mean")
}

// passes reports whether v meets a comparison with n.
func passes(op string, v, n float64) bool {
	switch op {
	case ">":
		return v > n
	case ">=":
		return v >= n
	}
	return false
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
	if depth > eventMaxDepth {
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
	for range eventMaxDepth {
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

// eventValue is, of the value a condition compares, the range it counts
// events over, and what one event in it comes to: increase(x[30m]) one,
// rate(x[10m]) * 600 one, sum by (ns) (changes(x[1h:1m])) one.
func eventValue(value string, depth int) (w time.Duration, one float64, ok bool) {
	value = unwrap(value)
	if depth > eventMaxDepth || value == "" {
		return 0, 0, false
	}
	term, k, ok := scaled(value)
	if !ok {
		return 0, 0, false
	}
	if term != value {
		w, one, ok = eventValue(term, depth+1)
		return w, k * one, ok
	}
	name, rest := leadingWord(value)
	switch {
	case eventAggs[strings.ToLower(name)]:
		// sum by (ns) (x), sum (x) by (ns), sum without (pod) (x)
		rest = skipGrouping(rest)
		inner, after, ok := call(rest)
		if !ok || !blank(skipGrouping(after)) {
			return 0, 0, false
		}
		return eventValue(inner, depth+1)
	case eventFuncs[name]:
		arg, after, ok := call(rest)
		if !ok || !blank(after) {
			return 0, 0, false
		}
		w, ok := rangeOf(arg)
		if !ok {
			return 0, 0, false
		}
		if name == "rate" {
			return w, 1 / w.Seconds(), true
		}
		return w, 1, true
	}
	return 0, 0, false
}

// logEventValue is eventValue of LogQL: of the value a condition compares,
// the range it counts a stream's lines over, and what one line comes to:
// count_over_time({app="shop"} |= "error" [5m]) one, rate({app="shop"}
// [5m]) one over 300, bytes_over_time(...) one byte at least, sum by (app)
// (count_over_time(...)) one; times or over numbers, as eventValue reads
// them.
func logEventValue(value string, depth int) (w time.Duration, one float64, ok bool) {
	value = unwrap(value)
	if depth > eventMaxDepth || value == "" {
		return 0, 0, false
	}
	term, k, ok := scaled(value)
	if !ok {
		return 0, 0, false
	}
	if term != value {
		w, one, ok = logEventValue(term, depth+1)
		return w, k * one, ok
	}
	name, rest := leadingWord(value)
	perSecond, counts := logEventFuncs[name]
	switch {
	case eventAggs[strings.ToLower(name)]:
		rest = skipGrouping(rest)
		inner, after, ok := call(rest)
		if !ok || !blank(skipGrouping(after)) {
			return 0, 0, false
		}
		return logEventValue(inner, depth+1)
	case counts:
		arg, after, ok := call(rest)
		if !ok || !blank(after) {
			return 0, 0, false
		}
		w, ok := logRangeOf(arg)
		if !ok {
			return 0, 0, false
		}
		if perSecond {
			return w, 1 / w.Seconds(), true
		}
		return w, 1, true
	}
	return 0, 0, false
}

// logRangeOf is the range a LogQL range function counts lines over: a
// stream selector, its pipeline or none, then the range, with an offset
// after it or not: {app="shop"} |= "error" | json [5m], {app="shop"} !=
// "debug" [5m]. ok is false for
// any other: a range before the pipeline, a pipeline that unwraps a label,
// whose values, not lines, are then taken, or no stream selector.
func logRangeOf(arg string) (time.Duration, bool) {
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
	if !ok || open <= 0 || shut < 0 || !modifiers(arg[shut+1:]) {
		return 0, false
	}
	lo, hi, ok := trimmed(arg[:open])
	if !ok || lo >= hi || arg[lo] != '{' {
		return 0, false
	}
	stream := arg[lo:hi]
	end := closing(stream, 0)
	if end < 0 {
		return 0, false
	}
	if pipe := stream[end+1:]; !blank(pipe) {
		if at := skipSpace(pipe, 0); !pipeStart(pipe[at:]) || hasLogWord(pipe, "unwrap") {
			return 0, false
		}
	}
	d, ok := readRange(strings.Join(strings.Fields(arg[open+1:shut]), ""))
	return d, ok && d > 0
}

// pipeStart reports whether a LogQL pipeline starts as one does: a stage
// (|), or a line filter that keeps the lines without what it names (!=,
// !~, !>).
func pipeStart(pipe string) bool {
	return strings.HasPrefix(pipe, "|") || strings.HasPrefix(pipe, "!=") || strings.HasPrefix(pipe, "!~") || strings.HasPrefix(pipe, "!>")
}

// hasLogWord reports whether a LogQL pipeline has word outside its strings
// and comments: a stage's keyword, such as unwrap.
func hasLogWord(expr, word string) bool {
	found := false
	walk(expr, func(i, _ int) {
		if found || !isWordByte(expr[i]) || i > 0 && isWordByte(expr[i-1]) {
			return
		}
		j := i
		for j < len(expr) && isWordByte(expr[j]) {
			j++
		}
		found = strings.EqualFold(expr[i:j], word)
	})
	return found
}

// plainLogQL reports whether a LogQL expression has no comment of the kinds
// Loki reads beside PromQL's (// and /* */) outside its strings: what the
// rest of this file reads it by, # and strings, says all there is.
func plainLogQL(expr string) bool {
	plain := true
	walk(expr, func(i, _ int) {
		if expr[i] == '/' && i+1 < len(expr) && (expr[i+1] == '/' || expr[i+1] == '*') {
			plain = false
		}
	})
	return plain
}

// scaled is a value as a term times or over numbers, and what they come to:
// rate(x[10m]) * 60 * 5 is rate(x[10m]) and 300; a value with no number
// beside it is itself, and 1. ok is false for any other arithmetic, a
// number not over nothing, or a term divided by.
func scaled(value string) (term string, k float64, ok bool) {
	var cuts []int
	if !walk(value, func(i, depth int) {
		if depth == 0 && (value[i] == '*' || value[i] == '/') {
			cuts = append(cuts, i)
		}
	}) {
		return "", 0, false
	}
	if len(cuts) == 0 {
		return value, 1, true
	}
	k, start := 1.0, 0
	for i := 0; i <= len(cuts); i++ {
		end := len(value)
		if i < len(cuts) {
			end = cuts[i]
		}
		part := value[start:end]
		div := i > 0 && value[cuts[i-1]] == '/'
		if n, isNum := number(part); isNum {
			switch {
			case n <= 0 || math.IsInf(n, 0):
				return "", 0, false
			case div:
				k /= n
			default:
				k *= n
			}
		} else if term != "" || div {
			return "", 0, false
		} else {
			term = part
		}
		start = end + 1
	}
	if term == "" || k <= 0 || math.IsInf(k, 0) {
		return "", 0, false
	}
	return unwrap(term), k, true
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
	if !ok || open <= 0 || shut < 0 || !modifiers(arg[shut+1:]) {
		return 0, false
	}
	// Of a counter's selector, or a subquery of one, whose name isn't an
	// amount's.
	name, ok := selectorName(unwrap(arg[:open]))
	if !ok || slices.ContainsFunc(continuous, func(end string) bool { return strings.HasSuffix(name, end) }) {
		return 0, false
	}
	text := strings.Join(strings.Fields(arg[open+1:shut]), "")
	text, _, _ = strings.Cut(text, ":")
	d, ok := readRange(text)
	return d, ok && d > 0
}

// selectorName is the metric name of a selector, x or x{a="b"}, "" for
// {a="b"} alone; ok is false for anything else.
func selectorName(expr string) (string, bool) {
	name, rest := leadingWord(expr)
	if name != "" && (name[0] >= '0' && name[0] <= '9') {
		return "", false
	}
	if blank(rest) {
		return name, name != ""
	}
	at := skipSpace(rest, 0)
	if rest[at] != '{' {
		return "", false
	}
	end := closing(rest[at:], 0)
	if end < 0 || !blank(rest[at+end+1:]) {
		return "", false
	}
	return name, true
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
	const most = 200 * 365 * 24 * time.Hour
	var d time.Duration
	for i, u := range units {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		if err != nil || n > int64(most/u) {
			return 0, false
		}
		if add := time.Duration(n) * u; add > most-d {
			return 0, false // past two hundred years, all told: no range
		}
		d += time.Duration(n) * u
	}
	return d, true
}
