package verdict

import (
	"cmp"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/rules"
)

// A Redactor gives each name in a report an alias, so the report can be
// shown to someone outside without its rules, services, label values,
// incidents, teams or sources: what lapilli verdict --redact writes. Numbers,
// severities, durations and times stay as they are.
//
// Without a key, aliases are an output's own and say nothing of the name:
// rule-01, rule-02, numbered in an order no name decides (a keyed hash of
// each, with a key made for the output), and incidents incident-01 on, by
// when they started. With a key (--redact-key-file), a name's alias is a keyed
// hash of it, rule-3f9a1c04e2, the same in every output made with the key,
// and not to be worked back from without it. A hash with no key would be:
// a short name is found by trying names.
type Redactor struct {
	key     []byte
	keyed   bool
	aliases map[string]map[string]string // kind, then name
	// dependsLabel is the label whose values the services on a catch's
	// path are (Settings.Depends.Label): aliased as its values are.
	dependsLabel string
}

// NewRedactor is a Redactor: with key, aliases that hold across outputs
// made with it; with none, this output's own.
func NewRedactor(key []byte) *Redactor {
	x := &Redactor{key: key, keyed: len(key) > 0, aliases: map[string]map[string]string{}}
	if !x.keyed {
		x.key = make([]byte, 32)
		rand.Read(x.key)
	}
	return x
}

// RedactKeyMin is the fewest bytes a key may have.
const RedactKeyMin = 16

// Redacted is said for text that may hold names and has no alias: why a
// rule couldn't be replayed, an expression, shifts naming a schedule.
const Redacted = "[redacted]"

func (x *Redactor) mac(kind, name string) string {
	m := hmac.New(sha256.New, x.key)
	m.Write([]byte(kind + "\x00" + name))
	return hex.EncodeToString(m.Sum(nil))
}

// prefix is what a kind's aliases start with: a label's own name, when it
// is one Prometheus allows; label-team for a label named as a kind is, so
// a team label's value is never taken for a team's alias.
var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,39}$`)

var kinds = map[string]bool{"rule": true, "group": true, "team": true, "source": true, "record": true, "incident": true, "label": true, "signal": true, "kind": true, "page": true, "receiver": true, "inhibition": true, "silence": true, "interval": true}

func prefix(kind string) string {
	if name, ok := strings.CutPrefix(kind, "label:"); ok {
		switch {
		case !labelName.MatchString(name):
			return "label"
		case kinds[name]:
			return "label-" + name
		}
		return name
	}
	return kind
}

// number gives each of names, in order, its alias of kind: numbered from
// 01, as wide as the most of them needs, or with a key, its keyed hash.
func (x *Redactor) number(kind string, names []string) {
	if x.aliases[kind] == nil {
		x.aliases[kind] = map[string]string{}
	}
	have := x.aliases[kind]
	width := max(2, len(fmt.Sprint(len(names)+len(have))))
	for _, n := range names {
		if _, ok := have[n]; ok || n == "" {
			continue
		}
		if x.keyed {
			have[n] = prefix(kind) + "-" + x.mac(kind, n)[:10]
			continue
		}
		have[n] = fmt.Sprintf("%s-%0*d", prefix(kind), width, len(have)+1)
	}
}

// numberAnyhow numbers names in an order no name decides: by a keyed hash
// of each.
func (x *Redactor) numberAnyhow(kind string, names map[string]bool) {
	list := slices.Collect(maps.Keys(names))
	slices.SortFunc(list, func(a, b string) int { return cmp.Compare(x.mac(kind, a), x.mac(kind, b)) })
	x.number(kind, list)
}

// alias is name's alias of kind, once numbered; "" stays "".
func (x *Redactor) alias(kind, name string) string {
	if name == "" {
		return ""
	}
	if a, ok := x.aliases[kind][name]; ok {
		return a
	}
	x.number(kind, []string{name}) // one the walk missed: still never shown
	return x.aliases[kind][name]
}

// kept are the labels whose values stay: a severity says how loud, and
// names nothing.
var kept = map[string]bool{"severity": true}

func (x *Redactor) labels(l map[string]string) map[string]string {
	if l == nil {
		return nil
	}
	out := make(map[string]string, len(l))
	for k, v := range l {
		if kept[k] {
			out[k] = v
			continue
		}
		out[k] = x.alias("label:"+k, v)
	}
	return out
}

func (x *Redactor) incidentLabels(l incident.Labels) incident.Labels {
	if l == nil {
		return nil
	}
	out := make(incident.Labels, len(l))
	for k, vs := range l {
		var as []string
		for _, v := range vs {
			if kept[k] {
				as = append(as, v)
			} else {
				as = append(as, x.alias("label:"+k, v))
			}
		}
		slices.Sort(as)
		out[k] = as
	}
	return out
}

// reason is why a rule couldn't be replayed, without what it names: a
// query, a host, an error from the history. Whether its replay failed, as
// --fail-unreplayed and the exit status count it, is kept.
func reason(s string) string {
	switch {
	case s == "", s == rules.Unfinished, s == rules.Disabled, s == RecordedReason:
		return s
	case strings.HasPrefix(s, rules.HistoryUnread):
		return rules.HistoryUnread + Redacted
	case strings.HasPrefix(s, rules.QueryFailed):
		return rules.QueryFailed + Redacted
	case strings.HasPrefix(s, rules.TooWide):
		return rules.TooWide + Redacted
	}
	return "not replayed: " + Redacted
}

// names gathers every name in the reports, by kind, so each is numbered
// before any is used.
type names struct {
	rules, groups, teams, sources map[string]bool
	labels                        map[string]map[string]bool
	// signals are the named impact signals' names.
	signals map[string]bool
	// kinds are the kinds of marked window, which a team names.
	kinds map[string]bool
	// pages are the ids of the pages firings were sent as.
	pages map[string]bool
	// notified are the names the worked-out notifications carry, by kind:
	// receivers, inhibition rules, silences and time intervals.
	notified  map[string]map[string]bool
	incidents []incident.Incident
	records   map[string]bool
	// expected are the incidents expectations name, numbered after those
	// of the period.
	expected map[string]bool
}

// notification gathers the names a firing's notifications carry.
func (n *names) notification(fn *FiringNotifications) {
	if fn == nil {
		return
	}
	add := func(kind, name string) {
		if name == "" {
			return
		}
		if n.notified[kind] == nil {
			n.notified[kind] = map[string]bool{}
		}
		n.notified[kind][name] = true
	}
	for _, name := range fn.Receivers {
		add("receiver", name)
	}
	for _, name := range fn.NoIntegrations {
		add("receiver", name)
	}
	for _, m := range fn.Muted {
		n.rules[m.SourceRule] = true
		switch kindOfMute(m.Kind) {
		case "interval":
			for _, part := range strings.Split(m.By, ",") {
				add("interval", part)
			}
		default:
			add(kindOfMute(m.Kind), m.By)
		}
	}
}

// kindOfMute is the kind of name a mute's By is.
func kindOfMute(kind string) string {
	switch kind {
	case string(routingMutedInhibition):
		return "inhibition"
	case string(routingMutedSilence):
		return "silence"
	}
	return "interval"
}

func (n *names) label(k, v string) {
	if kept[k] || v == "" {
		return
	}
	if n.labels[k] == nil {
		n.labels[k] = map[string]bool{}
	}
	n.labels[k][v] = true
}

// alerts are the names in the alerts an incident lists: their rules', and
// their labels' values.
func (n *names) alerts(as []incident.Alert) {
	for _, a := range as {
		n.rules[a.Rule] = true
		for k, v := range a.Labels {
			n.label(k, v)
		}
	}
}

func (n *names) rule(rr RuleResult) {
	n.rules[rr.Rule] = true
	n.groups[rr.Group] = true
	n.teams[rr.Team] = true
	n.sources[rr.Source] = true
	for k := range rr.Marked {
		n.kinds[k] = true
	}
}

// path notes the services on the path of a catch by dependency: values of
// the label the graph's nodes are.
func (n *names) path(label string, cs []Catch) {
	for _, c := range cs {
		for _, v := range c.Dependency {
			n.label(label, v)
		}
	}
}

func (n *names) report(r *Report) {
	if r == nil {
		return
	}
	if r.Settings != nil && r.Settings.Depends != nil {
		for _, in := range r.Incidents {
			n.path(r.Settings.Depends.Label, in.CaughtBy)
		}
	}
	for _, rr := range r.Rules {
		n.rule(rr)
	}
	if r.Settings != nil {
		for _, sg := range r.Settings.ImpactSignals {
			n.signals[sg.Name] = true
		}
		for _, e := range r.Settings.ImpactRules {
			for _, sg := range e.Signals {
				n.signals[sg] = true
			}
			for k, vs := range e.Labels {
				for _, v := range vs {
					n.label(k, v)
				}
			}
		}
	}
	for _, in := range r.Incidents {
		n.incidents = append(n.incidents, in.Incident)
		n.signals[in.Signal] = true
		n.alerts(in.Alerts)
		for k, vs := range in.Labels {
			for _, v := range vs {
				n.label(k, v)
			}
		}
		for _, sp := range in.Spans {
			for k, v := range sp.Labels {
				n.label(k, v)
			}
		}
	}
	if r.Notifications != nil {
		for _, rc := range r.Notifications.Receivers {
			if n.notified["receiver"] == nil {
				n.notified["receiver"] = map[string]bool{}
			}
			n.notified["receiver"][rc.Name] = true
		}
	}
	for _, rr := range r.Rules {
		if rr.Notifications != nil {
			n.rules[rr.Notifications.InhibitedBy] = true
		}
	}
	for _, f := range r.Firings {
		n.rules[f.Rule] = true
		n.notification(f.Notifications)
		for _, sg := range f.Signals {
			n.signals[sg] = true
		}
		for k, v := range f.Labels {
			n.label(k, v)
		}
		for _, k := range f.Marked {
			n.kinds[k] = true
		}
		if f.Response != nil {
			if f.Response.Page != nil {
				n.pages[f.Response.Page.ID] = true
			}
			if f.Response.Change != nil {
				for k, v := range f.Response.Change.Labels {
					n.label(k, v)
				}
			}
		}
	}
	for _, u := range r.Unrecorded {
		n.teams[u.Team] = true
		n.teams[u.Scope] = true
		for _, a := range u.Top {
			n.teams[a.Team] = true
			for k, v := range a.Labels {
				n.label(k, v)
			}
		}
	}
	for _, nm := range r.NearMisses {
		for k, v := range nm.Labels {
			n.label(k, v)
		}
		if r.Settings != nil && r.Settings.Depends != nil {
			for _, v := range nm.Dependency {
				n.label(r.Settings.Depends.Label, v)
			}
		}
	}
	if r.Hours != nil {
		for _, t := range r.Hours.Teams {
			n.teams[t.Team] = true
		}
	}
}

// prepare numbers every name in the reports and the comparison.
func (x *Redactor) prepare(c *Comparison, reps ...*Report) {
	n := &names{rules: map[string]bool{}, groups: map[string]bool{}, teams: map[string]bool{}, sources: map[string]bool{},
		labels: map[string]map[string]bool{}, records: map[string]bool{}, expected: map[string]bool{}, signals: map[string]bool{}, kinds: map[string]bool{}, pages: map[string]bool{}, notified: map[string]map[string]bool{}}
	for _, r := range reps {
		n.report(r)
		if r != nil && r.Settings != nil && r.Settings.Depends != nil {
			x.dependsLabel = r.Settings.Depends.Label
		}
	}
	if c != nil {
		if x.dependsLabel != "" {
			for _, ic := range c.Incidents {
				n.path(x.dependsLabel, ic.Before)
				n.path(x.dependsLabel, ic.After)
			}
			for _, ic := range slices.Concat(c.Missed, c.MissedByDependency) {
				n.path(x.dependsLabel, ic.Before)
				n.path(x.dependsLabel, ic.After)
			}
		}
		for _, rc := range c.Rules {
			n.rules[rc.Rule] = true
		}
		for _, ic := range c.Incidents {
			n.incidents = append(n.incidents, ic.Incident)
			n.signals[ic.Signal] = true
			n.alerts(ic.Alerts)
			for k, vs := range ic.Labels {
				for _, v := range vs {
					n.label(k, v)
				}
			}
		}
		for _, rc := range c.Recording {
			n.records[rc.Record] = true
			n.groups[rc.Group] = true
		}
		if c.Expectations != nil {
			for _, r := range c.Expectations.Results {
				n.rules[r.Rule] = true
				if id, ok := strings.CutPrefix(r.What, "catches "); ok {
					n.expected[id] = true // not in the period, maybe
				}
			}
			for _, name := range c.Expectations.Unexpected {
				n.rules[name] = true
			}
		}
	}
	delete(n.rules, "")
	x.numberAnyhow("rule", n.rules)
	delete(n.groups, "")
	x.numberAnyhow("group", n.groups)
	delete(n.teams, "")
	x.numberAnyhow("team", n.teams)
	delete(n.sources, "")
	x.numberAnyhow("source", n.sources)
	x.numberAnyhow("kind", n.kinds)
	delete(n.pages, "")
	x.numberAnyhow("page", n.pages)
	for _, kind := range slices.Sorted(maps.Keys(n.notified)) {
		x.numberAnyhow(kind, n.notified[kind])
	}
	x.numberAnyhow("record", n.records)
	delete(n.signals, "")
	x.numberAnyhow("signal", n.signals)
	for _, k := range slices.Sorted(maps.Keys(n.labels)) {
		x.numberAnyhow("label:"+k, n.labels[k])
	}
	// Incidents by when they started, which a redacted report shows
	// anyway; at one time, in an order no id decides.
	slices.SortStableFunc(n.incidents, func(a, b incident.Incident) int {
		return cmp.Or(a.Start.Compare(b.Start), cmp.Compare(x.mac("incident", a.ID), x.mac("incident", b.ID)))
	})
	var ids []string
	for _, in := range n.incidents {
		ids = append(ids, in.ID)
	}
	x.number("incident", ids)
	x.numberAnyhow("incident", n.expected)
}

func (x *Redactor) ruleResult(rr RuleResult) RuleResult {
	rr.Rule = x.alias("rule", rr.Rule)
	rr.Group = x.alias("group", rr.Group)
	rr.Team = x.alias("team", rr.Team)
	rr.Source = x.alias("source", rr.Source)
	rr.Unsupported = reason(rr.Unsupported)
	rr.Unseen = slices.Clone(rr.Unseen)
	if rr.NoData != nil {
		// Expressions: metrics' and labels' names.
		rr.NoData = slices.Repeat([]string{Redacted}, len(rr.NoData))
	}
	if rr.Notifications != nil {
		n := *rr.Notifications
		n.InhibitedBy = x.alias("rule", n.InhibitedBy)
		rr.Notifications = &n
	}
	if rr.Marked != nil {
		m := make(map[string]MarkedCount, len(rr.Marked))
		for k, c := range rr.Marked {
			m[x.alias("kind", k)] = c
		}
		rr.Marked = m
	}
	return rr
}

func (x *Redactor) ruleResultPtr(rr *RuleResult) *RuleResult {
	if rr == nil {
		return nil
	}
	out := x.ruleResult(*rr)
	return &out
}

func (x *Redactor) incident(in incident.Incident) incident.Incident {
	in.ID = x.alias("incident", in.ID)
	in.Title = ""
	in.Signal = x.alias("signal", in.Signal)
	in.Match = slices.Clone(in.Match) // labels' names, which stay
	in.Labels = x.incidentLabels(in.Labels)
	if in.Alerts != nil {
		as := make([]incident.Alert, len(in.Alerts))
		for i, a := range in.Alerts {
			as[i] = incident.Alert{Rule: x.alias("rule", a.Rule), Labels: x.labels(a.Labels)}
		}
		in.Alerts = incident.SortAlerts(as) // by alias: no name decides the order
	}
	in.Start, in.End = utc(in.Start), utc(in.End)
	if in.Spans != nil {
		spans := make([]incident.Span, len(in.Spans))
		for i, sp := range in.Spans {
			spans[i] = incident.Span{Labels: x.labels(sp.Labels), Start: utc(sp.Start), End: utc(sp.End)}
		}
		// By when each started, at one time by its labels: as found.
		slices.SortStableFunc(spans, func(a, b incident.Span) int {
			return cmp.Or(a.Start.Compare(b.Start), strings.Compare(labelsText(a.Labels), labelsText(b.Labels)))
		})
		in.Spans = spans
	}
	return in
}

// utc is t in UTC: an offset an incidents file gave says where it was
// written.
func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

// byStart orders incidents as a report does, by when they started, and at
// one time by alias: neither a file's order nor an id decides it.
func byStart(a, b incident.Incident) int {
	return cmp.Or(a.Start.Compare(b.Start), strings.Compare(a.ID, b.ID))
}

// labelsText is labels in order, k=v, to break a tie by.
func labelsText(l map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(l)) {
		b.WriteString(k + "=" + l[k] + "\x00")
	}
	return b.String()
}

func (x *Redactor) catches(cs []Catch) []Catch {
	if cs == nil {
		return nil
	}
	out := make([]Catch, len(cs))
	for i, c := range cs {
		c.Rule = x.alias("rule", c.Rule)
		if c.Dependency != nil {
			path := make([]string, len(c.Dependency))
			for k, v := range c.Dependency {
				path[k] = x.alias("label:"+x.dependsLabel, v)
			}
			c.Dependency = path
		}
		out[i] = c
	}
	// At one delay, by name, as Judge orders them: by alias now.
	slices.SortStableFunc(out, func(a, b Catch) int { return cmp.Or(cmp.Compare(a.Delay, b.Delay), strings.Compare(a.Rule, b.Rule)) })
	return out
}

// sortRules puts rules in the order Judge does, by alias at a tie, so no
// name decides it.
func sortRules(rs []RuleResult) {
	slices.SortStableFunc(rs, func(a, b RuleResult) int {
		return cmp.Or(cmp.Compare(b.Black, a.Black), cmp.Compare(b.Firings, a.Firings), strings.Compare(a.Rule, b.Rule))
	})
}

// report is r with every name given its alias. Each list is put in the
// order r's has, by what it is ordered by, with the alias where a name
// broke a tie or a file's order stood: a list in r's order would say which
// names sort first.
func (x *Redactor) report(r *Report) *Report {
	if r == nil {
		return nil
	}
	out := *r
	out.From, out.To = utc(r.From), utc(r.To)
	if r.Settings != nil {
		st := *r.Settings // label names, which stay, and durations
		st.Match = slices.Clone(r.Settings.Match)
		st.ImpactFrom = utc(r.Settings.ImpactFrom)
		if r.Settings.Depends != nil {
			dp := *r.Settings.Depends // a label's name, depth, direction and a count
			st.Depends = &dp
		}
		// Where it was made, as the hours' zone: its days stay, which
		// part the episodes.
		if st.Zone != "" {
			st.Zone = Redacted
		}
		st.ImpactSignals = nil
		for _, sg := range r.Settings.ImpactSignals {
			sg.Name = x.alias("signal", sg.Name)
			sg.Match = slices.Clone(sg.Match)
			sg.Start = utc(sg.Start)
			st.ImpactSignals = append(st.ImpactSignals, sg)
		}
		// A rule entry's pattern names rules: not shown as it is.
		st.ImpactRules = nil
		for _, e := range r.Settings.ImpactRules {
			re := RuleEntry{Labels: x.incidentLabels(e.Labels), Match: slices.Clone(e.Match)}
			if e.Rules != "" {
				re.Rules = Redacted
			}
			for _, sg := range e.Signals {
				re.Signals = append(re.Signals, x.alias("signal", sg))
			}
			st.ImpactRules = append(st.ImpactRules, re)
		}
		out.Settings = &st
	}
	// Lapilli's own words, or none: what else may come one day is not shown.
	if out.Extras != "" && out.Extras != "skipped" && out.Extras != "cut" {
		out.Extras = Redacted
	}
	out.Rules = make([]RuleResult, len(r.Rules))
	for i, rr := range r.Rules {
		out.Rules[i] = x.ruleResult(rr)
	}
	sortRules(out.Rules)
	out.Incidents = make([]IncidentResult, len(r.Incidents))
	for i, in := range r.Incidents {
		out.Incidents[i] = IncidentResult{Incident: x.incident(in.Incident), CaughtBy: x.catches(in.CaughtBy), FiredAfter: x.catches(in.FiredAfter), NotKnown: in.NotKnown}
	}
	slices.SortStableFunc(out.Incidents, func(a, b IncidentResult) int { return byStart(a.Incident, b.Incident) })
	place := map[string]int{} // an incident's place in the list
	for i, in := range out.Incidents {
		place[in.ID] = i
	}
	out.Firings = x.firings(r.Firings)
	out.Notifications = x.reportNotifications(r.Notifications)
	if r.Suggestions != nil {
		out.Suggestions = make([]Suggestion, len(r.Suggestions))
		for i, s := range r.Suggestions {
			s.Rule = x.alias("rule", s.Rule)
			s.Before, s.After = x.ruleResult(s.Before), x.ruleResult(s.After)
			out.Suggestions[i] = s
		}
		// As the rules they are of, noisiest first (source.Suggest).
		slices.SortStableFunc(out.Suggestions, func(a, b Suggestion) int {
			return cmp.Or(cmp.Compare(b.Before.Black, a.Before.Black), cmp.Compare(b.Before.Firings, a.Before.Firings), strings.Compare(a.Rule, b.Rule))
		})
	}
	if r.NearMisses != nil {
		out.NearMisses = make([]NearMiss, len(r.NearMisses))
		for i, nm := range r.NearMisses {
			nm.Incident = x.alias("incident", nm.Incident)
			nm.Rule = x.alias("rule", nm.Rule)
			nm.Labels = x.labels(nm.Labels)
			if nm.Dependency != nil {
				path := make([]string, len(nm.Dependency))
				for k, v := range nm.Dependency {
					path[k] = x.alias("label:"+x.dependsLabel, v)
				}
				nm.Dependency = path
			}
			nm.Before = x.ruleResult(nm.Before)
			nm.After = x.ruleResultPtr(nm.After)
			if nm.Value != nil {
				v := *nm.Value
				if v.Of != "" {
					v.Of = Redacted // an expression: metrics' and labels' names
				}
				nm.Value = &v
			}
			out.NearMisses[i] = nm
		}
		// The incidents' order, then the most of its "for" or number kept,
		// as source.NearMisses has them.
		slices.SortStableFunc(out.NearMisses, func(a, b NearMiss) int {
			return cmp.Or(cmp.Compare(place[a.Incident], place[b.Incident]), cmp.Compare(b.Kept(), a.Kept()), strings.Compare(a.Rule, b.Rule))
		})
	}
	if r.Unrecorded != nil {
		out.Unrecorded = make([]Unrecorded, len(r.Unrecorded))
		for i, u := range r.Unrecorded {
			u.Rule = x.alias("rule", u.Rule)
			u.Team, u.Scope = x.alias("team", u.Team), x.alias("team", u.Scope)
			top := make([]AlertCount, len(u.Top))
			for j, a := range u.Top {
				top[j] = AlertCount{Labels: x.labels(a.Labels), Firings: a.Firings, Team: x.alias("team", a.Team)}
			}
			// The most firings first, as source.Unrecorded has them.
			slices.SortStableFunc(top, func(a, b AlertCount) int {
				return cmp.Or(cmp.Compare(b.Firings, a.Firings), strings.Compare(labelsText(a.Labels), labelsText(b.Labels)))
			})
			u.Top = top
			out.Unrecorded[i] = u
		}
		slices.SortStableFunc(out.Unrecorded, func(a, b Unrecorded) int {
			return cmp.Or(cmp.Compare(b.Firings, a.Firings), strings.Compare(a.Rule, b.Rule))
		})
	}
	if r.ImpactShort != nil {
		s := *r.ImpactShort
		s.Signal = x.alias("signal", s.Signal)
		out.ImpactShort = &s
	}
	out.Unreplayed = x.unreplayed(r.Unreplayed)
	if r.Hours != nil {
		h := *r.Hours
		// The hours firings came in were counted in the zone given; where
		// it is says where the team is.
		h.Zone = Redacted
		if h.Shifts != "" {
			h.Shifts = Redacted // an on-call schedule's name, among them
		}
		h.Teams = nil
		for _, t := range r.Hours.Teams {
			t.Team = x.alias("team", t.Team)
			t.Zone = Redacted
			if t.Shifts != "" {
				t.Shifts = Redacted
			}
			h.Teams = append(h.Teams, t)
		}
		slices.SortStableFunc(h.Teams, func(a, b TeamHours) int { return strings.Compare(a.Team, b.Team) })
		out.Hours = &h
	}
	out.RulesChanged = nil
	for _, t := range r.RulesChanged {
		out.RulesChanged = append(out.RulesChanged, utc(t))
	}
	return &out
}

func (x *Redactor) unreplayed(us []Unreplayed) []Unreplayed {
	if us == nil {
		return nil
	}
	out := make([]Unreplayed, len(us))
	for i, u := range us {
		u.Rule = x.alias("rule", u.Rule)
		u.Reason = reason(u.Reason)
		out[i] = u
	}
	return failedFirst(out)
}

// firings are fs with every name given its alias, in order of their
// start, then rule and labels by alias.
func (x *Redactor) firings(fs []Judged) []Judged {
	out := make([]Judged, len(fs))
	for i, f := range fs {
		f.Rule = x.alias("rule", f.Rule)
		f.Labels = x.labels(f.Labels)
		f.Start, f.End, f.Held = utc(f.Start), utc(f.End), utc(f.Held)
		if f.Signals != nil {
			sigs := make([]string, len(f.Signals))
			for i, sg := range f.Signals {
				sigs[i] = x.alias("signal", sg)
			}
			f.Signals = sigs
		}
		var ids []string
		for _, id := range f.Incidents {
			ids = append(ids, x.alias("incident", id))
		}
		slices.Sort(ids)
		f.Incidents = ids
		var afters []string
		for _, id := range f.After {
			afters = append(afters, x.alias("incident", id))
		}
		slices.Sort(afters)
		f.After = afters
		var unrelated []string
		for _, id := range f.Unrelated {
			unrelated = append(unrelated, x.alias("incident", id))
		}
		slices.Sort(unrelated)
		f.Unrelated = unrelated
		var kinds []string
		for _, k := range f.Marked {
			kinds = append(kinds, x.alias("kind", k))
		}
		slices.Sort(kinds)
		f.Marked = kinds
		f.Response = x.response(f.Response)
		f.Notifications = x.firingNotifications(f.Notifications)
		out[i] = f
	}
	slices.SortStableFunc(out, func(a, b Judged) int {
		return cmp.Or(a.Start.Compare(b.Start), strings.Compare(a.Rule, b.Rule), strings.Compare(labelsText(a.Labels), labelsText(b.Labels)))
	})
	// The episodes numbered again in this order, as Judge numbers them in
	// its own: the order of first firings at one time would say which
	// names sort first.
	again := map[int]int{}
	for i, f := range out {
		if f.Episode == 0 {
			continue
		}
		n, ok := again[f.Episode]
		if !ok {
			n = len(again) + 1
			again[f.Episode] = n
		}
		out[i].Episode = n
	}
	return out
}

// Report is r with every name given its alias: a copy, r as it was.
func (x *Redactor) Report(r *Report) *Report {
	red, _ := x.ReportWith(r, nil)
	return red
}

// ReportWith is Report, and also, firings beside the report's (those
// quiet windows excused, which lapilli serve's CSV has), with the same
// aliases: their rules, labels and incidents numbered with the report's.
func (x *Redactor) ReportWith(r *Report, also []Judged) (*Report, []Judged) {
	all := *r
	all.Firings = append(slices.Clone(r.Firings), also...)
	x.prepare(nil, &all)
	return x.report(r), x.firings(also)
}

// Comparison is c with every name given its alias, alike on both sides: a
// copy, c as it was.
func (x *Redactor) Comparison(c *Comparison) *Comparison {
	x.prepare(c, c.Before, c.After)
	out := *c
	out.From, out.To = utc(c.From), utc(c.To)
	out.Before, out.After = x.report(c.Before), x.report(c.After)
	out.Rules = make([]RuleChange, len(c.Rules))
	for i, rc := range c.Rules {
		out.Rules[i] = RuleChange{Rule: x.alias("rule", rc.Rule), Before: x.ruleResultPtr(rc.Before), After: x.ruleResultPtr(rc.After)}
	}
	// As Compare orders them, those whose firings change most first; at a
	// tie, by alias, so no name decides it.
	moved := func(rc RuleChange) int {
		var b, a int
		if rc.Before != nil {
			b = rc.Before.Firings
		}
		if rc.After != nil {
			a = rc.After.Firings
		}
		return max(a-b, b-a)
	}
	slices.SortStableFunc(out.Rules, func(a, b RuleChange) int {
		return cmp.Or(cmp.Compare(moved(b), moved(a)), strings.Compare(a.Rule, b.Rule))
	})
	at := map[string]int{}
	for i, rc := range out.Rules {
		at[rc.Rule] = i
	}
	out.Incidents = make([]IncidentChange, len(c.Incidents))
	for i, ic := range c.Incidents {
		out.Incidents[i] = IncidentChange{Incident: x.incident(ic.Incident), Before: x.catches(ic.Before), After: x.catches(ic.After)}
	}
	slices.SortStableFunc(out.Incidents, func(a, b IncidentChange) int { return byStart(a.Incident, b.Incident) })
	// Those missed are of those caught differently: their aliases.
	if c.Missed != nil {
		out.Missed = make([]IncidentChange, len(c.Missed))
		for i, ic := range c.Missed {
			out.Missed[i] = IncidentChange{Incident: x.incident(ic.Incident), Before: x.catches(ic.Before), After: x.catches(ic.After)}
		}
		slices.SortStableFunc(out.Missed, func(a, b IncidentChange) int { return byStart(a.Incident, b.Incident) })
	}
	if c.MissedByDependency != nil {
		out.MissedByDependency = make([]IncidentChange, len(c.MissedByDependency))
		for i, ic := range c.MissedByDependency {
			out.MissedByDependency[i] = IncidentChange{Incident: x.incident(ic.Incident), Before: x.catches(ic.Before), After: x.catches(ic.After)}
		}
		slices.SortStableFunc(out.MissedByDependency, func(a, b IncidentChange) int { return byStart(a.Incident, b.Incident) })
	}
	if c.Recording != nil {
		out.Recording = make([]RecordingChange, len(c.Recording))
		for i, rc := range c.Recording {
			out.Recording[i] = RecordingChange{Record: x.alias("record", rc.Record), Group: x.alias("group", rc.Group), Change: rc.Change}
		}
		slices.SortFunc(out.Recording, func(a, b RecordingChange) int {
			return cmp.Or(strings.Compare(a.Record, b.Record), strings.Compare(a.Group, b.Group))
		})
	}
	if c.Expectations != nil {
		ex := *c.Expectations
		ex.Results = make([]ExpectResult, len(c.Expectations.Results))
		for i, r := range c.Expectations.Results {
			r.Rule = x.alias("rule", r.Rule)
			switch id, ok := strings.CutPrefix(r.What, "catches "); {
			case ok:
				r.What = "catches " + x.alias("incident", id)
			case strings.HasPrefix(r.What, ScenarioWhat):
				r.What = ScenarioWhat + x.alias("scenario", strings.TrimPrefix(r.What, ScenarioWhat))
			case r.What != "budget week" && r.What != "budget asleep":
				r.What = Redacted // none Lapilli writes: not shown as it is
			}
			// What promtool said names the rule, its labels, its series.
			if r.Said != "" {
				r.Said = Redacted
			}
			ex.Results[i] = r
		}
		// In the order of their rules, as they were made.
		slices.SortStableFunc(ex.Results, func(a, b ExpectResult) int { return cmp.Compare(at[a.Rule], at[b.Rule]) })
		ex.Unexpected = nil
		for _, name := range c.Expectations.Unexpected {
			ex.Unexpected = append(ex.Unexpected, x.alias("rule", name))
		}
		slices.Sort(ex.Unexpected)
		out.Expectations = &ex
	}
	// PassedOver is Lapilli's own words and counts: kept, a copy.
	out.PassedOver = maps.Clone(c.PassedOver)
	out.Unreplayed = x.unreplayed(c.Unreplayed)
	return &out
}

// response is a firing's response with the page's id aliased, and the
// labels of the change as labels are: no person is named in it.
func (x *Redactor) response(fr *FiringResponse) *FiringResponse {
	if fr == nil {
		return nil
	}
	out := *fr
	if fr.Page != nil {
		p := *fr.Page
		p.ID = x.alias("page", p.ID)
		p.Sent, p.Acknowledged, p.Resolved = utc(p.Sent), utc(p.Acknowledged), utc(p.Resolved)
		out.Page = &p
	}
	if fr.Change != nil {
		c := *fr.Change
		c.At = utc(c.At)
		c.Labels = x.labels(c.Labels)
		out.Change = &c
	}
	if fr.Useful != nil {
		u := *fr.Useful
		out.Useful = &u
	}
	return &out
}

// firingNotifications is a firing's notifications with the receivers, the
// inhibition rules, silences and time intervals that muted it, and the rule
// that inhibited it given their aliases.
func (x *Redactor) firingNotifications(n *FiringNotifications) *FiringNotifications {
	if n == nil {
		return nil
	}
	out := *n
	out.FirstPage = utc(n.FirstPage)
	if n.Sent != nil {
		out.Sent = make([]NotificationSent, len(n.Sent))
		for i, sn := range n.Sent {
			sn.At = utc(sn.At)
			sn.Receiver = x.alias("receiver", sn.Receiver)
			out.Sent[i] = sn
		}
	}
	out.Receivers = make([]string, len(n.Receivers))
	for i, name := range n.Receivers {
		out.Receivers[i] = x.alias("receiver", name)
	}
	if n.NoIntegrations != nil {
		out.NoIntegrations = make([]string, len(n.NoIntegrations))
		for i, name := range n.NoIntegrations {
			out.NoIntegrations[i] = x.alias("receiver", name)
		}
	}
	if n.Muted != nil {
		out.Muted = make([]NotificationMute, len(n.Muted))
		for i, m := range n.Muted {
			m.SourceRule = x.alias("rule", m.SourceRule)
			kind := kindOfMute(m.Kind)
			if kind == "interval" {
				parts := strings.Split(m.By, ",")
				for j, p := range parts {
					parts[j] = x.alias(kind, p)
				}
				m.By = strings.Join(parts, ",")
			} else {
				m.By = x.alias(kind, m.By)
			}
			out.Muted[i] = m
		}
	}
	return &out
}

// reportNotifications is what a report worked out of notifications with its
// receivers given their aliases.
func (x *Redactor) reportNotifications(n *ReportNotifications) *ReportNotifications {
	if n == nil {
		return nil
	}
	out := *n
	// A warning names a receiver or a policy in free text: left out, as the
	// reason a work-out stopped says no name.
	out.Warnings = nil
	out.Receivers = make([]NotificationReceiver, len(n.Receivers))
	for i, rc := range n.Receivers {
		rc.Name = x.alias("receiver", rc.Name)
		rc.Kinds = slices.Clone(rc.Kinds)
		out.Receivers[i] = rc
	}
	slices.SortStableFunc(out.Receivers, func(a, b NotificationReceiver) int { return strings.Compare(a.Name, b.Name) })
	return &out
}
