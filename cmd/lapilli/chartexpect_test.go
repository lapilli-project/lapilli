package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/verdict"
)

// serve.expectations is rendered into a ConfigMap of its own, mounted as
// a directory (the kubelet updates it in place) and passed as --expect,
// which reads what it holds as lapilli verdict --expect does, a text as
// written or a map; serve.expectTasks is --expect-tasks. Left out,
// nothing of them is rendered.
func TestChartExpectations(t *testing.T) {
	plain, err := helmTemplate(t, serveValues...)
	if err != nil {
		t.Fatalf("%v\n%s", err, plain)
	}
	for _, not := range []string{"--expect", "/etc/lapilli/expectations", "-expectations\n"} {
		if strings.Contains(plain, not) {
			t.Errorf("by default, %q is rendered", not)
		}
	}
	file := "expectations:\n  - rule: ContainerRestarts\n    budget: {week: 10, asleep: 2}\n    catches:\n      - {incident: INC-43, within: 5m}\n  - labels: severity=page\n    budget: {refires: 3}\n"
	path := filepath.Join(t.TempDir(), "expect.yaml")
	os.WriteFile(path, []byte(file), 0o644)
	asText := append(append([]string{}, serveValues...), "--set-file", "serve.expectations="+path, "--set", "serve.expectTasks=true")
	values := filepath.Join(t.TempDir(), "values.yaml")
	os.WriteFile(values, []byte("serve:\n  expectations:\n"+indent(file, "    ")+"\n"), 0o644)
	asMap := append(append([]string{}, serveValues...), "-f", values)
	for name, args := range map[string][]string{"text": asText, "map": asMap} {
		out, err := helmTemplate(t, args...)
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		for _, want := range []string{"- --expect=/etc/lapilli/expectations/expectations.yaml", "mountPath: /etc/lapilli/expectations", "name: ops-lapilli-expectations"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: no %q", name, want)
			}
		}
		if (name == "text") != strings.Contains(out, "- --expect-tasks") {
			t.Errorf("%s: --expect-tasks as given", name)
		}
		var held string
		for _, doc := range strings.Split(out, "\n---\n") {
			var cm struct {
				Kind     string
				Metadata struct{ Name string }
				Data     map[string]string
			}
			if yaml.Unmarshal([]byte(doc), &cm) == nil && cm.Kind == "ConfigMap" && cm.Metadata.Name == "ops-lapilli-expectations" {
				held = cm.Data["expectations.yaml"]
			}
		}
		e, err := verdict.ReadExpectations(strings.NewReader(held))
		if err != nil || len(e.Expectations) != 2 || e.Expectations[1].Labels != "severity=page" {
			t.Errorf("%s: %v %+v\n%s", name, err, e, held)
		}
	}
}

// serve.scenarios are put beside expectations.yaml in its ConfigMap, a
// scenario's path its name. serve.promtool.image is copied from by an
// init container into an emptyDir only the pod has, and given as
// --promtool; serve.promtool.path is an image of one's own's; with either,
// /tmp is an emptyDir, the root being read-only. Both at once, promtool
// with no expectations, scenarios with no expectations file, and a file's
// name a ConfigMap can't hold, are refused.
func TestChartScenarios(t *testing.T) {
	plain, err := helmTemplate(t, append(append([]string{}, serveValues...), "--set", "serve.expectations=expectations: []")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, plain)
	}
	for _, not := range []string{"--promtool", "name: promtool", "scenarios-tmp"} {
		if strings.Contains(plain, not) {
			t.Errorf("by default, %q is rendered", not)
		}
	}
	expect := "expectations:\n  - rule: ContainerRestarts\n    scenarios: [restarts.test.yaml]\n"
	test := "tests:\n  - name: restarts\n    input_series: []\n"
	dir := t.TempDir()
	ef, tf := filepath.Join(dir, "expect.yaml"), filepath.Join(dir, "restarts.yaml")
	os.WriteFile(ef, []byte(expect), 0o644)
	os.WriteFile(tf, []byte(test), 0o644)
	image := "prom/prometheus:v3.0.0@sha256:" + strings.Repeat("a", 64)
	with := func(more ...string) []string {
		return append(append(append([]string{}, serveValues...), "--set-file", "serve.expectations="+ef, "--set-file", "serve.scenarios.restarts\\.test\\.yaml="+tf), more...)
	}
	out, err := helmTemplate(t, with("--set", "serve.promtool.image="+image)...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"- --promtool=/opt/lapilli-promtool/promtool", `image: "` + image + `"`, `command: ["cp", "/bin/promtool", "/promtool/promtool"]`,
		"mountPath: /opt/lapilli-promtool", "mountPath: /tmp", "name: scenarios-tmp"} {
		if !strings.Contains(out, want) {
			t.Errorf("image: no %q", want)
		}
	}
	var held map[string]string
	for _, doc := range strings.Split(out, "\n---\n") {
		var cm struct {
			Kind     string
			Metadata struct{ Name string }
			Data     map[string]string
		}
		if yaml.Unmarshal([]byte(doc), &cm) == nil && cm.Kind == "ConfigMap" && cm.Metadata.Name == "ops-lapilli-expectations" {
			held = cm.Data
		}
	}
	// As written, but for a last line break YAML may leave off.
	if strings.TrimSpace(held["restarts.test.yaml"]) != strings.TrimSpace(test) || held["expectations.yaml"] != expect {
		t.Errorf("the ConfigMap: %q", held)
	}
	out, err = helmTemplate(t, with("--set", "serve.promtool.path=/usr/local/bin/promtool")...)
	if err != nil || !strings.Contains(out, `- "--promtool=/usr/local/bin/promtool"`) || strings.Contains(out, "name: promtool\n") || !strings.Contains(out, "mountPath: /tmp") {
		t.Errorf("path: %v\n%s", err, out)
	}
	for _, c := range []struct {
		args []string
		said string
	}{
		{with("--set", "serve.promtool.image="+image, "--set", "serve.promtool.path=/p"), "not both"},
		{append(append([]string{}, serveValues...), "--set", "serve.promtool.path=/p"), "give one"},
		{append(append([]string{}, serveValues...), "--set-file", "serve.scenarios.x\\.yaml="+tf), "give serve.expectations"},
		{with("--set-file", "serve.scenarios.expectations\\.yaml="+tf), "not expectations.yaml"},
	} {
		if out, err := helmTemplate(t, c.args...); err == nil || !strings.Contains(out, c.said) {
			t.Errorf("%v: %v\n%s", c.args, err, out)
		}
	}
}
