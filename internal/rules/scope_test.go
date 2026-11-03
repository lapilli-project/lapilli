package rules

import "testing"

func TestScope(t *testing.T) {
	ns := map[string]string{"namespace": "shop"}
	for in, want := range map[string]string{
		`errors > 5`: `errors{namespace="shop"} > 5`,
		`sum by (job) (rate(http_requests_total{code=~"5.."}[5m])) / sum by (job) (rate(http_requests_total[5m])) > 0.01`: `sum by (job) (rate(http_requests_total{code=~"5..",namespace="shop"}[5m])) / sum by (job) (rate(http_requests_total{namespace="shop"}[5m])) > 0.01`,
		`up{job="api",} == 0`:                       `up{job="api",namespace="shop"} == 0`,
		`{__name__=~"job:.*"} > 1`:                  `{__name__=~"job:.*",namespace="shop"} > 1`,
		`{"my.metric"} > 1`:                         `{"my.metric",namespace="shop"} > 1`,
		`a * on (instance) group_left (role) b`:     `a{namespace="shop"} * on (instance) group_left (role) b{namespace="shop"}`,
		`a * on(instance) group_left b`:             `a{namespace="shop"} * on(instance) group_left b{namespace="shop"}`,
		`avg_over_time((x)[10m:15s]) > 1e+3`:        `avg_over_time((x{namespace="shop"})[10m:15s]) > 1e+3`,
		`x offset 5m and y @ start()`:               `x{namespace="shop"} offset 5m and y{namespace="shop"} @ start()`,
		`label_replace(up, "a", "$1", "b", "(.*)")`: `label_replace(up{namespace="shop"}, "a", "$1", "b", "(.*)")`,
		`topk(5, x) unless absent(y)`:               `topk(5, x{namespace="shop"}) unless absent(y{namespace="shop"})`,
		`count_values without (a) ("v", x)`:         `count_values without (a) ("v", x{namespace="shop"})`,
		`SUM BY (a) (x) > bool 0x1e`:                `SUM BY (a) (x{namespace="shop"}) > bool 0x1e`,
		"x # a comment (y)\n> 1":                    "x{namespace=\"shop\"} # a comment (y)\n> 1",
		`rate(x[$__rate_interval])`:                 `rate(x{namespace="shop"}[$__rate_interval])`,
		`job:errors:rate5m > 0`:                     `job:errors:rate5m{namespace="shop"} > 0`,
		`vector(1) + time() > inf`:                  `vector(1) + time() > inf`,
	} {
		got, err := Scope(in, ns)
		if err != nil || got != want {
			t.Errorf("Scope(%s)\n got %s, %v\nwant %s", in, got, err, want)
		}
	}
	for _, bad := range []string{`x{a="b"`, `sum(x`, `x[5m`, `x{a="b}`} {
		if _, err := Scope(bad, ns); err == nil {
			t.Errorf("Scope(%s) should fail", bad)
		}
	}
}
