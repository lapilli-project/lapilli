package rules

import (
	"strings"
	"testing"
	"time"
)

// What a pull request's rule in LogQL may read through lapilli serve: each
// stream named by a label's value, a range and its offset within the
// bound; braces and brackets in strings, in a selector's matchers and in a
// comment's text read past, as Loki reads them.
func TestLogQLWideReach(t *testing.T) {
	week := 8 * 24 * time.Hour
	for expr, want := range map[string][2]string{
		`sum by (service) (count_over_time({app="checkout"} |= "error" [5m])) > 5`:                     {"", ""},
		`sum(count_over_time({app="x", env=~"prod|stage"} | json | line_format "{{.msg}} [9d]" [5m]))`: {"", ""},
		`sum(rate({app="x"} | logfmt | unwrap bytes [1m] offset 7d))`:                                  {"", ""},
		`sum(count_over_time({offset="x", app="y"}[5m]))`:                                              {"", ""},
		"sum(count_over_time({app=`x`}[5m])) # {} [30d] offset 1y\n > 1":                               {"", ""},
		`sum(count_over_time({app=~"x.+"}[5m]))`:                                                       {`{app=~"x.+"}`, ""},
		`sum(count_over_time({app="x"} != "y" [8d1m]))`:                                                {"", "past 8d"},
		`sum(count_over_time({app="x"}[1d] offset 7d1m))`:                                              {"", "past 8d"},
		`sum(count_over_time({app="x"}[5m] offset (1d)))`:                                              {"", "other than as a duration"},
		`sum(count_over_time({app="x"}[five]))`:                                                        {"", "other than as a duration"},
		`sum(count_over_time({app="x"}[5m)) > 1`:                                                       {"", "doesn't read"},
		`sum(count_over_time({app="x"[5m]))`:                                                           {`sum(count_over_time({app="x"[5m]))`, "doesn't read"},
		`vector(1)`:                                                                                    {"", ""},
	} {
		sel, wide := LogQLWide(expr)
		if (want[0] != "") != wide || wide && sel != want[0] {
			t.Errorf("%s: wide %t %q, want %q", expr, wide, sel, want[0])
		}
		got := LogQLReach(expr, week)
		if want[1] == "" && got != "" || want[1] != "" && !strings.Contains(got, want[1]) {
			t.Errorf("%s: reach %q, want %q", expr, got, want[1])
		}
	}
}

// Loki's lexer is Go's scanner's: it skips a // comment and a /* */ one
// outside a string as well as #, and reads a ' as a character. A # or a
// quote inside one of those would hide what follows from every check, so
// an expression with one is refused as a whole; inside a string, and
// a # comment read past, are as before.
func TestLogQLComments(t *testing.T) {
	week := 8 * 24 * time.Hour
	for _, expr := range []string{
		`/* # */ sum(count_over_time({namespace=~".+"}[30d])) > 1`,
		`/* " */ sum(count_over_time({namespace=~".+"}[30d])) > 1 /* " */`,
		"// #\nsum(count_over_time({namespace=~\".+\"}[30d])) > 1",
		`/**/{app="x"} |= "x"`,
		`sum(count_over_time({app="x"}[5m])) > 1 // a note`,
		`sum(count_over_time({app='x'}[5m])) > 1`,
	} {
		if _, ok := LogQLText(expr); ok {
			t.Errorf("%s: read", expr)
		}
		if _, wide := LogQLWide(expr); !wide {
			t.Errorf("%s: not wide", expr)
		}
		if LogQLReach(expr, week) == "" {
			t.Errorf("%s: reaches nothing", expr)
		}
	}
	for _, expr := range []string{
		`sum(count_over_time({app="x"} |= "http://x/*y" |~ "can't" [5m])) > 1`,
		"sum(count_over_time({app=`x`} |= `//` [5m])) # it's // /* \n > 1",
		`sum(count_over_time({app="x"} |= "a\"//" [5m])) / 2 > 1`,
	} {
		if text, ok := LogQLText(expr); !ok || len(text) != len(expr) {
			t.Errorf("%s: %q %t", expr, text, ok)
		}
		if sel, wide := LogQLWide(expr); wide || LogQLReach(expr, week) != "" {
			t.Errorf("%s: wide %q, reach %q", expr, sel, LogQLReach(expr, week))
		}
	}
	// A log query past comments of every kind is one.
	for expr, want := range map[string]bool{
		`/**/{app="x"} |= "x"`:                  true,
		"# c\n{app=\"x\"}":                      true,
		"// c\n  /* d\n */ {app=\"x\"}":         true,
		"/* {app=\"x\"} */ count_over_time":     false,
		`sum(count_over_time({app="x"}[5m]))`:   false,
		`({app="x"} |= "x")`:                    true,
		"( (/* c */ {app=\"x\"}))":              true,
		"\uFEFF{app=\"x\"} |= \"x\"":            true,
		`(sum(count_over_time({app="x"}[5m])))`: false,
	} {
		if got := LogQuery(expr); got != want {
			t.Errorf("LogQuery(%q) = %t", expr, got)
		}
	}
}
