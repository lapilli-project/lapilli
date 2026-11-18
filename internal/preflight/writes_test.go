package preflight

import (
	"context"
	"encoding/json"
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
	"lapilli.dev/internal/rules"
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
		// An empty scope covers no folder (Grafana's evaluator, match).
		{"rule writes of no scope", map[string][]string{"alert.rules:read": {""}, "alert.rules:write": {""}, "alert.rules:create": {""}, "alert.rules:delete": {""},
			"alert.provisioning.provenance:write": {""}, "folders:read": {""}}, Unknown, "folder by folder"},
		{"nothing", map[string][]string{}, Problem, "a read of the rules"},
	} {
		it := grafanaMay(x.perms)
		if it.State != x.state || !strings.Contains(it.What+" "+it.Why, x.says) {
			t.Errorf("%s: %s %q %q", x.name, it.State, it.What, it.Why)
		}
	}
	// A list of 4 MB, 190,000 folders, is counted to mostFolders.
	many := perms(grafanaAs["Editor"])
	for i := range 190000 {
		many["folders:read"] = append(many["folders:read"], fmt.Sprintf("folders:uid:f%06d", i))
	}
	if it := grafanaMay(many); it.State != Unknown || !strings.HasSuffix(it.What, "it may read more than 10000 folders") {
		t.Errorf("many folders: %+v", it)
	}
}

// The user's memberships are read once each, mostMemberships at most;
// mostGroups of them read for their names, and only those hidden: more
// is a group not read, never a grant.
func TestOCIGroupsBounded(t *testing.T) {
	gets := 0
	standIns(t, map[string]http.Handler{"identity.eu-frankfurt-1.oci.oraclecloud.com": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/20160918/userGroupMemberships" {
			var ms []map[string]string
			for i := range 1500 {
				id := fmt.Sprintf("ocid1.group.oc1..example%04d", i%1200) // some twice
				ms = append(ms, map[string]string{"groupId": id, "lifecycleState": "ACTIVE"})
			}
			b, _ := json.Marshal(ms)
			w.Write(b)
			return
		}
		gets++
		io.WriteString(w, `{"name":"g`+fmt.Sprint(gets)+`","lifecycleState":"ACTIVE"}`)
	})})
	p := guarded(t, writesConfig(t))
	o := &ociReader{p: p, sign: signed, host: "https://identity.eu-frankfurt-1.oci.oraclecloud.com", tenancy: ociTenancy}
	g, err := o.groups(context.Background(), "ocid1.user.oc1..examplewriter")
	if err != nil || len(g.ids) != mostGroups || len(g.more) != mostMemberships-mostGroups || !g.unread || gets != mostGroups || len(g.names) != mostGroups ||
		len(p.hidden) != mostGroups {
		t.Errorf("%v: %d ids, %d more, unread %v, %d read, %d names, %d hidden", err, len(g.ids), len(g.more), g.unread, gets, len(g.names), len(p.hidden))
	}
	// A statement of a group not read is never a grant.
	x := parseOCI("Allow group id " + g.more[0] + " to manage alarms in tenancy")
	if x.applies(g) != maybe {
		t.Errorf("a group not read: %v", x.applies(g))
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
// alone, with the region asked in as its one context entry (no other
// action, a read or not, nor another role's, region or key); Cloud
// Resource Manager's testIamPermissions on the project of --alarms alone,
// with the alerting policies' permissions as its body; Grafana's list,
// read. A form or a query that doesn't read whole is refused.
func TestAllowWrites(t *testing.T) {
	c := writesConfig(t)
	c.Serve.Grafana, c.Serve.ChangesGrafanaToken = "https://grafana.example/sub", "/etc/lapilli/grafana-writes"
	p := newRun(c)
	p.allowAll()
	sim := func(kv ...string) string {
		f := url.Values{"Action": {"SimulatePrincipalPolicy"}, "Version": {"2010-05-08"}, "PolicySourceArn": {"arn:aws:iam::123456789012:role/example-writer"},
			"ActionNames.member.1": {"cloudwatch:PutMetricAlarm"}, "ResourceArns.member.1": {"arn:aws:cloudwatch:eu-west-1:123456789012:alarm:QueueDepth"}, "MaxItems": {"1000"},
			ctxName: {"aws:RequestedRegion"}, ctxType: {"string"}, ctxValue: {"eu-west-1"}}
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
		{"POST", "https://iam.amazonaws.com/", "form", sim(ctxValue, "us-east-1"), false},                  // a region serve doesn't write in
		{"POST", "https://iam.amazonaws.com/", "form", sim(ctxName, "aws:SourceIp"), false},                // another key
		{"POST", "https://iam.amazonaws.com/", "form", sim(ctxType, "stringList"), false},                  // another type
		{"POST", "https://iam.amazonaws.com/", "form", sim(ctxName, "", ctxType, "", ctxValue, ""), false}, // no region
		{"POST", "https://iam.amazonaws.com/", "form", sim("ContextEntries.member.2.ContextKeyName", "aws:PrincipalTag/x"), false},
		{"POST", "https://iam.amazonaws.com/", "form", sim("ContextEntries.member.1.ContextKeyValues.member.2", "us-east-1"), false},
		{"POST", "https://iam.amazonaws.com/", "form", sim() + ";Action=CreateRole", false},            // a pair that doesn't read
		{"POST", "https://iam.amazonaws.com/", "form", sim() + "&PolicyInputList.member.1=x;y", false}, // dropped, the rest would pass
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
		{"GET", "https://grafana.example/sub/api/access-control/user/permissions", "", "", true},
		{"GET", "https://grafana.example/sub/api/access-control/user/permissions?reloadcache=true", "", "", false},
		{"GET", "https://grafana.example/sub/api/v1/provisioning/alert-rules/export?format=yaml;x=1", "", "", false},     // a query that doesn't read
		{"GET", "https://grafana.example/sub/api/v1/provisioning/alert-rules/export?format=yaml&x=1;y=2", "", "", false}, // dropped, the rest would pass
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

// cwDocs are CloudWatch's alarms as --alarms read them: their ARNs, two
// under checkout-, one of them in another account, which serve doesn't
// write in, and another.
func cwDocs(names ...string) []rules.Doc {
	var out []rules.Doc
	for _, n := range names {
		acct := "123456789012"
		if strings.HasPrefix(n, "other:") {
			acct, n = "210987654321", strings.TrimPrefix(n, "other:")
		}
		out = append(out, rules.Doc{Source: "arn:aws:cloudwatch:eu-west-1:" + acct + ":alarm:" + n,
			Data: []byte(`{"MetricAlarms":[{"AlarmName":"` + n + `"}]}`)})
	}
	return out
}

// IAM's policy simulator, asked as the role of its own policies, given the
// region asked in: every action allowed on every name is ok; one denied, a
// problem naming it; one decided with a context key the simulation lacks,
// allowed or not, unknown; IAM refusing the simulation itself (no
// iam:SimulatePrincipalPolicy), unknown, saying what would make it known,
// never a problem. A prefix is asked of the alarms read under it, in the
// role's account, never as written.
func TestAWSWrites(t *testing.T) {
	allowed := func(string, string) (string, []string) { return "allowed", nil }
	const queue = "arn:aws:cloudwatch:eu-west-1:123456789012:alarm:QueueDepth"
	for _, x := range []struct {
		name      string
		code      int
		decide    func(a, r string) (string, []string)
		truncated bool
		state     State
		says      string
	}{
		{"allowed", 200, allowed, false, OK, "CloudWatch's alarms of --alarms cw: cloudwatch:PutMetricAlarm"},
		{"a delete denied", 200, func(a, r string) (string, []string) {
			if a == "cloudwatch:DeleteAlarms" && r == queue {
				return "implicitDeny", nil
			}
			return "allowed", nil
		}, false, Problem, "cloudwatch:DeleteAlarms denied for 1 of 3"},
		{"a name under the prefix denied", 200, func(a, r string) (string, []string) {
			if a == "cloudwatch:PutMetricAlarm" && strings.HasSuffix(r, ":alarm:checkout-b") {
				return "explicitDeny", nil
			}
			return "allowed", nil
		}, false, Problem, "cloudwatch:PutMetricAlarm denied for 1 of 3"},
		{"a condition", 200, func(a, r string) (string, []string) {
			if a == "cloudwatch:TagResource" {
				return "implicitDeny", []string{"aws:RequestTag/team"}
			}
			return "allowed", nil
		}, false, Unknown, "on a condition of aws:RequestTag/team"},
		{"allowed on a key it lacks", 200, func(a, r string) (string, []string) {
			if a == "cloudwatch:PutMetricAlarm" {
				return "allowed", []string{"aws:ResourceTag/team"}
			}
			return "allowed", nil
		}, false, Unknown, "cloudwatch:PutMetricAlarm for 3 of 3 on a condition of aws:ResourceTag/team"},
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
		p.read("alarms:cw", cwDocs("checkout-a", "checkout-b", "other:checkout-c", "Disk"))
		it := judged(p, p.awsWrites(context.Background(), writer{id: "changes.aws", flag: "--changes-aws-role", who: c.Serve.ChangesAWSRole, iam: signed}))
		if it.State != x.state || !strings.Contains(it.What+" "+it.Why+" "+it.Fix, x.says) {
			t.Errorf("%s: %+v", x.name, it)
		}
		if it.State == OK && !strings.Contains(it.What, "given the region; an SCP's conditions on any other key aren't judged") {
			t.Errorf("%s: no caveat: %+v", x.name, it)
		}
		if len(forms) != 1 || forms[0].Get("ResourceArns.member.1") != "arn:aws:cloudwatch:eu-west-1:123456789012:alarm:checkout-a" ||
			forms[0].Get("ResourceArns.member.2") != "arn:aws:cloudwatch:eu-west-1:123456789012:alarm:checkout-b" ||
			forms[0].Get("ResourceArns.member.3") != queue || forms[0].Get("ResourceArns.member.4") != "" || forms[0].Get("ActionNames.member.5") == "" ||
			forms[0].Get("ActionNames.member.6") != "" || forms[0].Get(ctxName) != "aws:RequestedRegion" || forms[0].Get(ctxValue) != "eu-west-1" {
			t.Errorf("%s: asked %v", x.name, forms)
		}
		if a := p.al.asked(); a.Refused != 0 || a.By["iam:aws"] != 1 || len(h.all()) != 1 {
			t.Errorf("%s: %+v, sent %v", x.name, a, h.all())
		}
	}
	// A prefix with nothing read under it, or names past twenty, are
	// unknown, never ok; the names given whole are still asked.
	for _, docs := range [][]rules.Doc{nil, cwDocs("Disk", "other:checkout-c"), cwDocs(func() []string {
		var ns []string
		for i := range 25 {
			ns = append(ns, fmt.Sprintf("checkout-%02d", i))
		}
		return ns
	}()...)} {
		var form url.Values
		standIns(t, map[string]http.Handler{"iam.amazonaws.com": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			form, _ = url.ParseQuery(string(b))
			io.WriteString(w, iamAnswer(form, allowed, false))
		})})
		c := writesConfig(t)
		c.Serve.Alarms = c.Serve.Alarms[:1]
		c.Serve.ChangesIn = []string{"cw:QueueDepth", "cw:checkout-*"}
		p := guarded(t, c)
		p.read("alarms:cw", docs)
		it := judged(p, p.awsWrites(context.Background(), writer{id: "changes.aws", flag: "--changes-aws-role", who: c.Serve.ChangesAWSRole, iam: signed}))
		if it.State != Unknown || !strings.Contains(it.Why, "not asked") || form.Get("ResourceArns.member.1") != queue || strings.Contains(form.Encode(), "%2A") {
			t.Errorf("%d read: %+v %v", len(docs), it, form)
		}
	}
	// Amazon's namespaces, the ruler's, each by its ARN in the workspace:
	// one given whole, and those read under a prefix.
	var form url.Values
	standIns(t, map[string]http.Handler{"iam.amazonaws.com": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(b))
		io.WriteString(w, iamAnswer(form, allowed, false))
	})})
	c := writesConfig(t)
	c.Serve.Alarms, c.Serve.Ruler, c.Serve.ChangesIn = nil, "https://aps.eu-west-1.amazonaws.com/workspaces/ws-example", []string{"shop", "team-*"}
	p := guarded(t, c)
	p.read("ruler", []rules.Doc{{Namespace: "team-a"}, {Namespace: "other"}, {Namespace: "team-b"}})
	const ns = "arn:aws:aps:eu-west-1:123456789012:rulegroupsnamespace/ws-example/"
	if it := p.awsWrites(context.Background(), writer{id: "changes.aws", flag: "--changes-aws-role", who: c.Serve.ChangesAWSRole, iam: signed}); it.State != OK ||
		form.Get("ActionNames.member.1") != "aps:PutRuleGroupsNamespace" || form.Get("ResourceArns.member.1") != ns+"shop" ||
		form.Get("ResourceArns.member.2") != ns+"team-a" || form.Get("ResourceArns.member.3") != ns+"team-b" || form.Get("ResourceArns.member.4") != "" {
		t.Errorf("%+v %v", it, form)
	}
}

// arm is a stand-in for Resource Manager's lists, by the scope's path:
// the caller's permissions, the deny assignments at or above it, its
// locks, each as Resource Manager gives them; an empty list where none
// is given, and a 404 for permissions not given. It fails the test on
// anything but those GETs, each with its api-version (and atScope() for
// the deny assignments).
type arm struct {
	t                    *testing.T
	perms, denies, locks map[string]string
	asked                []string
}

func (a *arm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.asked = append(a.asked, r.Method+" "+r.URL.RequestURI())
	q := r.URL.Query()
	if q.Get("$skiptoken") != "" {
		io.WriteString(w, `{"value":[]}`)
		return
	}
	lists := []struct {
		suffix, api, filter string
		by                  map[string]string
	}{{azurePermissionsPath, "2022-04-01", "", a.perms}, {azureDenyPath, "2022-04-01", "atScope()", a.denies}, {azureLocksPath, "2016-09-01", "", a.locks}}
	for _, l := range lists {
		scope, ok := strings.CutSuffix(r.URL.Path, l.suffix)
		if !ok {
			continue
		}
		if r.Method != "GET" || q.Get("api-version") != l.api || q.Get("$filter") != l.filter || r.Header.Get("Authorization") == "" {
			a.t.Errorf("Azure was sent %s %s", r.Method, r.URL)
		}
		b, ok := l.by[scope]
		switch {
		case !ok && l.suffix == azurePermissionsPath:
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"code":"ResourceGroupNotFound","message":"Resource group 'shop-rg' could not be found in 00000000-0000-0000-0000-00000000000a."}}`)
		case !ok:
			io.WriteString(w, `{"value":[]}`)
		default:
			io.WriteString(w, b)
		}
		return
	}
	a.t.Errorf("Azure was sent %s %s, which it doesn't serve", r.Method, r.URL)
	w.WriteHeader(404)
}

func newARM(t *testing.T, perms map[string]string) *arm {
	return &arm{t: t, perms: perms, denies: map[string]string{}, locks: map[string]string{}}
}

const (
	azSub = "/subscriptions/00000000-0000-0000-0000-00000000000a"
	azRG  = azSub + "/resourceGroups/shop-rg"
)

// azDoc is an Azure alert rule as --alarms read it, naming what it names.
func azDoc(typ, name string, names ...string) rules.Doc {
	id := strings.ToLower(azSub) + "/resourceGroups/shop-rg/providers/Microsoft.Insights/" + typ + "/" + name
	b, _ := json.Marshal(map[string]any{"id": id, "name": name, "resourceGroup": "shop-rg", "properties": map[string]any{"scopes": names}})
	return rules.Doc{Source: id, Data: b}
}

// checkoutA is a metric alert under checkout-, as --alarms read it.
var checkoutA = []rules.Doc{azDoc("metricAlerts", "checkout-a")}

// azureRun is changes.azure for --alarms az and entries, against a.
func azureRun(t *testing.T, a *arm, docs []rules.Doc, entries ...string) (*run, Item) {
	standIns(t, map[string]http.Handler{"management.azure.com": a})
	c := writesConfig(t)
	c.Serve.Alarms = c.Serve.Alarms[1:2]
	c.Serve.ChangesIn = entries
	p := guarded(t, c)
	p.read("alarms:az", docs)
	return p, judged(p, p.azureWrites(context.Background(), writer{id: "changes.azure", flag: "--changes-azure-client-id", who: c.Serve.ChangesAzureClientID, sign: signed}))
}

// Azure's permissions of the identity on each scope, judged against the
// actions serve's writes need: actions less the same entry's notActions,
// wildcards and letter case as Azure has them. A prefix may name any type
// of alert rule, a metric alert among them, whose delete it needs; a name
// given whole and read is of its type alone. A name given whole may be
// given on its alert rule alone: not on the group is unknown then; a
// prefix needs the group, and not there is a problem.
func TestAzureWrites(t *testing.T) {
	for _, x := range []struct {
		name  string
		perms string
		state State
		says  string
	}{
		{"everything", `{"value":[{"actions":["*"],"notActions":[]}]}`, OK, "Microsoft.Insights/metricAlerts/delete"},
		{"Insights, lower case", `{"value":[{"actions":["microsoft.insights/*"],"notActions":[]}]}`, OK, "no deny assignment"},
		{"no deletes", `{"value":[{"actions":["*"],"notActions":["Microsoft.Insights/*/delete"]}]}`, Problem, "not given Microsoft.Insights/metricAlerts/delete"},
		{"notActions of another entry", `{"value":[{"actions":["*"],"notActions":["Microsoft.Insights/metricAlerts/delete"]},{"actions":["Microsoft.Insights/metricAlerts/*"]}]}`, OK, ""},
		{"writes only", `{"value":[{"actions":["*/write"]}]}`, Problem, "not given Microsoft.Insights/metricAlerts/delete"},
		{"reads only", `{"value":[{"actions":["*/read"],"notActions":[]}]}`, Problem, "metricAlerts/write"},
		{"a wildcard in the middle", `{"value":[{"actions":["Microsoft.Insights/*Alerts/*","Microsoft.Insights/scheduledQueryRules/write"]}]}`, OK, ""},
	} {
		a := newARM(t, map[string]string{azRG: x.perms})
		p, it := azureRun(t, a, checkoutA, "az:shop-rg/checkout-*")
		if it.State != x.state || !strings.Contains(it.What+" "+it.Why, x.says) {
			t.Errorf("%s: %+v", x.name, it)
		}
		// The group's permissions, deny assignments and locks, the
		// subscription's locks, and the deny assignments at the rule read.
		if got := p.al.asked(); len(a.asked) != 5 || got.Refused != 0 || got.By["iam:azure"] != 5 {
			t.Errorf("%s: asked %v, %+v", x.name, a.asked, got)
		}
	}
	// A prefix with nothing read under it: a deny on a rule itself, below
	// the group, can't be asked, so it is unknown, never ok.
	if _, it := azureRun(t, newARM(t, map[string]string{azRG: `{"value":[{"actions":["*"]}]}`}), nil, "az:shop-rg/checkout-*"); it.State != Unknown ||
		!strings.Contains(it.Why, "names no alert rule this run read there, so a deny assignment on one itself, below the group, isn't asked") {
		t.Errorf("nothing read: %+v", it)
	}
	// A name given whole, read as a scheduled query rule, needs its write
	// alone, no metric alert's delete; read as a metric alert, its delete,
	// which not given on the group is unknown: it may be given on the rule.
	writesOnly := `{"value":[{"actions":["Microsoft.Insights/scheduledQueryRules/write","Microsoft.Insights/metricAlerts/write"]}]}`
	if _, it := azureRun(t, newARM(t, map[string]string{azRG: writesOnly}), []rules.Doc{azDoc("scheduledQueryRules", "LatencyHigh")}, "az:shop-rg/LatencyHigh"); it.State != OK ||
		strings.Contains(it.Why, "delete") {
		t.Errorf("a scheduled query rule: %+v", it)
	}
	if _, it := azureRun(t, newARM(t, map[string]string{azRG: writesOnly}), []rules.Doc{azDoc("metricAlerts", "LatencyHigh")}, "az:shop-rg/LatencyHigh"); it.State != Unknown ||
		!strings.Contains(it.Why, "not given there Microsoft.Insights/metricAlerts/delete, which may be given on each alert rule named") {
		t.Errorf("a metric alert: %+v", it)
	}
	// Not read, a name given whole may be of any type.
	if _, it := azureRun(t, newARM(t, map[string]string{azRG: writesOnly}), nil, "az:shop-rg/LatencyHigh"); it.State != Unknown ||
		!strings.Contains(it.Why, "activityLogAlerts/write") {
		t.Errorf("not read: %+v", it)
	}
	// A rule group of the ruler, named whole, is asked on itself; its
	// resource group for a prefix; the next page followed on the same
	// host and path.
	group := azRG + "/providers/Microsoft.AlertsManagement/prometheusRuleGroups/api"
	a := newARM(t, map[string]string{
		azRG:  `{"value":[{"actions":["*/read"]}],"nextLink":"https://management.azure.com` + azRG + `/providers/Microsoft.Authorization/permissions?api-version=2022-04-01&$skiptoken=2"}`,
		group: `{"value":[{"actions":["Microsoft.AlertsManagement/prometheusRuleGroups/*"]}]}`,
		azRG + "/providers/Microsoft.AlertsManagement/prometheusRuleGroups/edge rules": `{"value":[{"actions":["*"]}]}`,
	})
	standIns(t, map[string]http.Handler{"management.azure.com": a})
	c := writesConfig(t)
	c.Serve.Alarms = c.Serve.Alarms[1:2]
	c.Serve.Ruler = "https://management.azure.com" + azRG
	c.Serve.ChangesIn = []string{"az:shop-rg/LatencyHigh", "api", "edge rules"}
	p := guarded(t, c)
	it := judged(p, p.azureWrites(context.Background(), writer{id: "changes.azure", flag: "--changes-azure-client-id", who: c.Serve.ChangesAzureClientID, sign: signed}))
	if it.State != Unknown || !strings.Contains(it.Why, "may be given on each alert rule") || !strings.Contains(it.Why, "--ruler's rule groups on a rule group --changes-in names: Microsoft.AlertsManagement/prometheusRuleGroups/write") {
		t.Errorf("%+v", it)
	}
	if !slices.ContainsFunc(a.asked, func(x string) bool { return strings.Contains(x, "prometheusRuleGroups/api/providers") }) ||
		!slices.ContainsFunc(a.asked, func(x string) bool { return strings.Contains(x, "skiptoken=2") }) ||
		!slices.ContainsFunc(a.asked, func(x string) bool { return strings.Contains(x, "prometheusRuleGroups/edge%20rules/providers") }) ||
		p.al.asked().Refused != 0 || p.al.asked().By["iam:azure"] != len(a.asked) {
		t.Errorf("asked %v, %+v", a.asked, p.al.asked())
	}
	// A next page elsewhere isn't followed, nor asked.
	a = newARM(t, map[string]string{azRG: `{"value":[{"actions":["*"]}],"nextLink":"https://elsewhere.example/permissions?api-version=2022-04-01"}`})
	if _, it := azureRun(t, a, nil, "az:shop-rg/checkout-*"); it.State != Unknown || !strings.Contains(it.Why, "isn't followed") || len(a.asked) != 2 {
		t.Errorf("%+v %v", it, a.asked)
	}
}

// What is asked of Azure is allowed as it is asked, exactly: another
// resource group, another query, another api-version is refused.
func TestAzureAllowedAsAsked(t *testing.T) {
	standIns(t, map[string]http.Handler{"management.azure.com": newARM(t, map[string]string{azRG: `{"value":[{"actions":["*"]}]}`})})
	c := writesConfig(t)
	c.Serve.Alarms, c.Serve.ChangesIn = c.Serve.Alarms[1:2], []string{"az:shop-rg/checkout-*"}
	p := newRun(c)
	p.read("alarms:az", checkoutA)
	t.Cleanup(egress.Guard(p.al.check))
	if it := p.azureWrites(context.Background(), writer{id: "changes.azure", flag: "--changes-azure-client-id", sign: signed}); it.State != OK {
		t.Fatalf("%+v", it)
	}
	base := "https://management.azure.com" + azRG
	for _, x := range []struct {
		method, url string
		ok          bool
	}{
		{"GET", base + "/providers/Microsoft.Authorization/permissions?api-version=2022-04-01", true},
		{"GET", base + "/providers/Microsoft.Authorization/denyAssignments?%24filter=atScope%28%29&api-version=2022-04-01", true},
		{"GET", base + "/providers/Microsoft.Authorization/locks?api-version=2016-09-01", true},
		{"GET", "https://management.azure.com" + azSub + "/providers/Microsoft.Authorization/locks?api-version=2016-09-01", true},
		{"GET", "https://management.azure.com" + checkoutA[0].Source + "/providers/Microsoft.Authorization/denyAssignments?%24filter=atScope%28%29&api-version=2022-04-01", true},
		{"GET", "https://management.azure.com" + checkoutA[0].Source + "/providers/Microsoft.Authorization/permissions?api-version=2022-04-01", false}, // not asked
		{"GET", strings.Replace(base, "shop-rg", "other-rg", 1) + "/providers/Microsoft.Authorization/permissions?api-version=2022-04-01", false},
		{"GET", base + "/providers/Microsoft.Authorization/permissions?api-version=2022-04-01&$filter=x", false},
		{"GET", base + "/providers/Microsoft.Authorization/permissions?api-version=2015-07-01", false},
		{"GET", base + "/providers/Microsoft.Authorization/denyAssignments?api-version=2022-04-01", false}, // without atScope()
		{"GET", base + "/providers/Microsoft.Authorization/roleAssignments?api-version=2022-04-01", false},
		{"PUT", base + "/providers/Microsoft.Authorization/locks/x?api-version=2016-09-01", false},
		{"DELETE", base + "/providers/Microsoft.Authorization/locks/x?api-version=2016-09-01", false},
		{"PUT", base + "/providers/Microsoft.Insights/metricAlerts/checkout-a?api-version=2026-01-01", false},
		{"GET", "https://management.azure.com" + azSub + "/providers/Microsoft.Authorization/permissions?api-version=2022-04-01", false}, // not asked
	} {
		if err := p.al.check(req(t, x.method, x.url, "")); (err == nil) != x.ok {
			t.Errorf("%s %s: %v, want allowed %v", x.method, x.url, err, x.ok)
		}
	}
}

// Locks and deny assignments: a ReadOnly lock on the group, the
// subscription or a rule written is a problem; a CanNotDelete one, where a
// metric alert may be deleted; one on another resource, nothing. A deny
// assignment that may refuse a write is unknown, naming it; one with the
// write in its notActions, or that doesn't apply below its scope, nothing.
func TestAzureLocksAndDenies(t *testing.T) {
	all := `{"value":[{"actions":["*"]}]}`
	lock := func(on, name, level string) string {
		return `{"value":[{"id":"` + on + `/providers/Microsoft.Authorization/locks/` + name + `","name":"` + name + `","properties":{"level":"` + level + `"}}]}`
	}
	deny := func(name, scope string, notChildren bool, actions, not string) string {
		return fmt.Sprintf(`{"value":[{"properties":{"denyAssignmentName":"%s","scope":"%s","doNotApplyToChildScopes":%v,"permissions":[{"actions":[%s],"notActions":[%s]}]}}]}`,
			name, scope, notChildren, actions, not)
	}
	sqr := []rules.Doc{azDoc("scheduledQueryRules", "LatencyHigh")}
	for _, x := range []struct {
		name          string
		locks, denies map[string]string
		docs          []rules.Doc
		entry         string
		state         State
		says          string
	}{
		{"a ReadOnly lock on the group", map[string]string{azRG: lock(azRG, "freeze", "ReadOnly")}, nil, nil, "az:shop-rg/checkout-*", Problem, "a ReadOnly lock, freeze, refuses every write"},
		{"a ReadOnly lock on the subscription", map[string]string{azSub: lock(azSub, "freeze", "ReadOnly")}, nil, nil, "az:shop-rg/checkout-*", Problem, "in the subscription's locks: a ReadOnly lock"},
		{"a lock on the group in the subscription's list alone", map[string]string{azSub: lock(azRG, "freeze", "ReadOnly")}, nil, nil, "az:shop-rg/checkout-*", Problem, "a ReadOnly lock, freeze"},
		{"a lock on another group, in the subscription's list", map[string]string{azSub: lock(azSub+"/resourceGroups/other-rg", "freeze", "ReadOnly")}, nil, nil, "az:shop-rg/checkout-*", OK, ""},
		{"a CanNotDelete lock, deletes", map[string]string{azRG: lock(azRG, "keep", "CanNotDelete")}, nil, nil, "az:shop-rg/checkout-*", Problem, "a CanNotDelete lock, keep, refuses deleting"},
		{"a CanNotDelete lock, no deletes", map[string]string{azRG: lock(azRG, "keep", "CanNotDelete")}, nil, sqr, "az:shop-rg/LatencyHigh", OK, ""},
		{"a ReadOnly lock on a rule written", map[string]string{azRG: lock(azRG+"/providers/Microsoft.Insights/metricAlerts/checkout-a", "freeze", "ReadOnly")}, nil, nil, "az:shop-rg/checkout-*", Problem, "a ReadOnly lock"},
		{"a ReadOnly lock on another rule", map[string]string{azRG: lock(azRG+"/providers/Microsoft.Insights/metricAlerts/payments", "freeze", "ReadOnly")}, nil, nil, "az:shop-rg/checkout-*", OK, ""},
		{"a deny assignment", nil, map[string]string{azRG: deny("No alert changes", azSub, false, `"Microsoft.Insights/*/write"`, "")}, nil, "az:shop-rg/checkout-*", Unknown, "a deny assignment, No alert changes, may refuse"},
		{"a deny with the writes left out", nil, map[string]string{azRG: deny("No deletes but", azSub, false, `"*"`, `"Microsoft.Insights/*"`)}, nil, "az:shop-rg/checkout-*", OK, ""},
		{"a deny that doesn't apply below", nil, map[string]string{azRG: deny("Here only", azSub, true, `"*"`, "")}, nil, "az:shop-rg/checkout-*", OK, ""},
		{"a deny of another action", nil, map[string]string{azRG: deny("No VMs", azSub, false, `"Microsoft.Compute/*"`, "")}, nil, "az:shop-rg/checkout-*", OK, ""},
	} {
		a := newARM(t, map[string]string{azRG: all})
		for k, v := range x.locks {
			a.locks[k] = v
		}
		for k, v := range x.denies {
			a.denies[k] = v
		}
		docs := x.docs
		if docs == nil {
			docs = checkoutA
		}
		_, it := azureRun(t, a, docs, x.entry)
		if it.State != x.state || !strings.Contains(it.Why, x.says) {
			t.Errorf("%s: %+v", x.name, it)
		}
	}
	// A deny assignment on the rule itself, below the group, as a
	// deployment stack puts one on what it manages: asked at the rule.
	a := newARM(t, map[string]string{azRG: all})
	a.denies[checkoutA[0].Source] = deny("Stack deny", checkoutA[0].Source, false, `"*/write"`, "")
	if _, it := azureRun(t, a, checkoutA, "az:shop-rg/checkout-*"); it.State != Unknown ||
		!strings.Contains(it.Why, "on an alert rule --changes-in names: a deny assignment, Stack deny, may refuse Microsoft.Insights/metricAlerts/write") {
		t.Errorf("a deny on the rule: %+v", it)
	}
	// Past ten rules read, the rest's own deny assignments aren't asked.
	var many []rules.Doc
	for i := range 12 {
		many = append(many, azDoc("metricAlerts", fmt.Sprintf("checkout-%02d", i)))
	}
	if _, it := azureRun(t, newARM(t, map[string]string{azRG: all}), many, "az:shop-rg/checkout-*"); it.State != Unknown ||
		!strings.Contains(it.Why, "2 more alert rules named, past the first 10, whose own deny assignments aren't asked") {
		t.Errorf("many rules: %+v", it)
	}
	// A lock both lists give is judged once.
	a = newARM(t, map[string]string{azRG: all})
	a.locks[azRG], a.locks[azSub] = lock(azRG, "freeze", "ReadOnly"), lock(azRG, "freeze", "ReadOnly")
	if _, it := azureRun(t, a, checkoutA, "az:shop-rg/checkout-*"); it.State != Problem || strings.Count(it.Why, "freeze") != 1 {
		t.Errorf("once: %+v", it)
	}
}

// The resources an alert rule names, its targets and its action groups,
// read in this run: Azure Monitor asks for read on them as a metric alert
// is written. Each is asked at itself: not given there is a problem for a
// metric alert's, unknown for another kind's; given, ok.
func TestAzureLinkedReads(t *testing.T) {
	vm := azRG + "/providers/Microsoft.Compute/virtualMachines/web-1"
	group := azSub + "/resourceGroups/ops-rg/providers/Microsoft.Insights/actionGroups/oncall"
	for _, x := range []struct {
		name, typ string
		perms     map[string]string
		state     State
		says      string
	}{
		{"both read", "metricAlerts", map[string]string{vm: `{"value":[{"actions":["*/read"]}]}`, group: `{"value":[{"actions":["Microsoft.Insights/actionGroups/read"]}]}`}, OK,
			"Microsoft.Compute/virtualMachines/read"},
		{"the action group not read", "metricAlerts", map[string]string{vm: `{"value":[{"actions":["*/read"]}]}`, group: `{"value":[{"actions":["Microsoft.Compute/*"]}]}`}, Problem,
			"not given Microsoft.Insights/actionGroups/read, which Azure Monitor asks for"},
		{"another kind's not read", "scheduledQueryRules", map[string]string{vm: `{"value":[]}`, group: `{"value":[]}`}, Unknown, "of this kind says nothing"},
		{"not answered", "metricAlerts", map[string]string{vm: `{"value":[{"actions":["*/read"]}]}`}, Unknown, "Azure didn't say its permissions there"},
	} {
		perms := map[string]string{azRG: `{"value":[{"actions":["*"]}]}`}
		for k, v := range x.perms {
			perms[k] = v
		}
		a := newARM(t, perms)
		_, it := azureRun(t, a, []rules.Doc{azDoc(x.typ, "checkout-a", vm, group)}, "az:shop-rg/checkout-*")
		if it.State != x.state || !strings.Contains(it.Why, x.says) {
			t.Errorf("%s: %+v", x.name, it)
		}
		if !slices.ContainsFunc(a.asked, func(s string) bool {
			return strings.Contains(s, "virtualMachines/web-1/providers/Microsoft.Authorization/permissions")
		}) {
			t.Errorf("%s: asked %v", x.name, a.asked)
		}
	}
	// Each resource, and each rule, once: two rules naming the same
	// target, a rule named by two entries.
	a := newARM(t, map[string]string{azRG: `{"value":[{"actions":["*"]}]}`, vm: `{"value":[{"actions":["*/read"]}]}`})
	if _, it := azureRun(t, a, []rules.Doc{azDoc("metricAlerts", "checkout-a", vm, vm), azDoc("metricAlerts", "checkout-b", vm)},
		"az:shop-rg/checkout-*", "az:shop-rg/checkout-a"); it.State != OK {
		t.Errorf("twice: %+v", it)
	}
	count := func(sub string) int {
		n := 0
		for _, x := range a.asked {
			if strings.Contains(x, sub) {
				n++
			}
		}
		return n
	}
	if count("web-1/providers/Microsoft.Authorization/permissions") != 1 || count("metricAlerts/checkout-a/providers/Microsoft.Authorization/denyAssignments") != 1 {
		t.Errorf("asked %v", a.asked)
	}
	// A target that isn't a resource under a resource group (the
	// subscription itself), or an id past 1 KB, isn't asked, and is
	// unknown, never passed over.
	long := azRG + "/providers/Microsoft.Compute/virtualMachines/" + strings.Repeat("v", 1100)
	for _, target := range []string{azSub, "/providers/Microsoft.Management/managementGroups/example", long} {
		a := newARM(t, map[string]string{azRG: `{"value":[{"actions":["*"]}]}`})
		_, it := azureRun(t, a, []rules.Doc{azDoc("metricAlerts", "checkout-a", target)}, "az:shop-rg/checkout-*")
		if it.State != Unknown || !strings.Contains(it.Why, "1 resource the alert rules name that isn't one under a resource group") ||
			slices.ContainsFunc(a.asked, func(x string) bool {
				return strings.Contains(x, "vvvv") || strings.HasPrefix(x, "GET "+azSub+"/providers/Microsoft.Authorization/permissions")
			}) {
			t.Errorf("%.80s: %+v %v", target, it, a.asked)
		}
	}
	if got := readAction("/subscriptions/x/resourceGroups/y/providers/Microsoft.Monitor/accounts/z"); got != "Microsoft.Monitor/accounts/read" {
		t.Error(got)
	}
	if got := readAction("/subscriptions/x/resourceGroups/y"); got != "Microsoft.Resources/subscriptions/resourceGroups/read" {
		t.Error(got)
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
		{403, `{"error":{"code":403,"message":"Cloud Resource Manager API has not been used in project 987654321098 before or it is disabled.","status":"PERMISSION_DENIED"}}`, Unknown,
			"has not been used in project [a number] before"},
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
		if it.State != x.state || !strings.Contains(it.Why, x.says) || strings.Contains(it.Why, "987654321098") {
			t.Errorf("%d %s: %+v", x.code, x.holds, it)
		}
		if x.state == OK && !strings.Contains(it.What, "IAM deny policies, principal access boundaries or VPC Service Controls isn't documented") {
			t.Errorf("no caveat: %+v", it)
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
	// Only the projects --changes-in names a policy of are asked.
	var asked []string
	standIns(t, map[string]http.Handler{"cloudresourcemanager.googleapis.com": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		io.WriteString(w, `{"permissions":["monitoring.alertPolicies.create","monitoring.alertPolicies.delete","monitoring.alertPolicies.update"]}`)
	})})
	c := writesConfig(t)
	c.Serve.Alarms = append(c.Serve.Alarms[2:3], serve.AlarmsAt{Source: "gcp2", URL: "https://monitoring.googleapis.com/v3/projects/other-project/alertPolicies"})
	p := guarded(t, c)
	if it := p.googleWrites(context.Background(), writer{id: "changes.google", flag: "--changes-google-service-account", who: c.Serve.ChangesGoogleServiceAccount}); it.State != OK ||
		len(asked) != 1 || asked[0] != "/v1/projects/example-project:testIamPermissions" {
		t.Errorf("%+v %v", it, asked)
	}
}

// Without --changes-google-service-account, the pod's own identity
// writes Google's alerting policies: changes.google asks of it, its own
// token (read-only here), as serve writes with its token of the write
// scope (cloud.Writes); and isn't listed where --changes-in names none.
func TestGoogleWritesAsThePod(t *testing.T) {
	var scopes, took []string
	was, wasT := googleAs, take
	googleAs = func(_ context.Context, as, scope string) (func(*http.Request) error, error) {
		scopes = append(scopes, as+"|"+scope)
		return signed, nil
	}
	take = func(_ context.Context, kind, endpoint, as string) (func(*http.Request) error, func(*http.Request) error, error) {
		took = append(took, kind+"|"+as)
		return signed, nil, nil
	}
	t.Cleanup(func() { googleAs, take = was, wasT })
	standIns(t, map[string]http.Handler{"cloudresourcemanager.googleapis.com": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"permissions":["monitoring.alertPolicies.update"]}`)
	})})
	c := writesConfig(t)
	c.Serve.Alarms, c.Serve.ChangesGoogleServiceAccount = c.Serve.Alarms[2:3], ""
	c.Serve.ChangesAWSRole, c.Serve.ChangesAzureClientID, c.Serve.ChangesOCIConfig = "", "", ""
	p := guarded(t, c)
	p.changes(context.Background())
	it := itemOf(p, "changes.google")
	if it == nil || it.State != Problem || !strings.Contains(it.What, "the pod's own Google identity") || len(scopes) != 1 ||
		scopes[0] != "|https://www.googleapis.com/auth/cloud-platform.read-only" || len(took) != 1 || took[0] != "google|" {
		t.Errorf("%+v %v %v", it, scopes, took)
	}
	c.Serve.ChangesIn = []string{"cw:QueueDepth"}
	p = guarded(t, c)
	p.changes(context.Background())
	if it := itemOf(p, "changes.google"); it != nil {
		t.Errorf("%+v", it)
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
		{"shop's, its own name", [][]string{both(""), nil, {"Allow group AlarmWriters to manage alarms in compartment shop"}}, false, true, 0},
		{"Apps's, its own name", [][]string{both(""), {"Allow group AlarmWriters to manage alarms in compartment Apps"}}, false, true, 0},
		{"Apps's, a child of its own name", [][]string{both(""), {"Allow group AlarmWriters to manage alarms in compartment shop:Apps"}}, false, false, 0},
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
// the compartments (shop under Apps under the tenancy; metrics under the
// tenancy), the policies of each, by its OCID (the tenancy's on two
// pages, the second as TENANCY#2), each as the API gives it, ACTIVE
// unless inactive names it (a membership, a group, a policy); noted as
// asked.
func ociIdentity(t *testing.T, policies map[string][]string, asked *[]string, refuse string, inactive ...string) http.Handler {
	state := func(what string) string {
		if slices.Contains(inactive, what) {
			return "INACTIVE"
		}
		return "ACTIVE"
	}
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
		compartments := map[string]string{
			"ocid1.compartment.oc1..exampleshop":    `{"id":"ocid1.compartment.oc1..exampleshop","name":"shop","compartmentId":"ocid1.compartment.oc1..exampleapps","lifecycleState":"ACTIVE"}`,
			"ocid1.compartment.oc1..exampleapps":    `{"id":"ocid1.compartment.oc1..exampleapps","name":"Apps","compartmentId":"ocid1.tenancy.oc1..example","lifecycleState":"ACTIVE"}`,
			"ocid1.compartment.oc1..examplemetrics": `{"id":"ocid1.compartment.oc1..examplemetrics","name":"metrics","compartmentId":"ocid1.tenancy.oc1..example","lifecycleState":"ACTIVE"}`,
		}
		switch p := r.URL.Path; {
		case p == "/20160918/userGroupMemberships" && q.Get("compartmentId") == "ocid1.tenancy.oc1..example" && q.Get("userId") == "ocid1.user.oc1..examplewriter":
			io.WriteString(w, `[{"groupId":"ocid1.group.oc1..examplewriters","userId":"ocid1.user.oc1..examplewriter","lifecycleState":"`+state("membership")+`"}]`)
		case p == "/20160918/groups/ocid1.group.oc1..examplewriters":
			io.WriteString(w, `{"id":"ocid1.group.oc1..examplewriters","name":"AlarmWriters","lifecycleState":"`+state("group")+`"}`)
		case strings.HasPrefix(p, "/20160918/compartments/") && compartments[strings.TrimPrefix(p, "/20160918/compartments/")] != "":
			io.WriteString(w, compartments[strings.TrimPrefix(p, "/20160918/compartments/")])
		case p == "/20160918/policies":
			c := q.Get("compartmentId")
			if q.Get("page") == "" && c == "ocid1.tenancy.oc1..example" {
				w.Header().Set("opc-next-page", "page-2")
			}
			if q.Get("page") == "page-2" {
				c += "#2"
			}
			b, _ := json.Marshal([]map[string]any{{"name": "p", "statements": policies[c], "lifecycleState": state("policy")}})
			w.Write(b)
		default:
			t.Errorf("Identity was sent %s, which it doesn't serve", r.URL)
			w.WriteHeader(404)
		}
	})
}

// ociDoc is an OCI alarm as --alarms read it: in shop, its metric in
// metrics, or read in every compartment.
func ociDoc(name, compartment, metric string, subtree bool) rules.Doc {
	b, _ := json.Marshal(map[string]any{"data": []any{map[string]any{"displayName": name, "compartmentId": compartment, "metricCompartmentId": metric,
		"metricCompartmentIdInSubtree": subtree}}})
	return rules.Doc{Source: "ocid1.alarm.oc1..example" + strings.ToLower(name), Data: b}
}

const (
	ociShop    = "ocid1.compartment.oc1..exampleshop"
	ociMetrics = "ocid1.compartment.oc1..examplemetrics"
	ociTenancy = "ocid1.tenancy.oc1..example"
)

// OCI's: the user's ACTIVE groups; for each alarm --changes-in names, as
// read, the compartments from its own up to the tenancy, and its metric's,
// every page of the ACTIVE policies of each, read once, as the user, and
// judged: manage alarms where the alarm is, read metrics where its metric
// is. A condition is unknown, "a policy with a condition"; no grant, a
// problem; the policies unreadable, unknown, saying what would let them be
// read.
func TestOCIWrites(t *testing.T) {
	was := ociConfigUser
	ociConfigUser = func(string) (cloud.OCIUser, error) {
		return cloud.OCIUser{Tenancy: ociTenancy, User: "ocid1.user.oc1..examplewriter", Region: "eu-frankfurt-1",
			Identity: "https://identity.eu-frankfurt-1.oci.oraclecloud.com"}, nil
	}
	t.Cleanup(func() { ociConfigUser = was })
	manage := map[string][]string{"ocid1.compartment.oc1..exampleapps": {"Allow group AlarmWriters to manage alarms in compartment shop"},
		ociTenancy + "#2": {"Allow group AlarmWriters to read metrics in compartment metrics"}}
	alarm := []rules.Doc{ociDoc("Checkout", ociShop, ociMetrics, false), ociDoc("Payments", ociMetrics, ociMetrics, false)}
	for _, x := range []struct {
		name     string
		policies map[string][]string
		docs     []rules.Doc
		inactive []string
		refuse   string
		state    State
		says     string
		asks     int
	}{
		{"granted", manage, alarm, nil, "", OK, "where an alarm --changes-in names is: manage alarms; where its metric is: read metrics", 10},
		{"read metrics where the alarm is, not its metric", map[string][]string{"ocid1.compartment.oc1..exampleapps": {
			"Allow group AlarmWriters to manage alarms in compartment shop", "Allow group AlarmWriters to read metrics in compartment shop"}}, alarm, nil, "",
			Problem, "where its metric is: no policy gives the user read metrics", 10},
		{"a condition", map[string][]string{ociShop: {
			"Allow group AlarmWriters to manage alarms in compartment shop where any {request.permission = 'ALARM_INSPECT', request.permission = 'ALARM_UPDATE'}"},
			ociTenancy: {"Allow group AlarmWriters to read metrics in tenancy"}}, alarm, nil, "", Unknown, "manage alarms given only by a policy with a condition", 10},
		{"none", map[string][]string{ociShop: {"Allow group AlarmWriters to read alarms in compartment shop"}}, alarm, nil, "", Problem, "no policy gives the user manage alarms", 10},
		{"an inactive membership", manage, alarm, []string{"membership"}, "", Problem, "no policy gives the user manage alarms", 9},
		{"an inactive group", manage, alarm, []string{"group"}, "", Problem, "no policy gives the user manage alarms", 10},
		{"inactive policies", manage, alarm, []string{"policy"}, "", Problem, "no policy gives the user manage alarms", 10},
		{"a metric read in every compartment", manage, []rules.Doc{ociDoc("Checkout", ociShop, ociMetrics, true)}, nil, "", Unknown,
			"in every compartment of the tenancy, where its metric is read: read metrics given only by a group not read, a permission list, a compartment named by its own name or a location only partly covering", 8},
		{"a metric read in every compartment, none", map[string][]string{"ocid1.compartment.oc1..exampleapps": {"Allow group AlarmWriters to manage alarms in compartment shop"}},
			[]rules.Doc{ociDoc("Checkout", ociShop, ociMetrics, true)}, nil, "", Unknown,
			"no policy of the tenancy's gives the user read metrics in the tenancy, and those in its compartments aren't read", 8},
		{"a metric read in every compartment, given in the tenancy", map[string][]string{"ocid1.compartment.oc1..exampleapps": {"Allow group AlarmWriters to manage alarms in compartment shop"},
			ociTenancy: {"Allow group AlarmWriters to read metrics in tenancy"}}, []rules.Doc{ociDoc("Checkout", ociShop, ociMetrics, true)}, nil, "", OK, "", 8},
		{"nothing read named", manage, []rules.Doc{ociDoc("Disk", ociShop, ociMetrics, false)}, nil, "", Unknown, "no alarm read that an entry of --changes-in names", 0},
		{"unreadable", nil, alarm, nil, "/20160918/policies", Unknown, "inspect groups, users, compartments and policies in the tenancy", -1},
	} {
		var asked []string
		h := standIns(t, map[string]http.Handler{"identity.eu-frankfurt-1.oci.oraclecloud.com": ociIdentity(t, x.policies, &asked, x.refuse, x.inactive...)})
		c := writesConfig(t)
		c.Serve.Alarms = c.Serve.Alarms[3:]
		c.Serve.ChangesIn = []string{"oci:Checkout"}
		p := guarded(t, c)
		p.read("alarms:oci", x.docs)
		it := judged(p, p.ociWrites(context.Background(), writer{id: "changes.oci", flag: "--changes-oci-config", who: c.Serve.ChangesOCIConfig, sign: signed}))
		if it.State != x.state || !strings.Contains(it.Why+" "+it.Fix, x.says) {
			t.Errorf("%s: %+v", x.name, it)
		}
		if a := p.al.asked(); a.Refused != 0 || a.By["iam:oci"] != len(h.all()) {
			t.Errorf("%s: %+v, sent %v", x.name, a, h.all())
		}
		if x.asks >= 0 && len(asked) != x.asks {
			t.Errorf("%s: asked %d: %v", x.name, len(asked), asked)
		}
		// Payments, in metrics, isn't named: its compartment isn't asked
		// for itself, only as Checkout's metric's.
		if x.name == "granted" && !slices.ContainsFunc(asked, func(a string) bool { return strings.Contains(a, "compartments/"+ociMetrics) }) {
			t.Errorf("the metric's compartment not read: %v", asked)
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
	// service with a client of its own: one is given, with a test's key,
	// and its alarms read from a stand-in, one --changes-in names.
	key, pemText := alarmtest.OCIKey()
	pemFile := fileOf(t, "key.pem", pemText)
	t.Setenv("OCI_CLI_CONFIG_FILE", fileOf(t, "config", "[DEFAULT]\nuser=ocid1.user.oc1..examplereader\nfingerprint=aa:bb\n"+
		"tenancy=ocid1.tenancy.oc1..example\nregion=eu-frankfurt-1\nkey_file="+pemFile+"\n"))
	telemetry := alarmtest.NewOCI(ociShop, &key.PublicKey, "ocid1.tenancy.oc1..example/ocid1.user.oc1..examplereader/aa:bb")
	telemetry.Add(map[string]any{"displayName": "Checkout", "metricCompartmentId": ociMetrics, "namespace": "oci_computeagent",
		"query": "CpuUtilization[1m].mean() > 80", "severity": "CRITICAL", "isEnabled": true, "destinations": []any{}, "pendingDuration": "PT5M"})
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
			io.WriteString(w, `{"error":{"code":"AuthorizationFailed","message":"The client '00000000-0000-0000-0000-00000000000c' with object id `+
				`'00000000-0000-0000-0000-00000000000d' may not read over '/resourceGroups/shop-rg'."}}`)
		}),
		"cloudresourcemanager.googleapis.com": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(403)
			io.WriteString(w, `{"error":{"code":403,"message":"The caller example-writer@example-project.iam.gserviceaccount.com may not, project example-project, number 987654321098","status":"PERMISSION_DENIED"}}`)
		}),
		"telemetry.eu-frankfurt-1.oraclecloud.com":    telemetry,
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
	if b := rep.Asked.By; rep.Asked.Refused != 0 || b["grafana"] != 2 || b["iam:aws"] != 1 || b["iam:google"] != 1 || b["iam:azure"] != 2 ||
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
		"[--changes-aws-role's account]", "[--changes-aws-role's name]", "[--alarms gcp's project]", "[--alarms oci's compartment]", "[an id]", "[a number]"} {
		if !strings.Contains(all.String(), as) {
			t.Errorf("the report doesn't say %s:\n%s", as, all.String())
		}
	}
	for _, hidden := range []string{"123456789012", "example-writer", "example-project", "00000000-0000-0000-0000-00000000000a", "shop-rg",
		"00000000-0000-0000-0000-00000000000d", "987654321098",
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

// The credential taken is the one serve writes with: for Google without a
// service account, the pod's own of the write scope (cloud.Writes), not
// the reads'.
func TestTakeWrites(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "none")) // nothing of this machine's
	was := writeCredential
	t.Cleanup(func() { writeCredential = was })
	var asked []string
	writeCredential = func(_ context.Context, kind, endpoint, as string) (func(*http.Request) error, error) {
		asked = append(asked, kind+"|"+as)
		return signed, nil
	}
	if _, _, err := take(context.Background(), "google", "https://monitoring.googleapis.com/v3/projects/example-project/alertPolicies", ""); err != nil ||
		len(asked) != 1 || asked[0] != "google|" {
		t.Errorf("%v %v", err, asked)
	}
}

// An identity that can't be taken is said by what the cloud answered,
// redacted as a cloud's text is: the pod's own role, of another account,
// an account written with dashes or glued to a word, none printed; the
// role serve writes as, by its flag.
func TestTakeErrorRedacted(t *testing.T) {
	was := take
	t.Cleanup(func() { take = was })
	take = func(context.Context, string, string, string) (func(*http.Request) error, func(*http.Request) error, error) {
		return nil, nil, errors.New("operation error STS: AssumeRole, api error AccessDenied: User: arn:aws:sts::210987654321:assumed-role/pod-role/i-0abc " +
			"is not authorized to perform: sts:AssumeRole on resource: arn:aws:iam::123456789012:role/example-writer (account 2109-8765-4321, acct_210987654321)")
	}
	c := writesConfig(t)
	c.Serve.Alarms, c.Serve.ChangesIn = c.Serve.Alarms[:1], []string{"cw:QueueDepth"}
	c.Serve.ChangesAzureClientID, c.Serve.ChangesGoogleServiceAccount, c.Serve.ChangesOCIConfig = "", "", ""
	p := guarded(t, c)
	p.changes(context.Background())
	p.redact()
	it := itemOf(p, "changes.cloud")
	if it == nil || it.State != Problem {
		t.Fatalf("%+v", it)
	}
	for _, hidden := range []string{"210987654321", "pod-role", "2109-8765-4321", "123456789012", "example-writer"} {
		if strings.Contains(it.Why, hidden) {
			t.Errorf("the report says %q: %s", hidden, it.Why)
		}
	}
	if !strings.Contains(it.Why, "[an ARN]") || strings.Count(it.Why, "[a number]") != 2 || !strings.Contains(it.Why, "[--changes-aws-role]") {
		t.Errorf("%s", it.Why)
	}
}

// What a cloud said loses its ids; Lapilli's own words keep theirs.
func TestCloudIDs(t *testing.T) {
	for in, want := range map[string]string{
		"account 123456789012.":                       "account [a number].",
		"acct_123456789012 and x123456789012y":        "acct_[a number] and x[a number]y",
		"1234-5678-9012 or 12345-6789-0123":           "[a number] or 12345-6789-0123",
		"a time 1762000000, a count 999999999":        "a time [a number], a count 999999999",
		"arn:aws:iam::123456789012:role/x/y, then":    "[an ARN], then",
		"arn:aws:s3:::bucket stays":                   "arn:aws:s3:::bucket stays",
		"object 00000000-0000-0000-0000-00000000000d": "object [an id]",
	} {
		if got := cloudIDs(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	c := writesConfig(t)
	p := newRun(c)
	p.add(Item{ID: "changes.aws", State: Unknown, What: "past 1234567890 names", Why: p.cloudSaidText("project 1234567890")})
	p.redact()
	if it := p.r.Items[0]; it.What != "past 1234567890 names" || it.Why != "project [a number]" {
		t.Errorf("%+v", it)
	}
}
