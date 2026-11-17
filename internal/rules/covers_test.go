package rules

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// A selector covers another that only narrows it: the same metric, offset
// and @, and each of its matchers among the other's, written any way.
func TestCovers(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{`http_requests_total`, `http_requests_total{code=~"5.."}`, true},
		{`http_requests_total{service="api"}`, `http_requests_total{code=~"5..", service = "api"}`, true},
		{`http_requests_total{service="api"}`, `http_requests_total{service="web",code=~"5.."}`, false},
		{`http_requests_total{code=~"5.."}`, `http_requests_total`, false},
		{`http_requests_total`, `http_requests_total`, true},
		{`http_requests_total`, `grpc_requests_total{code="5"}`, false},
		{`http_requests_total offset 1w`, `http_requests_total{code="5"} offset 1w`, true},
		{`http_requests_total`, `http_requests_total{code="5"} offset 1w`, false},
		{`http_requests_total @ 1700000000`, `http_requests_total{code="5"}  @  1700000000`, true},
		{`{__name__="up"}`, `up{job="x"}`, true},
		{`{"http.requests"}`, `{"http.requests", "status.code"="500"}`, true},
		{`{job="x"}`, `up{job="x"}`, true},
		{`up{job="a,b"}`, `up{job="a,b",code="5"}`, true},
		{`up{job="a}"}`, `up{job="a}",code="5"}`, true},
		{`up{job=`, `up{job="x"}`, false},
		{`up{job="x"}`, `up{job=`, false},
		{`up`, `up{}`, true},
	} {
		if got := Covers(c.a, c.b); got != c.want {
			t.Errorf("Covers(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// A selector that only narrows another stands in by that one's series with
// the labels it narrows them by; not one an absent() reads, at any depth.
func TestStandIns(t *testing.T) {
	five := `http_requests_total{code=~"5.."}`
	for _, c := range []struct {
		expr string
		want []StandIn
	}{
		{`sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m])) > 0.05`,
			[]StandIn{{`http_requests_total{code!=""}`, five, `http_requests_total`}}},
		{`sum(rate(x{service="api",code=~"5.."}[5m])) / sum(rate(x{service="api"}[5m]))`,
			[]StandIn{{`x{service="api",code!=""}`, `x{service="api",code=~"5.."}`, `x{service="api"}`}}},
		{`x{code!="200"} / x`, []StandIn{{`x`, `x{code!="200"}`, `x`}}},
		{`x{code=~".*"} / x`, []StandIn{{`x`, `x{code=~".*"}`, `x`}}},
		{`x{code!~"5.."} offset 1w / x offset 1w`, []StandIn{{`x offset 1w`, `x{code!~"5.."} offset 1w`, `x offset 1w`}}},
		{`absent(up{job="new"}) and on() up`, nil},
		{`absent_over_time(up{job="new"}[5m]) or up{job="new"} == 0 and on() count(up) > 0`, nil},
		{`absent(rate(x{code="200"}[5m])) and on() x`, nil},
		{`absent((x{code="200"})) and on() x`, nil},
		{`absent(x{l="a)"}) or x{code="5"} / x`, []StandIn{{`x{code!=""}`, `x{code="5"}`, `x`}}},
		{`probe_success{job=~"fan-.+"} == 0`, nil},
		{`{__name__="x",code="500"} / x`, []StandIn{{`x{code!=""}`, `{__name__="x",code="500"}`, `x`}}},
		// Narrowed by its metric's name: that name, as it is.
		{`http_errors_total{job="api"} / {job="api"}`, []StandIn{{`http_errors_total{job="api"}`, `http_errors_total{job="api"}`, `{job="api"}`}}},
		// A route's and everyone's: of the widest, everyone's requests.
		{`sum(rate(x{code=~"5..",route="/pay"}[5m])) / sum(rate(x{route="/pay"}[5m])) > 0.1 or sum(rate(x{code=~"5.."}[5m])) / sum(rate(x[5m])) > 0.05`,
			[]StandIn{
				{`x{code!="",route!=""}`, `x{code=~"5..",route="/pay"}`, `x`},
				{`x{route!=""}`, `x{route="/pay"}`, `x`},
				{`x{code!=""}`, `x{code=~"5.."}`, `x`},
			}},
		// A route's alone: of its requests.
		{`sum(rate(x{code=~"5..",route="/pay"}[5m])) / sum(rate(x{route="/pay"}[5m])) > 0.1`,
			[]StandIn{{`x{route="/pay",code!=""}`, `x{code=~"5..",route="/pay"}`, `x{route="/pay"}`}}},
	} {
		got, err := StandIns(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: %v, not %v", c.expr, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: %v, not %v", c.expr, got, c.want)
			}
		}
	}
}

// What opens a parenthesis as absent() or absent_over_time() is told as the
// expression `(?:^|[^a-zA-Z0-9_:])(absent|absent_over_time)\s*$` tells it,
// over what comes before the parenthesis, which it was until it was found
// to read the whole of that at each parenthesis.
func TestAbsentBefore(t *testing.T) {
	re := regexp.MustCompile(`(?:^|[^a-zA-Z0-9_:])(absent|absent_over_time)\s*$`)
	cases := []string{"", "absent", " absent", "absent ", "absent \t\n\f\r ", "absent\v", "xabsent", "_absent", ":absent", "9absent",
		"(absent", "+absent_over_time", "absent_over_time", "absent_over_times", "xabsent_over_time", "absent_over", "a(absent", "absent#",
		`"absent`, "sum by (x) absent", "sum by (x) absent_over_time  ", "absentabsent", "absent_absent_over_time"}
	// And many made of the pieces such a text is made of, the same each run.
	pieces := []string{"absent", "_over_time", "absent_over_time", "x", "_", ":", "9", " ", "\t", "\n", "\v", "(", ")", "{", "#", `"`, "é"}
	r := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		var b strings.Builder
		for range r.IntN(6) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		cases = append(cases, b.String())
	}
	for _, s := range cases {
		if got, want := absentBefore(s), re.MatchString(s); got != want {
			t.Errorf("absentBefore(%q) = %v, want %v", s, got, want)
		}
	}
}

// An expression of 1 MiB, of 200,000 parentheses and as many selectors,
// is read in time linear in its length: each parenthesis looked back from
// over what is before it since the last, never over all of it, and what
// follows each selector looked at for its offset and @ no further than
// they go. As it was, the first took most of an hour, and the second,
// lowering all that followed each selector to find an offset, allocated
// as much again at each: some 100 GB. Within absent() at any depth as
// before.
func TestStandInsLongExpression(t *testing.T) {
	var b strings.Builder
	b.WriteString("absent(")
	for b.Len() < 1<<20 {
		b.WriteString("(Up)+")
	}
	expr := b.String() + "Up) or rate(x{code=~\"5..\"}[5m]) / rate(x[5m])"
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	got, err := StandIns(expr)
	runtime.ReadMemStats(&m1)
	if err != nil || len(got) != 1 || got[0] != (StandIn{`x{code!=""}`, `x{code=~"5.."}`, `x`}) {
		t.Errorf("%v, %v", got, err)
	}
	if a := m1.TotalAlloc - m0.TotalAlloc; a > 64<<20 {
		t.Errorf("%d MiB allocated for an expression of %d KiB", a>>20, len(expr)>>10)
	}
	if in := inAbsent(expr); !in[len("absent(")] || !in[len(expr)-len(") or rate(x{code=~\"5..\"}[5m]) / rate(x[5m])")-1] || in[len(expr)-5] {
		t.Error("within absent() told wrong")
	}
}

// An expression's stand-ins are looked for among 256 selectors at most,
// each held against each other: past them, it is refused.
func TestStandInsSelectorsBounded(t *testing.T) {
	sels := func(n int) string {
		var parts []string
		for i := range n {
			parts = append(parts, fmt.Sprintf(`m{a="1",b%d="1"}`, i))
		}
		return strings.Join(parts, " + ")
	}
	if got, err := StandIns(sels(maxStandInSelectors-1) + ` + m{a="1"}`); err != nil || len(got) != MaxStandIns {
		t.Errorf("%d selectors: %d stand-ins, %v", maxStandInSelectors, len(got), err)
	}
	if _, err := StandIns(sels(maxStandInSelectors) + ` + m{a="1"}`); err == nil || !strings.Contains(err.Error(), "257 selectors: stand-ins are looked for among 256 at most") {
		t.Errorf("%d selectors: %v", maxStandInSelectors+1, err)
	}
}
