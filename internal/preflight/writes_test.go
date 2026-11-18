package preflight

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"lapilli.dev/internal/alarmtest"
	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/egress"
	"lapilli.dev/internal/serve"
)

// The examples in these tests are made up: an account 123456789012, a
// project example-project, OCIDs of OCI's documented form ending in
// example.

// grafanaAs are Grafana OSS 13.3.1's answers to GET
// /api/access-control/user/permissions for a service account of each
// basic role, the actions alert rules' provisioning reads, as measured
// (docs/design/preflight.md, "As built (step 2)"). 12.4.15 answers alike.
var grafanaAs = map[string]string{
	"Admin": `{"alert.provisioning:read":["",""],"alert.provisioning:write":[""],"alert.provisioning.secrets:read":[""],
"alert.rules.provisioning:read":["",""],"alert.rules.provisioning:write":[""],"alert.provisioning.provenance:write":[""],
"alert.rules:read":["folders:*"],"alert.rules:write":["folders:*"],"alert.rules:create":["folders:*"],"alert.rules:delete":["folders:*"],
"folders:read":["folders:*","folders:uid:general","folders:uid:sharedwithme"],"datasources:query":["datasources:*","datasources:uid:grafana"]}`,
	"Editor": `{"alert.provisioning.provenance:write":[""],"alert.rules:read":["folders:*","folders:uid:shop"],
"alert.rules:write":["folders:*","folders:uid:shop"],"alert.rules:create":["folders:*","folders:uid:shop"],"alert.rules:delete":["folders:*","folders:uid:shop"],
"folders:read":["folders:uid:general","folders:uid:shop","folders:uid:shop","folders:uid:payments","folders:uid:sharedwithme"],
"datasources:query":["datasources:*","datasources:uid:grafana"]}`,
	"Viewer": `{"alert.rules:read":["folders:*","folders:uid:shop"],"folders:read":["folders:uid:general","folders:uid:shop","folders:uid:sharedwithme"],
"datasources:query":["datasources:*","datasources:uid:grafana"]}`,
}

// What Grafana lists of a token is judged against what serve's changes
// send: an Admin's provisioning write is every rule; an Editor's rule
// writes are folder by folder, unknown, unless folders:read is every
// folder; a Viewer's, or one lacking any action a change needs, a
// problem that names it.
func TestGrafanaMay(t *testing.T) {
	perms := func(raw string) map[string][]string {
		var m map[string][]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	editor := perms(grafanaAs["Editor"])
	everyFolder := perms(grafanaAs["Editor"])
	everyFolder["folders:read"] = []string{"folders:*"}
	noDelete := perms(grafanaAs["Editor"])
	delete(noDelete, "alert.rules:delete")
	noProvenance := perms(grafanaAs["Editor"])
	delete(noProvenance, "alert.provisioning.provenance:write")
	writeNoRead := map[string][]string{"alert.provisioning:write": {""}}
	for _, x := range []struct {
		name  string
		perms map[string][]string
		state State
		says  string
	}{
		{"Admin", perms(grafanaAs["Admin"]), OK, "every alert rule"},
		{"Editor", editor, Unknown, "folder by folder: it may read 2 folders"},
		{"Editor, every folder", everyFolder, OK, "in every folder"},
		{"Viewer", perms(grafanaAs["Viewer"]), Problem, "alert.rules:write, alert.rules:delete, alert.rules:create, alert.provisioning.provenance:write"},
		{"no delete", noDelete, Problem, "lacks alert.rules:delete:"},
		{"no provenance", noProvenance, Problem, "lacks alert.provisioning.provenance:write"},
		{"writes, no read", writeNoRead, Problem, "a read of the rules"},
		{"nothing", map[string][]string{}, Problem, "a read of the rules"},
	} {
		it := grafanaMay(x.perms)
		if it.State != x.state || !strings.Contains(it.What+" "+it.Why, x.says) {
			t.Errorf("%s: %s %q %q", x.name, it.State, it.What, it.Why)
		}
	}
}

// hosts is a stand-in for http.DefaultTransport, which egress.Wrap(nil)
// sends through: each host's handler, by name; every request sent noted.
type hosts struct {
	mu   sync.Mutex
	by   map[string]http.Handler
	sent []string
}

func (h *hosts) RoundTrip(r *http.Request) (*http.Response, error) {
	h.mu.Lock()
	h.sent = append(h.sent, r.Method+" "+r.URL.Host+r.URL.Path)
	f := h.by[r.URL.Host]
	h.mu.Unlock()
	if f == nil {
		return nil, fmt.Errorf("no stand-in for %s", r.URL.Host)
	}
	return (&alarmtest.Transport{H: f}).RoundTrip(r)
}

func (h *hosts) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.sent)
}

// standIns has every request sent go to h, while the test runs.
func standIns(t *testing.T, by map[string]http.Handler) *hosts {
	t.Helper()
	h := &hosts{by: by}
	before := http.DefaultTransport
	http.DefaultTransport = h
	t.Cleanup(func() { http.DefaultTransport = before })
	return h
}

// guarded is a run of c whose allowlist is the process's guard, as Run
// sets it.
func guarded(t *testing.T, c Config) *run {
	t.Helper()
	p := newRun(c)
	p.allowAll()
	t.Cleanup(egress.Guard(p.al.check))
	return p
}

func fileOf(t *testing.T, name, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// signed is a signer that only marks a request: the credential each
// cloud's chain makes is step 1's, held by its own tests.
func signed(r *http.Request) error {
	r.Header.Set("Authorization", "Bearer example-credential")
	return nil
}

// judged is an item as the report has it, redacted.
func judged(p *run, it Item) Item {
	p.r.Items = nil
	p.add(it)
	p.redact()
	return p.r.Items[0]
}

// Grafana's list is asked once, with the token, through the allowlist,
// and nothing else is sent: a 404 is unknown, a 401 a problem.
func TestGrafanaWriteAsked(t *testing.T) {
	for _, x := range []struct {
		code  int
		body  string
		state State
	}{
		{200, grafanaAs["Admin"], OK},
		{200, grafanaAs["Viewer"], Problem},
		{404, `{"message":"Not found"}`, Unknown},
		{401, `{"message":"Invalid API key","messageId":"api-key.invalid"}`, Problem},
		{403, `{"message":"forbidden by a proxy"}`, Unknown},
		{200, `["not", "a", "map"]`, Unknown},
	} {
		var got []string
		standIns(t, map[string]http.Handler{"grafana.example": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = append(got, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
			w.WriteHeader(x.code)
			io.WriteString(w, x.body)
		})})
		c := config()
		c.Serve.Changes, c.Serve.Grafana = true, "http://grafana.example"
		c.Serve.ChangesGrafanaToken = fileOf(t, "token", "glsa_example_writes_0123456789\n")
		p := guarded(t, c)
		p.grafanaWrite(context.Background())
		it := itemOf(p, "grafana.write")
		if it == nil || it.State != x.state {
			t.Errorf("%d %s: %+v", x.code, x.body, it)
		}
		if len(got) != 1 || got[0] != "GET /api/access-control/user/permissions Bearer glsa_example_writes_0123456789" {
			t.Errorf("sent %v", got)
		}
		if a := p.al.asked(); a.By["grafana"] != 1 || a.Refused != 0 {
			t.Errorf("%+v", a)
		}
	}
}

// writesConfig is serve's flags with every cloud's write identity, each
// cloud's alarms and Azure's and Amazon's rulers named in --changes-in.
func writesConfig(t *testing.T) Config {
	c := config()
	s := &c.Serve
	s.Changes = true
	s.Alarms = []serve.AlarmsAt{
		{Source: "cw", URL: "https://monitoring.eu-west-1.amazonaws.com"},
		{Source: "az", URL: "https://management.azure.com/subscriptions/00000000-0000-0000-0000-00000000000a"},
		{Source: "gcp", URL: "https://monitoring.googleapis.com/v3/projects/example-project/alertPolicies"},
		{Source: "oci", URL: "https://telemetry.eu-frankfurt-1.oraclecloud.com/20180401/alarms?compartmentId=ocid1.compartment.oc1..exampleshop"},
	}
	s.ChangesIn = []string{"cw:checkout-*", "cw:QueueDepth", "az:shop-rg/checkout-*", "az:shop-rg/LatencyHigh", "gcp:Checkout", "oci:Checkout"}
	s.ChangesAWSRole = "arn:aws:iam::123456789012:role/example-writer"
	s.ChangesAzureClientID = "00000000-0000-0000-0000-00000000000c"
	s.ChangesGoogleServiceAccount = "example-writer@example-project.iam.gserviceaccount.com"
	s.ChangesOCIConfig = "/etc/lapilli/oci/config"
	c.Scrubber = byFlag
	return c
}

// byFlag is a scrubber that writes each value to redact as its flag.
func byFlag(more ...[2]string) func(string) string {
	return func(s string) string {
		for _, m := range more {
			s = strings.ReplaceAll(s, m[1], "["+m[0]+"]")
		}
		return s
	}
}

// The allowlist lets through what the checks of writes ask, and nothing
// else on the same hosts: IAM's SimulatePrincipalPolicy of the role
// alone (no other action, a read or not, nor another role's); Cloud
// Resource Manager's testIamPermissions on the project of --alarms alone,
// with the alerting policies' permissions as its body; Azure's
// permissions on the scopes --changes-in names, read; Grafana's list,
// read.
func TestAllowWrites(t *testing.T) {
	c := writesConfig(t)
	c.Serve.Grafana, c.Serve.ChangesGrafanaToken = "https://grafana.example/sub", "/etc/lapilli/grafana-writes"
	p := newRun(c)
	p.allowAll()
	sim := func(kv ...string) string {
		f := url.Values{"Action": {"SimulatePrincipalPolicy"}, "Version": {"2010-05-08"}, "PolicySourceArn": {"arn:aws:iam::123456789012:role/example-writer"},
			"ActionNames.member.1": {"cloudwatch:PutMetricAlarm"}, "ResourceArns.member.1": {"arn:aws:cloudwatch:eu-west-1:123456789012:alarm:QueueDepth"}, "MaxItems": {"1000"}}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == "" {
				f.Del(kv[i])
			} else {
				f.Set(kv[i], kv[i+1])
			}
		}
		return f.Encode()
	}
	perms := `{"permissions":["monitoring.alertPolicies.update","monitoring.alertPolicies.delete","monitoring.alertPolicies.create"]}`
	azure := "https://management.azure.com/subscriptions/00000000-0000-0000-0000-00000000000a/resourceGroups/shop-rg/providers/Microsoft.Authorization/permissions?api-version=2022-04-01"
	// The precise entries alone: Azure's permissions are under the
	// subscription --alarms reads, whose wider read holds them too.
	precise := newRun(c)
	precise.allowAsking()
	for _, x := range []struct {
		method, url, ctype, body string
		ok                       bool
	}{
		{"POST", "https://iam.amazonaws.com/", "form", sim(), true},
		{"POST", "https://iam.amazonaws.com/", "form", sim("Action", "GetContextKeysForPrincipalPolicy"), false},
		{"POST", "https://iam.amazonaws.com/", "form", sim("Action", "CreateRole"), false},
		{"POST", "https://iam.amazonaws.com/", "form", sim("Action", "PutRolePolicy", "PolicyDocument", "{}"), false},
		{"POST", "https://iam.amazonaws.com/", "form", sim("Action", "AttachRolePolicy"), false},
		{"POST", "https://iam.amazonaws.com/", "form", sim("Action", "GetRole"), false},                                   // a read, but not the one
		{"POST", "https://iam.amazonaws.com/", "form", sim("Action", "SimulateCustomPolicy"), false},                      // another simulation
		{"POST", "https://iam.amazonaws.com/", "form", sim("PolicySourceArn", "arn:aws:iam::123456789012:role/x"), false}, // another role
		{"POST", "https://iam.amazonaws.com/", "form", sim("PolicyInputList.member.1", `{"Statement":[]}`), false},
		{"POST", "https://iam.amazonaws.com/", "form", sim() + "&Action=CreateRole", false}, // a second action
		{"POST", "https://iam.amazonaws.com/", "form", sim("Version", ""), false},
		{"GET", "https://iam.amazonaws.com/?" + sim(), "", "", false},
		{"POST", "https://iam.amazonaws.com/x", "form", sim(), false},
		{"POST", "https://sts.eu-west-1.amazonaws.com/", "form", sim(), false}, // the simulator, on STS's host
		{"POST", "https://iam.amazonaws.com/", "form", "Action=AssumeRole&Version=2011-06-15", false},
		{"POST", "https://cloudresourcemanager.googleapis.com/v1/projects/example-project:testIamPermissions", "json", perms, true},
		{"POST", "https://cloudresourcemanager.googleapis.com/v1/projects/example-project:testIamPermissions", "json", `{"permissions":["resourcemanager.projects.delete"]}`, false},
		{"POST", "https://cloudresourcemanager.googleapis.com/v1/projects/example-project:testIamPermissions", "json", `{"permissions":["monitoring.alertPolicies.update"],"x":1}`, false},
		{"POST", "https://cloudresourcemanager.googleapis.com/v1/projects/example-project:testIamPermissions", "form", "permissions=monitoring.alertPolicies.update", false},
		{"POST", "https://cloudresourcemanager.googleapis.com/v1/projects/example-project:setIamPolicy", "json", `{"policy":{}}`, false},
		{"POST", "https://cloudresourcemanager.googleapis.com/v1/projects/example-project:setIamPolicy", "json", perms, false}, // by its path alone
		{"POST", "https://cloudresourcemanager.googleapis.com/v1/projects/example-project:undelete", "json", perms, false},
		{"POST", "https://cloudresourcemanager.googleapis.com/v1/projects/example-project:getIamPolicy", "json", `{}`, false},
		{"POST", "https://cloudresourcemanager.googleapis.com/v1/projects/another-project:testIamPermissions", "json", perms, false},
		{"POST", "https://cloudresourcemanager.googleapis.com/v3/projects/example-project:testIamPermissions", "json", perms, false},
		{"DELETE", "https://cloudresourcemanager.googleapis.com/v1/projects/example-project", "", "", false},
		{"GET", azure, "", "", true},
		{"PUT", strings.Split(azure, "/providers/")[0] + "/providers/Microsoft.Insights/metricAlerts/LatencyHigh?api-version=2026-01-01", "json", "{}", false},
		{"PUT", azure, "json", "{}", false},
		{"GET", "https://grafana.example/sub/api/access-control/user/permissions", "", "", true},
		{"GET", "https://grafana.example/sub/api/access-control/user/permissions?reloadcache=true", "", "", false},
		{"POST", "https://grafana.example/sub/api/access-control/user/permissions", "json", "{}", false},
		{"POST", "https://grafana.example/sub/api/access-control/roles", "json", "{}", false},
	} {
		r := req(t, x.method, x.url, "")
		if x.body != "" {
			r, _ = http.NewRequest(x.method, x.url, strings.NewReader(x.body))
			r.Header.Set("Content-Type", map[string]string{"form": "application/x-www-form-urlencoded", "json": "application/json"}[x.ctype])
		}
		if err := p.al.check(r); (err == nil) != x.ok {
			t.Errorf("%s %s %s: %v, want allowed %v", x.method, x.url, x.body, err, x.ok)
		}
		if err := precise.al.check(r); (err == nil) != x.ok && !strings.Contains(x.url, "grafana") && !strings.Contains(x.url, "sts.") {
			t.Errorf("precise: %s %s %s: %v, want allowed %v", x.method, x.url, x.body, err, x.ok)
		}
	}
	for _, x := range []string{strings.Replace(azure, "shop-rg", "other-rg", 1), azure + "&$filter=x", strings.Replace(azure, "2022-04-01", "2015-07-01", 1)} {
		if err := precise.al.check(req(t, "GET", x, "")); err == nil {
			t.Errorf("precise: GET %s allowed", x)
		}
	}
	// What a precise entry holds is counted by its API, not the wider
	// read's.
	p.al.check(req(t, "GET", azure, ""))
	if a := p.al.asked(); a.By["iam:azure"] != 2 || a.By["alarms:az"] != 0 {
		t.Errorf("%+v", a)
	}
}

// iamAnswer is IAM's answer to SimulatePrincipalPolicy for each action and
// resource the form asks, decided by decide (a decision, and the context
// keys missing), as the query API gives it.
func iamAnswer(form url.Values, decide func(action, resource string) (string, []string), truncated bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<SimulatePrincipalPolicyResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/"><SimulatePrincipalPolicyResult><IsTruncated>%v</IsTruncated><EvaluationResults>`, truncated)
	for i := 1; form.Get(fmt.Sprintf("ActionNames.member.%d", i)) != ""; i++ {
		a := form.Get(fmt.Sprintf("ActionNames.member.%d", i))
		fmt.Fprintf(&b, `<member><EvalActionName>%s</EvalActionName><EvalResourceName>*</EvalResourceName><EvalDecision>allowed</EvalDecision><ResourceSpecificResults>`, a)
		for j := 1; form.Get(fmt.Sprintf("ResourceArns.member.%d", j)) != ""; j++ {
			r := form.Get(fmt.Sprintf("ResourceArns.member.%d", j))
			d, missing := decide(a, r)
			fmt.Fprintf(&b, `<member><EvalResourceName>%s</EvalResourceName><EvalResourceDecision>%s</EvalResourceDecision><MissingContextValues>`, r, d)
			for _, k := range missing {
				fmt.Fprintf(&b, `<member>%s</member>`, k)
			}
			b.WriteString(`</MissingContextValues></member>`)
		}
		b.WriteString(`</ResourceSpecificResults></member>`)
	}
	b.WriteString(`</EvaluationResults></SimulatePrincipalPolicyResult></SimulatePrincipalPolicyResponse>`)
	return b.String()
}

// IAM's policy simulator, asked as the role of its own policies: every
// action allowed on every name is ok; one denied, a problem naming it; one
// allowed only with a condition key the simulation isn't given, unknown;
// IAM refusing the simulation itself (no iam:SimulatePrincipalPolicy),
// unknown, saying what would make it known, never a problem.
func TestAWSWrites(t *testing.T) {
	allowed := func(string, string) (string, []string) { return "allowed", nil }
	for _, x := range []struct {
		name      string
		code      int
		decide    func(a, r string) (string, []string)
		truncated bool
		state     State
		says      string
	}{
		{"allowed", 200, allowed, false, OK, "CloudWatch's alarms: cloudwatch:PutMetricAlarm"},
		{"a delete denied", 200, func(a, r string) (string, []string) {
			if a == "cloudwatch:DeleteAlarms" && strings.HasSuffix(r, ":alarm:QueueDepth") {
				return "implicitDeny", nil
			}
			return "allowed", nil
		}, false, Problem, "cloudwatch:DeleteAlarms denied for 1 of 2"},
		{"explicitly denied", 200, func(a, r string) (string, []string) {
			if a == "cloudwatch:PutMetricAlarm" {
				return "explicitDeny", nil
			}
			return "allowed", nil
		}, false, Problem, "cloudwatch:PutMetricAlarm denied for 2 of 2"},
		{"a condition", 200, func(a, r string) (string, []string) {
			if a == "cloudwatch:TagResource" {
				return "implicitDeny", []string{"aws:RequestTag/team"}
			}
			return "allowed", nil
		}, false, Unknown, "on a condition of aws:RequestTag/team"},
		{"truncated", 200, allowed, true, Unknown, "went on past its first page"},
		{"no simulation", 403, nil, false, Unknown, "iam:SimulatePrincipalPolicy on its own ARN"},
	} {
		var forms []url.Values
		h := standIns(t, map[string]http.Handler{"iam.amazonaws.com": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			f, _ := url.ParseQuery(string(b))
			forms = append(forms, f)
			if x.code != 200 {
				w.WriteHeader(x.code)
				io.WriteString(w, `<ErrorResponse><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>User: arn:aws:sts::123456789012:assumed-role/example-writer/lapilli is not authorized to perform: iam:SimulatePrincipalPolicy on resource: role example-writer</Message></Error></ErrorResponse>`)
				return
			}
			io.WriteString(w, iamAnswer(f, x.decide, x.truncated))
		})})
		c := writesConfig(t)
		c.Serve.Alarms = c.Serve.Alarms[:1]
		c.Serve.ChangesIn = []string{"cw:checkout-*", "cw:QueueDepth"}
		p := guarded(t, c)
		it := judged(p, p.awsWrites(context.Background(), writer{id: "changes.aws", flag: "--changes-aws-role", who: c.Serve.ChangesAWSRole, iam: signed}))
		if it.State != x.state || !strings.Contains(it.What+" "+it.Why+" "+it.Fix, x.says) {
			t.Errorf("%s: %+v", x.name, it)
		}
		if len(forms) != 1 || forms[0].Get("ResourceArns.member.1") != "arn:aws:cloudwatch:eu-west-1:123456789012:alarm:checkout-*" ||
			forms[0].Get("ResourceArns.member.2") != "arn:aws:cloudwatch:eu-west-1:123456789012:alarm:QueueDepth" || forms[0].Get("ActionNames.member.6") == "" {
			t.Errorf("%s: asked %v", x.name, forms)
		}
		if a := p.al.asked(); a.Refused != 0 || a.By["iam:aws"] != 1 || len(h.all()) != 1 {
			t.Errorf("%s: %+v, sent %v", x.name, a, h.all())
		}
	}
	// Amazon's namespaces, the ruler's, each by its ARN in the workspace.
	var form url.Values
	standIns(t, map[string]http.Handler{"iam.amazonaws.com": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(b))
		io.WriteString(w, iamAnswer(form, allowed, false))
	})})
	c := writesConfig(t)
	c.Serve.Alarms, c.Serve.Ruler, c.Serve.ChangesIn = nil, "https://aps.eu-west-1.amazonaws.com/workspaces/ws-example", []string{"shop"}
	p := guarded(t, c)
	if it := p.awsWrites(context.Background(), writer{id: "changes.aws", flag: "--changes-aws-role", who: c.Serve.ChangesAWSRole, iam: signed}); it.State != OK ||
		form.Get("ActionNames.member.1") != "aps:PutRuleGroupsNamespace" || form.Get("ResourceArns.member.1") != "arn:aws:aps:eu-west-1:123456789012:rulegroupsnamespace/ws-example/shop" {
		t.Errorf("%+v %v", it, form)
	}
}

// azurePerms answers Azure's permissions of the caller, for each scope by
// its path, as Resource Manager gives them.
func azurePerms(t *testing.T, by map[string]string, asked *[]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*asked = append(*asked, r.Method+" "+r.URL.RequestURI())
		if r.Method != "GET" || r.URL.Query().Get("api-version") != "2022-04-01" || r.Header.Get("Authorization") == "" {
			t.Errorf("Azure was sent %s %s", r.Method, r.URL)
		}
		if r.URL.Query().Get("$skiptoken") != "" {
			io.WriteString(w, `{"value":[]}`)
			return
		}
		b, ok := by[strings.TrimSuffix(r.URL.Path, "/providers/Microsoft.Authorization/permissions")]
		if !ok {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"code":"ResourceGroupNotFound","message":"Resource group 'shop-rg' could not be found in 00000000-0000-0000-0000-00000000000a."}}`)
			return
		}
		io.WriteString(w, b)
	})
}

// Azure's permissions of the identity on each scope, judged against the
// actions serve's writes need: actions less the same entry's notActions,
// wildcards and letter case as Azure has them. A name given whole may be
// given on its alert rule alone: not on the group is unknown then; a
// prefix needs the group, and not there is a problem.
func TestAzureWrites(t *testing.T) {
	const rg = "/subscriptions/00000000-0000-0000-0000-00000000000a/resourceGroups/shop-rg"
	for _, x := range []struct {
		name  string
		perms string
		state State
		says  string
	}{
		{"everything", `{"value":[{"actions":["*"],"notActions":[]}]}`, OK, "Microsoft.Insights/metricAlerts/delete"},
		{"Insights, lower case", `{"value":[{"actions":["microsoft.insights/*"],"notActions":[]}]}`, OK, "a deny assignment"},
		{"no deletes", `{"value":[{"actions":["*"],"notActions":["Microsoft.Insights/*/delete"]}]}`, Problem, "not given Microsoft.Insights/metricAlerts/delete"},
		{"notActions of another entry", `{"value":[{"actions":["*"],"notActions":["Microsoft.Insights/metricAlerts/delete"]},{"actions":["Microsoft.Insights/metricAlerts/*"]}]}`, OK, ""},
		{"writes only", `{"value":[{"actions":["*/write"]}]}`, Problem, "not given Microsoft.Insights/metricAlerts/delete"},
		{"reads only", `{"value":[{"actions":["*/read"],"notActions":[]}]}`, Problem, "metricAlerts/write"},
		{"a wildcard in the middle", `{"value":[{"actions":["Microsoft.Insights/*Alerts/*","Microsoft.Insights/scheduledQueryRules/write"]}]}`, OK, ""},
	} {
		var asked []string
		standIns(t, map[string]http.Handler{"management.azure.com": azurePerms(t, map[string]string{rg: x.perms}, &asked)})
		c := writesConfig(t)
		c.Serve.Alarms = c.Serve.Alarms[1:2]
		c.Serve.ChangesIn = []string{"az:shop-rg/checkout-*"}
		p := guarded(t, c)
		it := judged(p, p.azureWrites(context.Background(), writer{id: "changes.azure", flag: "--changes-azure-client-id", who: c.Serve.ChangesAzureClientID, sign: signed}))
		if it.State != x.state || !strings.Contains(it.What+" "+it.Why, x.says) {
			t.Errorf("%s: %+v", x.name, it)
		}
		if a := p.al.asked(); len(asked) != 1 || a.Refused != 0 || a.By["iam:azure"] != 1 {
			t.Errorf("%s: asked %v, %+v", x.name, asked, a)
		}
	}
	// A name given whole, the group not giving it: unknown. A rule group
	// of the ruler, named whole, is asked on itself; its resource group
	// for a prefix; the next page followed on the same host and path.
	var asked []string
	group := rg + "/providers/Microsoft.AlertsManagement/prometheusRuleGroups/api"
	standIns(t, map[string]http.Handler{"management.azure.com": azurePerms(t, map[string]string{
		rg:    `{"value":[{"actions":["*/read"]}],"nextLink":"https://management.azure.com` + rg + `/providers/Microsoft.Authorization/permissions?api-version=2022-04-01&$skiptoken=2"}`,
		group: `{"value":[{"actions":["Microsoft.AlertsManagement/prometheusRuleGroups/*"]}]}`,
		rg + "/providers/Microsoft.AlertsManagement/prometheusRuleGroups/edge rules": `{"value":[{"actions":["*"]}]}`,
	}, &asked)})
	c := writesConfig(t)
	c.Serve.Alarms = c.Serve.Alarms[1:2]
	c.Serve.Ruler = "https://management.azure.com/subscriptions/00000000-0000-0000-0000-00000000000a/resourceGroups/shop-rg"
	c.Serve.ChangesIn = []string{"az:shop-rg/LatencyHigh", "api", "edge rules"}
	p := guarded(t, c)
	it := judged(p, p.azureWrites(context.Background(), writer{id: "changes.azure", flag: "--changes-azure-client-id", who: c.Serve.ChangesAzureClientID, sign: signed}))
	if it.State != Unknown || !strings.Contains(it.Why, "may be given on each alert rule") || !strings.Contains(it.Why, "--ruler's rule groups on a rule group --changes-in names: Microsoft.AlertsManagement/prometheusRuleGroups/write") {
		t.Errorf("%+v", it)
	}
	if !slices.ContainsFunc(asked, func(a string) bool { return strings.Contains(a, "prometheusRuleGroups/api/providers") }) ||
		!slices.ContainsFunc(asked, func(a string) bool { return strings.Contains(a, "skiptoken=2") }) ||
		!slices.ContainsFunc(asked, func(a string) bool { return strings.Contains(a, "prometheusRuleGroups/edge%20rules/providers") }) ||
		p.al.asked().Refused != 0 || p.al.asked().By["iam:azure"] != 4 {
		t.Errorf("asked %v, %+v", asked, p.al.asked())
	}
	// A next page elsewhere isn't followed, nor asked.
	asked = nil
	standIns(t, map[string]http.Handler{"management.azure.com": azurePerms(t, map[string]string{
		rg: `{"value":[{"actions":["*"]}],"nextLink":"https://elsewhere.example/permissions?api-version=2022-04-01"}`}, &asked)})
	c.Serve.Ruler, c.Serve.ChangesIn = "", []string{"az:shop-rg/checkout-*"}
	p = guarded(t, c)
	if it := judged(p, p.azureWrites(context.Background(), writer{id: "changes.azure", flag: "--changes-azure-client-id", sign: signed})); it.State != Unknown ||
		!strings.Contains(it.Why, "isn't followed") || len(asked) != 1 {
		t.Errorf("%+v %v", it, asked)
	}
}

// Azure's wildcards: * any text, / and . among it, in any letter case.
func TestAzureMatch(t *testing.T) {
	for _, x := range []struct {
		pattern, action string
		ok              bool
	}{
		{"*", "Microsoft.Insights/metricAlerts/write", true},
		{"Microsoft.Insights/*", "Microsoft.Insights/metricAlerts/write", true},
		{"microsoft.insights/METRICALERTS/write", "Microsoft.Insights/metricAlerts/write", true},
		{"*/write", "Microsoft.Insights/metricAlerts/write", true},
		{"*/read", "Microsoft.Insights/metricAlerts/write", false},
		{"Microsoft.Insights/*/delete", "Microsoft.Insights/metricAlerts/delete", true},
		{"Microsoft.Insights/*/delete", "Microsoft.Insights/metricAlerts/write", false},
		{"Microsoft.Insights/metricAlerts", "Microsoft.Insights/metricAlerts/write", false},
		{"Microsoft.Insight*", "Microsoft.Insights/metricAlerts/write", true},
		{"Microsoft.Compute/*", "Microsoft.Insights/metricAlerts/write", false},
		{"*Alerts/write", "Microsoft.Insights/metricAlerts/delete", false},
		{"Microsoft.*/*Alerts/*", "Microsoft.Insights/activityLogAlerts/write", true},
	} {
		if got := azureMatch(x.pattern, x.action); got != x.ok {
			t.Errorf("%s ~ %s: %v", x.pattern, x.action, got)
		}
	}
}

// Cloud Resource Manager's testIamPermissions on the project of Google's
// --alarms, as the service account, with a read-only token: all three of
// the alerting policies' writes held is ok; some, a problem naming the
// rest; an error, unknown.
func TestGoogleWrites(t *testing.T) {
	scopes := []string{}
	was := googleAs
	googleAs = func(_ context.Context, as, scope string) (func(*http.Request) error, error) {
		scopes = append(scopes, as+" "+scope)
		return signed, nil
	}
	t.Cleanup(func() { googleAs = was })
	for _, x := range []struct {
		code  int
		holds string
		state State
		says  string
	}{
		{200, `{"permissions":["monitoring.alertPolicies.create","monitoring.alertPolicies.delete","monitoring.alertPolicies.update"]}`, OK, "it holds"},
		{200, `{"permissions":["monitoring.alertPolicies.update"]}`, Problem, "doesn't hold monitoring.alertPolicies.create, monitoring.alertPolicies.delete"},
		{200, `{}`, Problem, "doesn't hold monitoring.alertPolicies.create, monitoring.alertPolicies.delete, monitoring.alertPolicies.update"},
		{403, `{"error":{"code":403,"message":"Cloud Resource Manager API has not been used in project 123456789012 before or it is disabled.","status":"PERMISSION_DENIED"}}`, Unknown, "didn't say"},
	} {
		var bodies []string
		standIns(t, map[string]http.Handler{"cloudresourcemanager.googleapis.com": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			bodies = append(bodies, r.Method+" "+r.URL.Path+" "+string(b))
			w.WriteHeader(x.code)
			io.WriteString(w, x.holds)
		})})
		c := writesConfig(t)
		c.Serve.Alarms = c.Serve.Alarms[2:3]
		p := guarded(t, c)
		it := judged(p, p.googleWrites(context.Background(), writer{id: "changes.google", flag: "--changes-google-service-account", who: c.Serve.ChangesGoogleServiceAccount}))
		if it.State != x.state || !strings.Contains(it.Why, x.says) {
			t.Errorf("%d %s: %+v", x.code, x.holds, it)
		}
		if len(bodies) != 1 || bodies[0] != `POST /v1/projects/example-project:testIamPermissions {"permissions":["monitoring.alertPolicies.create","monitoring.alertPolicies.delete","monitoring.alertPolicies.update"]}` {
			t.Errorf("sent %v", bodies)
		}
		if a := p.al.asked(); a.Refused != 0 || a.By["iam:google"] != 1 {
			t.Errorf("%+v", a)
		}
	}
	if scopes[0] != "example-writer@example-project.iam.gserviceaccount.com https://www.googleapis.com/auth/cloud-platform.read-only" {
		t.Errorf("%v", scopes)
	}
}

// OCI's policy statements are read conservatively: what the syntax has,
// and nothing else, which is never a grant.
func TestParseOCI(t *testing.T) {
	for _, x := range []struct {
		st                 string
		kind, verb, res    string
		subjects, location string
		cond               bool
	}{
		{"Allow group AlarmWriters to manage alarms in compartment shop", "allow", "manage", "alarms", "group::AlarmWriters", "shop", false},
		{"allow group A, B to read metrics in tenancy", "allow", "read", "metrics", "group::A group::B", "tenancy", false},
		{"Allow group id ocid1.group.oc1..examplewriters to manage all-resources in compartment id ocid1.compartment.oc1..exampleshop", "allow", "manage", "all-resources",
			"group-id::ocid1.group.oc1..examplewriters", "id ocid1.compartment.oc1..exampleshop", false},
		{"Allow group 'Default'/'Alarm Writers' to manage alarms in compartment Apps:shop", "allow", "manage", "alarms", "group:Default:Alarm Writers", "Apps:shop", false},
		{"Allow any-user to read metrics in compartment shop", "allow", "read", "metrics", "any-user::", "shop", false},
		{"Allow dynamic-group nodes to manage alarms in compartment shop", "allow", "manage", "alarms", "dynamic-group::nodes", "shop", false},
		{"Allow group G to manage alarms in compartment shop where any {request.permission = 'ALARM_UPDATE'}", "allow", "manage", "alarms", "group::G", "shop", true},
		{"Allow group G to {ALARM_UPDATE, METRIC_READ} in compartment shop", "allow", "", "", "group::G", "shop", false},
		{"Deny group G to manage alarms in tenancy", "deny", "manage", "alarms", "group::G", "tenancy", false},
		{"Define tenancy Other as ocid1.tenancy.oc1..exampleother", "other", "", "", "", "", false},
		{"Endorse group G to manage alarms in tenancy Other", "other", "", "", "", "", false},
		{"Allow group G to manage alarms", "", "", "", "", "", false},
		{"Allow group G to fly alarms in tenancy", "", "", "", "", "", false},
		{"Allow group G manage alarms in tenancy", "", "", "", "", "", false},
		{"Allow group 'G to manage alarms in tenancy", "", "", "", "", "", false},
		{"Allow group G to manage alarms in tenancy and more", "", "", "", "", "", false},
		{"Allow to manage alarms in tenancy", "", "", "", "", "", false},
		{"Allow group G to manage alarms in compartment", "", "", "", "", "", false},
		{"", "", "", "", "", "", false},
	} {
		got := parseOCI(x.st)
		var subs []string
		for _, s := range got.subjects {
			subs = append(subs, s.kind+":"+s.domain+":"+s.name)
		}
		loc := strings.Join(got.loc.path, ":")
		if got.loc.tenancy {
			loc = "tenancy"
		}
		if got.loc.id != "" {
			loc = "id " + got.loc.id
		}
		if got.kind != x.kind || got.kind != "" && got.kind != "other" &&
			(got.verb != x.verb || got.resource != x.res || strings.Join(subs, " ") != x.subjects || loc != x.location || got.cond != x.cond) {
			t.Errorf("%q: %+v", x.st, got)
		}
	}
}

// ociChain is the tenancy, Apps under it, shop under Apps.
var ociChain = []ociCompartment{{id: "ocid1.tenancy.oc1..example"}, {id: "ocid1.compartment.oc1..exampleapps", name: "Apps"},
	{id: "ocid1.compartment.oc1..exampleshop", name: "shop"}}

// What the policies above shop give the user: a statement of theirs,
// where it applies, as its location reads from where it is attached. One
// with a condition, a deny, one not understood, a group of another
// domain: never a grant.
func TestOCIJudge(t *testing.T) {
	g := ociGroups{ids: []string{"ocid1.group.oc1..examplewriters"}, names: []string{"AlarmWriters"}}
	both := func(st string) []string { return []string{st, "Allow group AlarmWriters to read metrics in tenancy"} }
	for _, x := range []struct {
		name     string
		policies [][]string
		granted  bool // ALARM_UPDATE granted
		perhaps  bool
		denies   int
	}{
		{"the tenancy's, by path", [][]string{both("Allow group AlarmWriters to manage alarms in compartment Apps:shop")}, true, false, 0},
		{"the tenancy's, the parent", [][]string{both("allow group alarmwriters to manage alarms in compartment Apps")}, true, false, 0},
		{"the tenancy's, the name alone", [][]string{both("Allow group AlarmWriters to manage alarms in compartment shop")}, false, false, 0},
		{"the tenancy's, in tenancy", [][]string{both("Allow group AlarmWriters to manage all-resources in tenancy")}, true, false, 0},
		{"Apps's, by name", [][]string{both(""), {"Allow group AlarmWriters to manage alarms in compartment shop"}}, true, false, 0},
		{"Apps's, in tenancy", [][]string{both(""), {"Allow group AlarmWriters to manage alarms in tenancy"}}, false, false, 0},
		{"shop's, its own name", [][]string{both(""), nil, {"Allow group AlarmWriters to manage alarms in compartment shop"}}, true, false, 0},
		{"by OCID", [][]string{both("Allow group id ocid1.group.oc1..examplewriters to manage alarms in compartment id ocid1.compartment.oc1..exampleapps")}, true, false, 0},
		{"another compartment", [][]string{both("Allow group AlarmWriters to manage alarms in compartment Apps:payments")}, false, false, 0},
		{"another group", [][]string{both("Allow group Readers to manage alarms in tenancy")}, false, false, 0},
		{"a dynamic group", [][]string{both("Allow dynamic-group AlarmWriters to manage alarms in tenancy")}, false, false, 0},
		{"any user", [][]string{both("Allow any-user to manage alarms in tenancy")}, true, false, 0},
		{"use is not manage", [][]string{both("Allow group AlarmWriters to use alarms in tenancy")}, false, false, 0},
		{"a condition", [][]string{both("Allow group AlarmWriters to manage alarms in tenancy where request.permission = 'ALARM_UPDATE'")}, false, true, 0},
		{"a permission list", [][]string{both("Allow group AlarmWriters to {ALARM_UPDATE} in tenancy")}, false, true, 0},
		{"another domain", [][]string{both("Allow group 'Ops'/'AlarmWriters' to manage alarms in tenancy")}, false, true, 0},
		{"the Default domain", [][]string{both("Allow group 'Default'/'AlarmWriters' to manage alarms in tenancy")}, true, false, 0},
		{"not understood", [][]string{both("Allow group AlarmWriters to manage alarms in tenancy, please")}, false, true, 0},
		{"a deny", [][]string{append(both("Allow group AlarmWriters to manage alarms in tenancy"), "Deny group AlarmWriters to manage alarms in tenancy")}, true, false, 1},
	} {
		j := ociJudge(ociChain, x.policies, g, false)
		if j.granted["ALARM_UPDATE"] != x.granted || j.perhaps["ALARM_UPDATE"] != x.perhaps || j.denies != x.denies || !j.granted["METRIC_READ"] && x.policies[0][1] != "" {
			t.Errorf("%s: %+v", x.name, j)
		}
	}
	// Every compartment of the tenancy: only the tenancy covers them all.
	if j := ociJudge(ociChain[:1], [][]string{both("Allow group AlarmWriters to manage alarms in compartment Apps")}, g, true); j.granted["ALARM_UPDATE"] || !j.perhaps["ALARM_UPDATE"] {
		t.Errorf("subtree: %+v", j)
	}
	if j := ociJudge(ociChain[:1], [][]string{both("Allow group AlarmWriters to manage alarms in tenancy")}, g, true); !j.granted["ALARM_UPDATE"] {
		t.Errorf("subtree, the tenancy: %+v", j)
	}
}

// ociIdentity is a stand-in for OCI's Identity API: the user's groups,
// the compartments, the policies, each as the API gives it, noted as
// asked.
func ociIdentity(t *testing.T, policies map[string][]string, asked *[]string, refuse string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*asked = append(*asked, r.Method+" "+r.URL.RequestURI())
		if r.Method != "GET" || r.Header.Get("Authorization") == "" {
			t.Errorf("Identity was sent %s %s", r.Method, r.URL)
		}
		if refuse != "" && strings.HasPrefix(r.URL.Path, refuse) {
			w.WriteHeader(404)
			io.WriteString(w, `{"code":"NotAuthorizedOrNotFound","message":"Authorization failed or requested resource not found for `+
				`ocid1.compartment.oc1..exampleshop, user ocid1.user.oc1..examplewriter in AlarmWriters."}`)
			return
		}
		q := r.URL.Query()
		switch p := r.URL.Path; {
		case p == "/20160918/userGroupMemberships" && q.Get("compartmentId") == "ocid1.tenancy.oc1..example" && q.Get("userId") == "ocid1.user.oc1..examplewriter":
			io.WriteString(w, `[{"groupId":"ocid1.group.oc1..examplewriters","userId":"ocid1.user.oc1..examplewriter"}]`)
		case p == "/20160918/groups/ocid1.group.oc1..examplewriters":
			io.WriteString(w, `{"id":"ocid1.group.oc1..examplewriters","name":"AlarmWriters"}`)
		case p == "/20160918/compartments/ocid1.compartment.oc1..exampleshop":
			io.WriteString(w, `{"id":"ocid1.compartment.oc1..exampleshop","name":"shop","compartmentId":"ocid1.compartment.oc1..exampleapps"}`)
		case p == "/20160918/compartments/ocid1.compartment.oc1..exampleapps":
			io.WriteString(w, `{"id":"ocid1.compartment.oc1..exampleapps","name":"Apps","compartmentId":"ocid1.tenancy.oc1..example"}`)
		case p == "/20160918/policies":
			c := q.Get("compartmentId")
			if q.Get("page") == "" && c == "ocid1.tenancy.oc1..example" {
				w.Header().Set("opc-next-page", "page-2")
				io.WriteString(w, `[{"name":"read","statements":["Allow group AlarmWriters to read metrics in tenancy"]}]`)
				return
			}
			if q.Get("page") == "page-2" {
				c += "#2"
			}
			b, _ := json.Marshal([]map[string]any{{"name": "p", "statements": policies[c]}})
			w.Write(b)
		default:
			t.Errorf("Identity was sent %s, which it doesn't serve", r.URL)
			w.WriteHeader(404)
		}
	})
}

// OCI's: the user's groups, the compartments from the alarms' up to the
// tenancy, every page of the policies of each, read as the user and
// judged. A condition is unknown, "a policy with a condition"; no grant, a
// problem; the policies unreadable, unknown, saying what would let them be
// read.
func TestOCIWrites(t *testing.T) {
	was := ociConfigUser
	ociConfigUser = func(string) (cloud.OCIUser, error) {
		return cloud.OCIUser{Tenancy: "ocid1.tenancy.oc1..example", User: "ocid1.user.oc1..examplewriter", Region: "eu-frankfurt-1",
			Identity: "https://identity.eu-frankfurt-1.oci.oraclecloud.com"}, nil
	}
	t.Cleanup(func() { ociConfigUser = was })
	for _, x := range []struct {
		name     string
		policies map[string][]string
		refuse   string
		state    State
		says     string
	}{
		{"granted", map[string][]string{"ocid1.compartment.oc1..exampleapps": {"Allow group AlarmWriters to manage alarms in compartment shop"}}, "", OK, "manage alarms, read metrics"},
		{"on the second page", map[string][]string{"ocid1.tenancy.oc1..example#2": {"Allow group AlarmWriters to manage alarms in compartment Apps:shop"}}, "", OK, ""},
		{"a condition", map[string][]string{"ocid1.compartment.oc1..exampleshop": {
			"Allow group AlarmWriters to manage alarms in compartment shop where any {request.permission = 'ALARM_INSPECT', request.permission = 'ALARM_UPDATE'}"}}, "",
			Unknown, "manage alarms given only by a policy with a condition"},
		{"none", map[string][]string{"ocid1.compartment.oc1..exampleshop": {"Allow group AlarmWriters to read alarms in compartment shop"}}, "", Problem, "no policy gives the user manage alarms"},
		{"unreadable", nil, "/20160918/policies", Unknown, "inspect groups, users, compartments and policies in the tenancy"},
	} {
		var asked []string
		h := standIns(t, map[string]http.Handler{"identity.eu-frankfurt-1.oci.oraclecloud.com": ociIdentity(t, x.policies, &asked, x.refuse)})
		c := writesConfig(t)
		c.Serve.Alarms = c.Serve.Alarms[3:]
		p := guarded(t, c)
		it := judged(p, p.ociWrites(context.Background(), writer{id: "changes.oci", flag: "--changes-oci-config", who: c.Serve.ChangesOCIConfig, sign: signed}))
		if it.State != x.state || !strings.Contains(it.Why+" "+it.Fix, x.says) {
			t.Errorf("%s: %+v", x.name, it)
		}
		if a := p.al.asked(); a.Refused != 0 || a.By["iam:oci"] != len(h.all()) {
			t.Errorf("%s: %+v, sent %v", x.name, a, h.all())
		}
		if x.refuse == "" && len(asked) != 8 { // memberships, a group, two compartments, the tenancy's two pages of policies, Apps's, shop's
			t.Errorf("%s: asked %d: %v", x.name, len(asked), asked)
		}
	}
}

// With --offline, the checks of writes ask nothing, and are unknown.
func TestWritesOffline(t *testing.T) {
	h := standIns(t, map[string]http.Handler{})
	c := writesConfig(t)
	c.Offline = true
	c.Serve.Grafana, c.Serve.ChangesGrafanaToken = "http://grafana.example", "/etc/lapilli/grafana-writes"
	rep := Run(context.Background(), c)
	for _, id := range []string{"grafana.write", "changes.cloud", "changes.aws", "changes.azure", "changes.google", "changes.oci"} {
		if it := reportItem(rep, id); it == nil || it.State != Unknown || !strings.Contains(it.Why, "--offline") {
			t.Errorf("%s: %+v", id, it)
		}
	}
	if len(h.all()) != 0 || rep.Asked.Requests+rep.Asked.Refused != 0 {
		t.Errorf("sent %v, %+v", h.all(), rep.Asked)
	}
}

func reportItem(r *Report, id string) *Item {
	for i := range r.Items {
		if r.Items[i].ID == id {
			return &r.Items[i]
		}
	}
	return nil
}

// Run, with every cloud's identity and Grafana's token, against
// stand-ins for each API: what it sends is reads and the two POSTs that
// only answer, each one the allowlist holds, none refused, every one
// counted; and the report names no identity, account, project,
// subscription, compartment or group, nor what a server said of them.
func TestWritesNothingWrittenAndRedacted(t *testing.T) {
	// Nothing of this machine's credentials is read: step 1's reads of
	// the alarms fail at once, each cloud's chain finding none.
	dir := t.TempDir()
	for k, v := range map[string]string{"HOME": dir, "CLOUDSDK_CONFIG": dir, "GOOGLE_APPLICATION_CREDENTIALS": filepath.Join(dir, "none"),
		"AWS_CONFIG_FILE": filepath.Join(dir, "none"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "none"), "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "", "AWS_PROFILE": "", "AWS_ROLE_ARN": "", "AWS_WEB_IDENTITY_TOKEN_FILE": "",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI": "", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "", "AZURE_TOKEN_CREDENTIALS": "EnvironmentCredential",
		"AZURE_CLIENT_ID": "", "AZURE_TENANT_ID": "", "AZURE_CLIENT_SECRET": "", "OCI_CLI_CONFIG_FILE": filepath.Join(dir, "none"),
		"OCI_RESOURCE_PRINCIPAL_VERSION": ""} {
		t.Setenv(k, v)
	}
	// OCI's chain, without a config file, asks the instance's metadata
	// service with a client of its own: one is given, with a key made here.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pemFile := fileOf(t, "key.pem", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
	t.Setenv("OCI_CLI_CONFIG_FILE", fileOf(t, "config", "[DEFAULT]\nuser=ocid1.user.oc1..examplereader\nfingerprint=00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00\n"+
		"tenancy=ocid1.tenancy.oc1..example\nregion=eu-frankfurt-1\nkey_file="+pemFile+"\n"))
	was, wasG, wasO := take, googleAs, ociConfigUser
	take = func(_ context.Context, kind, endpoint, as string) (func(*http.Request) error, func(*http.Request) error, error) {
		return signed, signed, nil
	}
	googleAs = func(context.Context, string, string) (func(*http.Request) error, error) { return signed, nil }
	ociConfigUser = func(string) (cloud.OCIUser, error) {
		return cloud.OCIUser{Tenancy: "ocid1.tenancy.oc1..example", User: "ocid1.user.oc1..examplewriter", Region: "eu-frankfurt-1",
			Identity: "https://identity.eu-frankfurt-1.oci.oraclecloud.com"}, nil
	}
	t.Cleanup(func() { take, googleAs, ociConfigUser = was, wasG, wasO })
	const rg = "/subscriptions/00000000-0000-0000-0000-00000000000a/resourceGroups/shop-rg"
	var asked []string
	// Grafana's a server of its own: its rules are read by a client with
	// a transport of its own (source.Grafana), its list through egress.
	grafana := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/access-control/user/permissions":
			io.WriteString(w, grafanaAs["Viewer"])
		case "/api/v1/provisioning/alert-rules/export":
			io.WriteString(w, "apiVersion: 1\ngroups: []\n")
		default:
			w.WriteHeader(404)
		}
	})
	gs := httptest.NewServer(grafana)
	t.Cleanup(gs.Close)
	h := standIns(t, map[string]http.Handler{
		gs.Listener.Addr().String(): grafana,
		"iam.amazonaws.com": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(403)
			io.WriteString(w, `<ErrorResponse><Error><Code>AccessDenied</Code><Message>User: arn:aws:sts::123456789012:assumed-role/example-writer/lapilli is not authorized to perform: iam:SimulatePrincipalPolicy on resource: role example-writer</Message></Error></ErrorResponse>`)
		}),
		"management.azure.com": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(403)
			io.WriteString(w, `{"error":{"code":"AuthorizationFailed","message":"The client '00000000-0000-0000-0000-00000000000c' `+
				`may not read over '/resourceGroups/shop-rg'."}}`)
		}),
		"cloudresourcemanager.googleapis.com": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(403)
			io.WriteString(w, `{"error":{"code":403,"message":"The caller example-writer@example-project.iam.gserviceaccount.com does not have permission on project example-project","status":"PERMISSION_DENIED"}}`)
		}),
		"identity.eu-frankfurt-1.oci.oraclecloud.com": ociIdentity(t, nil, &asked, "/20160918/compartments/ocid1.compartment.oc1..exampleapps"),
	})
	c := writesConfig(t)
	c.Serve.Grafana = gs.URL
	c.Serve.ChangesGrafanaToken = fileOf(t, "token", "glsa_example_writes_0123456789")
	rep := Run(context.Background(), c)
	sent := h.all()
	news := 0
	for _, s := range sent {
		host := strings.SplitN(strings.SplitN(s, " ", 2)[1], "/", 2)[0]
		if !slices.Contains([]string{gs.Listener.Addr().String(), "iam.amazonaws.com", "management.azure.com", "cloudresourcemanager.googleapis.com",
			"identity.eu-frankfurt-1.oci.oraclecloud.com"}, host) {
			continue // step 1's reads: the rules, the alarms, the history
		}
		news++
		switch {
		case strings.HasPrefix(s, "GET "):
		case s == "POST iam.amazonaws.com/", s == "POST cloudresourcemanager.googleapis.com/v1/projects/example-project:testIamPermissions":
		default:
			t.Errorf("sent %s", s)
		}
	}
	if news < 8 {
		t.Errorf("%d sent to the new APIs: %v", news, sent)
	}
	if b := rep.Asked.By; rep.Asked.Refused != 0 || b["grafana"] != 2 || b["iam:aws"] != 1 || b["iam:google"] != 1 || b["iam:azure"] != 1 ||
		b["iam:oci"] != 4 || b["iam:aws"]+b["iam:google"]+b["iam:azure"]+b["iam:oci"]+1 != news {
		t.Errorf("%+v, sent %v", rep.Asked, sent)
	}
	states := map[string]State{}
	var all strings.Builder
	for _, it := range rep.Items {
		states[it.ID] = it.State
		all.WriteString(it.What + " " + it.Why + " " + it.Fix + "\n")
	}
	if states["grafana.write"] != Problem || states["changes.cloud"] != OK || states["changes.aws"] != Unknown || states["changes.google"] != Unknown ||
		states["changes.azure"] != Unknown || states["changes.oci"] != Unknown {
		t.Errorf("%v\n%s", states, all.String())
	}
	// Each by what it is: the whole identity, not only its parts.
	for _, as := range []string{"[--changes-google-service-account]", "[--changes-azure-client-id]", "[--changes-oci-config's user]", "[--changes-in's resource group]",
		"[--changes-aws-role's account]", "[--changes-aws-role's name]", "[--alarms gcp's project]", "[--alarms oci's compartment]"} {
		if !strings.Contains(all.String(), as) {
			t.Errorf("the report doesn't say %s:\n%s", as, all.String())
		}
	}
	for _, hidden := range []string{"123456789012", "example-writer", "example-project", "00000000-0000-0000-0000-00000000000a", "shop-rg",
		"ocid1.compartment.oc1..exampleshop", "ocid1.tenancy.oc1..example", "ocid1.user.oc1..examplewriter", "00000000-0000-0000-0000-00000000000c",
		"AlarmWriters"} {
		if strings.Contains(all.String(), hidden) {
			t.Errorf("the report says %q:\n%s", hidden, all.String())
		}
	}
}

// What the URLs of --alarms and --ruler name inside them is redacted
// wherever a server's answer says it, with --changes or not: an OCI
// compartment, an Azure subscription, an Amazon workspace everywhere; a
// Google project and an Azure resource group, short names, only in what a
// server said.
func TestHideScopes(t *testing.T) {
	c := writesConfig(t)
	c.Serve.Changes, c.Serve.ChangesIn = false, nil
	c.Serve.Ruler = "https://aps.eu-west-1.amazonaws.com/workspaces/ws-example0123"
	c.Serve.Alarms[1].URL = "https://management.azure.com/subscriptions/00000000-0000-0000-0000-00000000000a/resourceGroups/shop"
	p := newRun(c)
	p.hideScopes()
	p.add(Item{ID: "rules.read", State: Problem, What: "the shop's rules", Why: said(errors.New(`Get "https://telemetry.eu-frankfurt-1.oraclecloud.com/20180401/alarms?`+
		`compartmentId=ocid1.compartment.oc1..exampleshop&limit=1000": refused`)) + "; " + said(errors.New(`resource group 'shop' in 00000000-0000-0000-0000-00000000000a`)) +
		"; " + said(errors.New(`project example-project; workspace ws-example0123`))})
	p.redact()
	it := p.r.Items[0]
	if it.What != "the shop's rules" || it.Why != `Get "https://telemetry.eu-frankfurt-1.oraclecloud.com/20180401/alarms?compartmentId=[--alarms oci's compartment]&limit=1000": `+
		`refused; resource group '[--alarms az's resource group]' in [--alarms az's subscription]; project [--alarms gcp's project]; workspace [--ruler's workspace]` {
		t.Errorf("%q\n%q", it.What, it.Why)
	}
}
