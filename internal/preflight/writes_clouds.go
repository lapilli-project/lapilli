package preflight

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/serve"
	"lapilli.dev/internal/source"
)

// ---- AWS ----

// What serve's writes as --changes-aws-role send, and so the actions they
// need (https://docs.aws.amazon.com/service-authorization/latest/reference/list_amazoncloudwatch.html,
// list_amazonmanagedserviceforprometheus.html): CloudWatch's
// PutMetricAlarm, DisableAlarmActions, EnableAlarmActions and DeleteAlarms
// on an alarm, and TagResource with ListTagsForResource, which
// PutMetricAlarm takes with its tags when it makes an alarm again to undo
// a deletion (API_PutMetricAlarm.html); Amazon Managed Service for
// Prometheus's PutRuleGroupsNamespace on a namespace. The chart's README
// gives the same as the role's least privilege.
var (
	cloudWatchWrites = []string{"cloudwatch:PutMetricAlarm", "cloudwatch:DisableAlarmActions", "cloudwatch:EnableAlarmActions",
		"cloudwatch:DeleteAlarms", "cloudwatch:TagResource", "cloudwatch:ListTagsForResource"}
	ampWrites = []string{"aps:PutRuleGroupsNamespace"}
)

// iamOf is IAM's API for a role's partition, and the region its requests
// are signed for (https://docs.aws.amazon.com/general/latest/gr/iam-service.html):
// IAM is one endpoint a partition. ok is false for what isn't a role's
// ARN.
func iamOf(role string) (host, region string, ok bool) {
	parts := strings.SplitN(role, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "iam" || parts[3] != "" || !awsAccountID.MatchString(parts[4]) ||
		!strings.HasPrefix(parts[5], "role/") {
		return "", "", false
	}
	switch parts[1] {
	case "aws":
		return "https://iam.amazonaws.com", "us-east-1", true
	case "aws-us-gov":
		return "https://iam.us-gov.amazonaws.com", "us-gov-west-1", true
	case "aws-cn":
		return "https://iam.cn-north-1.amazonaws.com.cn", "cn-north-1", true
	}
	return "", "", false
}

var awsAccountID = regexp.MustCompile(`^[0-9]{12}$`)

// simulateOnly is the one IAM request the preflight sends:
// SimulatePrincipalPolicy, of the role's own policies, with the actions
// and resources named and a page's size, and nothing else
// (https://docs.aws.amazon.com/IAM/latest/APIReference/API_SimulatePrincipalPolicy.html).
// It reads; no other action of IAM's is let through, a read or not.
func simulateOnly(role string) func(url.Values) bool {
	member := regexp.MustCompile(`^(ActionNames|ResourceArns)\.member\.[1-9][0-9]?$`)
	return func(f url.Values) bool {
		if len(f["Action"]) != 1 || f.Get("Action") != "SimulatePrincipalPolicy" || len(f["Version"]) != 1 || f.Get("Version") != "2010-05-08" ||
			len(f["PolicySourceArn"]) != 1 || f.Get("PolicySourceArn") != role {
			return false
		}
		for k, vs := range f {
			switch {
			case len(vs) != 1:
				return false
			case k == "Action", k == "Version", k == "PolicySourceArn", k == "MaxItems":
			case member.MatchString(k):
			default:
				return false
			}
		}
		return true
	}
}

// simulation is one ask of the simulator: what it is of, the actions, the
// resources.
type simulation struct {
	of        string
	actions   []string
	resources []string
}

// mostResources is how many resources one simulation names at most.
const mostResources = 20

// awsTargets are what serve writes as the role: CloudWatch's alarms that
// --changes-in names, each source's in its region, and the namespaces of
// Amazon's --ruler it names, as ARNs in the role's account, which is the
// one CloudWatch's and the control plane's writes are made in. A prefix
// (NAME*) is named as written: the simulator matches the policy's
// wildcards against it, so a policy for the prefix, or a wider one,
// allows it, and one for some names under it doesn't.
func (p *run) awsTargets(role string) []simulation {
	s := p.c.Serve
	parts := strings.SplitN(role, ":", 6)
	partition, account := parts[1], parts[4]
	var out []simulation
	var alarms []string
	for _, a := range s.Alarms {
		if k, _ := source.AlarmsKind(a.URL); k != source.AlarmsAWS {
			continue
		}
		u, _ := url.Parse(a.URL)
		_, region, _ := cloud.AWSHost(u.Hostname())
		for _, pat := range s.ChangesIn {
			if src, name, ok := strings.Cut(pat, ":"); ok && src == a.Source && name != "" {
				alarms = append(alarms, "arn:"+partition+":cloudwatch:"+region+":"+account+":alarm:"+name)
			}
		}
	}
	if len(alarms) > 0 {
		out = append(out, simulation{"CloudWatch's alarms", cloudWatchWrites, alarms})
	}
	if k, _ := source.RulerKind(s.Ruler); s.Ruler != "" && k == source.RulerAmazon {
		u, _ := url.Parse(s.Ruler)
		_, region, _ := cloud.AWSHost(u.Hostname())
		ws := strings.Trim(strings.TrimPrefix(u.Path, "/workspaces/"), "/")
		var nss []string
		for _, pat := range s.ChangesIn {
			if !p.alarmsEntry(pat) && pat != "" {
				nss = append(nss, "arn:"+partition+":aps:"+region+":"+account+":rulegroupsnamespace/"+ws+"/"+pat)
			}
		}
		if len(nss) > 0 {
			out = append(out, simulation{"Amazon's namespaces", ampWrites, nss})
		}
	}
	return out
}

// alarmsEntry reports whether an entry of --changes-in is an --alarms
// source's, SOURCE:PATTERN, as serve tells them apart.
func (p *run) alarmsEntry(pat string) bool {
	src, _, ok := strings.Cut(pat, ":")
	return ok && slices.ContainsFunc(p.c.Serve.Alarms, func(a serve.AlarmsAt) bool { return a.Source == src })
}

// simulated is the simulator's answer, as IAM's query API gives it
// (API_EvaluationResult.html, API_ResourceSpecificResult.html).
type simulated struct {
	Truncated bool        `xml:"SimulatePrincipalPolicyResult>IsTruncated"`
	Results   []simResult `xml:"SimulatePrincipalPolicyResult>EvaluationResults>member"`
}

// simResult is the simulator's decision on one action: allowed,
// explicitDeny or implicitDeny, over the resources asked, and on each.
type simResult struct {
	Action    string        `xml:"EvalActionName"`
	Decision  string        `xml:"EvalDecision"`
	Missing   []string      `xml:"MissingContextValues>member"`
	Resources []simResource `xml:"ResourceSpecificResults>member"`
}

type simResource struct {
	Name     string   `xml:"EvalResourceName"`
	Decision string   `xml:"EvalResourceDecision"`
	Missing  []string `xml:"MissingContextValues>member"`
}

// awsWrites asks IAM's policy simulator, as the role, whether its policies
// allow what serve's writes need on what --changes-in names. It needs
// iam:SimulatePrincipalPolicy on the role itself: without it, IAM refuses,
// and the item is unknown, never a problem. A decision that turns on a
// condition key the simulation isn't given is unknown too.
func (p *run) awsWrites(ctx context.Context, w writer) Item {
	id := w.id
	host, _, ok := iamOf(w.who)
	if !ok || w.iam == nil {
		return Item{ID: id, State: Unknown, What: "whether " + w.flag + " may write isn't asked: it isn't a role's ARN, so IAM's policy simulator can't be asked of it"}
	}
	sims := p.awsTargets(w.who)
	var oks, problems, unknowns []string
	each, cancel := context.WithTimeout(ctx, eachTime)
	defer cancel()
	for _, sim := range sims {
		names := sim.resources
		if len(names) > mostResources {
			unknowns = append(unknowns, fmt.Sprintf("%s: %d names past the first %d not asked", sim.of, len(names)-mostResources, mostResources))
			names = names[:mostResources]
		}
		f := url.Values{"Action": {"SimulatePrincipalPolicy"}, "Version": {"2010-05-08"}, "PolicySourceArn": {w.who}, "MaxItems": {"1000"}}
		for i, a := range sim.actions {
			f.Set("ActionNames.member."+strconv.Itoa(i+1), a)
		}
		for i, r := range names {
			f.Set("ResourceArns.member."+strconv.Itoa(i+1), r)
		}
		req, err := http.NewRequestWithContext(each, http.MethodPost, host+"/", strings.NewReader(f.Encode()))
		if err != nil {
			unknowns = append(unknowns, sim.of+": "+said(err))
			continue
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		req.Header.Set("User-Agent", p.agent())
		if err := w.iam(req); err != nil {
			unknowns = append(unknowns, sim.of+": "+said(err))
			continue
		}
		_, b, err := ask(req)
		if err != nil {
			if code := codeOf(err); code == http.StatusForbidden || code == http.StatusUnauthorized {
				return Item{ID: id, State: Unknown, What: "whether " + w.flag + " may write isn't known: IAM didn't let it ask its policy simulator", Why: said(err),
					Fix: "give the role iam:SimulatePrincipalPolicy on its own ARN, which shows it only what its own policies allow, and this is known; serve itself never needs it"}
			}
			unknowns = append(unknowns, sim.of+": IAM's policy simulator didn't say: "+said(err))
			continue
		}
		var out simulated
		if err := xml.Unmarshal(b, &out); err != nil {
			unknowns = append(unknowns, sim.of+": the simulator's answer can't be read")
			continue
		}
		pr, un := simDecisions(sim.actions, names, out)
		if len(pr) > 0 {
			problems = append(problems, sim.of+": "+strings.Join(pr, ", "))
		}
		if len(un) > 0 {
			unknowns = append(unknowns, sim.of+": "+strings.Join(un, ", "))
		}
		if len(pr)+len(un) == 0 {
			oks = append(oks, fmt.Sprintf("%s: %s on %s --changes-in names", sim.of, strings.Join(sim.actions, ", "), plural(len(names), "name")))
		}
	}
	return verdictOf(id, w.flag, oks, problems, unknowns, "as IAM's policy simulator says of the role's policies, its permissions boundary and the organization's SCPs",
		"give the role these actions on what --changes-in names (charts/lapilli/README.md has the least privilege)")
}

// simDecisions reads the simulator's decisions: each action on each
// resource asked, allowed, denied (a problem) or allowed only with a
// condition key it isn't given, or not answered (unknown).
func simDecisions(actions, resources []string, out simulated) (problems, unknowns []string) {
	if out.Truncated {
		unknowns = append(unknowns, "the simulator's answer went on past its first page")
	}
	for _, a := range actions {
		i := slices.IndexFunc(out.Results, func(r simResult) bool { return strings.EqualFold(r.Action, a) })
		if i < 0 {
			unknowns = append(unknowns, a+" not answered")
			continue
		}
		r := out.Results[i]
		denied, conditional := 0, 0
		var keys []string
		judge := func(decision string, missing []string) {
			switch {
			case decision == "allowed":
			case len(missing) > 0:
				conditional++
				keys = append(keys, missing...)
			default:
				denied++
			}
		}
		if len(r.Resources) == 0 {
			judge(r.Decision, r.Missing)
			if denied > 0 {
				denied = len(resources)
			}
			if conditional > 0 {
				conditional = len(resources)
			}
		} else {
			for _, res := range resources {
				j := slices.IndexFunc(r.Resources, func(x simResource) bool { return x.Name == res })
				if j < 0 {
					unknowns = append(unknowns, a+" not answered for one of the names")
					continue
				}
				judge(r.Resources[j].Decision, r.Resources[j].Missing)
			}
		}
		if denied > 0 {
			problems = append(problems, fmt.Sprintf("%s denied for %d of %d", a, denied, len(resources)))
		}
		if conditional > 0 {
			slices.Sort(keys)
			unknowns = append(unknowns, fmt.Sprintf("%s for %d of %d on a condition of %s, which the simulation isn't given", a, conditional, len(resources),
				saidText(strings.Join(slices.Compact(keys), ", "))))
		}
	}
	return problems, unknowns
}

// ---- Azure ----

// The actions serve's writes as --changes-azure-client-id are
// (https://learn.microsoft.com/en-us/azure/role-based-access-control/permissions/monitor):
// a rule group's PUT; an alert rule's PATCH or PUT, and a metric alert's
// DELETE, which only a PromQL one is ("write" is PUT or PATCH, role-definitions).
var (
	azureRulerWrites = []string{"Microsoft.AlertsManagement/prometheusRuleGroups/write"}
	azureAlarmWrites = []string{"Microsoft.Insights/metricAlerts/write", "Microsoft.Insights/metricAlerts/delete",
		"Microsoft.Insights/scheduledQueryRules/write", "Microsoft.Insights/activityLogAlerts/write"}
)

// azurePermissionsAPI is the api-version Azure's permissions of the
// caller are read with, on a resource group or a resource
// (https://learn.microsoft.com/en-us/rest/api/authorization/permissions/list-for-resource-group?view=rest-authorization-2022-04-01).
const (
	azurePermissionsAPI  = "2022-04-01"
	azurePermissionsPath = "/providers/Microsoft.Authorization/permissions"
)

// azureScope is where Azure is asked what the identity may do: a
// resource group, or a rule group named whole, on Resource Manager's host;
// its path as it reads, unescaped.
type azureScope struct {
	host, path string
	actions    []string
	// of says what is written there, for the item; named, that a name
	// --changes-in gives whole is among what is written there, which may
	// be given on each of it alone: not allowed on the group is unknown.
	of    string
	named bool
	// none is why no scope could be said, for an entry whose resource
	// group isn't named.
	none string
}

// mostScopes is how many scopes are asked at most.
const mostScopes = 10

var (
	azureSubRG = regexp.MustCompile(`(?i)^/subscriptions/([0-9A-Za-z-]{1,64})(?:/resourceGroups/([-\w.()]{1,90}))?/?$`)
	azureRG    = regexp.MustCompile(`^[-\w.()]{1,90}$`)
)

// azureScopes are the scopes of what serve writes as the identity, from
// --changes-in: Azure's --ruler's rule groups (RG/NAME, or NAME in the
// --ruler's resource group), each named whole asked on itself, a prefix on
// its resource group; Azure's --alarms' alert rules (SOURCE:RG/NAME), on
// their resource group, as an alert rule's type isn't in its name.
func (p *run) azureScopes() []azureScope {
	s := p.c.Serve
	var out []azureScope
	put := func(sc azureScope) {
		for i := range out {
			if out[i].none == "" && strings.EqualFold(out[i].host+out[i].path, sc.host+sc.path) {
				for _, a := range sc.actions {
					if !slices.Contains(out[i].actions, a) {
						out[i].actions = append(out[i].actions, a)
					}
				}
				out[i].named = out[i].named || sc.named
				if !strings.Contains(out[i].of, sc.of) {
					out[i].of += ", " + sc.of
				}
				return
			}
		}
		out = append(out, sc)
	}
	if k, _ := source.RulerKind(s.Ruler); s.Ruler != "" && k == source.RulerAzure {
		u, _ := url.Parse(s.Ruler)
		host := "https://" + strings.ToLower(u.Host)
		m := azureSubRG.FindStringSubmatch(u.Path)
		for _, pat := range s.ChangesIn {
			if p.alarmsEntry(pat) || m == nil {
				continue
			}
			if !strings.Contains(pat, "/") {
				if m[2] == "" {
					continue // serve names nothing by it: no resource group
				}
				pat = m[2] + "/" + pat
			}
			rg, name, _ := strings.Cut(pat, "/")
			if !azureRG.MatchString(rg) {
				put(azureScope{of: "--ruler's rule groups", none: "a resource group --changes-in doesn't name whole"})
				continue
			}
			base := "/subscriptions/" + m[1] + "/resourceGroups/" + rg
			if name == "" || strings.HasSuffix(name, "*") || strings.Contains(name, "/") {
				put(azureScope{host: host, path: base, actions: azureRulerWrites, of: "--ruler's rule groups"})
			} else {
				put(azureScope{host: host, path: base + "/providers/Microsoft.AlertsManagement/prometheusRuleGroups/" + name, actions: azureRulerWrites,
					of: "--ruler's rule groups"})
			}
		}
	}
	for _, a := range s.Alarms {
		if k, _ := source.AlarmsKind(a.URL); k != source.AlarmsAzure {
			continue
		}
		u, _ := url.Parse(a.URL)
		host := "https://" + strings.ToLower(u.Host)
		m := azureSubRG.FindStringSubmatch(u.Path)
		if m == nil {
			continue
		}
		for _, pat := range s.ChangesIn {
			src, rest, ok := strings.Cut(pat, ":")
			if !ok || src != a.Source {
				continue
			}
			of := "--alarms " + a.Source
			rg, name, slash := strings.Cut(rest, "/")
			switch {
			case slash && azureRG.MatchString(rg) && (m[2] == "" || strings.EqualFold(m[2], rg)):
				put(azureScope{host: host, path: "/subscriptions/" + m[1] + "/resourceGroups/" + rg, actions: azureAlarmWrites, of: of,
					named: name != "" && !strings.HasSuffix(name, "*")})
			case slash:
				// Not under the URL's scope, or no resource group: serve
				// writes nothing by it.
			case m[2] != "" && strings.HasSuffix(rest, "*") && strings.HasPrefix(strings.ToLower(m[2]), strings.ToLower(strings.TrimSuffix(rest, "*"))):
				put(azureScope{host: host, path: "/subscriptions/" + m[1] + "/resourceGroups/" + m[2], actions: azureAlarmWrites, of: of})
			case strings.HasSuffix(rest, "*"):
				put(azureScope{of: of, none: "resource groups --changes-in names by a prefix alone"})
			}
		}
	}
	return out
}

// azureMay reports whether Azure's permissions allow an action: some
// entry's actions match it, and none of that entry's notActions do
// (role-definitions: "Actions - NotActions"), each compared in any letter
// case with * standing for any text.
func azureMay(perms []azurePermission, action string) bool {
	for _, p := range perms {
		if slices.ContainsFunc(p.Actions, func(a string) bool { return azureMatch(a, action) }) &&
			!slices.ContainsFunc(p.NotActions, func(a string) bool { return azureMatch(a, action) }) {
			return true
		}
	}
	return false
}

// azureMatch matches an action against a pattern of Azure's: in any
// letter case, each * any text, / and . among it.
func azureMatch(pattern, action string) bool {
	pat, a := strings.ToLower(pattern), strings.ToLower(action)
	parts := strings.Split(pat, "*")
	if len(parts) == 1 {
		return pat == a
	}
	if !strings.HasPrefix(a, parts[0]) {
		return false
	}
	a = a[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(a, mid)
		if i < 0 {
			return false
		}
		a = a[i+len(mid):]
	}
	return strings.HasSuffix(a, parts[len(parts)-1])
}

// azurePermission is one of Azure's permissions of the caller.
type azurePermission struct {
	Actions    []string `json:"actions"`
	NotActions []string `json:"notActions"`
}

// azureWrites asks Azure, as the identity, its permissions on each scope
// of what serve writes as it, and judges them against the actions those
// writes need. A deny assignment isn't among them: one could still refuse
// a write they allow.
func (p *run) azureWrites(ctx context.Context, w writer) Item {
	id := w.id
	scopes := p.azureScopes()
	var oks, problems, unknowns []string
	each, cancel := context.WithTimeout(ctx, eachTime)
	defer cancel()
	asked := 0
	for _, sc := range scopes {
		if sc.none != "" {
			unknowns = append(unknowns, sc.of+": "+sc.none+", so where it writes isn't asked")
			continue
		}
		if asked++; asked > mostScopes {
			unknowns = append(unknowns, sc.of+": past the first "+strconv.Itoa(mostScopes)+" scopes, not asked")
			continue
		}
		// The subscription is --alarms's or --ruler's (hideScopes); the
		// resource group may be --changes-in's.
		if m := azureSubRG.FindStringSubmatch(strings.SplitN(sc.path, "/providers/", 2)[0]); m != nil {
			p.hideSaid("--changes-in's resource group", m[2])
		}
		where := sc.of + " on a resource group"
		if strings.Contains(sc.path, "/providers/") {
			where = sc.of + " on a rule group --changes-in names"
		}
		perms, err := p.azurePermissions(each, w, sc)
		if err != nil {
			unknowns = append(unknowns, where+": Azure didn't say: "+said(err))
			continue
		}
		var lacks []string
		for _, a := range sc.actions {
			if !azureMay(perms, a) {
				lacks = append(lacks, a)
			}
		}
		switch {
		case len(lacks) == 0:
			oks = append(oks, where+": "+strings.Join(sc.actions, ", "))
		case sc.named:
			unknowns = append(unknowns, where+": not given there "+strings.Join(lacks, ", ")+", which may be given on each alert rule named, not asked")
		default:
			problems = append(problems, where+": not given "+strings.Join(lacks, ", "))
		}
	}
	return verdictOf(id, w.flag, oks, problems, unknowns, "as Azure's permissions for it say, on "+plural(min(asked, mostScopes), "scope")+"; a deny assignment, which they leave out, could still refuse one",
		"give the identity a role with these actions where --changes-in names (charts/lapilli/README.md has the least privilege)")
}

// mostPages is how many pages of an answer are read at most.
const mostPages = 5

// azurePermissions reads the identity's permissions on a scope, following
// nextLink on the same host and path, mostPages pages at most.
func (p *run) azurePermissions(ctx context.Context, w writer, sc azureScope) ([]azurePermission, error) {
	// The path as it reads, escaped as a URL's is: the allowlist compares
	// it as it reads.
	h, _ := url.Parse(sc.host)
	next := (&url.URL{Scheme: h.Scheme, Host: h.Host, Path: sc.path + azurePermissionsPath, RawQuery: "api-version=" + azurePermissionsAPI}).String()
	var all []azurePermission
	for pages := 0; next != ""; pages++ {
		if pages >= mostPages {
			return nil, fmt.Errorf("more than %d pages", mostPages)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", p.agent())
		if err := w.sign(req); err != nil {
			return nil, err
		}
		_, b, err := ask(req)
		if err != nil {
			return nil, err
		}
		var page struct {
			Value    []azurePermission `json:"value"`
			NextLink string            `json:"nextLink"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, fmt.Errorf("not a list of permissions: %w", err)
		}
		all = append(all, page.Value...)
		next = ""
		if page.NextLink != "" {
			u, err := url.Parse(page.NextLink)
			if err != nil || u.Scheme != "https" || !strings.EqualFold("https://"+u.Host, sc.host) || u.User != nil ||
				!strings.EqualFold(u.Path, sc.path+azurePermissionsPath) {
				return nil, fmt.Errorf("the next page is elsewhere than the first: it isn't followed")
			}
			p.allowURLAt("iam:azure", page.NextLink, true)
			next = page.NextLink
		}
	}
	return all, nil
}
