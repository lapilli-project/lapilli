package content

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
)

// good is a rule with nothing to fix.
func good(name string) rules.Rule {
	return rules.Rule{Name: name, Expr: fmt.Sprintf("up{rule=%q} == 0", name), Labels: map[string]string{"severity": "page"}, For: 5 * time.Minute,
		Says: rules.Says{Summary: "{{ $labels.instance }} is down", Description: "Down for 5m.",
			Runbook: "https://runbooks.example/down", Template: rules.TemplatePrometheus}}
}

// checks are the checks found for the rule at i, with their details.
func checks(fs [][]Finding, i int) []string {
	var out []string
	for _, f := range fs[i] {
		if f.Class != Class(f.Check) {
			panic("a finding of the wrong class")
		}
		s := f.Check
		if f.Detail != "" {
			s += ":" + f.Detail
		}
		out = append(out, s)
	}
	return out
}

func one(t *testing.T, r rules.Rule, o Options, want ...string) {
	t.Helper()
	rs := []rules.Rule{r}
	for i := range 3 { // others, so a severity isn't rare for want of rules
		rs = append(rs, good(fmt.Sprint("other", i)))
	}
	got := checks(Check(rs, o), 0)
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got %q\nwant %q", r.Name, got, want)
	}
}

func TestNothingToFix(t *testing.T) {
	one(t, good("Down"), Options{Scrape: time.Minute})
}

func TestNoText(t *testing.T) {
	r := good("NoText")
	r.Says.Summary, r.Says.Description = "", ""
	one(t, r, Options{}, NoText)
	r.Says.Summary = "down"
	one(t, r, Options{})
	// A cloud's alarm is summed up by its name: it is the description
	// that counts.
	c := rules.Rule{Name: "cpu", Labels: map[string]string{}, Cloud: "cloudwatch", Unsupported: "x",
		Says: rules.Says{Summary: "cpu", Runbook: "https://runbooks.example/cpu", NoSeverity: true}}
	one(t, c, Options{}, NoText)
	c.Says.Description = "CPU high."
	one(t, c, Options{})
}

func TestRunbook(t *testing.T) {
	for link, want := range map[string][]string{
		"":                                         {NoRunbook},
		"runbooks/down.md":                         nil, // a path in the rules' repository
		"docs/runbooks/disk.md#full":               nil,
		"/docs/runbooks/#disk-full":                nil,
		"README.md":                                nil,
		"check-disk":                               {RunbookNotURL + ":check-disk"}, // a word alone
		"ask in #oncall":                           {RunbookNotURL + ":ask in #oncall"},
		"//evil.example/x":                         {RunbookNotURL + "://evil.example/x"},
		"javascript:alert(1)":                      {RunbookNotURL + ":javascript:alert(1)"},
		"ftp://runbooks.example/down":              {RunbookNotURL + ":ftp://runbooks.example/down"},
		"https://":                                 {RunbookNotURL + ":https://"},
		"http://runbooks.example/down":             nil,
		"https://wiki.example/{{ $labels.job }}":   nil,
		"{{ $externalURL }}/runbooks/down":         nil, // a template decides: unknown
		"https://wiki.example/x?a={{ $value }}&b=": nil,
	} {
		r := good("Runbook")
		r.Says.Runbook = link
		one(t, r, Options{}, want...)
	}
	// Read from the annotations named, the first the rule has; a cloud's
	// alarm, which has none, by the link its source gives.
	r := good("Annotated")
	r.Says.Runbook, r.Says.Annotations = "", map[string]string{"playbook": "docs/playbooks/down.md#restart", "dashboard": "https://grafana.example/d/x"}
	one(t, r, Options{})                                             // playbook is among the defaults
	one(t, r, Options{Runbooks: []string{"runbook_url"}}, NoRunbook) // not when only runbook_url is named
	r.Says.Annotations["wiki"] = "not a link"
	one(t, r, Options{Runbooks: []string{"wiki", "playbook"}}, RunbookNotURL+":not a link")
	if got := Runbook(r.Says, []string{"dashboard"}); got != "https://grafana.example/d/x" {
		t.Errorf("Runbook by dashboard: %q", got)
	}
	cloud := rules.Says{Runbook: "https://runbooks.example/cpu"}
	if got := Runbook(cloud, []string{"playbook"}); got != cloud.Runbook {
		t.Errorf("a cloud's alarm's runbook: %q", got)
	}
	g := rules.Rule{Name: "Latency", Labels: map[string]string{"severity": "warning"}, Cloud: "google", Unsupported: "x",
		Says: rules.Says{Summary: "Latency", Runbook: "${policy.user_label.runbook}", Template: rules.TemplateGoogle}}
	one(t, g, Options{})
}

func TestTemplateError(t *testing.T) {
	r := good("Broken")
	r.Says.Summary = "{{ $labels.instance is down"
	one(t, r, Options{}, TemplateError+`:line 1: unclosed action`)
	r.Says.Summary = "{{ $foo }}"
	one(t, r, Options{}, TemplateError+`:line 1: undefined variable "$foo"`)
	// A function this lapilli doesn't know, a newer Prometheus may have.
	r.Says.Summary = "{{ $value | someNewFunc }} {{ humanize $value }}"
	one(t, r, Options{})
	if names, err := Template(rules.TemplatePrometheus, "{{ é }} {{ $labels.pod }}"); err != nil || !slices.Equal(names, []string{"pod"}) {
		t.Errorf("a function named in any letters: %q %v", names, err)
	}
	// Grafana's $values, which Prometheus doesn't have.
	r.Says.Summary = "{{ $values.A.Value }}"
	one(t, r, Options{}, TemplateError+`:line 1: undefined variable "$values"`)
	r.Says.Template = rules.TemplateGrafana
	one(t, r, Options{})
	// Plain text isn't parsed.
	r.Says.Template, r.Says.Summary = "", "{{ broken"
	one(t, r, Options{})
	g := rules.Rule{Name: "G", Labels: map[string]string{"severity": "warning"}, Cloud: "google", Unsupported: "x",
		Says: rules.Says{Summary: "Latency on ${resource.label.service", Runbook: "https://runbooks.example/l", Template: rules.TemplateGoogle}}
	one(t, g, Options{}, TemplateError+":a ${ with no }")
	g.Says.Summary = "Costs $$5 on ${resource.label.service}"
	one(t, g, Options{})
}

// A template is parsed, never run: nothing in it is called.
func TestTemplateNotRun(t *testing.T) {
	r := good("Query")
	r.Says.Description = `{{ with query "up" }}{{ . | first | value }}{{ end }} {{ template "x" . }}`
	one(t, r, Options{})
}

func TestTemplateLabels(t *testing.T) {
	for text, want := range map[string][]string{
		"{{ $labels.instance }}":                       {"instance"},
		`{{ index $labels "pod" }}`:                    {"pod"},
		"{{ .Labels.job }}":                            {"job"},
		"{{ range .Alerts }}{{ .Labels.x }}{{ end }}":  nil,
		"{{ with $labels }}{{ .y }}{{ end }}":          nil,
		"{{ if $labels.a }}{{ $labels.b }}{{ end }}":   {"a", "b"},
		"{{ $labels.a | toUpper }} {{ $labels.a }} ok": {"a", "a"},
		"no template": nil,
	} {
		got, err := Template(rules.TemplatePrometheus, text)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %q %v", text, got, err)
		}
	}
}

func TestLabelNeverSeen(t *testing.T) {
	r := good("Down")
	r.Says.Summary = "{{ $labels.instance }} of {{ $labels.service }} ({{ $labels.severity }})"
	seen := func(fired bool) Options {
		return Options{Seen: func(string) (map[string]bool, bool) { return map[string]bool{"instance": true, "job": true}, fired }}
	}
	// service was never on a firing; severity is the rule's own label.
	one(t, r, seen(true), LabelNeverSeen+":service")
	// A rule that never fired has nothing measured.
	one(t, r, seen(false))
}

func TestLabelNotGiven(t *testing.T) {
	r := good("Errors")
	r.Expr = `sum by (service) (rate(http_errors_total{code=~"5.."}[5m])) > 1`
	r.Says.Summary = "{{ $labels.service }} on {{ $labels.instance }}"
	one(t, r, Options{}, LabelNotGiven+":instance")
	// Fired, it is measured instead.
	one(t, r, Options{Seen: func(string) (map[string]bool, bool) { return map[string]bool{"service": true, "instance": true}, true }})
	for _, expr := range []string{
		`sum without (pod) (x) > 1`, // without: what is left isn't known
		`topk by (service) (3, x)`,  // keeps every label
		`sum by (service) (x) > on() group_left(instance) y`,
		`sum by (service) (x) / sum by (service) (y) > 0.5`,
		`label_replace(sum by (service) (x), "instance", "$1", "service", "(.*)")`,
	} {
		r.Expr = expr
		one(t, r, Options{})
	}
	r.Expr = `(sum(x)) == 0`
	r.Says.Summary = "{{ $labels.alertname }} {{ $labels.job }}"
	one(t, r, Options{}, LabelNotGiven+":job")
	r.Expr = `sum(x) by (job) >= bool -1`
	one(t, r, Options{})
}

func TestSeverity(t *testing.T) {
	r := good("NoSev")
	r.Labels = map[string]string{}
	one(t, r, Options{}, NoSeverity)
	// A source with no severity at all.
	r.Says.NoSeverity = true
	one(t, r, Options{})
	r = good("Sev")
	r.Labels["severity"] = "pager"
	one(t, r, Options{Severities: []string{"page", "ticket"}}, UnknownSeverity+":pager")
	r.Labels["severity"] = "Page" // routes match a label exactly
	one(t, r, Options{Severities: []string{"page", "ticket"}}, UnknownSeverity+":Page")
	// A cloud's own field, in any case.
	o := rules.Rule{Name: "disk", Labels: map[string]string{"severity": "critical"}, Cloud: "oci", Unsupported: "x",
		Says: rules.Says{Summary: "disk", Description: "Disk.", Runbook: "https://runbooks.example/disk", Severity: "critical"}}
	one(t, o, Options{Severities: []string{"CRITICAL"}})
}

func TestRareSeverity(t *testing.T) {
	var rs []rules.Rule
	for i := range rareAmong {
		rs = append(rs, good(fmt.Sprint("r", i)))
	}
	rs[0].Labels["severity"] = "pgae"
	fs := Check(rs, Options{})
	if got := checks(fs, 0); !slices.Equal(got, []string{RareSeverity + ":pgae"}) {
		t.Errorf("a typo: %q", got)
	}
	// Given, the team's list decides instead.
	if got := checks(Check(rs, Options{Severities: []string{"page", "pgae"}}), 0); got != nil {
		t.Errorf("a severity the team lists: %q", got)
	}
	// In a small set, every value is rare: no guess.
	if got := checks(Check(rs[:rareAmong-1], Options{}), 0); got != nil {
		t.Errorf("a small set: %q", got)
	}
	// A cloud's own severities aren't counted with labels.
	c := rules.Rule{Name: "x", Labels: map[string]string{"severity": "Sev2"}, Cloud: "azure", Unsupported: "x",
		Says: rules.Says{Summary: "x", Description: "x", Runbook: "https://runbooks.example/x", Severity: "Sev2"}}
	rs[0].Labels["severity"] = "page"
	if got := checks(Check(append(rs, c), Options{}), rareAmong); got != nil {
		t.Errorf("Azure among labels: %q", got)
	}
}

func TestNoisyNoFor(t *testing.T) {
	r := good("Noisy")
	noisy := Options{Noisy: func(string) bool { return true }}
	one(t, r, noisy)
	r.For = 0
	one(t, r, noisy, NoisyNoFor)
	one(t, r, Options{Noisy: func(string) bool { return false }})
}

func TestDuplicate(t *testing.T) {
	a, b, c, a2 := good("A"), good("B"), good("C"), good("A")
	a.Expr, a2.Expr = "up == 0", "up == 0"
	b.Expr = "up   ==\n  0 # down"
	c.Expr = "up == 1"
	fs := Check([]rules.Rule{a, b, c, a2}, Options{})
	if got := checks(fs, 0); !slices.Equal(got, []string{Duplicate + ":B"}) {
		t.Errorf("A: %q", got)
	}
	if got := checks(fs, 1); !slices.Equal(got, []string{Duplicate + ":A"}) {
		t.Errorf("B: %q", got)
	}
	if got := checks(fs, 2); got != nil {
		t.Errorf("C: %q", got)
	}
	// Another for, or other labels, is another condition; spaces in a
	// string aren't spaces aside.
	b.For, c.Expr = time.Minute, `up{job="a b"} == 0`
	d := good("D")
	d.Expr = `up{job="ab"} == 0`
	b.Expr = "up == 0"
	if fs := Check([]rules.Rule{a, b, c, d}, Options{}); fs[0] != nil || fs[2] != nil {
		t.Errorf("not the same: %q %q", checks(fs, 0), checks(fs, 2))
	}
}

func TestShortRange(t *testing.T) {
	r := good("Rate")
	r.Expr = `rate(x[1m]) > 0 and increase(y[ 5m ]) > 0 and rate(z[1m:15s]) > 0 and irate(w[90s]) > 1`
	one(t, r, Options{Scrape: time.Minute}, ShortRange+":rate[1m] < 2 × 1m", ShortRange+":irate[90s] < 2 × 1m")
	// Not known, not checked.
	one(t, r, Options{})
	r.Expr = `rate(x{path=~"/a[0-9]"}[2m]) > 0`
	one(t, r, Options{Scrape: time.Minute})
	// A Loki ruler's range is of log lines, not scrapes, and its parsers
	// add labels: neither guess is made, as each would be of PromQL.
	r.Expr, r.Logs = `sum(count_over_time({app="shop"} | logfmt | rate > 1 [1m])) > 0 and rate(x[1m]) > 0`, rules.LokiRuler
	one(t, r, Options{Scrape: time.Minute})
	r.Logs = ""
	one(t, r, Options{Scrape: time.Minute}, ShortRange+":rate[1m] < 2 × 1m")
	r.Expr = `sum(count_over_time({app="shop"} | logfmt [1m])) by (app) > 0`
	one(t, r, Options{}, LabelNotGiven+":instance")
	r.Logs = rules.LokiRuler
	one(t, r, Options{})
}

func TestAbsentNoFor(t *testing.T) {
	r := good("Gone")
	r.Expr, r.For = `absent(up{job="api"})`, 0
	one(t, r, Options{}, AbsentNoFor)
	r.For = 2 * time.Minute
	one(t, r, Options{})
	r.Expr, r.For = `absent(up) or vector(1)`, 0
	one(t, r, Options{})
}

// A rule turned off fires nothing: nothing it says costs anyone.
func TestOffNotChecked(t *testing.T) {
	r := good("Off")
	r.Says = rules.Says{}
	r.Unsupported = rules.Disabled
	one(t, r, Options{})
	r.Unsupported = rules.Paused
	one(t, r, Options{})
}

// A detail comes from the rules: bounded, with no control characters.
func TestDetailBounded(t *testing.T) {
	r := good("Long")
	r.Says.Runbook = "x\x1b[31m\u202e" + strings.Repeat("é", 300)
	fs := Check([]rules.Rule{r}, Options{})
	if len(fs[0]) != 1 || len(fs[0][0].Detail) > maxDetail+len("…") || strings.ContainsAny(fs[0][0].Detail, "\x1b\u202e") {
		t.Errorf("%q", fs[0])
	}
}

// Facts come first, then what is measured, then heuristics.
func TestOrder(t *testing.T) {
	r := good("All")
	r.Expr, r.For, r.Labels = "absent(x)", 0, map[string]string{}
	r.Says.Runbook = ""
	got := checks(Check([]rules.Rule{r}, Options{Noisy: func(string) bool { return true }}), 0)
	if want := []string{NoRunbook, NoSeverity, NoisyNoFor, AbsentNoFor}; !slices.Equal(got, want) {
		t.Errorf("%q", got)
	}
}

// What each source says is checked as that source writes it.
func TestSources(t *testing.T) {
	rs, err := rules.Parse([]byte(`{"MetricAlarms": [{"AlarmName": "cpu-high", "AlarmDescription": "CPU over 90%. See https://runbooks.example/cpu."},
		{"AlarmName": "bare"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	fs := Check(rs, Options{})
	if got := checks(fs, 0); got != nil {
		t.Errorf("CloudWatch: %q", got)
	}
	if got := checks(fs, 1); !slices.Equal(got, []string{NoText, NoRunbook}) {
		t.Errorf("CloudWatch, bare: %q", got)
	}
	rs, err = rules.Parse([]byte(`[{"type": "Microsoft.Insights/activityLogAlerts", "name": "deleted", "description": "Deleted. https://runbooks.example/d"},
		{"type": "Microsoft.Insights/metricAlerts", "name": "cpu", "description": "CPU.", "severity": 2}]`))
	if err != nil {
		t.Fatal(err)
	}
	fs = Check(rs, Options{})
	if got := checks(fs, 0); got != nil {
		t.Errorf("Azure activity log: %q", got)
	}
	if got := checks(fs, 1); !slices.Equal(got, []string{NoRunbook}) {
		t.Errorf("Azure metric alert: %q", got)
	}
	rs, err = rules.Parse([]byte(`{"data": [{"id": "ocid1.alarm.oc1..a", "display-name": "disk", "body": "Disk on ${resourceId}."}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := checks(Check(rs, Options{}), 0); !slices.Equal(got, []string{NoRunbook, NoSeverity}) {
		t.Errorf("OCI: %q", got)
	}
	rs, err = rules.Parse([]byte(`apiVersion: 1
groups:
  - name: g
    folder: f
    interval: 1m
    rules:
      - uid: u1
        title: Down
        condition: C
        data:
          - refId: A
            datasourceUid: prom
            model: {expr: up, instant: true}
          - refId: C
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: lt, params: [1]}}]}
        labels: {severity: page}
        annotations: {summary: "{{ $values.A.Value }} on {{ $labels.instance", runbook_url: "https://runbooks.example/down"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := checks(Check(rs, Options{}), 0); len(got) != 1 || !strings.HasPrefix(got[0], TemplateError+":") {
		t.Errorf("Grafana: %q", got)
	}
}

func TestParseDuration(t *testing.T) {
	for s, want := range map[string]time.Duration{"5m": 5 * time.Minute, "1h30m": 90 * time.Minute, "2d": 48 * time.Hour, "500ms": 500 * time.Millisecond} {
		if d, ok := ParseDuration(s); !ok || d != want {
			t.Errorf("%s: %v", s, d)
		}
	}
	for _, s := range []string{"", "5", "m", "$__rate_interval", "1m5h"} {
		if _, ok := ParseDuration(s); ok {
			t.Errorf("%q read", s)
		}
	}
}

func TestScrapeInterval(t *testing.T) {
	for config, want := range map[string]time.Duration{
		"global:\n  scrape_interval: 30s\n  evaluation_interval: 1m\n": 30 * time.Second,
		"global:\n  evaluation_interval: 1m\n":                         time.Minute, // Prometheus's default
		"scrape_configs: []\n":                                         time.Minute,
	} {
		if d, ok := ScrapeInterval(config); !ok || d != want {
			t.Errorf("%q: %v %v", config, d, ok)
		}
	}
	for _, config := range []string{"global: [", "global:\n  scrape_interval: soon\n", "global:\n  scrape_interval: 0s\n"} {
		if _, ok := ScrapeInterval(config); ok {
			t.Errorf("%q read", config)
		}
	}
}

// Every check says what it found, and every heuristic why it is a guess.
func TestMessages(t *testing.T) {
	for _, c := range Checks {
		if Message(c) == "" {
			t.Errorf("%s says nothing", c)
		}
		if (Why(c) != "") != (Class(c) == Heuristic) {
			t.Errorf("%s: why %q, a %s", c, Why(c), Class(c))
		}
	}
}

// A runbook is shown as a link only when it is one to follow as it is.
func TestLink(t *testing.T) {
	for link, want := range map[string]bool{
		"https://runbooks.example/down":          true,
		"http://runbooks.example/down":           true,
		"javascript:alert(1)":                    false,
		"//evil.example/x":                       false,
		"https://wiki.example/{{ $labels.job }}": false,
		"https://x.example/${policy.name}":       false,
		"runbooks/down.md":                       false, // a path, shown as one
		"docs/runbooks/disk.md#full":             false,
		"":                                       false,
	} {
		if got := Link(rules.Says{Runbook: link}); (got != "") != want {
			t.Errorf("%q: %q", link, got)
		}
	}
}

// What comes from the rules costs the checks no more than a bound: a long
// expression isn't read, a condition many rules share names a few of them,
// and a template's oddities say nothing of it rather than break the check.
func TestBounded(t *testing.T) {
	r := good("Long")
	r.Expr, r.For = "absent("+strings.Repeat("(", maxExpr)+"x"+strings.Repeat(")", maxExpr)+")", 0
	one(t, r, Options{Scrape: time.Minute})
	var rs []rules.Rule
	for i := range 50 {
		x := good(fmt.Sprint("Same", i))
		x.Expr = "up == 0"
		rs = append(rs, x)
	}
	if fs := Check(rs, Options{}); len(fs[0]) != maxDuplicates || len(fs[49]) != maxDuplicates {
		t.Errorf("%d, %d duplicates told of", len(fs[0]), len(fs[49]))
	}
	for _, text := range []string{"{{ " + strings.Repeat("(", 20000) + " }}", "{{ define \"x\" }}{{ .Labels.a }}{{ end }}{{ template \"x\" }}"} {
		if _, err := Template(rules.TemplatePrometheus, text); err != nil && !strings.HasPrefix(err.Error(), "line ") {
			t.Errorf("%.40q: %v", text, err)
		}
	}
	if names, err := Template(rules.TemplatePrometheus, "{{ define \"x\" }}{{ .Labels.a }}{{ end }}"); err != nil || names != nil {
		t.Errorf("in a template defined in the text, . isn't the alert: %q %v", names, err)
	}
}

// A text to be written takes only the functions its language has: a
// function Prometheus hasn't, or misspelt, makes it refuse the whole file,
// though a rule read calling one is taken as a newer Prometheus's.
func TestTemplateStrict(t *testing.T) {
	for _, c := range []struct {
		lang, text string
		ok         bool
	}{
		{rules.TemplatePrometheus, `{{ nosuchfunc $labels.instance }}`, false},
		{rules.TemplatePrometheus, `{{ humanise $value }}`, false},
		{rules.TemplatePrometheus, `{{ $value | humanise }}`, false},
		{rules.TemplatePrometheus, `{{ removeLabels $labels "pod" }}`, false}, // Grafana's
		{rules.TemplatePrometheus, `{{ join "," $labels }}`, false},           // Alertmanager's
		{rules.TemplatePrometheus, `{{ now }}`, false},                        // Prometheus 3.6's
		{rules.TemplatePrometheus, `{{ $labels.instance | stripPort | title }} {{ humanizePercentage $value }} {{ printf "%.1f" $value }}`, true},
		{rules.TemplatePrometheus, `{{ define "x" }}{{ toTime 1700000000 }}{{ end }}{{ template "x" . }} {{ $externalURL }}`, true},
		{rules.TemplateGrafana, `{{ $values.A.Value | humanize }} on {{ removeLabels $labels "pod" }}`, true},
		{rules.TemplateGrafana, `{{ humanise $values.A.Value }}`, false},
		{"", `{{ nosuchfunc }}`, true}, // plain text: nothing is a function
		{rules.TemplateGoogle, `${resource.label.service} {{ nosuchfunc }}`, true},
	} {
		err := TemplateStrict(c.lang, c.text)
		if (err == nil) != c.ok {
			t.Errorf("%s %q: %v, want taken %v", c.lang, c.text, err, c.ok)
		}
		if !c.ok && strings.Contains(c.text, "humanise") && (err == nil || !strings.Contains(err.Error(), `function "humanise" not defined`)) {
			t.Errorf("%q: %v, want the function named", c.text, err)
		}
		if _, err := Template(c.lang, c.text); err != nil {
			t.Errorf("read as a rule's: %s %q: %v", c.lang, c.text, err)
		}
	}
}
