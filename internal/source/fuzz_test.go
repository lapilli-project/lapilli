package source

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
)

// What a ruler answers is read, whatever it is: answered or refused, never
// a panic (docs/design/rulers.md). Seeded with the answers Mimir 3.2.4
// gave and those Amazon's and Azure's API documents show.

func seed(f *testing.F, name string) {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(b)
}

func FuzzConfigDocs(f *testing.F) {
	seed(f, "mimir-3.2.4-list.yaml")
	seed(f, "mimir-3.2.4-indicator-list.yaml")
	f.Add([]byte("{}\n"))
	f.Add([]byte("a: [{name: g, rules: [{alert: A, expr: up == 0, for: 5m}]}]\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		docs, _, err := ConfigDocs("http://mimir/rules", b)
		if err != nil {
			return
		}
		rules.FromDocs(docs)
	})
}

func FuzzAmazon(f *testing.F) {
	seed(f, "amazon-list.json")
	seed(f, "amazon-describe.json")
	f.Fuzz(func(t *testing.T, b []byte) {
		AmazonList(b)
		if _, data, err := AmazonDescribed(b); err == nil {
			rules.Parse(data)
		}
	})
}

func FuzzAzure(f *testing.F) {
	seed(f, "azure-list.json")
	f.Fuzz(func(t *testing.T, b []byte) {
		items, _, err := AzureList(b)
		if err != nil {
			return
		}
		r := &Ruler{URL: "https://management.azure.com/subscriptions/sub-1", Kind: RulerAzure}
		for _, it := range items {
			if d, ok, err := r.azureDoc(it); err == nil && ok {
				rules.Parse(d.Data)
				AzureHash(d.Data)
				rules.Shape(d.Data, AzureSets)
			}
		}
	})
}

// A cloud's alarms, as each API answers them (docs/design/alarms.md): read
// as documents, then as rules, whatever the answer is.
func FuzzAlarms(f *testing.F) {
	f.Add(byte(0), []byte(`{"MetricAlarms":[{"AlarmName":"a","AlarmArn":"arn:aws:cloudwatch:eu-west-1:111122223333:alarm:a","StateValue":"ALARM",
  "EvaluationCriteria":{"PromQLCriteria":{"Query":"up == 0","PendingPeriod":300}}}],"CompositeAlarms":[{"AlarmName":"c",
  "AlarmArn":"arn:aws:cloudwatch:eu-west-1:111122223333:alarm:c","AlarmRule":"ALARM(a)","ActionsEnabled":false}],"NextToken":"x"}`))
	f.Add(byte(1), []byte(`{"value":[{"id":"/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Insights/metricAlerts/m","name":"m",
  "type":"Microsoft.Insights/metricAlerts","properties":{"severity":3,"enabled":true,"evaluationFrequency":"PT1M","criteria":
  {"odata.type":"Microsoft.Azure.Monitor.PromQLCriteria","failingPeriods":{"for":"PT5M"},"allOf":[{"name":"M","criterionType":
  "StaticThresholdCriterion","query":"up > 1"}]},"lastUpdatedTime":"2026-11-01T00:00:00Z"}}],"nextLink":""}`))
	f.Add(byte(2), []byte(`{"alertPolicies":[{"name":"projects/p/alertPolicies/1","displayName":"D","enabled":true,"conditions":[
  {"name":"projects/p/alertPolicies/1/conditions/2","conditionPrometheusQueryLanguage":{"query":"up == 0","duration":"60s"}}],
  "mutationRecord":{"mutateTime":"2026-11-01T00:00:00Z"}}],"nextPageToken":"t"}`))
	f.Add(byte(3), []byte(`{"id":"ocid1.alarm.oc1.ap-seoul-1.a1","displayName":"d","compartmentId":"ocid1.compartment.oc1..c",
  "namespace":"oci_computeagent","query":"CpuUtilization[1m].mean() > 90","severity":"CRITICAL","isEnabled":true,"body":"b",
  "timeUpdated":"2026-11-01T00:00:00Z","lifecycleState":"ACTIVE"}`))
	// Whether an alarm fires, as Azure's, Google's and OCI's answers say.
	f.Add(byte(4), []byte(`{"value":[{"id":"/subscriptions/sub-1/providers/Microsoft.AlertsManagement/alerts/1","properties":{"essentials":
  {"alertRule":"/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Insights/metricAlerts/m","monitorCondition":"Fired",
  "alertState":"New","startDateTime":"2025-11-01T12:00:00Z"}}}],"nextLink":"x"}`))
	f.Add(byte(5), []byte(`{"alerts":[{"name":"projects/p/alerts/1","state":"OPEN","policy":{"name":"projects/p/alertPolicies/1"}}],"nextPageToken":"t"}`))
	f.Add(byte(6), []byte(`[{"id":"ocid1.alarm.oc1.ap-seoul-1.a1","status":"FIRING"}]`))
	f.Fuzz(func(t *testing.T, kind byte, b []byte) {
		var docs []any
		switch kind % 7 {
		case 4:
			AzureFired(b, "/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Insights/metricAlerts/m", 5*time.Minute, time.Unix(1762000000, 0))
			AzureStateless(b)
		case 5:
			GoogleOpen(b, "projects/p/alertPolicies/1")
		case 6:
			OCIFiring(b, "ocid1.alarm.oc1.ap-seoul-1.a1")
		case 0:
			page, err := ReadAWSAlarms(b)
			if err != nil {
				return
			}
			for _, m := range page.MetricAlarms {
				docs = append(docs, map[string]any{"MetricAlarms": []any{Definition(AlarmsAWS, m)}})
			}
			for _, m := range page.CompositeAlarms {
				docs = append(docs, map[string]any{"CompositeAlarms": []any{Definition(AlarmsAWS, m)}})
			}
		case 1:
			items, _, err := AzureList(b)
			if err != nil {
				return
			}
			for _, it := range items {
				var m map[string]any
				if decode(it, &m) == nil && m != nil {
					docs = append(docs, Definition(AlarmsAzure, m))
				}
			}
		case 2:
			page, err := ReadGooglePolicies(b)
			if err != nil {
				return
			}
			for _, m := range page.AlertPolicies {
				docs = append(docs, Definition(AlarmsGoogle, m))
			}
		case 3:
			var m map[string]any
			if decode(b, &m) != nil || m == nil {
				return
			}
			docs = append(docs, map[string]any{"data": []any{ociDefinition(m)}})
		}
		a := &Alarms{Source: "s"}
		for _, d := range docs {
			if doc, err := a.doc("id", d); err == nil {
				rules.Parse(doc.Data)
				rules.TreeHash(doc.Data)
			}
		}
	})
}
