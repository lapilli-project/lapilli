package rules

import (
	"strings"
	"testing"
	"time"
)

// portChecks are the backtest port's checks of a rule's PromQL, in its
// order (serve's checkPromQL): "" for one taken.
func portChecks(expr string) string {
	if strings.Contains(expr, "\r") {
		return "\\r"
	}
	if what := Unalike(expr); what != "" {
		return what
	}
	if _, err := Selectors(expr); err != nil {
		return err.Error()
	}
	if sel, wide := Wide(expr); wide {
		return "wide " + sel
	}
	return Reach(expr, 8*24*time.Hour, 11000)
}

// Each of these reads past the backtest port's bounds as a history
// --prometheus names reads it, and was taken: VictoriaMetrics' MetricsQL
// (v0.87.6) reads a \ in a name as escaping what follows, a backtick
// among them, or in a selector's braces, an offset after any expression,
// WITH templates, keep_metric_names, 1doffset as 1d offset, 1d-1y as one
// duration, "__\name__" as __name__ and __graphite__ as a pattern of
// names; Prometheus with unquoted UTF-8 names (v0.316.0) reads a name
// that isn't ASCII. Each is refused now; Prometheus refuses, or reads
// alike, each but the last.
func TestUnalikeRefused(t *testing.T) {
	for expr, says := range map[string]string{
		"up\\` or {__name__=~\".+\"} or up\\`":     `'\\' outside a string`,
		"up\\` or rate(up[1400d]) or up\\`":        `'\\' outside a string`,
		"up\\` or up @ 0 or up\\`":                 `'\\' outside a string`,
		"{__name__='up' or __name__=~'.+'}":        "or in a selector's braces",
		"{__name__=`up` OR __name__=~`.+`}":        "or in a selector's braces",
		`up{a="b" and __name__=~".+"}`:             "and in a selector's braces",
		"rate(up[5m]) offset 1400d":                "an offset after something other than",
		"(up) offset 1400d":                        "an offset after something other than",
		"sum(up) by (a) offset 1400d":              "an offset after something other than",
		"- (up) offset 1400d":                      "an offset after something other than",
		"up offset 1d offset 1400d":                "an offset after something other than",
		"rate(up[5m]) @ 0":                         "@ after something other than",
		"up offset 1d [1400d]":                     "a range or a subquery after something other than",
		"up[5m][1400d:1h]":                         "a range or a subquery after something other than",
		"温度 offset 1400d":                          "isn't ASCII",
		"rate(温度[1400d])":                          "isn't ASCII",
		`WITH (x = {__name__=~".+"}) count(x)`:     "WITH",
		"with (f(a) = a offset 1400d) f(up)":       "WITH",
		"rate(up[7d]) keep_metric_names [7d:1m]":   "keep_metric_names",
		"rate(up[7d]) keep_metric_names offset 7d": "keep_metric_names",
		"1doffset 1400d":                           `"1doffset"`,
		"up offset-9doffset 1400d":                 `"9doffset"`,
		"up offset 7d7d":                           `"7d7d"`,
		"up[1400i]":                                `"1400i"`,
		"absent(up offset 1d-1y)":                  "a duration with - right after it",
		"foo.bar offset 1400d":                     "a name with a . in it",
		"x{\"__\\name__\"=~\".+\"}":                "a label's name in quotes",
		`up{__graphite__="*"}`:                     "__graphite__",
		"label_replace(up, \"a\", `x\\`, \"b\", \"c\") or {__name__=~\".+\"} or label_replace(up, \"a\", `y`, \"b\", \"c\")": "in backticks",
		"up $x":   `'$' outside a string`,
		"up; up":  `';' outside a string`,
		"up | up": `'|' outside a string`,
		"up\f>0":  `'\f' outside a string`,
	} {
		if got := portChecks(expr); !strings.Contains(got, says) {
			t.Errorf("%q: %q, want %q", expr, got, says)
		}
	}
	// What the port's checks refused before: wide, past the reach, or @.
	for _, expr := range []string{`(up, {__name__=~".+"})`, `up default {__name__=~".+"}`, `{"__graphite__"="*"}`,
		"max_over_time(rate(up[5m])[1400d])", "up @ end()", "up offset 1d @ 0"} {
		if got := portChecks(expr); got == "" {
			t.Errorf("%q taken", expr)
		}
	}
}

// What every history reads alike is taken: rules as alerting rules are
// written (kube-prometheus-stack's mixins, node-exporter's, Prometheus's
// own), each checked with Prometheus v0.316.0 and MetricsQL v0.87.6 to
// read alike and within the bounds; comments, quoted names, label names
// in by and on that are words of PromQL's; a rendered chart's rules.
func TestUnalikeTakesRules(t *testing.T) {
	for _, expr := range []string{
		`sum by (namespace, pod, container) (rate(container_cpu_usage_seconds_total{container!=""}[5m])) > 0.9`,
		`histogram_quantile(0.99, sum by (le, job) (rate(http_request_duration_seconds_bucket{job="api"}[5m]))) > 1`,
		`(node_filesystem_avail_bytes{job="node-exporter",fstype!="",mountpoint!=""} / node_filesystem_size_bytes{job="node-exporter",fstype!="",mountpoint!=""} * 100 < 15 and predict_linear(node_filesystem_avail_bytes{job="node-exporter",fstype!="",mountpoint!=""}[6h], 4*60*60) < 0 and node_filesystem_readonly{job="node-exporter",fstype!="",mountpoint!=""} == 0)`,
		`max_over_time(kube_pod_container_status_waiting_reason{reason="CrashLoopBackOff", job="kube-state-metrics", namespace=~".*"}[5m]) >= 1`,
		`absent(up{job="prometheus"} == 1)`,
		`sum(rate(apiserver_request_total{code=~"5.."}[5m])) / sum(rate(apiserver_request_total[5m])) > 0.05`,
		`1 - avg without (cpu) (rate(node_cpu_seconds_total{mode="idle"}[5m])) > 0.9`,
		`label_replace(up{job="node"}, "host", "$1", "instance", "(.*):.*") == 0`,
		`clamp_min(free_bytes, 0) < 1e+9`,
		`node_load1 * on (instance) group_left (nodename) node_uname_info > 4`,
		`sum by (le) (rate(x_bucket{le!~"\\+Inf"}[5m]))`,
		`x_total % 2 == 0 or x_total ^ 2 > 10`,
		`avg_over_time(temperature_celsius[1h:5m]) > 80`,
		`{"http.requests_total", code="500"} > 0`,
		`rate(http_requests_total[5m] offset 1w) * 1.5 < rate(http_requests_total[5m])`,
		`max_over_time(rate(http_requests_total[5m])[1h:1m] offset 1d) > 100`,
		`-1 * rate(x_total[5m]) < -0.5`,
		`probe_ssl_earliest_cert_expiry - time() < 86400 * 14`,
		`sum by (cluster, namespace) (kube_resourcequota{job="kube-state-metrics", type="used"}) / ignoring(instance, job, type) (sum by (cluster, namespace) (kube_resourcequota{job="kube-state-metrics", type="hard"}) > 0) > 0.9`,
		`sum without (instance) (rate(errors_total{env='prod'}[5m])) > 0.1`,
		"up{job=`api`} == 0",
		"x > 0x10 or x > 1_000 or x > .5 or x > 5.",
		"x and on(offset) y",
		"sum by (offset) (x{offset=\"1\"})",
		"x > x offset 1w * 1.5",
		"up offset 7d1h",
		"# 5xx ratio, in percent\nsum(rate(x_total{code=~\"5..\"}[5m])) / sum(rate(x_total[5m])) * 100 > 1 # of all",
		`{"温度"} > 30`,
	} {
		if got := portChecks(expr); got != "" {
			t.Errorf("%q: %q", expr, got)
		}
	}
	rs, err := Parse([]byte(kubeStack(1, true)))
	if err != nil || len(rs) == 0 {
		t.Fatal(len(rs), err)
	}
	for _, r := range rs {
		if what := Unalike(r.Expr); what != "" {
			t.Fatalf("%s: %q", r.Name, what)
		}
	}
}

// A string is one string: a quote of its kind inside it, not escaped,
// ends it, as PromQL reads it, and what follows isn't of it.
func TestUnquoteOneString(t *testing.T) {
	for q, want := range map[string]bool{
		`'up' or __name__=~'.+'`: false,
		"`up` or `.+`":           false,
		`"up" or "x"`:            false,
		`'it\'s'`:                true,
		`'a"b'`:                  true,
		"`a\\b`":                 true,
		`'a\\'`:                  true,
		`"a\"b"`:                 true,
	} {
		if _, ok := unquote(q); ok != want {
			t.Errorf("unquote(%s): %t, want %t", q, ok, want)
		}
	}
	// A selector with such a value, a label's name that doesn't unquote,
	// or __graphite__ is wide.
	for _, expr := range []string{"{__name__='up' or __name__=~'.+'}", `up{"__\_name__"=~".+"}`, `up{"__graphite__"="*"}`} {
		if sel, wide := Wide(expr); !wide {
			t.Errorf("Wide(%s): %q", expr, sel)
		}
	}
}
