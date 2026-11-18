package rules

import (
	"strings"
	"testing"
	"time"
)

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

// Reach holds what a rule reads to a week back, a subquery to 11,000
// points, and no @: what alerts write passes.
func TestReach(t *testing.T) {
	week := 7 * 24 * time.Hour
	for expr, want := range map[string]string{
		`rate(errors_total[5m]) > 1`:    "",
		`max_over_time(up[1h:1m]) == 0`: "",
		`rate(http_requests_total[5m] offset 1w) > 2 * rate(http_requests_total[5m])`: "",
		`predict_linear(disk_free[7d], 86400) < 0`:                                    "",
		`up{offset="x"} == 0`: "",
		`label_replace(up, "dst", "$1", "offset", "(.*)") > 0`: "",
		`max_over_time(up[30d:1s]) > 0`:                        "a range past",
		`max_over_time(up[1d:1s]) > 0`:                         "points",
		`rate(errors_total[5m] offset 5y) > 1`:                 "offset 5y",
		`rate(errors_total[5m] offset -30d) > 1`:               "offset -30d",
		`up @ 0 == 0`:                                          "@",
		`up @ start() == 0`:                                    "@",
		`max_over_time(rate(x[5m])[2h:1m] offset 2w) > 0`:      "offset 2w",
	} {
		got := Reach(expr, week, week, 11000)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("Reach(%s) = %q, want %q", expr, got, want)
		}
	}
}
