package rules

import (
	"context"
	"os"
	"strings"
	"testing"
)

// terraformFiles are what terraform show -json writes of a saved plan and
// of a state, by the Grafana provider's schema for grafana_rule_group: one
// group in the root module and one in a child module, three alert rules
// (one with two queries and a math expression, one paused) and a recording
// rule.
var terraformFiles = []string{"testdata/terraform-plan.json", "testdata/terraform-state.json"}

// readAs parses a file, and the recordings it holds.
func readAs(t *testing.T, path string) ([]Rule, []Recording) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec []Recording
	rs, err := parse(b, &rec)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return rs, rec
}

// Terraform's JSON is read to the rules the same Grafana provisioning file
// is, whether it is a plan's or a state's.
func TestTerraformSameAsProvisioning(t *testing.T) {
	want, wantRec := readAs(t, "testdata/terraform-equivalent.yaml")
	if len(want) != 3 || len(wantRec) != 1 {
		t.Fatalf("the equivalent file: %d rules, %d recordings", len(want), len(wantRec))
	}
	for _, f := range terraformFiles {
		got, rec := readAs(t, f)
		if len(got) != len(want) || len(rec) != len(wantRec) {
			t.Fatalf("%s: %d rules, %d recordings", f, len(got), len(rec))
		}
		for i := range got {
			g, w := got[i], want[i]
			if g.Name != w.Name || g.Group != w.Group || g.UID != w.UID || g.For != w.For || g.Interval != w.Interval ||
				g.Unsupported != w.Unsupported || g.Digest != w.Digest || g.Fingerprint != w.Fingerprint || g.Receiver != w.Receiver ||
				g.Folder != w.Folder || len(g.Labels) != len(w.Labels) {
				t.Errorf("%s: rule %d\n got %+v\nwant %+v", f, i, g, w)
			}
			if (g.Graph == nil) != (w.Graph == nil) || g.Graph != nil && (g.Graph.Cond != w.Graph.Cond || len(g.Graph.Queries) != len(w.Graph.Queries)) {
				t.Errorf("%s: rule %d graph differs", f, i)
			}
		}
		if rec[0].Record != wantRec[0].Record || rec[0].Expr != wantRec[0].Expr || rec[0].Group != wantRec[0].Group {
			t.Errorf("%s: recording %+v, want %+v", f, rec[0], wantRec[0])
		}
		if got[0].Address != "grafana_rule_group.checkout" || got[1].Address != "grafana_rule_group.checkout" ||
			got[2].Address != "module.edge.grafana_rule_group.edge" || got[1].Unsupported != Paused {
			t.Errorf("%s: %q %q %q", f, got[0].Address, got[2].Address, got[1].Unsupported)
		}
		if got[0].Graph == nil || len(got[0].Graph.Queries) != 2 {
			t.Errorf("%s: graph %+v", f, got[0].Graph)
		}
	}
}

// A Terraform JSON without a grafana_rule_group has no rules, and says what
// is wanted; a model that isn't JSON is named by its address, rule and
// query; a dollar is a dollar.
func TestTerraformRefusals(t *testing.T) {
	_, err := Parse([]byte(`{"format_version":"1.0","values":{"root_module":{"resources":[{"address":"grafana_folder.a","mode":"managed","type":"grafana_folder","name":"a","values":{}}]}}}`))
	if err == nil || !strings.Contains(err.Error(), "Terraform's JSON of grafana_rule_group (terraform show -json)") {
		t.Errorf("%v", err)
	}
	_, err = Parse([]byte(`{"format_version":"1.0","values":{"root_module":{"resources":[{"address":"grafana_rule_group.g","mode":"managed","type":"grafana_rule_group","name":"g","values":{"name":"g","folder_uid":"f","interval_seconds":60,"rule":[{"name":"R","condition":"A","data":[{"ref_id":"A","datasource_uid":"p","model":"{not json"}]}]}}]}}}`))
	for _, w := range []string{"grafana_rule_group.g", `"R"`, `"A"`, "isn't JSON"} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Fatalf("want %q in %v", w, err)
		}
	}
	rs, err := Parse([]byte(`{"format_version":"1.0","values":{"root_module":{"resources":[{"address":"grafana_rule_group.g","mode":"managed","type":"grafana_rule_group","name":"g","values":{"name":"g","folder_uid":"f","interval_seconds":60,"rule":[{"name":"Cost $5","uid":"c","condition":"A","labels":{"b":"$5"},"data":[{"ref_id":"A","datasource_uid":"p","model":"{\"expr\":\"up\"}"}]}]}}]}}}`))
	if err != nil || len(rs) != 1 || rs[0].Name != "Cost $5" || rs[0].Labels["b"] != "$5" {
		t.Errorf("%+v %v", rs, err)
	}
}

func FuzzTerraform(f *testing.F) {
	for _, p := range terraformFiles {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte(`{"format_version":"1.0","values":{"root_module":{"resources":[{"mode":"managed","type":"grafana_rule_group","values":{"rule":[{"data":[{"model":"{","relative_time_range":5}],"notification_settings":3,"record":"x"}]}}],"child_modules":[{}]}}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		var rec []Recording
		_, _ = parseContext(context.Background(), b, &rec)
	})
}
