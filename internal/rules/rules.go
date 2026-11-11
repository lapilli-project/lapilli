// Package rules reads alert rules in a form that can be replayed against
// history: PromQL for a Prometheus rule, queries and expressions for a
// Grafana rule.
//
// It reads Prometheus rule files, PrometheusRule resources, Grafana alerting
// provisioning files, each cloud's managed Prometheus rule groups, Google
// Cloud Monitoring alerting policies, and ConfigMaps that hold any of them.
package rules

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/sse"
)

// Rule is one alert rule.
type Rule struct {
	Name string
	// Alert is the name its alerts carry (alertname) when Name was made
	// another, to tell it apart from rules of the same name; "" when it is
	// Name. AlertName gives it either way.
	Alert  string
	Group  string
	Source string // the file it came from
	// Expr, for a Prometheus rule, returns a series while the condition
	// holds.
	Expr string
	// Graph, for a Grafana rule, is its queries and expressions.
	Graph *Graph
	For   time.Duration
	// KeepFiringFor keeps an alert firing that long after its condition
	// stops holding.
	KeepFiringFor time.Duration
	// Interval is how often the rule's group is evaluated; zero when the
	// file doesn't say.
	Interval time.Duration
	// QueryOffset is how long before each evaluation a Prometheus rule's
	// query is asked at: its group's query_offset.
	QueryOffset time.Duration
	// Phase, for a rule read from an API that runs it, is where its group
	// is placed within its interval, as the group's last evaluation there
	// says, to the second: it is evaluated at times Phase past a whole
	// number of intervals since the Unix epoch, as Prometheus places a
	// group by a hash of its name and file. Zero when not known.
	Phase time.Duration
	// Late is how long its metrics are taken to arrive after their time,
	// for a replay that sees only what had arrived (--late): each
	// evaluation reads the history as it stood Late before it, when that
	// is more than QueryOffset, or a Grafana query's own window end. It is
	// how the rule is replayed, not part of the rule.
	Late time.Duration
	// Scrape is how often the history's samples were scraped
	// (--scrape-interval), for the reduces a Grafana rule's replay asks the
	// server to work out (Ask.PromQL); 0 when not known. How the rule is
	// replayed, not part of the rule.
	Scrape time.Duration
	Labels map[string]string
	// UID is a Grafana rule's uid, which stays the same when it is edited.
	UID string
	// Digest, for a Grafana rule, changes when anything in the rule does;
	// Grafana starts a rule's alerts afresh when it changes.
	Digest string
	// Unsupported says why the rule can't be replayed.
	Unsupported string
	// Failed, of a Grafana rule replayed, are its queries outside its
	// condition that failed (UnseenFailed): not the rule, whose condition
	// was worked out, but whether they returned anything isn't known.
	Failed []string
	// Cloud, for an alarm read from a cloud's own list (cloudwatch,
	// google), says so: such an alarm keeps no series of what it fired, as
	// Prometheus's ALERTS is, for a replay to be set beside.
	Cloud string
	// Says is what the rule says when it fires, as its source has it. It
	// doesn't decide when it fires: Key leaves it out.
	Says Says
	// Namespace is where a ruler keeps the rule's group, for a rule read
	// with --ruler: a Mimir, Cortex or Amazon namespace, or an Azure rule
	// group's resource group; "" for every other rule
	// (docs/design/rulers.md).
	Namespace string
	// Folder and Receiver, for a Grafana rule, are its folder's title, its
	// alerts' grafana_folder, and the contact point it notifies itself
	// (notification_settings), "" when the notification policies route it
	// (docs/design/serveops.md). Neither decides when it fires.
	Folder, Receiver string
}

// Says is what a rule says when it fires: its summary, description and
// runbook, as its source has them, read so they can be checked
// (docs/design/content.md). Each is cut to maxSays bytes.
type Says struct {
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`
	Runbook     string `json:"runbook,omitempty"`
	// Template is the language Summary and Description are written in:
	// TemplatePrometheus, TemplateGrafana, TemplateGoogle, or "" for
	// plain text.
	Template string `json:"template,omitempty"`
	// Severity is the source's own severity field, apart from the label
	// severity, when it has one.
	Severity string `json:"severity,omitempty"`
	// NoSeverity says the source has no severity at all: CloudWatch's
	// alarms, Azure's activity log alerts. One missing there isn't a fault.
	NoSeverity bool `json:"no_severity,omitempty"`
	// TextUnknown says what the rule was read from doesn't carry its
	// description, nor so a runbook in it: OCI's alarm list has no body.
	// Neither is known, so neither missing is a fault.
	TextUnknown bool `json:"text_unknown,omitempty"`
}

// The languages a rule's text is written in.
const (
	TemplatePrometheus = "prometheus" // Go templates with Prometheus's functions
	TemplateGrafana    = "grafana"    // Go templates with Grafana's
	TemplateGoogle     = "google"     // ${...} variables, as Cloud Monitoring documents them
)

// maxSays is how much of each text is kept: what a rule says comes from
// whoever writes the rules, and is shown on pages and to agents.
const maxSays = 4 << 10

// cut keeps the first maxSays bytes of a text, as valid UTF-8.
func cut(s string) string {
	if len(s) <= maxSays {
		return s
	}
	return strings.ToValidUTF8(s[:maxSays], "")
}

// annotated reads what a rule says from its annotations, as Prometheus,
// Grafana and the managed services that run Prometheus's rules write them.
func annotated(v any, template string) Says {
	a := labels(v)
	runbook := a["runbook_url"]
	if runbook == "" {
		runbook = a["runbook"]
	}
	return Says{Summary: cut(a["summary"]), Description: cut(a["description"]), Runbook: cut(runbook), Template: template}
}

// urlIn is a link written into plain text.
var urlIn = regexp.MustCompile(`https?://[^\s<>"'()\[\]]+`)

// described reads what a cloud's alarm says: its name, its description, and
// the first link in it.
func described(name, description string) Says {
	return Says{Summary: cut(name), Description: cut(description), Runbook: cut(urlIn.FindString(description))}
}

// Paused is the Unsupported of a rule paused in Grafana, which fires
// nothing.
const Paused = "paused"

// AlertName is the name the rule's alerts carry, alertname: its name as
// written, before any to tell it apart.
func (r Rule) AlertName() string {
	if r.Alert != "" {
		return r.Alert
	}
	return r.Name
}

// Key is all of a rule that decides when it fires and how it is counted:
// two rules with the same key fire alike, whichever file they came from.
// A rule's namespace and its group's query_offset are in it only when it
// has them, so every rule read otherwise keeps the key it had.
func (r Rule) Key() string {
	k := fmt.Sprintf("%q %q %q %d %d %d %q %q %q %q", r.Name, r.Group, r.Expr, r.For, r.KeepFiringFor, r.Interval,
		sse.LabelKey(r.Labels), r.UID, r.Digest, r.Unsupported)
	if r.Namespace != "" {
		k += fmt.Sprintf(" %q", r.Namespace)
	}
	if r.QueryOffset != 0 {
		k += fmt.Sprintf(" offset %d", r.QueryOffset)
	}
	return k
}

// What a replay of a rule can't see, as Unseen says.
const (
	// UnseenLate is a rule on a cloud's own monitoring, replayed without
	// --late: a cloud's metrics often arrive minutes after their time, and
	// the replay reads them as filled in since.
	UnseenLate = "late"
	// UnseenOther is a Grafana rule with a query on a data source its
	// condition doesn't read (Graph.Other), which isn't asked: Grafana
	// takes the rule as having no data whenever it returns nothing.
	UnseenOther = "other-source"
	// UnseenFailed is a Grafana rule a query of which, outside its
	// condition, failed when it was replayed (Failed): whether it returned
	// anything, which Grafana's no data hangs on, isn't known then.
	UnseenFailed = "query-failed"
)

// Unseen is what a replay of the rule can't see, which may have made it
// fire otherwise than it did: UnseenLate. A Grafana rule's query errors
// aren't replayed either, but nearly every Grafana rule fires on them
// (execErrState Alerting by default), so saying so of each would say
// nothing: the check of the replay counts them where they happened.
func (r Rule) Unseen() []string {
	var out []string
	if r.Cloud != "" && r.Late == 0 && r.Unsupported == "" {
		out = append(out, UnseenLate)
	}
	if r.Graph != nil && len(r.Graph.Other) > 0 && r.Unsupported == "" {
		out = append(out, UnseenOther)
	}
	if len(r.Failed) > 0 && r.Unsupported == "" {
		out = append(out, UnseenFailed)
	}
	return out
}

// Lag is how long before an evaluation a Prometheus rule's query reads the
// history: its query_offset, or how late its metrics arrive, whichever is
// longer.
func (r Rule) Lag() time.Duration { return max(r.QueryOffset, r.Late) }

// ErrNoneInDir is a directory with no file of alert rules in it.
var ErrNoneInDir = errors.New("no file in it holds alert rules")

// errNoRules is a document with no alert rules in it.
var errNoRules = errors.New("no alert rules: want a Prometheus rule file, a PrometheusRule, a Grafana alerting provisioning file, a cloud's rule groups or alerting policies, or a ConfigMap holding one")

// Load reads every alert rule in a file, or in the files of a directory and
// those below it, such as a ConfigMap mounted in a pod. In a directory,
// hidden files and directories, files without alert rules, and files below
// the top that don't parse are passed over; links are followed to files but
// not to directories below.
func Load(path string) ([]Rule, error) {
	rs, _, err := Read(path)
	return rs, err
}

// Doc is a document alert rules were read from, kept so they can be read
// again, as they were, with FromDocs.
type Doc struct {
	Source string `json:"source"`
	// API says Data is an answer from GET /api/v1/rules, not a file.
	API  bool   `json:"api,omitempty"`
	Data []byte `json:"data"`
	// Namespace is the ruler's namespace, or the Azure rule group's
	// resource group, the document is: each rule read from it has it.
	Namespace string `json:"namespace,omitempty"`
}

// FromDocs reads the rules in documents kept from Read, FromAPI or a
// Grafana's export, each rule's Source being its document's.
func FromDocs(docs []Doc) ([]Rule, error) {
	var out []Rule
	for _, d := range docs {
		var rs []Rule
		var err error
		if d.API {
			rs, err = FromAPI(d.Data)
		} else {
			rs, err = Parse(d.Data)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Source, err)
		}
		for i := range rs {
			rs[i].Source, rs[i].Namespace = d.Source, d.Namespace
		}
		out = append(out, rs...)
	}
	return out, nil
}

// Read is Load, and the documents the rules came from.
func Read(path string) ([]Rule, []Doc, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if !fi.IsDir() {
		rs, d, err := load(path)
		if err != nil {
			return nil, nil, err
		}
		return rs, []Doc{d}, nil
	}
	// The directory may be a link to one: a trailing / walks where it leads.
	walk := path
	if l, err := os.Lstat(path); err == nil && l.Mode()&os.ModeSymlink != 0 {
		walk = path + string(filepath.Separator)
	}
	root := filepath.Clean(path)
	var out []Rule
	var docs []Doc
	err = filepath.WalkDir(walk, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// A mounted ConfigMap holds its keys as links into ..data.
		if filepath.Clean(p) != root && strings.HasPrefix(e.Name(), ".") {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.IsDir() {
			return nil
		}
		if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
			return nil
		}
		rs, d, err := load(p)
		switch {
		case errors.Is(err, errNoRules):
			return nil
		case err != nil && filepath.Dir(p) != root:
			// Below the top, files that aren't YAML, such as a Helm
			// chart's templates, are passed over.
			return nil
		case err != nil:
			return err
		}
		out = append(out, rs...)
		docs = append(docs, d)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("%s: %w", path, ErrNoneInDir)
	}
	return out, docs, nil
}

func load(path string) ([]Rule, Doc, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, Doc{}, err
	}
	rs, err := Parse(b)
	if err != nil {
		return nil, Doc{}, fmt.Errorf("%s: %w", path, err)
	}
	for i := range rs {
		rs[i].Source = path
	}
	return rs, Doc{Source: path, Data: b}, nil
}

// Parse reads every alert rule in a document, which may hold several YAML
// documents.
func Parse(b []byte) ([]Rule, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	var out []Rule
	found := false
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		// A list of documents, as a cloud's CLI lists resources.
		docs := []any{doc}
		if l, ok := doc.([]any); ok {
			docs = l
		}
		for _, d := range docs {
			m, _ := d.(map[string]any)
			rs, ok, err := parseDoc(m)
			if err != nil {
				return nil, err
			}
			found = found || ok
			out = append(out, rs...)
		}
	}
	if !found {
		return nil, errNoRules
	}
	return out, nil
}

func parseDoc(doc map[string]any) ([]Rule, bool, error) {
	switch {
	case doc == nil:
		return nil, false, nil
	case doc["kind"] == "ConfigMap":
		var out []Rule
		found := false
		data, _ := doc["data"].(map[string]any)
		for _, k := range sortedKeys(data) {
			s, _ := data[k].(string)
			rs, err := Parse([]byte(s))
			if err != nil {
				continue // not every key holds rules
			}
			found = true
			out = append(out, rs...)
		}
		return out, found, nil
	case doc["kind"] == "List":
		// kubectl get ... -o yaml
		var out []Rule
		found := false
		for _, it := range list(doc["items"]) {
			m, _ := it.(map[string]any)
			rs, ok, err := parseDoc(m)
			if err != nil {
				return nil, true, err
			}
			found = found || ok
			out = append(out, rs...)
		}
		return out, found, nil
	case doc["kind"] == "PrometheusRule":
		spec, _ := doc["spec"].(map[string]any)
		rs, err := prometheus(spec)
		return rs, true, err
	case strings.HasPrefix(str(doc["apiVersion"]), "monitoring.googleapis.com/") &&
		(doc["kind"] == "Rules" || doc["kind"] == "ClusterRules" || doc["kind"] == "GlobalRules"):
		// Google Cloud Managed Service for Prometheus: spec.groups as
		// Prometheus writes them. Rules see only their namespace's data.
		spec, _ := doc["spec"].(map[string]any)
		rs, err := prometheus(spec)
		meta, _ := doc["metadata"].(map[string]any)
		if ns := str(meta["namespace"]); doc["kind"] == "Rules" && ns != "" {
			for i := range rs {
				if rs[i].Expr, err = Scope(rs[i].Expr, map[string]string{"namespace": ns}); err != nil {
					rs[i].Unsupported = err.Error()
				}
			}
			err = nil
		}
		return rs, true, err
	case strings.EqualFold(str(doc["type"]), azureRuleGroup):
		rs, err := azure(doc)
		return rs, true, err
	case azureAlertKinds[strings.ToLower(str(doc["type"]))] != "":
		// az monitor metrics alert list, scheduled-query list,
		// activity-log alert list: one alert rule each.
		rule, ok := azureAlert(doc)
		if !ok {
			return nil, true, nil // no name: nothing to tie its firings to
		}
		return []Rule{rule}, true, nil
	case isOCIAlarms(doc):
		// oci monitoring alarm list
		return ociAlarms(doc), true, nil
	case doc["resources"] != nil && slices.ContainsFunc(list(doc["resources"]), func(r any) bool {
		m, _ := r.(map[string]any)
		return strings.EqualFold(str(m["type"]), azureRuleGroup)
	}):
		// An ARM template holding rule groups.
		var out []Rule
		for _, r := range list(doc["resources"]) {
			m, _ := r.(map[string]any)
			if strings.EqualFold(str(m["type"]), azureRuleGroup) {
				rs, err := azure(m)
				if err != nil {
					return nil, true, err
				}
				out = append(out, rs...)
			}
		}
		return out, true, nil
	case doc["alertPolicies"] != nil && doc["conditions"] == nil:
		// projects.alertPolicies.list's answer.
		var out []Rule
		for _, p := range list(doc["alertPolicies"]) {
			m, _ := p.(map[string]any)
			out = append(out, googlePolicy(m)...)
		}
		return out, true, nil
	case isGooglePolicy(doc) && doc["groups"] == nil:
		return googlePolicy(doc), true, nil
	case (doc["MetricAlarms"] != nil || doc["CompositeAlarms"] != nil) && doc["groups"] == nil:
		// aws cloudwatch describe-alarms
		rs, err := cloudWatch(doc)
		return rs, true, err
	case doc["ruleGroupsNamespace"] != nil:
		// Amazon Managed Service for Prometheus: the rule file a rule
		// groups namespace holds, base64 in the AWS CLI's answer.
		ns, _ := doc["ruleGroupsNamespace"].(map[string]any)
		b, err := base64.StdEncoding.DecodeString(str(ns["data"]))
		if err != nil {
			return nil, true, fmt.Errorf("rule groups namespace %s: %w", str(ns["name"]), err)
		}
		rs, err := Parse(b)
		if err != nil {
			return nil, true, fmt.Errorf("rule groups namespace %s: %w", str(ns["name"]), err)
		}
		return rs, true, nil
	case doc["apiVersion"] != nil && hasGrafanaRules(doc):
		rs, err := grafana(doc)
		return rs, true, err
	case doc["groups"] != nil:
		rs, err := prometheus(doc)
		return rs, true, err
	}
	return nil, false, nil
}

// prometheus reads groups of Prometheus rules; recording rules are skipped.
func prometheus(doc map[string]any) ([]Rule, error) {
	var out []Rule
	for _, g := range list(doc["groups"]) {
		gm, _ := g.(map[string]any)
		group := str(gm["name"])
		interval, err := duration(gm["interval"])
		if err != nil {
			return nil, fmt.Errorf("group %s: %w", group, err)
		}
		offset, err := duration(gm["query_offset"])
		if err != nil {
			return nil, fmt.Errorf("group %s: %w", group, err)
		}
		for _, r := range list(gm["rules"]) {
			rm, _ := r.(map[string]any)
			name := str(rm["alert"])
			if name == "" {
				continue
			}
			rule := Rule{Name: name, Group: group, Expr: strings.TrimSpace(str(rm["expr"])), Labels: labels(rm["labels"]), Interval: interval,
				QueryOffset: offset, Says: annotated(rm["annotations"], TemplatePrometheus)}
			if rule.For, err = duration(rm["for"]); err != nil {
				return nil, fmt.Errorf("rule %s: %w", name, err)
			}
			if rule.KeepFiringFor, err = duration(rm["keep_firing_for"]); err != nil {
				return nil, fmt.Errorf("rule %s: %w", name, err)
			}
			out = append(out, rule)
		}
	}
	return out, nil
}

// azureRuleGroup is the Azure resource type of a group of Prometheus rules.
const azureRuleGroup = "Microsoft.AlertsManagement/prometheusRuleGroups"

// azure reads an Azure Monitor managed service for Prometheus rule group.
// A rule's severity, 0 to 4, becomes the label severity=Sev0 to Sev4 unless
// it has one; its resolve configuration's timeToResolve is taken as how
// long it stays firing once its condition stops holding.
func azure(doc map[string]any) ([]Rule, error) {
	props, _ := doc["properties"].(map[string]any)
	group := str(doc["name"])
	interval, err := isoDuration(props["interval"])
	if err != nil {
		return nil, fmt.Errorf("rule group %s: %w", group, err)
	}
	groupOff := props["enabled"] == false
	var out []Rule
	for _, r := range list(props["rules"]) {
		rm, _ := r.(map[string]any)
		name := str(rm["alert"])
		if name == "" {
			continue
		}
		rule := Rule{Name: name, Group: group, Expr: strings.TrimSpace(str(rm["expression"])), Labels: labels(rm["labels"]), Interval: interval,
			Says: annotated(rm["annotations"], TemplatePrometheus)}
		if rule.For, err = isoDuration(rm["for"]); err != nil {
			return nil, fmt.Errorf("rule %s: %w", name, err)
		}
		if rc, ok := rm["resolveConfiguration"].(map[string]any); ok {
			if rule.KeepFiringFor, err = isoDuration(rc["timeToResolve"]); err != nil {
				return nil, fmt.Errorf("rule %s: %w", name, err)
			}
			if rc["autoResolved"] == false {
				rule.KeepFiringFor = never // resolved by hand, if at all
			}
		}
		if c := str(props["clusterName"]); c != "" && rule.Unsupported == "" {
			if rule.Expr, err = Scope(rule.Expr, map[string]string{"cluster": c}); err != nil {
				rule.Unsupported = err.Error()
			}
		}
		if sev := azureSeverity(rm["severity"]); sev != "" && rule.Labels["severity"] == "" {
			rule.Labels["severity"] = sev
		}
		if groupOff || rm["enabled"] == false {
			rule.Unsupported = Disabled
		}
		out = append(out, rule)
	}
	return out, nil
}

// azureAlertKinds are Azure Monitor's own alert rules, by resource type, in
// words: written in Azure Monitor's terms, not PromQL, so listed, not
// replayed, their firings judged from their history (recipes/firings).
var azureAlertKinds = map[string]string{
	"microsoft.insights/metricalerts":        "metric alert",
	"microsoft.insights/scheduledqueryrules": "log search alert",
	"microsoft.insights/activitylogalerts":   "activity log alert",
}

// azureSeverity is Azure Monitor's severity, 0 to 4, as Sev0 to Sev4; ""
// for anything else, which Azure doesn't give.
func azureSeverity(v any) string {
	f, ok := toFloat(v)
	if !ok || f != math.Trunc(f) || f < 0 || f > 4 {
		return ""
	}
	return fmt.Sprintf("Sev%d", int(f))
}

// azureAlert reads one of Azure Monitor's own alert rules, as the Azure
// CLI lists it (flat) or the API answers (under properties). Its name is
// the resource's, as recipes/firings/azure-monitor.jq names its firings'
// rule; its severity, 0 to 4, the label severity=Sev0 to Sev4. ok is false
// for one with no name, which is passed over, as every source's is.
func azureAlert(doc map[string]any) (rule Rule, ok bool) {
	props, ok := doc["properties"].(map[string]any)
	if !ok {
		props = doc
	}
	kind := azureAlertKinds[strings.ToLower(str(doc["type"]))]
	name := str(doc["name"])
	if name == "" {
		return Rule{}, false
	}
	summary := str(props["displayName"])
	if summary == "" {
		summary = name
	}
	// The resource group as the document has it: the Azure CLI writes it,
	// and --alarms writes it into what it reads, in lower case; a saved
	// API answer has none, and keeps the key it had.
	rule = Rule{Name: name, Group: str(doc["resourceGroup"]), Labels: map[string]string{}, Cloud: "azure",
		Unsupported: "an Azure Monitor " + kind + " isn't PromQL", Says: described(summary, str(props["description"]))}
	if kind == "metric alert" {
		azurePromQL(&rule, props)
	}
	if sev := azureSeverity(props["severity"]); sev != "" && kind != "activity log alert" {
		rule.Labels["severity"], rule.Says.Severity = sev, sev
	}
	rule.Says.NoSeverity = kind == "activity log alert"
	if props["enabled"] == false {
		rule.Unsupported = Disabled
	}
	return rule, true
}

// azureAlertID is an Azure alert rule's resource ID, and its resource
// group.
var azureAlertID = regexp.MustCompile(`(?i)^/subscriptions/[^/]+/resourceGroups/([^/]+)/providers/Microsoft\.Insights/(?:metricAlerts|scheduledQueryRules|activityLogAlerts)/[^/]+$`)

// AzurePromQL is the odata.type of a metric alert's criteria in PromQL.
const AzurePromQL = "Microsoft.Azure.Monitor.PromQLCriteria"

// azurePromQL reads a metric alert whose criteria is PromQL, with one
// static criterion (the API's 2026-01-01): a rule like a Prometheus one,
// its query, failingPeriods.for as its for, evaluationFrequency as its
// interval, and its resolve configuration as for Azure's rule groups. A
// dynamic threshold, or several criteria (all must hold, over queries that
// can't be joined), is listed, with why.
func azurePromQL(rule *Rule, props map[string]any) {
	crit, _ := props["criteria"].(map[string]any)
	if str(crit["odata.type"]) != AzurePromQL {
		return
	}
	all := list(crit["allOf"])
	c, _ := firstOf(all).(map[string]any)
	switch {
	case len(all) != 1:
		rule.Unsupported = "an Azure Monitor metric alert in PromQL with several criteria, all of which must hold, isn't replayed"
		return
	case str(c["criterionType"]) != "StaticThresholdCriterion":
		rule.Unsupported = "an Azure Monitor metric alert in PromQL with a dynamic threshold isn't replayed"
		return
	}
	rule.Unsupported, rule.Expr = "", strings.TrimSpace(str(c["query"]))
	fp, _ := crit["failingPeriods"].(map[string]any)
	var err error
	if rule.For, err = isoDuration(fp["for"]); err != nil {
		rule.Unsupported = err.Error()
	}
	if rule.Interval, err = isoDuration(props["evaluationFrequency"]); err != nil {
		rule.Unsupported = err.Error()
	}
	if rc, ok := props["resolveConfiguration"].(map[string]any); ok {
		if rule.KeepFiringFor, err = isoDuration(rc["timeToResolve"]); err != nil {
			rule.Unsupported = err.Error()
		}
		if rc["autoResolved"] == false {
			rule.KeepFiringFor = never // resolved by hand, if at all
		}
	}
	if rule.Expr == "" {
		rule.Unsupported = "no query"
	}
}

func firstOf(l []any) any {
	if len(l) == 0 {
		return nil
	}
	return l[0]
}

// isOCIAlarms reports whether a document is OCI Monitoring's alarms, as the
// OCI CLI's alarm list answers.
func isOCIAlarms(doc map[string]any) bool {
	data := list(doc["data"])
	if len(data) == 0 {
		return false
	}
	m, _ := data[0].(map[string]any)
	return strings.HasPrefix(str(m["id"]), "ocid1.alarm.")
}

// ociAlarms reads OCI Monitoring's alarms: written in MQL, not PromQL, so
// listed, not replayed. An alarm's name is its display name, as
// recipes/firings/oci.jq names its firings' rule; its severity (critical,
// error, warning, info), the label severity; its body, what it says. The
// list oci monitoring alarm list gives has no body: what such an alarm says
// isn't known, and nothing is faulted for it.
func ociAlarms(doc map[string]any) []Rule {
	camel := func(k string) string {
		// The CLI writes keys with dashes, the API in camel case.
		parts := strings.Split(k, "-")
		for i := 1; i < len(parts); i++ {
			if parts[i] != "" {
				parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
			}
		}
		return strings.Join(parts, "")
	}
	field := func(m map[string]any, k string) string {
		if v := str(m[k]); v != "" {
			return v
		}
		return str(m[camel(k)])
	}
	var out []Rule
	for _, a := range list(doc["data"]) {
		m, _ := a.(map[string]any)
		name := field(m, "display-name")
		if name == "" {
			continue
		}
		rule := Rule{Name: name, Group: field(m, "namespace"), Labels: map[string]string{}, Cloud: "oci",
			Unsupported: "an OCI alarm is written in MQL, not PromQL", Says: described(name, field(m, "body"))}
		_, hasBody := m["body"]
		rule.Says.TextUnknown = !hasBody
		if sev := strings.ToLower(field(m, "severity")); sev != "" {
			rule.Labels["severity"], rule.Says.Severity = sev, sev
		}
		if m["is-enabled"] == false || m["isEnabled"] == false {
			rule.Unsupported = Disabled
		}
		out = append(out, rule)
	}
	return out
}

// Disabled is the Unsupported of a rule turned off, which fires nothing.
const Disabled = "disabled"

// cloudWatch reads Amazon CloudWatch alarms as the AWS CLI's
// describe-alarms answers. A PromQL alarm is a rule like a Prometheus one:
// its query, its pending period as "for", its recovery period as
// keep_firing_for (CloudWatch's own words for both), and its evaluation
// interval. Other alarms watch CloudWatch metrics in CloudWatch's own terms,
// or other alarms, and can't be replayed; they are listed, with why.
func cloudWatch(doc map[string]any) ([]Rule, error) {
	var out []Rule
	for _, a := range list(doc["MetricAlarms"]) {
		m, _ := a.(map[string]any)
		name := str(m["AlarmName"])
		if name == "" {
			continue
		}
		rule := Rule{Name: name, Labels: map[string]string{}, Cloud: "cloudwatch", Says: described(name, str(m["AlarmDescription"]))}
		rule.Says.NoSeverity = true
		crit, _ := m["EvaluationCriteria"].(map[string]any)
		pq, ok := crit["PromQLCriteria"].(map[string]any)
		if !ok {
			rule.Unsupported = "a CloudWatch metric alarm isn't PromQL"
			actionsOff(&rule, m)
			out = append(out, rule)
			continue
		}
		rule.Expr = strings.TrimSpace(str(pq["Query"]))
		var err error
		for _, f := range []struct {
			to *time.Duration
			v  any
		}{{&rule.For, pq["PendingPeriod"]}, {&rule.KeepFiringFor, pq["RecoveryPeriod"]}, {&rule.Interval, m["EvaluationInterval"]}} {
			if *f.to, err = seconds(f.v); err != nil {
				return nil, fmt.Errorf("alarm %s: %w", name, err)
			}
		}
		if rule.Expr == "" {
			rule.Unsupported = "no query"
		}
		actionsOff(&rule, m)
		out = append(out, rule)
	}
	for _, a := range list(doc["CompositeAlarms"]) {
		m, _ := a.(map[string]any)
		if name := str(m["AlarmName"]); name != "" {
			says := described(name, str(m["AlarmDescription"]))
			says.NoSeverity = true
			rule := Rule{Name: name, Labels: map[string]string{}, Unsupported: "a CloudWatch composite alarm watches other alarms, not a query",
				Cloud: "cloudwatch", Says: says}
			actionsOff(&rule, m)
			out = append(out, rule)
		}
	}
	return out, nil
}

// actionsOff reads a CloudWatch alarm whose actions are off as off, as an
// Azure alert rule with enabled false is: it is still evaluated, and tells
// no one.
func actionsOff(rule *Rule, m map[string]any) {
	if m["ActionsEnabled"] == false {
		rule.Unsupported = Disabled
	}
}

// seconds reads a number of seconds, as AWS writes periods.
func seconds(v any) (time.Duration, error) {
	if v == nil {
		return 0, nil
	}
	f, ok := toFloat(v)
	if !ok || !(f >= 0) || f > never.Seconds() { // NaN too
		return 0, fmt.Errorf("unreadable number of seconds %v", v)
	}
	return time.Duration(f * float64(time.Second)), nil
}

// googleInterval is how often Google Cloud evaluates a PromQL condition
// that doesn't say.
const googleInterval = 30 * time.Second

// googleClose is when Google Cloud closes an alert from a PromQL condition:
// the larger of 270 seconds and twice the evaluation interval after the
// condition last held, whatever the policy's autoclose says. As
// keep_firing_for, that is less one evaluation, after which the alert would
// have resolved anyway.
func googleClose(interval time.Duration) time.Duration {
	return max(270*time.Second, 2*interval) - interval
}

// googleConditions are the kinds of condition a Google Cloud Monitoring
// alerting policy can hold.
var googleConditions = []string{"conditionPrometheusQueryLanguage", "conditionThreshold", "conditionAbsent",
	"conditionMatchedLog", "conditionMonitoringQueryLanguage", "conditionSql"}

// isGooglePolicy reports whether a document is a Google Cloud Monitoring
// alerting policy, as the API and gcloud write one.
func isGooglePolicy(doc map[string]any) bool {
	for _, c := range list(doc["conditions"]) {
		m, _ := c.(map[string]any)
		for _, k := range googleConditions {
			if m[k] != nil {
				return true
			}
		}
	}
	return false
}

// googlePolicy reads a Google Cloud Monitoring alerting policy. A condition
// in PromQL is a rule like a Prometheus one: its query, its duration as
// "for", its evaluation interval, its labels; the policy's severity becomes
// the label severity unless it has one. Other conditions are written in
// Cloud Monitoring's own terms and can't be replayed; they are listed, with
// why.
func googlePolicy(doc map[string]any) []Rule {
	display := str(doc["displayName"])
	if display == "" {
		display = str(doc["name"])
	}
	sev := strings.ToLower(str(doc["severity"]))
	if sev == "severity_unspecified" {
		sev = ""
	}
	// What the policy says: its documentation's subject and content, and its
	// first link, or else a link in the content.
	documentation, _ := doc["documentation"].(map[string]any)
	says := Says{Summary: cut(str(documentation["subject"])), Description: cut(str(documentation["content"])), Template: TemplateGoogle, Severity: sev}
	for _, l := range list(documentation["links"]) {
		if lm, _ := l.(map[string]any); str(lm["url"]) != "" {
			says.Runbook = cut(str(lm["url"]))
			break
		}
	}
	if says.Runbook == "" {
		says.Runbook = cut(urlIn.FindString(says.Description))
	}
	var out []Rule
	for _, c := range list(doc["conditions"]) {
		cm, _ := c.(map[string]any)
		pq, ok := cm["conditionPrometheusQueryLanguage"].(map[string]any)
		if !ok {
			kind := "unknown"
			for _, k := range googleConditions {
				if cm[k] != nil {
					kind = k
					break
				}
			}
			name := display
			if d := str(cm["displayName"]); d != "" && d != display {
				name = display + ": " + d
			}
			rule := Rule{Name: name, Group: display, Labels: map[string]string{},
				Unsupported: "a Google Cloud " + kind + " isn't PromQL", Cloud: "google", Says: says}
			if doc["enabled"] == false {
				rule.Unsupported = Disabled
			}
			out = append(out, rule)
			continue
		}
		rule := Rule{Name: str(pq["alertRule"]), Group: str(pq["ruleGroup"]), Expr: strings.TrimSpace(str(pq["query"])),
			Labels: labels(pq["labels"]), Interval: googleInterval, Cloud: "google", Says: says}
		if rule.Name == "" {
			rule.Name = display
		}
		if rule.Group == "" {
			rule.Group = display
		}
		var err error
		if rule.For, err = duration(pq["duration"]); err != nil {
			rule.Unsupported = err.Error()
		}
		if v := pq["evaluationInterval"]; v != nil {
			if rule.Interval, err = duration(v); err != nil || rule.Interval <= 0 {
				rule.Interval = googleInterval
				if err != nil {
					rule.Unsupported = err.Error()
				}
			}
		}
		rule.KeepFiringFor = googleClose(rule.Interval)
		if sev != "" && rule.Labels["severity"] == "" {
			rule.Labels["severity"] = sev
		}
		switch {
		case rule.Expr == "":
			rule.Unsupported = "no query"
		case doc["enabled"] == false:
			rule.Unsupported = Disabled
		}
		out = append(out, rule)
	}
	return out
}

// never is how long an alert that never resolves on its own keeps firing,
// and the longest duration read: a hundred years, which nothing waits out.
const never = 100 * 365 * 24 * time.Hour

var isoDur = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)

// isoDuration reads an ISO 8601 duration of days, hours, minutes and
// seconds, as Azure writes them (PT5M, P1DT2H), or a Prometheus one.
// ISODurationOf reads an ISO 8601 duration, such as PT5M.
func ISODurationOf(s string) (time.Duration, error) { return isoDuration(s) }

func isoDuration(v any) (time.Duration, error) {
	s := strings.ToUpper(str(v))
	if !strings.HasPrefix(s, "P") {
		return duration(v)
	}
	m := isoDur.FindStringSubmatch(s)
	if m == nil || s == "P" || s == "PT" {
		return 0, fmt.Errorf("unreadable duration %q", str(v))
	}
	var d float64
	for i, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second} {
		if m[i+1] != "" {
			n, _ := strconv.ParseFloat(m[i+1], 64)
			d += n * float64(unit)
		}
	}
	if d > float64(never) {
		return 0, fmt.Errorf("duration %q out of range", str(v))
	}
	return time.Duration(d), nil
}

// FromAPI reads the alert rules a Prometheus-compatible API has loaded, as
// GET /api/v1/rules answers: Prometheus, Thanos, Mimir, VictoriaMetrics'
// vmalert, Amazon Managed Service for Prometheus.
func FromAPI(b []byte) ([]Rule, error) {
	var r struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Groups []struct {
				Name           string    `json:"name"`
				Interval       float64   `json:"interval"`
				LastEvaluation time.Time `json:"lastEvaluation"`
				Rules          []struct {
					Type          string            `json:"type"`
					Name          string            `json:"name"`
					Query         string            `json:"query"`
					Duration      float64           `json:"duration"`
					KeepFiringFor float64           `json:"keepFiringFor"`
					Labels        map[string]string `json:"labels"`
					Annotations   map[string]string `json:"annotations"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("not a rules API answer: %w", err)
	}
	if r.Status != "success" {
		msg := strings.Map(func(c rune) rune {
			if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
				return -1
			}
			return c
		}, r.Error)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("rules API: %s", msg)
	}
	secs := func(f float64) time.Duration {
		if !(f > 0) { // NaN too
			return 0
		}
		return time.Duration(min(f, never.Seconds()) * float64(time.Second))
	}
	var out []Rule
	for _, g := range r.Data.Groups {
		// Where the group falls in its interval, from when it last ran: an
		// evaluation starts just after its time, so this is to the second
		// below. Before its first evaluation, Prometheus says year 1.
		var phase time.Duration
		if iv := int64(secs(g.Interval) / time.Second); iv >= 1 && g.LastEvaluation.Year() > 1970 {
			// In whole seconds: nanoseconds since 1970 overflow past 2262.
			phase = time.Duration((g.LastEvaluation.Unix()%iv+iv)%iv) * time.Second
		}
		for _, x := range g.Rules {
			if x.Type != "alerting" {
				continue
			}
			l := x.Labels
			if l == nil {
				l = map[string]string{}
			}
			a := map[string]any{}
			for k, v := range x.Annotations {
				a[k] = v
			}
			out = append(out, Rule{Name: x.Name, Group: g.Name, Source: "api", Expr: strings.TrimSpace(x.Query), For: secs(x.Duration),
				KeepFiringFor: secs(x.KeepFiringFor), Interval: secs(g.Interval), Phase: phase, Labels: l, Says: annotated(a, TemplatePrometheus)})
		}
	}
	return out, nil
}

func hasGrafanaRules(doc map[string]any) bool {
	for _, g := range list(doc["groups"]) {
		gm, _ := g.(map[string]any)
		for _, r := range list(gm["rules"]) {
			if rm, _ := r.(map[string]any); rm["data"] != nil && rm["condition"] != nil {
				return true
			}
		}
	}
	return false
}

// grafana reads a Grafana alerting provisioning file.
func grafana(doc map[string]any) ([]Rule, error) {
	var out []Rule
	for _, g := range list(doc["groups"]) {
		gm, _ := g.(map[string]any)
		group := provisioned(gm["name"])
		folder := provisioned(gm["folder"])
		if folder != "" {
			group = folder + "/" + group
		}
		interval, err := duration(provisioned(gm["interval"]))
		if err != nil {
			return nil, fmt.Errorf("group %s: %w", group, err)
		}
		for _, r := range list(gm["rules"]) {
			written, _ := r.(map[string]any)
			rm := provisionedRule(written)
			ns, _ := rm["notification_settings"].(map[string]any)
			rule := Rule{
				Name: str(rm["title"]), Group: group, Labels: labels(rm["labels"]), UID: str(rm["uid"]), Interval: interval,
				Says: annotated(rm["annotations"], TemplateGrafana), Folder: folder, Receiver: str(ns["receiver"]),
				// fmt prints maps in key order, so this is the same for the same rule.
				Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(written)))),
			}
			if rule.Name == "" {
				rule.Name = str(rm["uid"])
			}
			if rule.For, err = duration(rm["for"]); err != nil {
				return nil, fmt.Errorf("rule %s: %w", rule.Name, err)
			}
			if rule.KeepFiringFor, err = duration(rm["keepFiringFor"]); err != nil {
				return nil, fmt.Errorf("rule %s: %w", rule.Name, err)
			}
			if paused, _ := rm["isPaused"].(bool); paused {
				rule.Unsupported = Paused
			} else if rule.Graph, err = grafanaGraph(rm); err != nil {
				rule.Unsupported = err.Error()
			} else if err = rule.Graph.states(rm); err != nil {
				rule.Graph, rule.Unsupported = nil, err.Error()
			}
			out = append(out, rule)
		}
	}
	return out, nil
}

// provisioned is a value of a Grafana provisioning file as Grafana reads
// it: its file provisioning takes $$ for a $ (and its export writes every
// $ so), in the fields it interpolates. Grafana also puts environment
// variables in those fields ($VAR, ${VAR}, $__env{VAR}); Lapilli doesn't,
// as it doesn't have Grafana's environment: such a value is read as
// written.
func provisioned(v any) string { return strings.ReplaceAll(str(v), "$$", "$") }

// provisionedRule is a rule of a Grafana provisioning file with $$ read as
// $ where Grafana's file provisioning reads it so (Grafana 13.3,
// pkg/services/provisioning/alerting/rules_types.go): its title, uid,
// condition, durations, states, labels and notification settings (its
// receiver, as the content check routes it), and each query's refId and
// data source; not its annotations nor a query's model, which Grafana takes
// as written, and so doesn't escape when it exports them.
func provisionedRule(rm map[string]any) map[string]any {
	out := maps.Clone(rm)
	for _, k := range []string{"title", "uid", "condition", "for", "keepFiringFor", "noDataState", "execErrState"} {
		if v, ok := rm[k].(string); ok {
			out[k] = provisioned(v)
		}
	}
	if l, ok := rm["labels"].(map[string]any); ok {
		ls := map[string]any{}
		for k, v := range l {
			ls[k] = provisioned(v)
		}
		out["labels"] = ls
	}
	// Its notification settings: the receiver, and each of group_by and the
	// other lists.
	if ns, ok := rm["notification_settings"].(map[string]any); ok {
		set := map[string]any{}
		for k, v := range ns {
			switch v := v.(type) {
			case string:
				set[k] = provisioned(v)
			case []any:
				l := make([]any, len(v))
				for i, x := range v {
					if s, ok := x.(string); ok {
						l[i] = provisioned(s)
					} else {
						l[i] = x
					}
				}
				set[k] = l
			default:
				set[k] = v
			}
		}
		out["notification_settings"] = set
	}
	var data []any
	for _, d := range list(rm["data"]) {
		dm, ok := d.(map[string]any)
		if !ok {
			data = append(data, d)
			continue
		}
		dm = maps.Clone(dm)
		for _, k := range []string{"refId", "datasourceUid", "queryType"} {
			if v, ok := dm[k].(string); ok {
				dm[k] = provisioned(v)
			}
		}
		data = append(data, dm)
	}
	if data != nil {
		out["data"] = data
	}
	return out
}

// Graph is a Grafana rule's queries and expressions, to be evaluated
// together at each step the way Grafana evaluates them.
type Graph struct {
	// Queries maps a refId to PromQL giving that node's value at each
	// step; a reduce over a range query is folded into its PromQL.
	Queries map[string]string
	Steps   map[string]Step // refId to a threshold or math expression
	Cond    string          // the refId whose result decides firing
	// Asks is what the replay asks for besides the rule's queries, by refs
	// of their own (what a classic condition reduces each series to), and
	// in place of a reduce folded into Queries, written once the scrape
	// interval is known. They aren't the rule's, and aren't shown as its
	// queries.
	Asks map[string]Ask
	// Shift is, for a ref of Queries or Asks, how long before each
	// evaluation its query is asked at: the end of its query's
	// relativeTimeRange (to), which Grafana asks up to.
	Shift map[string]time.Duration
	// Data are the rule's data source queries by refId: the ref of Queries
	// or Asks that returns a series wherever the query returned one, and
	// its data source's uid. One that returns nothing makes the whole
	// evaluation NoData, as Grafana has it.
	Data map[string]Source
	// NoData and ExecErr are what Grafana does when a query returns nothing
	// (noDataState: NoData, Alerting, OK or KeepLast) and when one fails
	// (execErrState: Error, Alerting, OK or KeepLast), as read.
	NoData, ExecErr string
	// MissingToResolve is how many evaluations an alert whose series has
	// gone stays as it was before it resolves
	// (missing_series_evals_to_resolve, 2 when not set).
	MissingToResolve int
	// Other are the rule's queries, by refId, on a data source its
	// condition doesn't read, with that data source's uid: a Loki query
	// whose values an annotation shows. They aren't asked, as the history
	// is the condition's data source's; Grafana takes one that returns
	// nothing as no data, which the replay can't see (UnseenOther).
	Other map[string]string
}

// dataSources are the data sources a Grafana rule's condition reads, as
// the replay reads the history for them; and the expressions it doesn't
// work out, which read a query on another, and are left out, as the
// replay never evaluates them.
func dataSources(rm map[string]any, cond string) (replayed, tainted map[string]bool) {
	uses := map[string][]string{} // an expression's refs
	uid := map[string]string{}    // a data source query's data source
	for _, d := range list(rm["data"]) {
		dm, _ := d.(map[string]any)
		model, _ := dm["model"].(map[string]any)
		ref := str(dm["refId"])
		if str(dm["datasourceUid"]) != "__expr__" {
			uid[ref] = str(dm["datasourceUid"])
			continue
		}
		in := strings.TrimPrefix(strings.Trim(str(model["expression"]), "${}"), "$")
		switch str(model["type"]) {
		case "math":
			if e, err := sse.ParseMath(str(model["expression"])); err == nil {
				uses[ref] = e.Refs()
			}
		case "classic_conditions":
			for _, c := range list(model["conditions"]) {
				cm, _ := c.(map[string]any)
				qm, _ := cm["query"].(map[string]any)
				if ps := list(qm["params"]); len(ps) > 0 {
					uses[ref] = append(uses[ref], str(ps[0]))
				}
			}
		default:
			uses[ref] = []string{in}
		}
	}
	replayed = map[string]bool{}
	seen := map[string]bool{}
	var walk func(string)
	walk = func(ref string) {
		if seen[ref] {
			return
		}
		seen[ref] = true
		if u, ok := uid[ref]; ok {
			replayed[u] = true
		}
		for _, x := range uses[ref] {
			walk(x)
		}
	}
	walk(cond)
	// An expression is left out when it reads, through others or not, a
	// query on a data source not replayed.
	tainted = map[string]bool{}
	state := map[string]int{}
	var bad func(string) bool
	bad = func(ref string) bool {
		if u, ok := uid[ref]; ok {
			return !replayed[u]
		}
		if state[ref] != 0 {
			return state[ref] == 2
		}
		state[ref] = 1 // a loop is told of later, as the rule's error
		for _, x := range uses[ref] {
			if bad(x) {
				state[ref] = 2
				tainted[ref] = true
				return true
			}
		}
		state[ref] = 3
		return false
	}
	for ref := range uses {
		if !seen[ref] {
			bad(ref)
		}
	}
	return replayed, tainted
}

// Source is one of a Grafana rule's data source queries: the ref that says
// whether it returned anything, and its data source's uid.
type Source struct {
	Present string
	UID     string
}

// Expr is the PromQL the replay asks for a ref of Queries or Asks, for
// history scraped every scrape (0 when not known).
func (g *Graph) Expr(ref string, scrape time.Duration) (string, bool) {
	if a, ok := g.Asks[ref]; ok {
		return a.PromQL(scrape), true
	}
	q, ok := g.Queries[ref]
	return q, ok
}

// What Grafana does on no data and on an error.
const (
	StateNoData   = "NoData"
	StateAlerting = "Alerting"
	StateOK       = "OK"
	StateKeepLast = "KeepLast"
	StateError    = "Error"
)

// states reads what Grafana does when a query returns nothing or fails,
// with Grafana's provisioning defaults: NoData, and Alerting.
func (g *Graph) states(rm map[string]any) error {
	g.NoData, g.ExecErr, g.MissingToResolve = StateNoData, StateAlerting, 2
	if v := strings.TrimSpace(str(rm["noDataState"])); v != "" {
		if !slices.Contains([]string{StateNoData, StateAlerting, StateOK, StateKeepLast}, v) {
			return fmt.Errorf("noDataState %q", v)
		}
		g.NoData = v
	}
	if v := strings.TrimSpace(str(rm["execErrState"])); v != "" {
		if !slices.Contains([]string{StateError, StateAlerting, StateOK, StateKeepLast}, v) {
			return fmt.Errorf("execErrState %q", v)
		}
		g.ExecErr = v
	}
	for _, k := range []string{"missing_series_evals_to_resolve", "missingSeriesEvalsToResolve"} {
		if v, ok := toFloat(rm[k]); ok {
			if v < 0 || v > 1e6 || v != math.Trunc(v) {
				return fmt.Errorf("%s %v", k, rm[k])
			}
			if v > 0 {
				g.MissingToResolve = int(v)
			}
		}
	}
	return nil
}

// Step is a threshold, a math expression, a reduce over an expression, or
// classic conditions.
type Step struct {
	Threshold *Threshold
	Math      sse.Expr
	Reduce    *Reduce
	Classic   *Classic
}

// Reduce collapses an expression's values over a trailing window into one
// per series. A reduce straight over a query is folded into its PromQL
// instead.
type Reduce struct {
	Input string
	Fn    string // last, mean, max, min, sum, count
	// Window is the widest range query under Input; 0 when they are all
	// instant, and the reduce passes each value through.
	Window time.Duration
}

// Uses lists the refs a step reads.
func (s Step) Uses() []string {
	switch {
	case s.Threshold != nil:
		return []string{s.Threshold.Input}
	case s.Reduce != nil:
		return []string{s.Reduce.Input}
	case s.Math != nil:
		return s.Math.Refs()
	case s.Classic != nil:
		return s.Classic.asks()
	}
	return nil
}

// Threshold compares its input with one or two values.
type Threshold struct {
	Input string
	Type  string // gt, lt, gte, lte, within_range, outside_range, and the _included forms
	A, B  float64
}

// Pass reports whether v meets the threshold.
func (t Threshold) Pass(v float64) bool {
	switch t.Type {
	case "gt":
		return v > t.A
	case "lt":
		return v < t.A
	case "gte":
		return v >= t.A
	case "lte":
		return v <= t.A
	case "within_range":
		return v > t.A && v < t.B
	case "within_range_included":
		return v >= t.A && v <= t.B
	case "outside_range":
		return v < t.A || v > t.B
	case "outside_range_included":
		return v <= t.A || v >= t.B
	}
	return false
}

var reducers = map[string]string{
	"last": "last_over_time", "mean": "avg_over_time", "max": "max_over_time",
	"min": "min_over_time", "sum": "sum_over_time", "count": "count_over_time",
}

var thresholdTypes = map[string]int{ // how many values each takes
	"gt": 1, "lt": 1, "gte": 1, "lte": 1,
	"within_range": 2, "outside_range": 2, "within_range_included": 2, "outside_range_included": 2,
}

// grafanaGraph reads a rule's queries and expressions. It takes the shapes
// alert rules use: PromQL queries, reduce, threshold, math, and classic
// conditions.
func grafanaGraph(rm map[string]any) (*Graph, error) {
	queries := map[string]dataQuery{}
	exprs := map[string]map[string]any{}
	g := &Graph{Queries: map[string]string{}, Steps: map[string]Step{}, Cond: str(rm["condition"]),
		Asks: map[string]Ask{}, Shift: map[string]time.Duration{}, Data: map[string]Source{}}
	replayed, tainted := dataSources(rm, g.Cond)
	for _, d := range list(rm["data"]) {
		dm, _ := d.(map[string]any)
		model, _ := dm["model"].(map[string]any)
		ref := str(dm["refId"])
		if str(dm["datasourceUid"]) == "__expr__" {
			if !tainted[ref] {
				exprs[ref] = model
			}
			continue
		}
		if uid := str(dm["datasourceUid"]); !replayed[uid] {
			if g.Other == nil {
				g.Other = map[string]string{}
			}
			g.Other[ref] = uid
			continue
		}
		q := dataQuery{expr: strings.TrimSpace(str(model["expr"]))}
		if q.expr == "" {
			return nil, fmt.Errorf("query %s has no PromQL expr", ref)
		}
		var from, to float64
		if rt, ok := dm["relativeTimeRange"].(map[string]any); ok {
			from, _ = toFloat(rt["from"])
			to, _ = toFloat(rt["to"])
			if from < 0 || to < 0 || from > 1e9 || to > from {
				return nil, fmt.Errorf("query %s: relativeTimeRange from %v to %v", ref, rt["from"], rt["to"])
			}
			q.to = time.Duration(to) * time.Second
			if !truthy(model["instant"]) {
				q.window = time.Duration(from-to) * time.Second
			}
		}
		q.res = resolutionOf(model, time.Duration(from-to)*time.Second)
		q.expr = macros(q.expr, model, q.res, time.Duration(from-to)*time.Second)
		queries[ref] = q
		g.Queries[ref] = q.expr
		g.Shift[ref] = q.to
		g.Data[ref] = Source{Present: ref, UID: str(dm["datasourceUid"])}
	}
	// A data source query that returns nothing is told by what is asked
	// of it over its window: a reduce folded in, or a classic condition's
	// count, rather than its value at the evaluation alone.
	present := func(in, ref string) {
		if d := g.Data[in]; d.Present == in && queries[in].window > 0 {
			d.Present = ref
			g.Data[in] = d
		}
	}
	for _, ref := range slices.Sorted(maps.Keys(exprs)) {
		m := exprs[ref]
		in := strings.TrimPrefix(strings.Trim(str(m["expression"]), "${}"), "$")
		switch typ := str(m["type"]); typ {
		case "reduce":
			fn, ok := reducers[str(m["reducer"])]
			if !ok {
				return nil, fmt.Errorf("reducer %q", str(m["reducer"]))
			}
			q, ok := queries[in]
			if !ok {
				g.Steps[ref] = Step{Reduce: &Reduce{Input: in, Fn: str(m["reducer"])}}
				continue
			}
			if q.window > 0 {
				a := Ask{Fn: fn, Expr: q.expr, Window: q.window, Res: q.res, Counts: fn == "count_over_time" || fn == "sum_over_time"}
				if fn == "last_over_time" {
					a.Fn = "last"
				}
				g.Asks[ref] = a
				g.Queries[ref] = a.PromQL(0)
			} else {
				g.Queries[ref] = q.expr
			}
			g.Shift[ref] = q.to
			present(in, ref)
		case "threshold":
			conds := list(m["conditions"])
			if len(conds) != 1 {
				return nil, fmt.Errorf("threshold %s with %d conditions", ref, len(conds))
			}
			cm, _ := conds[0].(map[string]any)
			ev, _ := cm["evaluator"].(map[string]any)
			t := &Threshold{Input: in, Type: str(ev["type"])}
			n, ok := thresholdTypes[t.Type]
			if !ok {
				return nil, fmt.Errorf("threshold type %q", t.Type)
			}
			p := list(ev["params"])
			if len(p) < n {
				return nil, fmt.Errorf("threshold %s needs %d values", ref, n)
			}
			t.A, _ = toFloat(p[0])
			if n == 2 {
				t.B, _ = toFloat(p[1])
			}
			g.Steps[ref] = Step{Threshold: t}
		case "math":
			e, err := sse.ParseMath(str(m["expression"]))
			if err != nil {
				return nil, err
			}
			g.Steps[ref] = Step{Math: e}
		case "classic_conditions":
			c, err := classic(ref, m, queries, g)
			if err != nil {
				return nil, err
			}
			for _, cd := range c.Conds {
				present(cd.Query, cd.Present)
			}
			g.Steps[ref] = Step{Classic: c}
		case "":
			return nil, fmt.Errorf("expression %s has no type", ref)
		default:
			return nil, fmt.Errorf("%s expressions", typ)
		}
	}
	known := func(ref string) bool {
		_, q := g.Expr(ref, 0)
		_, s := g.Steps[ref]
		return q || s
	}
	if !known(g.Cond) {
		return nil, fmt.Errorf("condition %q is not one of the rule's queries", g.Cond)
	}
	for ref, st := range g.Steps {
		for _, u := range st.Uses() {
			if !known(u) {
				return nil, fmt.Errorf("%s refers to %q, which the rule doesn't have", ref, u)
			}
		}
	}
	// A step that comes back to itself, through others or not, could never
	// be worked out: the replay would recurse until the stack ran out.
	state := map[string]int{} // 1 while its inputs are walked, 2 after
	var visit func(ref string) error
	visit = func(ref string) error {
		switch state[ref] {
		case 1:
			return fmt.Errorf("%s refers back to itself", ref)
		case 2:
			return nil
		}
		state[ref] = 1
		for _, u := range g.Steps[ref].Uses() {
			if err := visit(u); err != nil {
				return err
			}
		}
		state[ref] = 2
		return nil
	}
	for _, ref := range slices.Sorted(maps.Keys(g.Steps)) {
		if err := visit(ref); err != nil {
			return nil, err
		}
	}
	// A reduce over an expression reduces over the widest range query
	// beneath it.
	for _, st := range g.Steps {
		if st.Reduce == nil {
			continue
		}
		seen := map[string]bool{}
		var walk func(string)
		walk = func(r string) {
			if seen[r] {
				return
			}
			seen[r] = true
			if q, ok := queries[r]; ok {
				st.Reduce.Window = max(st.Reduce.Window, q.window)
			}
			if s, ok := g.Steps[r]; ok {
				for _, u := range s.Uses() {
					walk(u)
				}
			}
		}
		walk(st.Reduce.Input)
	}
	return g, nil
}

func promDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

// promRange writes a subquery's range or resolution as PromQL takes it: 10m,
// 1h, 5m500ms.
func promRange(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	var b strings.Builder
	for _, u := range []struct {
		d    time.Duration
		name string
	}{{time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}, {time.Millisecond, "ms"}} {
		if n := d / u.d; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.name)
			d -= n * u.d
		}
	}
	if b.Len() == 0 {
		return "1ms"
	}
	return b.String()
}

// duration reads a "for" value: Prometheus durations such as 5m or 1h30m,
// or a number of seconds.
func duration(v any) (time.Duration, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case int:
		if x < 0 || x > int(never/time.Second) {
			return 0, fmt.Errorf("duration %d out of range", x)
		}
		return time.Duration(x) * time.Second, nil
	case string:
		if x == "" || x == "0" {
			return 0, nil
		}
		if d, err := time.ParseDuration(x); err == nil {
			if d < 0 || d > never {
				return 0, fmt.Errorf("duration %q out of range", x)
			}
			return d, nil
		}
		// Prometheus also takes days and weeks.
		if n, err := strconv.Atoi(strings.TrimSuffix(x, "d")); err == nil && strings.HasSuffix(x, "d") {
			if n < 0 || n > int(never/(24*time.Hour)) {
				return 0, fmt.Errorf("duration %q out of range", x)
			}
			return time.Duration(n) * 24 * time.Hour, nil
		}
		return 0, fmt.Errorf("unreadable duration %q", x)
	}
	return 0, fmt.Errorf("unreadable duration %v", v)
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func labels(v any) map[string]string {
	m, _ := v.(map[string]any)
	out := map[string]string{}
	for k, x := range m {
		out[k] = str(x)
	}
	return out
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func truthy(v any) bool {
	b, _ := v.(bool)
	return b
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
