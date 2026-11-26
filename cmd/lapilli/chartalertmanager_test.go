package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lapilli.dev/internal/source"
)

// The chart's Alertmanager configuration (alertmanager,
// --alertmanager-config): a file from a Secret, as it often holds a
// receiver's keys, or a ConfigMap, mounted whole in one directory with
// the template files' ConfigMaps beside it, as templates: globs are read
// there; or a Mimir's or a Cortex's configuration API, read with
// rules.ruler's tenant and token, with rules.ruler.url or without it.
// lapilli serve takes the flags the chart gives it, and reads the files
// where the chart mounts them.
func TestChartAlertmanager(t *testing.T) {
	parallel(t)
	file := append(append([]string{}, serveValues...), "--set", "alertmanager.secret.name=alertmanager-main",
		"--set", "alertmanager.templateConfigMaps={am-templates,am-more}")
	m, _ := render(t, file...)
	args := m.args(t)
	want := "--alertmanager-config=/etc/lapilli/alertmanager/alertmanager.yaml"
	if !slices.Contains(args, want) {
		t.Errorf("no %s in %v", want, args)
	}
	pod := dig(m["Deployment/ops-lapilli"], "spec", "template", "spec")
	mounted := false
	for _, v := range dig(pod, "containers", 0, "volumeMounts").([]any) {
		if dig(v, "name") == "alertmanager" {
			mounted = dig(v, "mountPath") == "/etc/lapilli/alertmanager" && dig(v, "readOnly") == true && dig(v, "subPath") == nil
		}
	}
	if !mounted {
		t.Errorf("not mounted whole, read-only: %v", dig(pod, "containers", 0, "volumeMounts"))
	}
	var held []string
	for _, v := range dig(pod, "volumes").([]any) {
		if dig(v, "name") != "alertmanager" {
			continue
		}
		for _, s := range dig(v, "projected", "sources").([]any) {
			switch {
			case dig(s, "secret") != nil:
				held = append(held, "secret "+dig(s, "secret", "name").(string))
				if dig(s, "secret", "items") != nil {
					t.Errorf("the Secret's keys picked: its templates beside it aren't mounted")
				}
			case dig(s, "configMap") != nil:
				held = append(held, "configMap "+dig(s, "configMap", "name").(string))
			}
		}
	}
	if strings.Join(held, ",") != "secret alertmanager-main,configMap am-templates,configMap am-more" {
		t.Errorf("the volume holds %v", held)
	}

	// serve reads the configuration and its templates where the chart
	// mounts them: Alertmanager's own path for the templates, read beside
	// the file by its last element.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "alertmanager.yaml"), []byte("route: {receiver: team}\nreceivers:\n  - name: team\n    slack_configs:\n      - channel: '#ops'\n        title: '{{ template \"team.title\" . }}'\ntemplates: ['/etc/alertmanager/templates/*.tmpl']\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "team.tmpl"), []byte(`{{ define "team.title" }}{{ .CommonAnnotations.summary }}{{ end }}`), 0o600)
	var flags []string
	for _, a := range args[1:] {
		flags = append(flags, strings.Replace(a, "/etc/lapilli/alertmanager", dir, 1))
	}
	st, err := readServe(flags, true)
	if err != nil {
		t.Fatalf("serve refuses the chart's flags: %v\n%v", err, flags)
	}
	_, files, passed, err := source.AlertmanagerFiles(st.cfg.AlertmanagerConfig)
	if err != nil || len(files) != 1 || files[0].Name != "team.tmpl" || len(passed) > 0 {
		t.Errorf("as serve reads it: %v %v %v", err, files, passed)
	}

	// A ConfigMap, under a key of its own.
	m, _ = render(t, append(append([]string{}, serveValues...), "--set", "alertmanager.configMap.name=am",
		"--set", "alertmanager.configMap.key=alertmanager.yml")...)
	if args := m.args(t); !slices.Contains(args, "--alertmanager-config=/etc/lapilli/alertmanager/alertmanager.yml") {
		t.Errorf("a ConfigMap: %v", args)
	}
	if _, vols := podFiles(m); vols["alertmanager"] != "projected am" {
		t.Errorf("a ConfigMap: %v", vols)
	}

	// Mimir's API, with no ruler: the tenant and the token are rules.ruler's,
	// given for it alone.
	api := append(append([]string{}, serveValues...), "--set", "alertmanager.url=http://mimir:8080/api/v1/alerts",
		"--set", "rules.ruler.tenant=shop", "--set", "rules.ruler.tokenSecret.name=mimir-read")
	m, _ = render(t, api...)
	args = m.args(t)
	for _, want := range []string{"--alertmanager-config=http://mimir:8080/api/v1/alerts", "--ruler-tenant=shop", "--ruler-token-file=/var/run/lapilli/ruler/token"} {
		if !slices.Contains(args, want) {
			t.Errorf("no %s in %v", want, args)
		}
	}
	if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--ruler=") }) {
		t.Errorf("a ruler given: %v", args)
	}
	if mounts, vols := podFiles(m); mounts["ruler-token"] != "/var/run/lapilli/ruler" || vols["ruler-token"] != "secret mimir-read" || vols["alertmanager"] != "" {
		t.Errorf("the token: %v %v", mounts, vols)
	}
	tok := filepath.Join(dir, "token")
	os.WriteFile(tok, []byte("mimir-reads-0123456789\n"), 0o600)
	flags = nil
	for _, a := range args[1:] {
		flags = append(flags, strings.Replace(a, "/var/run/lapilli/ruler/token", tok, 1))
	}
	st, err = readServe(flags, true)
	if err != nil {
		t.Fatalf("serve refuses the chart's flags: %v\n%v", err, flags)
	}
	if c := st.cfg; c.AlertmanagerConfig != "http://mimir:8080/api/v1/alerts" || c.RulerTenant != "shop" || c.RulerToken != tok || c.Ruler != "" {
		t.Errorf("as serve reads them: %+v", c)
	}
	// With the ruler, on its host.
	ruler := append(append([]string{}, api...), "--set", "rules.ruler.url=http://mimir:8080/prometheus/config/v1/rules", "--set", "rules.configMaps=null")
	if m, _ := render(t, ruler...); !slices.Contains(m.args(t), "--ruler=http://mimir:8080/prometheus/config/v1/rules") {
		t.Errorf("with the ruler: %v", m.args(t))
	}
	// None given, none of it.
	for _, set := range [][]string{{"--set", "alertmanager.url=", "--set", "rules.ruler.tenant=", "--set", "rules.ruler.tokenSecret.name="}} {
		if m, _ := render(t, append(append([]string{}, api...), set...)...); slices.ContainsFunc(m.args(t), func(a string) bool {
			return strings.HasPrefix(a, "--alertmanager-config") || strings.HasPrefix(a, "--ruler")
		}) {
			t.Errorf("none given: %v", m.args(t))
		}
	}
	// In cronjob mode it is given to lapilli verdict, and mounted: who a
	// firing reached is worked out from it.
	cron := []string{"--set", "prometheus.url=http://prometheus:9090", "--set", "impact=up", "--set", "rules.configMaps={rules}",
		"--set", "alertmanager.secret.name=am"}
	if out, err := helmTemplate(t, cron...); err != nil || !strings.Contains(out, "--alertmanager-config=/etc/lapilli/alertmanager/alertmanager.yaml") ||
		!strings.Contains(out, "mountPath: /etc/lapilli/alertmanager") || !strings.Contains(out, "name: \"am\"") {
		t.Errorf("cronjob mode: %v\n%s", err, out)
	}
	// With the content check off, serve reads it for who a firing reached
	// alone: it is still passed and mounted.
	if m, _ := render(t, append(append([]string{}, file...), "--set", "serve.checkContent.enabled=false")...); !slices.Contains(m.args(t), "--check-content=false") ||
		!slices.Contains(m.args(t), "--alertmanager-config=/etc/lapilli/alertmanager/alertmanager.yaml") {
		t.Errorf("the check off: %v", m.args(t))
	}
	// The renamed key still works: serve.checkContent.alertmanager and scrapeInterval.
	if out, err := helmTemplate(t, append(append([]string{}, serveValues...), "--set", "serve.checkContent.alertmanager.secret.name=am",
		"--set", "serve.checkContent.scrapeInterval=30s")...); err != nil || !strings.Contains(out, "--alertmanager-config=/etc/lapilli/alertmanager/alertmanager.yaml") ||
		!strings.Contains(out, `"--scrape-interval=30s"`) {
		t.Errorf("the old keys: %v\n%s", err, out)
	}

	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"two ways": {append(append([]string{}, file...), "--set", "alertmanager.url=http://mimir:8080/api/v1/alerts"),
			"give one of secret, configMap and url"},
		"templates alone": {append(append([]string{}, serveValues...), "--set", "alertmanager.templateConfigMaps={t}"),
			"give secret or configMap"},
		"both off":     {append(append([]string{}, file...), "--set", "serve.checkContent.enabled=false", "--set", "notifications.workedOut=false"), "nothing to read it for"},
		"cronjob, off": {append(append([]string{}, cron...), "--set", "notifications.workedOut=false"), "nothing to read it for"},
		"a user":       {append(append([]string{}, serveValues...), "--set", "alertmanager.url=http://ops@mimir:8080/api/v1/alerts"), "with no user, query or anchor"},
		"a query":      {append(append([]string{}, serveValues...), "--set", "alertmanager.url=http://mimir:8080/api/v1/alerts?x=1"), "with no user, query or anchor"},
		"in capitals":  {append(append([]string{}, serveValues...), "--set", "alertmanager.url=HTTP://mimir:8080/api/v1/alerts"), "want a Mimir's or a Cortex's"},
		"a key's path": {append(append([]string{}, serveValues...), "--set", "alertmanager.secret.name=am",
			"--set", "alertmanager.secret.key=../am.yaml"), "a Secret's or a ConfigMap's key"},
		"another host": {append(append([]string{}, ruler...), "--set", "alertmanager.url=http://alertmanager:9093/api/v1/alerts"),
			"isn't on rules.ruler.url's scheme and host"},
		"a tenant, no URL": {append(append([]string{}, serveValues...), "--set", "rules.ruler.tenant=shop"),
			"for rules.ruler.url or alertmanager.url"},
	} {
		if out, err := helmTemplate(t, c.args...); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v, want %q\n%s", name, err, c.want, out)
		}
	}
}
