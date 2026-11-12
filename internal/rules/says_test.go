package rules

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

// What a rule says is read from every source alike, as each writes it, and
// doesn't change what decides when the rule fires.
func TestSays(t *testing.T) {
	promRule := `- alert: Down
        expr: up == 0
        labels: {severity: page}
        annotations:
          summary: "{{ $labels.instance }} is down"
          description: "It has been down for 5m."
          runbook_url: https://runbooks.example/down`
	amp := base64.StdEncoding.EncodeToString([]byte("groups:\n  - name: g\n    rules:\n      " + promRule + "\n"))
	for name, c := range map[string]struct {
		doc  string
		want Says
	}{
		"a Prometheus rule file": {"groups:\n  - name: g\n    rules:\n      " + promRule + "\n",
			Says{Summary: "{{ $labels.instance }} is down", Description: "It has been down for 5m.", Runbook: "https://runbooks.example/down", Template: TemplatePrometheus}},
		"a PrometheusRule": {"kind: PrometheusRule\nspec:\n  groups:\n    - name: g\n      rules:\n        " + strings.ReplaceAll(promRule, "\n        ", "\n          ") + "\n",
			Says{Summary: "{{ $labels.instance }} is down", Description: "It has been down for 5m.", Runbook: "https://runbooks.example/down", Template: TemplatePrometheus}},
		"Google Cloud's Rules": {"apiVersion: monitoring.googleapis.com/v1\nkind: ClusterRules\nspec:\n  groups:\n    - name: g\n      rules:\n        " + strings.ReplaceAll(promRule, "\n        ", "\n          ") + "\n",
			Says{Summary: "{{ $labels.instance }} is down", Description: "It has been down for 5m.", Runbook: "https://runbooks.example/down", Template: TemplatePrometheus}},
		"Amazon Managed Service for Prometheus": {`{"ruleGroupsNamespace": {"name": "n", "data": "` + amp + `"}}`,
			Says{Summary: "{{ $labels.instance }} is down", Description: "It has been down for 5m.", Runbook: "https://runbooks.example/down", Template: TemplatePrometheus}},
		"Azure's Prometheus rule group": {`{"type": "Microsoft.AlertsManagement/prometheusRuleGroups", "name": "g", "properties": {"interval": "PT1M", "rules": [
			{"alert": "Down", "expression": "up == 0", "severity": 2, "annotations": {"summary": "down", "runbook": "https://runbooks.example/down"}}]}}`,
			Says{Summary: "down", Runbook: "https://runbooks.example/down", Template: TemplatePrometheus}},
		"Grafana's provisioning": {`apiVersion: 1
groups:
  - name: g
    folder: f
    interval: 1m
    rules:
      - uid: u1
        title: Down
        condition: C
        data:
          - refId: A
            datasourceUid: prom
            model: {expr: up, instant: true}
          - refId: C
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: lt, params: [1]}}]}
        annotations: {summary: "{{ $labels.instance }} down", description: "Down."}
`, Says{Summary: "{{ $labels.instance }} down", Description: "Down.", Template: TemplateGrafana}},
		"a CloudWatch alarm": {`{"MetricAlarms": [{"AlarmName": "cpu-high", "AlarmDescription": "CPU over 90%. See https://runbooks.example/cpu for what to do."}]}`,
			Says{Summary: "cpu-high", Description: "CPU over 90%. See https://runbooks.example/cpu for what to do.", Runbook: "https://runbooks.example/cpu", NoSeverity: true}},
		"a CloudWatch composite alarm": {`{"CompositeAlarms": [{"AlarmName": "both", "AlarmDescription": "Both are high."}]}`,
			Say(Says{Summary: "both", Description: "Both are high.", NoSeverity: true})},
		"a Google Cloud policy": {`{"displayName": "Latency", "severity": "WARNING", "documentation": {"subject": "Latency on ${resource.label.service}",
			"content": "p99 over 1s.", "links": [{"displayName": "runbook", "url": "https://runbooks.example/latency"}]},
			"conditions": [{"conditionThreshold": {"filter": "metric.type=x"}}]}`,
			Says{Summary: "Latency on ${resource.label.service}", Description: "p99 over 1s.", Runbook: "https://runbooks.example/latency", Template: TemplateGoogle, Severity: "warning"}},
		"an Azure Monitor metric alert, as the CLI lists it": {`[{"type": "Microsoft.Insights/metricAlerts", "name": "cpu-high", "resourceGroup": "rg",
			"description": "CPU high. https://runbooks.example/cpu", "severity": 2, "enabled": true}]`,
			Says{Summary: "cpu-high", Description: "CPU high. https://runbooks.example/cpu", Runbook: "https://runbooks.example/cpu", Severity: "Sev2"}},
		"an Azure Monitor log search alert, as the API answers": {`{"type": "Microsoft.Insights/scheduledQueryRules", "name": "errors",
			"properties": {"displayName": "Errors in the log", "description": "", "severity": 1}}`,
			Says{Summary: "Errors in the log", Severity: "Sev1"}},
		"an Azure Monitor activity log alert": {`[{"type": "Microsoft.Insights/activityLogAlerts", "name": "deleted", "description": "A resource was deleted."}]`,
			Says{Summary: "deleted", Description: "A resource was deleted.", NoSeverity: true}},
		// oci monitoring alarm list: an AlarmSummary each, with no body.
		"OCI's alarms, as the CLI lists them": {`{"data": [{"id": "ocid1.alarm.oc1..a", "display-name": "disk-full", "namespace": "oci_computeagent",
			"compartment-id": "ocid1.compartment.oc1..c", "metric-compartment-id": "ocid1.compartment.oc1..c", "query": "DiskUtilization[1m].mean() > 90",
			"severity": "CRITICAL", "destinations": ["ocid1.onstopic.oc1..t"], "suppression": null, "is-enabled": true, "lifecycle-state": "ACTIVE",
			"alarm-summary": "Disk over 90% on {{resourceDisplayName}}", "notification-title": "Disk full", "freeform-tags": {}, "defined-tags": {}}]}`,
			Says{Summary: "disk-full", Severity: "critical", TextUnknown: true}},
		// Each alarm as the API's GetAlarm gives it, in a list: with its body.
		"OCI's alarms, each with its body": {`{"data": [{"id": "ocid1.alarm.oc1..b", "displayName": "mem", "severity": "WARNING",
			"body": "Memory. Runbook: https://runbooks.example/mem"}]}`,
			Says{Summary: "mem", Description: "Memory. Runbook: https://runbooks.example/mem", Runbook: "https://runbooks.example/mem", Severity: "warning"}},
	} {
		rs, err := Parse([]byte(c.doc))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(rs) != 1 {
			t.Errorf("%s: %d rules", name, len(rs))
			continue
		}
		got := rs[0].Says
		got.Annotations = nil // TestSaysAnnotations
		if !got.Equal(c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", name, rs[0].Says, c.want)
		}
	}
	// The rules API, as --loaded-rules reads it.
	rs, err := FromAPI([]byte(`{"status": "success", "data": {"groups": [{"name": "g", "rules": [{"type": "alerting", "name": "Down", "query": "up == 0",
		"annotations": {"summary": "down", "runbook_url": "https://runbooks.example/down"}}]}]}}`))
	if err != nil || len(rs) != 1 || !rs[0].Says.Equal(Says{Summary: "down", Runbook: "https://runbooks.example/down", Template: TemplatePrometheus,
		Annotations: map[string]string{"runbook_url": "https://runbooks.example/down"}}) {
		t.Errorf("the rules API: %+v %v", rs, err)
	}
}

// Say is a Says, so a table can spell one out.
func Say(s Says) Says { return s }

// What a rule says doesn't decide when it fires: a new summary is no new
// version to review or judge again. Each text is cut to a bound.
func TestSaysApart(t *testing.T) {
	a, err := Parse([]byte("groups:\n  - name: g\n    rules:\n      - {alert: A, expr: up == 0, annotations: {summary: one}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]byte("groups:\n  - name: g\n    rules:\n      - {alert: A, expr: up == 0, annotations: {summary: two, description: '" + strings.Repeat("x", 9000) + "'}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Key() != b[0].Key() {
		t.Error("a new summary changed the rule's key")
	}
	if len(b[0].Says.Description) != maxSays {
		t.Errorf("a description of %d bytes kept", len(b[0].Says.Description))
	}
}

// Azure Monitor's and OCI's own alerts are listed, not replayed, with why,
// as CloudWatch's metric alarms are; turned off, they say so.
func TestCloudAlertsListed(t *testing.T) {
	rs, err := Parse([]byte(`[{"type": "Microsoft.Insights/metricAlerts", "name": "a", "enabled": false},
		{"type": "microsoft.insights/scheduledqueryrules", "name": "b"}]`))
	if err != nil || len(rs) != 2 || rs[0].Unsupported != Disabled || !strings.Contains(rs[1].Unsupported, "log search alert") || rs[1].Cloud != "azure" {
		t.Errorf("Azure: %+v %v", rs, err)
	}
	rs, err = Parse([]byte(`{"data": [{"id": "ocid1.alarm.oc1..a", "display-name": "x", "is-enabled": false}, {"id": "ocid1.alarm.oc1..b", "display-name": "y"}]}`))
	if err != nil || len(rs) != 2 || rs[0].Unsupported != Disabled || !strings.Contains(rs[1].Unsupported, "MQL") || rs[1].Cloud != "oci" {
		t.Errorf("OCI: %+v %v", rs, err)
	}
	if _, err := Parse([]byte(`{"data": [{"id": "ocid1.instance.oc1..a"}]}`)); err == nil {
		t.Error("an OCI list of something else read as alarms")
	}
}

// An Azure Monitor alert with no name is passed over, as every source's is;
// a severity Azure doesn't give (0 to 4, whole) isn't read as one. A Google
// Cloud policy turned off is off, whatever its conditions are written in.
func TestCloudOdd(t *testing.T) {
	rs, err := Parse([]byte(`[{"type": "Microsoft.Insights/metricAlerts", "name": "", "severity": 1},
		{"type": "Microsoft.Insights/metricAlerts", "properties": {"severity": 2}},
		{"type": "Microsoft.Insights/metricAlerts", "name": "huge", "severity": 1e300},
		{"type": "Microsoft.Insights/metricAlerts", "name": "five", "severity": 5},
		{"type": "Microsoft.Insights/metricAlerts", "name": "half", "severity": 1.5},
		{"type": "Microsoft.Insights/metricAlerts", "name": "two", "severity": "2"}]`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rs {
		got = append(got, r.Name+"="+r.Labels["severity"]+"/"+r.Says.Severity)
	}
	if want := []string{"huge=/", "five=/", "half=/", "two=Sev2/Sev2"}; !slices.Equal(got, want) {
		t.Errorf("Azure: %q, want %q", got, want)
	}
	rs, err = Parse([]byte(`{"type": "Microsoft.AlertsManagement/prometheusRuleGroups", "name": "g", "properties": {"interval": "PT1M", "rules": [
		{"alert": "A", "expression": "up == 0", "severity": 1e300}, {"alert": "B", "expression": "up == 0", "severity": 3}]}}`))
	if err != nil || len(rs) != 2 || rs[0].Labels["severity"] != "" || rs[1].Labels["severity"] != "Sev3" {
		t.Errorf("Azure's Prometheus rule group: %+v %v", rs, err)
	}
	rs, err = Parse([]byte(`{"displayName": "cpu", "enabled": false, "conditions": [{"displayName": "cpu", "conditionThreshold": {"filter": "x"}},
		{"displayName": "q", "conditionPrometheusQueryLanguage": {"query": "up == 0"}}]}`))
	if err != nil || len(rs) != 2 || rs[0].Unsupported != Disabled || rs[1].Unsupported != Disabled {
		t.Errorf("a Google Cloud policy turned off: %+v %v", rs, err)
	}
}
