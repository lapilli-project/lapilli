package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type workflow struct {
	Jobs map[string]struct {
		If          string `yaml:"if"`
		Concurrency *struct {
			Group            string `yaml:"group"`
			CancelInProgress bool   `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
		Steps []struct {
			Name string            `yaml:"name"`
			ID   string            `yaml:"id"`
			If   string            `yaml:"if"`
			Run  string            `yaml:"run"`
			Env  map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// Two tags pushed close together publish one at a time, none cancelled,
// and the newer is latest whichever is published last: the release
// workflow had no concurrency, so latest could be left on the older. A dry
// run, which has no publish job, never waits for a release.
func TestReleaseNewestIsLatest(t *testing.T) {
	parallel(t)
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	pub, ok := w.Jobs["publish"]
	if !ok || pub.Concurrency == nil || pub.Concurrency.Group == "" || pub.Concurrency.CancelInProgress {
		t.Fatalf("publish runs one at a time, none cancelled: %+v", pub.Concurrency)
	}
	for name, j := range w.Jobs {
		if name != "publish" && j.Concurrency != nil {
			t.Errorf("%s waits for a release: a dry run runs it", name)
		}
	}
	var newest string
	var latestIf, publish string
	for _, s := range pub.Steps {
		switch s.Name {
		case "Newest":
			newest = s.Run
			if s.ID != "newest" {
				t.Errorf("Newest's id: %q", s.ID)
			}
		case "Latest":
			latestIf = s.If
		case "Publish":
			publish = s.Run + " " + s.Env["NEWEST"]
		}
	}
	if latestIf != "steps.newest.outputs.newest == 'true'" {
		t.Errorf("latest is tagged whatever the version: %q", latestIf)
	}
	if !strings.Contains(publish, `--latest="$NEWEST"`) || !strings.Contains(publish, "steps.newest.outputs.newest") {
		t.Errorf("the release is GitHub's latest whatever the version: %s", publish)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	// The step itself, with a gh that lists the releases given.
	for _, c := range []struct {
		version, released, want string
	}{
		{"v0.28.1", "v0.28.0\nv0.27.0\n", "true"},
		{"v0.28.1", "v0.28.2\nv0.28.0\n", "false"},
		{"v0.28.10", "v0.28.9\nv0.28.2\n", "true"},
		{"v0.28.9", "v0.28.10\n", "false"},
		{"v1.0.0", "v0.99.0\nnot-a-version\nv2.0.0-rc.1\n", "true"},
		{"v0.28.1", "", "true"},
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nprintf '%s' \""+c.released+"\"\n"), 0o755)
		out := filepath.Join(dir, "out")
		cmd := exec.Command("bash", "-e", "-c", newest)
		cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "VERSION="+c.version, "GITHUB_OUTPUT="+out)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
		got, _ := os.ReadFile(out)
		if strings.TrimSpace(string(got)) != "newest="+c.want {
			t.Errorf("%s, released %q: %q, want newest=%s", c.version, c.released, got, c.want)
		}
	}
}

// The release step "Chart values marker" writes the release's version over
// the dev marker in values.yaml and in files/values.yaml, and nowhere else;
// a chart that already holds a version there is refused.
func TestReleaseChartMarker(t *testing.T) {
	parallel(t)
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	var run string
	for _, s := range wf.Jobs["build"].Steps {
		if s.Name == "Chart values marker" {
			run = s.Run
		}
	}
	if run == "" {
		t.Fatal("no step Chart values marker in build")
	}
	chart := filepath.Join("..", "..", "charts", "lapilli")
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "charts", "lapilli", "files"), 0o755)
	var before [2][]byte
	for i, f := range []string{"values.yaml", filepath.Join("files", "values.yaml")} {
		if before[i], err = os.ReadFile(filepath.Join(chart, f)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "charts", "lapilli", f), before[i], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "-c", run)
	cmd.Dir, cmd.Env = dir, append(os.Environ(), "VERSION=v9.9.9")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for i, f := range []string{"values.yaml", filepath.Join("files", "values.yaml")} {
		got, err := os.ReadFile(filepath.Join(dir, "charts", "lapilli", f))
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(string(before[i]), "\nchartValues: \"dev\"\n", "\nchartValues: \"v9.9.9\"\n", 1)
		if want == string(before[i]) || string(got) != want {
			t.Errorf("%s: the marker is not the version, or something else changed", f)
		}
	}
	// A copy without the marker stops the step before it writes either file.
	fresh := t.TempDir()
	os.MkdirAll(filepath.Join(fresh, "charts", "lapilli", "files"), 0o755)
	if err := os.WriteFile(filepath.Join(fresh, "charts", "lapilli", "values.yaml"), before[0], 0o644); err != nil {
		t.Fatal(err)
	}
	bare := []byte(strings.Replace(string(before[1]), "\nchartValues: \"dev\"\n", "\n", 1))
	if err := os.WriteFile(filepath.Join(fresh, "charts", "lapilli", "files", "values.yaml"), bare, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", "-c", run)
	cmd.Dir, cmd.Env = fresh, append(os.Environ(), "VERSION=v9.9.9")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("a copy without the marker passed:\n%s", out)
	}
	if got, _ := os.ReadFile(filepath.Join(fresh, "charts", "lapilli", "values.yaml")); string(got) != string(before[0]) {
		t.Error("values.yaml was written although files/values.yaml has no marker")
	}
	// Run again: the marker is no longer dev, so the step stops.
	cmd = exec.Command("bash", "-c", run)
	cmd.Dir, cmd.Env = dir, append(os.Environ(), "VERSION=v9.9.9")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("a second run passed:\n%s", out)
	}
}
