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
