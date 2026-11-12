package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// helmTemplate renders the chart with the values given; it skips the test
// without helm.
func helmTemplate(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm")
	}
	chart := filepath.Join("..", "..", "charts", "lapilli")
	cmd := exec.Command("helm", append([]string{"template", "ops", chart}, args...)...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(t.TempDir(), "none"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

var serveValues = []string{"--set", "mode=serve", "--set", "prometheus.url=http://prometheus:9090", "--set", "rules.configMaps={rules}"}

// The chart listens on the pod's address, which every pod in the cluster
// reaches: with no roles, lapilli serve takes no changes there, unless
// serve.everyoneAdmin says everyone may make them. Off by default, and
// refused beside roles or serve.readOnly.
func TestChartEveryoneAdmin(t *testing.T) {
	out, err := helmTemplate(t, serveValues...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "--everyone-admin") || !strings.Contains(out, "- --listen=:8080") {
		t.Errorf("by default: %s", out)
	}
	out, err = helmTemplate(t, append(serveValues, "--set", "serve.everyoneAdmin=true")...)
	if err != nil || !strings.Contains(out, "- --everyone-admin\n") {
		t.Errorf("serve.everyoneAdmin: %v\n%s", err, out)
	}
	for _, more := range [][]string{
		{"--set", "serve.userHeader=X-Forwarded-Email", "--set", "serve.networkPolicy.othersKeptOut=true", "--set", "serve.roles.admins={ana@example.com}"},
		{"--set", "serve.userHeader=X-Forwarded-Email", "--set", "serve.networkPolicy.othersKeptOut=true", "--set", "serve.roles.editors={*}"},
		{"--set", "serve.readOnly=true"},
	} {
		if out, err := helmTemplate(t, append(append(serveValues, "--set", "serve.everyoneAdmin=true"), more...)...); err == nil {
			t.Errorf("serve.everyoneAdmin with %v: taken\n%s", more, out)
		}
	}
}
