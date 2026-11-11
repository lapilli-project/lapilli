package rules

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Grafana 13.3.1's export of three rules, as --grafana reads it: their
// titles and labels with every $ written $$, their annotations and queries
// as written (testdata/grafana-13.3.1-export.yaml). Read as Grafana's file
// provisioning reads them.
func TestGrafanaExportDollars(t *testing.T) {
	b, err := os.ReadFile("testdata/grafana-13.3.1-export.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "title: Cost $$5") || !strings.Contains(string(b), "team: '{{ $$labels.team }}'") {
		t.Fatal("not the export as Grafana wrote it")
	}
	rs, err := Parse(b)
	if err != nil || len(rs) != 3 {
		t.Fatalf("%+v %v", rs, err)
	}
	by := map[string]Rule{}
	for _, r := range rs {
		by[r.UID] = r
	}
	cost := by["cost"]
	if cost.Name != "Cost $5" || cost.Labels["budget"] != "$5" || cost.Labels["team"] != "{{ $labels.team }}" || cost.Group != "dollars/dollars" {
		t.Errorf("read: %q %v %q", cost.Name, cost.Labels, cost.Group)
	}
	// Annotations, as Grafana takes them: as written.
	if cost.Says.Summary != "{{ $labels.team }} spends {{ $value }}" {
		t.Errorf("summary %q", cost.Says.Summary)
	}
	if d := by["dollars"].Says.Description; d != "costs $5 a day; ${HOME} stays as written" {
		t.Errorf("description %q", d)
	}
	// The macros, as Grafana's Prometheus data source puts them in, for a
	// 10m window at its 1s: the query holds only if they are 60s, 600s
	// and 1s, as measured.
	want := `label_replace(count_over_time(vector(1)[1m0s:1s]) == 60 and on() (count_over_time(vector(1)[600s:1s]) == 600) and on() (count_over_time(vector(1)[1s:100ms]) == 10), "team", "shop", "", "")`
	if q := cost.Graph.Queries["A"]; q != want {
		t.Errorf("query\n got %s\nwant %s", q, want)
	}
	if cost.Unsupported != "" || strings.Contains(cost.Graph.Queries["A"], "$") {
		t.Errorf("%q, %s", cost.Unsupported, cost.Graph.Queries["A"])
	}
}

// $$ is read as $, by Grafana's rule: split at $$, and joined with $; and
// only where its file provisioning interpolates. Nothing else is
// expanded: no environment variable.
func TestProvisioned(t *testing.T) {
	for in, want := range map[string]string{"$$5": "$5", "$$$": "$$", "$$$$": "$$", "a$b": "a$b", "${HOME}": "${HOME}", "$__env{X}": "$__env{X}"} {
		if got := provisioned(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	rm := map[string]any{"title": "A $$1", "labels": map[string]any{"x": "$$y"}, "annotations": map[string]any{"summary": "$$z"},
		"data": []any{map[string]any{"refId": "$$A", "model": map[string]any{"expr": "x[$$__range]"}}}}
	got := provisionedRule(rm)
	if got["title"] != "A $1" || got["labels"].(map[string]any)["x"] != "$y" || got["annotations"].(map[string]any)["summary"] != "$$z" {
		t.Errorf("%v", got)
	}
	d := got["data"].([]any)[0].(map[string]any)
	if d["refId"] != "$A" || d["model"].(map[string]any)["expr"] != "x[$$__range]" || rm["title"] != "A $$1" {
		t.Errorf("data %v, the rule itself %v", d, rm["title"])
	}
}

// Grafana's Prometheus macros, against what Grafana 13.3.1 put in queries
// through its /api/v1/eval (testdata/grafana-13.3.1-macros.json): each
// value, for each data source, window and intervalMs measured.
func TestMacros(t *testing.T) {
	b, err := os.ReadFile("testdata/grafana-13.3.1-macros.json")
	if err != nil {
		t.Fatal(err)
	}
	var measured map[string]float64
	if err := json.Unmarshal(b, &measured); err != nil {
		t.Fatal(err)
	}
	key := regexp.MustCompile(`^(\w+) from=(\d+) intervalMs=(\d+) (\w+)$`)
	// How each was asked: the value itself, or how many points of a
	// subquery fit in it.
	asked := map[string]string{"interval_ms": "$__interval_ms", "rate_interval_ms": "$__rate_interval_ms", "range_ms": "$__range_ms",
		"range_s": "$__range_s", "interval": "$__interval", "rate_interval": "$__rate_interval", "range": "$__range", "braced": "${__rate_interval}"}
	per := map[string]time.Duration{"interval": 100 * time.Millisecond, "rate_interval": time.Second, "range": time.Second, "braced": time.Second}
	for k, v := range measured {
		m := key.FindStringSubmatch(k)
		if m == nil {
			t.Fatalf("%q", k)
		}
		from, _ := strconv.Atoi(m[2])
		ms, _ := strconv.Atoi(m[3])
		model := map[string]any{"intervalMs": ms}
		span := time.Duration(from) * time.Second
		got := macros(asked[m[4]], model, resolutionOf(model, span, 0), span, 0)
		if p, ok := per[m[4]]; ok {
			d, err := time.ParseDuration(got)
			if err != nil || float64(d/p) != v {
				t.Errorf("%s: %s, %v points of %s where Grafana's had %v", k, got, float64(d/p), p, v)
			}
			continue
		}
		if n, err := strconv.ParseFloat(got, 64); err != nil || n != v {
			t.Errorf("%s: %s, Grafana's %v", k, got, v)
		}
	}
	if got := macros("rate(x[$__rate_interval])", map[string]any{"interval": "30s"}, time.Second, 10*time.Minute, 0); got != "rate(x[2m0s])" {
		t.Errorf("with the query's own interval: %s", got)
	}
}

// A rule's folder and notification settings are among what Grafana's
// export escapes: provisioned from a file with every $ written $$, Grafana
// 13.3.1 kept the rule in the folder "Ops $1", sent to "pager $team", and
// exported both with $$ again (testdata/grafana-13.3.1-export-notify.*).
// They are read as Grafana has them: the content check routes the rule
// by its folder and receiver, as Grafana's policies name them.
func TestGrafanaExportNotifyDollars(t *testing.T) {
	b, err := os.ReadFile("testdata/grafana-13.3.1-export-notify.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "folder: Ops $$1") || !strings.Contains(string(b), "receiver: pager $$team") {
		t.Fatal("not the export as Grafana wrote it")
	}
	var answer struct {
		Rule struct {
			Title, RuleGroup string
			Labels           map[string]string
			Settings         struct{ Receiver string } `json:"notification_settings"`
		}
		Folder        string
		ContactPoints []string
	}
	a, err := os.ReadFile("testdata/grafana-13.3.1-export-notify.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(a, &answer); err != nil {
		t.Fatal(err)
	}
	rs, err := Parse(b)
	if err != nil || len(rs) != 1 {
		t.Fatalf("%+v %v", rs, err)
	}
	r, want := rs[0], answer.Rule
	if r.Name != want.Title || r.Group != answer.Folder+"/"+want.RuleGroup || r.Labels["team"] != want.Labels["team"] {
		t.Errorf("read %q in %q, %v; Grafana has %q in %q/%q, %v", r.Name, r.Group, r.Labels, want.Title, answer.Folder, want.RuleGroup, want.Labels)
	}
	if r.Folder != answer.Folder || r.Receiver != want.Settings.Receiver || !slices.Contains(answer.ContactPoints, r.Receiver) {
		t.Errorf("read the folder %q and the receiver %q; Grafana has %q and %q", r.Folder, r.Receiver, answer.Folder, want.Settings.Receiver)
	}
}

// A Grafana provisioning file with $$ in a rule's folder, group, uid and
// title is edited as it is read: the rule found by what Grafana calls it,
// and everything else written as it was, $$ and all. The edit itself, a
// duration, a pause or an annotation, is in no field Grafana interpolates
// but its for, which has no $.
func TestEditGrafanaDollars(t *testing.T) {
	file := `apiVersion: 1
groups:
  - orgId: 1
    name: costs $$g
    folder: Ops $$1
    interval: 1m
    rules:
      - uid: cost$$5
        title: Cost over $$5
        condition: C
        data:
          - refId: A
            relativeTimeRange: {from: 600, to: 0}
            datasourceUid: prom
            model: {expr: cost, refId: A}
          - refId: C
            datasourceUid: __expr__
            model: {type: threshold, expression: A, refId: C, conditions: [{evaluator: {type: gt, params: [5]}}]}
        for: 5m
`
	for name, f := range map[string]string{"with its uid": file, "by its title": strings.Replace(file, "      - uid: cost$$5\n        title:", "      - title:", 1)} {
		rs, err := Parse([]byte(f))
		if err != nil || len(rs) != 1 {
			t.Fatalf("%s: %v %+v", name, err, rs)
		}
		r := rs[0]
		if r.Group != "Ops $1/costs $g" || r.Name != "Cost over $5" {
			t.Fatalf("%s: read %q in %q", name, r.Name, r.Group)
		}
		d := time.Minute
		for _, e := range []Edit{{For: &d}, {Pause: true}, {Annotation: "summary", Value: "costs {{ $value }}"}} {
			e.Name, e.Group, e.UID = r.Name, r.Group, r.UID
			out, err := EditDoc([]byte(f), e)
			if err != nil {
				t.Errorf("%s: %+v: %v", name, e, err)
				continue
			}
			if !strings.Contains(string(out), "folder: Ops $$1") || !strings.Contains(string(out), "name: costs $$g") || !strings.Contains(string(out), "title: Cost over $$5") {
				t.Errorf("%s: the file's own escaping lost:\n%s", name, out)
			}
		}
	}
}

// F1: Grafana's $__rate_interval is put in with the data source's own
// scrape interval (jsonData.timeInterval), measured through Grafana
// 13.3.1's /api/v1/eval on a data source that sets none and one that
// sets 30s (testdata/grafana-13.3.1-macros-ds.json): 1m and 2m; a query's
// own interval in its place, $__interval as the step, and
// $__rate_interval as the query's interval makes it its step too.
func TestGrafanaMacrosDataSource(t *testing.T) {
	b, err := os.ReadFile("testdata/grafana-13.3.1-macros-ds.json")
	if err != nil {
		t.Fatal(err)
	}
	var measured map[string][]any
	if err := json.Unmarshal(b, &measured); err != nil {
		t.Fatal(err)
	}
	scrapes := map[string]time.Duration{"prom": 0, "prom30": 30 * time.Second}
	value := regexp.MustCompile(`^(\w+) (\$\w+) (.+) from=(\d+)$`)
	steps := regexp.MustCompile(`^(\w+) range step (.+)$`)
	n := 0
	for k, v := range measured {
		model := map[string]any{"intervalMs": 1000.0, "maxDataPoints": 43200.0}
		if m := value.FindStringSubmatch(k); m != nil {
			json.Unmarshal([]byte(m[3]), &model)
			if m[3] != "null" {
				extra := map[string]any{}
				json.Unmarshal([]byte(m[3]), &extra)
				model = map[string]any{"intervalMs": 1000.0, "maxDataPoints": 43200.0}
				maps.Copy(model, extra)
			}
			from, _ := strconv.Atoi(m[4])
			span, scrape := time.Duration(from)*time.Second, scrapes[m[1]]
			got := macros("vector("+m[2]+")", model, resolutionOf(model, span, scrape), span, scrape)
			if want := fmt.Sprintf("vector(%v)", v[0]); got != want {
				t.Errorf("%s: %s, Grafana's %s", k, got, want)
			}
			n++
			continue
		}
		if m := steps.FindStringSubmatch(k); m != nil {
			if m[2] != "null" {
				extra := map[string]any{}
				json.Unmarshal([]byte(m[2]), &extra)
				maps.Copy(model, extra)
			}
			p, _ := v[0].([]any)
			step, _ := p[1].(float64)
			if got := resolutionOf(model, 10*time.Minute, scrapes[m[1]]); got != time.Duration(step*float64(time.Second)) {
				t.Errorf("%s: asked every %s, Grafana every %vs", k, got, step)
			}
			n++
		}
	}
	if n != len(measured) || n < 20 {
		t.Errorf("%d of %d measured", n, len(measured))
	}
	// Read without Grafana's word on its data sources, a rule takes 15s;
	// with it, the data source's; with --scrape-interval, that for a data
	// source Grafana didn't say of.
	rs, err := Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 10s
    rules:
      - uid: m30
        title: M
        condition: B
        data:
          - {refId: A, datasourceUid: prom30, relativeTimeRange: {from: 600, to: 0}, model: {expr: 'vector($__rate_interval_ms)', intervalMs: 1000, maxDataPoints: 43200, instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [0]}}]}
`))
	if err != nil || len(rs) != 1 || rs[0].Unsupported != "" || !slices.Equal(rs[0].Graph.ScrapeSources(), []string{"prom30"}) {
		t.Fatalf("%v %+v", err, rs)
	}
	for _, c := range []struct {
		p    Replaying
		want string
	}{
		{Replaying{}, "vector(60000)"},
		{Replaying{DataSources: DataSources{"prom30": 30 * time.Second}}, "vector(120000)"},
		{Replaying{Scrape: 30 * time.Second}, "vector(120000)"},
		{Replaying{Scrape: 30 * time.Second, DataSources: DataSources{"prom30": 0}}, "vector(60000)"},
		{Replaying{Scrape: time.Minute, DataSources: DataSources{"prom30": 30 * time.Second}}, "vector(120000)"},
	} {
		got := slices.Clone(rs)
		c.p.Apply(got)
		if q := got[0].Graph.Queries["A"]; q != c.want {
			t.Errorf("%+v: %s, want %s", c.p, q, c.want)
		}
	}
	if rs[0].Graph.Queries["A"] != "vector(60000)" {
		t.Errorf("the rule read was changed: %s", rs[0].Graph.Queries["A"])
	}
}

// F2: without --scrape-interval, each reduce the server works out is asked
// at the interval the series it reads are scraped at (Replaying.ScrapeOf);
// a reduce that counts points at Grafana's resolution all the same; and
// where neither is known, at Grafana's resolution, not the server's own.
func TestReplayScrapeOf(t *testing.T) {
	rs, err := Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 10s
    rules:
      - uid: c
        title: C
        condition: C
        data:
          - {refId: A, datasourceUid: prom, relativeTimeRange: {from: 60, to: 0}, model: {expr: 'fv{job="fid"}', range: true}}
          - refId: C
            datasourceUid: __expr__
            model: {type: classic_conditions, conditions: [{evaluator: {type: gt, params: [50]}, operator: {type: and}, query: {params: [A]}, reducer: {type: max}},
              {evaluator: {type: gt, params: [5]}, operator: {type: or}, query: {params: [A]}, reducer: {type: count}}]}
`))
	if err != nil || len(rs) != 1 || rs[0].Unsupported != "" {
		t.Fatalf("%v %+v", err, rs)
	}
	cs := rs[0].Graph.Steps["C"].Classic.Conds
	asked := func(r Rule, ref string) string {
		q, _ := r.Graph.Expr(ref, r.ScrapeOf(ref))
		return q
	}
	of := func(expr string) time.Duration {
		if strings.Contains(expr, "fid") {
			return 15 * time.Second
		}
		return 0
	}
	for _, c := range []struct {
		p          Replaying
		max, count string
	}{
		{Replaying{ScrapeOf: of}, `max_over_time(((fv{job="fid"}) >= -Inf)[1m7s500ms:15s])`, `count_over_time((fv{job="fid"})[1m500ms:1s])`},
		{Replaying{Scrape: 30 * time.Second, ScrapeOf: of}, `max_over_time(((fv{job="fid"}) >= -Inf)[1m15s:30s])`, `count_over_time((fv{job="fid"})[1m500ms:1s])`},
		{Replaying{}, `max_over_time(((fv{job="fid"}) >= -Inf)[1m500ms:1s])`, `count_over_time((fv{job="fid"})[1m500ms:1s])`},
	} {
		got := slices.Clone(rs)
		c.p.Apply(got)
		if m, n := asked(got[0], cs[0].Value), asked(got[0], cs[1].Value); m != c.max || n != c.count {
			t.Errorf("%+v:\n%s\n%s", c.p.Scrape, m, n)
		}
	}
}

// F3: a reduce is asked at the greatest step dividing the scrape interval,
// the rule's interval and the window, so each evaluation and its window's
// start are its steps: seen on Grafana 13.3.1, a rule every 10s on a gauge
// scraped every 15s, asked every 15s, started 20 of 21 firings an
// evaluation late; every 5s, none.
func TestAskStep(t *testing.T) {
	for _, c := range []struct {
		interval, scrape, window, want time.Duration
	}{
		{10 * time.Second, 15 * time.Second, time.Minute, 5 * time.Second},
		{time.Minute, 15 * time.Second, time.Minute, 15 * time.Second},
		{time.Minute, 15 * time.Second, 50 * time.Second, 5 * time.Second},
		{time.Minute, 0, time.Minute, 0},
		{10 * time.Second, 7 * time.Second, time.Minute, time.Second},
		{0, 30 * time.Second, 0, 30 * time.Second},
	} {
		r := Rule{Interval: c.interval, Scrape: c.scrape, Graph: &Graph{Asks: map[string]Ask{"A": {Fn: "max_over_time", Window: c.window}}}}
		if got := r.AskStep("A"); got != c.want {
			t.Errorf("%+v: %s", c, got)
		}
	}
}
