package preflight

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/serve"
	"lapilli.dev/internal/source"
)

// ---- AWS ----

// What serve's writes as --changes-aws-role send, and so the actions they
// need (https://docs.aws.amazon.com/service-authorization/latest/reference/list_amazoncloudwatch.html,
// list_amazonmanagedserviceforprometheus.html): CloudWatch's
// PutMetricAlarm, DisableAlarmActions, EnableAlarmActions and DeleteAlarms
// on an alarm, and TagResource, which PutMetricAlarm takes with its tags
// when it makes an alarm again to undo a deletion (API_PutMetricAlarm.html);
// Amazon Managed Service for Prometheus's PutRuleGroupsNamespace on a
// namespace. An alarm's tags are read with the pod's own identity
// (source.Alarms' Reads), not the role's.
var (
	cloudWatchWrites = []string{"cloudwatch:PutMetricAlarm", "cloudwatch:DisableAlarmActions", "cloudwatch:EnableAlarmActions",
		"cloudwatch:DeleteAlarms", "cloudwatch:TagResource"}
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
// and resources named, a page's size, and one context entry, the region
// asked in (aws:RequestedRegion, a string, one of regions), and nothing
// else (https://docs.aws.amazon.com/IAM/latest/APIReference/API_SimulatePrincipalPolicy.html).
// It reads; no other action of IAM's is let through, a read or not.
func simulateOnly(role string, regions []string) func(url.Values) bool {
	member := regexp.MustCompile(`^(ActionNames|ResourceArns)\.member\.[1-9][0-9]?$`)
	return func(f url.Values) bool {
		if len(f["Action"]) != 1 || f.Get("Action") != "SimulatePrincipalPolicy" || len(f["Version"]) != 1 || f.Get("Version") != "2010-05-08" ||
			len(f["PolicySourceArn"]) != 1 || f.Get("PolicySourceArn") != role {
			return false
		}
		if f.Get(ctxName) != "aws:RequestedRegion" || f.Get(ctxType) != "string" || !slices.Contains(regions, f.Get(ctxValue)) {
			return false
		}
		for k, vs := range f {
			switch {
			case len(vs) != 1:
				return false
			case k == "Action", k == "Version", k == "PolicySourceArn", k == "MaxItems", k == ctxName, k == ctxType, k == ctxValue:
			case member.MatchString(k):
			default:
				return false
			}
		}
		return true
	}
}

// The context entry a simulation is given: the region its writes are
// made in, as an SCP's region guardrail reads it.
const (
	ctxName  = "ContextEntries.member.1.ContextKeyName"
	ctxType  = "ContextEntries.member.1.ContextKeyType"
	ctxValue = "ContextEntries.member.1.ContextKeyValues.member.1"
)

// simulation is one ask of the simulator: what it is of, the region, the
// actions, the resources; and what of --changes-in it couldn't name.
type simulation struct {
	of, region string
	actions    []string
	resources  []string
	unnamed    []string
}

// mostResources is how many resources one simulation names at most.
const mostResources = 20

// awsRegions are the regions serve's writes as the role are made in: its
// CloudWatch --alarms' and Amazon's --ruler's.
func (p *run) awsRegions() []string {
	s := p.c.Serve
	var out []string
	add := func(raw string) {
		if u, err := url.Parse(raw); err == nil {
			if _, r, ok := cloud.AWSHost(u.Hostname()); ok && !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	for _, a := range s.Alarms {
		if k, _ := source.AlarmsKind(a.URL); k == source.AlarmsAWS {
			add(a.URL)
		}
	}
	if k, _ := source.RulerKind(s.Ruler); s.Ruler != "" && k == source.RulerAmazon {
		add(s.Ruler)
	}
	return out
}

// awsTargets are what serve writes as the role, by the region: the
// CloudWatch alarms --changes-in names, and the namespaces of Amazon's
// --ruler, as ARNs in the role's account, the one CloudWatch's and the
// control plane's writes are made in. A name given whole is its ARN. A
// prefix (NAME*) is each alarm or namespace this run read under it: a
// policy's Deny or NotResource for some names under a prefix never matches
// the prefix written as an ARN, so it isn't asked so. One with nothing read
// under it, or past mostResources, is unnamed: not asked, and unknown. A
// CloudWatch prefix is also asked of one name under it no alarm read has
// (addedUnder): the alarm a rule added there makes, which no read alarm
// stands for (docs/design/expectations.md, step 5).
func (p *run) awsTargets(role string) []simulation {
	s := p.c.Serve
	parts := strings.SplitN(role, ":", 6)
	partition, account := parts[1], parts[4]
	var out []simulation
	put := func(of, region string, actions []string, name func(string) string, read []string, pats []string, added bool) {
		sim := simulation{of: of, region: region, actions: actions}
		for _, pat := range pats {
			var arns []string
			if prefix, ok := strings.CutSuffix(pat, "*"); ok {
				for _, n := range read {
					if strings.HasPrefix(n, prefix) {
						arns = append(arns, name(n))
					}
				}
				if len(arns) == 0 {
					sim.unnamed = append(sim.unnamed, "nothing read under a prefix --changes-in gives")
				}
				if added {
					arns = append(arns, name(addedUnder(prefix, read)))
				}
			} else {
				arns = []string{name(pat)}
			}
			for _, a := range arns {
				switch {
				case slices.Contains(sim.resources, a):
				case len(sim.resources) >= mostResources:
					sim.unnamed = append(sim.unnamed, fmt.Sprintf("names past the first %d", mostResources))
				default:
					sim.resources = append(sim.resources, a)
				}
			}
		}
		slices.Sort(sim.unnamed)
		sim.unnamed = slices.Compact(sim.unnamed)
		if len(sim.resources)+len(sim.unnamed) > 0 {
			out = append(out, sim)
		}
	}
	for _, a := range s.Alarms {
		if k, _ := source.AlarmsKind(a.URL); k != source.AlarmsAWS {
			continue
		}
		u, _ := url.Parse(a.URL)
		_, region, _ := cloud.AWSHost(u.Hostname())
		var pats, read []string
		for _, pat := range s.ChangesIn {
			if src, name, ok := strings.Cut(pat, ":"); ok && src == a.Source && name != "" {
				pats = append(pats, name)
			}
		}
		// The alarms read, in the role's account: one in another isn't
		// written (serve's awsAccount).
		for _, d := range p.docs["alarms:"+a.Source] {
			if m := cloudwatchARN.FindStringSubmatch(d.Source); m != nil && m[2] == account {
				read = append(read, m[3])
			}
		}
		arn := func(n string) string {
			return "arn:" + partition + ":cloudwatch:" + region + ":" + account + ":alarm:" + n
		}
		put("CloudWatch's alarms of --alarms "+a.Source, region, cloudWatchWrites, arn, read, pats, true)
	}
	if k, _ := source.RulerKind(s.Ruler); s.Ruler != "" && k == source.RulerAmazon {
		u, _ := url.Parse(s.Ruler)
		_, region, _ := cloud.AWSHost(u.Hostname())
		ws := strings.Trim(strings.TrimPrefix(u.Path, "/workspaces/"), "/")
		var pats, read []string
		for _, pat := range s.ChangesIn {
			if !p.alarmsEntry(pat) && pat != "" {
				pats = append(pats, pat)
			}
		}
		for _, d := range p.docs["ruler"] {
			if d.Namespace != "" && !slices.Contains(read, d.Namespace) {
				read = append(read, d.Namespace)
			}
		}
		arn := func(n string) string {
			return "arn:" + partition + ":aps:" + region + ":" + account + ":rulegroupsnamespace/" + ws + "/" + n
		}
		put("Amazon's namespaces", region, ampWrites, arn, read, pats, false)
	}
	return out
}

// addedUnder is a name under a prefix that no alarm read has, standing for
// the alarm a rule added there would make: the prefix, then
// lapilli-added, a number after it while one read has that name.
func addedUnder(prefix string, read []string) string {
	n := prefix + "lapilli-added"
	for i := 2; slices.Contains(read, n); i++ {
		n = fmt.Sprintf("%slapilli-added-%d", prefix, i)
	}
	return n
}

// cloudwatchARN is a CloudWatch alarm's ARN: its region, account, name.
var cloudwatchARN = regexp.MustCompile(`^arn:aws(?:-cn|-us-gov)?:cloudwatch:([a-z0-9-]+):([0-9]{12}):alarm:(.{1,255})$`)

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
		for _, u := range sim.unnamed {
			unknowns = append(unknowns, sim.of+": "+u+", not asked")
		}
		names := sim.resources
		if len(names) == 0 {
			continue
		}
		f := url.Values{"Action": {"SimulatePrincipalPolicy"}, "Version": {"2010-05-08"}, "PolicySourceArn": {w.who}, "MaxItems": {"1000"},
			ctxName: {"aws:RequestedRegion"}, ctxType: {"string"}, ctxValue: {sim.region}}
		for i, a := range sim.actions {
			f.Set("ActionNames.member."+strconv.Itoa(i+1), a)
		}
		for i, r := range names {
			f.Set("ResourceArns.member."+strconv.Itoa(i+1), r)
		}
		req, err := http.NewRequestWithContext(each, http.MethodPost, host+"/", strings.NewReader(f.Encode()))
		if err != nil {
			unknowns = append(unknowns, sim.of+": "+p.cloudSaid(err))
			continue
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		req.Header.Set("User-Agent", p.agent())
		if err := w.iam(req); err != nil {
			unknowns = append(unknowns, sim.of+": "+p.cloudSaid(err))
			continue
		}
		_, b, err := ask(req)
		if err != nil {
			if code := codeOf(err); code == http.StatusForbidden || code == http.StatusUnauthorized {
				return Item{ID: id, State: Unknown, What: "whether " + w.flag + " may write isn't known: IAM didn't let it ask its policy simulator", Why: p.cloudSaid(err),
					Fix: "give the role iam:SimulatePrincipalPolicy on its own ARN, which shows it only what its own policies allow, and this is known; serve itself never needs it"}
			}
			unknowns = append(unknowns, sim.of+": IAM's policy simulator didn't say: "+p.cloudSaid(err))
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
			oks = append(oks, fmt.Sprintf("%s: %s on %s, in %s", sim.of, strings.Join(sim.actions, ", "), plural(len(names), "name"), sim.region))
		}
	}
	return verdictOf(id, w.flag, oks, problems, unknowns, "as IAM's policy simulator says of the role's policies, its permissions boundary and the organization's SCPs, "+
		"given the region; an SCP's conditions on any other key aren't judged (the simulator doesn't say which an SCP lacks), nor are resource control policies",
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
		// A decision with a context key the simulation lacks, allowed or
		// not, is on a condition it couldn't judge: unknown.
		judge := func(decision string, missing []string) {
			missing = append(slices.Clone(missing), r.Missing...)
			switch {
			case len(missing) > 0:
				conditional++
				keys = append(keys, missing...)
			case decision == "allowed":
			default:
				denied++
			}
		}
		if len(r.Resources) == 0 {
			judge(r.Decision, nil)
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

// What serve's writes as --changes-azure-client-id are, by the type they
// are of (https://learn.microsoft.com/en-us/azure/role-based-access-control/permissions/monitor):
// a rule group's PUT; an alert rule's PATCH or PUT ("write" is PUT or
// PATCH, role-definitions); and a metric alert's DELETE, which only a
// PromQL one is, so only where metric alerts may be named.
var azureWriteOf = map[string]string{
	"prometheusRuleGroups": "Microsoft.AlertsManagement/prometheusRuleGroups/write",
	"metricAlerts":         "Microsoft.Insights/metricAlerts/write",
	"scheduledQueryRules":  "Microsoft.Insights/scheduledQueryRules/write",
	"activityLogAlerts":    "Microsoft.Insights/activityLogAlerts/write",
}

const azureMetricDelete = "Microsoft.Insights/metricAlerts/delete"

// The api-versions and paths of what Azure is asked, at a scope: the
// caller's permissions (permissions/list-for-resource-group,
// rest-authorization-2022-04-01), the deny assignments at or above it
// (deny-assignments/list-for-resource-group: "$filter=atScope() to return
// all deny assignments at or above the scope"), and its management locks
// (management-locks/list-at-resource-group-level, rest-resources-2016-09-01).
const (
	azurePermissionsAPI  = "2022-04-01"
	azurePermissionsPath = "/providers/Microsoft.Authorization/permissions"
	azureDenyPath        = "/providers/Microsoft.Authorization/denyAssignments"
	azureLocksAPI        = "2016-09-01"
	azureLocksPath       = "/providers/Microsoft.Authorization/locks"
)

var (
	azurePermissionsQuery = url.Values{"api-version": {azurePermissionsAPI}}
	azureDenyQuery        = url.Values{"api-version": {azurePermissionsAPI}, "$filter": {"atScope()"}}
	azureLocksQuery       = url.Values{"api-version": {azureLocksAPI}}
)

// azureScope is where Azure is asked what the identity may do: a
// resource group, or a rule group named whole, on Resource Manager's host;
// its path as it reads, unescaped.
type azureScope struct {
	host, path, sub string
	// types are what may be written there; deletes, that a metric alert
	// may be deleted there; pats, the names (RG/NAME, in any letter case)
	// of what is written, for a lock on one of them.
	types   []string
	deletes bool
	pats    []string
	// of says what is written there, for the item; named, that a name
	// --changes-in gives whole is among what is written there, which may
	// be given on each of it alone: not allowed on the group is unknown.
	// unread, that an entry of --changes-in names nothing this run read
	// there: a deny on such a rule itself, below the group, can't be
	// asked.
	of     string
	named  bool
	unread bool
	// none is why no scope could be said, for an entry whose resource
	// group isn't named.
	none string
}

// actions are the actions a scope's writes need.
func (sc azureScope) actions() []string {
	var out []string
	for _, t := range sc.types {
		out = append(out, azureWriteOf[t])
	}
	if sc.deletes {
		out = append(out, azureMetricDelete)
	}
	slices.Sort(out)
	return out
}

// mostScopes is how many scopes are asked at most; mostLinked, how many
// resources the alert rules name are asked about.
const (
	mostScopes = 10
	mostLinked = 10
	mostNamed  = 10
)

var (
	azureSubRG = regexp.MustCompile(`(?i)^/subscriptions/([0-9A-Za-z-]{1,64})(?:/resourceGroups/([-\w.()]{1,90}))?/?$`)
	azureRG    = regexp.MustCompile(`^[-\w.()]{1,90}$`)
	// azureResource is a resource's id under a resource group: what an
	// alert rule names (its target, an action group).
	azureResource = regexp.MustCompile(`(?i)^/subscriptions/([0-9A-Za-z-]{1,64})/resourceGroups/([-\w.()]{1,90})(?:/providers/([A-Za-z0-9.]{1,100})((?:/[A-Za-z0-9.]{1,100}/[^/?#]+){1,4}))?$`)
	azureAlertID  = regexp.MustCompile(`(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.(?:Insights|AlertsManagement)/([A-Za-z]+)/[^/]+$`)
)

// azureNamed is an alert rule or a rule group --changes-in names, as this
// run read it: its id, its type, and the document.
type azureNamed struct {
	id, typ string
	data    []byte
}

// azureAlarmTypes are the alert rule types an entry of an Azure --alarms
// may name.
var azureAlarmTypes = []string{"activityLogAlerts", "metricAlerts", "scheduledQueryRules"}

// azureScopes are the scopes of what serve writes as the identity, from
// --changes-in: Azure's --ruler's rule groups (RG/NAME, or NAME in the
// --ruler's resource group), each named whole asked on itself, a prefix on
// its resource group; Azure's --alarms' alert rules (SOURCE:RG/NAME), on
// their resource group, as an alert rule's type isn't in its name. An
// alert rule named whole and read in this run is of its type; one not
// read, or a prefix, may be of any. named are the alert rules and rule
// groups read that --changes-in names.
func (p *run) azureScopes() (out []azureScope, named []azureNamed) {
	s := p.c.Serve
	put := func(sc azureScope) {
		for i := range out {
			if out[i].none == "" && sc.none == "" && strings.EqualFold(out[i].host+out[i].path, sc.host+sc.path) {
				for _, t := range sc.types {
					if !slices.Contains(out[i].types, t) {
						out[i].types = append(out[i].types, t)
					}
				}
				slices.Sort(out[i].types)
				out[i].deletes = out[i].deletes || sc.deletes
				out[i].named = out[i].named || sc.named
				out[i].unread = out[i].unread || sc.unread
				out[i].pats = append(out[i].pats, sc.pats...)
				if !strings.Contains(out[i].of, sc.of) {
					out[i].of += ", " + sc.of
				}
				return
			}
		}
		out = append(out, sc)
	}
	seen := map[string]bool{}
	addNamed := func(n azureNamed) {
		if k := strings.ToLower(n.id); !seen[k] {
			seen[k] = true
			named = append(named, n)
		}
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
			read := 0
			for _, d := range p.docs["ruler"] {
				if x := azureAlertID.FindStringSubmatch(d.Source); x != nil && namedFold(pat, d.Namespace+"/"+d.Source[strings.LastIndex(d.Source, "/")+1:]) {
					addNamed(azureNamed{d.Source, "prometheusRuleGroups", d.Data})
					read++
				}
			}
			base := "/subscriptions/" + m[1] + "/resourceGroups/" + rg
			sc := azureScope{host: host, path: base, sub: m[1], types: []string{"prometheusRuleGroups"}, pats: []string{pat}, of: "--ruler's rule groups",
				unread: read == 0}
			if name != "" && !strings.HasSuffix(name, "*") && !strings.Contains(name, "/") {
				sc.path = base + "/providers/Microsoft.AlertsManagement/prometheusRuleGroups/" + name
			}
			put(sc)
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
			// The types it may name: those read under it, or any.
			var types []string
			for _, d := range p.docs["alarms:"+a.Source] {
				if namedFold(rest, rules.AlarmName(d.Data, "azure")) {
					if x := azureAlertID.FindStringSubmatch(d.Source); x != nil {
						t := x[1]
						if i := slices.IndexFunc(azureAlarmTypes, func(y string) bool { return strings.EqualFold(y, t) }); i >= 0 {
							t = azureAlarmTypes[i]
						}
						if !slices.Contains(types, t) {
							types = append(types, t)
						}
						addNamed(azureNamed{d.Source, t, d.Data})
					}
				}
			}
			whole := rest != "" && !strings.HasSuffix(rest, "*")
			unread := len(types) == 0
			if !whole || len(types) == 0 {
				types = slices.Clone(azureAlarmTypes)
			}
			slices.Sort(types)
			sc := azureScope{host: host, sub: m[1], types: types, deletes: slices.Contains(types, "metricAlerts"), of: of, unread: unread}
			rg, name, slash := strings.Cut(rest, "/")
			switch {
			case slash && azureRG.MatchString(rg) && (m[2] == "" || strings.EqualFold(m[2], rg)):
				sc.path, sc.pats, sc.named = "/subscriptions/"+m[1]+"/resourceGroups/"+rg, []string{rest}, name != "" && !strings.HasSuffix(name, "*")
				put(sc)
			case slash:
				// Not under the URL's scope, or no resource group: serve
				// writes nothing by it.
			case m[2] != "" && strings.HasSuffix(rest, "*") && strings.HasPrefix(strings.ToLower(m[2]), strings.ToLower(strings.TrimSuffix(rest, "*"))):
				sc.path, sc.pats = "/subscriptions/"+m[1]+"/resourceGroups/"+m[2], []string{m[2] + "/*"}
				put(sc)
			case strings.HasSuffix(rest, "*"):
				put(azureScope{of: of, none: "resource groups --changes-in names by a prefix alone"})
			}
		}
	}
	return out, named
}

// namedFold reports whether an entry of --changes-in names a name in any
// letter case, as serve reads Azure's.
func namedFold(pat, name string) bool { return namedBy(strings.ToLower(pat), strings.ToLower(name)) }

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

// azurePermission is one of Azure's permissions of the caller, or of a
// deny assignment.
type azurePermission struct {
	Actions    []string `json:"actions"`
	NotActions []string `json:"notActions"`
}

// azureLock is a management lock (lock-resources: "ReadOnly means
// authorized users can read a resource, but they can't delete or update
// it"; "CanNotDelete ... can't delete it").
type azureLock struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Properties struct {
		Level string `json:"level"`
	} `json:"properties"`
}

// azureDeny is a deny assignment.
type azureDeny struct {
	ID         string `json:"id"`
	Properties struct {
		Name        string            `json:"denyAssignmentName"`
		Permissions []azurePermission `json:"permissions"`
		Scope       string            `json:"scope"`
		NotChildren bool              `json:"doNotApplyToChildScopes"`
	} `json:"properties"`
}

// under reports whether path is scope or under it, in any letter case.
func under(path, scope string) bool {
	path, scope = strings.ToLower(strings.TrimRight(path, "/")), strings.ToLower(strings.TrimRight(scope, "/"))
	return path == scope || strings.HasPrefix(path, scope+"/")
}

// lockOn is what a lock is on: its id less the lock's own part.
func lockOn(id string) string {
	if i := strings.Index(strings.ToLower(id), strings.ToLower(azureLocksPath)+"/"); i >= 0 {
		return id[:i]
	}
	return ""
}

// lockApplies says whether a lock applies to what is written in a scope:
// one on the scope or above it, or on a resource under it serve may write
// (of a type written there, its RG/NAME one --changes-in names); and
// whether it is on what may be deleted (a metric alert).
func (sc azureScope) lockApplies(on string) (applies, deletable bool) {
	if under(sc.path, on) {
		return true, sc.deletes
	}
	m := azureResource.FindStringSubmatch(on)
	if m == nil || !under(on, sc.path) || m[3] == "" {
		return false, false
	}
	segs := strings.Split(strings.Trim(m[4], "/"), "/")
	if len(segs) != 2 {
		return false, false
	}
	typ := segs[0]
	if !slices.ContainsFunc(sc.types, func(t string) bool { return strings.EqualFold(t, typ) }) {
		return false, false
	}
	if !slices.ContainsFunc(sc.pats, func(pat string) bool { return namedFold(pat, m[2]+"/"+segs[1]) }) {
		return false, false
	}
	return true, strings.EqualFold(typ, "metricAlerts")
}

// readAction is the action that reads a resource, by its id: its
// provider's types, then /read; a resource group's, Resource Manager's.
func readAction(id string) string {
	m := azureResource.FindStringSubmatch(id)
	if m == nil {
		return ""
	}
	if m[3] == "" {
		return "Microsoft.Resources/subscriptions/resourceGroups/read"
	}
	segs := strings.Split(strings.Trim(m[4], "/"), "/")
	out := m[3]
	for i := 0; i < len(segs); i += 2 {
		out += "/" + segs[i]
	}
	return out + "/read"
}

// mostIDBytes is the longest id of a resource an alert rule names that is
// asked about; a longer one is unknown, and isn't sent.
const mostIDBytes = 1024

// linkedOf are the resources an alert rule's document names beside itself:
// its targets (scopes), its action groups, a workspace, each a resource's
// id under a resource group; and odd, how many it names that aren't (a
// subscription, a management group, an id past mostIDBytes, one with a .
// or .. for a part of its path, which would ask of another resource than
// it names), which aren't asked and are unknown, never passed over.
func linkedOf(n azureNamed) (out []string, odd int) {
	var v any
	if json.Unmarshal(n.data, &v) != nil {
		return nil, 1
	}
	seen := map[string]bool{strings.ToLower(n.id): true}
	var walk func(any, int)
	walk = func(x any, depth int) {
		if depth > 32 || len(out) > 50 {
			return
		}
		switch y := x.(type) {
		case map[string]any:
			for _, z := range y {
				walk(z, depth+1)
			}
		case []any:
			for _, z := range y {
				walk(z, depth+1)
			}
		case string:
			low := strings.ToLower(y)
			if !strings.HasPrefix(low, "/subscriptions/") && !strings.HasPrefix(low, "/providers/") || seen[low] {
				return
			}
			seen[low] = true
			if len(y) > mostIDBytes || !azureResource.MatchString(y) ||
				slices.ContainsFunc(strings.Split(y, "/"), func(part string) bool { return part == "." || part == ".." }) {
				odd++
				return
			}
			out = append(out, y)
		}
	}
	walk(v, 0)
	slices.Sort(out)
	return out, odd
}

// azureWrites asks Azure, as the identity, on each scope of what serve
// writes as it: its permissions, judged against the actions those writes
// need; the deny assignments at or above it, any that may refuse one of
// them unknown (whom it applies to isn't read); and the locks on it, above
// it and on what is written under it, a ReadOnly one a problem, a
// CanNotDelete one where a metric alert may be deleted a problem. Then, for
// each alert rule --changes-in names that this run read, the resources it
// names (its targets, its action groups): Azure Monitor asks for read on
// them as a metric alert is written (alerts-create-metric-alert-rule).
func (p *run) azureWrites(ctx context.Context, w writer) Item {
	id := w.id
	scopes, named := p.azureScopes()
	var oks, problems, unknowns []string
	each, cancel := context.WithTimeout(ctx, eachTime)
	defer cancel()
	asked := 0
	subs := map[string][]azureScope{}
	// Each lock is judged once for a scope, from whichever list gives it;
	// each deny assignment once.
	seenLocks, seenDenies := map[string]bool{}, map[string]bool{}
	for _, sc := range scopes {
		if sc.none != "" {
			unknowns = append(unknowns, sc.of+": "+sc.none+", so where it writes isn't asked")
			continue
		}
		if asked++; asked > mostScopes {
			unknowns = append(unknowns, sc.of+": past the first "+strconv.Itoa(mostScopes)+" scopes, not asked")
			continue
		}
		subs[sc.host+"/subscriptions/"+sc.sub] = append(subs[sc.host+"/subscriptions/"+sc.sub], sc)
		// The subscription is --alarms's or --ruler's (hideScopes); the
		// resource group may be --changes-in's.
		if m := azureSubRG.FindStringSubmatch(strings.SplitN(sc.path, "/providers/", 2)[0]); m != nil {
			p.hideSaid("--changes-in's resource group", m[2])
		}
		where := sc.of + " on a resource group"
		if strings.Contains(sc.path, "/providers/") {
			where = sc.of + " on a rule group --changes-in names"
		}
		perms, err := p.azurePermissions(each, w, sc.host, sc.path)
		if err != nil {
			unknowns = append(unknowns, where+": Azure didn't say its permissions: "+p.cloudSaid(err))
			continue
		}
		var lacks []string
		for _, a := range sc.actions() {
			if !azureMay(perms, a) {
				lacks = append(lacks, a)
			}
		}
		switch {
		case len(lacks) == 0:
			oks = append(oks, where+": "+strings.Join(sc.actions(), ", "))
		case sc.named:
			unknowns = append(unknowns, where+": not given there "+strings.Join(lacks, ", ")+", which may be given on each alert rule named, not asked")
		default:
			problems = append(problems, where+": not given "+strings.Join(lacks, ", "))
		}
		// The deny assignments at or above it; one on an alert rule
		// itself, below the group, is asked at the rule (below).
		pr, un := p.azureDenies(each, w, sc.host, sc.path, sc.actions(), where, seenDenies)
		problems, unknowns = append(problems, pr...), append(unknowns, un...)
		if sc.unread && !strings.Contains(sc.path, "/providers/") {
			unknowns = append(unknowns, where+": an entry of --changes-in names no alert rule this run read there, so a deny assignment on one itself, "+
				"below the group, isn't asked")
		}
		// The locks on it, and on what is under it.
		pr, un = p.azureLocks(each, w, sc.host, sc.path, []azureScope{sc}, where, seenLocks)
		problems, unknowns = append(problems, pr...), append(unknowns, un...)
	}
	// A subscription's locks, which its resource groups' lists may not
	// hold: those on the subscription itself.
	for _, at := range slices.Sorted(maps.Keys(subs)) {
		scs := subs[at]
		h, _ := url.Parse(at)
		pr, un := p.azureLocks(each, w, h.Scheme+"://"+h.Host, h.Path, scs, "in the subscription's locks", seenLocks)
		problems, unknowns = append(problems, pr...), append(unknowns, un...)
	}
	// The deny assignments at each alert rule and rule group named and
	// read, below the group asked (a deployment stack's deny is on the
	// resources it manages), mostNamed at most.
	for i, n := range named {
		if i >= mostNamed {
			unknowns = append(unknowns, fmt.Sprintf("%d more alert rules named, past the first %d, whose own deny assignments aren't asked", len(named)-mostNamed, mostNamed))
			break
		}
		if len(n.id) > mostIDBytes || !azureResource.MatchString(n.id) {
			unknowns = append(unknowns, "an alert rule named whose id isn't one of a resource under a resource group: its own deny assignments aren't asked")
			continue
		}
		actions := []string{azureWriteOf[n.typ]}
		if n.typ == "metricAlerts" {
			actions = append(actions, azureMetricDelete)
		}
		p.hide("an alert rule --changes-in names", n.id)
		pr, un := p.azureDenies(each, w, "https://"+strings.ToLower(hostOf(scopes)), n.id, actions, "on an alert rule --changes-in names", seenDenies)
		problems, unknowns = append(problems, pr...), append(unknowns, un...)
	}
	// The resources the alert rules named name: read on each, asked on it.
	var linked []string
	seenLinked := map[string]bool{}
	metricLinks := map[string]bool{}
	odd := 0
	for _, n := range named {
		ls, o := linkedOf(n)
		odd += o
		for _, l := range ls {
			if k := strings.ToLower(l); !seenLinked[k] {
				seenLinked[k] = true
				linked = append(linked, l)
			}
			if n.typ == "metricAlerts" {
				metricLinks[strings.ToLower(l)] = true
			}
		}
	}
	if odd > 0 {
		unknowns = append(unknowns, plural(odd, "resource")+" the alert rules name that isn't one under a resource group (a subscription, say), whose read isn't asked")
	}
	slices.Sort(linked)
	for i, l := range linked {
		what := "a resource an alert rule --changes-in names names (a target, an action group)"
		if i >= mostLinked {
			unknowns = append(unknowns, fmt.Sprintf("%d more resources the alert rules name, past the first %d, not asked", len(linked)-mostLinked, mostLinked))
			break
		}
		m := azureResource.FindStringSubmatch(l)
		p.hide("a resource an alert rule names", l)
		p.hideSaid("its resource group", m[2])
		host := "https://" + strings.ToLower(hostOf(scopes))
		perms, err := p.azurePermissions(each, w, host, l)
		if err != nil {
			unknowns = append(unknowns, what+": Azure didn't say its permissions there: "+p.cloudSaid(err))
			continue
		}
		read := readAction(l)
		switch {
		case azureMay(perms, read):
			oks = append(oks, what+": "+read)
		case metricLinks[strings.ToLower(l)]:
			problems = append(problems, what+": not given "+read+", which Azure Monitor asks for as a metric alert naming it is written")
		default:
			unknowns = append(unknowns, what+": not given "+read+"; Azure's documentation asks for it of a metric alert's, and of this kind says nothing")
		}
	}
	return verdictOf(id, w.flag, oks, problems, unknowns, "as Azure's permissions for it say, on "+plural(min(asked, mostScopes), "scope")+
		", with no deny assignment that may refuse them, and no lock",
		"give the identity a role with these actions where --changes-in names, read on what the alert rules name, and no lock that refuses the writes (charts/lapilli/README.md has the least privilege)")
}

// hostOf is Resource Manager's host of the scopes asked.
func hostOf(scopes []azureScope) string {
	for _, sc := range scopes {
		if sc.host != "" {
			return strings.TrimPrefix(sc.host, "https://")
		}
	}
	return "management.azure.com"
}

// azureDenies reads the deny assignments at or above a scope, and says of
// each that may refuse one of actions (its actions less its notActions),
// once (seen), that it is unknown, naming it: whom it applies to isn't
// read. One that doesn't apply below its own scope is passed over below
// it.
func (p *run) azureDenies(ctx context.Context, w writer, host, path string, actions []string, where string, seen map[string]bool) (problems, unknowns []string) {
	var denies []azureDeny
	if err := p.azureList(ctx, w, host, path+azureDenyPath, azureDenyQuery, func(b []byte) error {
		var page struct {
			Value []azureDeny `json:"value"`
		}
		err := json.Unmarshal(b, &page)
		denies = append(denies, page.Value...)
		return err
	}); err != nil {
		return nil, []string{where + ": Azure didn't say its deny assignments (Microsoft.Authorization/denyAssignments/read): " + p.cloudSaid(err)}
	}
	for _, d := range denies {
		if d.Properties.NotChildren && !strings.EqualFold(strings.TrimRight(d.Properties.Scope, "/"), path) {
			continue
		}
		var hits []string
		for _, a := range actions {
			if slices.ContainsFunc(d.Properties.Permissions, func(x azurePermission) bool { return azureMay([]azurePermission{x}, a) }) {
				hits = append(hits, a)
			}
		}
		key := strings.ToLower(d.ID + "|" + d.Properties.Name)
		if len(hits) == 0 || seen[key] {
			continue
		}
		seen[key] = true
		unknowns = append(unknowns, where+": a deny assignment, "+p.cloudSaidText(d.Properties.Name)+", may refuse "+strings.Join(hits, ", ")+
			" (whom it applies to isn't read)")
	}
	return nil, unknowns
}

// azureLocks reads the locks at a scope, a resource group's or a
// subscription's (whose list holds every lock under it), and says of
// those that apply to the scopes' writes, each once (seen): a ReadOnly
// one, a problem; a CanNotDelete one where a metric alert may be deleted,
// a problem.
func (p *run) azureLocks(ctx context.Context, w writer, host, path string, scs []azureScope, where string, seen map[string]bool) (problems, unknowns []string) {
	var locks []azureLock
	if err := p.azureList(ctx, w, host, path+azureLocksPath, azureLocksQuery, func(b []byte) error {
		var page struct {
			Value []azureLock `json:"value"`
		}
		err := json.Unmarshal(b, &page)
		locks = append(locks, page.Value...)
		return err
	}); err != nil {
		return nil, []string{where + ": Azure didn't say its locks (Microsoft.Authorization/locks/read): " + p.cloudSaid(err)}
	}
	for _, l := range locks {
		on := lockOn(l.ID)
		if on == "" {
			continue
		}
		for _, sc := range scs {
			applies, deletable := sc.lockApplies(on)
			key := strings.ToLower(sc.host + sc.path + "|" + l.ID)
			switch {
			case !applies || seen[key]:
				continue
			}
			seen[key] = true
			switch {
			case strings.EqualFold(l.Properties.Level, "ReadOnly"):
				problems = append(problems, where+": a ReadOnly lock, "+p.cloudSaidText(l.Name)+", refuses every write to "+sc.of)
			case strings.EqualFold(l.Properties.Level, "CanNotDelete") && deletable:
				problems = append(problems, where+": a CanNotDelete lock, "+p.cloudSaidText(l.Name)+", refuses deleting a metric alert of "+sc.of)
			}
		}
	}
	slices.Sort(problems)
	return slices.Compact(problems), nil
}

// mostPages is how many pages of an answer are read at most.
const mostPages = 5

// azurePermissions reads the identity's permissions at a scope.
func (p *run) azurePermissions(ctx context.Context, w writer, host, path string) ([]azurePermission, error) {
	var all []azurePermission
	err := p.azureList(ctx, w, host, path+azurePermissionsPath, azurePermissionsQuery, func(b []byte) error {
		var page struct {
			Value []azurePermission `json:"value"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return fmt.Errorf("not a list of permissions: %w", err)
		}
		all = append(all, page.Value...)
		return nil
	})
	return all, err
}

// azureList reads a list of Resource Manager's, a GET of path with query,
// each page, following nextLink on the same host and path, mostPages at
// most, each allowed as it is learned.
func (p *run) azureList(ctx context.Context, w writer, host, path string, query url.Values, each func([]byte) error) error {
	h, _ := url.Parse(host)
	// The path as it reads, escaped as a URL's is: the allowlist compares
	// it as it reads.
	next := (&url.URL{Scheme: h.Scheme, Host: h.Host, Path: path, RawQuery: query.Encode()}).String()
	p.allowURLAt("iam:azure", next, true)
	for pages := 0; next != ""; pages++ {
		if pages >= mostPages {
			return fmt.Errorf("more than %d pages", mostPages)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", p.agent())
		if err := w.sign(req); err != nil {
			return err
		}
		_, b, err := ask(req)
		if err != nil {
			return err
		}
		var page struct {
			NextLink string `json:"nextLink"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return fmt.Errorf("not a list: %w", err)
		}
		if err := each(b); err != nil {
			return err
		}
		next = ""
		if page.NextLink != "" {
			u, err := url.Parse(page.NextLink)
			if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, h.Host) || u.User != nil || !strings.EqualFold(u.Path, path) {
				return fmt.Errorf("the next page is elsewhere than the first: it isn't followed")
			}
			p.allowURLAt("iam:azure", page.NextLink, true)
			next = page.NextLink
		}
	}
	return nil
}
