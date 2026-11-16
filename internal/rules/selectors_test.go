package rules

import (
	"slices"
	"testing"
)

// The selectors of an expression, as written, each once: with a name and
// matchers, a name alone, matchers alone; not the label names of by and
// on, functions, keywords or ranges.
func TestSelectors(t *testing.T) {
	for expr, want := range map[string][]string{
		`sum by (service) (rate(http_requests_total{code=~"5.."}[5m])) / sum by (service) (rate(http_requests_total[5m])) > 0.01`: {
			`http_requests_total{code=~"5.."}`, `http_requests_total`},
		`probe_success{job="blackbox"} == 0 or rate(errors_total {svc="a"}[1h] offset 1d) > 1`: {`probe_success{job="blackbox"}`, `errors_total {svc="a"} offset 1d`},
		`rate(x[5m]) > 2 * rate(x[5m] offset 1w)`:                                              {`x`, `x offset 1w`},
		`x @ 1700000000 offset -5m`:                                                            {`x @ 1700000000 offset -5m`},
		`max(y @ start())`:                                                                     {`y @ start()`},
		`up == 0 and on (instance) up`:                                                         {`up`},
		`{__name__="x", job="y"} > 0`:                                                          {`{__name__="x", job="y"}`},
		`absent(up{job="x"})`:                                                                  {`up{job="x"}`},
		`vector(1)`:                                                                            nil,
		`max_over_time(a[1h:5m]) # a comment with b{c="d"}` + "\n> 2":                          {`a`},
	} {
		got, err := Selectors(expr)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %q, not %q (%v)", expr, got, want, err)
		}
	}
	if _, err := Selectors(`sum(rate(x[5m])`); err == nil {
		t.Error("an expression not closed")
	}
}

// The ranges of an expression's range selectors and subqueries, as written.
func TestRanges(t *testing.T) {
	got, err := Ranges(`max_over_time(rate(a{b="[x]"}[5m])[1h:1m]) > 0`)
	if err != nil || !slices.Equal(got, []string{"5m", "1h:1m"}) {
		t.Errorf("%q %v", got, err)
	}
}
