package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// rateRule is a Grafana rule whose query puts in its data source's
// $__rate_interval.
const rateRule = `apiVersion: 1
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
`

// F1: with --grafana, each data source whose own scrape interval a rule's
// query puts in is read from Grafana, with the token, and the query put
// in as Grafana does: 30s makes $__rate_interval 2m. One that can't be
// read is said, and taken as --scrape-interval says, or as Grafana's 15s.
func TestGrafanaDataSourceScrape(t *testing.T) {
	refuse := false
	var asked int
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/provisioning/alert-rules/export":
			io.WriteString(w, rateRule)
		case "/api/datasources/uid/prom30":
			asked++
			auth = r.Header.Get("Authorization")
			if refuse {
				http.Error(w, `{"message": "Access denied"}`, http.StatusForbidden)
				return
			}
			io.WriteString(w, `{"uid": "prom30", "type": "prometheus", "jsonData": {"prometheusType": "Prometheus", "timeInterval": "30s"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	token := write(t, "token", "glsa_secret\n")
	for _, c := range []struct {
		refuse bool
		scrape time.Duration
		want   string
	}{
		{false, 0, "vector(120000)"},
		{false, time.Minute, "vector(120000)"},
		{true, 0, "vector(60000)"},
		{true, 20 * time.Second, "vector(80000)"},
	} {
		refuse = c.refuse
		f := &verdictFlags{grafana: srv.URL, grafanaTk: token, scrapeInterval: c.scrape}
		var stderr bytes.Buffer
		rs, err := readRules(context.Background(), f, nil, &stderr)
		if err != nil || len(rs) != 1 {
			t.Fatalf("%v %+v", err, rs)
		}
		if q := rs[0].Graph.Queries["A"]; q != c.want {
			t.Errorf("%+v: %s, want %s", c, q, c.want)
		}
		if said := strings.Contains(stderr.String(), "prom30 (HTTP 403) weren't read"); said != c.refuse {
			t.Errorf("%+v: %q", c, stderr.String())
		}
	}
	if asked != 4 || auth != "Bearer glsa_secret" {
		t.Errorf("asked %d times, with %q", asked, auth)
	}
}
