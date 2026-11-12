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
