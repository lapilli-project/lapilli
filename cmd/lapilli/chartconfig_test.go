package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/serve"
)

// serve.config is rendered into a ConfigMap, mounted as a directory (the
// kubelet updates it in place) and passed as --config, which takes what it
// holds; serve.configPrune and serve.proposalsExpire are their flags. Left
// out, nothing of them is rendered, and two renders are byte for byte the
// same; serve.configPrune without serve.config is refused.
func TestChartConfig(t *testing.T) {
	plain, err := helmTemplate(t, serveValues...)
	if err != nil {
		t.Fatalf("%v\n%s", err, plain)
	}
	for _, not := range []string{"--config", "--proposals-expire", "-config\n", "/etc/lapilli/config"} {
		if strings.Contains(plain, not) {
			t.Errorf("by default, %q is rendered", not)
		}
	}
	values := filepath.Join(t.TempDir(), "values.yaml")
	os.WriteFile(values, []byte(`serve:
  configPrune: true
  proposalsExpire: 30d
  config:
    teams:
      - {name: payments, rules: ^Checkout, labels: {team: payments}, sleep_hours: "23:00-07:00"}
    windows:
      - {title: Nightly backup, team: payments, start: "2026-11-02T02:00", end: "2026-11-02T03:00", every: day, until: 2027-06-30}
      - {title: Patching, start: "2026-11-07T22:00", end: "2026-11-08T01:00", every: month, repeat_on: first sat}
    reports:
      - {name: payments weekly, team: payments, schedule: weekly mon@09:00, dests: [team:payments], enabled: false}
    grants:
      - {group: payments-oncall, team: payments}
      - {user: ana@example.com}
`), 0o644)
	args := append(append([]string{}, serveValues...), "-f", values)
	out, err := helmTemplate(t, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	again, _ := helmTemplate(t, args...)
	if again != out {
		t.Error("two renders differ")
	}
	for _, want := range []string{"- --config=/etc/lapilli/config/config.yaml", "- --config-prune", `- "--proposals-expire=30d"`,
		"mountPath: /etc/lapilli/config", "name: ops-lapilli-config"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "subPath: config.yaml") {
		t.Error("mounted by subPath, which the kubelet doesn't update")
	}
	// The file as rendered is what --config takes.
	var file string
	for _, doc := range strings.Split(out, "\n---\n") {
		var cm struct {
			Kind     string
			Metadata struct{ Name string }
			Data     map[string]string
		}
		if yaml.Unmarshal([]byte(doc), &cm) == nil && cm.Kind == "ConfigMap" && cm.Metadata.Name == "ops-lapilli-config" {
			file = cm.Data["config.yaml"]
		}
	}
	d, err := serve.ParseConfig([]byte(file), time.UTC)
	if err != nil {
		t.Fatalf("the rendered file: %v\n%s", err, file)
	}
	// repeat_on, not on: Helm reads values as YAML 1.1, where a key on,
	// unquoted, is true.
	if len(d.Teams) != 1 || d.Teams[0].SleepHours != "23:00-07:00" || len(d.Windows) != 2 || d.Windows[0].Until.IsZero() || d.Windows[1].On != "first sat" ||
		len(d.Reports) != 1 || d.Reports[0].Enabled || len(d.Grants) != 2 {
		t.Errorf("the rendered file reads as %+v\n%s", d, file)
	}
	if out, err := helmTemplate(t, append(append([]string{}, serveValues...), "--set", "serve.configPrune=true")...); err == nil ||
		!strings.Contains(out, "serve.configPrune deletes what serve.config no longer declares") {
		t.Errorf("serve.configPrune alone: %v\n%s", err, out)
	}
}
