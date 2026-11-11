package content

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
)

// What two reviews found in the notification templates' check, each held
// here (docs/design/serveops.md, "What the reviews found").

func reviewFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/grafana-13.3.1/review/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// grafanaRule is a Grafana rule of one Prometheus query, sum by (job), and
// the labels given.
func grafanaRule(t *testing.T, labelsYAML string) rules.Rule {
	t.Helper()
	doc := `apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: shop
    interval: 1m
    rules:
      - uid: u1
        title: ShopErrors
        condition: B
        data:
          - refId: A
            datasourceUid: prom
            relativeTimeRange: {from: 600, to: 0}
            model: {refId: A, expr: 'sum by (job) (rate(http_errors_total[5m]))', instant: true}
          - refId: B
            datasourceUid: __expr__
            model: {refId: B, type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [1]}}]}
        for: 1m
        annotations: {summary: errors}
        labels:
` + labelsYAML
	rs, err := rules.FromDocs([]rules.Doc{{Source: "grafana", Data: []byte(doc)}})
	if err != nil || len(rs) != 1 {
		t.Fatalf("%v %d", err, len(rs))
	}
	return rs[0]
}

func usedBy(n *Notifications, r rules.Rule) []string {
	var out []string
	for _, u := range n.For(r) {
		out = append(out, u.Template)
	}
	return out
}

// A policy tree a real Grafana 13.3.1 accepted and gave back, 1,993
// policies, three levels, routes a rule in well under a second: what a
// child gives is worked out once, not once for each way that reaches it,
// and a way's contact points are a set.
func TestPolicyTreeCost(t *testing.T) {
	n, _, err := ReadNotifications([]byte(`[]`), []byte(`[{"name": "plain-hook", "settings": {"message": "{{ .CommonLabels.cluster }}"}}]`),
		reviewFile(t, "policies-wide.json"))
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := grafana13(t)
	began := time.Now()
	for _, name := range []string{"Disk", "ShopErrors", "PayLatency"} {
		// Every way goes to plain-hook, whose message is a template.
		if got := usedBy(n, ruleNamed(t, rs, name)); len(got) != 1 || got[0] != "plain-hook/0" {
			t.Errorf("%s: %q", name, got)
		}
	}
	if took := time.Since(began); took > 2*time.Second*time.Duration(slowFactor()) {
		t.Errorf("three rules routed in %s", took)
	}
	// Built: 64 ways at each of two levels, each to contact points of its
	// own, the inner level under every outer way, and two thousand
	// policies matching every alert.
	maybe := func(i int) map[string]any {
		return map[string]any{"receiver": fmt.Sprint("r", i), "object_matchers": [][]string{{fmt.Sprint("unknown", i), "=", "x"}}, "continue": true}
	}
	all := func() map[string]any {
		return map[string]any{"receiver": "plain-hook", "object_matchers": [][]string{{"alertname", "=~", ".*"}}, "continue": true}
	}
	var inner, outer []map[string]any
	for i := range 6 {
		inner, outer = append(inner, maybe(i)), append(outer, maybe(10+i))
	}
	for range 1980 {
		inner = append(inner, all())
	}
	// One level: 64 ways, each through the 1,980, routed.
	policies, _ := json.Marshal(map[string]any{"receiver": "plain-hook", "routes": inner})
	if n, _, err = ReadNotifications([]byte(`[]`), []byte(`[]`), policies); err != nil {
		t.Fatal(err)
	}
	began = time.Now()
	if steps := maxSteps; !routes(n, ruleNamed(t, rs, "ShopErrors"), &steps) {
		t.Error("64 ways through 1,986 policies weren't routed")
	}
	if took := time.Since(began); took > time.Second*time.Duration(slowFactor()) {
		t.Errorf("64 ways through 1,986 policies routed in %s", took)
	}
	// Two: the inner under each of 64 outer ways, too many ways to tell,
	// told quickly.
	r := all()
	r["routes"] = inner
	outer = append(outer, r)
	policies, _ = json.Marshal(map[string]any{"receiver": "plain-hook", "routes": outer})
	if n, _, err = ReadNotifications([]byte(`[]`), []byte(`[]`), policies); err != nil {
		t.Fatal(err)
	}
	began = time.Now()
	n.For(ruleNamed(t, rs, "ShopErrors"))
	if took := time.Since(began); took > time.Second*time.Duration(slowFactor()) {
		t.Errorf("a two-level tree of 1,993 policies routed one rule in %s", took)
	}
	// More work than a rule's routing may take: given up on, not guessed,
	// and quickly. 64 ways, each through 280 policies, each of which may
	// go 64 ways again.
	many := slices.Clone(outer[:6])
	for range 280 {
		r := all()
		r["receiver"], r["routes"] = "r10", outer[:6] // its ways among the 64 sets of r10 to r15
		many = append(many, r)
	}
	policies, _ = json.Marshal(map[string]any{"receiver": "plain-hook", "routes": many})
	if n, _, err = ReadNotifications([]byte(`[]`), []byte(`[]`), policies); err != nil {
		t.Fatal(err)
	}
	began = time.Now()
	if steps := maxSteps; routes(n, ruleNamed(t, rs, "ShopErrors"), &steps) {
		t.Error("routed past the steps a rule may take")
	}
	if took := time.Since(began); took > time.Second*time.Duration(slowFactor()) {
		t.Errorf("given up on in %s", took)
	}
}

// raceOn is set under the race detector, which slows everything.
var raceOn bool

// routes reports whether a rule's routing is told.
func routes(n *Notifications, r rules.Rule, steps *int) bool {
	known := map[string]string{"alertname": r.AlertName(), "grafana_folder": r.Folder}
	for k, v := range r.Labels {
		known[k] = v
	}
	check := maxCheckSteps
	b := &budget{rule: *steps, check: &check}
	_, why := n.route.outcomes(known, 0, len(n.names), 0, b)
	*steps = b.rule
	return why == ""
}

// slowFactor stretches a budget under the race detector.
func slowFactor() int {
	if raceOn {
		return 10
	}
	return 1
}

// Alertmanager's older match and match_re, which Grafana 13.3.1 keeps and
// gives back as they were written, match as = and =~.
func TestLegacyMatchers(t *testing.T) {
	tmpl := `[{"name": "s", "template": "{{ define \"s.msg\" }}on {{ .CommonLabels.cluster }} {{ .CommonAnnotations.runbook_url }}{{ end }}"}]`
	cps := `[{"name": "shop", "settings": {"message": "{{ template \"s.msg\" . }}"}}, {"name": "root", "settings": {"message": "{{ len .Alerts }}"}}]`
	n, _, err := ReadNotifications([]byte(tmpl), []byte(cps), reviewFile(t, "policies-legacy.json"))
	if err != nil {
		t.Fatal(err)
	}
	for labels, want := range map[string]string{
		"          team: billing\n          severity: page\n          env: dev\n          svc: cart\n": "",
		"          team: shop\n":                       "s.msg",
		"          svc: payments\n          team: x\n": "s.msg",
		"          env: prod\n          team: x\n":     "s.msg",
	} {
		if got := strings.Join(usedBy(n, grafanaRule(t, labels)), " "); got != want {
			t.Errorf("%q: %q, want %q", labels, got, want)
		}
	}
}

// A rule's label written as a template, as Grafana 13.3.1 exports it
// ('{{ $$labels.team }}'), is filled in from the series: a policy on it
// can go either way.
func TestTemplatedLabelRouting(t *testing.T) {
	rs, err := rules.FromDocs([]rules.Doc{{Source: "grafana", Data: reviewFile(t, "alert-rules-templated.yaml")}})
	if err != nil || len(rs) != 1 || !strings.Contains(rs[0].Labels["team"], "{{") {
		t.Fatalf("%+v %v", rs, err)
	}
	tmpl := `[{"name": "root", "template": "{{ define \"root.msg\" }}Runbook: {{ .CommonAnnotations.runbook_url }}{{ end }}"},
		{"name": "shop", "template": "{{ define \"shop.msg\" }}{{ len .Alerts }} alerts{{ end }}"}]`
	cps := `[{"name": "root", "settings": {"message": "{{ template \"root.msg\" . }}"}}, {"name": "shop", "settings": {"message": "{{ template \"shop.msg\" . }}"}}]`
	n, _, err := ReadNotifications([]byte(tmpl), []byte(cps), []byte(`{"receiver": "root", "routes": [{"receiver": "shop", "object_matchers": [["team", "=", "shop"]]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := usedBy(n, rs[0]); len(got) != 0 {
		t.Errorf("a team label filled in from the series may be shop's: %q", got)
	}
	if fs := Check(rs, Options{Notify: n}); strings.Contains(fmt.Sprint(fs[0]), NotifyNoText) {
		t.Errorf("held to root.msg's runbook: %+v", fs[0])
	}
}

// A label a template names that the rule's one query can't give is
// guessed at for a Grafana rule that hasn't fired: its query, not its
// expression, which a Grafana rule hasn't.
func TestNotifyHeuristicReachable(t *testing.T) {
	tmpl := `[{"name": "c", "template": "{{ define \"c.msg\" }}on {{ .CommonLabels.cluster }} for {{ .CommonLabels.job }}{{ end }}"}]`
	cps := `[{"name": "root", "settings": {"message": "{{ template \"c.msg\" . }}"}}]`
	n, _, err := ReadNotifications([]byte(tmpl), []byte(cps), []byte(`{"receiver": "root"}`))
	if err != nil {
		t.Fatal(err)
	}
	r := grafanaRule(t, "          severity: page\n")
	if got := checks(Check([]rules.Rule{r}, Options{Notify: n}), 0); strings.Join(got, " ") != NoRunbook+" "+NotifyLabelNotGiven+`:"c.msg": cluster` {
		t.Errorf("%q", got)
	}
}

// Past the policies read, 2,000 or 32 levels, a rule that may reach the
// rest isn't routed: no template is held against it.
func TestPolicyTreeCut(t *testing.T) {
	tmpl := `[{"name": "root", "template": "{{ define \"root.msg\" }}Runbook: {{ .CommonAnnotations.runbook_url }}{{ end }}"},
		{"name": "shop", "template": "{{ define \"shop.msg\" }}{{ len .Alerts }} on {{ .CommonLabels.cluster }}{{ end }}"}]`
	cps := `[{"name": "root", "settings": {"message": "{{ template \"root.msg\" . }}"}}, {"name": "deep", "settings": {"message": "{{ template \"shop.msg\" . }}"}},
		{"name": "shop", "settings": {"message": "{{ template \"shop.msg\" . }}"}}]`
	var wide strings.Builder
	wide.WriteString(`{"receiver": "root", "routes": [`)
	for i := range 2100 {
		if i > 0 {
			wide.WriteString(",")
		}
		fmt.Fprintf(&wide, `{"receiver": "shop", "object_matchers": [["n", "=", "%d"]]}`, i)
	}
	wide.WriteString(`]}`)
	n, _, err := ReadNotifications([]byte(tmpl), []byte(cps), []byte(wide.String()))
	if err != nil {
		t.Fatal(err)
	}
	if got := usedBy(n, grafanaRule(t, "          n: '2050'\n")); len(got) != 0 {
		t.Errorf("past 2,000 policies: %q", got)
	}
	if got := usedBy(n, grafanaRule(t, "          n: '5'\n")); len(got) != 1 || got[0] != "shop.msg" {
		t.Errorf("before them: %q", got)
	}
	var deep strings.Builder
	deep.WriteString(`{"receiver": "root", "routes": [`)
	for i := range 34 {
		recv := ""
		if i == 33 {
			recv = "deep"
		}
		fmt.Fprintf(&deep, `{"receiver": %q, "object_matchers": [["severity", "=", "page"]], "routes": [`, recv)
	}
	deep.WriteString(strings.Repeat(`]}`, 34) + `]}`)
	if n, _, err = ReadNotifications([]byte(tmpl), []byte(cps), []byte(deep.String())); err != nil {
		t.Fatal(err)
	}
	if got := usedBy(n, grafanaRule(t, "          severity: page\n")); len(got) != 0 {
		t.Errorf("past 32 levels: %q", got)
	}
}

// or falls back: {{ or .CommonLabels.cluster "unknown" }} never shows
// nothing, nor does what is piped into or.
func TestOrFallback(t *testing.T) {
	defs, err := notifyTemplates(`{{ define "a" }}{{ or .CommonLabels.cluster "unknown" }} {{ .CommonAnnotations.summary | or "none" }} {{ .CommonLabels.pod }}{{ end }}`)
	if err != nil {
		t.Fatal(err)
	}
	var open []string
	for _, l := range defs["a"].labels {
		if !l.guarded {
			open = append(open, l.label)
		}
	}
	for _, a := range defs["a"].annotations {
		if !a.guarded {
			open = append(open, a.label)
		}
	}
	if strings.Join(open, " ") != "pod" {
		t.Errorf("unguarded: %q", open)
	}
}

// A tree a real Grafana 13.3.1 accepted and gave back, built to cost the
// most a rule can (1,697 policies: a hundred that match every alert, each
// to a contact point of its own, then 228 that may, each with six that may
// again): each rule's routing stops at its budget, the work charged by the
// words of its contact points, and all the rules of a check stop at the
// check's, so 500 rules take well under the time pages wait; past a time
// given, none is routed. Each rule given up says why (a verification of
// the fixes found 500 rules taking twelve minutes).
func TestPolicyTreeBudget(t *testing.T) {
	rs, _ := grafana13(t)
	r := ruleNamed(t, rs, "ShopErrors")
	var many []rules.Rule
	for i := range 500 {
		x := r
		x.Name = fmt.Sprint("R", i)
		many = append(many, x)
	}
	for _, f := range []string{"policies-1697.json", "policies-1697-long-names.json"} {
		n, _, err := ReadNotifications([]byte(`[]`), reviewFile(t, "contact-points-1697.json"), reviewFile(t, f))
		if err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		if steps := maxSteps; routes(n, r, &steps) {
			t.Errorf("%s: routed past a rule's budget", f)
		}
		if took := time.Since(began); took > 200*time.Millisecond*time.Duration(slowFactor()) {
			t.Errorf("%s: one rule given up on in %s", f, took)
		}
		why := map[string]int{}
		began = time.Now()
		Check(many, Options{Notify: n, Unchecked: func(_, w string) { why[w]++ }})
		if took := time.Since(began); took > 2*time.Second*time.Duration(slowFactor()) {
			t.Errorf("%s: 500 rules checked in %s", f, took)
		}
		if why[WhySteps] == 0 || why[WhyAllSteps] == 0 || why[WhySteps]+why[WhyAllSteps] != 500 {
			t.Errorf("%s: unchecked %v", f, why)
		}
		// Past the time given: none routed, each says so.
		clear(why)
		Check(many[:3], Options{Notify: n, NotifyBy: time.Now().Add(-time.Second), Unchecked: func(_, w string) { why[w]++ }})
		if why[WhyTime] != 3 {
			t.Errorf("%s: past the time given, %v", f, why)
		}
	}
	// Rules alike in the labels the policies match on are routed once.
	n, _, _ := ReadNotifications([]byte(`[]`), []byte(`[{"name": "a", "settings": {"message": "{{ .CommonLabels.x }}"}}]`),
		[]byte(`{"receiver": "a", "routes": [{"receiver": "a", "object_matchers": [["team", "=", "shop"]]}]}`))
	x := n.run(time.Time{})
	once := 0
	for i := range 100 {
		r := r
		r.Name = fmt.Sprint("R", i)
		if uses, why := x.For(r); len(uses) != 1 || why != "" {
			t.Fatalf("%q %+v", why, uses)
		}
		if i == 0 {
			once = maxCheckSteps - x.steps
		}
	}
	if len(x.kept) != 1 || once == 0 || maxCheckSteps-x.steps != once {
		t.Errorf("100 rules alike: %d kept, %d steps, %d for one", len(x.kept), maxCheckSteps-x.steps, once)
	}
}
