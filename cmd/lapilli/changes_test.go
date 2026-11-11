package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Changes are refused, before lapilli serve starts, without what makes
// approval a known person's, or with what would make two tokens one.
func TestServeChangesFlags(t *testing.T) {
	hook := write(t, "hook", "https://hooks.example/x")
	tok := write(t, "tok", "changes-token-0123456789\n")
	other := write(t, "other", "changes-token-0123456789\n")
	short := write(t, "short", "x\n")
	signed := []string{"--user-header", "X-Forwarded-Email", "--admins", "ana@example.com"}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no sign-in":            {[]string{"--changes"}, "--changes needs --user-header"},
		"no roles":              {[]string{"--changes", "--user-header", "X-Forwarded-Email"}, "--changes needs roles, --editors or --admins"},
		"approvers, no roles":   {[]string{"--changes", "--user-header", "X-Forwarded-Email", "--approvers", "ana@example.com", "--mcp-compare", "editors"}, "--changes needs roles"}, // refused later if let through: never served
		"no one to approve":     {[]string{"--changes", "--user-header", "X-Forwarded-Email", "--editors", "bo@example.com"}, "--changes needs --approvers or --admins"},
		"turned off":            {[]string{"--changes=false", "--approvers", "ana@example.com"}, "go with --changes"},
		"turned on":             {[]string{"--changes=true"}, "--changes needs --user-header"},
		"neither":               {[]string{"--changes=maybe"}, "--changes=maybe: want true or false"},
		"read-only":             {append([]string{"--changes", "--read-only"}, signed...), "--read-only refuses them"},
		"three approvals":       {append([]string{"--changes", "--approvals", "3"}, signed...), "--approvals 3"},
		"approvers, no changes": {append([]string{"--approvers", "ana@example.com"}, signed...), "go with --changes"},
		"bad limits":            {append([]string{"--changes", "--changes-limits", "5"}, signed...), "--changes-limits"},
		"a short token":         {append([]string{"--changes", "--incidents-listen", "127.0.0.1:0", "--changes-token-file", short}, signed...), "16 characters"},
		"the token twice":       {append([]string{"--changes", "--incidents-listen", "127.0.0.1:0", "--changes-token-file", tok, "--incidents-token-file", other}, signed...), "same token"},
		"no port for it":        {append([]string{"--changes", "--changes-token-file", tok}, signed...), "go with --incidents-listen"},
		"Grafana's, no Grafana": {append([]string{"--changes", "--changes-grafana-token-file", tok}, signed...), "--grafana"},
		"an agent, no roles":    {[]string{"--mcp-propose", "editors"}, "--mcp-propose needs"},
		"an agent, everyone":    {[]string{"--mcp-propose", "everyone"}, "want editors or admins"},
	} {
		var out, errs strings.Builder
		if code := run(append([]string{"serve", "--prometheus", "http://127.0.0.1:1", "--rules", hook}, c.args...), &out, &errs); code != 2 || !strings.Contains(errs.String(), c.want) {
			t.Errorf("%s: %d, want 2 and %q\n%s", name, code, c.want, errs.String())
		}
	}
	// With --editors, no one is an admin who isn't named: approvers will do.
	var out, errs strings.Builder
	run([]string{"serve", "--prometheus", "http://127.0.0.1:1", "--rules", hook, "--changes", "--user-header", "X-Forwarded-Email",
		"--editors", "bo@example.com", "--approvers", "ana@example.com,cy@example.com", "--listen", "256.0.0.1:1"}, &out, &errs)
	if strings.Contains(errs.String(), "--changes needs") {
		t.Errorf("editors and approvers, no admins: %s", errs.String())
	}
}

// The chart turns changes on only with admins, and the ConfigMap that
// pauses them is the one its docs name, for a release named ops as for
// one named lapilli. It needs helm.
func TestChartChanges(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm")
	}
	chart := filepath.Join("..", "..", "charts", "lapilli")
	template := func(release string, more ...string) (string, error) {
		args := append([]string{"template", release, chart, "--set", "mode=serve", "--set", "prometheus.url=http://prometheus:9090",
			"--set", "rules.configMaps={rules}", "--set", "serve.changes.enabled=true", "--set", "serve.userHeader=X-Forwarded-Email",
			"--set", "serve.networkPolicy.othersKeptOut=true"}, more...)
		cmd := exec.Command("helm", args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(t.TempDir(), "none"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := template("ops", "--set", "serve.changes.approvers={ana@example.com}"); err == nil || !strings.Contains(out, "serve.changes needs roles") {
		t.Errorf("changes with approvers and no roles: %v\n%s", err, out)
	}
	if out, err := template("ops", "--set", "serve.roles.editors={bo@example.com}"); err == nil || !strings.Contains(out, "serve.changes needs serve.changes.approvers or serve.roles.admins") {
		t.Errorf("changes with editors and no one to approve: %v\n%s", err, out)
	}
	if out, err := template("ops", "--set", "serve.roles.editors={bo@example.com}", "--set", "serve.changes.approvers={ana@example.com}"); err != nil {
		t.Errorf("changes with editors and approvers, no admins: %v\n%s", err, out)
	}
	readme, err := os.ReadFile(filepath.Join(chart, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	values, err := os.ReadFile(filepath.Join(chart, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, release := range []string{"ops", "lapilli"} {
		out, err := template(release, "--set", "serve.roles.admins={ana@example.com}")
		if err != nil {
			t.Fatalf("%s: %v\n%s", release, err, out)
		}
		name := map[string]string{"ops": "ops-lapilli-changes-paused", "lapilli": "lapilli-changes-paused"}[release]
		if !strings.Contains(out, "name: "+name+"\n") {
			t.Errorf("%s: the pod doesn't mount %s", release, name)
		}
		for doc, b := range map[string][]byte{"README.md": readme, "values.yaml": values} {
			if !strings.Contains(string(b), name) {
				t.Errorf("%s doesn't name %s, the ConfigMap a release named %s pauses with", doc, name, release)
			}
		}
	}
}
