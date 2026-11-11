package rules

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Lateness is how late rules' metrics are taken to arrive, as --late gives
// it: for the rules a pattern matches, or for every rule.
type Lateness []Late

// Late is one --late: rules whose name, or the file or cloud they were
// read from, matches Match (every rule when nil) are replayed as if their
// metrics arrived D after their time.
type Late struct {
	Match *regexp.Regexp
	D     time.Duration
}

// maxLate is the longest a metric is taken to be late: a day, past which a
// replay would read a day that isn't the one it judges.
const maxLate = 24 * time.Hour

// ParseLate reads one --late: DUR, or PATTERN=DUR.
func ParseLate(v string) (Late, error) {
	pat, d := "", v
	if i := strings.LastIndexByte(v, '='); i >= 0 {
		pat, d = v[:i], v[i+1:]
	}
	dur, err := duration(d)
	if err != nil || dur <= 0 || dur > maxLate {
		return Late{}, fmt.Errorf("--late %s: want a duration up to 24h, or PATTERN=DURATION", v)
	}
	l := Late{D: dur}
	if pat != "" {
		if l.Match, err = regexp.Compile(pat); err != nil {
			return Late{}, fmt.Errorf("--late %s: %v", v, err)
		}
	}
	return l, nil
}

// Of is how late r's metrics are taken to arrive: the first --late whose
// pattern matches it, or else one with no pattern; 0 when none does.
func (ls Lateness) Of(r Rule) time.Duration {
	var all time.Duration
	for _, l := range ls {
		switch {
		case l.Match == nil:
			if all == 0 {
				all = l.D
			}
		case l.Match.MatchString(r.AlertName()) || l.Match.MatchString(r.Name) || r.Source != "" && l.Match.MatchString(r.Source) ||
			r.Cloud != "" && l.Match.MatchString(r.Cloud):
			return l.D
		}
	}
	return all
}

// Apply sets Late on each of rs, as Of says.
func (ls Lateness) Apply(rs []Rule) {
	if len(ls) == 0 {
		return
	}
	for i := range rs {
		rs[i].Late = ls.Of(rs[i])
	}
}
