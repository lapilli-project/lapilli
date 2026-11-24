package content

import (
	"slices"
	"strings"
	"testing"

	"lapilli.dev/internal/rules"
)

// notifies are a rule's findings of its notifications' templates.
func notifies(fs [][]Finding) []string {
	var out []string
	for _, s := range checks(fs, 0) {
		if strings.HasPrefix(s, "notify_") {
			out = append(out, s)
		}
	}
	return out
}

func grafanaRuleIn(name string, folder string, labels map[string]string, summary string) rules.Rule {
	r := promRule(name, labels, map[string]string{"summary": summary})
	r.Says.Template = rules.TemplateGrafana
	r.Folder = folder
	return r
}

// A Grafana rule sent to an external Alertmanager is checked against its
// routes and templates, as a ruler's rule is; not when Grafana doesn't
// send there (GrafanaExternal false).
func TestGrafanaRulesSentToAlertmanager(t *testing.T) {
	n := amTest(t)
	g := grafanaRuleIn("G", "", map[string]string{"severity": "page", "team": "db", "service": "db"}, "")
	want := []string{NotifyNoText + `:"shop.text": summary`}
	got := notifies(Check([]rules.Rule{g}, Options{Alertmanager: n, GrafanaExternal: true}))
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := notifies(Check([]rules.Rule{g}, Options{Alertmanager: n})); len(got) != 0 {
		t.Errorf("Grafana not sending there: %q", got)
	}
	// A Prometheus rule is checked as before.
	p := promRule("P", map[string]string{"severity": "page", "team": "db", "service": "db"}, nil)
	if got := notifies(Check([]rules.Rule{p}, Options{Alertmanager: n, GrafanaExternal: true})); !slices.Equal(got, want) {
		t.Errorf("a Prometheus rule: %q", got)
	}
}

// Grafana gives its alerts a grafana_folder label, to an external
// Alertmanager too: a route on it is followed; a ruler's rule has none.
func TestGrafanaRulesSentToAlertmanagerFolder(t *testing.T) {
	n, _, err := ReadAlertmanager([]byte(`route:
  receiver: all
  routes:
    - matchers: ['grafana_folder="Shop"']
      receiver: shop
receivers:
  - name: all
  - name: shop
    slack_configs:
      - text: '{{ .CommonAnnotations.runbook_url }}'`), nil)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Alertmanager: n, GrafanaExternal: true}
	in := grafanaRuleIn("G", "Shop", map[string]string{"severity": "page"}, "x")
	if got := notifies(Check([]rules.Rule{in}, o)); len(got) != 1 || got[0][:len(NotifyNoText)] != NotifyNoText {
		t.Errorf("in the folder: %q", got)
	}
	out := grafanaRuleIn("G", "Other", map[string]string{"severity": "page"}, "x")
	if got := notifies(Check([]rules.Rule{out}, o)); len(got) != 0 {
		t.Errorf("in another folder: %q", got)
	}
}

// Sent to both, a Grafana rule is checked against Grafana's own templates
// and Alertmanager's.
func TestGrafanaRulesSentToBoth(t *testing.T) {
	am := amTest(t)
	graf, _, err := ReadNotifications([]byte(`[{"name":"gt","template":"{{ define \"gt\" }}{{ .CommonAnnotations.description }}{{ end }}"}]`),
		[]byte(`[{"name":"c","type":"slack","settings":{"text":"{{ template \"gt\" . }}"}}]`), []byte(`{"receiver":"c"}`))
	if err != nil {
		t.Fatal(err)
	}
	g := grafanaRuleIn("G", "", map[string]string{"severity": "page", "team": "db", "service": "db"}, "")
	got := notifies(Check([]rules.Rule{g}, Options{Notify: graf, Alertmanager: am, GrafanaExternal: true}))
	if len(got) != 2 {
		t.Errorf("both: %q", got)
	}
}
