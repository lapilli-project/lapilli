package source

import (
	"context"
	"testing"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// Near misses by value of Grafana conditions read as a whole before: a
// math expression joining comparisons with &&, and classic conditions.
// The value that nearly held is named, and the number short of it.
func TestNearMissesJoined(t *testing.T) {
	rs, err := rules.Parse([]byte(`apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: m1
        title: Joined
        condition: C
        for: 5m
        noDataState: OK
        labels: {service: checkout}
        data:
          - {refId: A, datasourceUid: prom, model: {expr: latency, instant: true}}
          - {refId: B, datasourceUid: prom, model: {expr: gate, instant: true}}
          - {refId: C, datasourceUid: __expr__, model: {type: math, expression: '$A > 0.9 && $B > 0.05'}}
      - uid: c1
        title: Classic
        condition: C
        for: 5m
        noDataState: OK
        labels: {service: checkout}
        data:
          - {refId: A, datasourceUid: prom, model: {expr: latency, instant: true}}
          - {refId: B, datasourceUid: prom, model: {expr: gate, instant: true}}
          - refId: C
            datasourceUid: __expr__
            model:
              type: classic_conditions
              conditions:
                - {evaluator: {type: gt, params: [0.9]}, operator: {type: and}, query: {params: [A]}, reducer: {type: last}}
                - {evaluator: {type: gt, params: [0.05]}, operator: {type: and}, query: {params: [B]}, reducer: {type: last}}
`))
	if err != nil || len(rs) != 2 || rs[0].Unsupported != "" || rs[1].Unsupported != "" {
		t.Fatalf("%+v %v", rs, err)
	}
	for _, r := range rs {
		bs := nearBounds(r)
		if len(bs) != 2 || !bs[0].Values || bs[0].Value != 0.9 || bs[1].Value != 0.05 {
			t.Fatalf("%s: bounds %+v", r.Name, bs)
		}
	}
	o := verdict.Options{From: t0, To: t0.Add(7 * 24 * time.Hour), Lead: 15 * time.Minute, Match: []string{"service"},
		RefireWithin: time.Hour, UnknownLen: time.Hour}
	tue := t0.Add(24 * time.Hour)
	incs := []incident.Incident{{ID: "INC-1", Start: tue.Add(10 * time.Hour), End: tue.Add(10*time.Hour + 30*time.Minute),
		Labels: incident.Labels{"service": {"checkout"}}}}
	// latency is 0.88 through INC-1, and gate 0.1 throughout.
	h := &valued{}
	versions := []replay.Version{{Rules: rs}}
	_, fs := replay.Run(context.Background(), h, versions, o.From, o.To, time.Minute, 4)
	rep := verdict.Judge(rs, fs, incs, o)
	if len(fs) != 0 || !rep.Incidents[0].Missed() {
		t.Fatalf("fired %+v", fs)
	}
	nms, done, err := NearMisses(context.Background(), h, versions, rep, incs, o, time.Minute, 4, nil)
	if !done || len(nms) != 2 {
		t.Fatalf("%v %v %d near misses: %+v", done, err, len(nms), nms)
	}
	// An instant query's reducer is passed over: its value is compared.
	of := map[string]string{"Joined": "$A", "Classic": "A"}
	for _, nm := range nms {
		v := nm.Value
		if v == nil || v.Of != of[nm.Rule] || v.Op != ">" || v.Bound != 0.9 || v.Reached == nil || *v.Reached != 0.88 || v.New != 0.87 ||
			nm.Delay != 5*time.Minute {
			t.Errorf("%s: %+v %+v", nm.Rule, nm, v)
			continue
		}
		// Replayed with 0.87, it fires on INC-1, and on the Thursday for
		// nothing.
		if nm.After == nil || nm.After.Firings != 2 || nm.After.White != 1 {
			t.Errorf("%s's cost: %+v", nm.Rule, nm.After)
		}
	}
}
