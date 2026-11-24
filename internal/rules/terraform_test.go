package rules

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// terraform-plan-real.json is what terraform show -json wrote of a plan made
// with Terraform 1.12.2 and the grafana/grafana provider 4.55.0 (terraform
// plan -refresh=false, nothing applied) of one new grafana_rule_group with
// one rule. Being new, the rule has no uid (computed, unknown until apply)
// and its queries' query_type is null: neither is a fault.
func TestTerraformRealPlan(t *testing.T) {
	rs, rec := readAs(t, "testdata/terraform-plan-real.json")
	if len(rs) != 1 || len(rec) != 0 {
		t.Fatalf("%d rules, %d recordings", len(rs), len(rec))
	}
	r := rs[0]
	if r.Name != "CheckoutErrors" || r.Group != "alerts-folder/checkout" || r.Address != "grafana_rule_group.checkout" || r.For != 5*time.Minute {
		t.Errorf("%+v", r)
	}
	if r.UID != "" {
		t.Errorf("uid %q, none is known", r.UID)
	}
	if len(r.Labels) < 2 || r.Labels["severity"] != "page" || r.Labels["service"] != "checkout" {
		t.Errorf("labels %v", r.Labels)
	}
	if r.Says.Summary != "errors over 5 dollars $5" {
		t.Errorf("summary %q", r.Says.Summary)
	}
	g := r.Graph
	if g == nil || g.Cond != "C" || len(g.Queries) != 1 || g.Queries["A"] != `sum(rate(http_errors_total{service="checkout"}[5m]))` {
		t.Fatalf("graph %+v", g)
	}
	if g.Data["A"].UID != "prom-a" {
		t.Errorf("data %+v", g.Data)
	}
	if _, ok := g.Steps["C"]; !ok {
		t.Errorf("steps %+v", g.Steps)
	}
	if r.Unsupported != "" {
		t.Errorf("unsupported %q", r.Unsupported)
	}
}

// A module tree nested past terraformDepth is refused, not read as having
// no rules; one at the depth is read.
func TestTerraformTooDeep(t *testing.T) {
	nest := func(n int) string {
		m := `{"resources":[{"address":"grafana_rule_group.g","mode":"managed","type":"grafana_rule_group","name":"g","values":{"name":"g","folder_uid":"f","interval_seconds":60,"rule":[{"name":"R","uid":"r","condition":"A","data":[{"ref_id":"A","datasource_uid":"p","model":"{\"expr\":\"up\"}"}]}]}}]}`
		for range n {
			m = `{"child_modules":[` + m + `]}`
		}
		return `{"format_version":"1.0","values":{"root_module":` + m + `}}`
	}
	rs, err := Parse([]byte(nest(terraformDepth)))
	if err != nil || len(rs) != 1 {
		t.Fatalf("at the depth: %d rules, %v", len(rs), err)
	}
	if _, err := Parse([]byte(nest(terraformDepth + 1))); err == nil || !strings.Contains(err.Error(), "modules nested more than 64 deep") {
		t.Errorf("past the depth: %v", err)
	}
}

// TerraformRulesOnly keeps the rule groups of a plan's or a state's JSON and
// nothing else, so that what is read of it is read as before; a document
// that isn't Terraform's comes back as it is.
func TestTerraformRulesOnly(t *testing.T) {
	for _, f := range append([]string{"testdata/terraform-plan-real.json"}, terraformFiles...) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// A resource that isn't a rule group, with a secret, in a module and
		// among the roots.
		doc := strings.Replace(string(raw), `"resources"`, `"x":{"secret":"s3cr3t"},"resources"`, 1)
		kept, err := TerraformRulesOnly([]byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if strings.Contains(string(kept), "s3cr3t") || strings.Contains(string(kept), "resource_changes") || strings.Contains(string(kept), "configuration") {
			t.Errorf("%s: more than the rule groups kept: %s", f, kept)
		}
		var want, got []Rule
		if want, _ = readAs(t, f); len(want) == 0 {
			t.Fatalf("%s: no rules", f)
		}
		tmp := filepath.Join(t.TempDir(), "kept.json")
		os.WriteFile(tmp, kept, 0o600)
		got, _ = readAs(t, tmp)
		if len(got) != len(want) {
			t.Fatalf("%s: %d rules kept of %d", f, len(got), len(want))
		}
		for i := range got {
			if got[i].Name != want[i].Name || got[i].Address != want[i].Address || got[i].Digest != want[i].Digest {
				t.Errorf("%s: rule %d read differently: %+v, was %+v", f, i, got[i], want[i])
			}
		}
	}
	for _, in := range []string{"groups:\n  - name: g\n", `{"a":1}`, `[1]`, `{"format_version":"1","values":{}}`} {
		if out, err := TerraformRulesOnly([]byte(in)); err != nil || string(out) != in {
			t.Errorf("%q: %q, %v", in, out, err)
		}
	}
	if _, err := TerraformRulesOnly([]byte(`{"format_version":"1","values":{"root_module":{}}} {}`)); err == nil {
		t.Errorf("a second document was let through")
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
