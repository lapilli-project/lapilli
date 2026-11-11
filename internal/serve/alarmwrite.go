package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
)

// Changing a cloud's own alarm through its API (docs/design/alarms.md):
// CloudWatch's alarms, Azure Monitor's alert rules, Google Cloud
// Monitoring's alerting policies and OCI Monitoring's alarms, read with
// --alarms SOURCE=URL, changed only where --changes-in names them as
// SOURCE:NAME, with the write identity given for the cloud.

// Why a cloud's alarm is for a person to change: shown on its proposal's
// page.
const (
	noPathAlarmFile   = "it was read from a file: read it with --alarms to change it through the cloud"
	noPathAlarmIn     = "it isn't one --changes-in names: name it there as SOURCE:NAME"
	noPathAlarmName   = "the cloud names it so that Lapilli doesn't write to it"
	noPathAlarmCFN    = "CloudFormation keeps it, as its tag aws:cloudformation:stack-id says: change it in the stack's template"
	noPathCNRM        = "Config Connector keeps it, as its label managed-by-cnrm says: change it where its resource is kept"
	noPathTerraform   = "Terraform keeps it, as its label goog-terraform-provisioned says: change it in its configuration"
	noPathAutomation  = "its actions do more than tell (Auto Scaling, EC2): it is automation, changed where that is kept"
	noPathWarmUp      = "it has a warm-up, which rewriting it would start again, quieting it: change it in the console"
	noPathComposite   = "a composite alarm is turned off and on here, nothing more: change it in the console"
	noPathWatched     = "a composite alarm watches it: delete it in the console, the composite alarm with it"
	noPathUnknown     = "CloudWatch gave a field this Lapilli doesn't know, which writing the alarm back would lose: change it in the console, or upgrade"
	noPathAzureFields = "Azure gave a field this Lapilli doesn't know, which writing the alert rule back would lose: change it in the portal, or upgrade"
	noPathConditions  = "its policy has several conditions: what it says, its off and deleting it are the policy's, changed in the console"
	noPathSummary     = "its summary is its name: rename it in the console"
	noPathAlarmEdit   = "the change can't be written into the alarm as the cloud has it"
)

var alarmNoPaths = []string{noPathAlarmFile, noPathAlarmIn, noPathAlarmName, noPathAlarmCFN, noPathCNRM, noPathTerraform,
	noPathAutomation, noPathWarmUp, noPathComposite, noPathWatched, noPathUnknown, noPathAzureFields, noPathConditions,
	noPathSummary, noPathAlarmEdit}

// errMovedSince is an alarm written to by someone else between its read
// and Lapilli's write: it is read and checked again, once.
var errMovedSince = errors.New("the alarm changed in the cloud a moment before the change was sent")

// alarmTarget is --alarms's.
type alarmTarget struct{ s *Server }

func (t alarmTarget) path() string     { return ledger.PathAlarm }
func (t alarmTarget) where() string    { return "the cloud" }
func (t alarmTarget) gone() string     { return "the alarm is gone from the cloud" }
func (t alarmTarget) cantEdit() string { return noPathAlarmEdit }
func (t alarmTarget) again() string {
	return "an alarm of its name is in the cloud again, which making it again would overwrite: undo it in the console"
}

func (t alarmTarget) goneUndo() string {
	return "the alarm is gone from the cloud: undo it in the console"
}
func (t alarmTarget) notThere() string { return notThereAlarm }

// lag: a cloud may be read a moment behind a write it took.
func (t alarmTarget) lag() (time.Duration, string) { return readBackWithin, readBehind }

// oneUnit: the same alarm, or Google policy.
func (t alarmTarget) oneUnit(q ledger.Proposal, _ rules.Edit, p ledger.Proposal, _ rules.Edit) bool {
	return strings.EqualFold(q.Source, p.Source)
}

// condOf is the name of the Google condition the rule a proposal's hash
// names is in a unit, "" for a change to the policy or another cloud's.
func (t alarmTarget) condOf(u apiUnit, p ledger.Proposal, hash string, e rules.Edit) string {
	a := t.s.alarmsOf(u.ns)
	if a == nil || a.Kind != source.AlarmsGoogle || e.Delete || e.Pause || e.Annotation != "" {
		return ""
	}
	rs, _ := t.rules(u)
	for _, r := range rs {
		if hashOf(p, r) == hash {
			r.Namespace, r.Source = "", ""
			return rules.GoogleConditionName(u.Raw, r)
		}
	}
	return ""
}

// goneAllowed: an alarm made again is asked of as it was kept: its name,
// for --changes-in, and its tags or labels, for another tool's.
func (t alarmTarget) goneAllowed(p ledger.Proposal, e rules.Edit) string {
	a := t.s.alarmsOf(e.Namespace)
	if a == nil {
		return noPathAlarmFile
	}
	if !t.s.changesInAlarm(a.Source, rules.AlarmName([]byte(p.Backup), cloudOf[a.Kind]), a.Kind) {
		return noPathAlarmIn
	}
	return t.managed(apiUnit{alarm: source.AlarmUnit{Tags: keptTags([]byte(p.Backup), a.Kind)}})
}

// keptTags are an alarm's tags or labels as its document kept them:
// CloudWatch's (kept with it for this), Azure's, Google's user labels,
// OCI's freeform and defined tags.
func keptTags(raw []byte, kind string) map[string]string {
	m, err := decodeTree(raw)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	flat := func(v any, prefix string) {
		if tm, ok := v.(map[string]any); ok {
			for k, x := range tm {
				out[prefix+k] = fmt.Sprint(x)
			}
		}
	}
	switch kind {
	case source.AlarmsAWS:
		alarm, _ := cwAlarmOf(m)
		for _, x := range list(alarm["Tags"]) {
			if tm, _ := x.(map[string]any); tm != nil {
				out[str(tm["Key"])] = str(tm["Value"])
			}
		}
	case source.AlarmsAzure:
		flat(m["tags"], "")
	case source.AlarmsGoogle:
		flat(m["userLabels"], "")
	case source.AlarmsOCI:
		if l := list(m["data"]); len(l) == 1 {
			a, _ := l[0].(map[string]any)
			flat(a["freeformTags"], "")
			if d, ok := a["definedTags"].(map[string]any); ok {
				for ns, v := range d {
					flat(v, ns+".")
				}
			}
		}
	}
	return out
}

// leftAsChanged: the fields the change wrote read now as it left them.
func (t alarmTarget) leftAsChanged(orig ledger.Proposal, u apiUnit, p ledger.Proposal, e rules.Edit) bool {
	a := t.s.alarmsOf(u.ns)
	if a == nil {
		return false
	}
	cond := t.condOf(u, p, p.BeforeHash, e)
	left, ok1 := rules.AlarmFields([]byte(orig.Document), cloudOf[a.Kind], cond, e)
	now, ok2 := rules.AlarmFields(u.Raw, cloudOf[a.Kind], cond, e)
	return ok1 && ok2 && reflect.DeepEqual(left, now)
}

// onlyRule: the fields the change wrote put back, and nothing else; a
// deleted alarm made again whole.
func (t alarmTarget) onlyRule(u apiUnit, out []byte, p ledger.Proposal, e rules.Edit) bool {
	a := t.s.alarmsOf(u.ns)
	if a == nil {
		return false
	}
	if u.Gone {
		return e.Delete
	}
	return rules.AlarmOnly(u.Raw, out, cloudOf[a.Kind], t.condOf(u, p, p.BeforeHash, e), e)
}

// cloud is the kind of rule an alarm of a source is, as rules.Parse names
// it.
var cloudOf = map[string]string{source.AlarmsAWS: "cloudwatch", source.AlarmsAzure: "azure", source.AlarmsGoogle: "google", source.AlarmsOCI: "oci"}

// alarmKept reports whether a rule was read through --alarms: changed
// through its cloud's API, where --changes-in lets it be.
func (s *Server) alarmKept(f found) bool {
	return f.rule.Cloud != "" && f.doc.Namespace != "" && f.rule.Namespace == f.doc.Namespace && s.alarmsOf(f.doc.Namespace) != nil
}

// alarmsEntry reports whether an entry of --changes-in is an --alarms
// source's: SOURCE:PATTERN.
func (s *Server) alarmsEntry(pat string) bool {
	src, _, ok := strings.Cut(pat, ":")
	return ok && s.alarmsOf(src) != nil
}

func (t alarmTarget) unitName(src string, e rules.Edit) string {
	if a := t.s.alarmsOf(e.Namespace); a != nil {
		if d, ok := a.Doc(src); ok {
			return e.Namespace + "/" + rules.AlarmName(d.Data, cloudOf[a.Kind])
		}
	}
	return e.Namespace + "/" + e.Name
}

// changesInAlarm reports whether --changes-in names an alarm: SOURCE:NAME
// or SOURCE:PREFIX*, the name its cloud's: CloudWatch's alarm name,
// Azure's RG/NAME in any letter case, Google's policy's display name,
// OCI's display name.
func (s *Server) changesInAlarm(src, name string, kind string) bool {
	for _, pat := range s.cfg.ChangesIn {
		x, p, ok := strings.Cut(pat, ":")
		if ok && x == src && namedBy(p, name, kind == source.AlarmsAzure) {
			return true
		}
	}
	return false
}

func (t alarmTarget) allowed(src string, e rules.Edit) string {
	a := t.s.alarmsOf(e.Namespace)
	switch {
	case a == nil:
		return noPathAlarmFile
	case a.OwnsID(src) != nil:
		return noPathAlarmName
	}
	// The name as last read, when it was: the alarm read again is asked
	// too (offered), so a restart before the first read refuses nothing.
	if d, ok := a.Doc(src); ok && !t.s.changesInAlarm(a.Source, rules.AlarmName(d.Data, cloudOf[a.Kind]), a.Kind) {
		return noPathAlarmIn
	}
	return ""
}

func (t alarmTarget) owns(src string, e rules.Edit) error {
	a := t.s.alarmsOf(e.Namespace)
	if a == nil {
		return source.ErrAlarmElsewhere
	}
	return a.OwnsID(src)
}

func (t alarmTarget) unit(ctx context.Context, src string, e rules.Edit) (apiUnit, error) {
	a := t.s.alarmsOf(e.Namespace)
	if a == nil {
		return apiUnit{}, source.ErrAlarmElsewhere
	}
	u, err := a.Unit(ctx, src, a.Reads)
	return apiUnit{Raw: u.Raw, Hash: u.Hash, Gone: u.Gone, alarm: u, ns: a.Source}, err
}

// managed is why another tool keeps an alarm, as its tags or labels say:
// CloudFormation's own tag, Config Connector's and Terraform's labels,
// and those --changes-managed names.
func (t alarmTarget) managed(u apiUnit) string {
	for k, v := range u.alarm.Tags {
		switch {
		case strings.EqualFold(k, "aws:cloudformation:stack-id"):
			return noPathAlarmCFN
		case k == "managed-by-cnrm" && v == "true":
			return noPathCNRM
		case k == "goog-terraform-provisioned" && v == "true":
			return noPathTerraform
		}
	}
	if managedBy(t.s.cfg.ChangesManaged, u.alarm.Tags) {
		return noPathManaged
	}
	return ""
}

// ready says the cloud's own state fires: turning it off, or deleting it,
// waits. CloudWatch's is on the alarm as read (its ALARM); every other
// cloud's is read apart: Azure's alerts, Google's open alerts, OCI's
// alarm status.
func (t alarmTarget) ready(ctx context.Context, u apiUnit, p ledger.Proposal) error {
	if p.Kind != ledger.KindDisable && p.Kind != ledger.KindDelete {
		return nil
	}
	if u.alarm.Firing {
		return errFires
	}
	a := t.s.alarmsOf(u.ns)
	if a == nil || a.Kind == source.AlarmsAWS {
		return nil
	}
	if fires, err := a.Fires(ctx, u.alarm.ID); err != nil {
		return fmt.Errorf("whether it fires isn't known: %w", err)
	} else if fires {
		return errFires
	}
	return nil
}

// telling are the actions an alarm's are when they only tell: SNS topics,
// Lambda functions, Systems Manager's OpsItems and Incident Manager's
// response plans.
var telling = regexp.MustCompile(`^arn:aws(?:-cn|-us-gov)?:(?:sns|lambda|ssm|ssm-incidents):`)

// offered says why a change of a kind isn't offered for an alarm, "" when
// it is: what CloudWatch's alarms that are automation, warm up or are
// composite allow; a Google policy of several conditions; a field
// CloudWatch gave that this Lapilli doesn't know.
func (t alarmTarget) offered(ctx context.Context, u apiUnit, r rules.Rule, kind string) (string, error) {
	a := t.s.alarmsOf(u.ns)
	if a == nil {
		return noPathAlarmFile, nil
	}
	m, err := decodeTree(u.Raw)
	if err != nil {
		return noPathAlarmEdit, nil
	}
	if !t.s.changesInAlarm(a.Source, rules.AlarmName(u.Raw, cloudOf[a.Kind]), a.Kind) {
		return noPathAlarmIn, nil // as it is named now
	}
	switch a.Kind {
	case source.AlarmsAWS:
		alarm, composite := cwAlarmOf(m)
		for _, k := range []string{"AlarmActions", "OKActions", "InsufficientDataActions"} {
			for _, x := range list(alarm[k]) {
				if s, _ := x.(string); !telling.MatchString(s) {
					return noPathAutomation, nil
				}
			}
		}
		rewrites := kind == ledger.KindFor || kind == ledger.KindThreshold || kind == ledger.KindSays
		switch {
		case composite && kind != ledger.KindDisable:
			return noPathComposite, nil
		case rewrites && alarm["WarmUpConfiguration"] != nil:
			return noPathWarmUp, nil
		case rewrites && unknownField(alarm, cloudWatchKnown) != "":
			return noPathUnknown, nil
		case kind == ledger.KindDelete:
			watched, err := cwWatched(ctx, a, alarm)
			if err != nil {
				return "", err
			}
			if watched {
				return noPathWatched, nil
			}
		}
	case source.AlarmsAzure:
		if kind == ledger.KindSays && strings.EqualFold(str(m["type"]), "Microsoft.Insights/activityLogAlerts") &&
			unknownField(m, activityLogKnown) != "" {
			return noPathAzureFields, nil
		}
	case source.AlarmsGoogle:
		policy := kind == ledger.KindSays || kind == ledger.KindDisable || kind == ledger.KindDelete
		if policy && rules.AlarmConditions(u.Raw, "google") != 1 {
			return noPathConditions, nil
		}
	}
	return "", nil
}

func (t alarmTarget) rules(u apiUnit) ([]rules.Rule, error) {
	if len(u.Raw) == 0 {
		return nil, nil
	}
	return rules.FromDocs([]rules.Doc{{Source: u.alarm.ID, Namespace: u.ns, Data: u.Raw}})
}

func (t alarmTarget) edit(u apiUnit, r rules.Rule, e rules.Edit, kind string, b rules.Bound, v float64) ([]byte, string) {
	if kind == ledger.KindSays && e.Annotation == "summary" && rules.OneDescription(r.Cloud) {
		return nil, noPathSummary
	}
	var out []byte
	var err error
	if kind == ledger.KindThreshold {
		out, err = rules.EditAlarmThreshold(u.Raw, r, e, b, v)
	} else {
		out, err = rules.EditAlarm(u.Raw, r, e)
	}
	if err != nil {
		return nil, noPathAlarmEdit
	}
	return out, ""
}

func (t alarmTarget) reads(before, after []byte, r, want rules.Rule, kind string) (*rules.Rule, bool) {
	if kind == ledger.KindDelete {
		return nil, len(after) == 0
	}
	return rulerTarget{}.reads(before, after, r, want, kind)
}

// backup is what a write of an alarm keeps for undo: the alarm as it was,
// with CloudWatch's tags, which making a deleted alarm again needs.
func (t alarmTarget) backup(u apiUnit) []byte {
	if len(u.alarm.Tags) == 0 || !bytes.Contains(u.Raw, []byte(`"MetricAlarms"`)) {
		return u.Raw
	}
	m, err := decodeTree(u.Raw)
	if err != nil {
		return u.Raw
	}
	alarm, _ := cwAlarmOf(m)
	var tags []any
	for _, k := range slices.Sorted(maps.Keys(u.alarm.Tags)) {
		tags = append(tags, map[string]any{"Key": k, "Value": u.alarm.Tags[k]})
	}
	alarm["Tags"] = tags
	b, err := json.Marshal(m)
	if err != nil {
		return u.Raw
	}
	out, err := rules.CanonJSON(b)
	if err != nil {
		return u.Raw
	}
	return out
}

func (t alarmTarget) putBack(u apiUnit, p ledger.Proposal, e rules.Edit) ([]byte, error) {
	a := t.s.alarmsOf(u.ns)
	if a == nil {
		return nil, source.ErrAlarmElsewhere
	}
	return rules.PutBackAlarm(u.Raw, []byte(p.Backup), cloudOf[a.Kind], t.condOf(u, p, p.BeforeHash, e), e)
}

// undoGone: a deleted alarm is made again from what was kept.
func (t alarmTarget) undoGone(_ apiUnit, p ledger.Proposal) bool { return p.BeforeHash == "" }

func (t alarmTarget) settling(apiUnit) (string, string) { return "", "" }

// same reports whether an alarm read back holds what raw holds: its rules,
// each as it fires and says, and the rest alike, however the cloud writes
// it; none, when it is gone.
func (t alarmTarget) same(u apiUnit, raw []byte) bool {
	if len(raw) == 0 {
		return u.Gone
	}
	a := t.s.alarmsOf(u.ns)
	if u.Gone || a == nil {
		return false
	}
	x, err1 := rules.AlarmShape(u.Raw, cloudOf[a.Kind])
	y, err2 := rules.AlarmShape(raw, cloudOf[a.Kind])
	return err1 == nil && err2 == nil && x == y
}

// readBack reads back the alarm a change was sent to. A Google policy
// made again has a new name, kept on the proposal from the create's answer
// (madeAs); one whose answer was lost is found by what it holds, and only
// when one policy alone holds it.
func (t alarmTarget) readBack(ctx context.Context, p ledger.Proposal, e rules.Edit) (apiUnit, error) {
	a := t.s.alarmsOf(e.Namespace)
	if a == nil {
		return apiUnit{}, source.ErrAlarmElsewhere
	}
	u, err := t.unit(ctx, p.Source, e)
	if err != nil || !u.Gone || a.Kind != source.AlarmsGoogle || p.Kind != ledger.KindUndo || p.BeforeHash != "" {
		return u, err
	}
	if q, err := t.s.l.ProposalByID(ctx, p.ID); err == nil {
		p = q // the name the create answered, kept since
	}
	if m, err := decodeTree([]byte(p.Document)); err == nil {
		if name := str(m["name"]); name != p.Source && a.OwnsID(name) == nil {
			return t.unit(ctx, name, e)
		}
	}
	_, docs, err := a.Read(ctx)
	if err != nil {
		return u, err
	}
	want, err := rules.AlarmShape([]byte(p.Document), "google")
	if err != nil {
		return u, err
	}
	var found []string
	for _, d := range docs {
		if got, err := rules.AlarmShape(d.Data, "google"); err == nil && got == want {
			found = append(found, d.Source)
		}
	}
	switch len(found) {
	case 0:
		return u, nil
	case 1:
		return t.unit(ctx, found[0], e)
	}
	return u, errors.New("more than one policy holds what was made again, and which is Lapilli's isn't known")
}

// googleAgain is why a deleted Google policy isn't made again: one of its
// display name is there, as a person may have made it in the console
// meanwhile. One holding what it held has its display name too.
func (t alarmTarget) googleAgain(ctx context.Context, a *source.Alarms, doc []byte) (string, error) {
	_, docs, err := a.Read(ctx)
	if err != nil {
		return "", err
	}
	name := rules.AlarmName(doc, "google")
	for _, d := range docs {
		if rules.AlarmName(d.Data, "google") == name {
			return "a policy of its display name is in the project again, which making it again would double: undo it in the console", nil
		}
	}
	return "", nil
}

// madeAs keeps on an undo the name of the Google policy it made again, as
// the create answered it, so it is read back there.
func (t alarmTarget) madeAs(ctx context.Context, p ledger.Proposal, a *source.Alarms, doc, answer []byte) {
	ans, err := decodeTree(answer)
	name := str(ans["name"])
	if err != nil || a.OwnsID(name) != nil {
		return
	}
	m, err := decodeTree(doc)
	if err != nil {
		return
	}
	m["name"] = name
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	if out, err := rules.CanonJSON(b); err == nil {
		t.s.l.Move(ctx, p.ID, []string{ledger.StateApplying}, ledger.StateApplying, ledger.Proposer,
			ledger.Step{Document: string(out), DocHash: p.DocHash, Diff: p.Diff})
	}
}

// request is what is sent to the cloud, worked out from the alarm as just
// read and as it is to be: a deletion, an alarm made again, its off and on
// where the cloud has calls of their own (CloudWatch), or the fields
// changed; every request to the --alarms host. CloudWatch's and Google's
// alarms are read once more just before, as they have no version a write
// can be made over: one written meanwhile is checked again (errMovedSince).
func (t alarmTarget) request(ctx context.Context, p ledger.Proposal, u apiUnit, doc []byte) (func(context.Context, ledger.Proposal) error, error) {
	s := t.s
	a := s.alarmsOf(u.ns)
	if a == nil {
		return nil, refusal{"lapilli serve no longer reads the alarm's --alarms"}
	}
	writes := s.alarmWrites[a.Source]
	id := u.alarm.ID
	var send func(context.Context) (int, []byte, error)
	ok := []int{http.StatusOK}
	switch a.Kind {
	case source.AlarmsAWS:
		name := source.AlarmName(a.Kind, id)
		action, in, err := cwRequest(name, u, doc)
		if err != nil {
			return nil, err
		}
		send = func(ctx context.Context) (int, []byte, error) {
			return a.AWSCall(ctx, action, in, writes, source.UnitLimit)
		}
	case source.AlarmsAzure:
		at, err := a.AzureURL(id)
		if err != nil {
			return nil, refusal{err.Error()}
		}
		method, body, err := azureRequest(u, doc)
		if err != nil {
			return nil, err
		}
		h := http.Header{}
		if u.alarm.Etag != "" && strings.Contains(strings.ToLower(id), "/scheduledqueryrules/") {
			h.Set("If-Match", u.alarm.Etag)
		}
		switch method {
		case http.MethodPut:
			ok = []int{http.StatusOK, http.StatusCreated}
		case http.MethodDelete:
			ok = []int{http.StatusOK, http.StatusNoContent}
		}
		send = func(ctx context.Context) (int, []byte, error) {
			code, _, out, err := a.Call(ctx, method, at, body, h, writes, source.UnitLimit)
			return code, out, err
		}
	case source.AlarmsGoogle:
		method, at, body, err := googleRequest(a, u, doc)
		if err != nil {
			return nil, err
		}
		if method == http.MethodPost {
			// A policy made again has a new name: refused when one of its
			// display name is there again.
			if why, err := t.googleAgain(ctx, a, doc); err != nil {
				return nil, err
			} else if why != "" {
				return nil, refusal{why}
			}
		}
		send = func(ctx context.Context) (int, []byte, error) {
			code, _, out, err := a.Call(ctx, method, at, body, nil, writes, source.UnitLimit)
			if err == nil && method == http.MethodPost && code == http.StatusOK {
				t.madeAs(ctx, p, a, doc, out)
			}
			return code, out, err
		}
	case source.AlarmsOCI:
		method, body, err := ociRequest(u, doc)
		if err != nil {
			return nil, err
		}
		h := http.Header{}
		if u.alarm.Etag != "" {
			h.Set("If-Match", u.alarm.Etag)
		}
		if method == http.MethodDelete {
			ok = []int{http.StatusNoContent, http.StatusOK}
		}
		send = func(ctx context.Context) (int, []byte, error) {
			code, _, out, err := a.Call(ctx, method, a.OCIURL(id), body, h, writes, source.UnitLimit)
			return code, out, err
		}
	default:
		return nil, refusal{fmt.Sprintf("alarms of kind %q", a.Kind)}
	}
	if !u.Gone && (a.Kind == source.AlarmsAWS || a.Kind == source.AlarmsGoogle) {
		again, err := a.Unit(ctx, id, a.Reads)
		if err != nil {
			return nil, err
		}
		if again.Version != u.alarm.Version || again.Hash != u.Hash {
			return nil, errMovedSince
		}
	}
	return func(ctx context.Context, _ ledger.Proposal) error {
		code, out, err := send(ctx)
		switch {
		case err != nil:
			return err
		case !slices.Contains(ok, code):
			return source.CloudFailed(code, out)
		}
		return nil
	}, nil
}

// alarmApply applies an approved proposal to a cloud's alarm, read and
// checked again once more when it was written to meanwhile.
func (s *Server) alarmApply(ctx context.Context, p ledger.Proposal, step ledger.Step) error {
	tg := alarmTarget{s}
	err := s.apiApply(ctx, tg, p, step)
	if errors.Is(err, errMovedSince) {
		if err = s.apiApply(ctx, tg, p, step); errors.Is(err, errMovedSince) {
			return refusal{"the alarm changed in the cloud a moment before the change was sent, twice: propose it again"}
		}
	}
	return err
}

// ---- CloudWatch ----

// cwAlarmOf is the alarm in a CloudWatch alarm's document, and whether it is
// a composite alarm.
func cwAlarmOf(m map[string]any) (map[string]any, bool) {
	for _, k := range []string{"MetricAlarms", "CompositeAlarms"} {
		if l := list(m[k]); len(l) == 1 {
			a, _ := l[0].(map[string]any)
			return a, k == "CompositeAlarms"
		}
	}
	return map[string]any{}, false
}

// cloudWatchKnown are the fields of a metric alarm, at any depth, in the
// cloudwatch model (2010-08-01) this Lapilli was built with: a test holds
// them to the model. An alarm with any other is written back without it,
// lost, so it isn't rewritten.
var cloudWatchKnown = setOf("AccountId", "ActionConfiguration", "ActionsEnabled", "AlarmActions", "AlarmArn",
	"AlarmConfigurationUpdatedTimestamp", "AlarmDescription", "AlarmName", "AlarmTagsPropagation", "ComparisonOperator",
	"DatapointsToAlarm", "Dimensions", "Enabled", "EvaluateLowSampleCountPercentile", "EvaluationCriteria", "EvaluationInterval",
	"EvaluationPeriods", "EvaluationState", "EvaluationWindow", "ExcludedTagKeys", "Expression", "ExtendedStatistic", "Id",
	"IncludedTagKeys", "InsufficientDataActions", "Label", "Metric", "MetricName", "MetricStat", "Metrics", "Name", "Namespace",
	"OKActions", "OnlyStartEvaluatingAfterWarmUpPeriodEnds", "PendingPeriod", "Period", "PromQLCriteria", "Query", "RecoveryPeriod",
	"ReturnData", "SlidingWindow", "Stat", "StateReason", "StateReasonData", "StateTransitionedTimestamp", "StateUpdatedTimestamp",
	"StateValue", "Statistic", "Threshold", "ThresholdMetricId", "Timezone", "TreatMissingData", "Unit", "Value", "WallClockWindow",
	"WarmUpConfiguration", "WarmUpPeriodDurationInMinutes")

// putMetricAlarmInput are the top-level fields PutMetricAlarm takes, in the
// model: of an alarm as read, those the answer has and the input doesn't
// (its ARN, its state) are left out.
var putMetricAlarmInput = setOf("ActionConfiguration", "ActionsEnabled", "AlarmActions", "AlarmDescription", "AlarmName",
	"ComparisonOperator", "DatapointsToAlarm", "Dimensions", "EvaluateLowSampleCountPercentile", "EvaluationCriteria",
	"EvaluationInterval", "EvaluationPeriods", "EvaluationWindow", "ExtendedStatistic", "InsufficientDataActions", "MetricName",
	"Metrics", "Namespace", "OKActions", "Period", "Statistic", "Tags", "Threshold", "ThresholdMetricId", "TreatMissingData",
	"Unit", "WarmUpConfiguration")

// cwRequest is the CloudWatch action that makes an alarm read as doc: a
// deletion, one alarm a request; its actions turned off or on alone; or
// PutMetricAlarm with the whole alarm, as read, with the change, since it
// overwrites every field, ActionsEnabled left out being true.
func cwRequest(name string, u apiUnit, doc []byte) (string, any, error) {
	names := map[string]any{"AlarmNames": []string{name}}
	if len(doc) == 0 {
		return "DeleteAlarms", names, nil
	}
	after, err := decodeTree(doc)
	if err != nil {
		return "", nil, refusal{"the alarm as it is to be isn't JSON"}
	}
	alarm, composite := cwAlarmOf(after)
	if !u.Gone {
		before, err := decodeTree(u.Raw)
		if err != nil {
			return "", nil, refusal{"the alarm as read isn't JSON"}
		}
		was, _ := cwAlarmOf(before)
		b, a := maps.Clone(was), maps.Clone(alarm)
		delete(b, "ActionsEnabled")
		delete(a, "ActionsEnabled")
		if reflect.DeepEqual(a, b) && was["ActionsEnabled"] != alarm["ActionsEnabled"] {
			if alarm["ActionsEnabled"] == false {
				return "DisableAlarmActions", names, nil
			}
			return "EnableAlarmActions", names, nil
		}
	}
	if composite {
		return "", nil, refusal{noPathComposite}
	}
	if f := unknownField(alarm, cloudWatchKnown); f != "" && !u.Gone {
		return "", nil, refusal{noPathUnknown + ": " + f}
	}
	in := map[string]any{}
	for k, v := range alarm {
		if putMetricAlarmInput[k] && (k != "Tags" || u.Gone) {
			in[k] = v
		}
	}
	if str(in["AlarmName"]) != name {
		return "", nil, refusal{"the alarm as it is to be is named apart from the one read"}
	}
	return "PutMetricAlarm", in, nil
}

// cwWatched reports whether a composite alarm watches an alarm, so deleting
// it would leave one watching nothing: DescribeAlarms with
// ParentsOfAlarmName, which takes no other parameter but MaxRecords and
// NextToken, a page at a time. An error is the cloud's, not an answer.
func cwWatched(ctx context.Context, a *source.Alarms, alarm map[string]any) (bool, error) {
	token := ""
	for pages := 0; pages < 100; pages++ {
		in := map[string]any{"ParentsOfAlarmName": str(alarm["AlarmName"])}
		if token != "" {
			in["NextToken"] = token
		}
		code, body, err := a.AWSCall(ctx, "DescribeAlarms", in, a.Reads, source.UnitLimit)
		if err != nil {
			return false, err
		}
		if code != http.StatusOK {
			return false, source.CloudFailed(code, body)
		}
		page, err := source.ReadAWSAlarms(body)
		if err != nil {
			return false, err
		}
		if len(page.CompositeAlarms) > 0 {
			return true, nil
		}
		if page.NextToken == "" || page.NextToken == token {
			return false, nil
		}
		token = page.NextToken
	}
	return false, errors.New("DescribeAlarms gave more than a hundred pages of an alarm's parents")
}

// ---- Azure ----

// activityLogKnown are the fields of an activity log alert, at any depth,
// in the REST specification (activityLogAlerts_API 2026-01-01): its
// description is written as a PUT of the whole resource, which would lose
// any other. Its webhook and action properties are maps of any keys, not
// read below.
var activityLogKnown = setOf("id", "name", "type", "location", "tags", "properties", "tenantScope", "scopes", "condition",
	"resourceGroup", // Lapilli's own, written beside the id as it reads one, never sent
	"actions", "enabled", "description", "allOf", "anyOf", "field", "equals", "containsAny", "actionGroups", "actionGroupId",
	"webhookProperties", "actionProperties")

// azureRequest is the request that makes an Azure alert rule read as doc:
// a DELETE; a PUT of the whole rule, made again, or for an activity log
// alert's description, whose PATCH takes enabled alone; otherwise a PATCH
// of the properties changed alone, a description taken out sent empty.
func azureRequest(u apiUnit, doc []byte) (string, []byte, error) {
	if len(doc) == 0 {
		return http.MethodDelete, nil, nil
	}
	after, err := decodeTree(doc)
	if err != nil {
		return "", nil, refusal{"the alert rule as it is to be isn't JSON"}
	}
	whole := func() (string, []byte, error) {
		m := maps.Clone(after)
		for _, k := range []string{"id", "name", "type", "systemData", "etag", "resourceGroup"} {
			delete(m, k)
		}
		b, err := json.Marshal(m)
		return http.MethodPut, b, err
	}
	if u.Gone {
		return whole()
	}
	before, err := decodeTree(u.Raw)
	if err != nil {
		return "", nil, refusal{"the alert rule as read isn't JSON"}
	}
	bp, _ := before["properties"].(map[string]any)
	ap, _ := after["properties"].(map[string]any)
	changed := map[string]any{}
	for k, v := range ap {
		if !reflect.DeepEqual(bp[k], v) {
			changed[k] = v
		}
	}
	for k := range bp {
		if _, ok := ap[k]; ok {
			continue
		}
		if k != "description" {
			return "", nil, refusal{"the change takes a property out of the alert rule, which Lapilli doesn't write"}
		}
		// A description added where there was none, undone: a PATCH
		// merges, so leaving it out would leave it.
		changed[k] = ""
	}
	switch {
	case len(changed) == 0:
		return "", nil, refusal{"the change writes nothing to the alert rule"}
	case strings.EqualFold(str(after["type"]), "Microsoft.Insights/activityLogAlerts") && (len(changed) > 1 || changed["enabled"] == nil):
		if f := unknownField(after, activityLogKnown); f != "" {
			return "", nil, refusal{noPathAzureFields + ": " + f}
		}
		return whole()
	}
	b, err := json.Marshal(map[string]any{"properties": changed})
	return http.MethodPatch, b, err
}

// ---- Google Cloud ----

// googleFields are the fields of a policy a change writes, named in
// updateMask.
var googleFields = []string{"documentation", "conditions", "enabled"}

// googleRequest is the request that makes a Google policy read as doc: a
// DELETE; a create of the policy, made again, with a new name and its
// conditions named anew; otherwise a patch of the fields changed, named in
// updateMask, whose empty mask would replace the whole policy and is never
// sent.
func googleRequest(a *source.Alarms, u apiUnit, doc []byte) (string, string, []byte, error) {
	base := strings.TrimSuffix(a.URL, "/")
	host, err := url.Parse(base)
	if err != nil {
		return "", "", nil, refusal{err.Error()}
	}
	at := host.Scheme + "://" + host.Host + "/v3/" + u.alarm.ID
	if len(doc) == 0 {
		return http.MethodDelete, at, nil, nil
	}
	after, err := decodeTree(doc)
	if err != nil {
		return "", "", nil, refusal{"the policy as it is to be isn't JSON"}
	}
	if u.Gone {
		m := maps.Clone(after)
		for _, k := range []string{"name", "creationRecord", "mutationRecord"} {
			delete(m, k)
		}
		for _, c := range list(m["conditions"]) {
			if cm, _ := c.(map[string]any); cm != nil {
				delete(cm, "name")
			}
		}
		b, err := json.Marshal(m)
		return http.MethodPost, base, b, err
	}
	before, err := decodeTree(u.Raw)
	if err != nil {
		return "", "", nil, refusal{"the policy as read isn't JSON"}
	}
	var mask []string
	body := map[string]any{}
	for _, f := range googleFields {
		if !reflect.DeepEqual(before[f], after[f]) {
			mask = append(mask, f)
			if v, ok := after[f]; ok {
				body[f] = v
			}
		}
	}
	for k := range after {
		if !slices.Contains(googleFields, k) && !reflect.DeepEqual(before[k], after[k]) {
			return "", "", nil, refusal{"the change writes a field of the policy Lapilli doesn't change: " + k}
		}
	}
	if len(mask) == 0 {
		return "", "", nil, refusal{"the change writes nothing to the policy"}
	}
	b, err := json.Marshal(body)
	return http.MethodPatch, at + "?" + url.Values{"updateMask": {strings.Join(mask, ",")}}.Encode(), b, err
}

// ---- OCI ----

// ociFields are the fields of an alarm a change writes, sent alone in
// UpdateAlarm.
var ociFields = []string{"body", "isEnabled"}

// ociRequest is UpdateAlarm with the fields changed alone, or DeleteAlarm,
// each with the etag the alarm was read with, so a write made over a
// change Lapilli didn't see is refused (412).
func ociRequest(u apiUnit, doc []byte) (string, []byte, error) {
	if len(doc) == 0 {
		return http.MethodDelete, nil, nil
	}
	if u.Gone {
		return "", nil, refusal{"an OCI alarm is never made again here"}
	}
	after, err1 := decodeTree(doc)
	before, err2 := decodeTree(u.Raw)
	if err1 != nil || err2 != nil {
		return "", nil, refusal{"the alarm isn't JSON"}
	}
	a, _ := list(after["data"])[0].(map[string]any)
	b, _ := list(before["data"])[0].(map[string]any)
	body := map[string]any{}
	for _, f := range ociFields {
		if !reflect.DeepEqual(a[f], b[f]) {
			body[f] = a[f]
		}
	}
	for k := range a {
		if !slices.Contains(ociFields, k) && !reflect.DeepEqual(a[k], b[k]) {
			return "", nil, refusal{"the change writes a field of the alarm Lapilli doesn't change: " + k}
		}
	}
	if len(body) == 0 {
		return "", nil, refusal{"the change writes nothing to the alarm"}
	}
	out, err := json.Marshal(body)
	return http.MethodPut, out, err
}

// ---- trees ----

// decodeTree reads a JSON document as a tree, its numbers as written.
func decodeTree(b []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("not a JSON object")
	}
	return m, nil
}

func list(v any) []any {
	l, _ := v.([]any)
	if len(l) == 0 {
		return []any{nil}[:0]
	}
	return l
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// unknownField is the first field, at any depth, a tree has that known
// hasn't, by its path; "" for none. A map of tags or properties, whose
// keys are anyone's, isn't read below.
func unknownField(m map[string]any, known map[string]bool) string {
	var walk func(v any, at string) string
	walk = func(v any, at string) string {
		switch x := v.(type) {
		case map[string]any:
			keys := slices.Collect(maps.Keys(x))
			sort.Strings(keys)
			for _, k := range keys {
				if !known[k] {
					return at + k
				}
				switch k {
				case "tags", "webhookProperties", "actionProperties":
					continue
				}
				if f := walk(x[k], at+k+"."); f != "" {
					return f
				}
			}
		case []any:
			for _, y := range x {
				if f := walk(y, at); f != "" {
					return f
				}
			}
		}
		return ""
	}
	return walk(m, "")
}
