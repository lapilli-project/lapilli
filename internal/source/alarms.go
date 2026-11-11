package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
)

// Reading a cloud's own alarms through its API (docs/design/alarms.md):
// Amazon CloudWatch's alarms, Azure Monitor's alert rules, Google Cloud
// Monitoring's alerting policies and OCI Monitoring's alarms, one document
// an alarm, in the shape the cloud's CLI writes to a file, its state left
// out. --alarms SOURCE=URL names the alarms, and its URL's host says which
// cloud they are in.

// The clouds --alarms reads.
const (
	AlarmsAWS    = "aws"
	AlarmsAzure  = "azure"
	AlarmsGoogle = "google"
	AlarmsOCI    = "oci"
)

// The API versions Azure's alert rules are read and written with.
var azureAlertAPI = map[string]string{
	"metricAlerts":        "2026-01-01",
	"scheduledQueryRules": "2026-03-01",
	"activityLogAlerts":   "2026-01-01",
}

// AzureAlertTypes are Azure's alert rule types, in the order they are read.
var AzureAlertTypes = []string{"metricAlerts", "scheduledQueryRules", "activityLogAlerts"}

var (
	googlePolicies = regexp.MustCompile(`^/v3/projects/([a-z0-9][-a-z0-9.:]{0,99})/alertPolicies/?$`)
	googlePolicy   = regexp.MustCompile(`^projects/([a-z0-9][-a-z0-9.:]{0,99})/alertPolicies/([0-9A-Za-z_-]{1,128})$`)
	// ociTelemetry is OCI Monitoring's host, telemetry.REGION.DOMAIN, in one
	// of Oracle's realms, as the notifications' host is.
	ociTelemetry  = regexp.MustCompile(`^telemetry\.[a-z]{2}(?:-[a-z]+){1,2}-[0-9]{1,2}\.(?:oraclecloud(?:[1-9][0-9]?)?\.com|oraclegovcloud\.(?:com|uk)|oraclecloud\.eu)$`)
	ociOCID       = regexp.MustCompile(`^ocid1\.([a-z]+)\.[a-z0-9]+\.[a-z0-9-]*(?:\.[a-z0-9-]*)?\.[a-z0-9]{1,200}$`)
	cloudwatchARN = regexp.MustCompile(`^arn:aws(?:-cn|-us-gov)?:cloudwatch:([a-z0-9-]+):([0-9]{12}):alarm:(.{1,255})$`)
	azureAlertRes = regexp.MustCompile(`(?i)^/subscriptions/([0-9A-Za-z-]{1,64})/resourceGroups/([-\w.()]{1,90})/providers/Microsoft\.Insights/(metricAlerts|scheduledQueryRules|activityLogAlerts)/([^/]{1,260})$`)
)

// AlarmsKind says which cloud an --alarms URL is: CloudWatch in a region,
// Azure Resource Manager (a subscription, or a resource group in one), a
// Google Cloud project's alerting policies, or OCI Monitoring's alarms in
// a compartment.
func AlarmsKind(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Port() != "" {
		return "", fmt.Errorf("--alarms %s: want a cloud's API over https, such as https://monitoring.eu-west-1.amazonaws.com", raw)
	}
	host := strings.ToLower(u.Hostname())
	if service, _, ok := cloud.AWSHost(host); ok {
		if service != "monitoring" || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" {
			return "", fmt.Errorf("--alarms %s: CloudWatch's alarms are read at https://monitoring.REGION.amazonaws.com", raw)
		}
		return AlarmsAWS, nil
	}
	if slices.Contains(cloud.AzureHosts, host) {
		if u.RawQuery != "" || !azurePath.MatchString(u.Path) {
			return "", fmt.Errorf("--alarms %s: want a subscription, https://management.azure.com/subscriptions/SUB, or a resource group in one, .../subscriptions/SUB/resourceGroups/RG", raw)
		}
		return AlarmsAzure, nil
	}
	if host == "monitoring.googleapis.com" {
		if u.RawQuery != "" || !googlePolicies.MatchString(u.Path) {
			return "", fmt.Errorf("--alarms %s: want a project's alerting policies, https://monitoring.googleapis.com/v3/projects/PROJECT/alertPolicies", raw)
		}
		return AlarmsGoogle, nil
	}
	if ociTelemetry.MatchString(host) {
		if _, _, err := ociScope(u); err != nil {
			return "", fmt.Errorf("--alarms %s: %w", raw, err)
		}
		return AlarmsOCI, nil
	}
	return "", fmt.Errorf("--alarms %s: not CloudWatch's, Azure Resource Manager's, Google Cloud Monitoring's or OCI Monitoring's API", raw)
}

// ociScope is an OCI --alarms URL's compartment, and whether every
// compartment under it is read (the tenancy's, with
// compartmentIdInSubtree=true).
func ociScope(u *url.URL) (string, bool, error) {
	q := u.Query()
	c, sub := q.Get("compartmentId"), q.Get("compartmentIdInSubtree")
	m := ociOCID.FindStringSubmatch(c)
	switch {
	case strings.TrimRight(u.Path, "/") != "/20180401/alarms":
		return "", false, errors.New("want OCI Monitoring's alarms, https://telemetry.REGION.oraclecloud.com/20180401/alarms?compartmentId=OCID")
	case m == nil || (m[1] != "compartment" && m[1] != "tenancy") || len(q["compartmentId"]) != 1:
		return "", false, errors.New("want compartmentId=OCID, a compartment's or the tenancy's")
	case sub != "" && (sub != "true" || m[1] != "tenancy"):
		return "", false, errors.New("compartmentIdInSubtree=true reads every compartment of the tenancy: give the tenancy's OCID with it")
	}
	for k := range q {
		if k != "compartmentId" && k != "compartmentIdInSubtree" {
			return "", false, fmt.Errorf("%s isn't asked for: give compartmentId, and compartmentIdInSubtree=true with the tenancy", k)
		}
	}
	return c, sub == "true", nil
}

// Alarms reads the alarms an --alarms URL names, and is where they are
// written through.
type Alarms struct {
	// Source names the alarms: the name their history is copied in under.
	Source string
	URL    string
	Kind   string
	Agent  string
	// Reads are the credentials the alarms are read with.
	Reads Creds
	// Transport carries the requests; nil for the default.
	Transport http.RoundTripper
	// Now is the time, for when OCI's alarms are all got again; nil for
	// the clock's.
	Now func() time.Time

	mu sync.Mutex
	// docs are the alarms as last read, and when, for Docs.
	docs []rules.Doc
	read time.Time
	// oci are OCI's alarms as last got, by OCID: one listed alike isn't
	// got again until all are, once an hour.
	oci    map[string]ociSeen
	ociAll time.Time
}

type ociSeen struct {
	listed string
	doc    []byte
}

// NewAlarms is a reader for the alarms at an --alarms URL, named source.
func NewAlarms(source, raw, agent string) (*Alarms, error) {
	if !ledger.ValidSource(source) {
		return nil, fmt.Errorf("--alarms %s=...: a source is lowercase letters, digits and dashes, 40 at most, such as cloudwatch-prod", source)
	}
	kind, err := AlarmsKind(raw)
	if err != nil {
		return nil, err
	}
	if kind != AlarmsOCI {
		raw = strings.TrimRight(raw, "/")
	}
	return &Alarms{Source: source, URL: raw, Kind: kind, Agent: agent}, nil
}

func (a *Alarms) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Call asks the cloud, and nowhere else: the URL must be on --alarms's
// host, and a redirect isn't followed. The answer's status, headers and
// body, read up to limit bytes.
func (a *Alarms) Call(ctx context.Context, method, target string, body []byte, header http.Header, c Creds, limit int64) (int, http.Header, []byte, error) {
	h := http.Header{"Accept": {"application/json"}}
	for k, vs := range header {
		h[k] = vs
	}
	if body != nil && h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/json")
	}
	e := endpoint{base: a.URL, what: "the cloud", flag: "--alarms", agent: a.Agent, transport: a.Transport}
	return e.call(ctx, method, target, body, h, c, limit)
}

// CloudFailed is an answer from a cloud that isn't what was asked for.
func CloudFailed(code int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return fmt.Errorf("the cloud: HTTP %d: %s", code, clipURL(msg))
}

// Docs are the alarms, read again when every has passed since they last
// were, or as last read: --alarms is read at an interval of its own, so
// the clouds' request quotas hold.
func (a *Alarms) Docs(ctx context.Context, every time.Duration) ([]rules.Doc, error) {
	a.mu.Lock()
	fresh := !a.read.IsZero() && a.now().Sub(a.read) < every
	docs := a.docs
	a.mu.Unlock()
	if fresh {
		return docs, nil
	}
	_, docs, err := a.Read(ctx)
	return docs, err
}

// Read reads every alarm, one document each, each rule's namespace the
// source.
func (a *Alarms) Read(ctx context.Context) ([]rules.Rule, []rules.Doc, error) {
	var docs []rules.Doc
	var err error
	switch a.Kind {
	case AlarmsAWS:
		docs, err = a.readAWS(ctx)
	case AlarmsAzure:
		docs, err = a.readAzure(ctx)
	case AlarmsGoogle:
		docs, err = a.readGoogle(ctx)
	case AlarmsOCI:
		docs, err = a.readOCI(ctx)
	default:
		err = fmt.Errorf("alarms of kind %q", a.Kind)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("--alarms %s: %w", a.Source, err)
	}
	rs, err := rules.FromDocs(docs)
	if err != nil {
		return nil, nil, fmt.Errorf("--alarms %s: %w", a.Source, err)
	}
	a.mu.Lock()
	a.docs, a.read = docs, a.now()
	a.mu.Unlock()
	return rs, docs, nil
}

// doc is an alarm as a document: its definition in its file's shape,
// written out as Lapilli keeps a JSON document, under its ID.
func (a *Alarms) doc(id string, v any) (rules.Doc, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return rules.Doc{}, err
	}
	c, err := rules.CanonJSON(b)
	if err != nil {
		return rules.Doc{}, err
	}
	return rules.Doc{Source: id, Namespace: a.Source, Data: c}, nil
}

// decode reads a JSON answer as a tree, its numbers as written.
func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}

// AlarmState are what changes as an alarm runs, left out of its document:
// a version is then a change to an alarm, not to its state.
var AlarmState = map[string][]string{
	AlarmsAWS: {"StateValue", "StateReason", "StateReasonData", "StateUpdatedTimestamp", "StateTransitionedTimestamp",
		"EvaluationState", "AlarmConfigurationUpdatedTimestamp", "ActionsSuppressedBy", "ActionsSuppressedReason"},
	AlarmsAzure:  {"systemData", "etag"},
	AlarmsGoogle: {"mutationRecord"},
	AlarmsOCI:    {"timeUpdated", "lifecycleState"},
}

// Definition is an alarm as the cloud answers it, but for its state.
func Definition(kind string, m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	for _, k := range AlarmState[kind] {
		delete(out, k)
	}
	if kind == AlarmsAzure {
		if p, ok := out["properties"].(map[string]any); ok {
			q := make(map[string]any, len(p))
			for k, v := range p {
				if k != "lastUpdatedTime" {
					q[k] = v
				}
			}
			out["properties"] = q
		}
	}
	return out
}

// region is a CloudWatch --alarms URL's region.
func (a *Alarms) region() string {
	u, _ := url.Parse(a.URL)
	_, region, _ := cloud.AWSHost(u.Hostname())
	return region
}

// AWSCall asks CloudWatch for an action, in its JSON 1.0 protocol.
func (a *Alarms) AWSCall(ctx context.Context, action string, in any, c Creds, limit int64) (int, []byte, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return 0, nil, err
	}
	h := http.Header{"Content-Type": {"application/x-amz-json-1.0"}, "X-Amz-Target": {"GraniteServiceVersion20100801." + action}}
	code, _, out, err := a.Call(ctx, http.MethodPost, a.URL+"/", body, h, c, limit)
	return code, out, err
}

// AWSAlarms is DescribeAlarms's answer.
type AWSAlarms struct {
	MetricAlarms    []map[string]any `json:"MetricAlarms"`
	CompositeAlarms []map[string]any `json:"CompositeAlarms"`
	NextToken       string           `json:"NextToken"`
}

// ReadAWSAlarms reads a DescribeAlarms answer.
func ReadAWSAlarms(b []byte) (AWSAlarms, error) {
	var out AWSAlarms
	if err := decode(b, &out); err != nil {
		return out, fmt.Errorf("not DescribeAlarms's answer: %w", err)
	}
	return out, nil
}

func (a *Alarms) readAWS(ctx context.Context) ([]rules.Doc, error) {
	var docs []rules.Doc
	token := ""
	seen := map[string]bool{}
	for pages := 0; ; pages++ {
		if pages >= 1000 {
			return nil, errors.New("DescribeAlarms gave more than a thousand pages")
		}
		in := map[string]any{"AlarmTypes": []string{"MetricAlarm", "CompositeAlarm"}, "MaxRecords": 100}
		if token != "" {
			in["NextToken"] = token
		}
		code, body, err := a.AWSCall(ctx, "DescribeAlarms", in, a.Reads, ListLimit)
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, CloudFailed(code, body)
		}
		page, err := ReadAWSAlarms(body)
		if err != nil {
			return nil, err
		}
		for _, x := range []struct {
			key string
			l   []map[string]any
		}{{"MetricAlarms", page.MetricAlarms}, {"CompositeAlarms", page.CompositeAlarms}} {
			for _, m := range x.l {
				arn, _ := m["AlarmArn"].(string)
				if a.OwnsID(arn) != nil || seen[arn] {
					continue // not one of --alarms's: not read
				}
				seen[arn] = true
				d, err := a.doc(arn, map[string]any{x.key: []any{Definition(AlarmsAWS, m)}})
				if err != nil {
					return nil, err
				}
				docs = append(docs, d)
			}
		}
		if page.NextToken == "" || page.NextToken == token {
			break
		}
		token = page.NextToken
	}
	return docs, nil
}

// azureScope is an Azure --alarms URL's subscription, and its resource
// group when it names one.
func (a *Alarms) azureScope() (sub, rg string) {
	u, _ := url.Parse(a.URL)
	if m := azurePath.FindStringSubmatch(u.Path); m != nil {
		return m[1], m[2]
	}
	return "", ""
}

func (a *Alarms) base() string {
	u, _ := url.Parse(a.URL)
	return u.Scheme + "://" + u.Host
}

// AzureURL is where an Azure alert rule of its id is read and written, at
// its type's API version.
func (a *Alarms) AzureURL(id string) (string, error) {
	m := azureAlertRes.FindStringSubmatch(id)
	if m == nil {
		return "", fmt.Errorf("%s isn't an Azure alert rule's id", clipURL(id))
	}
	typ := ""
	for _, t := range AzureAlertTypes {
		if strings.EqualFold(t, m[3]) {
			typ = t
		}
	}
	return a.base() + "/subscriptions/" + url.PathEscape(m[1]) + "/resourceGroups/" + url.PathEscape(m[2]) +
		"/providers/Microsoft.Insights/" + typ + "/" + url.PathEscape(m[4]) + "?api-version=" + azureAlertAPI[typ], nil
}

func (a *Alarms) readAzure(ctx context.Context) ([]rules.Doc, error) {
	sub, rg := a.azureScope()
	var docs []rules.Doc
	seen := map[string]bool{}
	for _, typ := range AzureAlertTypes {
		next := a.base() + "/subscriptions/" + url.PathEscape(sub)
		if rg != "" {
			next += "/resourceGroups/" + url.PathEscape(rg)
		}
		next += "/providers/Microsoft.Insights/" + typ + "?api-version=" + azureAlertAPI[typ]
		for pages := 0; next != ""; pages++ {
			if pages >= 1000 {
				return nil, fmt.Errorf("the list of %s gave more than a thousand pages", typ)
			}
			code, _, body, err := a.Call(ctx, http.MethodGet, next, nil, nil, a.Reads, ListLimit)
			if err != nil {
				return nil, err
			}
			if code != http.StatusOK {
				return nil, CloudFailed(code, body)
			}
			items, link, err := AzureList(body)
			if err != nil {
				return nil, err
			}
			for _, it := range items {
				var m map[string]any
				if err := decode(it, &m); err != nil || m == nil {
					return nil, fmt.Errorf("an Azure alert rule: %v", err)
				}
				id, _ := m["id"].(string)
				if a.OwnsID(id) != nil || seen[strings.ToLower(id)] {
					continue
				}
				seen[strings.ToLower(id)] = true
				d, err := a.doc(id, Definition(AlarmsAzure, m))
				if err != nil {
					return nil, err
				}
				docs = append(docs, d)
			}
			if link != "" {
				u, err := url.Parse(link)
				if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, mustHost(a.URL)) || u.User != nil {
					return nil, fmt.Errorf("the list's next page is at %s, not on --alarms's host: it isn't followed", clipURL(link))
				}
			}
			next = link
		}
	}
	return docs, nil
}

// project is a Google --alarms URL's project.
func (a *Alarms) project() string {
	u, _ := url.Parse(a.URL)
	if m := googlePolicies.FindStringSubmatch(u.Path); m != nil {
		return m[1]
	}
	return ""
}

// GooglePolicies is alertPolicies.list's answer.
type GooglePolicies struct {
	AlertPolicies []map[string]any `json:"alertPolicies"`
	NextPageToken string           `json:"nextPageToken"`
}

// ReadGooglePolicies reads an alertPolicies.list answer.
func ReadGooglePolicies(b []byte) (GooglePolicies, error) {
	var out GooglePolicies
	if err := decode(b, &out); err != nil {
		return out, fmt.Errorf("not alertPolicies.list's answer: %w", err)
	}
	return out, nil
}

func (a *Alarms) readGoogle(ctx context.Context) ([]rules.Doc, error) {
	var docs []rules.Doc
	seen := map[string]bool{}
	token := ""
	for pages := 0; ; pages++ {
		if pages >= 1000 {
			return nil, errors.New("alertPolicies.list gave more than a thousand pages")
		}
		q := url.Values{"pageSize": {"1000"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		code, _, body, err := a.Call(ctx, http.MethodGet, a.URL+"?"+q.Encode(), nil, nil, a.Reads, ListLimit)
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, CloudFailed(code, body)
		}
		page, err := ReadGooglePolicies(body)
		if err != nil {
			return nil, err
		}
		for _, m := range page.AlertPolicies {
			name, _ := m["name"].(string)
			if a.OwnsID(name) != nil || seen[name] || len(rulesOf(m)) == 0 {
				continue
			}
			seen[name] = true
			d, err := a.doc(name, Definition(AlarmsGoogle, m))
			if err != nil {
				return nil, err
			}
			docs = append(docs, d)
		}
		if page.NextPageToken == "" || page.NextPageToken == token {
			break
		}
		token = page.NextPageToken
	}
	return docs, nil
}

// rulesOf are the rules a decoded alarm holds; none for one that isn't.
func rulesOf(m map[string]any) []rules.Rule {
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	rs, _ := rules.Parse(b)
	return rs
}

func (a *Alarms) readOCI(ctx context.Context) ([]rules.Doc, error) {
	u, _ := url.Parse(a.URL)
	var all []map[string]any
	page := ""
	for pages := 0; ; pages++ {
		if pages >= 1000 {
			return nil, errors.New("ListAlarms gave more than a thousand pages")
		}
		q := u.Query()
		q.Set("limit", "1000")
		if page != "" {
			q.Set("page", page)
		}
		code, h, body, err := a.Call(ctx, http.MethodGet, a.base()+u.Path+"?"+q.Encode(), nil, nil, a.Reads, ListLimit)
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, CloudFailed(code, body)
		}
		var l []map[string]any
		if err := decode(body, &l); err != nil {
			return nil, fmt.Errorf("not ListAlarms's answer: %w", err)
		}
		all = append(all, l...)
		next := h.Get("opc-next-page")
		if next == "" || next == page {
			break
		}
		page = next
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.oci == nil {
		a.oci = map[string]ociSeen{}
	}
	everyOne := a.now().Sub(a.ociAll) >= time.Hour
	listed := map[string]bool{}
	var docs []rules.Doc
	for _, m := range all {
		id, _ := m["id"].(string)
		if a.ownsOCI(id, m) != nil || listed[id] {
			continue
		}
		listed[id] = true
		b, _ := json.Marshal(m)
		sum := sha256.Sum256(b)
		fp := hex.EncodeToString(sum[:])
		was, ok := a.oci[id]
		if !ok || was.listed != fp || everyOne {
			// The list has no body and no time of update: what changed is
			// told by what the list has, and every alarm is got again once
			// an hour.
			got, gone, _, err := a.getOCI(ctx, id, a.Reads)
			if err != nil {
				return nil, err
			}
			if gone {
				continue // deleted since it was listed
			}
			d, err := a.doc(id, map[string]any{"data": []any{ociDefinition(got)}})
			if err != nil {
				return nil, err
			}
			was = ociSeen{listed: fp, doc: d.Data}
			a.oci[id] = was
		}
		docs = append(docs, rules.Doc{Source: id, Namespace: a.Source, Data: was.doc})
	}
	for id := range a.oci {
		if !listed[id] {
			delete(a.oci, id)
		}
	}
	if everyOne {
		a.ociAll = a.now()
	}
	return docs, nil
}

// ociDefinition is an OCI alarm as GetAlarm gives it, but for its state,
// with its body there even when it has none: read so, what it says is
// known.
func ociDefinition(m map[string]any) map[string]any {
	d := Definition(AlarmsOCI, m)
	if _, ok := d["body"]; !ok || d["body"] == nil {
		d["body"] = ""
	}
	return d
}

// OCIURL is where an OCI alarm is read and written.
func (a *Alarms) OCIURL(id string) string {
	return a.base() + "/20180401/alarms/" + url.PathEscape(id)
}

// getOCI is GetAlarm's answer, and its etag; gone for an alarm OCI hasn't.
func (a *Alarms) getOCI(ctx context.Context, id string, c Creds) (map[string]any, bool, string, error) {
	code, h, body, err := a.Call(ctx, http.MethodGet, a.OCIURL(id), nil, nil, c, UnitLimit)
	if err != nil {
		return nil, false, "", err
	}
	if code == http.StatusNotFound {
		return nil, true, "", nil
	}
	if code != http.StatusOK {
		return nil, false, "", CloudFailed(code, body)
	}
	var m map[string]any
	if err := decode(body, &m); err != nil || m == nil {
		return nil, false, "", fmt.Errorf("not GetAlarm's answer: %v", err)
	}
	if got, _ := m["id"].(string); got != id {
		return nil, false, "", fmt.Errorf("GetAlarm answered for %s with another alarm", clipURL(id))
	}
	if err := a.ownsOCI(id, m); err != nil {
		return nil, false, "", err
	}
	if st, _ := m["lifecycleState"].(string); st == "DELETED" || st == "DELETING" {
		return nil, true, "", nil
	}
	return m, false, h.Get("etag"), nil
}

// OwnsID checks that an alarm's ID is of its cloud's form and under the
// URL's scope: an ARN of the URL's region; an Azure alert rule's id under
// the subscription or resource group, of one of the three types; a policy
// under the URL's project; an OCI alarm's OCID. A proposal is written
// where it was read.
func (a *Alarms) OwnsID(id string) error {
	switch a.Kind {
	case AlarmsAWS:
		if m := cloudwatchARN.FindStringSubmatch(id); m != nil && m[1] == a.region() && nameOK(m[3], 255) == nil {
			return nil
		}
	case AlarmsAzure:
		sub, rg := a.azureScope()
		if m := azureAlertRes.FindStringSubmatch(id); m != nil && strings.EqualFold(m[1], sub) && (rg == "" || strings.EqualFold(m[2], rg)) &&
			nameOK(m[4], 260) == nil {
			return nil
		}
	case AlarmsGoogle:
		if m := googlePolicy.FindStringSubmatch(id); m != nil && m[1] == a.project() {
			return nil
		}
	case AlarmsOCI:
		if m := ociOCID.FindStringSubmatch(id); m != nil && m[1] == "alarm" && len(id) <= 255 {
			return nil
		}
	}
	return ErrAlarmElsewhere
}

// ownsOCI checks an OCI alarm read is of the URL's compartment, or, read
// with the subtree, any under the tenancy.
func (a *Alarms) ownsOCI(id string, m map[string]any) error {
	if err := a.OwnsID(id); err != nil {
		return err
	}
	u, _ := url.Parse(a.URL)
	c, sub, _ := ociScope(u)
	if got, _ := m["compartmentId"].(string); got != c && !sub {
		return ErrAlarmElsewhere
	}
	return nil
}

// ErrAlarmElsewhere is an alarm, or a proposal's source, that isn't one of
// --alarms's.
var ErrAlarmElsewhere = errors.New("the alarm was read from another account, region, subscription, project or compartment than --alarms names: propose it again")

// nameOK checks a name that goes into a request: not empty, . or .., no
// control character, n bytes at most.
func nameOK(name string, n int) error {
	switch {
	case name == "" || name == "." || name == ".." || len(name) > n:
		return errors.New("a name Lapilli doesn't write to")
	case strings.IndexFunc(name, func(c rune) bool { return unicode.IsControl(c) || c == unicode.ReplacementChar }) >= 0:
		return errors.New("a name with a control character in it, which Lapilli doesn't write to")
	}
	return nil
}

// AlarmUnit is what a write to a cloud's alarm replaces: the alarm (a
// Google policy), read from the cloud as it is now.
type AlarmUnit struct {
	// ID is the alarm's own: its ARN, its resource id, its policy's name,
	// its OCID.
	ID string
	// Raw is the alarm's document as Read gives it: its definition in its
	// file's shape; Hash what it hashes to.
	Raw  []byte
	Hash string
	Gone bool
	// Tags are its tags (CloudWatch, Azure; OCI's freeform tags, and its
	// defined tags as NAMESPACE.KEY), or its user labels (Google).
	Tags map[string]string
	// Version is what moves when it is written: CloudWatch's
	// AlarmConfigurationUpdatedTimestamp, Google's mutateTime, Azure's and
	// OCI's etag. Etag is the etag a write is sent with (if-match), where
	// the cloud has one.
	Version, Etag string
	// Firing says the cloud's own state is firing: CloudWatch's ALARM.
	Firing bool
}

// AlarmName is the name an alarm's ID ends in.
func AlarmName(kind, id string) string {
	switch kind {
	case AlarmsAWS:
		if m := cloudwatchARN.FindStringSubmatch(id); m != nil {
			return m[3]
		}
	case AlarmsAzure:
		if m := azureAlertRes.FindStringSubmatch(id); m != nil {
			return m[4]
		}
	}
	return ""
}

// Unit reads one alarm again from the cloud, with its state, its tags and
// its version.
func (a *Alarms) Unit(ctx context.Context, id string, c Creds) (AlarmUnit, error) {
	u := AlarmUnit{ID: id}
	if err := a.OwnsID(id); err != nil {
		return u, err
	}
	var def any
	switch a.Kind {
	case AlarmsAWS:
		name := AlarmName(a.Kind, id)
		code, body, err := a.AWSCall(ctx, "DescribeAlarms", map[string]any{"AlarmNames": []string{name},
			"AlarmTypes": []string{"MetricAlarm", "CompositeAlarm"}}, c, UnitLimit)
		if err != nil {
			return u, err
		}
		if code != http.StatusOK {
			return u, CloudFailed(code, body)
		}
		page, err := ReadAWSAlarms(body)
		if err != nil {
			return u, err
		}
		var m map[string]any
		key := ""
		for _, x := range []struct {
			key string
			l   []map[string]any
		}{{"MetricAlarms", page.MetricAlarms}, {"CompositeAlarms", page.CompositeAlarms}} {
			for _, y := range x.l {
				if y["AlarmArn"] == id {
					m, key = y, x.key
				}
			}
		}
		if m == nil {
			u.Gone = true
			return u, nil
		}
		u.Firing = m["StateValue"] == "ALARM"
		u.Version = fmt.Sprint(m["AlarmConfigurationUpdatedTimestamp"])
		def = map[string]any{key: []any{Definition(AlarmsAWS, m)}}
		code, body, err = a.AWSCall(ctx, "ListTagsForResource", map[string]any{"ResourceARN": id}, c, UnitLimit)
		if err != nil {
			return u, err
		}
		if code != http.StatusOK {
			return u, CloudFailed(code, body)
		}
		var tags struct{ Tags []struct{ Key, Value string } }
		if err := decode(body, &tags); err != nil {
			return u, fmt.Errorf("not ListTagsForResource's answer: %w", err)
		}
		u.Tags = map[string]string{}
		for _, t := range tags.Tags {
			u.Tags[t.Key] = t.Value
		}
	case AlarmsAzure:
		at, err := a.AzureURL(id)
		if err != nil {
			return u, err
		}
		code, _, body, err := a.Call(ctx, http.MethodGet, at, nil, nil, c, UnitLimit)
		if err != nil {
			return u, err
		}
		if code == http.StatusNotFound {
			u.Gone = true
			return u, nil
		}
		if code != http.StatusOK {
			return u, CloudFailed(code, body)
		}
		var m map[string]any
		if err := decode(body, &m); err != nil || m == nil {
			return u, fmt.Errorf("an Azure alert rule: %v", err)
		}
		if got, _ := m["id"].(string); !strings.EqualFold(got, id) {
			return u, fmt.Errorf("Azure answered for %s with another alert rule: %w", clipURL(id), ErrAlarmElsewhere)
		}
		u.Etag, _ = m["etag"].(string)
		u.Version = u.Etag
		if tags, ok := m["tags"].(map[string]any); ok {
			u.Tags = flatTags(tags, "")
		}
		def = Definition(AlarmsAzure, m)
	case AlarmsGoogle:
		code, _, body, err := a.Call(ctx, http.MethodGet, a.base()+"/v3/"+id, nil, nil, c, UnitLimit)
		if err != nil {
			return u, err
		}
		if code == http.StatusNotFound {
			u.Gone = true
			return u, nil
		}
		if code != http.StatusOK {
			return u, CloudFailed(code, body)
		}
		var m map[string]any
		if err := decode(body, &m); err != nil || m == nil {
			return u, fmt.Errorf("an alerting policy: %v", err)
		}
		if got, _ := m["name"].(string); got != id {
			return u, fmt.Errorf("Google answered for %s with another policy: %w", clipURL(id), ErrAlarmElsewhere)
		}
		if mr, ok := m["mutationRecord"].(map[string]any); ok {
			u.Version = fmt.Sprint(mr["mutateTime"])
		}
		if l, ok := m["userLabels"].(map[string]any); ok {
			u.Tags = flatTags(l, "")
		}
		def = Definition(AlarmsGoogle, m)
	case AlarmsOCI:
		m, gone, etag, err := a.getOCI(ctx, id, c)
		if err != nil {
			return u, err
		}
		if gone {
			u.Gone = true
			return u, nil
		}
		u.Etag, u.Version = etag, etag
		u.Tags = map[string]string{}
		if t, ok := m["freeformTags"].(map[string]any); ok {
			u.Tags = flatTags(t, "")
		}
		if d, ok := m["definedTags"].(map[string]any); ok {
			for ns, v := range d {
				if kv, ok := v.(map[string]any); ok {
					for k, x := range flatTags(kv, ns+".") {
						u.Tags[k] = x
					}
				}
			}
		}
		def = map[string]any{"data": []any{ociDefinition(m)}}
	default:
		return u, fmt.Errorf("alarms of kind %q", a.Kind)
	}
	d, err := a.doc(id, def)
	if err != nil {
		return u, err
	}
	u.Raw = d.Data
	u.Hash, err = rules.TreeHash(u.Raw)
	return u, err
}

func flatTags(m map[string]any, prefix string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[prefix+k] = fmt.Sprint(v)
	}
	return out
}
