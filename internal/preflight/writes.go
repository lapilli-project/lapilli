package preflight

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/egress"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/serve"
	"lapilli.dev/internal/source"
)

// What a token or a cloud's identity may write, asked of an API that says
// so without anything being written (docs/design/preflight.md, step 2):
// Grafana's list of the caller's own actions, Cloud Resource Manager's
// testIamPermissions, Azure's permissions of the caller on a scope, IAM's
// policy simulator, and OCI's policies, read and judged here. Every one
// of them only reads, or, the two POSTs among them, only answers: the
// allowlist holds each to its path, and the simulator to its action. An
// answer that says "allowed" is judged, and nothing more is done with it.

// askClient is how these are asked: through egress, so the allowlist
// sees each request first, following no redirect.
var askClient = &http.Client{Transport: egress.Wrap(nil),
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// answered is an answer that isn't a success: its status, and what it
// said, without what the request carried to sign in.
type answered struct {
	code int
	text string
}

func (e *answered) Error() string { return fmt.Sprintf("HTTP %d: %s", e.code, e.text) }

// codeOf is the status of an answer that wasn't a success, 0 for none.
func codeOf(err error) int {
	var a *answered
	if errors.As(err, &a) {
		return a.code
	}
	return 0
}

// askMost is the most an answer of these is read: Grafana's list of
// actions is some 20 KB for an Admin, a page of policies or permissions
// less.
const askMost = 4 << 20

// ask sends a request and reads its answer, askMost bytes at most; an
// answer that isn't a success is an error, answered.
func ask(req *http.Request) (http.Header, []byte, error) {
	resp, err := askClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, askMost+1))
	if err != nil {
		return nil, nil, err
	}
	if len(b) > askMost {
		return nil, nil, fmt.Errorf("an answer past %d bytes", askMost)
	}
	switch {
	case resp.StatusCode/100 == 3:
		return resp.Header, b, &answered{resp.StatusCode, "a redirect, which isn't followed"}
	case resp.StatusCode/100 != 2:
		return resp.Header, b, &answered{resp.StatusCode, clip(prom.ServerText(errorText(b), req), 2*saidMost)}
	}
	return resp.Header, b, nil
}

// errorText is what an API's error says, its code and message, as the
// APIs asked here give them: IAM's query API in XML (Error, Code and
// Message), Google's and Azure's JSON (error, its code or status and
// message), OCI's and Grafana's (code or messageId, and message); the
// answer as it is otherwise.
func errorText(b []byte) string {
	var x struct {
		Code    string `xml:"Error>Code"`
		Message string `xml:"Error>Message"`
	}
	if xml.Unmarshal(b, &x) == nil && x.Message != "" {
		return strings.TrimSpace(x.Code + ": " + x.Message)
	}
	var j struct {
		Error *struct {
			Code    any    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
		Code      any    `json:"code"`
		MessageID string `json:"messageId"`
		Message   string `json:"message"`
	}
	if json.Unmarshal(b, &j) == nil {
		code, msg := j.Code, j.Message
		if j.Error != nil {
			code, msg = j.Error.Code, j.Error.Message
			if j.Error.Status != "" {
				code = j.Error.Status
			}
		}
		if c, ok := code.(string); (!ok || c == "") && j.MessageID != "" {
			code = j.MessageID
		}
		if msg != "" {
			if c, ok := code.(string); ok && c != "" {
				return c + ": " + msg
			}
			return msg
		}
	}
	return strings.TrimSpace(string(b))
}

// allowAsking allows what the checks of writes ask that the flags say
// already: Grafana's list, each Google project's testIamPermissions, IAM's
// simulator for the role, each Azure scope's permissions. What is learned
// as they go (Azure's next pages, OCI's compartments, groups and
// policies) is allowed when it is learned. Each is before the rest
// (allowAll asks this first), so it is counted by its own API.
func (p *run) allowAsking() {
	s, a := p.c.Serve, p.al
	if !s.Changes {
		return
	}
	if host, path, ok := at(s.Grafana); ok && s.ChangesGrafanaToken != "" {
		a.add(allow{api: "grafana", method: http.MethodGet, host: host, path: path + grafanaPermissions})
	}
	if s.ChangesGoogleServiceAccount != "" {
		for _, x := range googleProjects(s.Alarms) {
			a.add(allow{api: "iam:google", method: http.MethodPost, host: crmHost, path: "/v1/projects/" + x.project + ":testIamPermissions",
				body: onlyPermissions})
		}
	}
	if role := s.ChangesAWSRole; role != "" {
		if host, _, ok := iamOf(role); ok {
			a.add(allow{api: "iam:aws", method: http.MethodPost, host: host, path: "/", form: simulateOnly(role)})
		}
	}
	if s.ChangesAzureClientID != "" {
		for _, sc := range p.azureScopes() {
			a.add(allow{api: "iam:azure", method: http.MethodGet, host: sc.host, path: sc.path + azurePermissionsPath,
				query: exactly("api-version", azurePermissionsAPI)})
		}
	}
}

// hideScopes has the report redact what the URLs of --alarms and --ruler
// name inside them, which a cloud's answer, or an error, may repeat apart
// from the URL: an OCI compartment's OCID, an Azure subscription and its
// resource group, an Amazon workspace, a Google project. Short names, a
// resource group's or a project's, only in what a server said.
func (p *run) hideScopes() {
	s := p.c.Serve
	for _, a := range s.Alarms {
		u, err := url.Parse(a.URL)
		if err != nil {
			continue
		}
		flag := "--alarms " + a.Source
		switch k, _ := source.AlarmsKind(a.URL); k {
		case source.AlarmsOCI:
			p.hide(flag+"'s compartment", u.Query().Get("compartmentId"))
		case source.AlarmsAzure:
			if m := azureSubRG.FindStringSubmatch(u.Path); m != nil {
				p.hide(flag+"'s subscription", m[1])
				p.hideSaid(flag+"'s resource group", m[2])
			}
		case source.AlarmsGoogle:
			if m := googlePoliciesPath.FindStringSubmatch(u.Path); m != nil {
				p.hideSaid(flag+"'s project", m[1])
			}
		}
	}
	if u, err := url.Parse(s.Ruler); err == nil && s.Ruler != "" {
		switch k, _ := source.RulerKind(s.Ruler); k {
		case source.RulerAzure:
			if m := azureSubRG.FindStringSubmatch(u.Path); m != nil {
				p.hide("--ruler's subscription", m[1])
				p.hideSaid("--ruler's resource group", m[2])
			}
		case source.RulerAmazon:
			p.hide("--ruler's workspace", strings.Trim(strings.TrimPrefix(u.Path, "/workspaces/"), "/"))
		}
	}
}

// ---- Grafana ----

// grafanaPermissions is where Grafana lists the caller's own actions, each
// with its scopes: served by Grafana OSS 12.4 and 13.3 as by Enterprise
// and Cloud, for any role, as measured (docs/design/preflight.md, "As
// built (step 2)"); Grafana's documentation names it among Enterprise's.
const grafanaPermissions = "/api/access-control/user/permissions"

// The actions Grafana's provisioning API asks of what serve sends with
// --changes-grafana-token-file (pkg/services/ngalert/api/authorization.go,
// 12.4.15 and 13.3.1): a rule read (GET of the rule and of the export)
// takes any of the first three, or both alert.rules:read and folders:read,
// whose scopes Grafana checks in the handler; a rule changed (PUT),
// deleted (DELETE) or made again (POST) takes any of the two provisioning
// writes, or alert.rules:write, :delete or :create, each with
// alert.provisioning.provenance:write, and then the rule's folder.
var (
	grafanaReads      = []string{"alert.provisioning:read", "alert.rules.provisioning:read", "alert.provisioning.secrets:read"}
	grafanaWrites     = []string{"alert.provisioning:write", "alert.rules.provisioning:write"}
	grafanaRuleWrites = []string{"alert.rules:write", "alert.rules:delete", "alert.rules:create", "alert.provisioning.provenance:write"}
)

// grafanaMay judges what a token may do, from Grafana's list of its
// actions: ok when it reads and writes every rule; a problem, naming what
// it lacks, when it can't make every change serve makes; unknown when it
// writes folder by folder, which rule is in which folder not read here.
func grafanaMay(perms map[string][]string) Item {
	has := func(a string) bool { return len(perms[a]) > 0 }
	everywhere := func(a string) bool {
		for _, sc := range perms[a] {
			if sc == "" || sc == "*" || sc == "folders:*" || sc == "folders:uid:*" {
				return true
			}
		}
		return false
	}
	whole := slices.ContainsFunc(grafanaWrites, has)
	readsAll := slices.ContainsFunc(grafanaReads, has)
	reads := readsAll || has("alert.rules:read") && has("folders:read")
	var lacks []string
	if !reads {
		lacks = append(lacks, "a read of the rules (alert.provisioning:read, or alert.rules:read with folders:read)")
	}
	if !whole {
		for _, a := range grafanaRuleWrites {
			if !has(a) {
				lacks = append(lacks, a)
			}
		}
	}
	it := Item{ID: "grafana.write"}
	switch {
	case len(lacks) > 0:
		it.State, it.What = Problem, "--changes-grafana-token-file's token may not make every change serve makes"
		it.Why = "Grafana says it lacks " + strings.Join(lacks, ", ") + ": serve reads a rule, then changes it (PUT), deletes it, or makes it again to undo a deletion (POST)"
		it.Fix = "give its service account the Admin role (alert.provisioning:write), or the Editor role: alert.rules:write, :delete and :create with alert.provisioning.provenance:write, folder by folder"
	case whole && (readsAll || everywhere("alert.rules:read") && everywhere("folders:read")):
		it.State, it.What = OK, "--changes-grafana-token-file's token may read and write every alert rule through the provisioning API (alert.provisioning:write), as Grafana lists its actions"
	case !whole && everywhere("alert.rules:write") && everywhere("alert.rules:delete") && everywhere("alert.rules:create") && everywhere("folders:read") &&
		(readsAll || everywhere("alert.rules:read")):
		it.State, it.What = OK, "--changes-grafana-token-file's token may write alert rules in every folder (alert.rules:write, :delete and :create on folders:*, with alert.provisioning.provenance:write), as Grafana lists its actions"
	default:
		// The folders it may read, but for Grafana's own: the root
		// (general) and "Shared with me", which hold no alert rule.
		var folders []string
		for _, sc := range perms["folders:read"] {
			if uid, ok := strings.CutPrefix(sc, "folders:uid:"); ok && uid != "general" && uid != "sharedwithme" && !slices.Contains(folders, uid) {
				folders = append(folders, uid)
			}
		}
		n := len(folders)
		it.State, it.What = Unknown, fmt.Sprintf("--changes-grafana-token-file's token may write alert rules folder by folder: it may read %s", plural(n, "folder"))
		it.Why = "Grafana checks each rule's folder as it writes it, and which folder each rule is in isn't read here: a rule in a folder it may not read is refused then"
		it.Fix = "to write in every folder, give its service account the Admin role (alert.provisioning:write); or give it the Editor role on each folder the rules are in"
	}
	return it
}

// grafanaWrite asks Grafana what --changes-grafana-token-file's token may
// do, once, and judges it (grafanaMay).
func (p *run) grafanaWrite(ctx context.Context) {
	s := p.c.Serve
	if s.ChangesGrafanaToken == "" {
		return
	}
	if p.c.Offline {
		p.offline("grafana.write", "what --changes-grafana-token-file's token may write not asked")
		return
	}
	b, err := os.ReadFile(s.ChangesGrafanaToken)
	if err != nil {
		p.add(Item{ID: "grafana.write", State: Unknown, What: "--changes-grafana-token-file can't be read, so what its token may write isn't asked", Why: said(err)})
		return
	}
	each, cancel := context.WithTimeout(ctx, eachTime)
	defer cancel()
	req, err := http.NewRequestWithContext(each, http.MethodGet, strings.TrimRight(s.Grafana, "/")+grafanaPermissions, nil)
	if err != nil {
		p.add(Item{ID: "grafana.write", State: Unknown, What: "what --changes-grafana-token-file's token may write isn't asked", Why: said(err)})
		return
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", p.agent())
	_, body, err := ask(req)
	switch code := codeOf(err); {
	case err == nil:
	case code == http.StatusUnauthorized:
		p.add(Item{ID: "grafana.write", State: Problem, What: "Grafana didn't take --changes-grafana-token-file's token", Why: said(err),
			Fix: "every change serve sends to Grafana fails with it: give it a service account's token Grafana takes"})
		return
	case code == http.StatusNotFound:
		p.add(Item{ID: "grafana.write", State: Unknown, What: "this Grafana doesn't say what a token may do: " + grafanaPermissions + " isn't served",
			Why: said(err) + "; no other API of Grafana's says what a token may write without its writing: the first change approved finds out"})
		return
	default:
		p.add(Item{ID: "grafana.write", State: Unknown, What: "what --changes-grafana-token-file's token may write isn't known", Why: said(err)})
		return
	}
	var perms map[string][]string
	if err := json.Unmarshal(body, &perms); err != nil {
		p.add(Item{ID: "grafana.write", State: Unknown, What: "Grafana answered " + grafanaPermissions + ", but not with a list of actions", Why: said(err)})
		return
	}
	p.add(grafanaMay(perms))
}

// ---- the clouds ----

// writer is a cloud's identity serve writes as, and what writes as it.
type writer struct {
	// id is its item's: changes.CLOUD.
	cloud, id, flag, who, endpoint string
	sign                           func(*http.Request) error
	// iam signs a request to IAM's API as the same identity: AWS's.
	iam func(*http.Request) error
}

// cloudWrites asks, for each identity taken, what it may write, and says
// changes.CLOUD of it.
func (p *run) cloudWrites(ctx context.Context, ws []writer) {
	for _, w := range ws {
		switch w.cloud {
		case "aws":
			p.add(p.awsWrites(ctx, w))
		case "azure":
			p.add(p.azureWrites(ctx, w))
		case "google":
			p.add(p.googleWrites(ctx, w))
		case "oci":
			p.add(p.ociWrites(ctx, w))
		}
	}
}

// hideWho has the report redact an identity serve writes as, and what a
// cloud's answer may name of it.
func (p *run) hideWho(cloudKind, flag, who string) {
	switch cloudKind {
	case "aws":
		p.hide(flag, who)
		if parts := strings.SplitN(who, ":", 6); len(parts) == 6 {
			p.hide(flag+"'s account", parts[4])
			if _, name, ok := strings.Cut(parts[5], "/"); ok {
				p.hideSaid(flag+"'s name", name[strings.LastIndex(name, "/")+1:])
			}
		}
	case "azure":
		p.hide(flag, who)
		p.hide("AZURE_TENANT_ID", os.Getenv("AZURE_TENANT_ID"))
	case "google":
		p.hide(flag, who)
		if name, _, ok := strings.Cut(who, "@"); ok {
			p.hideSaid(flag+"'s name", name)
		}
	case "oci":
		if u, err := ociConfigUser(who); err == nil {
			p.hide(flag+"'s tenancy", u.Tenancy)
			p.hide(flag+"'s user", u.User)
		}
	}
}

// ociConfigUser reads what an OCI config file names; tests replace it.
var ociConfigUser = cloud.OCIConfigUser

// googleAs is a service account's token, impersonated, of a scope; tests
// replace it.
var googleAs = cloud.GoogleAs

// verdictOf is one item from the parts of a cloud's answer: a problem
// when any part is one, unknown when any is, ok otherwise.
func verdictOf(id, flag string, oks, problems, unknowns []string, okWhat, fix string) Item {
	switch {
	case len(problems) > 0:
		return Item{ID: id, State: Problem, What: flag + " may not make every write serve makes", Why: strings.Join(slices.Concat(problems, unknowns, oks), "; "), Fix: fix}
	case len(unknowns) > 0:
		return Item{ID: id, State: Unknown, What: "whether " + flag + " may make every write serve makes isn't known", Why: strings.Join(slices.Concat(unknowns, oks), "; ")}
	case len(oks) == 0:
		return Item{ID: id, State: OK, What: flag + ": --changes-in names nothing it writes"}
	}
	return Item{ID: id, State: OK, What: flag + " may make every write serve makes: " + okWhat, Why: strings.Join(oks, "; ")}
}

// ---- Google Cloud ----

// crmHost is Cloud Resource Manager's: testIamPermissions on a project
// (https://cloud.google.com/resource-manager/reference/rest/v1/projects/testIamPermissions)
// gives the permissions of those asked that the caller holds there; it
// needs no permission, and takes the cloud-platform.read-only scope.
const crmHost = "https://cloudresourcemanager.googleapis.com"

// googleAlarmWrites are what serve's writes to a Google alerting policy
// need (https://cloud.google.com/monitoring/access-control): a PATCH of a
// policy, its DELETE, and its create, to undo a deletion.
var googleAlarmWrites = []string{"monitoring.alertPolicies.create", "monitoring.alertPolicies.delete", "monitoring.alertPolicies.update"}

// readOnly is the scope the service account's token is asked for here.
const readOnly = "https://www.googleapis.com/auth/cloud-platform.read-only"

var googlePoliciesPath = regexp.MustCompile(`^/v3/projects/([a-z0-9][-a-z0-9.:]{0,99})/alertPolicies/?$`)

type googleProject struct{ src, project string }

// googleProjects are the projects of Google's --alarms, each once.
func googleProjects(alarms []serve.AlarmsAt) []googleProject {
	var out []googleProject
	for _, a := range alarms {
		if k, _ := source.AlarmsKind(a.URL); k != source.AlarmsGoogle {
			continue
		}
		u, err := url.Parse(a.URL)
		if err != nil {
			continue
		}
		m := googlePoliciesPath.FindStringSubmatch(u.Path)
		if m == nil || slices.ContainsFunc(out, func(x googleProject) bool { return x.project == m[1] }) {
			continue
		}
		out = append(out, googleProject{a.Source, m[1]})
	}
	return out
}

// onlyPermissions is testIamPermissions' body as the preflight sends it:
// the permissions of Google's alerting policies, and nothing else.
func onlyPermissions(b []byte) bool {
	var in map[string][]string
	if err := json.Unmarshal(b, &in); err != nil || len(in) != 1 || len(in["permissions"]) == 0 {
		return false
	}
	for _, x := range in["permissions"] {
		if !slices.Contains(googleAlarmWrites, x) {
			return false
		}
	}
	return true
}

// googleWrites asks, for each project of Google's --alarms, which of the
// permissions serve's writes need the service account holds there.
func (p *run) googleWrites(ctx context.Context, w writer) Item {
	id := w.id
	each, cancel := context.WithTimeout(ctx, eachTime)
	defer cancel()
	var oks, problems, unknowns []string
	projects := googleProjects(p.c.Serve.Alarms)
	if len(projects) == 0 {
		return verdictOf(id, w.flag, nil, nil, nil, "", "")
	}
	sign, err := googleAs(each, w.who, readOnly)
	if err != nil {
		return Item{ID: id, State: Unknown, What: "whether " + w.flag + " may write isn't asked", Why: said(err)}
	}
	for _, x := range projects {
		where := "in the project of --alarms " + x.src
		body, _ := json.Marshal(map[string][]string{"permissions": googleAlarmWrites})
		req, err := http.NewRequestWithContext(each, http.MethodPost, crmHost+"/v1/projects/"+x.project+":testIamPermissions", bytes.NewReader(body))
		if err != nil {
			unknowns = append(unknowns, where+": "+said(err))
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", p.agent())
		if err := sign(req); err != nil {
			unknowns = append(unknowns, where+": "+said(err))
			continue
		}
		_, b, err := ask(req)
		if err != nil {
			unknowns = append(unknowns, where+": Cloud Resource Manager didn't say: "+said(err))
			continue
		}
		var out struct {
			Permissions []string `json:"permissions"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			unknowns = append(unknowns, where+": Cloud Resource Manager's answer isn't a list of permissions")
			continue
		}
		var lacks []string
		for _, want := range googleAlarmWrites {
			if !slices.Contains(out.Permissions, want) {
				lacks = append(lacks, want)
			}
		}
		if len(lacks) > 0 {
			problems = append(problems, where+": it doesn't hold "+strings.Join(lacks, ", "))
			continue
		}
		oks = append(oks, where+": it holds "+strings.Join(googleAlarmWrites, ", "))
	}
	return verdictOf(id, w.flag, oks, problems, unknowns, "the alerting policies' create, update and delete, in "+plural(len(projects), "project")+", as testIamPermissions says",
		"give the service account the permissions named on the project (charts/lapilli/README.md has the least privilege)")
}
