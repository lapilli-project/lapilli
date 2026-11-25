package rules

import (
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// Loki 3.7.9 runs the rules of rules.yaml (internal/replay/testdata), as its
// configuration API gives them, a namespace to its groups; and its rules
// API answers each as api.json has it: its LogQL printed again (a binary
// operation between parentheses, by (a,b) without a space, a range with no
// space before it), its for in seconds. The same rule, its Whole another,
// has one Parsed. (Serve reads the configuration API, which gives the rules
// as stored; its copy against a pull request's file is
// TestParsedLokiRulerAndPullRequestFile, in internal/serve.)
func TestParsedLogQLRecordedLoki(t *testing.T) {
	b, err := os.ReadFile("../replay/testdata/loki-3.7.9/rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var all map[string][]any
	if err := yaml.Unmarshal(b, &all); err != nil {
		t.Fatal(err)
	}
	var docs []Doc
	for _, ns := range slices.Sorted(func(yield func(string) bool) {
		for k := range all {
			if !yield(k) {
				return
			}
		}
	}) {
		data, err := yaml.Marshal(map[string]any{"groups": all[ns]})
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, Doc{Source: "loki/" + ns, Namespace: ns, Logs: LokiRuler, Data: data})
	}
	file, err := FromDocs(docs)
	if err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile("../replay/testdata/loki-3.7.9/api.json")
	if err != nil {
		t.Fatal(err)
	}
	api, err := FromAPI(b)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Rule{}
	for _, r := range api {
		// Read as a Loki's rule; the rules API has no namespace, and Parsed
		// has none to leave out.
		r.Logs = LokiRuler
		byName[r.Name] = r
	}
	if len(file) != 9 || len(api) != 9 {
		t.Fatalf("%d rules in the file, %d in the answer", len(file), len(api))
	}
	reprinted := 0
	for _, f := range file {
		a, ok := byName[f.Name]
		if !ok {
			t.Fatalf("%s isn't in the answer", f.Name)
		}
		fp, fok := f.Parsed()
		ap, aok := a.Parsed()
		if !fok || !aok {
			t.Fatalf("%s: not read: %q %q", f.Name, f.Expr, a.Expr)
		}
		if f.Expr != a.Expr {
			reprinted++
		}
		if fp != ap {
			t.Errorf("%s isn't one:\n%s\n%s", f.Name, fp, ap)
		}
	}
	// The API prints all but one of them again.
	if reprinted != len(file)-1 {
		t.Errorf("%d of %d rules written otherwise by the API", reprinted, len(file))
	}
}

// The same expression, written two ways, is one: as Loki 3.7.9 reads and
// prints it.
func TestParsedLogQLAlike(t *testing.T) {
	for _, c := range [][2]string{
		// As Loki prints them again.
		{`sum(count_over_time({app="shop", level="error"}[30s])) > 20`, `(sum(count_over_time({app="shop", level="error"}[30s])) > 20)`},
		{`sum by (app, level) (count_over_time({level="error"}[20s])) > 15`, `(sum by (app,level)(count_over_time({level="error"}[20s])) > 15)`},
		{`sum by (app) (rate({app="shop"} |= "error" [1m])) > 0.5`, `(sum by (app)(rate({app="shop"} |= "error"[1m])) > 0.5)`},
		{`quantile_over_time(0.9, {app="api"} | logfmt | unwrap latency_ms [2m]) by (app) > 400`,
			`(quantile_over_time(0.9,{app="api"} | logfmt | unwrap latency_ms[2m]) by (app) > 400)`},
		{`max by (app) (max_over_time({app="api"} | logfmt | unwrap latency_ms [1m])) > 500`,
			`(max by (app)(max_over_time({app="api"} | logfmt | unwrap latency_ms[1m])) > 500)`},
		{`absent_over_time({app="gappy"}[1m])`, `absent_over_time({app="gappy"}[1m])`},
		{`rate({a="b"}[5m] offset 1h) > 1`, `(rate({a="b"}[5m] offset 1h0m0s) > 1)`},
		{`sum(rate({a="b"}[5m])) > 1000000`, `(sum(rate({a="b"}[5m])) > 1e+06)`},
		{`sum(rate({a="b"}[5m])) > 1.5`, `(sum(rate({a="b"}[5m])) > 1.5)`},
		{`rate({a="b"} | json | x="1" [5m]) + 1`, `(rate({a="b"} | json | x="1"[5m]) + 1)`},
		// The same, written otherwise.
		{`sum(rate({a="b", c="d"}[5m]))`, `sum(rate({c="d", a="b"}[5m]))`},
		{`sum(rate({a="b"}[5m]))`, "sum(rate({a=`b`}[5m]))"},
		{`sum(rate({a="b"}[5m]))`, `SUM(RATE({a="b"}[300s]))`},
		{`sum(rate({a="b"}[5m]))`, `sum ( rate ( { a = "b" } [5m] ) ) # c` + "\n"},
		{`sum by (a, b) (rate({x="y"}[5m]))`, `sum(rate({x="y"}[5m])) by (b, a)`},
		{`sum without (a) (rate({x="y"}[5m]))`, `sum(rate({x="y"}[5m])) WITHOUT (a)`},
		{`sum(rate({x="y"}[5m]))`, `sum by () (rate({x="y"}[5m]))`},
		{`topk(3, rate({x="y"}[5m]))`, `topk by () (3, rate({x="y"}[5m]))`},
		{`topk by (a) (3, rate({x="y"}[5m]))`, `topk(3, rate({x="y"}[5m])) by (a)`},
		{`sum(rate({a="b"}[5m])) > 5`, `((sum(rate({a="b"}[5m]))) > (5))`},
		{`sum(rate({a="b"}[5m])) > 5`, `sum(rate({a="b"}[5m])) > 5.0`},
		{`sum(rate({a="b"}[5m])) > 5`, `sum(rate({a="b"}[5m])) > 5e0`},
		{`sum(rate({a="b"}[5m])) > -5`, `sum(rate({a="b"}[5m])) > - 5`},
		{`sum(rate({a="b"}[5m])) > 5`, `sum(rate({a="b"}[5m])) > +5`},
		{`sum(rate({a="b"}[5m])) > 5`, `sum(rate({a="b"}[5m])) > (5)`},
		{`rate({a="b"}[5m]) * 2 + 3`, `((rate({a="b"}[5m]) * 2) + 3)`},
		{`vector(1) + vector(2) * vector(3)`, `vector(1) + (vector(2) * vector(3))`},
		{`rate({a="b"}[5m]) > bool 1`, `rate({a="b"}[5m]) > BOOL 1`},
		{`rate({a="b"} |= "x" [5m] offset 1h)`, `rate({a="b"}[5m] offset 1h |= "x")`},
		{`rate({a="b"} | unwrap x [5m])`, `rate({a="b"}[5m] | unwrap x)`},
		{`rate({a="b"} |= "x" | unwrap x [5m])`, `rate({a="b"}[5m] |= "x" | unwrap x)`},
		{`sum_over_time({a="b"} | json | unwrap x | y > 1 [5m])`, `sum_over_time({a="b"}[5m] | json | unwrap x | y > 1)`},
		{`rate({a="b"} | json [5m])`, `rate({a="b"} | JSON [5m])`},
		{`rate({a="b"} | x = 5 [5m])`, `rate({a="b"} | x == 5 [5m])`},
		{`rate({a="b"} | x > 5.0 [5m])`, `rate({a="b"} | x > 5 [5m])`},
		{`rate({a="b"} | d > 300s [5m])`, `rate({a="b"} | d > 5m [5m])`},
		{`rate({a="b"} | d > 1500ms [5m])`, `rate({a="b"} | d > 1.5s [5m])`},
		{`rate({a="b"} | json a, b="c" [5m])`, `rate({a="b"} | json a="a", b=` + "`c`" + ` [5m])`},
		{`rate({a="b"} |~ "a.*b" [5m])`, `rate({a="b"} |~ ` + "`a.*b`" + ` [5m])`},
		{`rate({a="b"} | regexp "(?P<x>a)" [5m])`, `rate({a="b"} | regexp ` + "`(?P<x>a)`" + ` [5m])`},
		{`rate({a="b"} | label_format x=y, z="{{.w}}" [5m])`, `rate({a="b"} | label_format x = y , z = "{{.w}}" [5m])`},
		{`rate({a="b"} | drop x, y="z" [5m])`, `rate({a="b"} | drop x,y = "z" [5m])`},
		{`rate({a="b"} | unwrap bytes(x) [5m])`, `rate({a="b"} | unwrap BYTES ( x ) [5m])`},
		{`rate({a="b"} | unwrap duration [5m])`, `rate({a="b"} | unwrap duration [5m])`},
		{`label_replace(rate({a="b"}[5m]), "x", "$1", "y", "(.*)")`, `label_replace(rate({a="b"}[5m]),` + "`x`, `$1`, `y`, `(.*)`" + `)`},
		{`vector(1) > 0`, `vector(1.0) > 0`},
		{`sum(rate({a="b"}[5m])) > 5 and sum(rate({c="d"}[5m])) > 1`, `((sum(rate({a="b"}[5m])) > 5) and (sum(rate({c="d"}[5m])) > 1))`},
		{`vector(2) ^ vector(3) ^ vector(2)`, `vector(2) ^ (vector(3) ^ vector(2))`},
		{`vector(1) - vector(2) - vector(3)`, `(vector(1) - vector(2)) - vector(3)`},
		{`rate({a="b"}[5m])`, `rate ({a="b"}[5m])`},
		{`sum(rate({a="b"}[5m]))`, `sum (rate ({a="b"}[5m]))`},
		{`rate({a="b"} | json | x > - 0.5 [5m])`, `rate({a="b"} | json | x > -.5 [5m])`},
		{`rate({a="b"} | json | x > -0.0 [5m])`, `rate({a="b"} | json | x > - 0 [5m])`},
		{`sum(rate({a="b"}[5m])) - 0`, `sum(rate({a="b"}[5m])) -0.0`},
		{`sort(rate({a="b"}[5m]))`, `sort by () (rate({a="b"}[5m]))`},
		{`rate({a="b", c=~"d|e"}[5m])`, `rate({c=~"d|e",a="b"}[5m])`},
		{`rate({a="b"} | regexp "(?P<x>a)" | pattern "<_>:<y>" [5m])`, `rate({a="b"} | regexp ` + "`(?P<x>a)`" + ` | pattern "<_>:<y>" [5m])`},
	} {
		a, aok := parsedLogQL(c[0])
		b, bok := parsedLogQL(c[1])
		if !aok || !bok || a != b {
			t.Errorf("%q and %q aren't one:\n%t %s\n%t %s", c[0], c[1], aok, a, bok, b)
		}
	}
}

// Each pair may fire otherwise: never one form.
func TestParsedLogQLUnalike(t *testing.T) {
	for _, c := range [][2]string{
		{`sum(rate({a="b"}[5m])) > 5`, `sum(rate({a="b"}[5m])) > 6`},
		{`sum(rate({a="b"}[5m])) > 5`, `sum(rate({a="b"}[5m])) >= 5`},
		{`sum(rate({a="b"}[5m])) > 5`, `sum(rate({a="b"}[5m])) > bool 5`},
		{`sum(rate({a="b"}[5m]))`, `sum(rate({a="c"}[5m]))`},
		{`sum(rate({c="d", a="b"}[5m]))`, `sum(rate({c="d", a!="b"}[5m]))`},
		{`sum(rate({a=~"b"}[5m]))`, `sum(rate({a="b"}[5m]))`},
		{`sum(rate({c="d", a=~"b"}[5m]))`, `sum(rate({c="d", a!~"b"}[5m]))`},
		{`sum(rate({a="b"}[5m]))`, `sum(rate({a="b", c="d"}[5m]))`},
		{`sum(rate({a="b"}[5m]))`, `sum(rate({a="b"}[10m]))`},
		{`sum(rate({a="b"}[5m]))`, `sum(rate({a="b"}[5m] offset 5m))`},
		{`sum(rate({a="b"}[5m]))`, `sum(count_over_time({a="b"}[5m]))`},
		{`sum(rate({a="b"}[5m]))`, `avg(rate({a="b"}[5m]))`},
		{`sum(rate({a="b"}[5m]))`, `sum by (a) (rate({a="b"}[5m]))`},
		{`sum by (a) (rate({a="b"}[5m]))`, `sum without (a) (rate({a="b"}[5m]))`},
		{`sum by (a) (rate({a="b"}[5m]))`, `sum by (a, b) (rate({a="b"}[5m]))`},
		{`sum without () (rate({a="b"}[5m]))`, `sum(rate({a="b"}[5m]))`},
		{`quantile_over_time(0.9, {a="b"} | unwrap x [5m])`, `quantile_over_time(0.99, {a="b"} | unwrap x [5m])`},
		{`quantile_over_time(0.9, {a="b"} | unwrap x [5m])`, `quantile_over_time(0.9, {a="b"} | unwrap x [5m]) by ()`},
		{`rate({a="b"} | unwrap x [5m])`, `rate({a="b"} [5m])`},
		{`rate({a="b"} | unwrap x [5m])`, `rate({a="b"} | unwrap y [5m])`},
		{`rate({a="b"} | unwrap x [5m])`, `rate({a="b"} | unwrap bytes(x) [5m])`},
		{`rate({a="b"} | unwrap bytes(x) [5m])`, `rate({a="b"} | unwrap duration(x) [5m])`},
		{`rate({a="b"} | unwrap x | y > 1 [5m])`, `rate({a="b"} | y > 1 | unwrap x [5m])`},
		{`rate({a="b"} |= "x" [5m])`, `rate({a="b"} |= "y" [5m])`},
		{`rate({a="b"} |= "x" [5m])`, `rate({a="b"} != "x" [5m])`},
		{`rate({a="b"} |= "x" [5m])`, `rate({a="b"} |~ "x" [5m])`},
		{`rate({a="b"} !~ "x" [5m])`, `rate({a="b"} != "x" [5m])`},
		{`rate({a="b"} |> "x" [5m])`, `rate({a="b"} |= "x" [5m])`},
		{`rate({a="b"} |= "x" |= "y" [5m])`, `rate({a="b"} |= "y" |= "x" [5m])`},
		{`rate({a="b"} | json [5m])`, `rate({a="b"} | logfmt [5m])`},
		{`rate({a="b"} | json [5m])`, `rate({a="b"} | json x [5m])`},
		{`rate({a="b"} | json x [5m])`, `rate({a="b"} | json x="y" [5m])`},
		{`rate({a="b"} | label_format x=y [5m])`, `rate({a="b"} | label_format x="y" [5m])`},
		{`rate({a="b"} | drop x [5m])`, `rate({a="b"} | keep x [5m])`},
		{`rate({a="b"} | x = "5" [5m])`, `rate({a="b"} | x == 5 [5m])`},
		{`rate({a="b"} | x > 5 [5m])`, `rate({a="b"} | x >= 5 [5m])`},
		{`rate({a="b"} | x > 5 [5m])`, `rate({a="b"} | x > 5s [5m])`},
		{`rate({a="b"} | x > 5s [5m])`, `rate({a="b"} | x > 5m [5m])`},
		{`rate({a="b"} | line_format "x" [5m])`, `rate({a="b"} | line_format "y" [5m])`},
		{`rate({a="b"} | regexp "x" [5m])`, `rate({a="b"} | pattern "x" [5m])`},
		{`rate({a="b"} | unpack [5m])`, `rate({a="b"} | decolorize [5m])`},
		{`vector(1) + vector(2) * vector(3)`, `(vector(1) + vector(2)) * vector(3)`},
		{`vector(1) - vector(2) - vector(3)`, `vector(1) - (vector(2) - vector(3))`},
		{`vector(1) > vector(2) and vector(3) > vector(4)`, `(vector(1) > vector(2)) and vector(3) > vector(4) or vector(5)`},
		{`rate({a="b"}[5m]) > 5`, `5 > rate({a="b"}[5m])`},
		{`rate({a="b"}[5m]) > 5`, `rate({a="b"}[5m]) > -5`},
		{`topk(3, rate({a="b"}[5m]))`, `topk(4, rate({a="b"}[5m]))`},
		{`topk(3, rate({a="b"}[5m]))`, `bottomk(3, rate({a="b"}[5m]))`},
		{`label_replace(rate({a="b"}[5m]), "x", "$1", "y", "(.*)")`, `label_replace(rate({a="b"}[5m]), "x", "$1", "z", "(.*)")`},
		{`vector(1)`, `vector(2)`},
		{`vector(1) ^ vector(2) ^ vector(3)`, `(vector(1) ^ vector(2)) ^ vector(3)`},
	} {
		a, aok := parsedLogQL(c[0])
		b, bok := parsedLogQL(c[1])
		if aok != bok || aok && a == b {
			t.Errorf("%q and %q are one, or not both read:\n%t %s\n%t %s", c[0], c[1], aok, a, bok, b)
		}
	}
}

// What Loki may read otherwise, or Lapilli isn't sure of, isn't read.
func TestParsedLogQLRefused(t *testing.T) {
	for _, expr := range []string{
		``,
		`  # nothing`,
		// A log query returns lines, not numbers.
		`{a="b"}`,
		`{a="b"} |= "x"`,
		`sum(rate({a="b"}[5m]`,
		`rate({a="b"}[5m]))`,
		`rate({a="b"}[5m]`,
		`rate({}[5m])`,
		`rate({a="b",}[5m])`,
		`rate({a=b}[5m])`,
		`rate({a="b"} [5m] [5m])`,
		`rate({a="b"})`,
		`rate({a="b"}[5m]) >`,
		// Space, comments, a duration in brackets Loki doesn't read so.
		`rate({a="b"}[ 5m])`,
		`rate({a="b"}[5m ])`,
		`rate({a="b"}[5m # c` + "\n])",
		`rate({a="b"}[1.5s])`,
		`rate({a="b"}[300])`,
		`rate({a="b"}[0])`,
		`rate({a="b"}[])`,
		`rate({a="b"}[5m] offset -5m)`,
		`rate({a="b"}[5m] offset 5)`,
		`rate({a="b"}[5m] offset 1d-1y)`,
		`rate #c` + "\n" + `({a="b"}[5m])`,
		`sum #c` + "\n" + `by (a) (rate({a="b"}[5m]))`,
		`rate ({a="b"}[5m]) // c`,
		`rate ({a="b"}[5m]) /* c */`,
		`rate({a="b"} /* c */ [5m])`,
		`rate({a='b'}[5m])`,
		"rate({a=\"b\nc\"}[5m])",
		`rate({a="` + "\uFFFD" + `"}[5m])`,
		`rate({a="\q"}[5m])`,
		"rate({a=`a\rb`}[5m])",
		`rate({métrique="b"}[5m])`,
		// Words of LogQL as a label's name.
		`rate({by="b"}[5m])`,
		`rate({json="b"}[5m])`,
		`rate({OR="b"}[5m])`,
		`rate({a="b"} | rate(x) [5m])`,
		`rate({a="b"} | sum by (x) ="y" [5m])`,
		// What isn't read.
		`rate({a="b"} |= ip("1.1.1.1") [5m])`,
		`rate({a="b"} |= "x" or "y" [5m])`,
		`rate({a="b"} | x = ip("1.1.1.1") [5m])`,
		`rate({a="b"} | x > 5 and y > 1 [5m])`,
		`rate({a="b"} | x > 5 or y > 1 [5m])`,
		`rate({a="b"} | x > 5, y > 1 [5m])`,
		`rate({a="b"} | (x > 5) [5m])`,
		`rate({a="b"} | x > 5 y > 1 [5m])`,
		`rate({a="b"} | x > 5KB [5m])`,
		`rate({a="b"} | x > 0x10 [5m])`,
		`rate({a="b"} | x > 010 [5m])`,
		`rate({a="b"} | x > 1_000 [5m])`,
		`rate({a="b"} | x > -5s [5m])`,
		`rate({a="b"} | x > 1e400 [5m])`,
		`rate({a="b"} | x == "5" [5m])`,
		`rate({a="b"} | x > "5" [5m])`,
		`rate({a="b"} | x =~ 5 [5m])`,
		`rate({a="b"} | logfmt --strict [5m])`,
		`rate({a="b"} | logfmt --keep-empty a [5m])`,
		`rate({a="b"} | json a= [5m])`,
		`rate({a="b"} | json a, [5m])`,
		`rate({a="b"} | label_format x [5m])`,
		`rate({a="b"} | label_format x=5 [5m])`,
		`rate({a="b"} | regexp [5m])`,
		`rate({a="b"} | unwrap [5m])`,
		`rate({a="b"} | unwrap x | json [5m])`,
		`rate({a="b"} | unwrap x |= "y" [5m])`,
		`rate({a="b"} | unwrap x | unwrap y [5m])`,
		`rate({a="b"} | unwrap bytes(x [5m])`,
		`rate({a="b"}[5m] | unwrap x | json)`,
		`rate({a="b"}[5m] | json | unwrap x | y > 1 [5m])`,
		`rate(({a="b"})[5m])`,
		`rate(({a="b"} |= "x")[5m])`,
		`rate(({a="b"}[5m]))`,
		`variants(rate)  of ({a="b"}[5m])`,
		`rate({a="b"}[5m]) > on (a) rate({c="d"}[5m])`,
		`rate({a="b"}[5m]) > ignoring (a) rate({c="d"}[5m])`,
		`rate({a="b"}[5m]) > on (a) group_left rate({c="d"}[5m])`,
		`rate({a="b"}[5m]) > 5m`,
		`rate({a="b"}[5m]) > 0x10`,
		`rate({a="b"}[5m]) > 010`,
		`rate({a="b"}[5m]) > 1_000`,
		`rate({a="b"}[5m]) > 1e400`,
		`rate({a="b"}[5m]) > -rate({a="b"}[5m])`,
		`-rate({a="b"}[5m])`,
		`rate({a="b"}[5m]) = 5`,
		`rate({a="b"}[5m]) =~ 5`,
		// Two numbers: Loki works them out as it reads, a number beside
		// and, or or unless is its error.
		`rate({a="b"}[5m]) > 1 + 2 > 3`,
		`1 + 2`,
		`(1) + (2)`,
		`rate({a="b"}[5m]) and 1`,
		`1 or rate({a="b"}[5m])`,
		`topk(0, rate({a="b"}[5m]))`,
		`topk(3.0, rate({a="b"}[5m]))`,
		`topk((3), rate({a="b"}[5m]))`,
		`topk(rate({a="b"}[5m]))`,
		`sum(3, rate({a="b"}[5m]))`,
		`quantile_over_time({a="b"} | unwrap x [5m])`,
		`rate(0.9, {a="b"}[5m])`,
		`sum(rate({a="b"}[5m])) by (a) by (b)`,
		`sum by (a) (rate({a="b"}[5m])) by (b)`,
		`sum by (a,) (rate({a="b"}[5m]))`,
		`sum(rate({a="b"}[5m])`,
		// What Loki 3.7.9's lexer reads as a duration: a - and the digits
		// and units after it (x > -0 is x>0s in a label filter, where -0.0,
		// -00 and - 0 are x>-0, a number's), and where no duration may be
		// it is a syntax error.
		`rate({a="b"} | json | x > -0 [5m])`,
		`rate({a="b"} | json | x == -0 [5m])`,
		`rate({a="b"} | json | x > -0s [5m])`,
		`rate({a="b"} | json | x > -0e0 [5m])`,
		`rate({a="b"} | json | x > -5m [5m])`,
		`sum(rate({a="b"}[5m])) -0`,
		`sum(rate({a="b"}[5m])) > -0`,
		`sum(rate({a="b"}[5m])) + -0`,
		`sum(rate({a="b"}[5m])) * -0`,
		`sum(rate({a="b"}[5m])) > -5m`,
		`sum(rate({a="b"}[5m])) -5m`,
		`rate({a="b"}[5m] offset 0)`,
		// What Loki 3.7.9 refuses after it has read it (ParseExpr).
		`approx_topk by () (2, rate({a="b"}[5m]))`,
		`approx_topk by (a) (2, rate({a="b"}[5m]))`,
		`approx_topk without () (2, rate({a="b"}[5m]))`,
		`approx_topk(2, rate({a="b"}[5m])) by (a)`,
		`sort by (a) (rate({a="b"}[5m]))`,
		`sort(rate({a="b"}[5m])) without (a)`,
		`sort_desc by (a) (rate({a="b"}[5m]))`,
		`rate({a="b"}[5m]) by (a)`,
		`rate({a="b"}[5m]) by ()`,
		`count_over_time({a="b"}[5m]) by (a)`,
		`bytes_rate({a="b"}[5m]) without (a)`,
		`absent_over_time({a="b"}[5m]) by (a)`,
		`sum_over_time({a="b"} | unwrap x [5m]) by (a)`,
		`avg_over_time({a="b"}[5m])`,
		`quantile_over_time(0.5, {a="b"}[5m])`,
		`rate_counter({a="b"}[5m])`,
		`count_over_time({a="b"} | unwrap x [5m])`,
		`bytes_over_time({a="b"} | unwrap x [5m])`,
		`rate({a=~".*"}[5m])`,
		`rate({a!="b"}[5m])`,
		`rate({a=""}[5m])`,
		`rate({a=~"b|"}[5m])`,
		`rate({a!~"b"}[5m])`,
		`rate({a=~"b*", c!="d"}[5m])`,
		`rate({a=~"("}[5m])`,
		`rate({a="b"} | json | x =~ "(" [5m])`,
		`rate({a="b"} | drop x=~"(" [5m])`,
		`rate({a="b"} | regexp "(" [5m])`,
		`rate({a="b"} | regexp ".*" [5m])`,
		`rate({a="b"} | regexp "(.*)" [5m])`,
		`rate({a="b"} | pattern "x" [5m])`,
		`rate({a="b"} | pattern "<_>" [5m])`,
		`rate({a="b"} | pattern "<a><b>" [5m])`,
		`rate({a="b"} | pattern "<a> <a>" [5m])`,
		`label_replace(vector(1), "a", "b", "c", "(")`,
		`label_replace(rate({a="b"}[5m]), "x", "$1", "y")`,
		`label_replace(rate({a="b"}[5m]), "x", "$1", "y", 5)`,
		`vector(x)`,
		`vector(-1)`,
		`vector()`,
		`up`,
		`rate`,
		// Bounds: 16 KiB, 32 levels of brackets.
		strings.Repeat("(", 33) + `vector(1)` + strings.Repeat(")", 33),
		strings.Repeat("sum(", 32) + `rate({a="b"}[5m])` + strings.Repeat(")", 32),
		`vector(1) > ` + strings.Repeat("vector(1) + ", 2<<10) + `vector(1)`,
	} {
		if got, ok := parsedLogQL(expr); ok {
			t.Errorf("%q read: %s", expr, got)
		}
	}
	// 32 levels are read.
	if _, ok := parsedLogQL(strings.Repeat("(", 31) + `vector(1)` + strings.Repeat(")", 31)); !ok {
		t.Error("31 parentheses deep, not read")
	}
	if _, ok := parsedLogQL(strings.Repeat("sum(", 30) + `rate({a="b"}[5m])` + strings.Repeat(")", 30)); !ok {
		t.Error("30 aggregations deep, not read")
	}
}

// A Loki ruler's rule: its Parsed has its labels, for, limit and the rest as
// a PromQL rule's does, and is never a PromQL rule's of the same text.
func TestRuleParsedLogQL(t *testing.T) {
	logql := Rule{Name: "Errors", Group: "shop", Namespace: "ns", Logs: LokiRuler, For: time.Minute, Interval: time.Minute,
		Expr: `sum(count_over_time({app="shop"}[5m])) > 10`, Labels: map[string]string{"severity": "page"}}
	one := func(r Rule) string {
		t.Helper()
		s, ok := r.Parsed()
		if !ok {
			t.Fatalf("%+v: no Parsed", r)
		}
		return s
	}
	want := one(logql)
	reprinted := logql
	reprinted.Expr = `(sum(count_over_time({app="shop"}[5m])) > 10)`
	if got := one(reprinted); got != want {
		t.Errorf("reprinted:\n%s\n%s", got, want)
	}
	if logql.Whole() == reprinted.Whole() {
		t.Error("its Whole is one: nothing to see")
	}
	// Not a PromQL rule's of the same text, which parsedExpr reads too.
	prom := logql
	prom.Logs = ""
	prom.Expr = `vector(1) > 0`
	logql2 := logql
	logql2.Expr = prom.Expr
	if p, ok := prom.Parsed(); !ok {
		t.Fatal("PromQL not read")
	} else if l := one(logql2); l == p || strings.HasPrefix(p, "parsed logql") {
		t.Errorf("one with PromQL's:\n%s\n%s", l, p)
	}
	for name, change := range map[string]func(*Rule){
		"another expression":  func(r *Rule) { r.Expr = `sum(count_over_time({app="shop"}[5m])) > 11` },
		"another for":         func(r *Rule) { r.For = time.Hour },
		"another interval":    func(r *Rule) { r.Interval = time.Hour },
		"another label":       func(r *Rule) { r.Labels = map[string]string{"severity": "ticket"} },
		"another group":       func(r *Rule) { r.Group = "cart" },
		"a limit":             func(r *Rule) { r.Limit = 5 },
		"a keep_firing_for":   func(r *Rule) { r.KeepFiringFor = time.Minute },
		"a Grafana's Loki":    func(r *Rule) { r.Logs = "uid" },
		"a stream filtered":   func(r *Rule) { r.Expr = `sum(count_over_time({app="shop"} |= "x" [5m])) > 10` },
		"a different matcher": func(r *Rule) { r.Expr = `sum(count_over_time({app=~"shop"}[5m])) > 10` },
	} {
		r := logql
		r.Labels = map[string]string{"severity": "page"}
		change(&r)
		if got := one(r); got == want {
			t.Errorf("%s: the same Parsed", name)
		}
	}
	// Where a ruler keeps its group isn't when its rule fires.
	{
		r := logql
		r.Labels = map[string]string{"severity": "page"}
		r.Namespace = "other"
		if got := one(r); got != want {
			t.Errorf("another namespace: another Parsed:\n%s\n%s", got, want)
		}
	}
	for name, r := range map[string]Rule{
		"PromQL, not LogQL": {Name: "L", Expr: `x > 1`, Logs: LokiRuler},
		"too long":          {Name: "L", Expr: `vector(1) > ` + strings.Repeat("vector(1) + ", 2<<10) + `vector(1)`, Logs: LokiRuler},
		"a log query":       {Name: "L", Expr: `{a="b"}`, Logs: LokiRuler},
		"a Grafana rule":    {Name: "G", Graph: &Graph{}, Logs: "uid"},
	} {
		if s, ok := r.Parsed(); ok {
			t.Errorf("%s: %s", name, s)
		}
	}
}

// A cloud's alarm in PromQL: the same expression written two ways is one;
// never one with another cloud's, nor with Prometheus's; one that isn't
// PromQL isn't read.
func TestRuleParsedCloud(t *testing.T) {
	base := Rule{Name: "Errors", Expr: `sum by (a, b) (rate(x[5m])) > 1`, Cloud: "cloudwatch", For: time.Minute, Interval: time.Minute,
		Labels: map[string]string{}}
	one := func(r Rule) string {
		t.Helper()
		s, ok := r.Parsed()
		if !ok {
			t.Fatalf("%+v: no Parsed", r)
		}
		return s
	}
	want := one(base)
	again := base
	again.Expr = "SUM(rate(x[300s])) by (b,a) > 1.0 # c"
	if got := one(again); got != want {
		t.Errorf("written otherwise:\n%s\n%s", got, want)
	}
	for name, change := range map[string]func(*Rule){
		"another cloud":      func(r *Rule) { r.Cloud = "google" },
		"another expression": func(r *Rule) { r.Expr = `sum by (a, b) (rate(x[5m])) > 2` },
		"another for":        func(r *Rule) { r.For = time.Hour },
		"Prometheus":         func(r *Rule) { r.Cloud = "" },
		"a Loki ruler":       func(r *Rule) { r.Cloud, r.Logs = "", LokiRuler },
	} {
		r := base
		change(&r)
		got, ok := r.Parsed()
		if name == "a Loki ruler" {
			if ok {
				t.Errorf("%s: PromQL read as LogQL: %s", name, got)
			}
			continue
		}
		if !ok || got == want {
			t.Errorf("%s: %t, the same Parsed or none: %s", name, ok, got)
		}
	}
	// Each cloud's, two by two.
	seen := map[string]string{"": one(Rule{Name: "Errors", Expr: base.Expr, For: time.Minute, Interval: time.Minute, Labels: map[string]string{}})}
	for _, cloud := range []string{"cloudwatch", "google", "azure"} {
		r := base
		r.Cloud = cloud
		s := one(r)
		if was, ok := seen[s]; ok {
			t.Errorf("%s and %q are one", cloud, was)
		}
		seen[s] = cloud
	}
	for name, r := range map[string]Rule{
		"not PromQL (an OCI alarm)":           {Name: "O", Cloud: "oci", Unsupported: "an OCI alarm is written in MQL, not PromQL"},
		"a CloudWatch metric alarm":           {Name: "C", Cloud: "cloudwatch", Unsupported: "a CloudWatch metric alarm isn't PromQL"},
		"a Google Cloud condition":            {Name: "G", Cloud: "google", Unsupported: "a Google Cloud threshold condition isn't PromQL"},
		"a query that isn't read":             {Name: "Q", Cloud: "azure", Expr: `x > 1_000`},
		"a query that is no PromQL (LogQL's)": {Name: "Q", Cloud: "google", Expr: `{a="b"} |= "x"`},
	} {
		if s, ok := r.Parsed(); ok {
			t.Errorf("%s: %s", name, s)
		}
	}
}

// Whatever is given, it is read to an end or refused: never a panic, nor
// two ways.
func FuzzParsedLogQL(f *testing.F) {
	for _, s := range []string{`sum by (app) (rate({app="shop"} |= "error" [1m])) > 0.5`,
		`quantile_over_time(0.9, {app="api"} | logfmt | unwrap latency_ms [2m]) by (app) > 400`,
		`rate({a="b"}[5m] offset 1h | json | x > 5s | unwrap bytes(y) | z="1")`,
		"max(rate({a=`b`} | label_format x=y, z=\"w\" | drop a, b=\"c\" [5m])) # c\n",
		`label_replace(rate({a="b"}[5m]), "x", "$1", "y", "(.*)") > bool 1e+06`,
		`vector(2) ^ vector(3) ^ vector(2)`, `topk by (a) (3, rate({a="b"}[5m]))`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		a, aok := parsedLogQL(expr)
		b, bok := parsedLogQL(expr)
		if a != b || aok != bok {
			t.Errorf("%q read twice otherwise", expr)
		}
		// What is read, written, is read again as itself... when it is
		// LogQL as Loki prints it: not always, so only that it doesn't
		// panic and is stable.
		if aok {
			if c, cok := parsedLogQL(expr + " "); !cok || c != a {
				t.Errorf("%q: space after it changes it: %q", expr, c)
			}
		}
	})
}

// The words of a function's name read ahead of a space and a ( or by, and
// no more of the expression than that: the work is linear in the
// expression, as the bytes it allocates show (it lowered all that was left
// for each word, 27 MB for 16 KiB).
func TestParsedLogQLFunctionWordsAreLinear(t *testing.T) {
	expr := `count_over_time({a="b"}` + strings.Repeat(` | RATE="x"`, 1300) + ` [5m])`
	if len(expr) > 15<<10 || len(expr) < 13<<10 {
		t.Fatalf("%d bytes", len(expr))
	}
	if _, ok := parsedLogQL(expr); !ok {
		t.Fatal("not read")
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	parsedLogQL(expr)
	runtime.ReadMemStats(&after)
	if used := after.TotalAlloc - before.TotalAlloc; used > 2<<20 {
		t.Errorf("%d bytes allocated for %d", used, len(expr))
	}
}
