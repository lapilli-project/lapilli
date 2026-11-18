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

// Reach holds what a rule reads, its range and its offset with those of
// every subquery around it, to 8 days back in all, a subquery to 11,000
// points and a step of its own, a duration as written and no @: what
// alerts write passes, a week-over-week comparison among them.
func TestReach(t *testing.T) {
	most := 8 * 24 * time.Hour
	for expr, want := range map[string]string{
		`rate(errors_total[5m]) > 1`:    "",
		`max_over_time(up[1h:1m]) == 0`: "",
		`rate(http_requests_total[5m] offset 1w) > 2 * rate(http_requests_total[5m])`: "",
		`rate(x[1h] offset 1w) > 0`:                            "",
		`predict_linear(disk_free[7d], 86400) < 0`:             "",
		`up{offset="x"} == 0`:                                  "",
		`up offset 1d == 0`:                                    "",
		`label_replace(up, "dst", "$1", "offset", "(.*)") > 0`: "",
		`sum by (job) (rate(x[5m])) / sum by (job) (rate(y{a="b"}[5m] offset 1d)) > 0.1`: "",
		`max_over_time(rate(x[5m])[1h:1m]) > 0`:                                          "",
		`max_over_time(x[1h:1m]) > 0`:                                                    "",
		`max_over_time(up[30d:1s]) > 0`:                                                  "points",
		`max_over_time(up[30d:1h]) > 0`:                                                  "30d back",
		`max_over_time(up[1d:1s]) > 0`:                                                   "points",
		`rate(up[7d] offset 7d) > 0`:                                                     "14d",
		`max_over_time(max_over_time(rate(x[5m] offset 3d)[3d:1m] offset 3d)[3d:1m] offset 3d) > 0`: "past",
		`max_over_time(rate(x[5m])[7d:]) > 0`:                                                       "no step",
		`max_over_time(x[7d:]) > 0`:                                                                 "no step",
		`rate(errors_total[5m] offset 5y) > 1`:                                                      "past",
		`rate(errors_total[5m] offset -30d) > 1`:                                                    "past",
		`up offset (700d) > 0`:                                                                      "other than as a duration",
		`up offset -(700d) > 0`:                                                                     "other than as a duration",
		`rate(up[1d + 1d]) > 0`:                                                                     "other than as a duration",
		`up @ 0 == 0`:                                                                               "@",
		`max_over_time(rate(x[5m] offset 7d)[2d:1m]) > 0`:                                           "past 8d",
		`up @ start() == 0`:                                                                         "@",
		`max_over_time(rate(x[5m])[2h:1m] offset 9d) > 0`:                                           "past",
	} {
		got := Reach(expr, most, 11000)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("Reach(%s) = %q, want %q", expr, got, want)
		}
	}
}
