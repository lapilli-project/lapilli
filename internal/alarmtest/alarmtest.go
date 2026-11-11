// Package alarmtest has stand-ins for the four clouds' alarm APIs, for
// tests only (docs/design/alarms.md): no cloud's alarms can be run
// locally, so each answers as its documents say it does. CloudWatch as
// botocore's cloudwatch model (2010-08-01) has it, in its JSON 1.0
// protocol; Azure Monitor's alert rules as the REST specifications
// (metricAlert and activityLogAlerts_API 2026-01-01, scheduledQueryRule_API
// 2026-03-01) have them; Google Cloud Monitoring's alerting policies as
// its v3 reference has them; OCI Monitoring's alarms as the OCI Go SDK's
// monitoring package has them. None has been checked against an account.
package alarmtest

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Transport sends each request to a stand-in, whatever its host, as a
// server would take it, the host it was for kept: that is what Lapilli
// checks, and what a signature covers.
type Transport struct {
	H     http.Handler
	mu    sync.Mutex
	Hosts map[string]int
}

func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if t.Hosts == nil {
		t.Hosts = map[string]int{}
	}
	t.Hosts[r.URL.Host]++
	t.mu.Unlock()
	req := r.Clone(r.Context())
	req.RequestURI = r.URL.RequestURI()
	if req.Host == "" {
		req.Host = r.URL.Host
	}
	if req.Body == nil {
		req.Body = http.NoBody
	}
	rec := httptest.NewRecorder()
	t.H.ServeHTTP(rec, req)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// Writes are the writes a stand-in took, each as "METHOD what".
type Writes struct {
	mu   sync.Mutex
	list []string
}

func (w *Writes) add(s string) {
	w.mu.Lock()
	w.list = append(w.list, s)
	w.mu.Unlock()
}

// Took are the writes taken, in order.
func (w *Writes) Took() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.list)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func clone(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	d.Decode(&out)
	return out
}

// Common to every stand-in: who must have sent a request (an identity, as
// the tests' credentials say in X-Identity), what to do instead of
// answering, and what someone else does meanwhile.
type Common struct {
	Writes
	// Writer is the identity writes must come from, Reader reads'.
	Writer, Reader string
	// Errs collects what a request did wrong, for the test to fail on.
	Errs []string
	// FailReads answers every read with a 503; FailAfterWrite every read
	// after the first write.
	FailReads, FailAfterWrite bool
	// Redirect answers each write with a redirect there.
	Redirect string
	// Meanwhile runs before each write is taken: someone else writing.
	Meanwhile func()
	// Ignore takes a write, answers that it did, and changes nothing.
	Ignore  bool
	written bool
	mu      sync.Mutex
}

func (c *Common) errorf(format string, args ...any) {
	c.Errs = append(c.Errs, fmt.Sprintf(format, args...))
}

// gate checks who sent a request, and says whether it is to be answered
// as it would be: false when it was answered already.
func (c *Common) gate(w http.ResponseWriter, r *http.Request, write bool) bool {
	who := c.Reader
	if write {
		who = c.Writer
	}
	if who != "" && r.Header.Get("X-Identity") != who {
		c.errorf("%s %s sent as %q, want %q", r.Method, r.URL.Path, r.Header.Get("X-Identity"), who)
	}
	switch {
	case write && c.Redirect != "":
		http.Redirect(w, r, c.Redirect, http.StatusTemporaryRedirect)
		return false
	case !write && (c.FailReads || c.FailAfterWrite && c.written):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "unavailable"})
		return false
	}
	if write {
		c.written = true
		if c.Meanwhile != nil {
			f := c.Meanwhile
			c.Meanwhile = nil
			c.mu.Unlock()
			f()
			c.mu.Lock()
		}
	}
	return true
}

// Lock and Unlock hold a stand-in still, to change it from a test.
func (c *Common) Lock()   { c.mu.Lock() }
func (c *Common) Unlock() { c.mu.Unlock() }

// ---- CloudWatch ----

// CloudWatch is Amazon CloudWatch's alarms in one account and region:
// DescribeAlarms (paged by NextToken, MaxRecords at a time),
// PutMetricAlarm (the whole alarm, its state kept), DisableAlarmActions,
// EnableAlarmActions, DeleteAlarms and ListTagsForResource, each a POST of
// JSON 1.0 with X-Amz-Target, answered 200 with no body for a write.
type CloudWatch struct {
	Common
	Region, Account string
	Metric          map[string]map[string]any
	Composite       map[string]map[string]any
	Tags            map[string]map[string]string
	stamp           int
	// FailParents answers DescribeAlarms of an alarm's parents as AWS does
	// when it throttles.
	FailParents bool
	// OnDescribe, when set, is run before each DescribeAlarms of alarms by
	// name (an alarm read again) is answered, with how many there have
	// been: someone writing between Lapilli's reads.
	OnDescribe func(n int)
	described  int
}

// NewCloudWatch is CloudWatch in a region, with no alarms.
func NewCloudWatch(region string) *CloudWatch {
	return &CloudWatch{Region: region, Account: "111122223333", Metric: map[string]map[string]any{},
		Composite: map[string]map[string]any{}, Tags: map[string]map[string]string{}}
}

// ARN is an alarm's.
func (c *CloudWatch) ARN(name string) string {
	return "arn:aws:cloudwatch:" + c.Region + ":" + c.Account + ":alarm:" + name
}

// putMetricAlarmInput are PutMetricAlarmInput's members in the model.
var putMetricAlarmInput = []string{"ActionConfiguration", "ActionsEnabled", "AlarmActions", "AlarmDescription", "AlarmName",
	"ComparisonOperator", "DatapointsToAlarm", "Dimensions", "EvaluateLowSampleCountPercentile", "EvaluationCriteria",
	"EvaluationInterval", "EvaluationPeriods", "EvaluationWindow", "ExtendedStatistic", "InsufficientDataActions", "MetricName",
	"Metrics", "Namespace", "OKActions", "Period", "Statistic", "Tags", "Threshold", "ThresholdMetricId", "TreatMissingData",
	"Unit", "WarmUpConfiguration"}

// Add puts a metric alarm, as PutMetricAlarm would, with its state.
func (c *CloudWatch) Add(alarm map[string]any, state string, tags map[string]string) {
	name := alarm["AlarmName"].(string)
	a := clone(alarm)
	c.stamp++
	a["AlarmArn"] = c.ARN(name)
	a["StateValue"], a["StateReason"] = state, "Threshold Crossed"
	a["StateUpdatedTimestamp"] = json.Number(strconv.Itoa(1762000000 + c.stamp))
	a["AlarmConfigurationUpdatedTimestamp"] = json.Number(strconv.Itoa(1762000000 + c.stamp))
	if _, ok := a["ActionsEnabled"]; !ok {
		a["ActionsEnabled"] = true
	}
	c.Metric[name] = a
	c.Tags[c.ARN(name)] = tags
}

// AddComposite puts a composite alarm.
func (c *CloudWatch) AddComposite(name, rule string) {
	c.Composite[name] = map[string]any{"AlarmName": name, "AlarmArn": c.ARN(name), "AlarmRule": rule, "ActionsEnabled": true,
		"StateValue": "OK", "AlarmConfigurationUpdatedTimestamp": json.Number("1762000000")}
	c.Tags[c.ARN(name)] = nil
}

func (c *CloudWatch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=") || !strings.Contains(auth, "/"+c.Region+"/monitoring/aws4_request") {
		c.errorf("%s isn't signed for monitoring in %s: %q", r.Header.Get("X-Amz-Target"), c.Region, auth)
	}
	action, ok := strings.CutPrefix(r.Header.Get("X-Amz-Target"), "GraniteServiceVersion20100801.")
	if r.Method != http.MethodPost || r.URL.Path != "/" || !ok || r.Header.Get("Content-Type") != "application/x-amz-json-1.0" {
		c.errorf("not CloudWatch's JSON 1.0: %s %s %q %q", r.Method, r.URL.Path, r.Header.Get("X-Amz-Target"), r.Header.Get("Content-Type"))
		writeJSON(w, http.StatusBadRequest, map[string]string{"__type": "UnknownOperationException"})
		return
	}
	var in map[string]any
	d := json.NewDecoder(r.Body)
	d.UseNumber()
	if err := d.Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"__type": "SerializationException", "message": err.Error()})
		return
	}
	names := func() []string {
		var out []string
		for _, n := range asList(in["AlarmNames"]) {
			s, _ := n.(string)
			out = append(out, s)
		}
		return out
	}
	write := action != "DescribeAlarms" && action != "ListTagsForResource"
	if !c.gate(w, r, write) {
		return
	}
	switch action {
	case "DescribeAlarms":
		if parents, ok := in["ParentsOfAlarmName"].(string); ok {
			// "If you specify ParentsOfAlarmName, you cannot specify any
			// other parameters in the request except for MaxRecords and
			// NextToken."
			for k := range in {
				if k != "ParentsOfAlarmName" && k != "MaxRecords" && k != "NextToken" {
					writeJSON(w, http.StatusBadRequest, map[string]string{"__type": "InvalidParameterCombination",
						"message": "ParentsOfAlarmName takes no other parameter but MaxRecords and NextToken: " + k})
					return
				}
			}
			if c.FailParents {
				writeJSON(w, http.StatusBadRequest, map[string]string{"__type": "Throttling", "message": "Rate exceeded"})
				return
			}
			var out []any
			for _, n := range slices.Sorted(maps.Keys(c.Composite)) {
				if rule := c.Composite[n]["AlarmRule"].(string); strings.Contains(rule, "("+parents+")") || strings.Contains(rule, "(\""+parents+"\")") {
					out = append(out, c.Composite[n])
				}
			}
			// One a page, to be followed.
			start, _ := strconv.Atoi(fmt.Sprint(in["NextToken"]))
			page := map[string]any{"MetricAlarms": []any{}, "CompositeAlarms": []any{}}
			if start < len(out) {
				page["CompositeAlarms"] = out[start : start+1]
			}
			if start+1 < len(out) {
				page["NextToken"] = strconv.Itoa(start + 1)
			}
			writeJSON(w, http.StatusOK, page)
			return
		}
		if len(names()) > 0 && c.OnDescribe != nil {
			c.described++
			c.OnDescribe(c.described)
		}
		types := asList(in["AlarmTypes"])
		var metric, composite []map[string]any
		only := names()
		for _, n := range slices.Sorted(maps.Keys(c.Metric)) {
			if (len(only) == 0 || slices.Contains(only, n)) && slices.Contains(types, any("MetricAlarm")) {
				metric = append(metric, c.Metric[n])
			}
		}
		for _, n := range slices.Sorted(maps.Keys(c.Composite)) {
			if (len(only) == 0 || slices.Contains(only, n)) && slices.Contains(types, any("CompositeAlarm")) {
				composite = append(composite, c.Composite[n])
			}
		}
		// A page at a time: MaxRecords, of metric alarms then composite.
		max := 100
		if m, err := strconv.Atoi(fmt.Sprint(in["MaxRecords"])); err == nil && m > 0 {
			max = m
		}
		start, _ := strconv.Atoi(fmt.Sprint(in["NextToken"]))
		all := append(slices.Clone(metric), composite...)
		end := min(start+max, len(all))
		out := map[string]any{"MetricAlarms": []any{}, "CompositeAlarms": []any{}}
		for i := start; i < end; i++ {
			key := "MetricAlarms"
			if i >= len(metric) {
				key = "CompositeAlarms"
			}
			out[key] = append(out[key].([]any), all[i])
		}
		if end < len(all) {
			out["NextToken"] = strconv.Itoa(end)
		}
		writeJSON(w, http.StatusOK, out)
	case "ListTagsForResource":
		arn, _ := in["ResourceARN"].(string)
		tags, ok := c.Tags[arn]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"__type": "ResourceNotFoundException", "message": "no alarm " + arn})
			return
		}
		var out []map[string]string
		for _, k := range slices.Sorted(maps.Keys(tags)) {
			out = append(out, map[string]string{"Key": k, "Value": tags[k]})
		}
		writeJSON(w, http.StatusOK, map[string]any{"Tags": out})
	case "PutMetricAlarm":
		for k := range in {
			if !slices.Contains(putMetricAlarmInput, k) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"__type": "SerializationException", "message": "unknown member " + k})
				return
			}
		}
		if q := dig(in, "EvaluationCriteria", "PromQLCriteria"); q != nil {
			if p, err := strconv.Atoi(fmt.Sprint(q["PendingPeriod"])); q["PendingPeriod"] != nil && (err != nil || p < 0 || p > 86400) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"__type": "ValidationError", "message": "PendingPeriod must be 0 to 86400"})
				return
			}
		}
		name, _ := in["AlarmName"].(string)
		c.add("PutMetricAlarm " + name)
		if c.Ignore {
			w.WriteHeader(http.StatusOK)
			return
		}
		was, ok := c.Metric[name]
		a := clone(in)
		tags, _ := a["Tags"].([]any)
		delete(a, "Tags")
		if _, ok := a["ActionsEnabled"]; !ok {
			a["ActionsEnabled"] = true // left out, TRUE
		}
		c.stamp++
		a["AlarmArn"] = c.ARN(name)
		a["AlarmConfigurationUpdatedTimestamp"] = json.Number(strconv.Itoa(1762000000 + c.stamp))
		if ok {
			for _, k := range []string{"StateValue", "StateReason", "StateUpdatedTimestamp", "StateTransitionedTimestamp"} {
				if v, ok := was[k]; ok {
					a[k] = v
				}
			}
		} else {
			a["StateValue"] = "OK"
			t := map[string]string{}
			for _, x := range tags { // an update ignores them
				m, _ := x.(map[string]any)
				t[fmt.Sprint(m["Key"])] = fmt.Sprint(m["Value"])
			}
			c.Tags[c.ARN(name)] = t
		}
		c.Metric[name] = a
		w.WriteHeader(http.StatusOK)
	case "DisableAlarmActions", "EnableAlarmActions":
		for _, n := range names() {
			c.add(action + " " + n)
			if c.Ignore {
				continue
			}
			for _, m := range []map[string]map[string]any{c.Metric, c.Composite} {
				if a, ok := m[n]; ok {
					a["ActionsEnabled"] = action == "EnableAlarmActions"
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	case "DeleteAlarms":
		ns := names()
		if len(ns) != 1 {
			c.errorf("DeleteAlarms of %d alarms, not one", len(ns))
		}
		for _, n := range ns {
			c.add("DeleteAlarms " + n)
			if !c.Ignore {
				delete(c.Metric, n) // answered alike for one that isn't there
				delete(c.Composite, n)
			}
		}
		w.WriteHeader(http.StatusOK)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"__type": "UnknownOperationException"})
	}
}

// Alarm is a metric alarm as CloudWatch keeps it now.
func (c *CloudWatch) Alarm(name string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a, ok := c.Metric[name]; ok {
		return clone(a)
	}
	return nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func dig(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m, _ = m[k].(map[string]any)
	}
	return m
}

// ---- Azure ----

// Azure is Azure Resource Manager's alert rules in one subscription:
// metricAlerts, scheduledQueryRules and activityLogAlerts, listed a page
// (one rule) at a time with nextLink, read, PATCHed (properties merged a
// field at a time, 200), PUT (200, or 201 when made), DELETEd (200, or 204
// when there was none). A log search alert carries an etag, and a write
// with If-Match of another is refused with 412.
type Azure struct {
	Common
	Sub   string
	Rules map[string]map[string]any // by lower-cased id
	etag  int
	// Alerts are Alerts Management's, each its alertRule (an id, in any
	// letter case, or a name) and monitorCondition (Fired or Resolved):
	// listed a page (one alert) at a time, filtered by monitorCondition.
	Alerts [][2]string
}

// AzureVersions are the API versions each type is read and written with.
var AzureVersions = map[string]string{"metricalerts": "2026-01-01", "scheduledqueryrules": "2026-03-01", "activitylogalerts": "2026-01-01"}

var azureID = regexp.MustCompile(`(?i)^/subscriptions/([^/]+)/resourceGroups/([^/]+)/providers/Microsoft\.Insights/(metricAlerts|scheduledQueryRules|activityLogAlerts)/([^/]+)$`)

// NewAzure is a subscription with no alert rules.
func NewAzure(sub string) *Azure { return &Azure{Sub: sub, Rules: map[string]map[string]any{}} }

// ID is an alert rule's.
func (z *Azure) ID(rg, typ, name string) string {
	return "/subscriptions/" + z.Sub + "/resourceGroups/" + rg + "/providers/Microsoft.Insights/" + typ + "/" + name
}

// Add puts an alert rule: its properties, tags and type.
func (z *Azure) Add(rg, typ, name string, props map[string]any, tags map[string]any) string {
	id := z.ID(rg, typ, name)
	r := map[string]any{"id": id, "name": name, "type": "Microsoft.Insights/" + typ, "location": "global", "tags": tags,
		"properties": clone(props)}
	if typ == "scheduledQueryRules" {
		z.etag++
		r["etag"] = fmt.Sprintf(`"etag-%d"`, z.etag)
		r["kind"] = "LogAlert"
		r["systemData"] = map[string]any{"lastModifiedBy": "someone"}
	}
	if typ == "metricAlerts" {
		r["properties"].(map[string]any)["lastUpdatedTime"] = "2026-11-01T00:00:00Z"
	}
	z.Rules[strings.ToLower(id)] = r
	return id
}

// Rule is an alert rule as Azure keeps it now.
func (z *Azure) Rule(id string) map[string]any {
	z.mu.Lock()
	defer z.mu.Unlock()
	if r, ok := z.Rules[strings.ToLower(id)]; ok {
		return clone(r)
	}
	return nil
}

func (z *Azure) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		z.errorf("%s %s has no token", r.Method, r.URL.Path)
	}
	write := r.Method != http.MethodGet
	if !z.gate(w, r, write) {
		return
	}
	path := r.URL.Path
	if m := azureID.FindStringSubmatch(path); m != nil {
		typ := strings.ToLower(m[3])
		if r.URL.Query().Get("api-version") != AzureVersions[typ] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "InvalidApiVersionParameter"}})
			return
		}
		key := strings.ToLower(path)
		cur, ok := z.Rules[key]
		if write && typ == "scheduledqueryrules" && ok && r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != cur["etag"] {
			writeJSON(w, http.StatusPreconditionFailed, map[string]any{"error": map[string]string{"code": "PreconditionFailed"}})
			return
		}
		var body map[string]any
		if write && r.Method != http.MethodDelete {
			d := json.NewDecoder(r.Body)
			d.UseNumber()
			if err := d.Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "InvalidRequestContent"}})
				return
			}
		}
		if write {
			z.add(r.Method + " " + m[4])
			if z.Ignore {
				if ok {
					writeJSON(w, http.StatusOK, cur)
				} else {
					w.WriteHeader(http.StatusNoContent)
				}
				return
			}
		}
		switch r.Method {
		case http.MethodGet:
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "ResourceNotFound"}})
				return
			}
			writeJSON(w, http.StatusOK, cur)
		case http.MethodPatch:
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "ResourceNotFound"}})
				return
			}
			if typ == "activitylogalerts" {
				// AlertRulePatchProperties: enabled only.
				if p, _ := body["properties"].(map[string]any); len(p) > 1 || (len(p) == 1 && p["enabled"] == nil) {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "InvalidRequestContent", "message": "only enabled"}})
					return
				}
			}
			props, _ := cur["properties"].(map[string]any)
			p, _ := body["properties"].(map[string]any)
			maps.Copy(props, p)
			if t, ok := body["tags"]; ok {
				cur["tags"] = t
			}
			z.touch(cur)
			writeJSON(w, http.StatusOK, cur)
		case http.MethodPut:
			made := !ok
			for _, k := range []string{"id", "name", "type", "systemData", "etag"} {
				delete(body, k)
			}
			body["id"], body["name"], body["type"] = cur["id"], m[4], "Microsoft.Insights/"+m[3]
			if made {
				body["id"] = z.ID(m[2], m[3], m[4])
			}
			if typ == "scheduledqueryrules" {
				body["kind"] = "LogAlert"
			}
			z.Rules[key] = body
			z.touch(body)
			code := http.StatusOK
			if made {
				code = http.StatusCreated
			}
			writeJSON(w, code, body)
		case http.MethodDelete:
			if !ok {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			delete(z.Rules, key)
			w.WriteHeader(http.StatusOK)
		}
		return
	}
	if path == "/subscriptions/"+z.Sub+"/providers/Microsoft.AlertsManagement/alerts" && r.Method == http.MethodGet {
		z.alerts(w, r)
		return
	}
	// A list: one rule a page, the next page's link on the same host.
	m := regexp.MustCompile(`(?i)^/subscriptions/([^/]+)(?:/resourceGroups/([^/]+))?/providers/Microsoft\.Insights/(metricAlerts|scheduledQueryRules|activityLogAlerts)$`).FindStringSubmatch(path)
	if m == nil || r.Method != http.MethodGet {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "NotFound"}})
		return
	}
	typ := strings.ToLower(m[3])
	if r.URL.Query().Get("api-version") != AzureVersions[typ] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "InvalidApiVersionParameter"}})
		return
	}
	var all []map[string]any
	for _, k := range slices.Sorted(maps.Keys(z.Rules)) {
		x := azureID.FindStringSubmatch(z.Rules[k]["id"].(string))
		if strings.EqualFold(x[3], m[3]) && (m[2] == "" || strings.EqualFold(x[2], m[2])) {
			all = append(all, z.Rules[k])
		}
	}
	skip, _ := strconv.Atoi(r.URL.Query().Get("$skipToken"))
	out := map[string]any{"value": []any{}}
	if skip < len(all) {
		out["value"] = []any{all[skip]}
	}
	if skip+1 < len(all) {
		q := r.URL.Query()
		q.Set("$skipToken", strconv.Itoa(skip+1))
		out["nextLink"] = "https://" + r.Host + path + "?" + q.Encode()
		if r.Host == "" {
			out["nextLink"] = "https://management.azure.com" + path + "?" + q.Encode()
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// alerts is Alerts_GetAll at 2019-03-01: monitorCondition and timeRange
// are its query's, an alert's essentials hold its rule and condition.
func (z *Azure) alerts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("api-version") != "2019-03-01" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "InvalidApiVersionParameter"}})
		return
	}
	if c := q.Get("monitorCondition"); c != "" && c != "Fired" && c != "Resolved" || !slices.Contains([]string{"", "1h", "1d", "7d", "30d"}, q.Get("timeRange")) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "BadRequest"}})
		return
	}
	var all []map[string]any
	for i, a := range z.Alerts {
		if c := q.Get("monitorCondition"); c == "" || c == a[1] {
			all = append(all, map[string]any{"id": fmt.Sprintf("/subscriptions/%s/providers/Microsoft.AlertsManagement/alerts/%d", z.Sub, i),
				"properties": map[string]any{"essentials": map[string]any{"alertRule": a[0], "monitorCondition": a[1], "alertState": "New", "severity": "Sev3"}}})
		}
	}
	skip, _ := strconv.Atoi(q.Get("$skipToken"))
	out := map[string]any{"value": []any{}}
	if skip < len(all) {
		out["value"] = []any{all[skip]}
	}
	if skip+1 < len(all) {
		q.Set("$skipToken", strconv.Itoa(skip+1))
		out["nextLink"] = "https://management.azure.com" + r.URL.Path + "?" + q.Encode()
	}
	writeJSON(w, http.StatusOK, out)
}

func (z *Azure) touch(r map[string]any) {
	if strings.HasSuffix(strings.ToLower(r["type"].(string)), "scheduledqueryrules") {
		z.etag++
		r["etag"] = fmt.Sprintf(`"etag-%d"`, z.etag)
	}
	if p, ok := r["properties"].(map[string]any); ok && strings.HasSuffix(strings.ToLower(r["type"].(string)), "metricalerts") {
		p["lastUpdatedTime"] = time.Date(2026, 11, 2, 0, 0, z.etag, 0, time.UTC).Format(time.RFC3339)
		z.etag++
	}
}

// ---- Google Cloud ----

// Google is Cloud Monitoring's alerting policies in one project: listed
// (pageSize, nextPageToken), got, patched (each field in updateMask set to
// the value sent, the rest left; an empty mask replaces the policy),
// deleted, and created with a new ID, each condition named.
type Google struct {
	Common
	Project  string
	Policies map[string]map[string]any // by name
	next     int
	mutate   int
	// AfterCreate, when set, is run once a policy is made: a person
	// making another meanwhile.
	AfterCreate func()
	// LoseCreate makes the policy and answers as though the answer were
	// lost on the way: a 504, as a load balancer gives.
	LoseCreate bool
	// Alerts are the project's, each its policy's name and state (OPEN or
	// CLOSED): listed a page (one alert) at a time, filtered by state.
	Alerts [][2]string
}

// alerts is projects.alerts.list: its filter as Google's documents show
// one of state (state=open), a page of pageSize at most, and one here.
func (g *Google) alerts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := strings.ToUpper(strings.ReplaceAll(q.Get("filter"), " ", ""))
	if f != "" && f != "STATE=OPEN" && f != "STATE=CLOSED" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400, "status": "INVALID_ARGUMENT"}})
		return
	}
	var all []map[string]any
	for i, a := range g.Alerts {
		if f == "" || f == "STATE="+a[1] {
			all = append(all, map[string]any{"name": fmt.Sprintf("projects/%s/alerts/%d", g.Project, i), "state": a[1],
				"openTime": "2026-11-01T00:00:00Z", "policy": map[string]any{"name": a[0], "displayName": "x"}})
		}
	}
	start, _ := strconv.Atoi(q.Get("pageToken"))
	out := map[string]any{}
	if start < len(all) {
		out["alerts"] = []any{all[start]}
	}
	if start+1 < len(all) {
		out["nextPageToken"] = strconv.Itoa(start + 1)
	}
	writeJSON(w, http.StatusOK, out)
}

// NewGoogle is a project with no policies.
func NewGoogle(project string) *Google {
	return &Google{Project: project, Policies: map[string]map[string]any{}, next: 100}
}

// Add puts a policy, naming it and its conditions, and gives its name.
func (g *Google) Add(p map[string]any) string {
	p = clone(p)
	g.next++
	name := fmt.Sprintf("projects/%s/alertPolicies/%d", g.Project, g.next)
	p["name"] = name
	for i, c := range asList(p["conditions"]) {
		cm, _ := c.(map[string]any)
		if cm["name"] == nil {
			cm["name"] = fmt.Sprintf("%s/conditions/%d", name, i+1)
		}
	}
	g.stamp(p)
	g.Policies[name] = p
	return name
}

func (g *Google) stamp(p map[string]any) {
	g.mutate++
	p["mutationRecord"] = map[string]any{"mutateTime": time.Date(2026, 11, 1, 0, 0, g.mutate, 0, time.UTC).Format(time.RFC3339Nano), "mutatedBy": "someone@example.com"}
}

// Policy is a policy as Google keeps it now.
func (g *Google) Policy(name string) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p, ok := g.Policies[name]; ok {
		return clone(p)
	}
	return nil
}

// googleFields are the fields of a policy a mask may name.
var googleFields = []string{"displayName", "documentation", "userLabels", "conditions", "combiner", "enabled", "validity",
	"notificationChannels", "alertStrategy", "severity"}

func (g *Google) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		g.errorf("%s %s has no token", r.Method, r.URL.Path)
	}
	write := r.Method != http.MethodGet
	if !g.gate(w, r, write) {
		return
	}
	if r.URL.Path == "/v3/projects/"+g.Project+"/alerts" && r.Method == http.MethodGet {
		g.alerts(w, r)
		return
	}
	base := "/v3/projects/" + g.Project + "/alertPolicies"
	rest, ok := strings.CutPrefix(r.URL.Path, base)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404}})
		return
	}
	notFound := func() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404, "status": "NOT_FOUND"}})
	}
	var body map[string]any
	if r.Method == http.MethodPatch || r.Method == http.MethodPost {
		d := json.NewDecoder(r.Body)
		d.UseNumber()
		if err := d.Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400, "message": err.Error()}})
			return
		}
	}
	switch {
	case rest == "" && r.Method == http.MethodGet:
		names := slices.Sorted(maps.Keys(g.Policies))
		size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
		if size <= 0 || size > 2 {
			size = 2 // small pages, to be followed
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
		end := min(start+size, len(names))
		out := map[string]any{}
		var ps []any
		for _, n := range names[min(start, end):end] {
			ps = append(ps, g.Policies[n])
		}
		if ps != nil {
			out["alertPolicies"] = ps
		}
		if end < len(names) {
			out["nextPageToken"] = strconv.Itoa(end)
		}
		writeJSON(w, http.StatusOK, out)
	case rest == "" && r.Method == http.MethodPost:
		g.add("POST create")
		delete(body, "name")
		name := g.Add(body)
		if g.LoseCreate {
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": map[string]any{"code": 504, "status": "DEADLINE_EXCEEDED"}})
		} else {
			writeJSON(w, http.StatusOK, g.Policies[name])
		}
		if g.AfterCreate != nil {
			g.AfterCreate()
		}
	case strings.HasPrefix(rest, "/"):
		name := strings.TrimPrefix(r.URL.Path, "/v3/")
		p, ok := g.Policies[name]
		if !ok {
			notFound()
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, p)
		case http.MethodDelete:
			g.add("DELETE " + name)
			if !g.Ignore {
				delete(g.Policies, name)
			}
			writeJSON(w, http.StatusOK, map[string]any{})
		case http.MethodPatch:
			mask := r.URL.Query().Get("updateMask")
			g.add("PATCH " + name + " " + mask)
			if g.Ignore {
				writeJSON(w, http.StatusOK, p)
				return
			}
			if mask == "" {
				body["name"] = name
				g.Policies[name] = body
				p = body
			} else {
				for _, f := range strings.Split(mask, ",") {
					if !slices.Contains(googleFields, f) && !strings.HasPrefix(f, "documentation.") {
						writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400, "message": "bad mask " + f}})
						return
					}
					if sub, ok := strings.CutPrefix(f, "documentation."); ok {
						d, _ := p["documentation"].(map[string]any)
						if d == nil {
							d = map[string]any{}
							p["documentation"] = d
						}
						if v := dig(body, "documentation")[sub]; v != nil {
							d[sub] = v
						} else {
							delete(d, sub)
						}
						continue
					}
					if v, ok := body[f]; ok {
						p[f] = v
					} else {
						delete(p, f)
					}
				}
			}
			for i, c := range asList(p["conditions"]) {
				if cm, _ := c.(map[string]any); cm["name"] == nil {
					cm["name"] = fmt.Sprintf("%s/conditions/n%d", name, i+1)
				}
			}
			g.stamp(p)
			writeJSON(w, http.StatusOK, p)
		}
	default:
		notFound()
	}
}

// ---- OCI ----

// OCI is OCI Monitoring's alarms in one compartment: ListAlarms (a JSON
// array of summaries, without a body, opc-next-page), GetAlarm (with an
// etag), UpdateAlarm (the fields sent; a stale if-match answered 412),
// DeleteAlarm (204) and CreateAlarm. Each request's signature is checked
// with the key's public half, apart from the SDK.
type OCI struct {
	Common
	Compartment string
	Alarms      map[string]map[string]any // by OCID
	Key         *rsa.PublicKey
	KeyID       string
	etag        int
	next        int
	// Gets counts GetAlarm by OCID.
	Gets map[string]int
	// Firing are the OCIDs of the alarms whose status is FIRING; every
	// other's is OK.
	Firing []string
}

// NewOCI is a compartment with no alarms, whose requests are signed with
// key.
func NewOCI(compartment string, key *rsa.PublicKey, keyID string) *OCI {
	return &OCI{Compartment: compartment, Alarms: map[string]map[string]any{}, Key: key, KeyID: keyID, Gets: map[string]int{}}
}

// Add puts an alarm, and gives its OCID.
func (o *OCI) Add(a map[string]any) string {
	a = clone(a)
	o.next++
	id := fmt.Sprintf("ocid1.alarm.oc1.ap-seoul-1.a%04d", o.next)
	a["id"] = id
	if a["compartmentId"] == nil {
		a["compartmentId"] = o.Compartment
	}
	a["lifecycleState"] = "ACTIVE"
	a["timeCreated"] = "2026-10-01T00:00:00.000Z"
	o.touch(a)
	o.Alarms[id] = a
	return id
}

func (o *OCI) touch(a map[string]any) {
	o.etag++
	a["timeUpdated"] = time.Date(2026, 11, 1, 0, 0, o.etag, 0, time.UTC).Format("2006-01-02T15:04:05.000Z")
	a["__etag"] = fmt.Sprintf("etag-%d", o.etag)
}

// Alarm is an alarm as OCI keeps it now.
func (o *OCI) Alarm(id string) map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	if a, ok := o.Alarms[id]; ok {
		out := clone(a)
		delete(out, "__etag")
		return out
	}
	return nil
}

// ociSummary are AlarmSummary's fields: no body, no times.
var ociSummary = []string{"id", "displayName", "compartmentId", "metricCompartmentId", "namespace", "query", "severity", "destinations",
	"isEnabled", "lifecycleState", "suppression", "isNotificationsPerMetricDimensionEnabled", "freeformTags", "definedTags", "overrides",
	"ruleName", "notificationVersion", "notificationTitle", "evaluationSlackDuration", "alarmSummary", "resourceGroup"}

// ociUpdate are UpdateAlarmDetails's fields.
var ociUpdate = []string{"displayName", "compartmentId", "metricCompartmentId", "metricCompartmentIdInSubtree", "namespace", "resourceGroup",
	"query", "resolution", "pendingDuration", "severity", "body", "isNotificationsPerMetricDimensionEnabled", "messageFormat", "destinations",
	"repeatNotificationDuration", "suppression", "isEnabled", "freeformTags", "definedTags", "overrides", "ruleName", "notificationVersion",
	"notificationTitle", "evaluationSlackDuration", "alarmSummary"}

func (o *OCI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	if err := o.verify(r, body); err != nil {
		o.errorf("%s %s: %v", r.Method, r.URL.Path, err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "NotAuthenticated", "message": err.Error()})
		return
	}
	write := r.Method != http.MethodGet
	if !o.gate(w, r, write) {
		return
	}
	notFound := func() {
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "NotAuthorizedOrNotFound", "message": "not found"})
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/20180401/alarms")
	if !ok {
		notFound()
		return
	}
	switch {
	case rest == "/status" && r.Method == http.MethodGet:
		o.status(w, r)
	case rest == "" && r.Method == http.MethodGet:
		q := r.URL.Query()
		if q.Get("compartmentId") != o.Compartment {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "InvalidParameter", "message": "compartmentId"})
			return
		}
		ids := slices.Sorted(maps.Keys(o.Alarms))
		start, _ := strconv.Atoi(q.Get("page"))
		end := min(start+2, len(ids)) // two a page, to be followed
		out := []map[string]any{}
		for _, id := range ids[min(start, end):end] {
			s := map[string]any{}
			for _, k := range ociSummary {
				if v, ok := o.Alarms[id][k]; ok {
					s[k] = v
				}
			}
			out = append(out, s)
		}
		if end < len(ids) {
			w.Header().Set("opc-next-page", strconv.Itoa(end))
		}
		writeJSON(w, http.StatusOK, out)
	case rest == "" && r.Method == http.MethodPost:
		o.add("POST create")
		var in map[string]any
		json.Unmarshal(body, &in)
		id := o.Add(in)
		w.Header().Set("etag", o.Alarms[id]["__etag"].(string))
		writeJSON(w, http.StatusOK, o.public(o.Alarms[id]))
	case strings.HasPrefix(rest, "/"):
		id, _ := url.PathUnescape(strings.TrimPrefix(rest, "/"))
		a, ok := o.Alarms[id]
		if !ok {
			notFound()
			return
		}
		if write && r.Header.Get("if-match") != "" && r.Header.Get("if-match") != a["__etag"] {
			writeJSON(w, http.StatusPreconditionFailed, map[string]string{"code": "PreconditionFailed", "message": "The resource's etag doesn't match"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			o.Gets[id]++
			w.Header().Set("etag", a["__etag"].(string))
			writeJSON(w, http.StatusOK, o.public(a))
		case http.MethodPut:
			var in map[string]any
			d := json.NewDecoder(bytes.NewReader(body))
			d.UseNumber()
			if err := d.Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"code": "InvalidParameter", "message": err.Error()})
				return
			}
			keys := slices.Sorted(maps.Keys(in))
			o.add("PUT " + id + " " + strings.Join(keys, ","))
			for _, k := range keys {
				if !slices.Contains(ociUpdate, k) {
					writeJSON(w, http.StatusBadRequest, map[string]string{"code": "InvalidParameter", "message": "unknown " + k})
					return
				}
			}
			if !o.Ignore {
				maps.Copy(a, in)
				o.touch(a)
			}
			w.Header().Set("etag", a["__etag"].(string))
			writeJSON(w, http.StatusOK, o.public(a))
		case http.MethodDelete:
			o.add("DELETE " + id)
			if !o.Ignore {
				delete(o.Alarms, id)
			}
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		notFound()
	}
}

// status is ListAlarmsStatus: AlarmStatusSummary's, filtered by status,
// one a page with opc-next-page.
func (o *OCI) status(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("compartmentId") != o.Compartment || !slices.Contains([]string{"", "FIRING", "OK"}, q.Get("status")) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "InvalidParameter", "message": "compartmentId or status"})
		return
	}
	var all []map[string]any
	for _, id := range slices.Sorted(maps.Keys(o.Alarms)) {
		st := "OK"
		if slices.Contains(o.Firing, id) {
			st = "FIRING"
		}
		if s := q.Get("status"); s == "" || s == st {
			all = append(all, map[string]any{"id": id, "displayName": o.Alarms[id]["displayName"], "severity": o.Alarms[id]["severity"],
				"timestampTriggered": "2026-11-01T00:00:00.000Z", "status": st})
		}
	}
	start, _ := strconv.Atoi(q.Get("page"))
	out := []map[string]any{}
	if start < len(all) {
		out = append(out, all[start])
	}
	if start+1 < len(all) {
		w.Header().Set("opc-next-page", strconv.Itoa(start+1))
	}
	writeJSON(w, http.StatusOK, out)
}

func (o *OCI) public(a map[string]any) map[string]any {
	out := clone(a)
	delete(out, "__etag")
	return out
}

var ociAuth = regexp.MustCompile(`^Signature version="1",headers="([^"]+)",keyId="([^"]+)",algorithm="rsa-sha256",signature="([^"]+)"$`)

// verify checks a request's OCI signature as Oracle documents it: the
// headers named, each "name: value" on a line, (request-target) the
// method and the path with its query, signed with RSA-SHA256; for a body,
// its length, type and SHA-256 among them.
func (o *OCI) verify(r *http.Request, body []byte) error {
	m := ociAuth.FindStringSubmatch(r.Header.Get("Authorization"))
	if m == nil {
		return fmt.Errorf("no OCI signature: %q", r.Header.Get("Authorization"))
	}
	headers := strings.Fields(m[1])
	want := []string{"date", "(request-target)", "host"}
	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		want = append(want, "content-length", "content-type", "x-content-sha256")
		sum := sha256.Sum256(body)
		if r.Header.Get("x-content-sha256") != base64.StdEncoding.EncodeToString(sum[:]) {
			return fmt.Errorf("x-content-sha256 isn't the body's")
		}
		if r.Header.Get("content-length") != strconv.Itoa(len(body)) {
			return fmt.Errorf("content-length isn't the body's")
		}
	}
	if !slices.Equal(headers, want) {
		return fmt.Errorf("signs %v, want %v", headers, want)
	}
	if m[2] != o.KeyID {
		return fmt.Errorf("keyId %q, want %q", m[2], o.KeyID)
	}
	if d, err := http.ParseTime(r.Header.Get("date")); err != nil || time.Since(d) > 5*time.Minute || time.Until(d) > 5*time.Minute {
		return fmt.Errorf("date %q isn't now", r.Header.Get("date"))
	}
	var lines []string
	for _, h := range headers {
		v := r.Header.Get(h)
		switch h {
		case "(request-target)":
			v = strings.ToLower(r.Method) + " " + r.URL.RequestURI()
		case "host":
			v = r.Host
		}
		lines = append(lines, h+": "+v)
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	sig, err := base64.StdEncoding.DecodeString(m[3])
	if err != nil {
		return err
	}
	if err := rsa.VerifyPKCS1v15(o.Key, crypto.SHA256, sum[:], sig); err != nil {
		return fmt.Errorf("the signature doesn't verify: %v", err)
	}
	return nil
}

var (
	keyOnce sync.Once
	key     *rsa.PrivateKey
	keyPEM  string
)

// OCIKey is a test's API key, made once: the private half as PEM, and the
// key.
func OCIKey() (*rsa.PrivateKey, string) {
	keyOnce.Do(func() {
		key, _ = rsa.GenerateKey(rand.Reader, 2048)
		keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	})
	return key, keyPEM
}
