package content

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
)

// An expression that can't be read is checked for what can be, and breaks
// nothing: a range left open, brackets that never close.
func TestMalformedExpr(t *testing.T) {
	for _, expr := range []string{"rate(x[", "[", "x[5m", "rate(x[5m", "sum by (", ")(", "((", "absent(", "rate(", "sum by (a) (x[1m"} {
		Gives(expr)
		Ranges(expr)
		Absent(expr)
		Squeeze(expr)
		r := good("Broken")
		r.Expr, r.Says.Runbook = expr, ""
		one(t, r, Options{Scrape: time.Minute}, NoRunbook)
	}
	if got := Ranges("rate(x[1s"); len(got) != 0 {
		t.Errorf("a rate left open: %v", got)
	}
}

// A check that fails is the checks' fault: that rule has no findings, the
// others are checked, and the failure is told.
func TestCheckFailsAlone(t *testing.T) {
	boom, fine := good("Boom"), good("Fine")
	boom.Says.Runbook, fine.Says.Runbook = "", ""
	var failed []string
	o := Options{
		Seen: func(rule string) (map[string]bool, bool) {
			if rule == "Boom" {
				panic("a fault of the checks'")
			}
			return nil, false
		},
		Failed: func(rule string, err error) { failed = append(failed, rule+": "+err.Error()) },
	}
	fs := Check([]rules.Rule{boom, fine, good("a"), good("b")}, o)
	if fs[0] != nil || !slices.Equal(checks(fs, 1), []string{NoRunbook}) {
		t.Errorf("Boom %q, Fine %q", checks(fs, 0), checks(fs, 1))
	}
	if len(failed) != 1 || !strings.HasPrefix(failed[0], "Boom: ") {
		t.Errorf("told: %q", failed)
	}
}

// The expression checks read an expression in one pass, however deep:
// a hostile expression costs about what a plain one as long does.
func TestHostileLinear(t *testing.T) {
	n := maxExpr/8 - 10
	plain := strings.Repeat("up + ", (maxExpr-10)/5) + "up"
	for name, expr := range map[string]string{
		"nested":      strings.Repeat("(", n*4) + "up" + strings.Repeat(")", n*4),
		"nested rate": strings.Repeat("rate(", (maxExpr-20)/6) + "x[1m]" + strings.Repeat(")", (maxExpr-20)/6),
		"sum by":      "sum by (a) " + strings.Repeat("(", n*4-20) + "x" + strings.Repeat(")", n*4-20) + " > 1",
		"absent":      "absent" + strings.Repeat("(", n*4) + "x" + strings.Repeat(")", n*4),
	} {
		if len(expr) > maxExpr {
			t.Fatalf("%s: %d bytes", name, len(expr))
		}
		cost := func(expr string) time.Duration {
			r := good("Hostile")
			r.Expr, r.For = expr, 0
			best := time.Hour
			for range 3 {
				start := time.Now()
				Check([]rules.Rule{r, good("a"), good("b")}, Options{Scrape: time.Minute})
				best = min(best, time.Since(start))
			}
			return best
		}
		hostile, base := cost(expr), cost(plain)
		if hostile > 20*base+20*time.Millisecond {
			t.Errorf("%s: %s, against %s for a plain expression as long", name, hostile, base)
		}
	}
}

// Every alert has alertname: a template naming it is never faulted, measured
// or guessed.
func TestAlertname(t *testing.T) {
	r := good("Down")
	r.Says.Summary = "{{ $labels.alertname }} on {{ $labels.instance }}"
	// Replayed firings carry the series' labels and the rule's, not
	// alertname.
	seen := func(string) (map[string]bool, bool) { return map[string]bool{"instance": true}, true }
	one(t, r, Options{Seen: seen})
	r.Expr = "sum by (instance) (x) > 1"
	one(t, r, Options{})
}

// A label named only under a guard renders nothing when it is missing: not
// a fault. Named outside one too, it is.
func TestGuardedLabel(t *testing.T) {
	seen := Options{Seen: func(string) (map[string]bool, bool) { return map[string]bool{"pod": true, "instance": true}, true }}
	r := good("Down")
	for text, want := range map[string][]string{
		`Pod {{ $labels.pod }} is down{{ with $labels.cluster }} on cluster {{ . }}{{ end }}.`:              nil,
		`Pod {{ $labels.pod }}{{ if $labels.cluster }} on {{ $labels.cluster }}{{ end }}.`:                  nil,
		`{{ if and $labels.cluster $labels.pod }}{{ $labels.cluster }}{{ end }}`:                            nil,
		`{{ if $labels.pod }}{{ $labels.cluster }}{{ end }}`:                                                {LabelNeverSeen + ":cluster"},
		`{{ if $labels.cluster }}x{{ else }}{{ $labels.cluster }}{{ end }}`:                                 {LabelNeverSeen + ":cluster"},
		`{{ $labels.cluster }}{{ with $labels.cluster }}{{ . }}{{ end }}`:                                   {LabelNeverSeen + ":cluster"},
		`{{ with $labels.pod }}{{ $labels.cluster }}{{ end }}`:                                              {LabelNeverSeen + ":cluster"},
		`{{ if index $labels "cluster" }}{{ index $labels "cluster" }}{{ else if $labels.zone }}z{{ end }}`: nil,
	} {
		r.Says.Description = text
		one(t, r, seen, want...)
	}
	// Guessed from the expression, the same.
	r.Expr, r.Says.Summary = "sum by (pod) (x) > 1", "down"
	r.Says.Description = `{{ $labels.pod }}{{ with $labels.cluster }} on {{ . }}{{ end }}`
	one(t, r, Options{})
}

// One alert in two groups, named apart, is one alert to an alert manager:
// not the same condition as another rule. Two alerts are.
func TestSameAlertTwoGroups(t *testing.T) {
	a, b, c := good("HighErrors"), good("HighErrors"), good("Other")
	a.Group, b.Group, c.Group = "team-a", "slo", "slo"
	c.Expr = a.Expr
	rs := []rules.Rule{a, b, good("x"), good("y")}
	source.TellApart(rs)
	if rs[0].Name == rs[1].Name {
		t.Fatalf("not named apart: %q", rs[0].Name)
	}
	fs := Check(rs, Options{})
	if fs[0] != nil || fs[1] != nil {
		t.Errorf("one alert in two groups: %q / %q", checks(fs, 0), checks(fs, 1))
	}
	rs = []rules.Rule{a, b, c, good("x")}
	source.TellApart(rs)
	fs = Check(rs, Options{})
	want := fmt.Sprint([]string{Duplicate + ":Other"})
	if fmt.Sprint(checks(fs, 0)) != want || fmt.Sprint(checks(fs, 1)) != want {
		t.Errorf("each of the two, beside another alert: %q / %q", checks(fs, 0), checks(fs, 1))
	}
	if got := checks(fs, 2); !slices.Equal(got, []string{Duplicate + ":" + rs[1].Name, Duplicate + ":" + rs[0].Name}) {
		t.Errorf("the other alert: %q", got)
	}
}

// OCI's alarm list has no body: what an alarm says isn't known there, so
// nothing is faulted; a body given empty is.
func TestOCIBodyUnknown(t *testing.T) {
	rs, err := rules.Parse([]byte(`{"data": [{"id": "ocid1.alarm.oc1..a", "display-name": "disk-full", "namespace": "oci_computeagent",
		"query": "DiskUtilization[1m].mean() > 90", "severity": "CRITICAL", "is-enabled": true, "lifecycle-state": "ACTIVE",
		"alarm-summary": "Disk over 90% on {{resourceDisplayName}}", "notification-title": "Disk full"},
		{"id": "ocid1.alarm.oc1..b", "displayName": "mem", "severity": "WARNING", "body": ""}]}`))
	if err != nil {
		t.Fatal(err)
	}
	fs := Check(rs, Options{})
	if got := checks(fs, 0); got != nil {
		t.Errorf("an alarm as listed: %q", got)
	}
	if got := checks(fs, 1); !slices.Equal(got, []string{NoText, NoRunbook}) {
		t.Errorf("an alarm with an empty body: %q", got)
	}
}
