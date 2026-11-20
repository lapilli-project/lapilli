package rules

import (
	"slices"
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

// Unnamed is info(), which reads target_info beside what it names,
// written as Prometheus or MetricsQL reads a function's name: in any case,
// spaces or a comment before its (; never a label, a string or a comment
// of that name.
func TestUnnamed(t *testing.T) {
	for expr, want := range map[string]bool{
		`info(rate(http_requests_total[5m])) > 1`:    true,
		`sum(info (up, {k8s_cluster_name="a"})) > 0`: true,
		`INFO(up) > 0`:           true,
		"info # (\n (up) > 0":    true,
		`sum by (info) (up) > 0`: false,
		`up{info="a"} > 0`:       false,
		`label_replace(up, "a", "info(", "b", "(.*)") > 0`: false,
		"up > 0 # info(up)": false,
		`build_info > 0`:    false,
		`info_total > 0`:    false,
	} {
		if got := Unnamed(expr) != ""; got != want {
			t.Errorf("Unnamed(%q) = %t, want %t", expr, got, want)
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
		// Comments, read past as Prometheus reads them: what follows one
		// on the next line counts, a quote in one starts no string, and
		// a # in a string starts no comment.
		"rate(errors[5m] #\n offset 30d) > 0":                   "past",
		"rate(errors[5m]) # \"\n + up @ 0 > 0":                  "@",
		"rate(errors[5m]) # \"\n + rate(up[30d]) > 0":           "30d",
		"rate(errors[5m]) # '\n + max_over_time(up[1d:1s]) > 0": "points",
		"rate(errors[5m] # @ and [30d] and offset 1y\n) > 0":    "",
		"# @ 0\nrate(errors[5m]) > 0":                           "",
		`rate(errors{a="#"}[5m] offset 30d) > 0`:                "past",
		"rate(errors{a=\"x\\\"#\"}[5m] offset 30d) > 0":         "past",
		"rate(errors{a=`#\\`}[5m] offset 30d) # `\n > 0":        "past",
	} {
		got := Reach(expr, most, 11000)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("Reach(%s) = %q, want %q", expr, got, want)
		}
	}
}

// A comment ends where Prometheus's lexer ends one, at a \r as at a \n,
// and is read anywhere outside a string, in a selector's braces and a
// range's brackets as well: what follows one on its line's end is a
// selector, wide or past the reach, and is found, and what a } or a ] or
// a quote in one would have hidden is read.
func TestCommentsEndAsPrometheusEndsThem(t *testing.T) {
	most := 8 * 24 * time.Hour
	for _, c := range []struct {
		expr, sel, wide, reach string
	}{
		{"vector(1) # \r or count({__name__=~\".+\"}) > 0", `{__name__=~".+"}`, `{__name__=~".+"}`, ""},
		{"up{ # x=\"y\" } \"\n} or {__name__=~'.+'} # \"", `{__name__=~'.+'}`, `{__name__=~'.+'}`, ""},
		{"rate(x[5m # ] \"\n]) or {__name__=~'.+'} # \")", `{__name__=~'.+'}`, `{__name__=~'.+'}`, ""},
		{"vector(1) # \r or max_over_time(errors[400d] @ 0 offset 1000d) > 0", `errors @ 0 offset 1000d`, "", "@"},
		{"vector(1) # \r or max_over_time(errors[400d] offset 1000d) > 0", `errors offset 1000d`, "", "past 8d"},
		{"vector(1) # \r or count_over_time({job=~\".+\"}[8d:1s]) > 0", `{job=~".+"}`, `{job=~".+"}`, "points"},
	} {
		sels, err := Selectors(c.expr)
		if err != nil || !slices.Contains(sels, c.sel) {
			t.Errorf("Selectors(%q) = %q, %v: %q not among them", c.expr, sels, err, c.sel)
		}
		if got, wide := Wide(c.expr); c.wide != "" && (!wide || got != c.wide) {
			t.Errorf("Wide(%q) = %q %t, want %q", c.expr, got, wide, c.wide)
		}
		if got := Reach(c.expr, most, 11000); c.reach != "" && !strings.Contains(got, c.reach) {
			t.Errorf("Reach(%q) = %q, want %q", c.expr, got, c.reach)
		}
	}
	// The range in brackets a comment is in: read to its ], past the
	// comment's.
	if got, err := Ranges("rate(x[5m # ] \"\n]) > 0"); err != nil || !slices.Equal(got, []string{"5m"}) {
		t.Errorf("Ranges: %q %v", got, err)
	}
	// A rule's own comment hides nothing, and reads nothing.
	honest := "# 5xx ratio\nsum(rate(x_total[5m])) > 1"
	if sels, err := Selectors(honest); err != nil || !slices.Equal(sels, []string{"x_total"}) {
		t.Errorf("Selectors(%q) = %q, %v", honest, sels, err)
	}
	if sel, wide := Wide(honest); wide {
		t.Errorf("Wide(%q) = %q", honest, sel)
	}
	if got := Reach(honest, most, 11000); got != "" {
		t.Errorf("Reach(%q) = %q", honest, got)
	}
	// Scope adds its matchers to what follows a \r, and to a selector a }
	// in a comment would have ended, its comments kept as written.
	ns := map[string]string{"namespace": "shop"}
	for in, want := range map[string]string{
		"vector(1) # \r or up > 0":                 "vector(1) # \r or up{namespace=\"shop\"} > 0",
		"up{ # x=\"y\" } \"\n} or {a=\"b\"} # \"":  "up{ # x=\"y\" } \"\nnamespace=\"shop\"} or {a=\"b\",namespace=\"shop\"} # \"",
		"up{a=\"b\" # c } \n} # d\n> 1":            "up{a=\"b\" # c } \n,namespace=\"shop\"} # d\n> 1",
		"absent(x # )\r) or rate(y[5m # ]\n]) > 0": "absent(x{namespace=\"shop\"} # )\r) or rate(y{namespace=\"shop\"}[5m # ]\n]) > 0",
	} {
		if got, err := Scope(in, ns); err != nil || got != want {
			t.Errorf("Scope(%q)\n got %q, %v\nwant %q", in, got, err, want)
		}
	}
	// absent() read as Prometheus reads it: what it holds, past a
	// comment's \r, has no stand-in.
	if got, err := StandIns("absent(x{code=~\"5..\"} # )\r) or x > 0"); err != nil || len(got) != 0 {
		t.Errorf("StandIns in absent: %v %v", got, err)
	}
}
