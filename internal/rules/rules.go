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
	Name   string
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
	Labels   map[string]string
	// UID is a Grafana rule's uid, which stays the same when it is edited.
	UID string
	// Digest, for a Grafana rule, changes when anything in the rule does;
	// Grafana starts a rule's alerts afresh when it changes.
	Digest string
	// Unsupported says why the rule can't be replayed.
	Unsupported string
	// Cloud, for an alarm read from a cloud's own list (cloudwatch,
	// google), says so: such an alarm keeps no series of what it fired, as
	// Prometheus's ALERTS is, for a replay to be set beside.
	Cloud string
	// Says is what the rule says when it fires, as its source has it. It
	// doesn't decide when it fires: Key leaves it out.
	Says Says
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

// Key is all of a rule that decides when it fires and how it is counted:
// two rules with the same key fire alike, whichever file they came from.
func (r Rule) Key() string {
	return fmt.Sprintf("%q %q %q %d %d %d %q %q %q %q", r.Name, r.Group, r.Expr, r.For, r.KeepFiringFor, r.Interval,
		sse.LabelKey(r.Labels), r.UID, r.Digest, r.Unsupported)
}

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
			rs[i].Source = d.Source
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
		return []Rule{azureAlert(doc)}, true, nil
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
		for _, r := range list(gm["rules"]) {
			rm, _ := r.(map[string]any)
			name := str(rm["alert"])
			if name == "" {
				continue
			}
			rule := Rule{Name: name, Group: group, Expr: strings.TrimSpace(str(rm["expr"])), Labels: labels(rm["labels"]), Interval: interval,
				Says: annotated(rm["annotations"], TemplatePrometheus)}
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
		if sev, ok := toFloat(rm["severity"]); ok && rule.Labels["severity"] == "" {
			rule.Labels["severity"] = fmt.Sprintf("Sev%d", int(sev))
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

// azureAlert reads one of Azure Monitor's own alert rules, as the Azure
// CLI lists it (flat) or the API answers (under properties). Its name is
// the resource's, as recipes/firings/azure-monitor.jq names its firings'
// rule; its severity, 0 to 4, the label severity=Sev0 to Sev4.
func azureAlert(doc map[string]any) Rule {
	props, ok := doc["properties"].(map[string]any)
	if !ok {
		props = doc
	}
	kind := azureAlertKinds[strings.ToLower(str(doc["type"]))]
	name := str(doc["name"])
	summary := str(props["displayName"])
	if summary == "" {
		summary = name
	}
	rule := Rule{Name: name, Group: str(doc["resourceGroup"]), Labels: map[string]string{}, Cloud: "azure",
		Unsupported: "an Azure Monitor " + kind + " isn't PromQL", Says: described(summary, str(props["description"]))}
	if sev, ok := toFloat(props["severity"]); ok && kind != "activity log alert" {
		rule.Labels["severity"] = fmt.Sprintf("Sev%d", int(sev))
		rule.Says.Severity = rule.Labels["severity"]
	}
	rule.Says.NoSeverity = kind == "activity log alert"
	if props["enabled"] == false {
		rule.Unsupported = Disabled
	}
	return rule
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
// error, warning, info), the label severity; its body, what it says.
func ociAlarms(doc map[string]any) []Rule {
	field := func(m map[string]any, k string) string {
		if v := str(m[k]); v != "" {
			return v
		}
		// The CLI writes keys with dashes, the API in camel case.
		parts := strings.Split(k, "-")
		for i := 1; i < len(parts); i++ {
			if parts[i] != "" {
				parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
			}
		}
		return str(m[strings.Join(parts, "")])
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
		out = append(out, rule)
	}
	for _, a := range list(doc["CompositeAlarms"]) {
		m, _ := a.(map[string]any)
		if name := str(m["AlarmName"]); name != "" {
			says := described(name, str(m["AlarmDescription"]))
			says.NoSeverity = true
			out = append(out, Rule{Name: name, Labels: map[string]string{}, Unsupported: "a CloudWatch composite alarm watches other alarms, not a query",
				Cloud: "cloudwatch", Says: says})
		}
	}
	return out, nil
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
			out = append(out, Rule{Name: name, Group: display, Labels: map[string]string{},
				Unsupported: "a Google Cloud " + kind + " isn't PromQL", Cloud: "google", Says: says})
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
				Name     string  `json:"name"`
				Interval float64 `json:"interval"`
				Rules    []struct {
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
				KeepFiringFor: secs(x.KeepFiringFor), Interval: secs(g.Interval), Labels: l, Says: annotated(a, TemplatePrometheus)})
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
		group := str(gm["name"])
		if f := str(gm["folder"]); f != "" {
			group = f + "/" + group
		}
		interval, err := duration(gm["interval"])
		if err != nil {
			return nil, fmt.Errorf("group %s: %w", group, err)
		}
		for _, r := range list(gm["rules"]) {
			rm, _ := r.(map[string]any)
			rule := Rule{
				Name: str(rm["title"]), Group: group, Labels: labels(rm["labels"]), UID: str(rm["uid"]), Interval: interval,
				Says: annotated(rm["annotations"], TemplateGrafana),
				// fmt prints maps in key order, so this is the same for the same rule.
				Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(rm)))),
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
			}
			out = append(out, rule)
		}
	}
	return out, nil
}

// Graph is a Grafana rule's queries and expressions, to be evaluated
// together at each step the way Grafana evaluates them.
type Graph struct {
	// Queries maps a refId to PromQL giving that node's value at each
	// step; a reduce over a range query is folded into its PromQL.
	Queries map[string]string
	Steps   map[string]Step // refId to a threshold or math expression
	Cond    string          // the refId whose result decides firing
}

// Step is a threshold, a math expression, or a reduce over an expression.
type Step struct {
	Threshold *Threshold
	Math      sse.Expr
	Reduce    *Reduce
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
// alert rules use: PromQL queries, reduce, threshold, and math.
func grafanaGraph(rm map[string]any) (*Graph, error) {
	type query struct {
		expr   string
		window time.Duration // a range query's window; 0 for an instant query
		res    time.Duration // how far apart its points are
	}
	queries := map[string]query{}
	exprs := map[string]map[string]any{}
	for _, d := range list(rm["data"]) {
		dm, _ := d.(map[string]any)
		model, _ := dm["model"].(map[string]any)
		ref := str(dm["refId"])
		if str(dm["datasourceUid"]) == "__expr__" {
			exprs[ref] = model
			continue
		}
		// Grafana asks for a point every intervalMs, or every 15s, the
		// Prometheus data source's default.
		q := query{expr: strings.TrimSpace(str(model["expr"])), res: 15 * time.Second}
		if ms, ok := toFloat(model["intervalMs"]); ok && ms >= 1000 {
			q.res = time.Duration(ms) * time.Millisecond
		}
		if q.expr == "" {
			return nil, fmt.Errorf("query %s has no PromQL expr", ref)
		}
		if rt, ok := dm["relativeTimeRange"].(map[string]any); ok && !truthy(model["instant"]) {
			from, _ := toFloat(rt["from"])
			to, _ := toFloat(rt["to"])
			q.window = time.Duration(from-to) * time.Second
		}
		queries[ref] = q
	}
	g := &Graph{Queries: map[string]string{}, Steps: map[string]Step{}, Cond: str(rm["condition"])}
	for ref, q := range queries {
		g.Queries[ref] = q.expr
	}
	for ref, m := range exprs {
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
				g.Queries[ref] = fmt.Sprintf("%s((%s)[%s:%s])", fn, q.expr, promDuration(q.window), resolution(fn, q.res))
			} else {
				g.Queries[ref] = q.expr
			}
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
			expr, err := classic(m, func(ref string) (string, time.Duration, time.Duration, bool) {
				q, ok := queries[ref]
				return q.expr, q.window, q.res, ok
			})
			if err != nil {
				return nil, err
			}
			g.Queries[ref] = expr
		case "":
			return nil, fmt.Errorf("expression %s has no type", ref)
		default:
			return nil, fmt.Errorf("%s expressions", typ)
		}
	}
	known := func(ref string) bool {
		_, q := g.Queries[ref]
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

var classicReducers = map[string]string{
	"avg": "avg_over_time", "min": "min_over_time", "max": "max_over_time", "sum": "sum_over_time",
	"count": "count_over_time", "count_non_null": "count_over_time", "last": "last_over_time",
	"median": "quantile_over_time",
}

// classic writes Grafana's classic conditions as one PromQL expression that
// returns a series while they hold, as Grafana evaluates them: a condition
// reduces each series of its query over the query's time range and
// compares the result; it holds when any series passes; and conditions
// join in order, each by its own and or or. Like Grafana's, the result has
// no labels: one alert.
func classic(m map[string]any, query func(ref string) (string, time.Duration, time.Duration, bool)) (string, error) {
	var out string
	for i, c := range list(m["conditions"]) {
		cm, _ := c.(map[string]any)
		qm, _ := cm["query"].(map[string]any)
		params := list(qm["params"])
		if len(params) == 0 {
			return "", errors.New("classic condition without a query")
		}
		expr, window, res, ok := query(str(params[0]))
		if !ok {
			return "", fmt.Errorf("classic condition on %q, which the rule doesn't have", str(params[0]))
		}
		rm, _ := cm["reducer"].(map[string]any)
		fn, ok := classicReducers[str(rm["type"])]
		if !ok {
			return "", fmt.Errorf("classic condition reducer %q", str(rm["type"]))
		}
		v := expr
		switch {
		case window > 0 && fn == "quantile_over_time":
			v = fmt.Sprintf("quantile_over_time(0.5, (%s)[%s:])", expr, promDuration(window))
		case window > 0:
			v = fmt.Sprintf("%s((%s)[%s:%s])", fn, expr, promDuration(window), resolution(fn, res))
		}
		em, _ := cm["evaluator"].(map[string]any)
		typ := str(em["type"])
		var vals []float64
		for _, p := range list(em["params"]) {
			f, ok := toFloat(p)
			if !ok {
				return "", fmt.Errorf("classic condition value %v", p)
			}
			vals = append(vals, f)
		}
		need := map[string]int{"gt": 1, "lt": 1, "within_range": 2, "outside_range": 2}[typ]
		if need == 0 {
			return "", fmt.Errorf("classic condition evaluator %q", typ)
		}
		if len(vals) < need {
			return "", fmt.Errorf("classic condition %s needs %d values", typ, need)
		}
		n := func(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }
		var test string
		switch lo, hi := min(vals[0], vals[len(vals)-1]), max(vals[0], vals[len(vals)-1]); typ {
		case "gt":
			test = fmt.Sprintf("(%s) > %s", v, n(vals[0]))
		case "lt":
			test = fmt.Sprintf("(%s) < %s", v, n(vals[0]))
		case "within_range": // Grafana takes the two values in either order
			test = fmt.Sprintf("((%s) > %s) < %s", v, n(lo), n(hi))
		case "outside_range":
			test = fmt.Sprintf("(%s) < %s or (%s) > %s", v, n(lo), v, n(hi))
		}
		cond := fmt.Sprintf("count(%s)", test)
		if i == 0 {
			out = cond
			continue
		}
		om, _ := cm["operator"].(map[string]any)
		op := str(om["type"])
		if op != "and" && op != "or" {
			return "", fmt.Errorf("classic condition operator %q", op)
		}
		out = fmt.Sprintf("(%s) %s (%s)", out, op, cond)
	}
	if out == "" {
		return "", errors.New("classic conditions without a condition")
	}
	return out, nil
}

// resolution is a subquery's step for a reducer over a range query: for
// count and sum, as far apart as Grafana's points, which they depend on;
// for the rest, the server's default.
func resolution(fn string, res time.Duration) string {
	if fn == "count_over_time" || fn == "sum_over_time" {
		return promDuration(res)
	}
	return ""
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
