// Package incident is what actually went wrong: incidents people wrote down,
// and windows where a signal of user impact says something did.
package incident

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/prom"
)

// Incident is one thing that went wrong.
type Incident struct {
	ID    string    `yaml:"id" json:"id"`
	Title string    `yaml:"title" json:"title,omitempty"`
	Start time.Time `yaml:"start" json:"start"`
	End   time.Time `yaml:"end" json:"end,omitzero"` // zero: not written down
	// Labels say where it went wrong, such as the services it hit.
	Labels Labels `yaml:"labels" json:"labels,omitempty"`
	// Alerts are those the incident tool linked to it, when it says
	// (docs/design/links.md): a firing of one catches it whatever its
	// labels say on the labels matched on. Sorted, each once (SortAlerts).
	Alerts []Alert `yaml:"alerts" json:"alerts,omitempty"`
	// Candidate is a window found in the impact signal, not one a person
	// wrote down.
	Candidate bool `yaml:"-" json:"candidate"`
	// Spans are, of a candidate, each series' own window: an incident of
	// several series lasts from the first one's start to the last one's
	// end, which none of them may have lasted.
	Spans []Span `yaml:"-" json:"spans,omitempty"`
	// Signal is, of a window a named impact signal found, the signal's name
	// (docs/design/signals.md): "" for any other incident, the windows of
	// an impact signal with no name among them.
	Signal string `yaml:"-" json:"signal,omitempty"`
	// Match are the labels a firing must agree with it on, of a named
	// signal's window: its signal's. Nil for the match in effect
	// (verdict.Options.Match), as for every other incident; empty for none.
	Match []string `yaml:"-" json:"-"`
	// Real says, in lapilli serve, that a person's "This was real" wrote it
	// from a firing (docs/design/judging.md, item 3).
	Real bool `yaml:"-" json:"-"`
}

// Alert is one of the alerts an incident tool linked to an incident: the
// rule it is of, by the name its alerts carry (alertname), and the labels
// it had, all or some, which hold a firing to them where it has them too
// (In).
type Alert struct {
	Rule   string            `yaml:"rule" json:"rule"`
	Labels map[string]string `yaml:"labels" json:"labels,omitempty"`
}

// The bounds of an incident's alerts, as an incident's labels have theirs:
// what a tool or a token can make every firing be held to.
const (
	MaxAlerts      = 100      // alerts an incident lists
	MaxAlertRule   = 512      // bytes of a rule's name
	MaxAlertLabels = 32       // labels an alert has
	MaxAlertBytes  = 16 << 10 // the alerts in all, as JSON
)

// UnmarshalYAML takes an alert as {rule: NAME, labels: {k: v}}, refusing
// a key it doesn't know (a misspelt labels would link every firing of the
// rule) and a label whose value isn't a value.
func (a *Alert) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("an alert is {rule: NAME, labels: {LABEL: VALUE}}")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch k := n.Content[i].Value; k {
		case "rule", "labels":
		default:
			return fmt.Errorf("an alert has a rule and labels, not %q", k)
		}
	}
	var raw struct {
		Rule   string               `yaml:"rule"`
		Labels map[string]yaml.Node `yaml:"labels"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	a.Rule, a.Labels = raw.Rule, nil
	for k, v := range raw.Labels {
		if v.Kind != yaml.ScalarNode || v.Tag == "!!null" {
			return fmt.Errorf("alert %s: label %s: want a value", raw.Rule, k)
		}
		if a.Labels == nil {
			a.Labels = map[string]string{}
		}
		a.Labels[k] = v.Value
	}
	return nil
}

// UnmarshalJSON takes an alert as {"rule": NAME, "labels": {"k": "v"}},
// refusing, as UnmarshalYAML does, a key it doesn't know (a misspelt
// "label" would link every firing of the rule) and a label whose value
// isn't text.
func (a *Alert) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return errors.New(`an alert is {"rule": NAME, "labels": {LABEL: VALUE}}`)
	}
	for k := range raw {
		if k != "rule" && k != "labels" {
			return fmt.Errorf("an alert has a rule and labels, not %q", k)
		}
	}
	var x struct {
		Rule   string            `json:"rule"`
		Labels map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(b, &x); err != nil {
		return fmt.Errorf("an alert's rule is text, and its labels text: %w", err)
	}
	a.Rule, a.Labels = x.Rule, x.Labels
	return nil
}

// Names reports whether the alert is of rule: rule is its name, or its
// name told apart from others of that name (HighErrorRate
// [severity=critical]), as the tool knows only the name its alerts carry.
func (a Alert) Names(rule string) bool {
	return rule == a.Rule || strings.HasPrefix(rule, a.Rule+" [") && strings.HasSuffix(rule, "]")
}

// In reports whether labels agree with the alert's, as --match has a label
// one side lacks: each the firing carries has the alert's value; one it
// doesn't carry (an external label Prometheus added as it sent the alert,
// one the receiver added) is passed over.
func (a Alert) In(labels map[string]string) bool {
	for k, v := range a.Labels {
		if w, ok := labels[k]; ok && w != v {
			return false
		}
	}
	return true
}

// Links reports whether a firing of rule with labels is one of the alerts
// the incident lists (docs/design/links.md).
func (in Incident) Links(rule string, labels map[string]string) bool {
	for _, a := range in.Alerts {
		if a.Names(rule) && a.In(labels) {
			return true
		}
	}
	return false
}

// CheckAlerts says what is wrong with an incident's alerts, nil for
// nothing: a rule's name and each label's are there, and the bounds kept.
func CheckAlerts(as []Alert) error {
	if len(as) > MaxAlerts {
		return fmt.Errorf("an incident lists %d alerts at most", MaxAlerts)
	}
	for _, a := range as {
		switch {
		case a.Rule == "":
			return errors.New("an alert needs a rule: the name its alerts carry")
		case len(a.Rule) > MaxAlertRule:
			return fmt.Errorf("an alert's rule is %d bytes at most", MaxAlertRule)
		case len(a.Labels) > MaxAlertLabels:
			return fmt.Errorf("an alert has %d labels at most", MaxAlertLabels)
		}
		for k, v := range a.Labels {
			if k == "" || v == "" || len(k) > 128 || len(v) > 256 {
				return errors.New("an alert's label is a name of up to 128 bytes and a value of up to 256, neither empty")
			}
		}
	}
	// As the ledger writes them, < > & as they are.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(as); b.Len()-1 > MaxAlertBytes {
		return fmt.Errorf("an incident's alerts are %d KB at most in all, as JSON", MaxAlertBytes>>10)
	}
	return nil
}

// SortAlerts is as, sorted by rule and labels, each once, nil for none: an
// incident copied again from its tool, its alerts in another order, is the
// same incident.
func SortAlerts(as []Alert) []Alert {
	if len(as) == 0 {
		return nil
	}
	type keyed struct {
		k string
		a Alert
	}
	ks := make([]keyed, 0, len(as))
	for _, a := range as {
		if len(a.Labels) == 0 {
			a.Labels = nil
		}
		b, _ := json.Marshal(a.Labels) // keys sorted
		ks = append(ks, keyed{a.Rule + "\x00" + string(b), a})
	}
	slices.SortFunc(ks, func(x, y keyed) int { return strings.Compare(x.k, y.k) })
	ks = slices.CompactFunc(ks, func(x, y keyed) bool { return x.k == y.k })
	out := make([]Alert, len(ks))
	for i, k := range ks {
		out[i] = k.a
	}
	return out
}

// Span is one series' window of impact, by its labels.
type Span struct {
	Labels map[string]string `json:"labels,omitempty"`
	Start  time.Time         `json:"start"`
	End    time.Time         `json:"end"`
}

// Labels are an incident's labels. A label can have several values: an
// incident that hit two services has both.
type Labels map[string][]string

// UnmarshalYAML takes a value, or a list of values, for each label.
func (l *Labels) UnmarshalYAML(n *yaml.Node) error {
	var raw map[string]yaml.Node
	if err := n.Decode(&raw); err != nil {
		return err
	}
	*l = Labels{}
	for k, v := range raw {
		var vs []string
		if v.Kind == yaml.ScalarNode {
			vs = []string{v.Value}
		} else if err := v.Decode(&vs); err != nil {
			return fmt.Errorf("label %s: want a value or a list of values", k)
		}
		(*l)[k] = vs
	}
	return nil
}

// Agree reports whether the labels don't contradict a label set on the
// labels named: where both have one, the value is among the incident's.
func (l Labels) Agree(other map[string]string, names []string) bool {
	for _, name := range names {
		v, ok := other[name]
		vs, mine := l[name]
		if ok && mine && !slices.Contains(vs, v) {
			return false
		}
	}
	return true
}

// Lacks reports whether the other labels lack one of the names that these
// have: what Agree lets through, that has nothing to disagree with.
func (l Labels) Lacks(other map[string]string, names []string) bool {
	return len(l.Lacked(other, names)) > 0
}

// Lacked is the names of those that the other labels lack and these have,
// in the order of names.
func (l Labels) Lacked(other map[string]string, names []string) []string {
	var lacked []string
	for _, name := range names {
		if _, ok := other[name]; !ok {
			if _, mine := l[name]; mine && !slices.Contains(lacked, name) {
				lacked = append(lacked, name)
			}
		}
	}
	return lacked
}

// MaxFile is the most of a file of incidents Load reads: more is refused.
// Years of incidents are a few megabytes.
const MaxFile = 64 << 20

// Load reads a file of incidents:
//
//	incidents:
//	  - id: INC-12
//	    title: Checkout errors
//	    start: 2026-10-30T17:00:00+09:00
//	    end: 2026-10-30T17:40:00+09:00
//	    labels: {service: [checkout, payments]}
//	    alerts: [{rule: CheckoutErrors, labels: {service: checkout}}]
func Load(path string) ([]Incident, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	f.Close()
	switch {
	case err != nil:
		return nil, err
	case len(b) > MaxFile:
		// /dev/zero, or a file made to take a reader's memory.
		return nil, fmt.Errorf("%s: more than %d MiB: a file of incidents is less", path, MaxFile>>20)
	}
	var raw struct {
		Incidents yaml.Node `yaml:"incidents"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		var top yaml.Node
		if yaml.Unmarshal(b, &top) == nil && top.Kind == yaml.DocumentNode && len(top.Content) == 1 && top.Content[0].Kind == yaml.SequenceNode {
			// A bare list is not taken: the file says what it is.
			return nil, fmt.Errorf("%s: want \"incidents:\" at the top, the list under it (lapilli.dev/docs/how-it-judges)", path)
		}
		return nil, fmt.Errorf("%s: %w", path, oneLine(err))
	}
	// An empty file isn't taken for no incidents: a job that failed leaves
	// one behind, and in lapilli serve the file owns what it holds.
	if raw.Incidents.Kind == 0 {
		return nil, fmt.Errorf("%s: no incidents key: want incidents:, with a list under it, an empty one if there are none", path)
	}
	var doc struct{ Incidents []Incident }
	if err := raw.Incidents.Decode(&doc.Incidents); err != nil {
		if bad := badTime(path, raw.Incidents); bad != nil {
			return nil, bad
		}
		return nil, fmt.Errorf("%s: %w", path, oneLine(err))
	}
	seen := map[string]bool{}
	for i, in := range doc.Incidents {
		switch {
		case in.ID == "":
			return nil, fmt.Errorf("%s: incident %d has no id", path, i+1)
		case seen[in.ID]:
			return nil, fmt.Errorf("%s: incident %s is there twice; give each its own id", path, in.ID)
		case in.Start.IsZero():
			return nil, fmt.Errorf("%s: incident %s has no start", path, in.ID)
		case !in.End.IsZero() && in.End.Before(in.Start):
			return nil, fmt.Errorf("%s: incident %s ends before it starts", path, in.ID)
		}
		// Each once before the bounds: the same alert twice is one.
		doc.Incidents[i].Alerts = SortAlerts(in.Alerts)
		if err := CheckAlerts(doc.Incidents[i].Alerts); err != nil {
			return nil, fmt.Errorf("%s: incident %s: %w", path, in.ID, err)
		}
		seen[in.ID] = true
	}
	return doc.Incidents, nil
}

// maxTypeErrors is how many of a YAML type error's errors oneLine keeps.
const maxTypeErrors = 5

// oneLine is a YAML type error on one line, not the parser's own list
// of lines under a heading: the first maxTypeErrors of them, then how many
// more there are.
func oneLine(err error) error {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		list := te.Errors
		more := ""
		if len(list) > maxTypeErrors {
			more = fmt.Sprintf("; and %d more", len(list)-maxTypeErrors)
			list = list[:maxTypeErrors]
		}
		return errors.New(strings.Join(list, "; ") + more)
	}
	return err
}

// badTime finds, in the list of incidents, a start or end that is not a
// time, and says which, as a person would write one: not as the Go layout
// the parser quotes. It is nil when there is none.
func badTime(path string, list yaml.Node) error {
	for i, item := range list.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		id := fmt.Sprintf("%d", i+1)
		for j := 0; j+1 < len(item.Content); j += 2 {
			if item.Content[j].Value == "id" {
				id = item.Content[j+1].Value
			}
		}
		for j := 0; j+1 < len(item.Content); j += 2 {
			field := item.Content[j].Value
			if field != "start" && field != "end" {
				continue
			}
			var t time.Time
			if err := item.Content[j+1].Decode(&t); err != nil {
				return fmt.Errorf("%s: incident %s: %s: want a time such as 2026-11-24T09:00:00Z (RFC 3339)", path, id, field)
			}
		}
	}
	return nil
}

// MaxWindowValues is how many values of a label a window found in an
// impact signal keeps, as the ledger keeps a named signal's (64 a label).
// A window that merged more series than that, each with a value of its
// own, keeps none of that label: as a label not all its series have, it
// constrains nothing, so the window is caught by more firings, never by
// fewer; one kept with some of its values only would be agreed with by
// those alone.
const MaxWindowValues = 64

// FromImpact finds the windows where an impact expression returned
// something for at least minLen: candidate incidents nobody wrote down. Each
// series is looked at on its own, and its windows carry its labels, so an
// expression by service finds which services were hit. Windows that overlap
// are one incident, with every value of the labels they all have, a label
// of more than MaxWindowValues values left out. Gaps shorter than two steps don't split a
// window. Each keeps its series' own windows, as Spans. It stops when ctx
// is done: an expression that isn't summed up can return 100,000 series.
func FromImpact(ctx context.Context, series []prom.Series, step, minLen time.Duration) ([]Incident, error) {
	type window struct {
		start, end time.Time
		labels     map[string]string
	}
	var ws []window
	for n, s := range series {
		if n%1024 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		ts := make([]time.Time, 0, len(s.Samples))
		for _, smp := range s.Samples {
			ts = append(ts, smp.T)
		}
		slices.SortFunc(ts, time.Time.Compare)
		l := maps.Clone(s.Labels)
		delete(l, "__name__")
		for i := 0; i < len(ts); {
			start, last := ts[i], ts[i]
			for i++; i < len(ts) && ts[i].Sub(last) <= 2*step; i++ {
				last = ts[i]
			}
			if end := last.Add(step); end.Sub(start) >= minLen {
				ws = append(ws, window{start, end, l})
			}
		}
	}
	slices.SortStableFunc(ws, func(a, b window) int { return a.start.Compare(b.start) })
	var out []Incident
	for i := 0; i < len(ws); {
		in := Incident{
			ID: fmt.Sprintf("impact-%d", len(out)+1), Start: ws[i].start, End: ws[i].end,
			Title: "user impact (--impact)", Candidate: true, Labels: Labels{},
		}
		for k, v := range ws[i].labels {
			in.Labels[k] = []string{v}
		}
		in.Spans = []Span{{Labels: ws[i].labels, Start: ws[i].start, End: ws[i].end}}
		for i++; i < len(ws) && !ws[i].start.After(in.End); i++ {
			if i%1024 == 0 && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if ws[i].end.After(in.End) {
				in.End = ws[i].end
			}
			in.Spans = append(in.Spans, Span{Labels: ws[i].labels, Start: ws[i].start, End: ws[i].end})
			for k, vs := range in.Labels {
				v, ok := ws[i].labels[k]
				if !ok {
					delete(in.Labels, k) // not all have it: no constraint
					continue
				}
				if vs, ok := KeepValue(vs, v); ok {
					in.Labels[k] = vs
				} else {
					delete(in.Labels, k) // too many to keep: no constraint
				}
			}
		}
		if len(in.Labels) == 0 {
			in.Labels = nil
		}
		out = append(out, in)
	}
	return out, nil
}

// KeepValue is vs, a label's values as a window keeps them, with v: each
// once, in order; false when v would be one more than MaxWindowValues,
// and the label is to be left out (MaxWindowValues). v is looked for among
// those kept, never among every value before it, so that a window of
// 100,000 series, each with a value of its own, is merged in time linear
// in them; the ledger keeps a window's values so too.
func KeepValue(vs []string, v string) ([]string, bool) {
	i, found := slices.BinarySearch(vs, v)
	switch {
	case found:
		return vs, true
	case len(vs) >= MaxWindowValues:
		return nil, false
	}
	return slices.Insert(vs, i, v), true
}

// FromSignal finds the windows of a named impact signal, as FromImpact
// finds them: of its series alone, never merged with another signal's,
// each carrying the signal's name and its match (nil for the match in
// effect), its id impact-NAME-1, impact-NAME-2 and so on.
func FromSignal(ctx context.Context, series []prom.Series, step, minLen time.Duration, name string, match []string) ([]Incident, error) {
	out, err := FromImpact(ctx, series, step, minLen)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].ID = fmt.Sprintf("impact-%s-%d", name, i+1)
		out[i].Title = "user impact (" + name + ")"
		out[i].Signal, out[i].Match = name, match
	}
	return out, nil
}

// Merge adds candidates that don't overlap an incident already written
// down. With labels to match on, a candidate is the same incident only if
// their values for those labels have one in common: a candidate's own
// Match, when it has one, else match.
func Merge(written, candidates []Incident, unknownLen time.Duration, match []string) []Incident {
	out := append([]Incident(nil), written...)
	for _, c := range candidates {
		dup := false
		on := match
		if c.Match != nil {
			on = c.Match
		}
		for _, w := range written {
			if !c.Start.After(w.EndOr(unknownLen)) && !c.End.Before(w.Start) && overlap(c.Labels, w.Labels, on) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

func overlap(a, b Labels, names []string) bool {
	for _, n := range names {
		va, okA := a[n]
		vb, okB := b[n]
		if okA && okB && !slices.ContainsFunc(va, func(v string) bool { return slices.Contains(vb, v) }) {
			return false
		}
	}
	return true
}

// EndOr is the end, or the start plus d when nobody wrote the end down.
func (in Incident) EndOr(d time.Duration) time.Time {
	if in.End.IsZero() {
		return in.Start.Add(d)
	}
	return in.End
}
