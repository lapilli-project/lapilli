package scenario

import (
	"fmt"
	"maps"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/rules"
)

// Runs says whether promtool runs a rule: one read from Prometheus's own
// format (rules.Rule.Prometheus), PromQL, with an expression. A Grafana
// rule, a Loki ruler's, a cloud's alarm, an Azure rule group's, isn't
// (docs/design/expectations.md, open question 1).
func Runs(r rules.Rule) bool {
	return r.Prometheus && r.Graph == nil && r.Logs == "" && r.Cloud == "" && r.Expr != ""
}

// GroupKey is what tells a rule's group from another's: the file it came
// from, its ruler's namespace, and its name, as Prometheus keeps a group by
// its file and name.
func GroupKey(source, namespace, group string) string {
	return fmt.Sprintf("%q %q %q", source, namespace, group)
}

// Group writes a group of rules as a Prometheus rule file holds it, as
// Lapilli has them: its name, interval, query_offset, limit and labels,
// the first rule's (a group's rules share them); its recording rules
// first, then its alert rules, each under the name its alerts carry
// (rules.Rule.AlertName), with what it says (its summary, description and
// other annotations) as Lapilli keeps them. Rules promtool doesn't run are
// left out.
func Group(alerts []rules.Rule, recorded []rules.Recording) ([]byte, error) {
	type rule struct {
		Record        string            `yaml:"record,omitempty"`
		Alert         string            `yaml:"alert,omitempty"`
		Expr          string            `yaml:"expr"`
		For           string            `yaml:"for,omitempty"`
		KeepFiringFor string            `yaml:"keep_firing_for,omitempty"`
		Labels        map[string]string `yaml:"labels,omitempty"`
		Annotations   map[string]string `yaml:"annotations,omitempty"`
	}
	type group struct {
		Name        string            `yaml:"name"`
		Interval    string            `yaml:"interval,omitempty"`
		QueryOffset string            `yaml:"query_offset,omitempty"`
		Limit       int               `yaml:"limit,omitempty"`
		Labels      map[string]string `yaml:"labels,omitempty"`
		Rules       []rule            `yaml:"rules"`
	}
	var g *group
	for _, r := range alerts {
		if !Runs(r) {
			continue
		}
		if g == nil {
			g = &group{Name: r.Group, Interval: duration(r.Interval), QueryOffset: duration(r.QueryOffset), Limit: r.Limit, Labels: r.GroupLabels}
		}
	}
	if g == nil {
		return nil, fmt.Errorf("no rule in it promtool runs")
	}
	if g.Name == "" {
		g.Name = "rules"
	}
	for _, r := range recorded {
		if r.Off || r.Expr == "" {
			continue
		}
		g.Rules = append(g.Rules, rule{Record: r.Record, Expr: r.Expr, Labels: r.Labels})
	}
	for _, r := range alerts {
		if !Runs(r) {
			continue
		}
		a := maps.Clone(r.Says.Annotations)
		if a == nil {
			a = map[string]string{}
		}
		for k, v := range map[string]string{"summary": r.Says.Summary, "description": r.Says.Description} {
			if v != "" {
				a[k] = v
			}
		}
		if len(a) == 0 {
			a = nil
		}
		g.Rules = append(g.Rules, rule{Alert: r.AlertName(), Expr: r.Expr, For: duration(r.For), KeepFiringFor: duration(r.KeepFiringFor),
			Labels: r.Labels, Annotations: a})
	}
	b, err := yaml.Marshal(map[string][]group{"groups": {*g}})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// duration is a duration as Prometheus writes one (model.Duration): 1h30m,
// 5m, 90s, 500ms; "" for none.
func duration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	var b strings.Builder
	for _, u := range []struct {
		d    time.Duration
		unit string
	}{{time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}, {time.Millisecond, "ms"}} {
		if n := d / u.d; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.unit)
			d -= n * u.d
		}
	}
	if b.Len() == 0 {
		return "1ms" // below a millisecond: as near as Prometheus writes one
	}
	return b.String()
}
