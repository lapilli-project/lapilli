package rules

import "testing"

// Wide holds each selector to one metric, named: matchers alone, a name as
// a pattern or a negation, and an empty name are wide; a name bare,
// quoted, or as __name__="x" is not, whatever else the selector matches.
func TestWide(t *testing.T) {
	for expr, want := range map[string]string{
		`rate(http_requests_total{code=~"5.."}[5m]) > 1`: "",
		`up == 0`:                           "",
		`{__name__="up"} == 0`:              "",
		`{"http.requests", code="500"} > 0`: "",
		`sum by (job) (rate(errors_total[5m])) / sum by (job) (rate(requests_total[5m])) > 0.1`: "",
		`{job="api"} > 0`:              `{job="api"}`,
		`{__name__=~"secret_.+"} > 41`: `{__name__=~"secret_.+"}`,
		`count({__name__!="up"}) > 0`:  `{__name__!="up"}`,
		`absent({"__name__"=~".+"})`:   `{"__name__"=~".+"}`,
		`up > 0 and {instance="db"}`:   `{instance="db"}`,
		`label_replace(up, "x", "$1", "instance", "(.*)") > 0 or {a="b"}`: `{a="b"}`,
	} {
		got, wide := Wide(expr)
		if wide != (want != "") || got != want {
			t.Errorf("Wide(%s) = %q %t, want %q", expr, got, wide, want)
		}
	}
}
