package scenario

import (
	"fmt"
	"strings"
	"testing"
)

// Values notation is counted as promtool's parser expands it: an item a
// value, a+bxN and axN N+1, _xN N blanks, a histogram an item whatever it
// holds; a count past the bound, or that isn't a whole number promtool
// reads, is refused before anything is expanded.
func TestSamples(t *testing.T) {
	for values, want := range map[string]int{
		"":                   0,
		"1 2 3":              3,
		"0+1x10":             11,
		"5x3":                4,
		"1 -1x4 _ _x3 stale": 1 + 5 + 1 + 3 + 1,
		"\t1\n2  3 ":         3,
		"{{schema:0 sum:5 count:4 buckets:[1 2 1]}}":                                          1,
		"{{schema:0 sum:5 count:4 buckets:[1 2 1]}}x3":                                        4,
		"{{schema:0 sum:5 count:4 buckets:[1 2 1]}}+{{schema:0 sum:1 count:1 buckets:[1]}}x5": 6,
		"0x10": 11, // never read as hexadecimal in a series: at most
	} {
		got, err := Samples(values)
		if err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", values, got, err, want)
		}
	}
	for _, values := range []string{"0+1x100000000", "_x100001", "1x18446744073709551616", "{{schema:0 sum:5}}x200000", "{{schema:0 sum:5",
		strings.Repeat("0+1x60000 ", 2)} {
		if _, err := Samples(values); err == nil {
			t.Errorf("%q: not refused", values)
		}
	}
}

// A test file is refused past its bounds, before promtool runs it: tests,
// series, the values they expand to, in a test and in all, and how often
// a test evaluates the rules; what isn't promtool's to read isn't read.
func TestBound(t *testing.T) {
	ok := "tests:\n  - interval: 1m\n    input_series:\n      - series: up{job=\"a\"}\n        values: 0+1x100\n    alert_rule_test:\n      - eval_time: 2h\n        alertname: Down\n"
	if err := Bound([]byte(ok)); err != nil {
		t.Fatalf("a plain test: %v", err)
	}
	many := func(n int, item string) string {
		var b strings.Builder
		b.WriteString("tests:\n")
		for range n {
			b.WriteString(item)
		}
		return b.String()
	}
	series := func(n int) string {
		var b strings.Builder
		b.WriteString("  - input_series:\n")
		for i := range n {
			fmt.Fprintf(&b, "      - {series: 's%d', values: '1'}\n", i)
		}
		return b.String()
	}
	for want, file := range map[string]string{
		"101 tests: 100 at most":                   many(101, "  - input_series: []\n"),
		"101 input series: 100 a test at most":     "tests:\n" + series(101),
		"more than 1000 input series in all":       many(11, series(100)),
		"expand to more than 100000 values in all": many(3, "  - input_series:\n      - {series: a, values: '0+1x40000'}\n"),
		"values repeat 100000000 times":            strings.Replace(ok, "0+1x100", "0+1x100000000", 1),
		"evaluated 20000 times, every 1m0s":        strings.Replace(ok, "eval_time: 2h", "eval_time: 20000m", 1),
		"evaluated 72000 times, every 1s":          "evaluation_interval: 1s\n" + strings.Replace(ok, "eval_time: 2h", "eval_time: 20h", 1),
		"evaluation_interval \"0s\"":               "evaluation_interval: 0s\n" + ok,
		"eval_time \"soon\"":                       strings.Replace(ok, "eval_time: 2h", "eval_time: soon", 1),
		"values is a text":                         "tests:\n  - input_series:\n      - {series: a, values: [1, 2]}\n",
		"promql_expr_test":                         "tests:\n  - promql_expr_test:\n      - {expr: up, eval_time: 200000m}\n",
		"it can't be read as promtool's test file": "tests: {a: 1}\n",
	} {
		err := Bound([]byte(file))
		if want == "promql_expr_test" {
			want = "evaluated 200000 times"
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", want, err, want)
		}
	}
	// An alias is read as promtool reads it, its expansion bounded as the
	// rules' is; a value that is one is refused, as not plain text.
	if err := Bound([]byte("x: &v '0+1x10'\ntests:\n  - input_series:\n      - {series: a, values: *v}\n")); err == nil || !strings.Contains(err.Error(), "values is a text") {
		t.Errorf("an alias: %v", err)
	}
}
