// Package incident is what actually went wrong: incidents people wrote down,
// and windows where a signal of user impact says something did.
package incident

import (
	"fmt"
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

// Load reads a file of incidents:
//
//	incidents:
//	  - id: INC-12
//	    title: Checkout errors
//	    start: 2026-10-30T17:00:00+09:00
//	    end: 2026-10-30T17:40:00+09:00
//	    labels: {service: [checkout, payments]}
func Load(path string) ([]Incident, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Incidents []Incident `yaml:"incidents"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i, in := range doc.Incidents {
		switch {
		case in.ID == "":
			return nil, fmt.Errorf("%s: incident %d has no id", path, i+1)
		case in.Start.IsZero():
			return nil, fmt.Errorf("%s: incident %s has no start", path, in.ID)
		case !in.End.IsZero() && in.End.Before(in.Start):
			return nil, fmt.Errorf("%s: incident %s ends before it starts", path, in.ID)
		}
	}
	return doc.Incidents, nil
}

// FromImpact finds the windows where an impact expression returned
// something for at least minLen: candidate incidents nobody wrote down. Each
// series is looked at on its own, and its windows carry its labels, so an
// expression by service finds which services were hit. Windows that overlap
// are one incident, with every value of the labels they all have. Gaps
// shorter than two steps don't split a window.
func FromImpact(series []prom.Series, step, minLen time.Duration) []Incident {
	type window struct {
		start, end time.Time
		labels     map[string]string
	}
	var ws []window
	for _, s := range series {
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
		for i++; i < len(ws) && !ws[i].start.After(in.End); i++ {
			if ws[i].end.After(in.End) {
				in.End = ws[i].end
			}
			for k, vs := range in.Labels {
				v, ok := ws[i].labels[k]
				switch {
				case !ok:
					delete(in.Labels, k) // not all have it: no constraint
				case !slices.Contains(vs, v):
					in.Labels[k] = append(vs, v)
				}
			}
		}
		for _, vs := range in.Labels {
			slices.Sort(vs)
		}
		if len(in.Labels) == 0 {
			in.Labels = nil
		}
		out = append(out, in)
	}
	return out
}

// Merge adds candidates that don't overlap an incident already written
// down. With labels to match on, a candidate is the same incident only if
// their values for those labels have one in common.
func Merge(written, candidates []Incident, unknownLen time.Duration, match []string) []Incident {
	out := append([]Incident(nil), written...)
	for _, c := range candidates {
		dup := false
		for _, w := range written {
			if !c.Start.After(w.EndOr(unknownLen)) && !c.End.Before(w.Start) && overlap(c.Labels, w.Labels, match) {
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
