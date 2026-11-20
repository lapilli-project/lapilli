package rules

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Unalike says the first thing in a PromQL expression that a history
// lapilli serve asks may read otherwise than the checks here (Selectors,
// Wide, Reach) read it, "" when there is none: what a pull request's rule
// at the backtest port could hide a selector, a range or an offset in.
// The histories --prometheus takes read PromQL each its own way:
// Prometheus with unquoted UTF-8 names reads a name that isn't ASCII;
// VictoriaMetrics' MetricsQL (v0.87.6, as checked) reads a \ in a name as
// escaping the character after it (a backtick among them), a . in a name,
// a \ before a string in backticks' last backtick as going on, a label's
// name in quotes by escapes of its own ("__\name__" is __name__), WITH
// (...) templates, or, and and unless in a selector's braces, default, if
// and ifnot as operators between two expressions, keep_metric_names, an offset or @ after any expression, a range after an
// offset, 1doffset as 1d offset, 1d-1y as one duration and 1.5d as one.
//
// So what is taken is what they all read alike, comments read past as
// Prometheus reads them (blankComments): outside its strings, ASCII names,
// numbers and durations as Prometheus writes them, operators, brackets,
// commas and @, and space, tab and newline; an offset or @ right after a
// selector (its name or its braces), a range or a subquery; a range or a
// subquery right after a selector or a parenthesis; a label's name in
// quotes with no \; none of those words of MetricsQL's; and no
// __graphite__, which VictoriaMetrics reads as a pattern of names. A
// string that doesn't end is left to Selectors, which refuses it.
func Unalike(expr string) string {
	t := blankComments(expr)
	// What came last, outside strings, but for spaces: a range or a
	// subquery, an offset or @ may follow only some.
	const (
		other   = iota
		name    // a name, a selector's or a function's
		brace   // a selector's }
		bracket // a range's or a subquery's ]
		paren   // a )
	)
	last := other
	braces := false
	for i := 0; i < len(t); {
		c := t[i]
		switch {
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for ; j < len(t) && t[j] != c; j++ {
				if t[j] == '\\' && c != '`' {
					j++
				}
			}
			if j >= len(t) {
				return ""
			}
			if c == '`' && t[j-1] == '\\' {
				return "a string in backticks whose last backtick follows a \\, which MetricsQL reads as going on"
			}
			if n := nextOf(t, j+1); braces && (n == '=' || n == '!') && strings.Contains(t[i:j], "\\") {
				// "__\name__" is __name__ to MetricsQL, __, a line's end
				// and ame__ to Prometheus.
				return "a label's name in quotes with a \\ in it, which histories read apart"
			}
			i, last = j+1, other
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '_' || c == ':' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(t) && isWordByte(t[j]) {
				j++
			}
			word := strings.ToLower(t[i:j])
			next := nextOf(t, j)
			switch {
			case j < len(t) && t[j] == '.':
				return "a name with a . in it, which MetricsQL reads as one name"
			case word == "__graphite__":
				return "__graphite__, which VictoriaMetrics reads as a pattern of names"
			case word == "keep_metric_names":
				return "keep_metric_names, which MetricsQL reads as a modifier"
			case braces && (word == "or" || word == "and" || word == "unless"):
				return word + " in a selector's braces, which MetricsQL reads as joining its matchers"
			case !braces && word == "with" && next == '(':
				return "WITH (...), which MetricsQL reads as templates"
			case !braces && (word == "default" || word == "if" || word == "ifnot") && next != ')' && next != ',':
				// An operator to MetricsQL (x default {a="b"} reads every
				// series with a="b"), a metric's name to Prometheus; not
				// a label's name in by (...) or on (...).
				return word + ", which MetricsQL reads as an operator between two expressions"
			case !braces && word == "offset" && next != ')' && next != ',':
				// An offset, not a label's name in by (...) or on (...).
				if last != name && last != brace && last != bracket {
					return "an offset after something other than a selector, a range or a subquery"
				}
				i, last = j, other
				continue
			}
			last = name
			if braces || keywords[word] || next == '(' {
				last = other
			}
			i = j
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(t) && t[i+1] >= '0' && t[i+1] <= '9':
			// A number or a duration, as scopeScan reads one, then held
			// to what Prometheus writes: MetricsQL reads 1doffset as 1d
			// offset, 1.5d as a day and a half, 1d-1y as one duration.
			start := i
			hex := c == '0' && i+1 < len(t) && t[i+1]|0x20 == 'x'
			for i < len(t) {
				d := t[i]
				alnum := d >= '0' && d <= '9' || d >= 'a' && d <= 'z' || d >= 'A' && d <= 'Z' || d == '.' || d == '_'
				exp := i > start && (d == '+' || d == '-') && (t[i-1] == 'e' || t[i-1] == 'E') && !hex
				if !alnum && !exp {
					break
				}
				i++
			}
			switch n := t[start:i]; {
			case !plainNumber.MatchString(n) && !plainDuration.MatchString(n):
				return strconv.Quote(clipText(n, 40)) + ", a number or a duration Prometheus doesn't write so"
			case plainDuration.MatchString(n) && i+1 < len(t) && (t[i] == '+' || t[i] == '-') && (t[i+1] >= '0' && t[i+1] <= '9' || t[i+1] == '.'):
				return "a duration with " + string(t[i]) + " right after it, which MetricsQL reads as one duration"
			}
			last = other
		case c == '{':
			i, last, braces = i+1, other, true
		case c == '}':
			i, last, braces = i+1, brace, false
		case c == '[':
			if last != name && last != brace && last != paren {
				return "a range or a subquery after something other than a selector or a parenthesis"
			}
			i, last = i+1, other
		case c == ']':
			i, last = i+1, bracket
		case c == ')':
			i, last = i+1, paren
		case c == '@':
			if last != name && last != brace && last != bracket {
				return "@ after something other than a selector, a range or a subquery"
			}
			i, last = i+1, other
		case strings.IndexByte("(,+-*/%^=!<>~:", c) >= 0:
			i, last = i+1, other
		default:
			r, _ := utf8.DecodeRuneInString(t[i:])
			if r >= utf8.RuneSelf || r == utf8.RuneError {
				return "a character that isn't ASCII outside a string, which a history may read as part of a name"
			}
			return strconv.QuoteRune(r) + " outside a string"
		}
	}
	return ""
}

// plainNumber is a number as Prometheus writes one: in decimal, with a
// fraction and an exponent or not, or in hex, its digits apart by _ or
// not; plainDuration, a duration, its units each once, largest first.
var (
	plainNumber   = regexp.MustCompile(`^(?:0[xX][0-9a-fA-F_]+|(?:[0-9][0-9_]*(?:\.[0-9_]*)?|\.[0-9][0-9_]*)(?:[eE][-+]?[0-9][0-9_]*)?)$`)
	plainDuration = rangeUnits
)

// clipText is s, n bytes at most.
func clipText(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// nextOf is the first character of t at i or after that isn't a space, 0
// for none.
func nextOf(t string, i int) byte {
	for ; i < len(t); i++ {
		if c := t[i]; c != ' ' && c != '\t' && c != '\n' {
			return c
		}
	}
	return 0
}
