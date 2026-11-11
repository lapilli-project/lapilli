package replay

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// points is a history whose every query returns one series, its value at
// each step that function of the time; it keeps what it was asked.
type points struct {
	mu    sync.Mutex
	name  string
	v     func(t time.Time) (float64, bool)
	asked []string
}

func (p *points) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	p.mu.Lock()
	p.asked = append(p.asked, fmt.Sprintf("%s %s %s %s", expr, start.UTC().Format("15:04:05.000"), end.UTC().Format("15:04:05.000"), step))
	p.mu.Unlock()
	var s prom.Series
	s.Labels = map[string]string{"from": p.name}
	for t := start; !t.After(end); t = t.Add(step) {
		if v, ok := p.v(t); ok {
			s.Samples = append(s.Samples, prom.Sample{T: t, V: v})
		}
	}
	if len(s.Samples) == 0 {
		return nil, nil
	}
	return []prom.Series{s}, nil
}

// TestLogReduce: a Grafana reduce over a LogQL range query's points, as
// Grafana's Loki data source asks them: every Res on its grid, from the
// window before the evaluation, less its window's end, to that end, both
// in; one range asked for the whole stretch.
func TestLogReduce(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 19, 0, 0, 0, time.UTC)
	// The value at each second is its second of the minute; nothing from
	// 19:02:00 to 19:02:59.
	h := &points{v: func(t time.Time) (float64, bool) { return float64(t.Second()), t.Minute() != 2 }}
	a := rules.Ask{Expr: `sum(count_over_time({app="x"}[1m]))`, Window: 30 * time.Second, Res: time.Second, Logs: true}
	for fn, want := range map[string]string{
		// At 19:01:00 less 10s: the points from 19:00:20 to 19:00:50.
		"max": "50", "min": "20", "mean": "35", "sum": "1085", "count": "31", "last": "50",
	} {
		a.Fn = fn
		got, err := logReduce(context.Background(), h, a, t0.Add(time.Minute), t0.Add(4*time.Minute), time.Minute, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var vals []string
		for _, s := range got[0].Samples {
			vals = append(vals, fmt.Sprintf("%s=%g", s.T.Format("04"), s.V))
		}
		// 19:03 reads 19:02:20 to 19:02:50: no point, no value.
		if len(got) != 1 || !strings.HasPrefix(strings.Join(vals, " "), "01="+want+" 02=") || strings.Contains(strings.Join(vals, " "), "03=") {
			t.Errorf("%s: %v, want 01=%s, and none at 03", fn, vals, want)
		}
	}
	if len(h.asked) != 6 || h.asked[0] != `sum(count_over_time({app="x"}[1m])) 19:00:20.000 19:03:50.000 1s` {
		t.Errorf("asked %q: one range a reduce", h.asked)
	}
	// A step in milliseconds: the window's ends are on its grid since the
	// epoch, outward, as Loki answers: 19:00:28.528 to 19:01:01.916, 18
	// points.
	h.asked = nil
	a.Fn, a.Res = "count", 1964*time.Millisecond
	got, err := logReduce(context.Background(), h, a, t0.Add(time.Minute), t0.Add(time.Minute), time.Minute, 0)
	if err != nil || len(got) != 1 || got[0].Samples[0].V != 18 || !strings.HasPrefix(h.asked[0], "sum(count_over_time({app=\"x\"}[1m])) 19:00:28.528 19:01:01.916") {
		t.Errorf("%v %+v %q", err, got, h.asked)
	}
}

// TestRoutedToLoki: a rule written in LogQL is asked of its Loki, a PromQL
// rule of the same text of Prometheus, and the two never share an answer;
// without a Loki, the LogQL rule isn't replayed.
func TestRoutedToLoki(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 19, 0, 0, 0, time.UTC)
	promH := &points{name: "prometheus", v: func(t time.Time) (float64, bool) { return 1, t.Minute()%10 < 5 }}
	lokiH := &points{name: "loki", v: func(t time.Time) (float64, bool) { return 1, t.Minute()%10 >= 5 }}
	expr := `sum(count_over_time({app="shop"}[5m])) > 0`
	rs := []rules.Rule{{Name: "P", Expr: expr}, {Name: "L", Expr: expr, Logs: rules.LokiRuler}}
	h := WithLogs(promH, func(r rules.Rule) (History, error) {
		if r.Logs != rules.LokiRuler {
			t.Errorf("asked for %q", r.Logs)
		}
		return lokiH, nil
	})
	ran, fs := Run(context.Background(), h, []Version{{Rules: rs}}, t0, t0.Add(time.Hour), time.Minute, 2)
	for _, r := range ran {
		if r.Unsupported != "" {
			t.Errorf("%s: %s", r.Name, r.Unsupported)
		}
	}
	for _, f := range fs {
		if (f.Rule == "P") != (f.Labels["from"] == "prometheus") || f.Start.Minute()%10 != map[string]int{"P": 0, "L": 5}[f.Rule] {
			t.Errorf("%s fired %v from %s", f.Rule, f.Labels, f.Start.Format("15:04"))
		}
	}
	// P at 19:00 and every ten minutes to 20:00, L at 19:05 to 19:55.
	if len(fs) != 13 || len(promH.asked) == 0 || len(lokiH.asked) == 0 {
		t.Errorf("%d firings, Prometheus asked %d, Loki %d", len(fs), len(promH.asked), len(lokiH.asked))
	}
	if Base(h) != History(promH) {
		t.Error("Base should be the Prometheus")
	}
	// Without WithLogs, nothing to ask.
	ran, _ = Run(context.Background(), promH, []Version{{Rules: rs}}, t0, t0.Add(time.Hour), time.Minute, 2)
	if ran[0].Unsupported != "" || !strings.Contains(ran[1].Unsupported, "no Loki to ask") {
		t.Errorf("%q %q", ran[0].Unsupported, ran[1].Unsupported)
	}
	if _, err := Condition(context.Background(), promH, rs[1], t0, t0.Add(time.Hour), time.Minute); err == nil {
		t.Error("a LogQL rule's condition without a Loki should fail")
	}
}

// TestLokiNoData: a Grafana rule on Loki whose query returned nothing is
// NoData only once it is known to have returned something: in the day
// before, asked of Loki over the day, as LogQL has no subqueries.
func TestLokiNoData(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 19, 0, 0, 0, time.UTC)
	rs, err := rules.Parse([]byte(lokiNoData))
	if err != nil {
		t.Fatal(err)
	}
	for _, seen := range []bool{true, false} {
		// Something at 12:00 the day before, if seen; nothing since.
		lokiH := &points{v: func(t time.Time) (float64, bool) { return 3, seen && t.Equal(t0.Add(-7*time.Hour)) }}
		h := WithLogs(&points{v: func(time.Time) (float64, bool) { return 0, false }}, func(rules.Rule) (History, error) { return lokiH, nil })
		ran, fs := Run(context.Background(), h, []Version{{Rules: rs}}, t0, t0.Add(10*time.Minute), time.Minute, 1)
		if ran[0].Unsupported != "" {
			t.Fatal(ran[0].Unsupported)
		}
		if (len(fs) == 1 && fs[0].Labels["grafana_alertstate"] == "nodata") != seen {
			t.Errorf("seen the day before %v: %+v", seen, fs)
		}
		probed := false
		for _, a := range lokiH.asked {
			probed = probed || strings.HasPrefix(a, `sum(count_over_time({app="gappy"}[30s])) 18:`) && strings.HasSuffix(a, " 1m0s")
		}
		if !probed {
			t.Errorf("the day before wasn't asked of Loki: %q", lokiH.asked)
		}
	}
}

const lokiNoData = `apiVersion: 1
groups:
  - orgId: 1
    name: logs
    folder: Logs
    interval: 1m
    rules:
      - uid: gappy
        title: Gappy
        condition: B
        noDataState: NoData
        data:
          - refId: A
            queryType: instant
            relativeTimeRange: {from: 600, to: 0}
            datasourceUid: loki
            model: {datasource: {type: loki, uid: loki}, expr: 'sum(count_over_time({app="gappy"}[30s]))', queryType: instant}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [0]}}]}
`
