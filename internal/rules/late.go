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

// Replaying is how rules are replayed, which isn't part of them: how late
// their metrics arrive (--late), how often the history was scraped
// (--scrape-interval), and the scrape interval each Grafana data source
// says it has, as read from Grafana.
type Replaying struct {
	Late   Lateness
	Scrape time.Duration
	// DataSources, when known, are what Grafana's $__rate_interval is put
	// in with; for a data source not among them, Scrape, or Grafana's 15s.
	DataSources DataSources
}

// Apply sets Late and Scrape on each of rs, and reads a Grafana rule's
// queries again with its data sources' scrape intervals.
func (p Replaying) Apply(rs []Rule) {
	for i := range rs {
		rs[i].Late, rs[i].Scrape = p.Late.Of(rs[i]), p.Scrape
		rs[i].Graph = rs[i].Graph.withScrapes(p.DataSources, p.Scrape)
	}
}
