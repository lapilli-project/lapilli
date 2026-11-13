package rules

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A Prometheus group's labels and limit are its rules' apart from their
// own: under them in what their alerts carry, which the rule's own
// override, and in Whole, never in Key, which a rule's name and its
// history in lapilli serve hang on. Read from a rules API, which gives the
// labels in the rule's own, the rule is the same as a whole.
func TestGroupLabelsAndLimit(t *testing.T) {
	rs, err := Parse([]byte(`groups:
  - name: shop
    labels: {team: shop, severity: warning}
    limit: 10
    rules:
      - alert: Errors
        expr: errors > 5
        labels: {severity: page}
  - name: plain
    rules:
      - alert: Errors
        expr: errors > 5
        labels: {severity: page}
`))
	if err != nil || len(rs) != 2 {
		t.Fatal(rs, err)
	}
	grouped, plain := rs[0], rs[1]
	if want := map[string]string{"team": "shop", "severity": "page"}; !maps.Equal(grouped.AlertLabels(), want) {
		t.Errorf("alert labels %v, want %v: the rule's own over its group's", grouped.AlertLabels(), want)
	}
	if !maps.Equal(grouped.Labels, map[string]string{"severity": "page"}) || grouped.Limit != 10 ||
		!maps.Equal(grouped.GroupLabels, map[string]string{"team": "shop", "severity": "warning"}) {
		t.Errorf("read as %+v", grouped)
	}
	if !maps.Equal(plain.AlertLabels(), plain.Labels) || plain.GroupLabels != nil {
		t.Errorf("a group without labels: %+v", plain)
	}
	// Its key is as before Lapilli read either; its whole has both.
	again := grouped
	again.Group, again.GroupLabels, again.Limit = "plain", nil, 0
	if again.Key() != plain.Key() {
		t.Errorf("a group's labels or limit in the key:\n%s\n%s", again.Key(), plain.Key())
	}
	for name, r := range map[string]Rule{
		"labels": func() Rule { r := grouped; r.Limit = 0; return r }(),
		"limit":  func() Rule { r := grouped; r.GroupLabels = nil; return r }(),
	} {
		if r.Key() != grouped.Key() || r.Whole() == grouped.Whole() {
			t.Errorf("a group's %s: key or whole wrong", name)
		}
	}
	// The same rule, as Prometheus 3's API answers: its group's labels in
	// its own, its group's limit.
	api, err := FromAPI([]byte(`{"status":"success","data":{"groups":[{"name":"shop","interval":0,"limit":10,"rules":[
		{"type":"alerting","name":"Errors","query":"errors > 5","labels":{"team":"shop","severity":"page"}}]}]}}`))
	if err != nil || len(api) != 1 {
		t.Fatal(api, err)
	}
	api[0].Source = grouped.Source
	if api[0].Whole() != grouped.Whole() || api[0].Limit != 10 {
		t.Errorf("read from the API, another rule:\n%s\n%s", api[0].Whole(), grouped.Whole())
	}
}

// Recording rules are read wherever they are: with their group's labels
// under their own; in an Azure rule group, alone or in an ARM template; in
// a Grafana provisioning file, where they are no alert rules.
func TestRecordingsEverywhere(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"prometheus.yaml": `groups:
  - name: shop
    labels: {team: shop}
    rules:
      - record: shop:errors
        expr: sum(errors)
        labels: {kept: "yes"}
      - alert: Errors
        expr: shop:errors > 5
`,
		"azure.json": `{"type": "Microsoft.AlertsManagement/prometheusRuleGroups", "name": "az", "properties": {"interval": "PT1M", "clusterName": "prod", "rules": [
  {"record": "az:errors", "expression": "sum(errors)"},
  {"record": "az:off", "expression": "sum(off)", "enabled": false},
  {"alert": "AzErrors", "expression": "az:errors > 5"}]}}`,
		"arm.json": `{"resources": [{"type": "Microsoft.AlertsManagement/prometheusRuleGroups", "name": "arm", "properties": {"rules": [
  {"record": "arm:errors", "expression": "sum(errors)"},
  {"alert": "ArmErrors", "expression": "arm:errors > 5"}]}}]}`,
		"grafana.yaml": `apiVersion: 1
groups:
  - orgId: 1
    name: rec
    folder: shop
    interval: 1m
    rules:
      - uid: r1
        title: Errors recorded
        data:
          - refId: A
            datasourceUid: prom
            model: {expr: sum(errors), refId: A}
        record: {metric: grafana:errors, from: A, targetDatasourceUid: prom}
      - uid: a1
        title: Grafana errors
        condition: A
        data:
          - refId: A
            datasourceUid: prom
            model: {expr: grafana:errors > 5, refId: A}
`,
		"grafana-recording-only.yaml": `apiVersion: 1
groups:
  - orgId: 1
    name: only
    folder: shop
    interval: 1m
    rules:
      - uid: r2
        title: Only recorded
        data:
          - refId: A
            datasourceUid: prom
            model: {expr: sum(more), refId: A}
        record: {metric: grafana:more, from: A}
`,
	} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
	}
	var recs []Recording
	rs, _, _, err := ReadPicked(dir, Picking{Recordings: &recs})
	if err != nil {
		t.Fatal(err)
	}
	var alerts []string
	for _, r := range rs {
		alerts = append(alerts, r.Name)
	}
	slices.Sort(alerts)
	if want := []string{"ArmErrors", "AzErrors", "Errors", "Grafana errors"}; !slices.Equal(alerts, want) {
		t.Errorf("alert rules %q, want %q", alerts, want)
	}
	by := map[string]Recording{}
	for _, r := range recs {
		by[r.Record] = r
	}
	if len(recs) != 6 {
		t.Errorf("%d recording rules: %+v", len(recs), recs)
	}
	if r := by["shop:errors"]; !maps.Equal(r.Labels, map[string]string{"team": "shop", "kept": "yes"}) || r.Expr != "sum(errors)" {
		t.Errorf("a Prometheus recording rule: %+v", r)
	}
	if r := by["az:errors"]; r.Group != "az" || r.Expr != `sum(errors{cluster="prod"})` || r.Off || filepath.Base(r.Source) != "azure.json" {
		t.Errorf("an Azure recording rule: %+v", r)
	}
	if r := by["az:off"]; !r.Off {
		t.Errorf("an Azure recording rule not enabled: %+v", r)
	}
	if r := by["arm:errors"]; r.Group != "arm" {
		t.Errorf("an ARM template's recording rule: %+v", r)
	}
	g := by["grafana:errors"]
	if g.Group != "shop/rec" || g.Expr == "" || g.Interval.Minutes() != 1 {
		t.Errorf("a Grafana recording rule: %+v", g)
	}
	if r := by["grafana:more"]; r.Group != "shop/only" {
		t.Errorf("a Grafana file of recording rules alone: %+v", r)
	}
	// What a Grafana recording rule records from is its queries.
	os.WriteFile(filepath.Join(dir, "grafana.yaml"), []byte(`apiVersion: 1
groups:
  - orgId: 1
    name: rec
    folder: shop
    interval: 1m
    rules:
      - uid: r1
        title: Errors recorded
        data:
          - refId: A
            datasourceUid: prom
            model: {expr: sum(errors) * 0, refId: A}
        record: {metric: grafana:errors, from: A, targetDatasourceUid: prom}
`), 0o600)
	recs = nil
	if _, _, _, err := ReadPicked(dir, Picking{Recordings: &recs}); err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Record == "grafana:errors" && r.Key() == g.Key() {
			t.Errorf("a Grafana recording rule's query changed, its key didn't")
		}
	}
}

// A limit no int holds, or none at all, is no limit read.
func TestGroupLimitOutOfRange(t *testing.T) {
	for _, limit := range []string{"99999999999999999999", "0.5", "-3", "lots"} {
		rs, err := Parse([]byte("groups:\n  - name: g\n    limit: " + limit + "\n    rules:\n      - {alert: A, expr: up == 0}\n"))
		if err != nil || len(rs) != 1 || rs[0].Limit != 0 {
			t.Errorf("limit %s: %+v %v", limit, rs, err)
		}
	}
}
