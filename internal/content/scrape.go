package content

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// defaultScrape is the scrape interval Prometheus uses when its
// configuration gives none.
const defaultScrape = time.Minute

// ScrapeInterval is the global scrape interval in a Prometheus
// configuration, as /api/v1/status/config gives it; ok is false when the
// configuration can't be read.
func ScrapeInterval(config string) (d time.Duration, ok bool) {
	s, ok := ScrapeConfig(config)
	return s.Global, ok
}

// Scrapes are the scrape intervals the range check counts with: the
// global one, and each job's own (docs/design/serveops.md).
type Scrapes struct {
	Global time.Duration
	// Jobs are each job's interval, by the value its targets' job label
	// has: its name, as Prometheus sets it, or what relabelling made it,
	// as its targets say. A label several jobs share has the longest.
	Jobs map[string]time.Duration
	// names are Jobs' names, sorted, once for every range checked.
	names []string
}

// sorted is the jobs' names, sorted.
func (s Scrapes) sorted() []string {
	if s.names != nil || len(s.Jobs) == 0 {
		return s.names
	}
	return slices.Sorted(maps.Keys(s.Jobs))
}

// String is the intervals in one line, the same for the same.
func (s Scrapes) String() string {
	var b strings.Builder
	b.WriteString(promDuration(s.Global))
	for _, j := range slices.Sorted(maps.Keys(s.Jobs)) {
		fmt.Fprintf(&b, " %q=%s", j, promDuration(s.Jobs[j]))
	}
	return b.String()
}

// maxJobs bounds how many jobs are read.
const maxJobs = 10000

// ScrapeConfig is the global scrape interval and each job's in a Prometheus
// configuration, as /api/v1/status/config gives it; ok is false when the
// configuration can't be read. A job is known by its name, the job label
// Prometheus gives its targets unless relabelling changes it.
func ScrapeConfig(config string) (s Scrapes, ok bool) {
	var c struct {
		Global struct {
			ScrapeInterval string `yaml:"scrape_interval"`
		} `yaml:"global"`
		ScrapeConfigs []struct {
			JobName        string `yaml:"job_name"`
			ScrapeInterval string `yaml:"scrape_interval"`
		} `yaml:"scrape_configs"`
	}
	if err := yaml.Unmarshal([]byte(config), &c); err != nil {
		return s, false
	}
	s.Global = defaultScrape
	if c.Global.ScrapeInterval != "" {
		d, ok := ParseDuration(c.Global.ScrapeInterval)
		if !ok || d <= 0 {
			return Scrapes{}, false
		}
		s.Global = d
	}
	for _, j := range c.ScrapeConfigs[:min(len(c.ScrapeConfigs), maxJobs)] {
		if j.JobName == "" {
			continue
		}
		d := s.Global
		if j.ScrapeInterval != "" {
			var ok bool
			if d, ok = ParseDuration(j.ScrapeInterval); !ok || d <= 0 {
				continue
			}
		}
		s.add(j.JobName, d)
	}
	return s, true
}

func (s *Scrapes) add(job string, d time.Duration) {
	if s.Jobs == nil {
		s.Jobs = map[string]time.Duration{}
	}
	if d > s.Jobs[job] {
		s.Jobs[job] = d
	}
}

// Targets adds what Prometheus's active targets say, as
// /api/v1/targets?state=active gives them: the job label each ends up with,
// and its interval (or its job's, by its scrape pool, when it doesn't say).
// Under the Prometheus Operator, job serviceMonitor/shop/checkout/0 is
// job="checkout". ok is false when the answer can't be read.
func (s *Scrapes) Targets(body []byte) (ok bool) {
	var a struct {
		Status string `json:"status"`
		Data   struct {
			ActiveTargets []struct {
				Labels         map[string]string `json:"labels"`
				ScrapePool     string            `json:"scrapePool"`
				ScrapeInterval string            `json:"scrapeInterval"`
			} `json:"activeTargets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &a); err != nil || a.Status != "success" {
		return false
	}
	pools := maps.Clone(s.Jobs)
	for _, t := range a.Data.ActiveTargets {
		job := t.Labels["job"]
		if job == "" || len(s.Jobs) >= maxJobs && s.Jobs[job] == 0 {
			continue
		}
		d, ok := ParseDuration(t.ScrapeInterval)
		if !ok || d <= 0 {
			if d, ok = pools[t.ScrapePool]; !ok {
				continue
			}
		}
		s.add(job, d)
	}
	return true
}

// uniform is the one interval every job has, when they all have one.
func (s Scrapes) uniform() (time.Duration, bool) {
	var d time.Duration
	for _, x := range s.Jobs {
		if d != 0 && x != d {
			return 0, false
		}
		d = x
	}
	return d, d != 0
}

// For is the interval a range over a selector with these job matchers is
// counted with, and the job it is that job's, "" for the global one: the
// longest of the jobs the matchers can be, since the range must hold two
// points of each; otherwise the one every job has, or the global.
func (s Scrapes) For(jobs []Matcher) (time.Duration, string) {
	var best time.Duration
	var name string
	if len(jobs) > 0 && len(s.Jobs) > 0 {
		// Each regular expression compiled once, not once a job.
		jobs = slices.Clone(jobs)
		for i := range jobs {
			if jobs[i].Re && jobs[i].re == nil {
				jobs[i].re = compileJob(jobs[i].Value)
			}
		}
		for _, j := range s.sorted() {
			if d := s.Jobs[j]; all(jobs, j) && d > best {
				best, name = d, j
			}
		}
		if best > 0 {
			return best, name
		}
	}
	if d, ok := s.uniform(); ok {
		return d, ""
	}
	return s.Global, ""
}

// A Matcher is a selector's matcher on the job label, = or =~; those that
// say what the job isn't don't say which it is, and aren't kept.
type Matcher struct {
	Re    bool
	Value string
	re    *regexp.Regexp // Value compiled, for =~; nil when it doesn't
}

// compileJob is a job matcher's regular expression, anchored as PromQL's
// are; nil when it doesn't compile, and matches no job.
func compileJob(v string) *regexp.Regexp {
	re, err := regexp.Compile("^(?:" + v + ")$")
	if err != nil {
		return nil
	}
	return re
}

func all(ms []Matcher, job string) bool {
	for _, m := range ms {
		if !m.Re && m.Value != job || m.Re && (m.re == nil || !m.re.MatchString(job)) {
			return false
		}
	}
	return true
}

// unquote is a PromQL string's value: "x", 'x' or `x`.
func unquote(s string) (string, bool) {
	if len(s) < 2 {
		return "", false
	}
	switch q := s[0]; {
	case q == '`' && s[len(s)-1] == '`':
		return s[1 : len(s)-1], true
	case q == '\'' && s[len(s)-1] == '\'':
		inner := strings.ReplaceAll(strings.ReplaceAll(s[1:len(s)-1], `\'`, `'`), `"`, `\"`)
		v, err := strconv.Unquote(`"` + inner + `"`)
		return v, err == nil
	case q == '"':
		v, err := strconv.Unquote(s)
		return v, err == nil
	}
	return "", false
}

// Shortest is how often the series an expression reads are scraped, at the
// shortest, as a replay must ask to see every sample of each: for a
// selector naming its job, the shortest of the jobs it can be; for one
// that doesn't, the shortest of every job and the global interval; the
// global interval for one that reads no series. 0 when the global
// interval isn't known.
func (s Scrapes) Shortest(expr string) time.Duration {
	if s.Global <= 0 {
		return 0
	}
	sels, ok := selectorJobs(expr)
	switch {
	case !ok:
		return s.shortestOf(nil)
	case len(sels) == 0:
		return s.Global // it reads no series
	}
	var best time.Duration
	for _, ms := range sels {
		if d := s.shortestOf(ms); best == 0 || d < best {
			best = d
		}
	}
	return best
}

// shortestOf is the shortest interval of the jobs these matchers on the
// job label can be: for none, of every job and the global interval; the
// global interval when they match no job known.
func (s Scrapes) shortestOf(ms []Matcher) time.Duration {
	var best time.Duration
	if len(ms) == 0 {
		best = s.Global
	}
	for _, job := range s.sorted() {
		if d := s.Jobs[job]; (len(ms) == 0 || all(ms, job)) && (best == 0 || d < best) {
			best = d
		}
	}
	if best == 0 {
		return s.Global
	}
	return best
}

// grouping are the words whose parentheses hold label names, not
// selectors.
var grouping = map[string]bool{"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true}

// notSelector are the words that aren't a metric's name where one could be.
var notSelector = map[string]bool{"and": true, "or": true, "unless": true, "bool": true, "offset": true, "by": true, "without": true,
	"on": true, "ignoring": true, "group_left": true, "group_right": true, "inf": true, "nan": true, "Inf": true, "NaN": true, "atan2": true}

// selectorJobs are, for each selector in an expression, its matchers on
// the job label (none for one that names no job); ok is false when the
// expression can't be read.
func selectorJobs(expr string) (out [][]Matcher, ok bool) {
	e := lex(expr)
	ts := e.ts
	if ts == nil && strings.TrimSpace(expr) != "" {
		return nil, false
	}
	compiled := func(ms []Matcher) []Matcher {
		for i := range ms {
			if ms[i].Re && ms[i].re == nil {
				ms[i].re = compileJob(ms[i].Value)
			}
		}
		return ms
	}
	for j := 0; j < len(ts); j++ {
		t := ts[j]
		next := ""
		if j+1 < len(ts) {
			next = ts[j+1].text
		}
		switch {
		case t.kind == 'i' && grouping[t.text] && next == "(":
			if e.match[j+1] < 0 {
				return nil, false
			}
			j = e.match[j+1]
		case t.text == "{":
			end := j + 1
			for end < len(ts) && ts[end].text != "}" {
				end++
			}
			if end == len(ts) {
				return nil, false
			}
			out = append(out, compiled(jobMatchers(ts, end+1)))
			j = end
		case t.kind == 'i' && !notSelector[t.text] && next != "(" && next != "{" && !grouping[next]:
			out = append(out, nil) // a metric's name alone; max by (...) isn't
		}
	}
	return out, true
}
