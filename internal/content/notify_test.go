package content

import (
	"os"
	"slices"
	"testing"

	"lapilli.dev/internal/rules"
)

// grafana13 is what a real Grafana 13.3.1 answered (testdata/grafana-13.3.1,
// made by its setup.sh): its rules, and its templates, contact points and
// policies.
func grafana13(t *testing.T) ([]rules.Rule, *Notifications) {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile("testdata/grafana-13.3.1/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	rs, err := rules.FromDocs([]rules.Doc{{Source: "grafana", Data: read("alert-rules.yaml")}})
	if err != nil {
		t.Fatal(err)
	}
	n, skipped, err := ReadNotifications(read("templates.json"), read("contact-points.json"), read("policies.json"))
	if err != nil || len(skipped) != 0 {
		t.Fatalf("%v %v", skipped, err)
	}
	return rs, n
}

func ruleNamed(t *testing.T, rs []rules.Rule, name string) rules.Rule {
	t.Helper()
	i := slices.IndexFunc(rs, func(r rules.Rule) bool { return r.Name == name })
	if i < 0 {
		t.Fatalf("no rule %s", name)
	}
	return rs[i]
}

// Each rule's notifications are written with the templates every way its
// routing can go reaches: its own contact point, or the policies', by the
// labels it sets, its alertname and folder; a label it doesn't set may be
// its series', so both ways are followed.
func TestNotifyRouting(t *testing.T) {
	rs, n := grafana13(t)
	for name, want := range map[string][]string{
		// team=shop: shop-hook, its title and message.
		"ShopErrors": {"shop-hook/0", "shop-hook/1", "shop.message", "shop.title"},
		// service=payments: shop-hook, and, its severity unknown,
		// plain-hook too, or not: shop-hook's are sent either way.
		"PayLatency": {"shop-hook/0", "shop-hook/1", "shop.message", "shop.title"},
		// severity=info: shop-hook if its series carry team=shop or
		// service=pay*, plain-hook otherwise: only shop.title is both's.
		"Disk": {"shop.title"},
		// notification_settings: shop-hook.
		"Routed": {"shop-hook/0", "shop-hook/1", "shop.message", "shop.title"},
	} {
		r := ruleNamed(t, rs, name)
		var got []string
		for _, u := range n.For(r) {
			got = append(got, u.Template)
		}
		// The contact point's own fields show nothing: For leaves them out.
		want = slices.DeleteFunc(slices.Clone(want), func(s string) bool { return s == "shop-hook/0" || s == "shop-hook/1" })
		if !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if r := ruleNamed(t, rs, "Routed"); r.Receiver != "shop-hook" || r.Folder != "shop" {
		t.Errorf("Routed: receiver %q, folder %q", r.Receiver, r.Folder)
	}
}

// What the templates show is checked as a rule's annotations are: an
// annotation shown that the rule hasn't (unless guarded by an if), a label
// named that its alerts never had (unless the rule sets it, or Grafana
// gives every alert it).
func TestNotifyChecks(t *testing.T) {
	rs, n := grafana13(t)
	seen := map[string]map[string]bool{
		"ShopErrors": {"alertname": true, "grafana_folder": true, "team": true, "severity": true, "pod": true},
		"Disk":       {"alertname": true, "severity": true},
	}
	o := Options{Notify: n, Seen: func(rule string) (map[string]bool, bool) {
		m, ok := seen[rule]
		return m, ok
	}}
	for name, want := range map[string][]string{
		"ShopErrors": {NotifyLabelNeverSeen + `:"shop.title": cluster`},
		"PayLatency": {NoRunbook, NoSeverity, NotifyNoText + `:"shop.message": summary`},
		"Disk":       {NoText, NoRunbook, NotifyLabelNeverSeen + `:"shop.title": cluster`},
		"Routed":     {NoRunbook, NoSeverity},
	} {
		one(t, ruleNamed(t, rs, name), o, want...)
	}
	// A rule whose alerts had cluster: nothing.
	seen["Disk"]["cluster"] = true
	one(t, ruleNamed(t, rs, "Disk"), o, NoText, NoRunbook)
	// Without Grafana's notifications, as before.
	one(t, ruleNamed(t, rs, "PayLatency"), Options{}, NoRunbook, NoSeverity)
}

// A notification template names labels and annotations as fields of the
// notification, of an alert in it, by index, through a variable; guarded
// where an if or a with names them; including others in turn.
func TestNotifyTemplates(t *testing.T) {
	defs, err := notifyTemplates(`
{{ define "a" }}{{ .CommonLabels.cluster }} {{ $.GroupLabels.region }} {{ index .CommonLabels "zone" }}
{{ range .Alerts.Firing }}{{ .Labels.pod }} {{ .Annotations.summary }}{{ end }}
{{ if .CommonAnnotations.runbook_url }}{{ .CommonAnnotations.runbook_url }}{{ end }}
{{ with .CommonLabels.namespace }}{{ . }}{{ else }}{{ .CommonLabels.node }}{{ end }}
{{ .CommonLabels.SortedPairs.Names }} {{ template "b" . }}{{ end }}
{{ define "b" }}{{ .CommonAnnotations.description | reReplaceAll "x" "y" | myOwnFunc }}{{ end }}`)
	if err != nil {
		t.Fatal(err)
	}
	var labels, notes []string
	for _, l := range defs["a"].labels {
		if !l.guarded {
			labels = append(labels, l.label)
		}
	}
	for _, l := range defs["a"].annotations {
		if !l.guarded {
			notes = append(notes, l.label)
		}
	}
	if !slices.Equal(labels, []string{"cluster", "region", "zone", "pod", "node"}) || !slices.Equal(notes, []string{"summary"}) ||
		!slices.Equal(defs["a"].includes, []string{"b"}) || len(defs["b"].annotations) != 1 || defs["b"].annotations[0].label != "description" {
		t.Errorf("labels %q, annotations %q, includes %q, b %+v", labels, notes, defs["a"].includes, defs["b"])
	}
	if _, err := notifyTemplates(`{{ define "x" }}{{ .CommonLabels.x }{{ end }}`); err == nil {
		t.Error("a template that doesn't parse")
	}
	// Answers that aren't Grafana's.
	if _, _, err := ReadNotifications([]byte(`{}`), []byte(`[]`), []byte(`{}`)); err == nil {
		t.Error("templates that aren't a list")
	}
	n, skipped, err := ReadNotifications([]byte(`[{"name": "bad", "template": "{{ define \"x\" }}{{ end"}]`), []byte(`[]`), []byte(`{"receiver": "r"}`))
	if err != nil || len(skipped) != 1 || n == nil {
		t.Errorf("a template that doesn't parse is passed over: %v %v", skipped, err)
	}
}

// Routing with matchers written as text, a regular expression that doesn't
// compile, and a tree too wide to follow.
func TestNotifyRoutes(t *testing.T) {
	n, _, err := ReadNotifications([]byte(`[{"name": "t", "template": "{{ define \"t\" }}{{ .CommonLabels.x }}{{ end }}"}]`),
		[]byte(`[{"name": "a", "settings": {"text": "{{ template \"t\" . }}"}}, {"name": "b", "settings": {"text": "plain"}}]`),
		[]byte(`{"receiver": "b", "routes": [{"receiver": "a", "matchers": ["team = \"shop\"", "env=~prod|stage"]}, {"receiver": "a", "matchers": ["bad=~("]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	r := rules.Rule{Name: "R", Says: rules.Says{Template: rules.TemplateGrafana}, Labels: map[string]string{"team": "shop", "env": "prod"}}
	if got := n.For(r); len(got) != 1 || got[0].Template != "t" {
		t.Errorf("team=shop env=prod: %+v", got)
	}
	r.Labels["env"] = "dev"
	if got := n.For(r); len(got) != 0 {
		t.Errorf("env=dev, maybe to a by an unreadable matcher, maybe not: %+v", got)
	}
	// Not a Grafana rule: none.
	r.Says.Template = rules.TemplatePrometheus
	r.Labels["env"] = "prod"
	if got := n.For(r); got != nil {
		t.Errorf("a Prometheus rule: %+v", got)
	}
	// Too many ways: none, rather than a guess.
	wide := `{"receiver": "b", "routes": [`
	for i := range 10 {
		if i > 0 {
			wide += ","
		}
		wide += `{"receiver": "a", "continue": true, "object_matchers": [["l` + string(rune('0'+i)) + `", "=", "x"]]}`
	}
	wide += `]}`
	n, _, _ = ReadNotifications([]byte(`[]`), []byte(`[]`), []byte(wide))
	if _, ok := n.route.outcomes(map[string]string{}, "", 0); ok {
		t.Error("2^10 ways followed")
	}
}
