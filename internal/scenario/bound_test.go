package scenario

import (
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"lapilli.dev/internal/rules"
)

// Values notation is counted as promtool's parser expands it: an item a
// value, a+bxN and axN N+1, _xN N blanks, a histogram the numbers it holds
// each time it is repeated; a count past the bound, or that isn't a whole
// number promtool reads, is refused before anything is expanded, and so
// is what isn't values notation.
func TestSamples(t *testing.T) {
	hist := "{{schema:0 sum:5 count:4 buckets:[1 2 1]}}" // six numbers
	for values, want := range map[string]int{
		"":                            0,
		"1 2 3":                       3,
		"0+1x10":                      11,
		"5x3":                         4,
		"_x3":                         3,
		"1 -1x4 _ _x3 stale":          1 + 5 + 1 + 3 + 1,
		"\t1\n2  3 ":                  3,
		"Inf -Inf NaN 1e3 .5 0x00010": 5 + 11,
		"0x10":                        11, // never read as hexadecimal in a series: at most
		hist:                          6,
		hist + "x3":                   6 * 4,
		"{{}}":                        1,
		"{{ }}x9":                     10,
		"{{sum:1 count:1}}x3":         2 * 4,
		"{{schema:-53 sum:1 count:2 custom_values:[1 5 10] buckets:[1 0 1 0]}}":      10,
		"{{counter_reset_hint:gauge sum:-1.5e3 z_bucket:1 z_bucket_w:0.1 offset:2}}": 5,
		hist + "+{{schema:0 sum:1 count:1 buckets:[1]}}x5":                           (6 + 4) * 6,
		hist + "-" + hist + "x5":       12 * 6,
		strings.Repeat("{{}} ", 60000): 60000,
	} {
		got, err := Samples(values)
		if err != nil || got != want {
			t.Errorf("%.80q: %d %v, want %d", values, got, err, want)
		}
	}
	// A histogram of 1,000 buckets repeated 200 times: 201 copies of a
	// thousand numbers, though 201 values.
	buckets := "{{schema:0 sum:1 count:1 buckets:[" + strings.TrimSpace(strings.Repeat("1 ", 1000)) + "]}}"
	for _, c := range []struct{ values, want string }{
		{"0+1x100000000", "more than 100000"},
		{"_x100001", "repeat 100001 times"},
		{"1x18446744073709551616", "repeat 18446744073709551616 times"},
		{"1x100000000x3", "repeat 100000000 times"}, // every repetition, not the last alone
		{"1x60000x60000", "expand to more than 100000"},
		{"1x9223372036854775807", "repeat 9223372036854775807 times"},
		{"{{schema:0 sum:5}}x200000", "repeat 200000 times"},
		{"{{schema:0 sum:5", "isn't closed"},
		{strings.Repeat("0+1x60000 ", 2), "expand to more than 100000"},
		{buckets + "x200", "expand to more than 100000"},
		{buckets + "+" + buckets + "x60", "expand to more than 100000"},
		{"{{}}+{{}}+{{}}x2", "more than two histograms"},
		{strings.Repeat("{{}}", 60000), "more than two histograms"},
		{"1 a{b} 0+1x5", "a brace that doesn't open"},
		{"1 a{b", "a brace that doesn't open"},
		{`1 {b="{{"} 0+1x100000000 "}}"`, "a brace that doesn't open"},
		{`1 "a"`, `'"' isn't values notation`},
		{"1 =", "'=' isn't values notation"},
		{"1\u00a02", `'\u00a0' isn't values notation`},
		{"1x", "x is a repetition"},
		{"stalex", "x is a repetition"},
		{`{{sum:"5"}}`, `'"' in a histogram`},
		{"{{sum:1 {{count:1}}}}", "'{' in a histogram"},
		{"{{buckets:[1 2}}", "'}' in a histogram"},
		{"{{buckets:1]}}", "']' in a histogram"},
		{"{{buckets:[[1]]}}", "'[' in a histogram"},
		{strings.Repeat(" ", MaxTest+1), "more than 1048576 bytes, as a test file"},
	} {
		if _, err := Samples(c.values); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%.60q: %v, want %q", c.values, err, c.want)
		}
	}
}

// A series is a metric name, its labels in braces, or both, as promtool's
// load command reads one at the start of a line; a space, a line break or
// anything else outside a label's quoted value is refused, as promtool
// would read it as values, or as another series.
func TestSeries(t *testing.T) {
	for _, s := range []string{
		"up", "a:b_c", "_", "up{}", "{}", `up{job="a"}`, `{job="a"}`, `up{job="a",}`,
		`up{a="sp ace, \"quoted\" {{ x5 }} 0+1x100000000",b='single \' q',c=` + "`raw \\`" + `}`,
		`{"my.metric"}`, `{"my.metric","le.x"="1"}`, `up{a="ünï ✓"}`, `up{a="\n is an escape"}`,
	} {
		if err := Series(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	const outside, control = "no space, line break or anything else", "line break or another control character"
	for _, c := range []struct{ series, want string }{
		{"", "series is empty"},
		{"big 0+0x100000000", outside},
		{"big{} 0+0x100000000", outside},
		{"big\n0+0x5", outside},
		{"big{}\n0+0x5", outside},
		{`up {a="b"}`, outside},
		{`up{a = "b"}`, outside},
		{`up{a="b"}x5`, outside},
		{`up{a="b"`, outside},
		{"up{a=b}", outside},
		{`up{a="b"}}`, outside},
		{"{,}", outside},
		{"9up", outside},
		{`up{a:b="c"}`, outside},
		{"#up", outside},
		{`up{a="b\`, outside},
		{`up{a!="b"}`, outside},
		{"big{a=\"x\nbig 0+0x100000000\n\"}", control},
		{"big{a=`x\ny`}", control},
		{"big{a=\"x\\\ny\"}", control},
		{"big{a=\"x\ty\"}", control},
		{"big{a=\"\xff\"}", control},
	} {
		if err := Series(c.series); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want %q", c.series, err, c.want)
		}
	}
}

// A test file is refused past its bounds, before promtool runs it: tests,
// series, the values they expand to, in a test and in all, and how often
// a test evaluates the rules; what isn't promtool's to read isn't read.
// What promtool reads as values notation is all counted: an input
// series' series is a series alone, carrying no values and no line of its
// own; its values hold no line break; a histogram counts its buckets each
// time it is repeated; an expected sample's histogram counts too; and a
// tag that would have promtool decode another text is refused.
func TestBound(t *testing.T) {
	ok := "tests:\n  - interval: 1m\n    input_series:\n      - series: up{job=\"a\"}\n        values: 0+1x100\n    alert_rule_test:\n      - eval_time: 2h\n        alertname: Down\n"
	if err := Bound([]byte(ok)); err != nil {
		t.Fatalf("a plain test: %v", err)
	}
	for _, file := range []string{
		// A block's last line break begins no line: promtool's load
		// command ends there.
		"tests:\n  - input_series:\n      - series: up\n        values: |\n          1 2 3\n",
		"tests:\n  - input_series:\n      - {series: up, values: null}\n      - {series: down, values: !!str 1 2}\n",
		"tests:\n  - promql_expr_test:\n      - expr: h\n        exp_samples:\n          - {labels: 'h', histogram: '{{schema:0 sum:5 count:4 buckets:[1 2 1]}}'}\n",
	} {
		if err := Bound([]byte(file)); err != nil {
			t.Errorf("%q: %v", file, err)
		}
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
	// About 40 KB: two histograms of 10,000 buckets, added up a hundred
	// times, which promtool copies at each step.
	buckets := "{{schema:0 sum:1 count:1 buckets:[" + strings.TrimSpace(strings.Repeat("1 ", 10000)) + "]}}"
	binary := base64.StdEncoding.EncodeToString([]byte("0+0x100000000"))
	for _, c := range []struct{ file, want string }{
		{many(101, "  - input_series: []\n"), "101 tests: 100 at most"},
		{"tests:\n" + series(101), "101 input series: 100 a test at most"},
		{many(11, series(100)), "more than 1000 input series in all"},
		{many(3, "  - input_series:\n      - {series: a, values: '0+1x40000'}\n"), "expand to more than 100000 values in all"},
		{strings.Replace(ok, "0+1x100", "0+1x100000000", 1), "values repeat 100000000 times"},
		{strings.Replace(ok, "eval_time: 2h", "eval_time: 20000m", 1), "evaluated 20000 times, every 1m0s"},
		{"evaluation_interval: 1s\n" + strings.Replace(ok, "eval_time: 2h", "eval_time: 20h", 1), "evaluated 72000 times, every 1s"},
		{"evaluation_interval: 0s\n" + ok, `evaluation_interval "0s"`},
		{strings.Replace(ok, "eval_time: 2h", "eval_time: soon", 1), `eval_time "soon"`},
		{"tests:\n  - input_series:\n      - {series: a, values: [1, 2]}\n", "values is a text"},
		{"tests:\n  - promql_expr_test:\n      - {expr: up, eval_time: 200000m}\n", "evaluated 200000 times"},
		{"tests: {a: 1}\n", "it can't be read as promtool's test file"},
		// Values in its series: promtool writes "  big 0+0x100000000 1".
		{"tests:\n  - input_series:\n      - {series: 'big 0+0x100000000', values: '1'}\n", "test 1: line 3: series is a metric name"},
		{"tests:\n  - input_series:\n      - {series: \"up\\nbig 0+0x100000000\", values: '1'}\n", "series is a metric name"},
		{"tests:\n  - input_series:\n      - {values: 'big 0+0x100000000'}\n", "series is empty"},
		{"tests:\n  - input_series:\n      - {series: [up], values: '1'}\n", "series is a text"},
		// A line break in values, and the line after it a series whose
		// label hides what follows it from a count by items.
		{"tests:\n  - input_series:\n      - {series: a, values: \"1\\nbig{a=\\\"{{\\\"} 0+0x100000000 1}}\"}\n", "test 1: line 3: values holds a line break"},
		{"tests:\n  - input_series:\n      - {series: a, values: \"1\\nbig 0+0x5\"}\n", "values holds a line break"},
		{"tests:\n  - input_series:\n      - {series: h, values: '" + buckets + "+" + buckets + "x100'}\n", "test 1: line 3: values expand to more than 100000"},
		{"tests:\n  - input_series:\n      - {series: h, values: '" + buckets + "x10'}\n", "values expand to more than 100000"},
		// An expected sample's histogram is read as values notation.
		{"tests:\n  - promql_expr_test:\n      - expr: up\n        exp_samples:\n          - {labels: 'up', histogram: '0+0x100000000'}\n", "test 1: line 5: values repeat 100000000 times"},
		{"tests:\n  - input_series:\n      - {series: a, values: '0+1x60000'}\n    promql_expr_test:\n      - expr: up\n        exp_samples:\n          - {labels: 'up', histogram: '0+0x60000'}\n",
			"expand to more than 100000 values in all"},
		{"tests:\n  - promql_expr_test:\n      - expr: up\n        exp_samples:\n          - {labels: 'up', histogram: !!binary " + binary + "}\n", "line 5: a tag isn't read"},
		// A tag promtool's YAML decodes into another text.
		{"tests:\n  - input_series:\n      - {series: a, values: !!binary " + binary + "}\n", "line 3: a tag isn't read"},
		{"tests:\n  - input_series:\n      - {series: !!binary " + base64.StdEncoding.EncodeToString([]byte("big 0+0x100000000")) + ", values: '1'}\n", "line 3: a tag isn't read"},
	} {
		if err := Bound([]byte(c.file)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want %q", c.want, err, c.want)
		}
	}
	// An alias, its anchor first, is refused, as YAML reads it past what
	// is written (TestBoundPlain).
	if err := Bound([]byte("x: &v '0+1x10'\ntests:\n  - input_series:\n      - {series: a, values: *v}\n")); err == nil || !strings.Contains(err.Error(), "line 1: an anchor or an alias") {
		t.Errorf("an alias: %v", err)
	}
}

// TestBoundPlain: what YAML reads past what is written, which Bound's YAML
// and promtool's read otherwise, is refused: a merge after a series' own
// values, which promtool's yaml.v2 lets override them (a hundred million
// values, not the one counted), after its eval_time, its series, or with
// a tag; an anchor, an alias; a tag but !!str on a text.
func TestBoundPlain(t *testing.T) {
	head := "tests:\n  - interval: 1m\n    input_series:\n"
	for _, c := range []struct{ file, want string }{
		{head + "      - series: 'up{job=\"x\"}'\n        values: '1'\n        <<: {values: '0+0x100000000'}\n", "line 6: a merge (<<)"},
		{head + "      - {series: up, values: '1', <<: {values: '0+0x100000000'}}\n", "line 4: a merge (<<)"},
		{head + "      - series: up\n        values: '1'\n    alert_rule_test:\n      - alertname: X\n        eval_time: 1m\n        <<: {eval_time: 100000000h}\n", "line 9: a merge (<<)"},
		{head + "      - series: up\n        values: '1'\n        <<: {series: \"up\\nbig 0+0x100000000\"}\n", "a merge (<<)"},
		{head + "      - series: up\n        values: '1'\n        \"<<\": {values: '0+0x100000000'}\n", "a merge (<<)"},
		{head + "      - series: up\n        values: '1'\n        !!merge <<: {values: '0+0x100000000'}\n", "line 6: a merge (<<)"},
		{head + "      - &s {series: up, values: '1'}\n      - *s\n", "line 4: an anchor or an alias"},
		{head + "      - {series: up, values: !!binary MSAy}\n", "line 4: a tag"},
		{head + "      - !!map {series: up, values: '1'}\n", "line 4: a tag"},
	} {
		if err := Bound([]byte(c.file)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want %q", c.file, err, c.want)
		}
	}
	// A key's or a value's text that holds << is text.
	for _, file := range []string{
		head + "      - {series: up, values: !!str 1 2}\n",
		head + "      - {series: 'up{a=\"<<\"}', values: '1'}\n",
	} {
		if err := Bound([]byte(file)); err != nil {
			t.Errorf("%q: %v", file, err)
		}
	}
}

// With a real promtool on PATH, what Bound takes is read as promtool reads
// it: each form of a series and of values notation it lets through loads
// what Samples counts; and a series carrying values, which Bound refuses,
// has promtool load them. Skipped without one.
func TestBoundRealPromtool(t *testing.T) {
	path, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("no promtool on PATH")
	}
	p := Promtool{Path: path}
	g, err := Group([]rules.Rule{{Name: "Quiet", Group: "shop", Expr: `vector(1) > 2`, Prometheus: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// run has promtool load one series, a value a minute from 0, and
	// count its samples an hour on.
	run := func(series, values, selector string, n int) Result {
		t.Helper()
		b, err := Rewrite([]byte(fmt.Sprintf("tests:\n  - interval: 1m\n    input_series:\n      - series: '%s'\n        values: '%s'\n"+
			"    promql_expr_test:\n      - expr: 'sum(count_over_time(%s[2h]))'\n        eval_time: 1h\n        exp_samples:\n          - {labels: '{}', value: %d}\n",
			strings.ReplaceAll(series, "'", "''"), values, strings.ReplaceAll(selector, "'", "''"), n)))
		if err != nil {
			t.Fatal(err)
		}
		return p.Run(context.Background(), g, b)
	}
	for _, c := range []struct {
		series, values, selector string
		loaded                   int // samples promtool keeps: a blank none, a stale marker none counted
	}{
		{"a", "0+1x10 _x3 stale 1", "a", 11 + 1},
		{`b{job="x y",le='0.5',raw=` + "`r`" + `,}`, "1 2 3", `b{job="x y"}`, 3},
		{`{"c.d","e.f"="g"}`, "-1x4 Inf", `{"c.d"}`, 6},
		{"h", "{{schema:0 sum:5 count:4 buckets:[1 2 1]}}+{{schema:0 sum:1 count:1 buckets:[1]}}x5 {{}}x2", "h", 6 + 3},
	} {
		file := fmt.Sprintf("tests:\n  - input_series:\n      - series: '%s'\n        values: '%s'\n", strings.ReplaceAll(c.series, "'", "''"), c.values)
		if err := Bound([]byte(file)); err != nil {
			t.Errorf("%s: %v", c.series, err)
		}
		if r := run(c.series, c.values, c.selector, c.loaded); r.State != Passed {
			t.Errorf("%s: promtool %+v", c.series, r)
		}
	}
	// What Bound refuses as a series, promtool reads as one carrying
	// values: "  big 0+0x5 1", seven of them.
	if err := Bound([]byte("tests:\n  - input_series:\n      - {series: 'big 0+0x5', values: '1'}\n")); err == nil {
		t.Error("a series carrying values: not refused")
	}
	if r := run("big 0+0x5", "1", "big", 7); r.State != Passed {
		t.Errorf("promtool doesn't load a series' own values: %+v", r)
	}
}
