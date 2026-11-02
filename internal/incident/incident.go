// Package incident is what actually went wrong: incidents people wrote down,
// and windows where a signal of user impact says something did.
package incident

import (
	"fmt"
	"os"
	"sort"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/prom"
)

// Incident is one thing that went wrong.
type Incident struct {
	ID     string            `yaml:"id" json:"id"`
	Title  string            `yaml:"title" json:"title,omitempty"`
	Start  time.Time         `yaml:"start" json:"start"`
	End    time.Time         `yaml:"end" json:"end,omitzero"` // zero: not written down
	Labels map[string]string `yaml:"labels" json:"labels,omitempty"`
	// Candidate is a window found in the impact signal, not one a person
	// wrote down.
	Candidate bool `yaml:"-" json:"candidate"`
}

// Load reads a file of incidents:
//
//	incidents:
//	  - id: INC-12
//	    title: Checkout errors
//	    start: 2026-10-30T17:00:00+09:00
//	    end: 2026-10-30T17:40:00+09:00
//	    labels: {service: checkout}
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

// FromImpact finds the windows where an impact expression returned anything,
// in any series, for at least minLen: candidate incidents nobody wrote down.
// Gaps shorter than two steps don't split a window.
func FromImpact(series []prom.Series, step, minLen time.Duration) []Incident {
	var ts []time.Time
	for _, s := range series {
		for _, smp := range s.Samples {
			ts = append(ts, smp.T)
		}
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	var out []Incident
	add := func(start, last time.Time) {
		if last.Add(step).Sub(start) >= minLen {
			out = append(out, Incident{
				ID: fmt.Sprintf("impact-%d", len(out)+1), Start: start, End: last.Add(step),
				Title: "user impact (--impact)", Candidate: true,
			})
		}
	}
	for i := 0; i < len(ts); {
		start, last := ts[i], ts[i]
		for i++; i < len(ts) && ts[i].Sub(last) <= 2*step; i++ {
			last = ts[i]
		}
		add(start, last)
	}
	return out
}

// Merge adds candidates that don't overlap an incident already written down.
func Merge(written, candidates []Incident, unknownLen time.Duration) []Incident {
	out := append([]Incident(nil), written...)
	for _, c := range candidates {
		dup := false
		for _, w := range written {
			if !c.Start.After(w.EndOr(unknownLen)) && !c.End.Before(w.Start) {
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

// EndOr is the end, or the start plus d when nobody wrote the end down.
func (in Incident) EndOr(d time.Duration) time.Time {
	if in.End.IsZero() {
		return in.Start.Add(d)
	}
	return in.End
}
