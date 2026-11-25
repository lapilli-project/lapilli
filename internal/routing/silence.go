package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// A Silence is a silence Alertmanager holds: the alerts its matchers match
// are muted from Start until End. Only what muting takes is kept; not the
// comment, not who made it.
type Silence struct {
	ID         string
	Matchers   []Matcher
	Start, End time.Time
}

// What ReadSilences reads: a file of MaxSilenceBytes, MaxSilences silences
// of MaxSilenceMatchers matchers each, a matcher's name and value of
// maxSilenceText bytes at most, and regular expressions whose programs add
// up to MaxConfigRegexProgram instructions. A run is given MaxSilences at
// most (Alertmanager keeps expired ones for five days; 20,000 is a busy
// cluster's).
const (
	MaxSilenceBytes    = 64 << 20
	MaxSilences        = 20000
	MaxSilenceMatchers = 64
	maxSilenceText     = 4096
)

// ReadSilences reads silences as Alertmanager's API lists them (GET
// /api/v2/silences, or amtool silence query -o json): a JSON array of
// silences, those that have expired included. Only each silence's id,
// matchers (name, value, isRegex, and isEqual, true when it isn't said),
// startsAt and endsAt are read; any other field is passed over without being
// kept, and a silence that lacks one of those, or whose end is before its
// start, is an error, as is a matcher that isn't one.
func ReadSilences(b []byte) ([]Silence, error) {
	if len(b) > MaxSilenceBytes {
		return nil, fmt.Errorf("silences of more than %d MB", MaxSilenceBytes>>20)
	}
	var raw []struct {
		ID       *string `json:"id"`
		Matchers []struct {
			Name    *string `json:"name"`
			Value   *string `json:"value"`
			IsRegex *bool   `json:"isRegex"`
			IsEqual *bool   `json:"isEqual"`
		} `json:"matchers"`
		StartsAt *string `json:"startsAt"`
		EndsAt   *string `json:"endsAt"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("silences: not Alertmanager's JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("silences: more than one JSON document")
	}
	if len(raw) > MaxSilences {
		return nil, fmt.Errorf("silences: more than %d", MaxSilences)
	}
	out := make([]Silence, 0, len(raw))
	budget := NewRegexBudget()
	for i, r := range raw {
		if r.ID == nil || r.StartsAt == nil || r.EndsAt == nil || len(r.Matchers) == 0 {
			return nil, fmt.Errorf("silence %d: needs an id, matchers, startsAt and endsAt", i+1)
		}
		if len(*r.ID) > 128 || !utf8.ValidString(*r.ID) {
			return nil, fmt.Errorf("silence %d: an id of 128 bytes at most", i+1)
		}
		s := Silence{ID: *r.ID}
		var err error
		if s.Start, err = time.Parse(time.RFC3339Nano, *r.StartsAt); err != nil {
			return nil, fmt.Errorf("silence %d: startsAt: %w", i+1, err)
		}
		if s.End, err = time.Parse(time.RFC3339Nano, *r.EndsAt); err != nil {
			return nil, fmt.Errorf("silence %d: endsAt: %w", i+1, err)
		}
		if s.End.Before(s.Start) {
			return nil, fmt.Errorf("silence %d: ends before it starts", i+1)
		}
		if len(r.Matchers) > MaxSilenceMatchers {
			return nil, fmt.Errorf("silence %d: more than %d matchers", i+1, MaxSilenceMatchers)
		}
		for _, m := range r.Matchers {
			if m.Name == nil || m.Value == nil || m.IsRegex == nil {
				return nil, fmt.Errorf("silence %d: a matcher needs name, value and isRegex", i+1)
			}
			if len(*m.Name) > maxSilenceText || len(*m.Value) > maxSilenceText {
				return nil, fmt.Errorf("silence %d: a matcher of more than %d bytes", i+1, maxSilenceText)
			}
			equal := m.IsEqual == nil || *m.IsEqual
			op := OpEqual
			switch {
			case *m.IsRegex && equal:
				op = OpRegexp
			case *m.IsRegex:
				op = OpNotRegex
			case !equal:
				op = OpNotEqual
			}
			mm, err := NewMatcherBudget(budget, *m.Name, string(op), *m.Value)
			if err != nil {
				return nil, fmt.Errorf("silence %d: %w", i+1, err)
			}
			s.Matchers = append(s.Matchers, mm)
		}
		out = append(out, s)
	}
	return out, nil
}
