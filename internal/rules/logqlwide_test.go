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
