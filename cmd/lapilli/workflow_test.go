package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type ciWorkflow struct {
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress string `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Jobs map[string]struct {
		Name     string            `yaml:"name"`
		If       string            `yaml:"if"`
		Needs    yaml.Node         `yaml:"needs"`
		Outputs  map[string]string `yaml:"outputs"`
		Strategy struct {
			Matrix struct {
				OS []string `yaml:"os"`
			} `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []struct {
			Run string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readCI(t *testing.T) ciWorkflow {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var w ciWorkflow
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

// reached runs test/e2e/reached.sh over the paths changed, and gives the
// jobs it says they reach, and every job it names.
func reached(t *testing.T, changed ...string) (reach, all []string) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join("..", "..", "test", "e2e", "reached.sh"))
	cmd.Stdin = strings.NewReader(strings.Join(changed, "\n"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reached.sh: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, v, ok := strings.Cut(line, "=")
		if !ok || (v != "true" && v != "false") {
			t.Fatalf("reached.sh says %q", line)
		}
		all = append(all, id)
		if v == "true" {
			reach = append(reach, id)
		}
	}
	return reach, all
}

// On main, on the schedule and when asked, every job of CI runs; on a pull
// request, an end-to-end job runs when the changes reach it
// (test/e2e/reached.sh), and the run is cancelled by the pull request's
// next push, no other run by anything. The checks the branch ruleset
// requires run on every pull request, by their names.
func TestWorkflowMainRunsEverything(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	w := readCI(t)
	if got := w.Concurrency.CancelInProgress; got != "${{ github.event_name == 'pull_request' }}" {
		t.Errorf("cancel-in-progress: %q, want a pull request's alone", got)
	}
	if g := w.Concurrency.Group; !strings.Contains(g, "github.event.pull_request.number") || !strings.Contains(g, "github.run_id") {
		t.Errorf("concurrency group %q: a pull request's by its number, any other run's its own", g)
	}

	for id, name := range map[string]string{"test": "test on ${{ matrix.os }}", "latest-go": "test on the latest Go", "vulnerabilities": "govulncheck"} {
		j, ok := w.Jobs[id]
		if !ok || j.Name != name || j.If != "" || j.Needs.Kind != 0 {
			t.Errorf("required check %s: named %q, if %q, needs %v; want %q, run on every pull request", id, j.Name, j.If, j.Needs.Value, name)
		}
	}
	if got := w.Jobs["test"].Strategy.Matrix.OS; !reflect.DeepEqual(got, []string{"ubuntu-latest", "macos-latest"}) {
		t.Errorf("test's matrix: %v", got)
	}

	_, all := reached(t)
	slices.Sort(all)
	var e2e, filtered []string
	for id, j := range w.Jobs {
		for _, s := range j.Steps {
			if strings.HasPrefix(s.Run, "test/e2e/") {
				e2e = append(e2e, id)
				break
			}
		}
		switch {
		case j.If == "":
		case id == "fuzz" && j.If == "github.event_name == 'schedule' || github.event_name == 'workflow_dispatch'":
		case j.If == "${{ !cancelled() && (github.event_name != 'pull_request' || needs.changes.outputs."+id+" == 'true') }}":
			filtered = append(filtered, id)
			if j.Needs.Value != "changes" {
				t.Errorf("%s needs %q, not changes", id, j.Needs.Value)
			}
			if got := w.Jobs["changes"].Outputs[id]; got != "${{ steps.reached.outputs."+id+" }}" {
				t.Errorf("changes' output %s: %q", id, got)
			}
		default:
			t.Errorf("%s runs only %q: on main, every job runs", id, j.If)
		}
	}
	slices.Sort(e2e)
	slices.Sort(filtered)
	if !reflect.DeepEqual(e2e, all) || !reflect.DeepEqual(filtered, all) {
		t.Errorf("the end-to-end jobs %v, those a pull request runs when reached %v, and reached.sh's %v", e2e, filtered, all)
	}
}

// What a pull request's changes reach: lapilli itself every end-to-end
// job but e2e-compat, which only what reads the history and replays it
// reaches, a script and what it alone reads its job, the docs and Go's
// tests none, and a path reached.sh doesn't name every job.
func TestWorkflowReached(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	_, all := reached(t)
	slices.Sort(all)
	lapilli := slices.DeleteFunc(slices.Clone(all), func(j string) bool { return j == "e2e-compat" })
	chart := []string{"e2e", "e2e-dashboard", "e2e-rendered", "e2e-replicas", "e2e-serve-lb", "e2e-service"}
	for _, c := range []struct {
		changed []string
		want    []string
	}{
		{nil, nil},
		{[]string{"docs/serve.md", "CHANGELOG.md", "site/index.html", "assets/og.png", "charts/lapilli/README.md", "recipes/backtest/README.md"}, nil},
		{[]string{".github/workflows/release.yml", ".github/workflows/upstream.yml", "internal/serve/web_test.go", "internal/serve/testdata/notify/a.json", "recipes/trackers/jira.sh", "test/shard.sh"}, nil},
		{[]string{"internal/replay/replay_test.go", "internal/source/testdata/a.json"}, nil},
		{[]string{"internal/serve/web.go"}, lapilli},
		{[]string{"internal/serve/templates/rules.html"}, lapilli},
		{[]string{"cmd/lapilli/verdict.go"}, lapilli},
		{[]string{"internal/replay/replay.go"}, all},
		{[]string{"internal/prom/prom.go", "internal/rules/rules.go", "internal/source/source.go"}, all},
		{[]string{"test/e2e/compat.sh"}, []string{"e2e-compat"}},
		{[]string{"go.sum"}, all},
		{[]string{".github/workflows/ci.yml"}, all},
		{[]string{"test/e2e/reached.sh"}, all},
		{[]string{"tools/new.sh"}, all},
		{[]string{"test/e2e/lib.sh"}, all},
		{[]string{"charts/lapilli/values.yaml"}, chart},
		{[]string{"Dockerfile"}, []string{"e2e", "e2e-replicas", "e2e-serve-lb", "e2e-service"}},
		{[]string{"test/e2e/signin-lib.sh"}, []string{"e2e-serve-lb", "e2e-service", "e2e-signin"}},
		{[]string{"test/a11y/package-lock.json"}, []string{"e2e-service"}},
		{[]string{"recipes/backtest/gitea.sh", "test/e2e/loki.sh"}, []string{"e2e-backtest", "e2e-loki"}},
		{[]string{"recipes/versions/git.sh"}, []string{"e2e-rendered"}},
		{[]string{"recipes/changes/lib.sh"}, []string{"e2e-changes"}},
	} {
		got, _ := reached(t, c.changed...)
		slices.Sort(got)
		if !reflect.DeepEqual(got, c.want) && (len(got) > 0 || len(c.want) > 0) {
			t.Errorf("%v reaches %v, want %v", c.changed, got, c.want)
		}
	}
}
