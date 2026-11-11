package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/oracle/oci-go-sdk/v65/common"

	"lapilli.dev/internal/alarmtest"
	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/rules"
)

// Reading a cloud's alarms through its API (docs/design/alarms.md), each
// cloud against its stand-in.

func TestAlarmsKind(t *testing.T) {
	for raw, want := range map[string]string{
		"https://monitoring.eu-west-1.amazonaws.com":                                                                                         AlarmsAWS,
		"https://monitoring-fips.us-east-1.amazonaws.com/":                                                                                   AlarmsAWS,
		"https://vpce-0123-abcd.monitoring.eu-west-1.vpce.amazonaws.com":                                                                     AlarmsAWS,
		"https://management.azure.com/subscriptions/sub-1":                                                                                   AlarmsAzure,
		"https://management.azure.com/subscriptions/sub-1/resourceGroups/shop-rg":                                                            AlarmsAzure,
		"https://monitoring.googleapis.com/v3/projects/shop-prod/alertPolicies":                                                              AlarmsGoogle,
		"https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms?compartmentId=ocid1.compartment.oc1..aaaa":                             AlarmsOCI,
		"https://telemetry.us-ashburn-1.oraclecloud.com/20180401/alarms?compartmentId=ocid1.tenancy.oc1..aaaa&compartmentIdInSubtree=true":   AlarmsOCI,
		"http://monitoring.eu-west-1.amazonaws.com":                                                                                          "",
		"https://aps.eu-west-1.amazonaws.com/workspaces/ws-1":                                                                                "",
		"https://monitoring.eu-west-1.amazonaws.com/alarms":                                                                                  "",
		"https://management.azure.com/subscriptions/sub-1/providers/x":                                                                       "",
		"https://monitoring.googleapis.com/v3/projects/shop/alertPolicies/123":                                                               "",
		"https://monitoring.googleapis.com.example.net/v3/projects/shop/alertPolicies":                                                       "",
		"https://telemetry.ap-seoul-1.oraclecloud00.com/20180401/alarms?compartmentId=ocid1.compartment.oc1..aaaa":                           "",
		"https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms":                                                                       "",
		"https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms?compartmentId=ocid1.compartment.oc1..aaaa&compartmentIdInSubtree=true": "",
		"https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms?compartmentId=ocid1.compartment.oc1..aaaa&page=x":                      "",
		"https://user@monitoring.eu-west-1.amazonaws.com":                                                                                    "",
		"https://example.com/alarms":                                                                                                         "",
	} {
		got, err := AlarmsKind(raw)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("%s: %q %v, want %q", raw, got, err, want)
		}
	}
	if _, err := NewAlarms("Bad_Source", "https://monitoring.eu-west-1.amazonaws.com", "t"); err == nil {
		t.Error("a source not in recorded.md's form")
	}
}

// signers stand in for each cloud's credentials, as the tests' are: Amazon
// signed with SigV4 for monitoring, OCI with a test key, the others a
// bearer token.
func signer(kind, region string) func(*http.Request) error {
	switch kind {
	case AlarmsAWS:
		creds := aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""))
		return cloud.SignRequest(creds, "monitoring", region, time.Now)
	case AlarmsOCI:
		_, pem := alarmtest.OCIKey()
		p := common.NewRawConfigurationProvider("ocid1.tenancy.oc1..t", "ocid1.user.oc1..u", "ap-seoul-1", "aa:bb", pem, nil)
		return cloud.OCISigner(p)
	}
	return func(r *http.Request) error { r.Header.Set("Authorization", "Bearer x"); return nil }
}

func reader(t *testing.T, src, raw string, h http.Handler) (*Alarms, *alarmtest.Transport) {
	t.Helper()
	a, err := NewAlarms(src, raw, "lapilli/test")
	if err != nil {
		t.Fatal(err)
	}
	tr := &alarmtest.Transport{H: h}
	a.Transport, a.Reads.Sign = tr, signer(a.Kind, "eu-west-1")
	return a, tr
}

func errs(t *testing.T, c *alarmtest.Common) {
	t.Helper()
	for _, e := range c.Errs {
		t.Error(e)
	}
}

func docMap(t *testing.T, d rules.Doc) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(d.Data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// CloudWatch: metric and composite alarms, one document each under its
// ARN, its state left out; the namespace is the source; an alarm and a
// rule of one name are told apart by it.
func TestAlarmsCloudWatch(t *testing.T) {
	ctx := context.Background()
	cw := alarmtest.NewCloudWatch("eu-west-1")
	cw.Add(map[string]any{"AlarmName": "cpu-high", "Namespace": "AWS/EC2", "MetricName": "CPUUtilization", "Statistic": "Average",
		"Period": 300, "EvaluationPeriods": 3, "Threshold": 90, "ComparisonOperator": "GreaterThanThreshold",
		"AlarmDescription": "CPU. Runbook: https://runbooks.example.com/cpu"}, "ALARM", map[string]string{"team": "shop"})
	cw.Add(map[string]any{"AlarmName": "checkout-5xx", "EvaluationInterval": 60,
		"EvaluationCriteria": map[string]any{"PromQLCriteria": map[string]any{"Query": "errors > 5", "PendingPeriod": 300}}}, "OK", nil)
	cw.AddComposite("shop-down", "ALARM(cpu-high) AND ALARM(checkout-5xx)")
	a, _ := reader(t, "cloudwatch-prod", "https://monitoring.eu-west-1.amazonaws.com", cw)
	rs, docs, err := a.Read(ctx)
	errs(t, &cw.Common)
	if err != nil || len(docs) != 3 || len(rs) != 3 {
		t.Fatalf("%d docs, %+v, %v", len(docs), rs, err)
	}
	for _, d := range docs {
		if d.Namespace != "cloudwatch-prod" || !strings.HasPrefix(d.Source, "arn:aws:cloudwatch:eu-west-1:111122223333:alarm:") {
			t.Errorf("a document: %s %s", d.Namespace, d.Source)
		}
		if s := string(d.Data); strings.Contains(s, "StateValue") || strings.Contains(s, "AlarmConfigurationUpdatedTimestamp") {
			t.Errorf("its state is in it:\n%s", s)
		}
	}
	byName := map[string]rules.Rule{}
	for _, r := range rs {
		byName[r.Name] = r
	}
	if r := byName["checkout-5xx"]; r.Unsupported != "" || r.For != 5*time.Minute || r.Namespace != "cloudwatch-prod" || r.Cloud != "cloudwatch" {
		t.Errorf("the PromQL alarm: %+v", r)
	}
	if r := byName["cpu-high"]; r.Unsupported == "" || r.Says.Runbook != "https://runbooks.example.com/cpu" {
		t.Errorf("the metric alarm: %+v", r)
	}
	// Told apart from a rule of its name by its source.
	both := []rules.Rule{byName["cpu-high"], {Name: "cpu-high", Group: "node", Expr: "cpu > 0.9"}}
	TellApart(both)
	if both[0].Name != "cpu-high [cloudwatch-prod]" || both[1].Name != "cpu-high [node]" {
		t.Errorf("told apart: %q %q", both[0].Name, both[1].Name)
	}
	// Read again: its state, tags and version.
	u, err := a.Unit(ctx, cw.ARN("cpu-high"), a.Reads)
	if err != nil || u.Gone || !u.Firing || u.Tags["team"] != "shop" || u.Version == "" || u.Hash == "" {
		t.Fatalf("the unit: %+v %v", u, err)
	}
	if u, err := a.Unit(ctx, cw.ARN("gone"), a.Reads); err != nil || !u.Gone {
		t.Errorf("a gone alarm: %+v %v", u, err)
	}
	if _, err := a.Unit(ctx, "arn:aws:cloudwatch:us-east-1:111122223333:alarm:cpu-high", a.Reads); err == nil {
		t.Error("an ARN of another region is read")
	}
	// The hash of the unit is the document's, as read in the list.
	for _, d := range docs {
		if d.Source == u.ID {
			if h, _ := rules.TreeHash(d.Data); h != u.Hash {
				t.Errorf("the unit hashes apart from its document")
			}
		}
	}
}

// A budget: 5,000 alarms read a page of 100 at a time, parsed, and kept
// as documents, in a few seconds.
func TestAlarmsBudget(t *testing.T) {
	cw := alarmtest.NewCloudWatch("eu-west-1")
	for i := range 5000 {
		cw.Add(map[string]any{"AlarmName": fmt.Sprintf("alarm-%04d", i), "Namespace": "AWS/EC2", "MetricName": "CPUUtilization",
			"Statistic": "Average", "Period": 300, "EvaluationPeriods": 3, "Threshold": 90, "ComparisonOperator": "GreaterThanThreshold",
			"Dimensions": []any{map[string]any{"Name": "InstanceId", "Value": fmt.Sprintf("i-%04d", i)}}}, "OK", nil)
	}
	a, tr := reader(t, "cloudwatch-prod", "https://monitoring.eu-west-1.amazonaws.com", cw)
	start := time.Now()
	rs, docs, err := a.Read(context.Background())
	if err != nil || len(rs) != 5000 || len(docs) != 5000 {
		t.Fatalf("%d %d %v", len(rs), len(docs), err)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("5,000 alarms took %s", took)
	}
	if n := tr.Hosts["monitoring.eu-west-1.amazonaws.com"]; n != 50 {
		t.Errorf("%d requests, want 50 pages", n)
	}
}

// Azure: the three types, a page at a time by nextLink, each rule under its
// id; what Azure sets as it runs left out; a PromQL metric alert replayed.
func TestAlarmsAzure(t *testing.T) {
	ctx := context.Background()
	z := alarmtest.NewAzure("sub-1")
	promql := map[string]any{"severity": 3, "enabled": true, "evaluationFrequency": "PT1M", "scopes": []any{"/subscriptions/sub-1/x"},
		"criteria": map[string]any{"odata.type": rules.AzurePromQL, "failingPeriods": map[string]any{"for": "PT5M"},
			"allOf": []any{map[string]any{"name": "M1", "criterionType": "StaticThresholdCriterion", "query": "errors > 5"}}}}
	z.Add("shop-rg", "metricAlerts", "checkout-errors", promql, map[string]any{"team": "shop"})
	z.Add("shop-rg", "scheduledQueryRules", "log-errors", map[string]any{"severity": 2, "enabled": true, "description": "Errors in logs"}, nil)
	z.Add("shop-rg", "activityLogAlerts", "deletes", map[string]any{"enabled": true, "description": "Deletes"}, nil)
	z.Add("other-rg", "metricAlerts", "elsewhere", promql, nil)
	a, _ := reader(t, "azure-prod", "https://management.azure.com/subscriptions/sub-1/resourceGroups/shop-rg", z)
	rs, docs, err := a.Read(ctx)
	errs(t, &z.Common)
	if err != nil || len(docs) != 3 {
		t.Fatalf("%d docs, %v", len(docs), err)
	}
	for _, d := range docs {
		if s := string(d.Data); strings.Contains(s, "lastUpdatedTime") || strings.Contains(s, "systemData") || strings.Contains(s, "etag") {
			t.Errorf("what Azure sets as it runs is in it:\n%s", s)
		}
	}
	if r := rs[0]; r.Name != "checkout-errors" || r.Unsupported != "" || r.Group != "shop-rg" || r.For != 5*time.Minute {
		t.Errorf("the PromQL metric alert: %+v", r)
	}
	u, err := a.Unit(ctx, z.ID("shop-rg", "scheduledQueryRules", "log-errors"), a.Reads)
	if err != nil || u.Etag == "" || u.Gone {
		t.Errorf("a log search alert's unit: %+v %v", u, err)
	}
	if _, err := a.Unit(ctx, z.ID("other-rg", "metricAlerts", "elsewhere"), a.Reads); err == nil {
		t.Error("an alert rule in another resource group is read")
	}
	// A next page elsewhere isn't followed.
	bad := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"value": []any{}, "nextLink": "https://evil.example.com/next"})
	})
	a2, _ := reader(t, "azure-prod", "https://management.azure.com/subscriptions/sub-1", bad)
	if _, _, err := a2.Read(ctx); err == nil || !strings.Contains(err.Error(), "isn't followed") {
		t.Errorf("a nextLink elsewhere: %v", err)
	}
}

// Google: a page at a time, each policy under its name; mutationRecord
// left out.
func TestAlarmsGoogle(t *testing.T) {
	ctx := context.Background()
	g := alarmtest.NewGoogle("shop-prod")
	for _, n := range []string{"Checkout errors", "CPU", "Disk"} {
		g.Add(map[string]any{"displayName": n, "enabled": true, "combiner": "OR",
			"documentation": map[string]any{"subject": n, "content": "About " + n},
			"conditions":    []any{map[string]any{"displayName": n, "conditionPrometheusQueryLanguage": map[string]any{"query": "errors > 5", "duration": "300s"}}}})
	}
	a, tr := reader(t, "gcp-prod", "https://monitoring.googleapis.com/v3/projects/shop-prod/alertPolicies", g)
	rs, docs, err := a.Read(ctx)
	errs(t, &g.Common)
	if err != nil || len(docs) != 3 || len(rs) != 3 || tr.Hosts["monitoring.googleapis.com"] != 2 {
		t.Fatalf("%d docs, %d rules, %v, %v", len(docs), len(rs), tr.Hosts, err)
	}
	if s := string(docs[0].Data); strings.Contains(s, "mutationRecord") || docs[0].Source != "projects/shop-prod/alertPolicies/101" {
		t.Errorf("a policy: %s\n%s", docs[0].Source, s)
	}
	u, err := a.Unit(ctx, docs[0].Source, a.Reads)
	if err != nil || u.Version == "" {
		t.Errorf("the unit: %+v %v", u, err)
	}
	if err := a.OwnsID("projects/other/alertPolicies/101"); err == nil {
		t.Error("a policy of another project")
	}
}

// OCI: the list, a page at a time by opc-next-page, has no body; each
// alarm is got once, then again only when it lists differently, and every
// alarm once an hour; what it says is then known.
func TestAlarmsOCI(t *testing.T) {
	ctx := context.Background()
	k, _ := alarmtest.OCIKey()
	o := alarmtest.NewOCI("ocid1.compartment.oc1..shop", &k.PublicKey, "ocid1.tenancy.oc1..t/ocid1.user.oc1..u/aa:bb")
	ids := []string{
		o.Add(map[string]any{"displayName": "checkout-errors", "namespace": "oci_apigateway", "query": "HttpResponses[1m].count() > 5",
			"severity": "CRITICAL", "isEnabled": true, "destinations": []any{"ocid1.onstopic.oc1..t"}, "body": "Checkout. Runbook: https://runbooks.example.com/checkout"}),
		o.Add(map[string]any{"displayName": "cpu", "namespace": "oci_computeagent", "query": "CpuUtilization[1m].mean() > 90",
			"severity": "WARNING", "isEnabled": true, "destinations": []any{}}),
		o.Add(map[string]any{"displayName": "disk", "namespace": "oci_computeagent", "query": "DiskUtilization[1m].mean() > 90",
			"severity": "INFO", "isEnabled": false, "destinations": []any{}, "freeformTags": map[string]any{"managed-by": "terraform"}}),
	}
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	a, _ := reader(t, "oci-prod", "https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms?compartmentId=ocid1.compartment.oc1..shop", o)
	a.Now = func() time.Time { return at }
	rs, docs, err := a.Read(ctx)
	errs(t, &o.Common)
	if err != nil || len(docs) != 3 || len(rs) != 3 {
		t.Fatalf("%d docs, %v", len(docs), err)
	}
	if r := rs[0]; r.Says.TextUnknown || r.Says.Runbook != "https://runbooks.example.com/checkout" || r.Labels["severity"] != "critical" {
		t.Errorf("what an alarm says is known: %+v", r)
	}
	if rs[1].Says.TextUnknown {
		t.Error("an alarm with no body says nothing, known")
	}
	if rs[2].Unsupported != rules.Disabled {
		t.Errorf("an alarm off: %+v", rs[2])
	}
	if s := string(docs[0].Data); strings.Contains(s, "timeUpdated") || strings.Contains(s, "lifecycleState") {
		t.Errorf("its state is in it:\n%s", s)
	}
	// Read again: the list alike, nothing got again.
	at = at.Add(10 * time.Minute)
	a.Read(ctx)
	if o.Gets[ids[0]] != 1 || o.Gets[ids[1]] != 1 {
		t.Errorf("got again though listed alike: %v", o.Gets)
	}
	// One lists differently: it alone is got again.
	o.Lock()
	o.Alarms[ids[1]]["severity"] = "CRITICAL"
	o.Unlock()
	at = at.Add(10 * time.Minute)
	if rs, _, _ := a.Read(ctx); o.Gets[ids[1]] != 2 || o.Gets[ids[0]] != 1 || rs[1].Labels["severity"] != "critical" {
		t.Errorf("a changed alarm: %v", o.Gets)
	}
	// Its body changed, which the list doesn't show: got again within the
	// hour.
	o.Lock()
	o.Alarms[ids[0]]["body"] = "Checkout, again."
	o.Unlock()
	at = at.Add(time.Hour)
	if rs, _, _ := a.Read(ctx); o.Gets[ids[0]] != 2 || rs[0].Says.Description != "Checkout, again." {
		t.Errorf("once an hour: %v", o.Gets)
	}
	u, err := a.Unit(ctx, ids[2], a.Reads)
	if err != nil || u.Etag == "" || u.Tags["managed-by"] != "terraform" {
		t.Errorf("the unit: %+v %v", u, err)
	}
}

// A read that fails anywhere fails whole; a redirect isn't followed; a
// read is kept for --alarms-every.
func TestAlarmsFailWhole(t *testing.T) {
	ctx := context.Background()
	cw := alarmtest.NewCloudWatch("eu-west-1")
	cw.Add(map[string]any{"AlarmName": "a", "Namespace": "AWS/EC2", "MetricName": "x"}, "OK", nil)
	a, tr := reader(t, "cloudwatch-prod", "https://monitoring.eu-west-1.amazonaws.com", cw)
	at := time.Now()
	a.Now = func() time.Time { return at }
	if _, err := a.Docs(ctx, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	cw.FailReads = true
	if _, err := a.Docs(ctx, 10*time.Minute); err != nil || tr.Hosts["monitoring.eu-west-1.amazonaws.com"] != 1 {
		t.Errorf("read again within --alarms-every: %v %v", tr.Hosts, err)
	}
	at = at.Add(11 * time.Minute)
	if _, err := a.Docs(ctx, 10*time.Minute); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("a 503: %v", err)
	}
	redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com/", http.StatusTemporaryRedirect)
	})
	a2, _ := reader(t, "cloudwatch-prod", "https://monitoring.eu-west-1.amazonaws.com", redirect)
	if _, _, err := a2.Read(ctx); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("a redirect: %v", err)
	}
}

// Every request goes to the host --alarms names, and nowhere else.
func TestAlarmsOnlyItsHost(t *testing.T) {
	cw := alarmtest.NewCloudWatch("eu-west-1")
	a, tr := reader(t, "cw", "https://monitoring.eu-west-1.amazonaws.com", cw)
	for _, at := range []string{"https://evil.example.com/", "http://monitoring.eu-west-1.amazonaws.com/", "https://user@monitoring.eu-west-1.amazonaws.com/",
		"https://monitoring.eu-west-1.amazonaws.com.example.net/"} {
		if _, _, _, err := a.Call(context.Background(), http.MethodGet, at, nil, nil, a.Reads, UnitLimit); err == nil || !strings.Contains(err.Error(), "isn't asked") {
			t.Errorf("%s: %v", at, err)
		}
	}
	if len(tr.Hosts) != 0 {
		t.Errorf("asked: %v", tr.Hosts)
	}
}
