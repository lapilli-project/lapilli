package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Terraform plan sent to the backtest port holds every resource's values,
// the sensitive ones among them: what the ledger keeps of it is its rule
// groups only.
func TestBacktestTerraformKeepsRuleGroupsOnly(t *testing.T) {
	plan, err := os.ReadFile("../rules/testdata/terraform-plan-real.json")
	if err != nil {
		t.Fatal(err)
	}
	const secret = "s3cr3t-basic-auth-password"
	// A data source with a secret beside the rule group, in the root module
	// and in a child module.
	with := func(p string) string {
		var doc map[string]any
		if err := json.Unmarshal([]byte(p), &doc); err != nil {
			t.Fatal(err)
		}
		ds := map[string]any{"address": "grafana_data_source.p", "mode": "managed", "type": "grafana_data_source", "name": "p",
			"values": map[string]any{"secure_json_data_encoded": secret}}
		root := doc["planned_values"].(map[string]any)["root_module"].(map[string]any)
		root["resources"] = append(root["resources"].([]any), ds)
		root["child_modules"] = []any{map[string]any{"address": "module.m", "resources": []any{ds}}}
		out, _ := json.Marshal(doc)
		return string(out)
	}
	before := with(string(plan))
	after := strings.Replace(before, `"for":"5m"`, `"for":"10m"`, -1)
	// The rules in effect read the plan's data source, as a rule of the
	// plan may only read those.
	grafana := "apiVersion: 1\ngroups:\n  - orgId: 1\n    name: g\n    folder: f\n    interval: 1m\n    rules:\n      - uid: u1\n        title: G\n        condition: A\n        data:\n          - refId: A\n            datasourceUid: prom-a\n            model: {expr: 'up == 0'}\n"
	b := btServer(t, nil, "", func(c *Config) { os.WriteFile(filepath.Join(c.RulesPath, "grafana.yaml"), []byte(grafana), 0o644) })
	body := btBody(before, after, map[string]any{"days": 3})
	if !strings.Contains(string(body), secret) {
		t.Fatal("the request holds no secret to keep out")
	}
	code, m, _ := btAsk(t, b.h, "POST", "/v1/backtests", b.plain, body)
	if code != http.StatusAccepted {
		t.Fatalf("POST: %d %v", code, m)
	}
	_, kept, ok, err := b.s.l.StartBacktest(context.Background(), b.s.now())
	if err != nil || !ok {
		t.Fatalf("not kept: %t %v", ok, err)
	}
	if strings.Contains(string(kept), secret) || strings.Contains(string(kept), "grafana_data_source") {
		t.Errorf("the ledger holds what isn't a rule group:\n%s", kept)
	}
	if !strings.Contains(string(kept), "grafana_rule_group") || !strings.Contains(string(kept), "CheckoutErrors") {
		t.Errorf("the ledger doesn't hold the rule group:\n%s", kept)
	}
}
