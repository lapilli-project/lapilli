package main

import (
	"context"
	"embed"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

const demoUsage = `lapilli demo shows lapilli on sample data, with nothing to connect: the
seven made-up alert rules of a small shop replayed over a made-up week of
their history, and judged against its incidents, by lapilli verdict's own
code. The history is made up in this process: it reads no Prometheus,
and sends nothing anywhere (docs/design/demo.md).

Usage:
  lapilli demo [--json] [--tz ZONE]

  --json              the whole report, every firing included, as lapilli
                      verdict --json gives it
  --tz ZONE           show times in this zone, an IANA name such as
                      Asia/Seoul or UTC (default: the machine's own). The
                      sample's week, from Monday 9 November 2026, is laid
                      in this zone: the report is the same in every zone
                      but for the zone it names

The same on your own rules, and incidents written down:
  lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 \
    --incidents incidents.yaml --match service
`

// demoFiles are lapilli demo's sample: its rules, as a rule file has them,
// and their history and incidents (demoSample).
//
//go:embed demo/rules.yaml demo/sample.yaml
var demoFiles embed.FS

// demoWeek is the start of the sample's week, Monday, in the zone shown.
func demoWeek(loc *time.Location) time.Time { return time.Date(2026, 11, 9, 0, 0, 0, 0, loc) }

// demoPrometheus is what lapilli demo gives verdict as --prometheus: never
// asked, the sample answering in its place (verdictOn).
const demoPrometheus = "lapilli-demo-sample"

func runDemo(args []string, stdout, stderr io.Writer) int {
	asJSON := false
	var tz string
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(args[i], "=")
		switch {
		case name == "-h" || name == "--help":
			fmt.Fprint(stdout, demoUsage)
			return 0
		case name == "--json" && !hasVal:
			asJSON = true
		case name == "--tz":
			if !hasVal {
				if i+1 == len(args) {
					return demoBad(stderr, "--tz needs a zone")
				}
				i++
				val = args[i]
			}
			tz = val
		default:
			return demoBad(stderr, fmt.Sprintf("unknown flag %s", verdict.Clean(name)))
		}
	}
	loc := time.Local
	if tz != "" {
		var err error
		if loc, err = time.LoadLocation(tz); err != nil {
			return demoBad(stderr, fmt.Sprintf("--tz %s: %v", verdict.Clean(tz), err))
		}
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "lapilli demo: %s\n", verdict.Clean(err.Error()))
		return 1
	}
	s, err := readDemoSample()
	if err != nil {
		return fail(err)
	}
	// The rules and the incidents as files, for verdict to read as it reads
	// anyone's; removed after.
	dir, err := os.MkdirTemp("", "lapilli-demo-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)
	rulesFile, incidentsFile := filepath.Join(dir, "rules.yaml"), filepath.Join(dir, "incidents.yaml")
	rules, err := demoFiles.ReadFile("demo/rules.yaml")
	if err == nil {
		err = os.WriteFile(rulesFile, rules, 0o600)
	}
	week := demoWeek(loc)
	if err == nil {
		// This week's incidents and the week before's, which keep the record
		// of its first days.
		err = os.WriteFile(incidentsFile, s.incidents(week.AddDate(0, 0, -7), week.AddDate(0, 0, 7), loc), 0o600)
	}
	if err != nil {
		return fail(err)
	}
	vargs := []string{"--rules", rulesFile, "--incidents", incidentsFile, "--match", "service", "--prometheus", demoPrometheus,
		"--from", week.Format(time.RFC3339), "--to", week.AddDate(0, 0, 7).Format(time.RFC3339), "--progress=false"}
	if tz != "" {
		vargs = append(vargs, "--tz", tz)
	}
	if asJSON {
		vargs = append(vargs, "--json")
	}
	if code := verdictOn(vargs, s.history(loc), stdout, stderr); code != 0 {
		return code
	}
	// What to run next: on stdout after the report; beside --json's, on
	// stderr, so that stdout is the JSON alone.
	next := stdout
	if asJSON {
		next = stderr
	}
	fmt.Fprint(next, demoNext)
	return 0
}

// demoNext is what lapilli demo says after its report.
const demoNext = `
That was lapilli verdict over a sample: made-up rules, history and incidents, nothing read or sent.
On your own: lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 --incidents incidents.yaml
With no incidents written down, lapilli impact suggest --prometheus http://prometheus:9090 says which --impact to give instead.
`

func demoBad(stderr io.Writer, why string) int {
	fmt.Fprintf(stderr, "lapilli demo: %s\nRun \"lapilli demo -h\" for the options.\n", why)
	return 2
}

// demoSample is demo/sample.yaml: the incidents of a week, and the series
// of each expression the rules compare, by the time into the week.
type demoSample struct {
	Incidents []struct {
		ID      string `yaml:"id"`
		Title   string `yaml:"title"`
		At      string `yaml:"at"`
		Lasted  string `yaml:"lasted"`
		Service string `yaml:"service"`
	} `yaml:"incidents"`
	Series []struct {
		Expr   string            `yaml:"expr"`
		Labels map[string]string `yaml:"labels"`
		Base   float64           `yaml:"base"`
		Spans  [][]string        `yaml:"spans"`
	} `yaml:"series"`

	incs   []demoIncident
	series map[string][]demoSeries
}

// demoIncident is an incident of every week: its number in the sample's
// week, and when it starts into a week.
type demoIncident struct {
	prefix         string
	n              int
	title, service string
	at, lasted     time.Duration
}

// demoSeries is a series of the sample: its labels, its value, and the
// spans it is another, by the time into a week.
type demoSeries struct {
	labels map[string]string
	base   float64
	spans  []demoSpan
}

type demoSpan struct {
	from, to time.Duration
	v        float64
}

var demoDays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// demoInto is a time into a week, "tue 12:01".
func demoInto(s string) (time.Duration, error) {
	day, clock, ok := strings.Cut(strings.TrimSpace(s), " ")
	d := -1
	for i, x := range demoDays {
		if x == day {
			d = i
		}
	}
	t, err := time.Parse("15:04", clock)
	if !ok || d < 0 || err != nil {
		return 0, fmt.Errorf("%q: want a day and a time, like tue 12:01", s)
	}
	return time.Duration(d)*24*time.Hour + time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

var demoID = regexp.MustCompile(`^(.*?)([0-9]+)$`)

func readDemoSample() (*demoSample, error) {
	b, err := demoFiles.ReadFile("demo/sample.yaml")
	if err != nil {
		return nil, err
	}
	s := &demoSample{series: map[string][]demoSeries{}}
	if err := yaml.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("demo/sample.yaml: %w", err)
	}
	const week = 7 * 24 * time.Hour
	for _, x := range s.Incidents {
		at, err := demoInto(x.At)
		lasted, lerr := time.ParseDuration(x.Lasted)
		m := demoID.FindStringSubmatch(x.ID)
		switch {
		case err != nil:
			return nil, fmt.Errorf("demo/sample.yaml: %s: %w", x.ID, err)
		case lerr != nil || at+lasted > week:
			return nil, fmt.Errorf("demo/sample.yaml: %s: lasted %q: want a duration within the week", x.ID, x.Lasted)
		case m == nil:
			return nil, fmt.Errorf("demo/sample.yaml: %s: want an id that ends in a number", x.ID)
		}
		n, _ := strconv.Atoi(m[2])
		s.incs = append(s.incs, demoIncident{prefix: m[1], n: n, title: x.Title, service: x.Service, at: at, lasted: lasted})
	}
	for _, x := range s.Series {
		ds := demoSeries{labels: x.Labels, base: x.Base}
		for _, sp := range x.Spans {
			if len(sp) != 3 {
				return nil, fmt.Errorf("demo/sample.yaml: %s: a span is [when, how long, value]", x.Expr)
			}
			from, err := demoInto(sp[0])
			long, lerr := time.ParseDuration(sp[1])
			v, verr := strconv.ParseFloat(sp[2], 64)
			if err != nil || lerr != nil || verr != nil || from+long > week {
				return nil, fmt.Errorf("demo/sample.yaml: %s: span %q: want [when, how long, value] within the week", x.Expr, sp)
			}
			ds.spans = append(ds.spans, demoSpan{from: from, to: from + long, v: v})
		}
		k := strings.TrimSpace(x.Expr)
		s.series[k] = append(s.series[k], ds)
	}
	return s, nil
}

// incidents is an incidents file of those that start in [from, to), each
// week's numbered on from the sample's: the week before's come before them.
func (s *demoSample) incidents(from, to time.Time, loc *time.Location) []byte {
	var b strings.Builder
	b.WriteString("incidents:\n")
	sample := demoWeek(loc)
	for w := from; w.Before(to); w = w.AddDate(0, 0, 7) {
		// Weeks from the sample's, by the calendar: a week across a change
		// of the clocks is a week still.
		k := int(w.Sub(sample).Round(24*time.Hour) / (7 * 24 * time.Hour))
		for _, x := range s.incs {
			start := demoAt(w, x.at, loc)
			if start.Before(from) || !start.Before(to) {
				continue
			}
			fmt.Fprintf(&b, "  - id: %s%d\n    title: %q\n    start: %s\n    end: %s\n    labels: {service: %q}\n",
				x.prefix, x.n+k*len(s.incs), x.title, start.Format(time.RFC3339), start.Add(x.lasted).Format(time.RFC3339), x.service)
		}
	}
	return []byte(b.String())
}

// demoAt is the time into a week from its Monday, by the clock in loc.
func demoAt(monday time.Time, into time.Duration, loc *time.Location) time.Time {
	day := into / (24 * time.Hour)
	rest := into - day*24*time.Hour
	return time.Date(monday.Year(), monday.Month(), monday.Day()+int(day), int(rest/time.Hour), int(rest%time.Hour/time.Minute), 0, 0, loc)
}

// history is the sample's history, its weeks laid in loc.
func (s *demoSample) history(loc *time.Location) *demoHistory {
	return &demoHistory{loc: loc, series: s.series}
}

// demoHistory answers as a Prometheus would that holds the sample's
// series: an expression the sample has a series of, alone or compared with
// a number at its end, which keeps the steps where it holds; anything
// else has no series. It asks nothing anywhere.
type demoHistory struct {
	loc    *time.Location
	series map[string][]demoSeries
}

// demoCompared is an expression compared with a number at its end: what,
// the operator and the number.
var demoCompared = regexp.MustCompile(`^(.*?)\s*(==|!=|>=|<=|>|<)\s*(-?[0-9][0-9.eE+-]*)\s*$`)

func (h *demoHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	what, op, n := strings.TrimSpace(expr), "", 0.0
	if _, ok := h.series[what]; !ok {
		m := demoCompared.FindStringSubmatch(what)
		if m == nil {
			return nil, nil
		}
		var err error
		if n, err = strconv.ParseFloat(m[3], 64); err != nil {
			return nil, nil
		}
		what, op = strings.TrimSpace(m[1]), m[2]
	}
	if step <= 0 {
		step = time.Minute
	}
	var out []prom.Series
	for _, s := range h.series[what] {
		x := prom.Series{Labels: maps.Clone(s.labels)}
		for t := start; !t.After(end); t = t.Add(step) {
			if v := s.at(t.In(h.loc)); op == "" || demoHolds(op, v, n) {
				x.Samples = append(x.Samples, prom.Sample{T: t, V: v})
			}
		}
		if len(x.Samples) > 0 {
			out = append(out, x)
		}
	}
	return out, nil
}

// at is the series' value at t, by the clock t is in.
func (s demoSeries) at(t time.Time) float64 {
	into := time.Duration((int(t.Weekday())+6)%7)*24*time.Hour +
		time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute + time.Duration(t.Second())*time.Second
	for _, sp := range s.spans {
		if into >= sp.from && into < sp.to {
			return sp.v
		}
	}
	return s.base
}

func demoHolds(op string, v, n float64) bool {
	switch op {
	case ">":
		return v > n
	case "<":
		return v < n
	case ">=":
		return v >= n
	case "<=":
		return v <= n
	case "==":
		return v == n
	}
	return v != n
}
