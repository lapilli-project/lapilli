package content

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lapilli.dev/internal/rules"
)

// amTest is the configuration in testdata/alertmanager, with its two
// template files, read as Alertmanager loads them.
func amTest(t *testing.T) *Notifications {
	t.Helper()
	cfg, err := os.ReadFile("testdata/alertmanager/alertmanager.yml")
	if err != nil {
		t.Fatal(err)
	}
	var files []TemplateFile
	for _, name := range []string{"defaults.tmpl", "shop.tmpl"} {
		b, err := os.ReadFile(filepath.Join("testdata/alertmanager/templates", name))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, TemplateFile{Name: "templates/" + name, Text: b})
	}
	n, skipped, err := ReadAlertmanager(cfg, files)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("%q %v", skipped, err)
	}
	return n
}

// promRule is a rule as a Prometheus rule file has it.
func promRule(name string, labels map[string]string, annotations map[string]string) rules.Rule {
	r := rules.Rule{Name: name, Expr: "up == 0", Labels: labels, Says: rules.Says{Template: rules.TemplatePrometheus, Annotations: map[string]string{}}}
	for k, v := range annotations {
		switch k {
		case "summary":
			r.Says.Summary = v
		case "description":
			r.Says.Description = v
		default:
			r.Says.Annotations[k] = v
			// As rules reads it: runbook_url, or else runbook.
			if k == "runbook_url" || k == "runbook" && r.Says.Runbook == "" {
				r.Says.Runbook = v
			}
		}
	}
	return r
}

// A rule's alerts go where the route tree sends them, by the labels the
// rule sets: matchers as lines, in braces or not, match and match_re,
// continue, the default receiver; a label it doesn't set may be its
// series', so both ways are followed, and a template counts only when
// every way uses it.
func TestAlertmanagerRouting(t *testing.T) {
	n := amTest(t)
	everyone := []string{"shop.footer", "shop.text", "slack.default.title"}
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   []string
	}{
		// team=shop: shop-pager, and on (continue) to the routes after,
		// which may or may not match; or, with service=pay-api, certainly
		// shop-pager again.
		{"shop", map[string]string{"team": "shop", "service": "pay-api", "env": "prod"}, []string{"shop-pager/pagerduty_configs[0].description",
			"shop-pager/pagerduty_configs[0].details.dashboard"}},
		// Nothing a route matches on: everyone's, or any route's.
		{"none", map[string]string{}, nil},
		// severity=info alone, team and service not set: quiet's, unless its
		// series carry team or service.
		{"info", map[string]string{"severity": "info", "team": "db", "service": "db"}, nil},
		// Every route ruled out: everyone's.
		{"db", map[string]string{"severity": "page", "team": "db", "service": "db"}, everyone},
		// match_re: web's; its Slack title doesn't parse; its webhook's
		// URL isn't a template, and isn't read.
		{"web", map[string]string{"severity": "page", "team": "edge", "service": "db"}, []string{"web/slack_configs[0].title"}},
		// env=dev rules the second route out.
		{"dev", map[string]string{"severity": "page", "team": "db", "service": "pay-api", "env": "dev"}, everyone},
	} {
		var got []string
		for _, u := range n.For(promRule(c.name, c.labels, nil)) {
			got = append(got, u.Template)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// Not a rule Alertmanager is sent: a Grafana rule, a cloud's alarm.
	g := promRule("g", map[string]string{"team": "db"}, nil)
	g.Says.Template = rules.TemplateGrafana
	a := promRule("a", map[string]string{"team": "db"}, nil)
	a.Cloud = "cloudwatch"
	if n.For(g) != nil || n.For(a) != nil {
		t.Error("a Grafana rule, or a cloud's alarm, sent with Alertmanager's templates")
	}
}

// What the templates show is checked as Grafana's are: an annotation
// shown the rule hasn't, any of them, a label its alerts never had or its
// expression can't give; and a template that doesn't parse, which
// Alertmanager fails on when it sends.
func TestAlertmanagerChecks(t *testing.T) {
	n := amTest(t)
	seen := map[string]map[string]bool{"Fired": {"alertname": true, "instance": true, "cluster": true}}
	o := Options{Alertmanager: n, Seen: func(rule string) (map[string]bool, bool) {
		m, ok := seen[rule]
		return m, ok
	}}
	check := func(r rules.Rule, want ...string) {
		t.Helper()
		var got []string
		for _, s := range checks(Check([]rules.Rule{r}, o), 0) {
			if strings.HasPrefix(s, "notify_") {
				got = append(got, s)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", r.Name, got, want)
		}
	}
	db := map[string]string{"severity": "page", "team": "db", "service": "db"}
	// Fired: everyone's; region never seen; no summary.
	check(promRule("Fired", db, nil),
		NotifyNoText+`:"shop.text": summary`, NotifyLabelNeverSeen+`:"slack.default.title": region`)
	// With a summary, and its alerts with region: nothing.
	seen["Fired"]["region"] = true
	check(promRule("Fired", db, map[string]string{"summary": "down"}))
	// Not fired: the labels its aggregation can't give, guessed.
	r := promRule("Sum", db, map[string]string{"summary": "down"})
	r.Expr = `sum by (cluster) (rate(errors_total[5m])) > 1`
	check(r, NotifyLabelNotGiven+`:"shop.text": instance`, NotifyLabelNotGiven+`:"slack.default.title": region`)
	// A label the rule sets is never faulted.
	r.Labels = map[string]string{"severity": "page", "team": "db", "service": "db", "instance": "all", "region": "eu"}
	check(r)
	// Web's: its title doesn't parse.
	check(promRule("Web", map[string]string{"severity": "page", "team": "web", "service": "db"}, nil),
		NotifyTemplateError+`:"web/slack_configs[0].title": line 1: unexpected "}" in operand`)
	// The shop's pager: a summary and cluster, the dashboard guarded.
	shop := map[string]string{"team": "shop", "service": "pay-api", "env": "prod"}
	check(promRule("Shop", shop, map[string]string{"summary": "slow"}))
	check(promRule("Shop", shop, nil), NotifyNoText+`:"shop-pager/pagerduty_configs[0].description": summary`)
}

// An annotation is looked for by its own name, any of them, where the
// rule's are all known; a runbook in runbook isn't runbook_url's.
func TestAlertmanagerAnnotations(t *testing.T) {
	cfg := []byte(`route: {receiver: r}
receivers:
  - name: r
    slack_configs:
      - text: '{{ .CommonAnnotations.runbook_url }} {{ .CommonAnnotations.dashboard }}'`)
	n, _, err := ReadAlertmanager(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Alertmanager: n}
	r := promRule("R", map[string]string{"severity": "page"}, map[string]string{"summary": "x", "runbook": "https://runbooks.example/r"})
	got := checks(Check([]rules.Rule{r}, o), 0)
	want := []string{NotifyNoText + `:"r/slack_configs[0].text": runbook_url`, NotifyNoText + `:"r/slack_configs[0].text": dashboard`}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	// Past the annotations kept, one not among them isn't known missing.
	r.Says.AnnotationsCut = true
	if got := checks(Check([]rules.Rule{r}, o), 0); len(got) != 0 {
		t.Errorf("annotations cut: %q", got)
	}
}

// A template file that doesn't parse: Alertmanager won't load the
// configuration, and a rule sent with a template it defines is told so.
func TestAlertmanagerBrokenFile(t *testing.T) {
	cfg := []byte(`route: {receiver: r}
receivers:
  - name: r
    slack_configs:
      - text: '{{ template "team.text" . }}'
  - name: other
    slack_configs:
      - text: '{{ template "other.text" . }}'`)
	n, skipped, err := ReadAlertmanager(cfg, []TemplateFile{
		{Name: "team.tmpl", Text: []byte("{{ define \"team.text\" }}\n{{ .CommonLabels.x }\n{{ end }}")},
		{Name: "other.tmpl", Text: []byte(`{{ define "other.text" }}{{ .CommonLabels.y }}{{ end }}`)},
	})
	if err != nil || len(skipped) != 1 || !strings.Contains(skipped[0], "team.tmpl: line 2:") || !strings.Contains(skipped[0], "won't load") {
		t.Fatalf("%q %v", skipped, err)
	}
	got := checks(Check([]rules.Rule{promRule("R", map[string]string{"severity": "page"}, map[string]string{"summary": "x"})}, Options{Alertmanager: n}), 0)
	if len(got) != 2 || !strings.HasPrefix(got[1], NotifyTemplateError+`:"team.text": team.tmpl: line 2: `) {
		t.Errorf("%q", got)
	}
}

// What isn't read: a field whose name says it is a secret or where a
// request goes, files past the bounds.
func TestAlertmanagerBounds(t *testing.T) {
	cfg := []byte(`route: {receiver: r}
receivers:
  - name: r
    email_configs:
      - to: '{{ .CommonLabels.team }}@example.com'
        auth_password: '{{ secret'
        auth_secret: '{{ .CommonLabels.leak }}'
    webhook_configs:
      - url: 'https://hooks.example.com/{{ broken'
        http_config:
          authorization:
            credentials: '{{ also'`)
	n, _, err := ReadAlertmanager(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := n.receivers["r"]; !slices.Equal(got, []string{"r/email_configs[0].to"}) {
		t.Errorf("fields read: %q", got)
	}
	big := TemplateFile{Name: "big.tmpl", Text: []byte(`{{ define "big" }}` + strings.Repeat("x", MaxTemplateFile) + `{{ end }}`)}
	many := make([]TemplateFile, MaxTemplateFiles+1)
	for i := range many {
		many[i] = TemplateFile{Name: fmt.Sprint(i), Text: []byte(`{{ define "t" }}{{ end }}`)}
	}
	for name, files := range map[string][]TemplateFile{"too large": {big}, "too many": many} {
		if _, skipped, err := ReadAlertmanager(cfg, files); err != nil || len(skipped) != 1 {
			t.Errorf("%s: %q %v", name, skipped, err)
		}
	}
	for _, bad := range []string{`{}`, `route: [x]`, `- a`, `route: {receiver: r}` + "\n" + strings.Repeat("x: [", 10)} {
		if _, _, err := ReadAlertmanager([]byte(bad), nil); err == nil {
			t.Errorf("%q read as Alertmanager's configuration", bad)
		}
	}
}

// Matchers as Alertmanager reads a line of them.
func TestAlertmanagerMatchers(t *testing.T) {
	for line, want := range map[string]string{
		`severity="critical"`:                   `[[severity = critical]]`,
		`{team=~"shop|pay", env!="dev"}`:        `[[team =~ shop|pay] [env != dev]]`,
		`foo = bar`:                             `[[foo = bar]]`,
		`msg="a, b", x!~"y"`:                    `[[msg = a, b] [x !~ y]]`,
		`"service.name"="checkout"`:             `[[service.name = checkout]]`,
		`quoted="with \"quotes\""`:              `[[quoted = with "quotes"]]`,
		`not a matcher`:                         `[]`,
		`x="open`:                               `[]`,
		`{severity="critical",}`:                `[[severity = critical]]`,
		`alertname=~"Disk.*",instance!~"db-.*"`: `[[alertname =~ Disk.*] [instance !~ db-.*]]`,
	} {
		ms, _ := amMatchers(line)
		var got []string
		for _, m := range ms {
			got = append(got, m[0]+" "+m[1]+" "+m[2])
		}
		g := "["
		for i, s := range got {
			if i > 0 {
				g += " "
			}
			g += "[" + s + "]"
		}
		g += "]"
		if g != want {
			t.Errorf("%s: %s, want %s", line, g, want)
		}
	}
}

// A receiver of many integrations, beside templates that define many of
// their defaults, follows each default once, and so many at most: not a
// receiver's every default once for each of its integrations.
func TestAlertmanagerDefaultsBounded(t *testing.T) {
	const n = 2500
	var cfg, tmpl strings.Builder
	cfg.WriteString("route: {receiver: r}\nreceivers:\n  - name: r\n    slack_configs:\n")
	for i := range n {
		cfg.WriteString("      - {}\n")
		fmt.Fprintf(&tmpl, `{{ define "slack.default.f%d" }}{{ .CommonLabels.x }}{{ end }}`, i)
	}
	got, skipped, err := ReadAlertmanager([]byte(cfg.String()), []TemplateFile{{Name: "d.tmpl", Text: []byte(tmpl.String())}})
	if err != nil {
		t.Fatal(err)
	}
	used := got.receivers["r"]
	seen := map[string]bool{}
	for _, d := range used {
		if seen[d] {
			t.Fatalf("%s followed twice", d)
		}
		seen[d] = true
	}
	if len(used) != maxDefaults || len(skipped) != 1 || !strings.Contains(skipped[0], "slack's defaults") {
		t.Errorf("%d templates followed, %q", len(used), skipped)
	}
}
