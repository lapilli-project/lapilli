package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// judgedRules are a Grafana's rules: GErrors on Prometheus, replayed, and
// Orders slow, whose condition reads a MySQL data source, which isn't.
const judgedRules = `apiVersion: 1
groups:
  - orgId: 1
    name: shop
    folder: Ops
    interval: 1m
    rules:
      - uid: gerrors
        title: GErrors
        condition: B
        noDataState: OK
        for: 0s
        data:
          - {refId: A, datasourceUid: prom, relativeTimeRange: {from: 600, to: 0}, model: {refId: A, expr: errors, instant: true}}
          - refId: B
            datasourceUid: __expr__
            model: {refId: B, type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [5]}}]}
      - uid: orders
        title: Orders slow
        condition: B
        for: 0s
        data:
          - {refId: A, datasourceUid: orders, relativeTimeRange: {from: 600, to: 0}, model: {refId: A, rawSql: "SELECT 1"}}
          - refId: B
            datasourceUid: __expr__
            model: {refId: B, type: threshold, expression: A, conditions: [{evaluator: {type: gt, params: [5]}}]}
`

// judgedGrafana stands in for a Grafana 12.4 holding judgedRules, its
// state history in annotations: Orders slow fired six times, ten minutes
// each, an hour apart up to an hour before t0; and a change long before,
// so the history reaches back past the period. With refuse, the settings
// answer 403.
type judgedGrafana struct {
	mu     sync.Mutex
	refuse bool
	asked  []string
}

func (g *judgedGrafana) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.asked = append(g.asked, r.URL.Path+" "+r.Header.Get("Authorization"))
	g.mu.Unlock()
	switch r.URL.Path {
	case "/api/v1/provisioning/alert-rules/export":
		io.WriteString(w, judgedRules)
	case "/api/datasources/uid/prom":
		io.WriteString(w, `{"uid": "prom", "type": "prometheus", "jsonData": {"prometheusType": "Prometheus"}}`)
	case "/api/datasources/uid/orders":
		io.WriteString(w, `{"uid": "orders", "type": "mysql", "jsonData": {}}`)
	case "/api/frontend/settings":
		if g.refuse {
			http.Error(w, `{"message":"no"}`, http.StatusForbidden)
			return
		}
		io.WriteString(w, `{"buildInfo":{"version":"12.4.14"},"unifiedAlerting":{"alertStateHistoryBackend":"annotations"}}`)
	case "/api/annotations":
		var from, to, limit int64
		json.Unmarshal([]byte(r.URL.Query().Get("from")), &from)
		json.Unmarshal([]byte(r.URL.Query().Get("to")), &to)
		json.Unmarshal([]byte(r.URL.Query().Get("limit")), &limit)
		note := func(id int, rule, prev, next string, at time.Time) map[string]any {
			return map[string]any{"id": id, "alertName": rule, "prevState": prev, "newState": next, "time": at.UnixMilli(),
				"tags": []string{"alertname:" + rule, "grafana_folder:Ops", "service:orders"}}
		}
		var all []map[string]any
		for k := 1; k <= 6; k++ { // newest first
			at := t0.Add(-time.Duration(k) * time.Hour)
			all = append(all, note(2*k, "Orders slow", "Alerting", "Normal", at.Add(10*time.Minute)), note(2*k-1, "Orders slow", "Normal", "Alerting", at))
		}
		all = append(all, note(100, "Old", "Normal", "Pending", t0.Add(-9*24*time.Hour)))
		out := []map[string]any{}
		for _, n := range all {
			if ms := n["time"].(int64); ms >= from && ms <= to && int64(len(out)) < limit {
				out = append(out, n)
			}
		}
		json.NewEncoder(w).Encode(out)
	default:
		http.NotFound(w, r)
	}
}

// The rules --grafana holds that can't be replayed are judged from what
// Grafana's own alert state history says they fired, read with the
// Viewer's token: their stones, precision and the incidents they caught
// as any rule's, said to be judged so, and with no longer "for" or near
// miss. --grafana-history=false lists them, as before; a history that
// can't be read lists them, and says why.
func TestVerdictGrafanaJudged(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return errorsHistory{}, nil }
	now = func() time.Time { return t0 }
	g := &judgedGrafana{}
	srv := httptest.NewServer(g)
	defer srv.Close()
	token := write(t, "token", "glsa_viewer\n")
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-9, title: Orders slow, start: 2026-11-01T19:00:00Z, end: 2026-11-01T19:30:00Z, labels: {service: orders}}\n")
	args := []string{"verdict", "--grafana", srv.URL, "--grafana-token-file", token, "--prometheus", "http://p", "--incidents", incidents, "--match", "service", "--tz", "UTC"}

	var stdout, stderr bytes.Buffer
	if code := run(append(args, "--json"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var rep struct {
		Rules []struct {
			Rule, Source, Unsupported string
			Firings, White            int
			Replayed                  bool
		} `json:"rules"`
		Incidents []struct {
			ID       string `json:"id"`
			CaughtBy []struct {
				Rule string `json:"rule"`
			} `json:"caught_by"`
		} `json:"incidents"`
		Suggestions []struct {
			Rule string `json:"rule"`
		} `json:"suggestions"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	judged := false
	for _, r := range rep.Rules {
		switch r.Rule {
		case "Orders slow":
			judged = true
			if r.Firings != 6 || r.White != 1 || r.Source != "grafana" || r.Replayed || !strings.Contains(r.Unsupported, "mysql") {
				t.Errorf("Orders slow: %+v", r)
			}
		case "GErrors":
			if r.Firings != 1 || r.Source != "" || !r.Replayed {
				t.Errorf("GErrors: %+v", r)
			}
		}
	}
	if !judged {
		t.Fatalf("Orders slow not judged: %s", stdout.String())
	}
	if len(rep.Incidents) != 1 || len(rep.Incidents[0].CaughtBy) != 1 || rep.Incidents[0].CaughtBy[0].Rule != "Orders slow" {
		t.Errorf("incidents: %+v", rep.Incidents)
	}
	for _, s := range rep.Suggestions {
		if s.Rule == "Orders slow" {
			t.Errorf("a longer for suggested for a rule judged from its history")
		}
	}
	for _, a := range g.asked {
		if strings.HasPrefix(a, "/api/annotations") && !strings.HasSuffix(a, " Bearer glsa_viewer") {
			t.Errorf("asked %s", a)
		}
	}

	stdout.Reset()
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{"Orders slow  ", "Judged from what their history says they fired, not replayed, 1 rules",
		"Orders slow, from Grafana's alert state history: its condition reads a mysql data source"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("no %q in:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "Could not replay") {
		t.Errorf("listed as not replayed alone:\n%s", stdout.String())
	}

	// Off, or with a history that can't be read: listed, with why.
	for _, c := range []struct {
		name   string
		more   []string
		refuse bool
		said   string
	}{
		{"off", []string{"--grafana-history=false"}, false, ""},
		{"refused", nil, true, "the 1 Grafana rules Lapilli can't replay aren't judged from Grafana's alert state history, and are listed"},
	} {
		g.mu.Lock()
		g.refuse, g.asked = c.refuse, nil
		g.mu.Unlock()
		stdout.Reset()
		stderr.Reset()
		if code := run(append(args, c.more...), &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d: %s", c.name, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Could not replay 1 rules:\n  Orders slow: its condition reads a mysql data source") {
			t.Errorf("%s: not listed:\n%s", c.name, stdout.String())
		}
		if c.said != "" && !strings.Contains(stderr.String(), c.said) {
			t.Errorf("%s: no %q in:\n%s", c.name, c.said, stderr.String())
		}
		for _, a := range g.asked {
			if c.name == "off" && (strings.HasPrefix(a, "/api/annotations") || strings.HasPrefix(a, "/api/frontend")) {
				t.Errorf("off, and asked %s", a)
			}
		}
	}
	// Only with --grafana.
	var errs bytes.Buffer
	if code := run([]string{"verdict", "--rules", write(t, "r.yaml", judgedRules), "--prometheus", "http://p", "--incidents", incidents, "--grafana-history=false"}, io.Discard, &errs); code != 2 ||
		!strings.Contains(errs.String(), "--grafana-history judges the rules --grafana reads") {
		t.Errorf("exit %d: %s", code, errs.String())
	}
}
