package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The chart mounts the recipes from its own copies, so that they travel in
// the packaged chart: charts/lapilli/files/recipes/{incidents,changes} must
// hold what recipes/incidents and recipes/changes do, their READMEs and
// testdata aside, and nothing else.
func TestChartRecipesAreTheRepositorys(t *testing.T) {
	list := func(dir string) map[string]string {
		got := map[string]string{}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || e.Name() == "README.md" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			got[e.Name()] = string(b)
		}
		return got
	}
	const fix = "run charts/sync-recipes.sh and commit what it changes"
	for _, kind := range []string{"incidents", "changes"} {
		want := list(filepath.Join("..", "..", "recipes", kind))
		got := list(filepath.Join("..", "..", "charts", "lapilli", "files", "recipes", kind))
		if len(want) == 0 {
			t.Fatalf("recipes/%s holds nothing", kind)
		}
		for name, w := range want {
			if g, ok := got[name]; !ok {
				t.Errorf("charts/lapilli/files/recipes/%s/%s is missing: %s", kind, name, fix)
			} else if g != w {
				t.Errorf("charts/lapilli/files/recipes/%s/%s differs from recipes/%s/%s: %s", kind, name, kind, name, fix)
			}
		}
		for name := range got {
			if _, ok := want[name]; !ok {
				t.Errorf("charts/lapilli/files/recipes/%s/%s is not in recipes/%s: %s", kind, name, kind, fix)
			}
		}
		core := map[string]string{"incidents": "put.sh", "changes": "lib.sh"}[kind]
		if _, ok := got[core]; !ok {
			t.Errorf("charts/lapilli/files/recipes/%s has no %s", kind, core)
		}
	}
}
