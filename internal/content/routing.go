package content

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/routing"
)

// The routing configuration (docs/design/routing.md, "What it reads"): the
// parts of Alertmanager's configuration, or of Grafana's policies, contact
// points and mute timings, that decide who a firing reaches, read for
// routing.Config. Like the template reader it keeps names, kinds, matchers,
// durations and booleans only: no field that holds a secret or a
// connection's address is read.

// ReadAlertmanagerRouting reads the routing of an Alertmanager
// configuration, as alertmanager.yml has it (and, with
// source.SplitDefinition, Mimir's, Cortex's and Amazon's): the route tree
// with each route's group_by, group_wait, group_interval, repeat_interval,
// mute_time_intervals and active_time_intervals, receivers' integrations
// with their kinds and send_resolved, time_intervals and the older
// mute_time_intervals, and inhibit_rules. What Alertmanager wouldn't load (a
// matcher or a regular expression that doesn't parse, a time interval that is
// wrong, a route naming a receiver that isn't there) is an error.
func ReadAlertmanagerRouting(config []byte) (*routing.Config, error) {
	m, err := yamlMap(config)
	if err != nil {
		return nil, fmt.Errorf("Alertmanager's configuration: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(config, &doc); err != nil || len(doc.Content) != 1 {
		return nil, fmt.Errorf("Alertmanager's configuration: %v", err)
	}
	return readRouting(m, &doc)
}

func readRouting(m map[string]any, doc *yaml.Node) (*routing.Config, error) {
	if _, ok := m["route"].(map[string]any); !ok {
		return nil, errors.New("Alertmanager's configuration has no route")
	}
	count, matchers := 0, 0
	rb := routing.NewRegexBudget()
	root, err := amRouting(yamlField(doc.Content[0], "route", 0), &count, &matchers, 0, rb)
	if err != nil {
		return nil, err
	}
	var receivers []routing.Receiver
	list, _ := m["receivers"].([]any)
	for _, x := range list {
		rm, _ := x.(map[string]any)
		name, _ := rm["name"].(string)
		if name == "" || len(name) > 1024 {
			continue
		}
		r := routing.Receiver{Name: name}
		for _, k := range slices.SortedFunc(maps.Keys(rm), func(a, b string) int {
			return routing.KindOrder(strings.TrimSuffix(a, "_configs"), strings.TrimSuffix(b, "_configs"))
		}) {
			kind, ok := strings.CutSuffix(k, "_configs")
			if !ok {
				continue
			}
			items, _ := rm[k].([]any)
			for j, it := range items {
				im, _ := it.(map[string]any)
				send := routing.DefaultSendResolved(kind)
				if b, ok := im["send_resolved"].(bool); ok {
					send = b
				}
				r.Integrations = append(r.Integrations, routing.Integration{Kind: kind, Index: j, SendResolved: send})
			}
		}
		receivers = append(receivers, r)
	}
	intervals := map[string][]routing.TimeInterval{}
	parts := 0
	for _, key := range []string{"mute_time_intervals", "time_intervals"} {
		items, _ := m[key].([]any)
		for _, x := range items {
			im, _ := x.(map[string]any)
			name, _ := im["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("a %s entry has no name", key)
			}
			if len(name) > routing.MaxName {
				return nil, fmt.Errorf("a time interval's name is %d bytes at most", routing.MaxName)
			}
			if _, dup := intervals[name]; dup {
				return nil, fmt.Errorf("time interval %q is defined twice", clipText(name))
			}
			if len(intervals) >= routing.MaxIntervals {
				return nil, fmt.Errorf("more than %d time intervals", routing.MaxIntervals)
			}
			tis, err := routing.ParseTimeIntervals(im["time_intervals"])
			if err != nil {
				return nil, fmt.Errorf("time interval %q: %w", clipText(name), err)
			}
			if parts += partsOf(tis); parts > routing.MaxTimeParts {
				return nil, fmt.Errorf("the time intervals list more than %d ranges, weekdays, days, months and years in all", routing.MaxTimeParts)
			}
			intervals[name] = tis
		}
	}
	var inhibit []routing.InhibitRule
	rules, _ := m["inhibit_rules"].([]any)
	for i, x := range rules {
		rm, _ := x.(map[string]any)
		r := routing.InhibitRule{}
		r.Name, _ = rm["name"].(string)
		var err error
		if r.SourceMatchers, err = inhibitMatchers(rm, "source", rb); err != nil {
			return nil, fmt.Errorf("inhibit_rules[%d]: %w", i, err)
		}
		if r.TargetMatchers, err = inhibitMatchers(rm, "target", rb); err != nil {
			return nil, fmt.Errorf("inhibit_rules[%d]: %w", i, err)
		}
		eq, _ := rm["equal"].([]any)
		for _, e := range eq {
			if s, ok := e.(string); ok {
				r.Equal = append(r.Equal, s)
			}
		}
		inhibit = append(inhibit, r)
	}
	return routing.NewConfig(root, receivers, intervals, inhibit, false)
}

// inhibitMatchers are an inhibition rule's source or target matchers: the
// older {side}_match and {side}_match_re, and {side}_matchers.
func inhibitMatchers(rm map[string]any, side string, rb *routing.RegexBudget) ([]routing.Matcher, error) {
	var out []routing.Matcher
	add := func(name, op, value string) error {
		m, err := routing.NewMatcherBudget(rb, name, op, value)
		if err != nil {
			return err
		}
		out = append(out, m)
		return nil
	}
	for _, p := range []struct{ key, op string }{{side + "_match", "="}, {side + "_match_re", "=~"}} {
		mm, _ := rm[p.key].(map[string]any)
		for _, k := range slices.Sorted(maps.Keys(mm)) {
			v, _ := mm[k].(string)
			if err := add(k, p.op, v); err != nil {
				return nil, err
			}
		}
	}
	lines, _ := rm[side+"_matchers"].([]any)
	for _, l := range lines {
		text, _ := l.(string)
		ms, ok := amMatchers(text)
		if !ok {
			return nil, fmt.Errorf("matcher %q can't be read", clipText(text))
		}
		for _, x := range ms {
			if err := add(x[0], x[1], x[2]); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func clipText(s string) string {
	if len(s) > 80 {
		s = s[:80]
	}
	return strings.ToValidUTF8(s, "")
}

// partsOf is how many parts a named time interval's list has in all.
func partsOf(tis []routing.TimeInterval) int {
	n := 0
	for _, ti := range tis {
		n += ti.Parts()
	}
	return n
}

// amRouting reads a route of Alertmanager's tree for routing: what it says
// of itself, as the routing package inherits it.
func amRouting(m *yaml.Node, count, matchers *int, depth int, rb *routing.RegexBudget) (*routing.Route, error) {
	if *count++; *count > routing.MaxRoutes {
		return nil, fmt.Errorf("more than %d routes", routing.MaxRoutes)
	}
	if depth > routing.MaxDepth {
		return nil, fmt.Errorf("routes more than %d levels deep", routing.MaxDepth)
	}
	r := &routing.Route{}
	r.Receiver = yamlText(yamlField(m, "receiver", 0))
	r.Continue = yamlTrue(yamlField(m, "continue", 0))
	add := func(name, op, value string) error {
		if *matchers++; *matchers > routing.MaxMatchers {
			return errors.New("more matchers than are read")
		}
		x, err := routing.NewMatcherBudget(rb, name, op, value)
		if err != nil {
			return err
		}
		r.Matchers = append(r.Matchers, x)
		return nil
	}
	for _, p := range []struct{ key, op string }{{"match", "="}, {"match_re", "=~"}} {
		mm := yamlField(m, p.key, 0)
		if mm == nil || mm.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(mm.Content); i += 2 {
			if err := add(yamlText(mm.Content[i]), p.op, yamlText(mm.Content[i+1])); err != nil {
				return nil, err
			}
		}
	}
	if l := yamlField(m, "matchers", 0); l != nil && l.Kind == yaml.SequenceNode {
		for _, x := range l.Content {
			ms, ok := amMatchers(yamlText(x))
			if !ok {
				return nil, fmt.Errorf("matcher %q can't be read", clipText(yamlText(x)))
			}
			for _, y := range ms {
				if err := add(y[0], y[1], y[2]); err != nil {
					return nil, err
				}
			}
		}
	}
	if l := yamlField(m, "group_by", 0); l != nil && l.Kind == yaml.SequenceNode {
		var by []string
		for _, x := range l.Content {
			if s := yamlText(x); s == "..." {
				r.GroupByAll = true
			} else {
				by = append(by, s)
			}
		}
		if r.GroupByAll && len(by) > 0 {
			return nil, errors.New("cannot have wildcard group_by (`...`) and other labels at the same time")
		}
		if !r.GroupByAll {
			r.GroupBy, r.GroupBySet = by, true
		}
	}
	for _, p := range []struct {
		key string
		to  **time.Duration
	}{{"group_wait", &r.GroupWait}, {"group_interval", &r.GroupInterval}, {"repeat_interval", &r.RepeatInterval}} {
		if v := yamlField(m, p.key, 0); v != nil {
			d, err := routing.ParseDuration(yamlText(v))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p.key, err)
			}
			*p.to = &d
		}
	}
	names := func(key string) []string {
		var out []string
		if l := yamlField(m, key, 0); l != nil && l.Kind == yaml.SequenceNode {
			for _, x := range l.Content {
				out = append(out, yamlText(x))
			}
		}
		return out
	}
	r.MuteTimeIntervals, r.ActiveTimeIntervals = names("mute_time_intervals"), names("active_time_intervals")
	if routes := yamlField(m, "routes", 0); routes != nil && routes.Kind == yaml.SequenceNode {
		for _, c := range routes.Content {
			if c = yamlNode(c); c == nil || c.Kind != yaml.MappingNode {
				continue
			}
			child, err := amRouting(c, count, matchers, depth+1, rb)
			if err != nil {
				return nil, err
			}
			r.Routes = append(r.Routes, child)
		}
	}
	return r, nil
}

// ReadGrafanaRouting reads the routing of a Grafana: GET
// /api/v1/provisioning/contact-points, /policies and /mute-timings (nil when
// there are none to read). A contact point's type is its integration's kind,
// and it sends resolves unless disableResolveMessage says it doesn't; contact
// points of one name are one receiver's integrations. A policy's group_by,
// group_wait, group_interval and repeat_interval, its mute and active timings
// and its matchers are read as Alertmanager's are.
func ReadGrafanaRouting(contactPoints, policies, muteTimings []byte) (*routing.Config, error) {
	var cps []struct {
		Name                  string `json:"name"`
		Type                  string `json:"type"`
		DisableResolveMessage bool   `json:"disableResolveMessage"`
	}
	if err := json.Unmarshal(contactPoints, &cps); err != nil {
		return nil, fmt.Errorf("the contact points: %w", err)
	}
	var tree policyJSON
	if err := json.Unmarshal(policies, &tree); err != nil {
		return nil, fmt.Errorf("the notification policies: %w", err)
	}
	var receivers []routing.Receiver
	at := map[string]int{}
	kinds := map[string]map[string]int{}
	for _, cp := range cps {
		if cp.Name == "" || len(cp.Name) > 1024 {
			continue
		}
		i, ok := at[cp.Name]
		if !ok {
			i = len(receivers)
			at[cp.Name] = i
			receivers = append(receivers, routing.Receiver{Name: cp.Name})
			kinds[cp.Name] = map[string]int{}
		}
		kind := strings.ToLower(cp.Type)
		receivers[i].Integrations = append(receivers[i].Integrations, routing.Integration{
			Kind: kind, Index: kinds[cp.Name][kind], SendResolved: !cp.DisableResolveMessage})
		kinds[cp.Name][kind]++
	}
	intervals := map[string][]routing.TimeInterval{}
	parts := 0
	if len(muteTimings) > 0 {
		var mts []map[string]any
		if err := json.Unmarshal(muteTimings, &mts); err != nil {
			return nil, fmt.Errorf("the mute timings: %w", err)
		}
		for _, mt := range mts {
			name, _ := mt["name"].(string)
			if name == "" {
				return nil, errors.New("a mute timing has no name")
			}
			if len(name) > routing.MaxName {
				return nil, fmt.Errorf("a mute timing's name is %d bytes at most", routing.MaxName)
			}
			if len(intervals) >= routing.MaxIntervals {
				return nil, fmt.Errorf("more than %d mute timings", routing.MaxIntervals)
			}
			tis, err := routing.ParseTimeIntervals(mt["time_intervals"])
			if err != nil {
				return nil, fmt.Errorf("mute timing %q: %w", clipText(name), err)
			}
			if parts += partsOf(tis); parts > routing.MaxTimeParts {
				return nil, fmt.Errorf("the mute timings list more than %d ranges, weekdays, days, months and years in all", routing.MaxTimeParts)
			}
			intervals[name] = tis
		}
	}
	count, matchers := 0, 0
	g := &grafanaRead{rb: routing.NewRegexBudget(), count: &count, matchers: &matchers}
	root, err := tree.routing(g, 0)
	if err != nil {
		return nil, err
	}
	cfg, err := routing.NewConfig(root, receivers, intervals, nil, true)
	if err != nil {
		return nil, err
	}
	for _, w := range g.warnings {
		cfg.Warn(w)
	}
	return cfg, nil
}

// grafanaRead is what reading a Grafana's policies shares across them: the
// regular expressions' budget, how many policies and matchers there have
// been, and what was changed on the way (a timing under a second).
type grafanaRead struct {
	rb       *routing.RegexBudget
	count    *int
	matchers *int
	warnings []string
}

// routing is a policy as the routing package reads a route.
func (p policyJSON) routing(g *grafanaRead, depth int) (*routing.Route, error) {
	if *g.count++; *g.count > routing.MaxRoutes {
		return nil, fmt.Errorf("more than %d policies", routing.MaxRoutes)
	}
	if depth > routing.MaxDepth {
		return nil, fmt.Errorf("policies more than %d levels deep", routing.MaxDepth)
	}
	r := &routing.Route{Receiver: p.Receiver, Continue: p.Continue}
	add := func(name, op, value string) error {
		if *g.matchers++; *g.matchers > routing.MaxMatchers {
			return fmt.Errorf("more than %d matchers", routing.MaxMatchers)
		}
		m, err := routing.NewMatcherBudget(g.rb, name, op, value)
		if err != nil {
			return err
		}
		r.Matchers = append(r.Matchers, m)
		return nil
	}
	for _, m := range p.ObjectMatchers {
		if len(m) == 3 {
			if err := add(m[0], m[1], m[2]); err != nil {
				return nil, err
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(p.Match)) {
		if err := add(k, "=", p.Match[k]); err != nil {
			return nil, err
		}
	}
	for _, k := range slices.Sorted(maps.Keys(p.MatchRe)) {
		if err := add(k, "=~", p.MatchRe[k]); err != nil {
			return nil, err
		}
	}
	for _, m := range p.Matchers {
		g := matcherText.FindStringSubmatch(m)
		if g == nil {
			return nil, fmt.Errorf("matcher %q can't be read", clipText(m))
		}
		v := g[3]
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = strings.ReplaceAll(v[1:len(v)-1], `\"`, `"`)
		}
		if err := add(g[1], g[2], v); err != nil {
			return nil, err
		}
	}
	if p.GroupBy != nil {
		var by []string
		for _, s := range p.GroupBy {
			if s == "..." {
				r.GroupByAll = true
			} else {
				by = append(by, s)
			}
		}
		if r.GroupByAll && len(by) > 0 {
			return nil, errors.New("cannot have wildcard group_by (`...`) and other labels at the same time")
		}
		if !r.GroupByAll {
			r.GroupBy, r.GroupBySet = by, true
		}
	}
	for _, d := range []struct {
		key  string
		text *string
		to   **time.Duration
	}{{"group_wait", p.GroupWait, &r.GroupWait}, {"group_interval", p.GroupInterval, &r.GroupInterval}, {"repeat_interval", p.RepeatInterval, &r.RepeatInterval}} {
		if d.text == nil || *d.text == "" {
			continue
		}
		v, err := routing.ParseDuration(*d.text)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.key, err)
		}
		// Grafana accepts a timing of any length (1ns), and the model works
		// out a flush for each interval: under a second it is read as one,
		// and said.
		if v > 0 && v < routing.MinInterval {
			g.warnings = append(g.warnings, fmt.Sprintf("a policy's %s of %s is read as %s: Lapilli doesn't model timings under %s", d.key, v, routing.MinInterval, routing.MinInterval))
			v = routing.MinInterval
		}
		*d.to = &v
	}
	r.MuteTimeIntervals, r.ActiveTimeIntervals = p.MuteTimeIntervals, p.ActiveTimeIntervals
	for _, c := range p.Routes {
		child, err := c.routing(g, depth+1)
		if err != nil {
			return nil, err
		}
		r.Routes = append(r.Routes, child)
	}
	return r, nil
}
