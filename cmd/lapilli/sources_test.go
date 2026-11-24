package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// grafanaOn is a file of Grafana's provisioning with a rule for each data
// source uid, each on its own PromQL query.
func grafanaOn(uids ...string) string {
	var b strings.Builder
	b.WriteString("apiVersion: 1\ngroups:\n  - orgId: 1\n    name: g\n    folder: f\n    interval: 1m\n    rules:\n")
	for i, uid := range uids {
		fmt.Fprintf(&b, `      - uid: r%d
        title: Rule%d
        condition: B
        data:
          - {refId: A, datasourceUid: %s, relativeTimeRange: {from: 600, to: 0}, model: {expr: errors, instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [5]}}]}
`, i, i, uid)
	}
	return b.String()
}

// Grafana's rules from files that name more than one data source for their
// PromQL are said to be replayed against the one --prometheus: a warning,
// the exit status as it was; one data source, or none, says nothing.
func TestVerdictWarnsOfSeveralHistories(t *testing.T) {
	was, wasNow := newHistory, now
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	t.Cleanup(func() { newHistory, now = was, wasNow })
	inc := write(t, "incidents.yaml", "incidents:\n  - {id: INC-1, title: Errors, start: \"2026-11-01T20:40:00Z\", end: \"2026-11-01T20:50:00Z\"}\n")
	verdict := func(file string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"verdict", "--prometheus", "http://p", "--incidents", inc, "--rules", write(t, "grafana.yaml", file), "--extras", "0", "--tz", "UTC", "--json", "--progress=false",
			"--from", "2026-11-01T00:00:00Z", "--to", "2026-11-02T00:00:00Z"}, &stdout, &stderr)
		return code, stderr.String()
	}
	code, said := verdict(grafanaOn("prom-a", "prom-b"))
	want := "lapilli verdict: Grafana's rules query 2 data sources (prom-a, prom-b), and every one is replayed against one history, --prometheus: replay each data source's rules in a run of their own against its Prometheus (--rule picks them by name), or the others' are judged by the wrong history\n"
	if code != 0 || strings.Count(said, want) != 1 {
		t.Errorf("two data sources: exit %d, stderr %q", code, said)
	}
	if code, said := verdict(grafanaOn("prom-a", "prom-a")); code != 0 || strings.Contains(said, "data sources") {
		t.Errorf("one data source: exit %d, stderr %q", code, said)
	}
	_, said = verdict(grafanaOn("prom-a", "prom-b", "prom-c", "prom-d", "prom-e", "prom-f", "prom-g"))
	if !strings.Contains(said, "query 7 data sources (prom-a, prom-b, prom-c, prom-d, prom-e and 2 more), and") {
		t.Errorf("seven data sources: %q", said)
	}
}
