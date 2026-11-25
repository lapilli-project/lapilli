package content

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/routing"
)

// A made up Alertmanager configuration that uses what routing reads, and
// holds secrets and addresses that it must not (and need not) read.
const routingYAML = `
global:
  resolve_timeout: 5m
  slack_api_url: https://hooks.example.com/services/T000/B000/XXXX
route:
  receiver: default
  group_by: [alertname, cluster]
  group_wait: 20s
  group_interval: 1m
  repeat_interval: 1h30m
  routes:
    - matchers: ['team="db"', 'severity=~"page|critical"']
      receiver: db-pager
      continue: true
      group_by: ['...']
      mute_time_intervals: [lunch, weekends]
    - match:
        team: web
      match_re:
        env: "prod.*"
      receiver: web
      group_by: []
      repeat_interval: 30m
      active_time_intervals: [office]
      routes:
        - matchers: ['{severity="info", cluster!="x"}']
          receiver: quiet
receivers:
  - name: default
    slack_configs:
      - api_url: https://hooks.example.com/services/T111/B111/YYYY
        channel: '#all'
  - name: db-pager
    pagerduty_configs:
      - routing_key: example-routing-key-0000
      - routing_key: example-routing-key-1111
        send_resolved: false
    webhook_configs:
      - url: http://hooks.example.com/db
        http_config:
          authorization:
            credentials: example-token
    email_configs:
      - to: db@example.com
        auth_password: example-password
        send_resolved: true
  - name: web
    opsgenie_configs:
      - api_key: example-key
    msteamsv2_configs:
      - webhook_url: https://example.com/hook
  - name: quiet
time_intervals:
  - name: lunch
    time_intervals:
      - times:
          - start_time: '12:00'
            end_time: '13:00'
        location: Asia/Seoul
  - name: office
    time_intervals:
      - weekdays: ['monday:friday']
        times: [{start_time: '09:00', end_time: '17:00'}]
mute_time_intervals:
  - name: weekends
    time_intervals:
      - weekdays: [saturday, sunday]
inhibit_rules:
  - name: nodes
    source_matchers: ['alertname="NodeDown"']
    target_matchers: ['alertname=~"Pod.*"']
    equal: [node]
  - source_match: {severity: critical}
    source_match_re: {team: "a|b"}
    target_match: {severity: warning}
    target_match_re: {env: "dev.*"}
    equal: [cluster, namespace]
`

func TestReadAlertmanagerRouting(t *testing.T) {
	c, err := ReadAlertmanagerRouting([]byte(routingYAML))
	if err != nil {
		t.Fatal(err)
	}
	rs := c.Routes()
	if len(rs) != 4 {
		t.Fatalf("%d routes", len(rs))
	}
	// Each route as it works: what it says, over what it inherits.
	root, db, web, quiet := rs[0], rs[1], rs[2], rs[3]
	if root.Receiver != "default" || !slices.Equal(root.GroupBy, []string{"alertname", "cluster"}) || root.GroupWait != 20*time.Second ||
		root.GroupInterval != time.Minute || root.RepeatInterval != 90*time.Minute {
		t.Errorf("root: %+v", root)
	}
	if db.Receiver != "db-pager" || !db.GroupByAll || db.GroupWait != 20*time.Second || db.RepeatInterval != 90*time.Minute ||
		!slices.Equal(db.MuteTimeIntervals, []string{"lunch", "weekends"}) {
		t.Errorf("db: %+v", db)
	}
	if web.Receiver != "web" || web.GroupByAll || len(web.GroupBy) != 0 || web.RepeatInterval != 30*time.Minute || web.GroupInterval != time.Minute ||
		!slices.Equal(web.ActiveTimeIntervals, []string{"office"}) {
		t.Errorf("web: %+v", web)
	}
	if quiet.Receiver != "quiet" || quiet.RepeatInterval != 30*time.Minute || len(quiet.ActiveTimeIntervals) != 0 || len(quiet.GroupBy) != 0 {
		t.Errorf("quiet: %+v", quiet)
	}
	// Receivers: kinds in Alertmanager's order, each kind's index, and
	// send_resolved by its own say or its kind's default.
	r, _ := c.Receiver("db-pager")
	want := []routing.Integration{ig("email", 0, true), ig("pagerduty", 0, true), ig("pagerduty", 1, false), ig("webhook", 0, true)}
	if !slices.Equal(r.Integrations, want) {
		t.Errorf("db-pager: %+v", r.Integrations)
	}
	if d, _ := c.Receiver("default"); !slices.Equal(d.Integrations, []routing.Integration{ig("slack", 0, false)}) {
		t.Errorf("default: %+v", d.Integrations)
	}
	if w, _ := c.Receiver("web"); !slices.Equal(w.Integrations, []routing.Integration{ig("opsgenie", 0, true), ig("msteamsv2", 0, true)}) {
		t.Errorf("web: %+v", w.Integrations)
	}
	if q, ok := c.Receiver("quiet"); !ok || len(q.Integrations) != 0 {
		t.Errorf("quiet: %+v", q)
	}
	// Both the intervals and the older top-level mute_time_intervals are
	// read, the zone applied: noon in Seoul is 03:00 UTC.
	alert := func(start time.Time, kv ...string) routing.Alert {
		a := routing.Alert{Labels: map[string]string{}, Start: start}
		for i := 0; i+1 < len(kv); i += 2 {
			a.Labels[kv[i]] = kv[i+1]
		}
		return a
	}
	mon := time.Date(2026, 1, 5, 2, 0, 0, 0, time.UTC) // Monday, 11:00 in Seoul
	res, err := c.Run([]routing.Alert{
		alert(mon, "alertname", "Disk", "team", "db", "severity", "page"),
		alert(time.Date(2026, 1, 10, 2, 0, 0, 0, time.UTC), "alertname", "Disk", "team", "db", "severity", "page"), // Saturday
	}, routing.Options{Until: mon.Add(7 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	first := func(i int) time.Time {
		if len(res.Alerts[i].Notifications) == 0 {
			return time.Time{}
		}
		return res.Notifications[res.Alerts[i].Notifications[0]].At
	}
	if got := first(0); !got.Equal(mon.Add(20 * time.Second)) {
		t.Errorf("a weekday morning: %v", got)
	}
	// Saturday at 11:00 in Seoul is a weekend, muted until Monday 00:00 UTC.
	if got, want := first(1), time.Date(2026, 1, 12, 0, 0, 20, 0, time.UTC); !got.Equal(want) && !got.After(want) {
		t.Errorf("a weekend: %v", got)
	}
	if got := res.Alerts[1].Muted; len(got) == 0 || got[0].Kind != routing.MutedMuteTime || got[0].By != "weekends" {
		t.Errorf("muted on the weekend: %+v", got)
	}
	// Four integrations on the db route, of which one doesn't send resolves
	// and the first is the page.
	if res.Alerts[0].Notified < 4 || res.Alerts[0].Paged < 2 {
		t.Errorf("notified %d paged %d", res.Alerts[0].Notified, res.Alerts[0].Paged)
	}
}

// What Alertmanager won't load isn't read: and said, not guessed.
func TestReadAlertmanagerRoutingErrors(t *testing.T) {
	for what, cfg := range map[string]string{
		"no route":                   "receivers: [{name: a}]",
		"an undefined receiver":      "route: {receiver: x}\nreceivers: [{name: a}]",
		"an undefined time interval": "route: {receiver: a, mute_time_intervals: [nope]}\nreceivers: [{name: a}]",
		"a matcher that isn't":       "route: {receiver: a, routes: [{matchers: ['team ~ db']}]}\nreceivers: [{name: a}]",
		"a regular expression":       "route: {receiver: a, match_re: {team: '('}}\nreceivers: [{name: a}]",
		"a duration that isn't":      "route: {receiver: a, group_wait: soon}\nreceivers: [{name: a}]",
		"a zero group_interval":      "route: {receiver: a, group_interval: 0s}\nreceivers: [{name: a}]",
		"... and another":            "route: {receiver: a, group_by: ['...', alertname]}\nreceivers: [{name: a}]",
		"a time interval, twice":     "route: {receiver: a}\nreceivers: [{name: a}]\ntime_intervals: [{name: x, time_intervals: [{}]}]\nmute_time_intervals: [{name: x, time_intervals: [{}]}]",
		"a wrong time":               "route: {receiver: a}\nreceivers: [{name: a}]\ntime_intervals: [{name: x, time_intervals: [{times: [{start_time: '9:00', end_time: '10:00'}]}]}]",
		"an inhibition matcher":      "route: {receiver: a}\nreceivers: [{name: a}]\ninhibit_rules: [{source_matchers: ['a ! b'], target_matchers: ['a=b']}]",
		"not a map":                  "- a",
	} {
		if _, err := ReadAlertmanagerRouting([]byte(cfg)); err == nil {
			t.Errorf("read %s", what)
		}
	}
	// ReadAlertmanager still reads the templates of one it can't route, and
	// says why.
	n, _, err := ReadAlertmanager([]byte("route: {receiver: a}\nreceivers: [{name: b}]"), nil)
	if err != nil || n.Routing() != nil || n.RoutingErr() == nil || !strings.Contains(n.RoutingErr().Error(), "receiver") {
		t.Errorf("%v %v %v", n, err, n.RoutingErr())
	}
	// And one it can route has it.
	if n := amTest(t); n.Routing() == nil || len(n.Routing().Routes()) != 5 {
		t.Errorf("the test configuration's routing: %v %v", n.Routing(), n.RoutingErr())
	}
}

// Inhibition rules, as written either way.
func TestReadInhibitRules(t *testing.T) {
	c, err := ReadAlertmanagerRouting([]byte(routingYAML))
	if err != nil {
		t.Fatal(err)
	}
	alert := func(start time.Time, kv ...string) routing.Alert {
		a := routing.Alert{Labels: map[string]string{}, Start: start}
		for i := 0; i+1 < len(kv); i += 2 {
			a.Labels[kv[i]] = kv[i+1]
		}
		return a
	}
	at := time.Date(2026, 1, 5, 2, 0, 0, 0, time.UTC)
	res, err := c.Run([]routing.Alert{
		alert(at, "alertname", "NodeDown", "node", "n1"),
		alert(at, "alertname", "PodCrash", "node", "n1"), // by the first rule
		alert(at, "alertname", "PodCrash", "node", "n2"), // another node
		alert(at, "alertname", "Big", "severity", "critical", "team", "a", "cluster", "c", "namespace", "x"),
		alert(at, "alertname", "Small", "severity", "warning", "env", "dev1", "cluster", "c", "namespace", "x"), // by the second
		alert(at, "alertname", "Small", "severity", "warning", "env", "dev1", "cluster", "c", "namespace", "y"),
		alert(at, "alertname", "Small", "severity", "warning", "env", "prod", "cluster", "c", "namespace", "x"),
	}, routing.Options{Until: at.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for i, by := range []string{"", "nodes", "", "", "#2", "", ""} {
		got := ""
		if m := res.Alerts[i].Muted; len(m) > 0 {
			got = m[0].By
		}
		if got != by {
			t.Errorf("alert %d: muted by %q, want %q", i, got, by)
		}
	}
}

// Grafana's: contact points by type, policies' grouping, timings and
// matchers, and mute timings by name.
func TestReadGrafanaRouting(t *testing.T) {
	const contactPoints = `[
	  {"uid": "a", "name": "oncall-team", "type": "oncall", "settings": {"url": "https://example.com/secret-url"}},
	  {"uid": "b", "name": "oncall-team", "type": "slack", "disableResolveMessage": true, "settings": {"token": "xoxb-example"}},
	  {"uid": "c", "name": "oncall-team", "type": "slack", "settings": {}},
	  {"uid": "d", "name": "email", "type": "email", "settings": {"addresses": "a@example.com"}}
	]`
	const policies = `{"receiver": "email", "group_by": ["grafana_folder", "alertname"], "group_wait": "10s", "group_interval": "2m", "repeat_interval": "6h",
	  "routes": [
	    {"receiver": "oncall-team", "object_matchers": [["team", "=", "db"], ["grafana_folder", "=~", "prod.*"]], "continue": true,
	     "group_by": ["..."], "mute_time_intervals": ["nights"], "repeat_interval": "1h",
	     "routes": [{"receiver": "email", "matchers": ["severity=\"info\""], "active_time_intervals": ["nights"]}]},
	    {"receiver": "email", "match": {"team": "web"}, "match_re": {"env": "p.*"}, "group_by": []}
	  ]}`
	const mutes = `[{"name": "nights", "version": "x", "provenance": "api", "time_intervals": [{"times": [{"start_time": "00:00", "end_time": "06:00"}], "weekdays": ["monday:friday"], "location": "UTC"}]}]`
	c, err := ReadGrafanaRouting([]byte(contactPoints), []byte(policies), []byte(mutes))
	if err != nil {
		t.Fatal(err)
	}
	rs := c.Routes()
	if len(rs) != 4 {
		t.Fatalf("%d routes", len(rs))
	}
	if r := rs[1]; r.Receiver != "oncall-team" || !r.GroupByAll || r.GroupWait != 10*time.Second || r.GroupInterval != 2*time.Minute || r.RepeatInterval != time.Hour ||
		!slices.Equal(r.MuteTimeIntervals, []string{"nights"}) {
		t.Errorf("route 1: %+v", r)
	}
	if r := rs[2]; r.Receiver != "email" || !slices.Equal(r.ActiveTimeIntervals, []string{"nights"}) || r.RepeatInterval != time.Hour {
		t.Errorf("route 2: %+v", r)
	}
	if r := rs[3]; len(r.GroupBy) != 0 || r.GroupByAll || r.RepeatInterval != 6*time.Hour {
		t.Errorf("route 3: %+v", r)
	}
	if r, _ := c.Receiver("oncall-team"); !slices.Equal(r.Integrations, []routing.Integration{ig("oncall", 0, true), ig("slack", 0, false), ig("slack", 1, true)}) {
		t.Errorf("oncall-team: %+v", r.Integrations)
	}
	// An alert at ten on a Monday, as Grafana's carry the folder: the db
	// policy's group is held to its own timings, and the integrations page
	// by Grafana's kinds (oncall does).
	at := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	res, err := c.Run([]routing.Alert{{Labels: map[string]string{"alertname": "A", "team": "db", "grafana_folder": "prod-db"}, Start: at}},
		routing.Options{Until: at.Add(30 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	a := res.Alerts[0]
	if a.Notified != 3 || a.Paged != 1 || !slices.Equal(a.Receivers, []string{"oncall-team"}) {
		t.Errorf("%+v", a)
	}
	for _, n := range res.Notifications {
		if n.Reason != routing.ReasonFirst || !n.At.Equal(at.Add(10*time.Second)) || n.Page != (n.Kind == "oncall") {
			t.Errorf("%+v", n)
		}
	}
	// Policies that name mute timings need them read.
	if _, err := ReadGrafanaRouting([]byte(contactPoints), []byte(policies), nil); err == nil {
		t.Error("policies with mute timings that weren't read")
	}
	for what, p := range map[string]string{
		"a policy for a contact point that isn't": `{"receiver": "nobody"}`,
		"a duration that isn't":                   `{"receiver": "email", "group_wait": "later"}`,
		"a matcher that isn't":                    `{"receiver": "email", "routes": [{"matchers": ["a b c"]}]}`,
	} {
		if _, err := ReadGrafanaRouting([]byte(contactPoints), []byte(p), nil); err == nil {
			t.Errorf("read %s", what)
		}
	}
}

// ig is an integration.
func ig(kind string, index int, send bool) routing.Integration {
	return routing.Integration{Kind: kind, Index: index, SendResolved: send}
}
