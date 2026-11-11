package rules

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// What a cloud's own alarm holds, changed where it is kept
// (docs/design/alarms.md): the alarm's document as --alarms reads it (its
// definition, in its file's shape), its JSON tree changed in the one place
// a change is written, and read back as rules.

// OneDescription reports whether a cloud keeps an alarm's text as one
// description, a runbook a link in it: CloudWatch's AlarmDescription,
// Azure's description, OCI's body. Google Cloud's documentation has a
// subject, a content and links.
func OneDescription(cloud string) bool {
	return cloud == "cloudwatch" || cloud == "azure" || cloud == "oci"
}

// cloudWatchDescription is how long a CloudWatch alarm's description may
// be, in characters, as its model says.
const cloudWatchDescription = 1024

// WithRunbook is a one-description cloud's text with its runbook set: the
// first link in it replaced, or, with none, the link added on a line of
// its own; unset takes the first link out, and its line with it when
// nothing else is on it.
func WithRunbook(text, url string, unset bool) string {
	loc := urlIn.FindStringIndex(text)
	switch {
	case loc == nil && unset:
		return text
	case loc == nil && strings.TrimSpace(text) == "":
		return url
	case loc == nil:
		return strings.TrimRight(text, " \t\n") + "\n" + url
	case !unset:
		return text[:loc[0]] + url + text[loc[1]:]
	}
	start := strings.LastIndexByte(text[:loc[0]], '\n') + 1
	end := len(text)
	if i := strings.IndexByte(text[loc[1]:], '\n'); i >= 0 {
		end = loc[1] + i
	}
	line := strings.TrimSpace(text[start:loc[0]] + text[loc[1]:end])
	if line == "" || strings.EqualFold(strings.TrimSuffix(line, ":"), "runbook") {
		out := text[:start] + strings.TrimPrefix(text[end:], "\n")
		return strings.TrimRight(out, "\n")
	}
	return strings.TrimRight(text[:loc[0]], " ") + text[loc[1]:]
}

// AlarmSaysAfter is what an alarm says with an annotation set or taken out,
// as its cloud keeps it: in a one-description cloud the summary is its
// name, which isn't changed here; the description is the text; a runbook,
// the first link in it. Google Cloud's are its documentation's subject,
// content and first link.
func AlarmSaysAfter(cloud string, says Says, key, v string, unset bool) (Says, error) {
	if unset {
		v = ""
	}
	if !OneDescription(cloud) {
		switch key {
		case "summary":
			says.Summary = cut(v)
		case "description":
			says.Description = cut(v)
		default:
			says.Runbook = cut(v)
		}
		return says, nil
	}
	text := says.Description
	switch key {
	case "summary":
		return says, errors.New("its summary is its name: rename it in the cloud's console")
	case "description":
		text = v
	default:
		text = WithRunbook(text, v, unset)
	}
	out := described(says.Summary, text)
	out.Template, out.Severity, out.NoSeverity = says.Template, says.Severity, says.NoSeverity
	return out, nil
}

// alarmOf is the alarm in an alarm's document, as its cloud's file has
// it, and whether it is a CloudWatch composite alarm.
func alarmOf(m map[string]any, cloud string) (map[string]any, bool) {
	switch cloud {
	case "cloudwatch":
		for _, k := range []string{"MetricAlarms", "CompositeAlarms"} {
			if l := list(m[k]); len(l) == 1 {
				a, _ := l[0].(map[string]any)
				return a, k == "CompositeAlarms"
			}
		}
		return nil, false
	case "oci":
		if l := list(m["data"]); len(l) == 1 {
			a, _ := l[0].(map[string]any)
			return a, false
		}
		return nil, false
	}
	return m, false
}

// googleCondition is the index of the condition of a Google Cloud policy
// that reads as r, -1 for none.
func googleCondition(m map[string]any, r Rule) int {
	r.Namespace, r.Source = "", ""
	for i, x := range googlePolicy(m) {
		if x.Key() == r.Key() {
			return i
		}
	}
	return -1
}

// AlarmConditions is how many conditions a Google Cloud policy holds; one
// for every other cloud's alarm.
func AlarmConditions(data []byte, cloud string) int {
	if cloud != "google" {
		return 1
	}
	m, err := decodeJSON(data)
	if err != nil {
		return 0
	}
	return len(list(m["conditions"]))
}

// EditAlarm is an alarm's document with the edit made in its tree: a new
// for written as its cloud writes one (CloudWatch's PendingPeriod in
// seconds, Azure's failingPeriods.for as ISO 8601, Google's duration as
// "600s"), a number in its query, its text, or its own off (CloudWatch's
// ActionsEnabled, Azure's and Google's enabled, OCI's isEnabled). A
// deletion is no document. r is the rule as the document reads, which
// says which of a Google policy's conditions it is; what is the policy's
// (its text, its off, deleting it) is changed only in a policy of one
// condition.
func EditAlarm(data []byte, r Rule, e Edit) ([]byte, error) {
	m, err := decodeJSON(data)
	if err != nil {
		return nil, ErrNotEditable
	}
	a, composite := alarmOf(m, r.Cloud)
	if a == nil {
		return nil, ErrNotEditable
	}
	policy := e.Delete || e.Pause || e.Annotation != ""
	if r.Cloud == "google" && policy && len(list(m["conditions"])) != 1 {
		return nil, ErrNotEditable
	}
	if composite && !e.Pause && !e.Delete {
		return nil, ErrNotEditable
	}
	switch {
	case e.Delete:
		return []byte{}, nil
	case e.Pause:
		switch r.Cloud {
		case "cloudwatch":
			a["ActionsEnabled"] = false
		case "azure":
			props, _ := a["properties"].(map[string]any)
			if props == nil {
				return nil, ErrNotEditable
			}
			props["enabled"] = false
		case "google":
			a["enabled"] = false
		case "oci":
			a["isEnabled"] = false
		default:
			return nil, ErrNotEditable
		}
	case e.Annotation != "":
		if err := editAlarmText(a, r, e); err != nil {
			return nil, err
		}
	default:
		q, err := alarmQuery(a, r)
		if err != nil {
			return nil, err
		}
		if e.For != nil {
			if err := q.setFor(*e.For); err != nil {
				return nil, err
			}
		}
		if e.From != "" {
			x, _ := q.m[q.key].(string)
			if strings.Count(x, e.From) != 1 {
				return nil, ErrNotEditable
			}
			q.m[q.key] = strings.Replace(x, e.From, e.To, 1)
		}
	}
	return canon(m)
}

// query is where an alarm keeps its PromQL query: the map and its key, and
// how its for is written beside it.
type query struct {
	m      map[string]any
	key    string
	setFor func(time.Duration) error
}

// alarmQuery is where a PromQL alarm keeps its query and its for:
// CloudWatch's PromQLCriteria, an Azure metric alert's one criterion and
// its failingPeriods, a Google policy's condition.
func alarmQuery(a map[string]any, r Rule) (query, error) {
	whole := func(d time.Duration, most time.Duration) error {
		if d < 0 || d > most || d%time.Second != 0 {
			return ErrNotEditable
		}
		return nil
	}
	switch r.Cloud {
	case "cloudwatch":
		crit, _ := a["EvaluationCriteria"].(map[string]any)
		pq, _ := crit["PromQLCriteria"].(map[string]any)
		if pq == nil {
			return query{}, ErrNotEditable
		}
		return query{pq, "Query", func(d time.Duration) error {
			if err := whole(d, 86400*time.Second); err != nil {
				return err
			}
			pq["PendingPeriod"] = json.Number(fmt.Sprint(int64(d / time.Second)))
			return nil
		}}, nil
	case "azure":
		props, _ := a["properties"].(map[string]any)
		crit, _ := props["criteria"].(map[string]any)
		all := list(crit["allOf"])
		if str(crit["odata.type"]) != AzurePromQL || len(all) != 1 {
			return query{}, ErrNotEditable
		}
		c, _ := all[0].(map[string]any)
		if c == nil {
			return query{}, ErrNotEditable
		}
		return query{c, "query", func(d time.Duration) error {
			fp, _ := crit["failingPeriods"].(map[string]any)
			if fp == nil {
				fp = map[string]any{}
				crit["failingPeriods"] = fp
			}
			fp["for"] = ISODuration(d)
			return nil
		}}, nil
	case "google":
		i := googleCondition(a, r)
		if i < 0 {
			return query{}, ErrNotEditable
		}
		c, _ := list(a["conditions"])[i].(map[string]any)
		pq, _ := c["conditionPrometheusQueryLanguage"].(map[string]any)
		if pq == nil {
			return query{}, ErrNotEditable
		}
		return query{pq, "query", func(d time.Duration) error {
			if err := whole(d, never); err != nil {
				return err
			}
			pq["duration"] = fmt.Sprintf("%ds", int64(d/time.Second))
			return nil
		}}, nil
	}
	return query{}, ErrNotEditable
}

// editAlarmText sets or takes out what an alarm says: a one-description
// cloud's text, the runbook a link in it; Google's documentation.
func editAlarmText(a map[string]any, r Rule, e Edit) error {
	v := e.Value
	if e.Unset {
		v = ""
	}
	if OneDescription(r.Cloud) {
		var at map[string]any
		var key string
		switch r.Cloud {
		case "cloudwatch":
			at, key = a, "AlarmDescription"
		case "azure":
			at, _ = a["properties"].(map[string]any)
			key = "description"
		case "oci":
			at, key = a, "body"
		}
		if at == nil {
			return ErrNotEditable
		}
		text := str(at[key])
		switch e.Annotation {
		case "description":
			text = v
		case "runbook_url", "runbook":
			text = WithRunbook(text, v, e.Unset)
		default:
			return ErrNotEditable // its summary is its name
		}
		if r.Cloud == "cloudwatch" && utf8.RuneCountInString(text) > cloudWatchDescription {
			return ErrNotEditable
		}
		at[key] = text
		return nil
	}
	doc, _ := a["documentation"].(map[string]any)
	if doc == nil {
		doc = map[string]any{"mimeType": "text/markdown"}
		a["documentation"] = doc
	}
	set := func(k string) {
		if e.Unset {
			delete(doc, k)
		} else {
			doc[k] = v
		}
	}
	switch e.Annotation {
	case "summary":
		set("subject")
	case "description":
		set("content")
	case "runbook_url", "runbook":
		links := list(doc["links"])
		switch {
		case len(links) > 0 && e.Unset:
			doc["links"] = links[1:]
			if len(links) == 1 {
				delete(doc, "links")
			}
		case len(links) > 0:
			l, _ := links[0].(map[string]any)
			if l == nil {
				return ErrNotEditable
			}
			l["url"] = v
		case e.Unset:
			doc["content"] = WithRunbook(str(doc["content"]), "", true)
		default:
			doc["links"] = []any{map[string]any{"displayName": "Runbook", "url": v}}
		}
	default:
		return ErrNotEditable
	}
	return nil
}

// alarmPath is a field of an alarm a change writes: under the alarm, or,
// cond, under the Google condition the change is to.
type alarmPath struct {
	cond bool
	path []string
}

// alarmPaths are the fields of an alarm an edit of e's kind writes, and an
// undo of it puts back: its off; its text; its for and its query.
func alarmPaths(cloud string, e Edit) []alarmPath {
	one := func(path ...string) []alarmPath { return []alarmPath{{path: path}} }
	switch {
	case e.Pause:
		switch cloud {
		case "cloudwatch":
			return one("ActionsEnabled")
		case "azure":
			return one("properties", "enabled")
		case "google":
			return one("enabled")
		case "oci":
			return one("isEnabled")
		}
	case e.Annotation != "":
		switch cloud {
		case "cloudwatch":
			return one("AlarmDescription")
		case "azure":
			return one("properties", "description")
		case "google":
			return one("documentation")
		case "oci":
			return one("body")
		}
	default:
		switch cloud {
		case "cloudwatch":
			return []alarmPath{{path: []string{"EvaluationCriteria", "PromQLCriteria", "Query"}},
				{path: []string{"EvaluationCriteria", "PromQLCriteria", "PendingPeriod"}}}
		case "azure":
			return one("properties", "criteria")
		case "google":
			return []alarmPath{{cond: true, path: []string{"conditionPrometheusQueryLanguage", "query"}},
				{cond: true, path: []string{"conditionPrometheusQueryLanguage", "duration"}}}
		}
	}
	return nil
}

// alarmBase is where an alarm path is: the alarm, or the condition named
// cond; nil for none.
func alarmBase(a map[string]any, p alarmPath, cond string) map[string]any {
	if !p.cond {
		return a
	}
	for _, c := range list(a["conditions"]) {
		if cm, _ := c.(map[string]any); cm != nil && cond != "" && str(cm["name"]) == cond {
			return cm
		}
	}
	return nil
}

func getAt(m map[string]any, path []string) (any, bool) {
	for _, k := range path[:len(path)-1] {
		if m, _ = m[k].(map[string]any); m == nil {
			return nil, false
		}
	}
	v, ok := m[path[len(path)-1]]
	return v, ok
}

func delAt(m map[string]any, path []string) {
	for _, k := range path[:len(path)-1] {
		if m, _ = m[k].(map[string]any); m == nil {
			return
		}
	}
	delete(m, path[len(path)-1])
}

// setAt sets a field of m to v, or takes it out when ok is false, making
// what is above it.
func setAt(m map[string]any, path []string, v any, ok bool) {
	for _, k := range path[:len(path)-1] {
		t, _ := m[k].(map[string]any)
		if t == nil {
			if !ok {
				return
			}
			t = map[string]any{}
			m[k] = t
		}
		m = t
	}
	if ok {
		m[path[len(path)-1]] = v
	} else {
		delete(m, path[len(path)-1])
	}
}

// PutBackAlarm is an alarm as it is now with what an edit changed put back
// as it was: the fields it wrote, from the alarm as it was kept; every
// other field as it is now, so a person's change since to the rest stays.
// A deletion put back is the alarm as it was, whole. A Google condition is
// found by its name, which it keeps; cond is the name of the condition the
// edit changed, "" for a change to the policy.
func PutBackAlarm(now, was []byte, cloud, cond string, e Edit) ([]byte, error) {
	if e.Delete {
		if _, err := decodeJSON(was); err != nil {
			return nil, ErrNotEditable
		}
		return CanonJSON(was)
	}
	n, err1 := decodeJSON(now)
	w, err2 := decodeJSON(was)
	if err1 != nil || err2 != nil {
		return nil, ErrNotEditable
	}
	na, _ := alarmOf(n, cloud)
	wa, _ := alarmOf(w, cloud)
	paths := alarmPaths(cloud, e)
	if na == nil || wa == nil || len(paths) == 0 {
		return nil, ErrNotEditable
	}
	for _, p := range paths {
		to, from := alarmBase(na, p, cond), alarmBase(wa, p, cond)
		if to == nil || from == nil {
			return nil, ErrNotEditable
		}
		v, ok := getAt(from, p.path)
		setAt(to, p.path, v, ok)
	}
	return canon(n)
}

// normTree is a decoded alarm's tree as compared: its numbers as numbers,
// whatever their form, and a for or a duration as a length of time.
func normTree(v any, key string) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, y := range x {
			out[k] = normTree(y, k)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, y := range x {
			out[i] = normTree(y, key)
		}
		return out
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f
		}
	case string:
		if key == "for" || key == "duration" || key == "timeToResolve" {
			if d, err := isoDuration(x); err == nil {
				return int64(d)
			}
		}
	}
	return v
}

// AlarmFields are the fields of an alarm an edit of e's kind writes, as a
// document holds them, durations as lengths of time: an undo puts them
// back only over them as the change left them. ok is false for a document
// without the alarm, or without the condition cond.
func AlarmFields(raw []byte, cloud, cond string, e Edit) (map[string]any, bool) {
	m, err := decodeJSON(raw)
	if err != nil {
		return nil, false
	}
	a, _ := alarmOf(m, cloud)
	paths := alarmPaths(cloud, e)
	if a == nil || len(paths) == 0 {
		return nil, false
	}
	out := map[string]any{}
	for _, p := range paths {
		b := alarmBase(a, p, cond)
		if b == nil {
			return nil, false
		}
		v, ok := getAt(b, p.path)
		if ok {
			out[strings.Join(p.path, ".")] = normTree(v, p.path[len(p.path)-1])
		}
	}
	return out, true
}

// AlarmOnly reports whether out is now but for the fields an edit of e's
// kind writes: an undo puts back those fields, never anything else.
func AlarmOnly(now, out []byte, cloud, cond string, e Edit) bool {
	n, err1 := decodeJSON(now)
	o, err2 := decodeJSON(out)
	if err1 != nil || err2 != nil {
		return false
	}
	na, _ := alarmOf(n, cloud)
	oa, _ := alarmOf(o, cloud)
	paths := alarmPaths(cloud, e)
	if na == nil || oa == nil || len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		x, y := alarmBase(na, p, cond), alarmBase(oa, p, cond)
		if x == nil || y == nil {
			return false
		}
		delAt(x, p.path)
		delAt(y, p.path)
	}
	return reflect.DeepEqual(normTree(n, ""), normTree(o, ""))
}

// GoogleConditionName is the name of the condition of a Google Cloud
// policy that reads as r, "" for none.
func GoogleConditionName(data []byte, r Rule) string {
	m, err := decodeJSON(data)
	if err != nil {
		return ""
	}
	i := googleCondition(m, r)
	if i < 0 {
		return ""
	}
	c, _ := list(m["conditions"])[i].(map[string]any)
	return str(c["name"])
}

// alarmWritten are the fields of an alarm a change writes, by cloud: left
// out of what AlarmShape hashes, as its rules say them; with what the
// cloud sets itself on an alarm made again.
var alarmWritten = map[string][][]string{
	"cloudwatch": {{"AlarmDescription"}, {"ActionsEnabled"}, {"EvaluationCriteria", "PromQLCriteria", "Query"},
		{"EvaluationCriteria", "PromQLCriteria", "PendingPeriod"}, {"Tags"}},
	"azure":  {{"properties", "description"}, {"properties", "enabled"}, {"properties", "criteria", "failingPeriods", "for"}},
	"google": {{"name"}, {"creationRecord"}, {"documentation"}, {"enabled"}},
	"oci":    {{"body"}, {"isEnabled"}},
}

// AlarmShape is what an alarm's document holds, but for how it is written:
// its rules, each as it fires and says, and the rest of its tree with the
// fields a change writes taken out. A cloud that writes a duration or a
// query its own way reads alike; an alarm made again, with a new name
// (Google), reads as it was.
func AlarmShape(raw []byte, cloud string) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	rs, err := Parse(raw)
	if err != nil {
		return "", err
	}
	var alerts []string
	for _, r := range rs {
		b, _ := json.Marshal(r.Says)
		alerts = append(alerts, r.Key()+"\x00"+string(b))
	}
	slices.Sort(alerts)
	m, err := decodeJSON(raw)
	if err != nil {
		return "", err
	}
	a, _ := alarmOf(m, cloud)
	if a == nil {
		return "", ErrNotEditable
	}
	drop := func(at map[string]any, path []string) {
		for _, k := range path[:len(path)-1] {
			at, _ = at[k].(map[string]any)
			if at == nil {
				return
			}
		}
		delete(at, path[len(path)-1])
	}
	for _, p := range alarmWritten[cloud] {
		drop(a, p)
	}
	if cloud == "azure" {
		if crit := dig(a, "properties", "criteria"); crit != nil {
			for _, c := range list(crit["allOf"]) {
				if cm, _ := c.(map[string]any); cm != nil {
					delete(cm, "query")
				}
			}
		}
	}
	if cloud == "google" {
		for _, c := range list(a["conditions"]) {
			if cm, _ := c.(map[string]any); cm != nil {
				delete(cm, "name")
				if pq, _ := cm["conditionPrometheusQueryLanguage"].(map[string]any); pq != nil {
					delete(pq, "query")
					delete(pq, "duration")
				}
			}
		}
	}
	b, err := json.Marshal(plainTree(m))
	if err != nil {
		return "", err
	}
	return strings.Join(alerts, "\n") + "\n" + Hash(b), nil
}

func dig(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m, _ = m[k].(map[string]any)
	}
	return m
}

// EditAlarmThreshold is EditAlarm with v written in place of b's number in
// r's query, as EditThreshold writes it into a file.
func EditAlarmThreshold(data []byte, r Rule, e Edit, b Bound, v float64) ([]byte, error) {
	cs := comparisons(r.Expr)
	if r.Graph != nil || b.At < 0 || b.At >= len(cs) {
		return nil, ErrNotEditable
	}
	c := cs[b.At]
	after := r.WithBound(b, v)
	if after.Expr == r.Expr {
		return nil, ErrNotEditable
	}
	n := after.Expr[c.at : len(after.Expr)-(len(r.Expr)-c.end)]
	for k := 0; k <= 16 && k <= c.at; k++ {
		before := r.Expr[c.at-k : c.at]
		e.From, e.To = before+r.Expr[c.at:c.end], before+n
		out, err := EditAlarm(data, r, e)
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, ErrNotEditable) {
			return nil, err
		}
	}
	return nil, ErrNotEditable
}

// AlarmName is the name an alarm's document gives it, as --changes-in
// names it: CloudWatch's alarm name, Azure's RG/NAME, Google's policy's
// display name, OCI's display name.
func AlarmName(data []byte, cloud string) string {
	m, err := decodeJSON(data)
	if err != nil {
		return ""
	}
	a, _ := alarmOf(m, cloud)
	switch cloud {
	case "cloudwatch":
		return str(a["AlarmName"])
	case "azure":
		if x := azureAlertID.FindStringSubmatch(str(a["id"])); x != nil {
			return x[1] + "/" + str(a["name"])
		}
	case "google", "oci":
		return str(a["displayName"])
	}
	return ""
}
