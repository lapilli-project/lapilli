package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// sigBase is the Monday the signals' tests' period starts.
var sigBase = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

// at is a time of the signals' tests: day d of the period (0 its Monday,
// negative before it), at hh:mm.
func at(d, hh, mm int) time.Time {
	return sigBase.Add(time.Duration(d)*24*time.Hour + time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute)
}

// on is a series that returns something from from to to, both included.
type on struct {
	labels   map[string]string
	from, to time.Time
}

// signalHistory answers each expression it knows with its series, at the
// step asked, within the span asked; count() of a selector it knows, with
// a sample from where that selector's data begins; and anything else with
// an error, so that a question nobody expected fails the test.
type signalHistory struct {
	series map[string][]on
	// begins is where each selector's data begins.
	begins map[string]time.Time
	asked  *[]string
}

func (h signalHistory) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if h.asked != nil {
		*h.asked = append(*h.asked, expr)
	}
	if sel, ok := strings.CutPrefix(expr, "count("); ok {
		sel = strings.TrimSuffix(sel, ")")
		b, known := h.begins[sel]
		if !known {
			return nil, fmt.Errorf("no selector %s here", sel)
		}
		s := prom.Series{Labels: map[string]string{}}
		for t := start; !t.After(end); t = t.Add(step) {
			if !t.Before(b) {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
			}
		}
		if len(s.Samples) == 0 {
			return nil, nil
		}
		return []prom.Series{s}, nil
	}
	ons, ok := h.series[expr]
	if !ok {
		return nil, fmt.Errorf("no expression %q here", expr)
	}
	var out []prom.Series
	for _, o := range ons {
		s := prom.Series{Labels: o.labels}
		for t := start.Truncate(step); !t.After(end); t = t.Add(step) {
			if !t.Before(start) && !t.Before(o.from) && !t.After(o.to) {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
			}
		}
		if len(s.Samples) > 0 {
			out = append(out, s)
		}
	}
	return out, nil
}

func lbl(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// firingsFile writes firings, one JSON object a line: rule, labels as
// JSON, start and end (empty: still firing).
func firingsFile(t *testing.T, fs [][4]string) string {
	t.Helper()
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(`{"rule":"` + f[0] + `","labels":` + f[1] + `,"start":"` + f[2] + `"`)
		if f[3] != "" {
			b.WriteString(`,"end":"` + f[3] + `"`)
		}
		b.WriteString("}\n")
	}
	return write(t, "firings.jsonl", b.String())
}

func ts(t time.Time) string { return t.Format(time.RFC3339) }

// unnamedImpact is a week of firings judged against incidents written down
// and one impact signal without a name, as lapilli verdict has judged them
// since --impact came: windows of series that overlap made one, one too
// short to count, one before the period, one that is an incident written
// down.
func unnamedImpact(t *testing.T) (signalHistory, []string) {
	t.Helper()
	h := signalHistory{series: map[string][]on{"impact": {
		{lbl("service", "checkout", "team", "shop"), at(1, 10, 0), at(1, 10, 30)},
		{lbl("service", "cart", "team", "shop"), at(1, 10, 20), at(1, 11, 0)},
		{lbl("instance", "probe-a", "job", "blackbox"), at(3, 14, 0), at(3, 14, 3)},
		{lbl("service", "search", "team", "find"), at(4, 3, 0), at(4, 3, 20)},
		{lbl("service", "payments", "team", "pay"), at(5, 20, 0), at(5, 20, 15)},
		{lbl("service", "checkout", "team", "shop"), at(-10, 10, 0), at(-10, 10, 30)},
	}}}
	incidents := write(t, "incidents.yaml", `incidents:
  - {id: INC-1, title: Search down, start: `+ts(at(4, 3, 5))+`, end: `+ts(at(4, 3, 30))+`, labels: {service: search}}
  - {id: INC-2, title: Checkout errors, start: `+ts(at(2, 8, 0))+`, end: `+ts(at(2, 8, 30))+`, labels: {service: checkout}}
`)
	co, ca := `{"service":"checkout","team":"shop","severity":"critical"}`, `{"service":"cart","team":"shop","severity":"warning"}`
	firings := firingsFile(t, [][4]string{
		{"ApiErrors", co, ts(at(1, 9, 50)), ts(at(1, 10, 40))},
		{"ApiErrors", co, ts(at(2, 8, 10)), ts(at(2, 8, 20))},
		{"ApiErrors", co, ts(at(3, 12, 0)), ts(at(3, 12, 5))},
		{"CartSlow", ca, ts(at(1, 10, 30)), ts(at(1, 10, 50))},
		{"CartSlow", ca, ts(at(4, 5, 0)), ts(at(4, 5, 10))},
		{"SearchDown", `{"service":"search","team":"find","severity":"critical"}`, ts(at(4, 3, 10)), ts(at(4, 3, 30))},
		{"PayQueue", `{"service":"payments","team":"pay"}`, ts(at(5, 21, 0)), ts(at(5, 21, 5))},
		{"BackendMissing", `{"backend":"checkout","team":"platform","severity":"warning"}`, ts(at(1, 10, 5)), ts(at(1, 10, 20))},
	})
	return h, []string{"verdict", "--firings", firings, "--incidents", incidents, "--prometheus", "http://p", "--impact", "impact",
		"--from", ts(sigBase), "--to", ts(sigBase.Add(7 * 24 * time.Hour)), "--match", "service", "--match", "team", "--tz", "UTC"}
}

// A single --impact with no name judges as it did before signals had
// names: the report, the text, the metrics and the CSV are, byte for byte,
// what lapilli verdict wrote before (testdata/unnamed-impact, written by
// the release before named signals; LAPILLI_GOLDEN=update writes them
// again).
func TestUnnamedImpactUnchanged(t *testing.T) {
	h, args := unnamedImpact(t)
	var asked []string
	h.asked = &asked
	newHistory = func(context.Context, *verdictFlags) (history, error) { return h, nil }
	now = func() time.Time { return sigBase.Add(8 * 24 * time.Hour) }
	for _, out := range []struct{ file, flag string }{{"report.json", "--json"}, {"report.txt", ""}, {"report.prom", "--metrics"}, {"report.csv", "--csv"}} {
		a := args
		if out.flag != "" {
			a = append(append([]string{}, args...), out.flag)
		}
		var stdout, stderr bytes.Buffer
		if code := run(a, &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d\n%s", out.file, code, stderr.String())
		}
		path := filepath.Join("testdata", "unnamed-impact", out.file)
		if os.Getenv("LAPILLI_GOLDEN") == "update" {
			os.MkdirAll(filepath.Dir(path), 0o755)
			if err := os.WriteFile(path, stdout.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := stdout.String(); got != string(want) {
			t.Errorf("%s differs from what an unnamed --impact gave before:\n%s\nwant\n%s", out.file, got, want)
		}
	}
	// One question of the signal a run, and none of where its data begins.
	for _, q := range asked {
		if q != "impact" {
			t.Errorf("asked %q", q)
		}
	}
	if len(asked) != 4 {
		t.Errorf("asked %d times, want once a run", len(asked))
	}
}
