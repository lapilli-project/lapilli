package rules

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// PickedFiles reads files given as ReadPicked reads a directory holding
// them, the same rules and documents, with a selector too; and a path is
// only a name: one that would climb out of a directory, or name a file on
// this machine, is read as the bytes given, nothing else.
func TestPickedFiles(t *testing.T) {
	files := []File{
		{Path: "alerts/api.yaml", Data: []byte("groups:\n- name: api\n  rules:\n  - alert: Errors\n    expr: rate(errors_total[5m]) > 1\n    for: 5m\n    labels: {severity: page}\n")},
		{Path: "chart.yaml", Data: []byte("apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata: {name: x, labels: {team: shop}}\nspec:\n  groups:\n  - name: shop\n    rules:\n    - alert: 결제실패\n      expr: payment_failures_total > 0\n")},
		{Path: "README.md", Data: []byte("# not rules\n")},
		{Path: "nested/broken.yaml", Data: []byte("groups: [")},
	}
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sel, _ := ParseSelector("team=shop")
	for _, p := range []Picking{{}, {Select: sel}} {
		want, wantDocs, wantPicked, err := ReadPicked(dir, p)
		if err != nil {
			t.Fatal(err)
		}
		got, docs, picked, err := PickedFiles(files, p)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) || len(docs) != len(wantDocs) || picked != wantPicked {
			t.Fatalf("select %q: %d rules %d docs %+v, want %d %d %+v", p.Select, len(got), len(docs), picked, len(want), len(wantDocs), wantPicked)
		}
		for i := range got {
			got[i].Source, want[i].Source = "", ""
			if !reflect.DeepEqual(got[i], want[i]) {
				t.Errorf("rule %d:\n %+v\nwant\n %+v", i, got[i], want[i])
			}
		}
	}
	// A path is a name.
	got, _, _, err := PickedFiles([]File{{Path: "../../../etc/passwd", Data: files[0].Data}, {Path: "/etc/hosts", Data: files[1].Data}}, Picking{})
	if err != nil || len(got) != 2 || got[0].Source != "../../../etc/passwd" || got[1].Name != "결제실패" {
		t.Fatalf("%+v %v", got, err)
	}
	// One at the top that doesn't read is refused, as ReadPicked refuses it.
	if _, _, _, err := PickedFiles([]File{{Path: "broken.yaml", Data: []byte("groups: [")}}, Picking{}); err == nil {
		t.Error("a file at the top that doesn't read was passed over")
	}
}
