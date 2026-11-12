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
		"no sign-in":            {[]string{"--changes"}, "--changes needs a sign-in"},
		"no roles":              {[]string{"--changes", "--user-header", "X-Forwarded-Email"}, "--changes needs roles, --editors or --admins"},
		"approvers, no roles":   {[]string{"--changes", "--user-header", "X-Forwarded-Email", "--approvers", "ana@example.com", "--mcp-compare", "editors"}, "--changes needs roles"},                                        // refused later if let through: never served
		"no one to approve":     {[]string{"--changes", "--user-header", "X-Forwarded-Email", "--editors", "bo@example.com", "--listen", "256.0.0.1:1", "--ledger", t.TempDir()}, "--changes needs --approvers or --admins"}, // fails to listen if let through
		"turned off":            {[]string{"--changes=false", "--approvers", "ana@example.com"}, "go with --changes"},
		"turned on":             {[]string{"--changes=true"}, "--changes needs a sign-in"},
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
		"--editors", "bo@example.com", "--approvers", "ana@example.com,cy@example.com", "--listen", "256.0.0.1:1", "--ledger", t.TempDir()}, &out, &errs)
	if strings.Contains(errs.String(), "--changes needs") || !strings.Contains(errs.String(), "--listen") {
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

// The chart reads a ruler with rules.ruler and changes rules in it with
// serve.changes: the write credentials mounted only with changes on, each
// for its own kind of ruler. It needs helm.
func TestChartRulers(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm")
	}
	chart := filepath.Join("..", "..", "charts", "lapilli")
	template := func(more ...string) (string, error) {
		args := append([]string{"template", "ops", chart, "--set", "mode=serve", "--set", "serve.userHeader=X-Forwarded-Email",
			"--set", "serve.networkPolicy.othersKeptOut=true", "--set", "serve.roles.admins={ana@example.com}"}, more...)
		cmd := exec.Command("helm", args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(t.TempDir(), "none"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	mimir := []string{"--set", "prometheus.url=http://mimir:8080/prometheus", "--set", "rules.ruler.url=http://mimir:8080/prometheus/config/v1/rules",
		"--set", "rules.ruler.tenant=team-a", "--set", "rules.ruler.tokenSecret.name=ruler-read"}
	amazon := []string{"--set", "prometheus.url=https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws-1", "--set", "prometheus.auth=aws",
		"--set", "rules.ruler.url=https://aps.us-east-1.amazonaws.com/workspaces/ws-1"}
	azure := []string{"--set", "prometheus.url=https://ws.koreacentral.prometheus.monitor.azure.com", "--set", "prometheus.auth=azure",
		"--set", "rules.ruler.url=https://management.azure.com/subscriptions/sub-1"}
	on := []string{"--set", "serve.changes.enabled=true"}
	for name, c := range map[string]struct {
		args []string
		want []string
		not  []string
	}{
		"Mimir, read only": {mimir, []string{`"--ruler=http://mimir:8080/prometheus/config/v1/rules"`, `"--ruler-tenant=team-a"`,
			`"--ruler-token-file=/var/run/lapilli/ruler/token"`, "secretName: ruler-read"}, []string{"--changes", "changes-ruler"}},
		"Mimir, changes": {append(append(mimir, on...), "--set", "serve.changes.in={shop,solo}", "--set", "serve.changes.rulerTokenSecret.name=ruler-write"),
			[]string{`"--changes-in=shop,solo"`, `"--changes-ruler-token-file=/var/run/lapilli/changes-ruler/token"`, "secretName: ruler-write",
				"mountPath: /var/run/lapilli/changes-ruler"}, nil},
		"Amazon, changes": {append(append(amazon, on...), "--set", "serve.changes.in={shop}", "--set", "serve.changes.managed={team=*}",
			"--set", "serve.changes.awsRole=arn:aws:iam::123456789012:role/lapilli-rules"),
			[]string{`"--changes-managed=team=*"`, `"--changes-aws-role=arn:aws:iam::123456789012:role/lapilli-rules"`}, []string{"changes-ruler"}},
		"Azure, changes": {append(append(azure, on...), "--set", "serve.changes.in={rg-shop/shop}",
			"--set", "serve.changes.azureClientId=11111111-2222-3333-4444-555555555555"),
			[]string{`"--changes-in=rg-shop/shop"`, `"--changes-azure-client-id=11111111-2222-3333-4444-555555555555"`}, nil},
	} {
		out, err := template(c.args...)
		if err != nil {
			t.Errorf("%s: %v\n%s", name, err, out)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: no %s", name, w)
			}
		}
		for _, w := range c.not {
			if strings.Contains(out, w) {
				t.Errorf("%s: %s", name, w)
			}
		}
	}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"two places":             {append(mimir, "--set", "rules.configMaps={rules}"), "give one of rules.configMaps"},
		"Amazon, no auth":        {append(amazon, "--set", "prometheus.auth="), "prometheus.auth to aws"},
		"Azure, Amazon's auth":   {append(azure, "--set", "prometheus.auth=aws"), "prometheus.auth to azure"},
		"a tenant for Amazon":    {append(amazon, "--set", "rules.ruler.tenant=a"), "a Mimir's or a Cortex's"},
		"in, changes off":        {append(mimir, "--set", "serve.changes.in={shop}"), "go with serve.changes.enabled"},
		"in, no ruler":           {append(on, "--set", "prometheus.url=http://p", "--set", "rules.configMaps={rules}", "--set", "serve.changes.in={shop}"), "rules read from a ruler"},
		"managed, Mimir":         {append(append(mimir, on...), "--set", "serve.changes.managed={team=*}"), "serve.changes.in alone"},
		"a token for Amazon":     {append(append(amazon, on...), "--set", "serve.changes.rulerTokenSecret.name=w"), "rulerTokenSecret is for a Mimir's"},
		"the read token":         {append(append(mimir, on...), "--set", "serve.changes.rulerTokenSecret.name=ruler-read"), "a token of their own"},
		"a role for Mimir":       {append(append(mimir, on...), "--set", "serve.changes.awsRole=arn:aws:iam::123456789012:role/r"), "awsRole is for Amazon's"},
		"a client ID for Amazon": {append(append(amazon, on...), "--set", "serve.changes.azureClientId=x"), "azureClientId is for Azure's"},
	} {
		if out, err := template(c.args...); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v, want %q\n%s", name, err, c.want, out)
		}
	}
}
