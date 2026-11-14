package serve

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/alarmtest"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
)

// Azure Monitor's alert rules, Google Cloud's alerting policies and OCI's
// alarms, each against its stand-in (docs/design/alarms.md).

func cloudErrs(t *testing.T, c *alarmtest.Common) {
	t.Helper()
	t.Cleanup(func() {
		for _, e := range c.Errs {
			t.Error(e)
		}
	})
}

const azWriter = "as 11111111-2222-3333-4444-555555555555"

func azureAlarms() (*alarmtest.Azure, map[string]string) {
	z := alarmtest.NewAzure("sub-1")
	z.Writer, z.Reader = azWriter, "pod"
	ids := map[string]string{
		"checkout-errors": z.Add("shop-rg", "metricAlerts", "checkout-errors", map[string]any{"severity": 2, "enabled": true,
			"evaluationFrequency": "PT1M", "scopes": []any{"/subscriptions/sub-1/resourceGroups/shop-rg/providers/microsoft.monitor/accounts/shop"},
			"description": "Checkout errors.",
			"criteria": map[string]any{"odata.type": rules.AzurePromQL, "failingPeriods": map[string]any{"for": "PT5M"},
				"allOf": []any{map[string]any{"name": "M1", "criterionType": "StaticThresholdCriterion", "query": "errors > 5"}}},
			"actions": []any{map[string]any{"actionGroupId": "/subscriptions/sub-1/resourceGroups/shop-rg/providers/microsoft.insights/actionGroups/oncall"}}},
			map[string]any{"team": "shop"}),
		"log-errors": z.Add("shop-rg", "scheduledQueryRules", "log-errors", map[string]any{"severity": 2, "enabled": true,
			"description": "Errors in the logs."}, nil),
		"deletes": z.Add("shop-rg", "activityLogAlerts", "deletes", map[string]any{"enabled": true, "description": "Deletes.",
			"scopes": []any{"/subscriptions/sub-1"}, "condition": map[string]any{"allOf": []any{map[string]any{"field": "category", "equals": "Administrative"}}},
			"actions": map[string]any{"actionGroups": []any{map[string]any{"actionGroupId": "/x"}}}}, nil),
		"newer": z.Add("shop-rg", "activityLogAlerts", "newer", map[string]any{"enabled": true, "description": "Newer.",
			"scopes": []any{"/subscriptions/sub-1"}, "condition": map[string]any{"allOf": []any{map[string]any{"field": "category", "equals": "Policy"}}},
			"actions": map[string]any{"actionGroups": []any{}}, "shinyNewSetting": true}, nil),
	}
	return z, ids
}

// Azure: a PromQL metric alert's for, a PATCH of its criteria alone; what
// a log search alert says, a PATCH of its description with If-Match, which
// Azure refuses when written meanwhile; an activity log alert's
// description, a PUT of the whole rule, since its PATCH takes enabled
// alone; turning one off, a PATCH of enabled; undone. --changes-in names
// RG/NAME in any letter case.
func TestAlarmAzure(t *testing.T) {
	ctx := context.Background()
	z, ids := azureAlarms()
	cloudErrs(t, &z.Common)
	s := alarmServer(t, "az", "https://management.azure.com/subscriptions/sub-1", z, []string{"az:SHOP-RG/*"}, nil)
	h := s.Handler()
	id := proposeRule(t, h, "checkout-errors", "for", url.Values{"for": {"15m"}})
	if p := state(t, s, id); p.Path != ledger.PathAlarm {
		t.Fatalf("the for: %s %q", p.Path, proposalEvidence(p).NoPath)
	}
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	r := z.Rule(ids["checkout-errors"])
	crit := r["properties"].(map[string]any)["criteria"].(map[string]any)
	if p := state(t, s, id); p.State != ledger.StateApplied || !slices.Equal(z.Took(), []string{"PATCH checkout-errors"}) ||
		crit["failingPeriods"].(map[string]any)["for"] != "PT15M" {
		t.Fatalf("the for: %s %q %v %v", p.State, p.Reason, z.Took(), crit)
	}
	// What a log search alert says, with its etag; written meanwhile, Azure
	// refuses it.
	id = proposeRule(t, h, "log-errors", "says", url.Values{"annotation": {"description"}, "text": {"Errors in the shop's logs."}})
	approve(t, h, id, ana)
	z.Meanwhile = func() {
		z.Lock()
		z.Rules[strings.ToLower(ids["log-errors"])]["etag"] = `"someone-else"`
		z.Unlock()
	}
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateFailed || !strings.Contains(p.Reason, "412") {
		t.Errorf("written meanwhile: %s %q", p.State, p.Reason)
	}
	id = proposeRule(t, h, "log-errors", "says", url.Values{"annotation": {"description"}, "text": {"Errors in the shop's logs."}})
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied || z.Rule(ids["log-errors"])["properties"].(map[string]any)["description"] != "Errors in the shop's logs." {
		t.Errorf("a log search alert's description: %s %q", p.State, p.Reason)
	}
	// An activity log alert's description: the whole rule, put.
	id = proposeRule(t, h, "deletes", "says", url.Values{"annotation": {"runbook_url"}, "text": {"https://runbooks.example.com/deletes"}})
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	r = z.Rule(ids["deletes"])
	if p := state(t, s, id); p.State != ledger.StateApplied || z.Took()[len(z.Took())-1] != "PUT deletes" ||
		r["properties"].(map[string]any)["description"] != "Deletes.\nhttps://runbooks.example.com/deletes" || r["properties"].(map[string]any)["condition"] == nil {
		t.Errorf("an activity log alert's runbook: %s %q %v %v", p.State, p.Reason, z.Took(), r)
	}
	// One with a field the specification doesn't have isn't put whole.
	id = proposeRule(t, h, "newer", "says", url.Values{"annotation": {"description"}, "text": {"Newer, said."}})
	if p := state(t, s, id); p.Path != ledger.PathNone || proposalEvidence(p).NoPath != noPathAzureFields {
		t.Errorf("an unknown field: %s %q", p.Path, proposalEvidence(p).NoPath)
	}
	// Turned off, and undone.
	id = proposeRule(t, h, "checkout-errors", "disable", nil)
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied || z.Rule(ids["checkout-errors"])["properties"].(map[string]any)["enabled"] != false {
		t.Fatalf("off: %s %q", p.State, p.Reason)
	}
	key := keyFor(t, h, id, ana, "undo")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}, "read": {"1"}})
	s.jobs(ctx, false)
	if u := state(t, s, locID(hd)); u.State != ledger.StateApplied || z.Rule(ids["checkout-errors"])["properties"].(map[string]any)["enabled"] != true {
		t.Errorf("on again: %s %q", u.State, u.Reason)
	}
	if n := len(z.Took()); n != 5 { // the write Azure refused isn't taken
		t.Errorf("writes: %v", z.Took())
	}
}

const gWriter = "as lapilli@shop.iam.gserviceaccount.com"

func googlePolicies() (*alarmtest.Google, map[string]string) {
	g := alarmtest.NewGoogle("shop")
	g.Writer, g.Reader = gWriter, "pod"
	cond := func(name, query, d string) map[string]any {
		return map[string]any{"displayName": name, "conditionPrometheusQueryLanguage": map[string]any{"query": query, "duration": d, "alertRule": name}}
	}
	ids := map[string]string{
		"two": g.Add(map[string]any{"displayName": "Shop", "enabled": true, "combiner": "OR",
			"documentation": map[string]any{"subject": "Shop", "content": "The shop."},
			"conditions":    []any{cond("ShopErrors", "errors > 5", "300s"), cond("ShopLatency", "latency > 1", "300s")}}),
		"one": g.Add(map[string]any{"displayName": "Down", "enabled": true, "combiner": "OR",
			"documentation": map[string]any{"subject": "Down", "content": "Down."},
			"conditions":    []any{cond("Down", "up == 0", "60s")}}),
		"cnrm": g.Add(map[string]any{"displayName": "Kept", "enabled": true, "userLabels": map[string]any{"managed-by-cnrm": "true"},
			"conditions": []any{cond("Kept", "kept > 1", "60s")}}),
	}
	return g, ids
}

// Google Cloud: a condition's for in a policy of two, a patch of the
// conditions (each with its name), the other as it was; what a policy of
// two says, for a person; a policy of one: what it says (the mask
// documentation), off (enabled), deleted four weeks later, and undone,
// made again with a new name, read back by what it holds; Config
// Connector's, for a person.
func TestAlarmGoogle(t *testing.T) {
	ctx := context.Background()
	g, ids := googlePolicies()
	cloudErrs(t, &g.Common)
	s := alarmServer(t, "gcp", "https://monitoring.googleapis.com/v3/projects/shop/alertPolicies", g, []string{"gcp:*"}, nil)
	h := s.Handler()
	id := proposeRule(t, h, "ShopErrors", "for", url.Values{"for": {"15m"}})
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	p := g.Policy(ids["two"])
	conds := p["conditions"].([]any)
	if st := state(t, s, id); st.State != ledger.StateApplied || !slices.Equal(g.Took(), []string{"PATCH " + ids["two"] + " conditions"}) ||
		conds[0].(map[string]any)["conditionPrometheusQueryLanguage"].(map[string]any)["duration"] != "900s" ||
		conds[1].(map[string]any)["conditionPrometheusQueryLanguage"].(map[string]any)["duration"] != "300s" ||
		conds[0].(map[string]any)["name"] != ids["two"]+"/conditions/1" {
		t.Fatalf("a condition's for: %s %q %v %v", st.State, st.Reason, g.Took(), conds)
	}
	for _, c := range []struct{ rule, kind, why string }{
		{"ShopLatency", "disable", noPathConditions},
		{"Kept", "for", noPathCNRM},
	} {
		id := proposeRule(t, h, c.rule, c.kind, url.Values{"for": {"15m"}})
		if p := state(t, s, id); p.Path != ledger.PathNone || proposalEvidence(p).NoPath != c.why {
			t.Errorf("%s %s: %s %q", c.rule, c.kind, p.Path, proposalEvidence(p).NoPath)
		}
	}
	id = proposeRule(t, h, "Down", "says", url.Values{"annotation": {"runbook_url"}, "text": {"https://runbooks.example.com/down"}})
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if st := state(t, s, id); st.State != ledger.StateApplied || !strings.HasSuffix(g.Took()[len(g.Took())-1], " documentation") {
		t.Fatalf("a runbook: %s %q %v", st.State, st.Reason, g.Took())
	}
	// Off, then deleted four weeks later; undone, made again.
	off := proposeRule(t, h, "Down", "disable", nil)
	approve(t, h, off, ana)
	s.jobs(ctx, false)
	if st := state(t, s, off); st.State != ledger.StateApplied || g.Policy(ids["one"])["enabled"] != false {
		t.Fatalf("off: %s %q", st.State, st.Reason)
	}
	raw := ""
	s.look(ctx, &raw)
	s.now = func() time.Time { return time.Now().Add(5 * weekLen) }
	if err := s.proposeFound(ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: "Down", States: []string{ledger.StateProposed}})
	if len(ps) != 1 || ps[0].Kind != ledger.KindDelete {
		t.Fatalf("deleting it: %+v", ps)
	}
	approve(t, h, ps[0].ID, cy)
	s.jobs(ctx, false)
	if st := state(t, s, ps[0].ID); st.State != ledger.StateApplied || g.Policy(ids["one"]) != nil {
		t.Fatalf("deleted: %s %q", st.State, st.Reason)
	}
	key := keyFor(t, h, ps[0].ID, ana, "undo")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(ps[0].ID)+"/undo", ana, "same-origin", url.Values{"key": {key}, "read": {"1"}})
	s.jobs(ctx, false)
	u := state(t, s, locID(hd))
	var again map[string]any
	for name, p := range g.Policies {
		if p["displayName"] == "Down" && name != ids["one"] {
			again = p
		}
	}
	if u.State != ledger.StateApplied || again == nil || again["enabled"] != false || g.Took()[len(g.Took())-1] != "POST create" {
		t.Errorf("made again: %s %q %v", u.State, u.Reason, again)
	}
}

func ociAlarms(t *testing.T) (*alarmtest.OCI, map[string]string) {
	k, _ := alarmtest.OCIKey()
	o := alarmtest.NewOCI("ocid1.compartment.oc1..shop", &k.PublicKey, "ocid1.tenancy.oc1..t/ocid1.user.oc1..u/aa:bb")
	o.Writer, o.Reader = "as /etc/lapilli/oci/config", "pod"
	ids := map[string]string{
		"checkout": o.Add(map[string]any{"displayName": "checkout-errors", "namespace": "oci_apigateway", "query": "HttpResponses[1m].count() > 5",
			"severity": "CRITICAL", "isEnabled": true, "destinations": []any{"ocid1.onstopic.oc1..t"}, "body": "Checkout errors.",
			"overrides": []any{map[string]any{"ruleName": "night", "body": "Checkout errors, at night."}}}),
		"cpu": o.Add(map[string]any{"displayName": "cpu", "namespace": "oci_computeagent", "query": "CpuUtilization[1m].mean() > 90",
			"severity": "WARNING", "isEnabled": true, "destinations": []any{}, "body": ""}),
		"kept": o.Add(map[string]any{"displayName": "kept", "namespace": "oci_computeagent", "query": "x[1m].mean() > 1",
			"severity": "INFO", "isEnabled": true, "destinations": []any{}, "definedTags": map[string]any{"ops": map[string]any{"managed-by": "terraform"}}}),
	}
	return o, ids
}

// OCI: what an alarm says, its body alone in UpdateAlarm with the etag it
// was read with, its overrides' bodies as they were; written meanwhile,
// OCI answers 412 and it fails with that; a defined tag --changes-managed
// names, for a person; turned off, its history tied to it.
func TestAlarmOCI(t *testing.T) {
	ctx := context.Background()
	o, ids := ociAlarms(t)
	cloudErrs(t, &o.Common)
	s := alarmServer(t, "oci", "https://telemetry.ap-seoul-1.oraclecloud.com/20180401/alarms?compartmentId=ocid1.compartment.oc1..shop", o,
		[]string{"oci:*"}, func(c *Config) {
			c.ChangesManaged = []string{"ops.managed-by=terraform"}
			c.SourceQuiet = 14 * weekLen // its copies up to now
		})
	h := s.Handler()
	id := proposeRule(t, h, "checkout-errors", "says", url.Values{"annotation": {"runbook_url"}, "text": {"https://runbooks.example.com/checkout"}})
	if p := state(t, s, id); p.Path != ledger.PathAlarm {
		t.Fatalf("a runbook: %s %q", p.Path, proposalEvidence(p).NoPath)
	}
	approve(t, h, id, ana)
	o.Meanwhile = func() {
		o.Lock()
		o.Alarms[ids["checkout"]]["severity"] = "ERROR"
		o.Alarms[ids["checkout"]]["__etag"] = "someone-else"
		o.Unlock()
	}
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateFailed || !strings.Contains(p.Reason, "412") || !strings.Contains(p.Reason, "neither") {
		t.Errorf("written meanwhile: %s %q", p.State, p.Reason)
	}
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	id = proposeRule(t, h, "checkout-errors", "says", url.Values{"annotation": {"runbook_url"}, "text": {"https://runbooks.example.com/checkout"}})
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	a := o.Alarm(ids["checkout"])
	if p := state(t, s, id); p.State != ledger.StateApplied || a["body"] != "Checkout errors.\nhttps://runbooks.example.com/checkout" ||
		a["overrides"].([]any)[0].(map[string]any)["body"] != "Checkout errors, at night." ||
		!strings.HasSuffix(o.Took()[len(o.Took())-1], " body") {
		t.Errorf("a runbook: %s %q %v %v", p.State, p.Reason, a, o.Took())
	}
	id = proposeRule(t, h, "kept", "says", url.Values{"annotation": {"description"}, "text": {"Kept."}})
	if p := state(t, s, id); p.Path != ledger.PathNone || proposalEvidence(p).NoPath != noPathManaged {
		t.Errorf("a defined tag: %s %q", p.Path, proposalEvidence(p).NoPath)
	}
	// Turned off, its history tied over twelve weeks.
	copyFirings(t, s, "oci", "2026-11-01T00:00:00Z", "2026-11-03T00:00:00Z",
		`{"rule":"cpu","labels":{"alarm_id":"`+ids["cpu"]+`"},"start":"2026-11-02T06:00:00Z","end":"2026-11-02T06:30:00Z"}`+"\n")
	s.now = func() time.Time { return now.Add(13 * weekLen) }
	id = proposeRule(t, h, "cpu", "disable", nil)
	approve(t, h, id, ana)
	s.jobs(ctx, false)
	if p := state(t, s, id); p.State != ledger.StateApplied || o.Alarm(ids["cpu"])["isEnabled"] != false ||
		!strings.HasSuffix(o.Took()[len(o.Took())-1], " isEnabled") {
		t.Errorf("off: %s %q %v", p.State, p.Reason, o.Took())
	}
}

// The guarantees of the one API path, on alarms: one change to a unit (a
// Google policy) at a time, a second waiting while the first isn't read
// back; read back only where it was sent; an undo put back only over the
// fields as the change left them, every key of them (an Azure criterion
// renamed since, which no hash covers, refuses it).
func TestAlarmOneUnitAtATime(t *testing.T) {
	ctx := context.Background()
	g, _ := googlePolicies()
	cloudErrs(t, &g.Common)
	s := alarmServer(t, "gcp", "https://monitoring.googleapis.com/v3/projects/shop/alertPolicies", g, []string{"gcp:*"}, nil)
	h := s.Handler()
	first := proposeRule(t, h, "ShopErrors", "for", url.Values{"for": {"15m"}})
	second := proposeRule(t, h, "ShopLatency", "for", url.Values{"for": {"15m"}})
	approve(t, h, first, ana)
	g.FailAfterWrite = true
	s.jobs(ctx, false)
	if p := state(t, s, first); p.State != ledger.StateApplying {
		t.Fatalf("the first: %s %q", p.State, p.Reason)
	}
	// Pointed at another project by a restart: the first is read back
	// where it was sent, or not at all.
	was := s.alarms[0].URL
	s.alarms[0].URL = "https://monitoring.googleapis.com/v3/projects/other/alertPolicies"
	g.Lock()
	g.FailAfterWrite = false
	g.Unlock()
	s.carryOut(ctx)
	if p := state(t, s, first); p.State != ledger.StateApplying || p.Reason != notThereAlarm {
		t.Fatalf("elsewhere: %s %q", p.State, p.Reason)
	}
	s.alarms[0].URL = was
	// The second, to the same policy, waits while the first isn't read
	// back.
	g.Lock()
	g.FailReads = true
	g.Unlock()
	approve(t, h, second, ana)
	s.carryOut(ctx)
	if p := state(t, s, second); p.State != ledger.StateApproved || len(g.Took()) != 1 {
		t.Fatalf("the second, while the first isn't read back: %s %q %v", p.State, p.Reason, g.Took())
	}
	g.Lock()
	g.FailReads = false
	g.Unlock()
	s.jobs(ctx, false)
	s.jobs(ctx, false)
	if p, q := state(t, s, first), state(t, s, second); p.State != ledger.StateApplied || q.State != ledger.StateApplied || len(g.Took()) != 2 {
		t.Errorf("both, one after the other: %s %s %q %v", p.State, q.State, q.Reason, g.Took())
	}

	z, zids := azureAlarms()
	cloudErrs(t, &z.Common)
	az := alarmServer(t, "az", "https://management.azure.com/subscriptions/sub-1", z, []string{"az:shop-rg/*"}, nil)
	ah := az.Handler()
	id := proposeRule(t, ah, "checkout-errors", "for", url.Values{"for": {"15m"}})
	approve(t, ah, id, ana)
	az.jobs(ctx, false)
	if p := state(t, az, id); p.State != ledger.StateApplied {
		t.Fatalf("the for: %s %q", p.State, p.Reason)
	}
	z.Lock()
	crit := z.Rules[strings.ToLower(zids["checkout-errors"])]["properties"].(map[string]any)["criteria"].(map[string]any)
	crit["allOf"].([]any)[0].(map[string]any)["name"] = "Renamed"
	z.Unlock()
	key := keyFor(t, ah, id, ana, "undo")
	_, _, hd := ui(t, ah, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}, "read": {"1"}})
	az.jobs(ctx, false)
	if u := state(t, az, locID(hd)); u.State != ledger.StateRefused || !strings.Contains(u.Reason, "changed in the cloud since the change") {
		t.Errorf("undo over a criterion renamed since: %s %q", u.State, u.Reason)
	}
}
