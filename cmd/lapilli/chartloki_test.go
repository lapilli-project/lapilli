package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The chart's Loki (docs/loki.md): its URL, tenant and a token from a
// Secret, mounted as a file; a Loki ruler's rules read from its API, or
// its rule files from ConfigMaps (one directory for serve, one flag each
// for the cronjob); the token Loki's ruler is written with, mounted only
// with changes on; and Grafana's history turned off. lapilli serve takes
// the flags the chart gives it.
func TestChartLoki(t *testing.T) {
	loki := []string{"--set", "mode=serve", "--set", "prometheus.url=http://prometheus:9090", "--set", "loki.url=http://loki:3100",
		"--set", "loki.tenant=acme", "--set", "loki.tokenSecret.name=loki-read"}
	loaded := append(append([]string{}, loki...), "--set", "rules.loki.loaded=true")
	signed := []string{"--set", "serve.userHeader=X-Forwarded-Email", "--set", "serve.networkPolicy.othersKeptOut=true",
		"--set", "serve.roles.admins={ana@example.com}"}
	changes := append(append(append([]string{}, loaded...), signed...), "--set", "serve.changes.enabled=true",
		"--set", "serve.changes.in={loki:shop}", "--set", "serve.changes.lokiTokenSecret.name=loki-write")
	m, _ := render(t, changes...)
	args := m.args(t)
	for _, want := range []string{"--loki=http://loki:3100", "--loki-tenant=acme", "--loki-token-file=/var/run/lapilli/loki/token",
		"--loki-loaded-rules", "--changes-in=loki:shop", "--changes-loki-token-file=/var/run/lapilli/changes-loki/token"} {
		if !slices.Contains(args, want) {
			t.Errorf("no %s in %v", want, args)
		}
	}
	mounts, vols := podFiles(m)
	if mounts["loki-token"] != "/var/run/lapilli/loki" || vols["loki-token"] != "secret loki-read" ||
		mounts["changes-loki"] != "/var/run/lapilli/changes-loki" || vols["changes-loki"] != "secret loki-write" {
		t.Errorf("the tokens: %v %v", mounts, vols)
	}
	// lapilli serve takes them, the tokens at the paths the chart mounts.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "read"), []byte("loki-reads-0123456789\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "write"), []byte("loki-writes-0123456789\n"), 0o600)
	var flags []string
	for _, a := range args[1:] {
		a = strings.Replace(a, "/var/run/lapilli/loki/token", filepath.Join(dir, "read"), 1)
		a = strings.Replace(a, "/var/run/lapilli/changes-loki/token", filepath.Join(dir, "write"), 1)
		flags = append(flags, a)
	}
	st, err := readServe(flags, true)
	if err != nil {
		t.Fatalf("serve refuses the chart's flags: %v\n%v", err, flags)
	}
	if c := st.cfg; c.Loki != "http://loki:3100" || c.LokiTenant != "acme" || !c.LokiLoaded || c.ChangesLokiToken != filepath.Join(dir, "write") {
		t.Errorf("as serve reads them: %+v", c)
	}
	// Changes off: no write token is mounted, and it is refused.
	if out, err := helmTemplate(t, append(append(append([]string{}, loaded...), signed...), "--set", "serve.changes.lokiTokenSecret.name=w")...); err == nil ||
		!strings.Contains(out, "go with serve.changes.enabled") {
		t.Errorf("a write token, changes off: %v\n%s", err, out)
	}

	// Rule files from ConfigMaps: one directory for serve, projected; a
	// flag each for lapilli verdict.
	m, _ = render(t, append(append([]string{}, loki...), "--set", "rules.loki.configMaps={logs,more}")...)
	if args := m.args(t); !slices.Contains(args, "--loki-rules=/etc/lapilli/loki-rules/0") || slices.Contains(args, "--loki-loaded-rules") ||
		slices.Contains(args, "--loki-rules=/etc/lapilli/loki-rules/1") {
		t.Errorf("serve's Loki rule files: %v", args)
	}
	if mounts, vols := podFiles(m); mounts["loki-rules-0"] != "/etc/lapilli/loki-rules/0" || vols["loki-rules-0"] != "projected logs,more" {
		t.Errorf("serve's Loki rule files: %v %v", mounts, vols)
	}
	out, err := helmTemplate(t, "--set", "prometheus.url=http://prometheus:9090", "--set", "impact=up", "--set", "loki.url=http://loki:3100",
		"--set", "rules.loki.configMaps={logs,more}")
	if err != nil || !strings.Contains(out, `- "--loki-rules=/etc/lapilli/loki-rules/0"`) || !strings.Contains(out, `- "--loki-rules=/etc/lapilli/loki-rules/1"`) ||
		!strings.Contains(out, "mountPath: /etc/lapilli/loki-rules/1") || !strings.Contains(out, "kind: CronJob") {
		t.Errorf("the cronjob's Loki rule files: %v\n%s", err, out)
	}

	// Through extraArgs, as before 0.70.0: still taken, alone.
	if m, _ := render(t, "--set", "mode=serve", "--set", "prometheus.url=http://prometheus:9090",
		"--set", "extraArgs={--loki,http://loki:3100,--loki-loaded-rules}"); !slices.Contains(m.args(t), "--loki-loaded-rules") {
		t.Errorf("through extraArgs: %v", m.args(t))
	}

	// Grafana's own history, off: --grafana-history=false, with Grafana's
	// rules alone.
	grafana := []string{"--set", "mode=serve", "--set", "prometheus.url=http://prometheus:9090", "--set", "rules.grafana.url=http://grafana:3000"}
	if m, _ := render(t, grafana...); slices.ContainsFunc(m.args(t), func(a string) bool { return strings.HasPrefix(a, "--grafana-history") }) {
		t.Errorf("on by default: %v", m.args(t))
	}
	for _, set := range []string{"--set", "--set-string"} { // false, or "false"
		if m, _ := render(t, append(append([]string{}, grafana...), set, "rules.grafana.history=false")...); !slices.Contains(m.args(t), "--grafana-history=false") {
			t.Errorf("%s rules.grafana.history=false: %v", set, m.args(t))
		}
	}

	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"rules, no URL":           {[]string{"--set", "prometheus.url=http://p", "--set", "impact=up", "--set", "rules.loki.loaded=true"}, "give loki.url"},
		"a tenant, no URL":        {append(append([]string{}, serveValues...), "--set", "loki.tenant=acme"), "go with loki.url"},
		"a tenant with a slash":   {append(append([]string{}, loaded...), "--set", "loki.tenant=a/b"), "a tenant's ID"},
		"both ways":               {append(append([]string{}, loaded...), "--set", "rules.loki.configMaps={logs}"), "two ways to the same rules"},
		"extraArgs too":           {append(append([]string{}, loaded...), "--set", "extraArgs={--loki-tenant,b}"), "not through extraArgs as well"},
		"history, no Grafana":     {append(append([]string{}, serveValues...), "--set", "rules.grafana.history=false"), "goes with rules.grafana.url"},
		"history, not a bool":     {append(append([]string{}, grafana...), "--set", "rules.grafana.history=off"), "rules.grafana.history: true or false"},
		"loki:, not loaded":       {append(append(append([]string{}, loki...), signed...), "--set", "rules.loki.configMaps={logs}", "--set", "serve.changes.enabled=true", "--set", "serve.changes.in={loki:shop}"), "set rules.loki.loaded"},
		"a write token, files":    {append(append(append([]string{}, loki...), signed...), "--set", "rules.loki.configMaps={logs}", "--set", "serve.changes.enabled=true", "--set", "serve.changes.lokiTokenSecret.name=w"), "lokiTokenSecret is for the rules of Loki's ruler"},
		"the read token to write": {append(append([]string{}, changes...), "--set", "serve.changes.lokiTokenSecret.name=loki-read"), "a token of their own"},
		"alarms named loki": {append(append([]string{}, changes...), "--set", "rules.alarms[0].source=loki",
			"--set", "rules.alarms[0].url=https://monitoring.eu-west-1.amazonaws.com"), "the source loki"},
	} {
		if out, err := helmTemplate(t, c.args...); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v, want %q\n%s", name, err, c.want, out)
		}
	}
}

// podFiles are the Deployment's read-only mounts, by name, and its
// volumes, each as its kind and what it holds.
func podFiles(m manifest) (map[string]string, map[string]string) {
	pod := dig(m["Deployment/ops-lapilli"], "spec", "template", "spec")
	mounts, vols := map[string]string{}, map[string]string{}
	for _, v := range dig(pod, "containers", 0, "volumeMounts").([]any) {
		if dig(v, "readOnly") == true {
			mounts[dig(v, "name").(string)] = dig(v, "mountPath").(string)
		}
	}
	for _, v := range dig(pod, "volumes").([]any) {
		name := dig(v, "name").(string)
		switch {
		case dig(v, "secret") != nil:
			vols[name] = "secret " + dig(v, "secret", "secretName").(string)
		case dig(v, "projected") != nil:
			var held []string
			for _, s := range dig(v, "projected", "sources").([]any) {
				if n, ok := dig(s, "configMap", "name").(string); ok {
					held = append(held, n)
				}
			}
			vols[name] = "projected " + strings.Join(held, ",")
		case dig(v, "configMap") != nil:
			vols[name] = "configMap " + dig(v, "configMap", "name").(string)
		}
	}
	return mounts, vols
}
