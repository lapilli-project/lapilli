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
	// ScrapeOf, without Scrape, is how often the series an expression
	// reads were scraped, as Prometheus's configuration says; 0 when not
	// known. Nil when the configuration isn't known.
	ScrapeOf func(expr string) time.Duration
	// Kinds are the types Grafana says its data sources are, by uid: a
	// Grafana rule whose condition reads a Loki one is LogQL
	// (docs/design/loki.md).
	Kinds map[string]string
	// Places are where a Loki ruler runs each of its groups within its
	// interval, as its rules API says, by Place.
	Places map[Place]time.Duration
	// NoLoki says there is no Loki to ask a Loki ruler's rules of (no
	// --loki): they are listed, not replayed.
	NoLoki bool
	// NoGrafanaLoki says there is no way to a Grafana rule's Loki: neither
	// --grafana's data source proxy nor --loki.
	NoGrafanaLoki bool
}

// Place is a Loki ruler's group: its namespace and its name.
type Place struct{ Namespace, Group string }

// Apply sets Late and Scrape on each of rs, reads a Grafana rule's queries
// again with its data sources' scrape intervals, and, without Scrape, sets
// how often each of its reduces' history was scraped.
func (p Replaying) Apply(rs []Rule) {
	for i := range rs {
		rs[i].Late, rs[i].Scrape = p.Late.Of(rs[i]), p.Scrape
		if rs[i].unread != nil && len(p.Kinds) > 0 {
			// Read again, with what Grafana says its data sources are.
			if g, err := buildGraph(rs[i].unread, p.DataSources, p.Kinds); err != nil {
				rs[i].Unsupported = err.Error()
			} else {
				rs[i].Graph, rs[i].Unsupported, rs[i].unread = g, "", nil
			}
		}
		if rs[i].Graph != nil && rs[i].Unsupported == "" {
			g, err := rs[i].Graph.withSources(p.DataSources, p.Scrape, p.Kinds)
			if err != nil {
				rs[i].Unsupported = err.Error()
			} else {
				rs[i].Graph, rs[i].Logs = g, g.Logs
			}
		}
		switch r := &rs[i]; {
		case r.Unsupported != "" || r.Logs == "":
		case r.Graph == nil && p.NoLoki:
			r.Unsupported = "it is LogQL, from a Loki ruler: give --loki URL to replay it"
		case r.Graph != nil && p.NoGrafanaLoki:
			r.Unsupported = "its condition reads Loki: read the rules with --grafana, or give --loki URL, to replay it"
		case r.Graph == nil:
			if d, ok := p.Places[Place{r.Namespace, r.Group}]; ok {
				r.Phase = d
			}
		}
		rs[i].Scrapes = nil
		if p.Scrape > 0 || p.ScrapeOf == nil || rs[i].Graph == nil {
			continue
		}
		for ref, a := range rs[i].Graph.Asks {
			if d := p.ScrapeOf(a.Expr); d > 0 {
				if rs[i].Scrapes == nil {
					rs[i].Scrapes = map[string]time.Duration{}
				}
				rs[i].Scrapes[ref] = d
			}
		}
	}
}
