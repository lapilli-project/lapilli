package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// A Terraform plan RENDER writes holds every resource's values, the
// sensitive ones among them: what the recipe sends serve holds the plan's
// rule groups, and nothing else of it.
func TestBacktestRecipeTerraformRuleGroupsOnly(t *testing.T) {
	parallel(t)
	needRecipeTools(t)
	raw, err := os.ReadFile("../../internal/rules/testdata/terraform-plan-real.json")
	if err != nil {
		t.Fatal(err)
	}
	const secret = "s3cr3t-basic-auth-password"
	plan := func(forDur string) string {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		ds := map[string]any{"address": "grafana_data_source.p", "mode": "managed", "type": "grafana_data_source", "name": "p",
			"values": map[string]any{"secure_json_data_encoded": secret}}
		root := doc["planned_values"].(map[string]any)["root_module"].(map[string]any)
		root["resources"] = append(root["resources"].([]any), ds)
		root["child_modules"] = []any{map[string]any{"address": "module.m", "resources": []any{ds}}}
		b, _ := json.Marshal(doc)
		return strings.ReplaceAll(string(b), `"for":"5m"`, fmt.Sprintf(`"for":%q`, forDur))
	}
	repo := newPRRepo(t, map[string]string{"plan.json": plan("5m")})
	repo.pullRequest(map[string]string{"plan.json": plan("10m")})
	srv := newServeStandIn(t)
	srv.answers(nil, []serveAnswer{serveDone(serveMarkdown(changedSummary))})
	sr := serveRun(t, backtestHosts[0], repo, srv)
	if out, err := sr.run([]string{"RENDER=cat plan.json"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	sent, _ := json.Marshal(srv.sent())
	if len(sent) < 10 || !strings.Contains(string(sent), "grafana_rule_group.checkout") {
		t.Fatalf("what was sent holds no rule group: %s", sent)
	}
	for _, not := range []string{secret, "grafana_data_source", "admin:admin", "resource_changes"} {
		if strings.Contains(string(sent), not) {
			t.Errorf("what was sent holds %q", not)
		}
	}
}
