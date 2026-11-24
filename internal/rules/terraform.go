package rules

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// terraformType is the Terraform resource a Grafana rule group is managed
// as, in the Grafana provider.
const terraformType = "grafana_rule_group"

// terraformDepth is how many modules deep a module tree is read: Terraform
// writes none near it.
const terraformDepth = 64

// isTerraform reports whether doc is the JSON `terraform show -json` writes
// of a plan (planned_values) or of a state (values): an object with a
// format_version and a root_module in one of them.
func isTerraform(doc map[string]any) bool {
	if doc["format_version"] == nil {
		return false
	}
	for _, k := range []string{"planned_values", "values"} {
		if v, ok := doc[k].(map[string]any); ok {
			if _, ok := v["root_module"].(map[string]any); ok {
				return true
			}
		}
	}
	return false
}

// terraformRules reads the grafana_rule_group resources of Terraform's JSON
// (a saved plan's, or a state's), each as the Grafana alerting provisioning
// file it would write, so that its rules are read as a Grafana file's are.
// Where they came from is each rule's Address.
//
// A plan has the values the resources will have (planned_values): a
// resource the plan destroys isn't among them, and so isn't read, which
// is right, as it won't be there. A value Terraform can't know before it
// applies (a folder_uid taken from a folder made in the same plan, a rule's
// uid) is left out of the JSON, and so is empty here.
func terraformRules(ctx context.Context, doc map[string]any, rec *[]Recording) ([]Rule, bool, error) {
	top, _ := doc["planned_values"].(map[string]any)
	if _, ok := top["root_module"].(map[string]any); !ok {
		top, _ = doc["values"].(map[string]any)
	}
	root, _ := top["root_module"].(map[string]any)
	var out []Rule
	found := false
	var walk func(m map[string]any, depth int) error
	walk = func(m map[string]any, depth int) error {
		for _, r := range list(m["resources"]) {
			rm, _ := r.(map[string]any)
			if str(rm["mode"]) != "managed" || str(rm["type"]) != terraformType {
				continue
			}
			addr := str(rm["address"])
			if addr == "" {
				addr = terraformType + "." + str(rm["name"])
			}
			values, _ := rm["values"].(map[string]any)
			g, err := terraformGroup(addr, values)
			if err != nil {
				return err
			}
			b, err := json.Marshal(map[string]any{"apiVersion": 1, "groups": []any{g}})
			if err != nil {
				return fmt.Errorf("%s: %w", addr, err)
			}
			// The file it would be, charged as what rules make of the input.
			if err := inputOf(ctx).charge(len(b), func() string {
				return fmt.Sprintf("%s, written as a Grafana provisioning file, %d bytes", addr, len(b))
			}); err != nil {
				return err
			}
			var n yaml.Node
			if err := yaml.NewDecoder(bytes.NewReader(b)).Decode(&n); err != nil {
				return fmt.Errorf("%s: %w", addr, err)
			}
			prov, err := YAMLValueContext(ctx, &n)
			if err != nil {
				return fmt.Errorf("%s: %w", addr, err)
			}
			pm, _ := prov.(map[string]any)
			rs, err := grafana(ctx, pm, rec)
			if err != nil {
				return fmt.Errorf("%s: %w", addr, err)
			}
			for i := range rs {
				rs[i].Address = addr
			}
			found = true
			out = append(out, rs...)
		}
		kids := list(m["child_modules"])
		if depth >= terraformDepth && len(kids) > 0 {
			return fmt.Errorf("Terraform's JSON: modules nested more than %d deep", terraformDepth)
		}
		for _, c := range kids {
			cm, _ := c.(map[string]any)
			if err := walk(cm, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, 0); err != nil {
		return nil, true, err
	}
	return out, found, nil
}

// escaped is s as a Grafana provisioning file writes it where the file's
// reader takes $$ for a $ (provisioned): Terraform's values are as they are
// in Grafana, not as a file writes them.
func escaped(s string) string { return strings.ReplaceAll(s, "$", "$$") }

// put sets m[k] to v unless v is empty or null, as Terraform leaves an
// attribute it has no value for.
func put(m map[string]any, k string, v any) {
	switch x := v.(type) {
	case nil:
		return
	case string:
		if x == "" {
			return
		}
	case []any:
		if len(x) == 0 {
			return
		}
	case map[string]any:
		if len(x) == 0 {
			return
		}
	}
	m[k] = v
}

// escapedMap is a map of strings with each value escaped.
func escapedMap(v any) map[string]any {
	in, _ := v.(map[string]any)
	out := map[string]any{}
	for k, x := range in {
		if s, ok := x.(string); ok {
			out[k] = escaped(s)
		}
	}
	return out
}

// first is the one block of a Terraform block list that holds at most one.
func first(v any) map[string]any {
	l := list(v)
	if len(l) == 0 {
		return nil
	}
	m, _ := l[0].(map[string]any)
	return m
}

// terraformGroup is a grafana_rule_group's values, v, as a group of a
// Grafana provisioning file. Its blocks are lists of objects.
func terraformGroup(addr string, v map[string]any) (map[string]any, error) {
	g := map[string]any{}
	put(g, "orgId", str(v["org_id"]))
	put(g, "name", escaped(str(v["name"])))
	put(g, "folder", escaped(str(v["folder_uid"])))
	if s, ok := toFloat(v["interval_seconds"]); ok && s > 0 {
		g["interval"] = fmt.Sprintf("%ds", int64(s))
	}
	rules := []any{}
	for _, r := range list(v["rule"]) {
		rm, _ := r.(map[string]any)
		title := str(rm["name"])
		rule := map[string]any{}
		put(rule, "uid", escaped(str(rm["uid"])))
		put(rule, "title", escaped(title))
		put(rule, "condition", escaped(str(rm["condition"])))
		put(rule, "for", escaped(str(rm["for"])))
		put(rule, "keepFiringFor", escaped(str(rm["keep_firing_for"])))
		put(rule, "noDataState", escaped(str(rm["no_data_state"])))
		put(rule, "execErrState", escaped(str(rm["exec_err_state"])))
		put(rule, "labels", escapedMap(rm["labels"]))
		put(rule, "annotations", rm["annotations"])
		if p, ok := rm["is_paused"].(bool); ok && p {
			rule["isPaused"] = true
		}
		if n, ok := toFloat(rm["missing_series_evals_to_resolve"]); ok && n > 0 {
			rule["missing_series_evals_to_resolve"] = int64(n)
		}
		if ns := first(rm["notification_settings"]); ns != nil {
			set := map[string]any{}
			put(set, "receiver", escaped(str(ns["contact_point"])))
			for from, to := range map[string]string{"group_by": "group_by", "mute_timings": "mute_time_intervals", "active_timings": "active_time_intervals"} {
				var l []any
				for _, x := range list(ns[from]) {
					l = append(l, escaped(str(x)))
				}
				put(set, to, l)
			}
			for _, k := range []string{"group_wait", "group_interval", "repeat_interval"} {
				put(set, k, escaped(str(ns[k])))
			}
			put(rule, "notification_settings", set)
		}
		if rec := first(rm["record"]); rec != nil {
			put(rule, "record", map[string]any{"metric": escaped(str(rec["metric"])), "from": escaped(str(rec["from"])),
				"targetDatasourceUid": escaped(str(rec["target_datasource_uid"]))})
		}
		data := []any{}
		for _, d := range list(rm["data"]) {
			dm, _ := d.(map[string]any)
			ref := str(dm["ref_id"])
			q := map[string]any{}
			put(q, "refId", escaped(ref))
			put(q, "datasourceUid", escaped(str(dm["datasource_uid"])))
			put(q, "queryType", escaped(str(dm["query_type"])))
			if t := first(dm["relative_time_range"]); t != nil {
				q["relativeTimeRange"] = map[string]any{"from": t["from"], "to": t["to"]}
			}
			if s := str(dm["model"]); s != "" {
				dec := json.NewDecoder(strings.NewReader(s))
				dec.UseNumber()
				var model any
				if err := dec.Decode(&model); err != nil {
					return nil, fmt.Errorf("%s: rule %q: query %q: its model isn't JSON: %w", addr, title, ref, err)
				}
				if dec.More() {
					return nil, fmt.Errorf("%s: rule %q: query %q: its model isn't one JSON value", addr, title, ref)
				}
				q["model"] = model
			}
			data = append(data, q)
		}
		rule["data"] = data
		rules = append(rules, rule)
	}
	g["rules"] = rules
	return g, nil
}
