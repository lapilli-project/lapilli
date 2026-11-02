// Package firing is when alerts fired: recorded, or worked out by replaying
// a rule against history.
package firing

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"sort"
	"time"

	"lapilli.dev/internal/prom"
)

// Firing is one alert, one label set, from when it fired until it resolved.
type Firing struct {
	Rule   string            `json:"rule"`
	Labels map[string]string `json:"labels,omitempty"`
	Start  time.Time         `json:"start"`
	// End is when it resolved; zero when it was still firing at the end of
	// the period.
	End time.Time `json:"end,omitzero"`
}

// Active reports whether the firing was on at any time in [from, to].
func (f Firing) Active(from, to time.Time) bool {
	return !f.Start.After(to) && (f.End.IsZero() || !f.End.Before(from))
}

// ReadJSONL reads one firing per line, as written by other tools or by
// "lapilli replay --json".
func ReadJSONL(r io.Reader) ([]Firing, error) {
	var out []Firing
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f Firing
		if err := json.Unmarshal(line, &f); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if f.Rule == "" || f.Start.IsZero() {
			return nil, fmt.Errorf("line %d: a firing needs rule and start", n)
		}
		out = append(out, f)
	}
	return out, sc.Err()
}

// FromSeries works out when a rule fired from the history of its
// expression, sampled every step: a label set is pending from its first
// sample, fires once it has been there for "for", and resolves at the first
// step it is missing. Firings still on at end have no End.
func FromSeries(rule string, ruleLabels map[string]string, series []prom.Series, step, forDur time.Duration, end time.Time) []Firing {
	var out []Firing
	gap := step + step/2 // allow for evaluation jitter
	for _, s := range series {
		l := maps.Clone(s.Labels)
		delete(l, "__name__")
		maps.Copy(l, ruleLabels)
		if len(s.Samples) == 0 {
			continue
		}
		runStart, last := s.Samples[0].T, s.Samples[0].T
		flush := func(resolved time.Time) {
			fire := runStart.Add(forDur)
			if last.Before(fire) {
				return // pending, never fired
			}
			f := Firing{Rule: rule, Labels: l, Start: fire}
			if !resolved.IsZero() {
				f.End = resolved
			}
			out = append(out, f)
		}
		for _, smp := range s.Samples[1:] {
			if smp.T.Sub(last) > gap {
				flush(last.Add(step))
				runStart = smp.T
			}
			last = smp.T
		}
		if end.Sub(last) > gap {
			flush(last.Add(step))
		} else {
			flush(time.Time{})
		}
	}
	Sort(out)
	return out
}

// FromAlerts reads the ALERTS series Prometheus keeps for its own rules.
// Those samples only exist while an alert fires, so "for" has already been
// applied.
func FromAlerts(series []prom.Series, step time.Duration, end time.Time) []Firing {
	byRule := map[string][]prom.Series{}
	for _, s := range series {
		l := maps.Clone(s.Labels)
		name := l["alertname"]
		delete(l, "alertname")
		delete(l, "alertstate")
		byRule[name] = append(byRule[name], prom.Series{Labels: l, Samples: s.Samples})
	}
	var out []Firing
	for name, ss := range byRule {
		out = append(out, FromSeries(name, nil, ss, step, 0, end)...)
	}
	Sort(out)
	return out
}

// Sort orders firings by start, then rule.
func Sort(fs []Firing) {
	sort.SliceStable(fs, func(i, j int) bool {
		if !fs[i].Start.Equal(fs[j].Start) {
			return fs[i].Start.Before(fs[j].Start)
		}
		return fs[i].Rule < fs[j].Rule
	})
}
