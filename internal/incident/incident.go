// Package incident is what actually went wrong: incidents people wrote down,
// and windows where a signal of user impact says something did.
package incident

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
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
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// An empty file isn't taken for no incidents: a job that failed leaves
	// one behind, and in lapilli serve the file owns what it holds.
	if raw.Incidents.Kind == 0 {
		return nil, fmt.Errorf("%s: no incidents key: want incidents:, with a list under it, an empty one if there are none", path)
	}
	var doc struct{ Incidents []Incident }
	if err := raw.Incidents.Decode(&doc.Incidents); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
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
		seen[in.ID] = true
	}
	return doc.Incidents, nil
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
