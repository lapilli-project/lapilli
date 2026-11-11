package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
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

// scrapedHistory records what it is asked, and serves Prometheus's
// configuration when config is set.
type scrapedHistory struct {
	mu    *sync.Mutex
	asked *[]string
}

func (h scrapedHistory) QueryRange(_ context.Context, expr string, _, _ time.Time, _ time.Duration) ([]prom.Series, error) {
	h.mu.Lock()
	*h.asked = append(*h.asked, expr)
	h.mu.Unlock()
	return nil, nil
}

type configuredScrapes struct{ scrapedHistory }

func (configuredScrapes) StatusConfig(context.Context) (string, error) {
	return "global:\n  scrape_interval: 1m\nscrape_configs:\n  - job_name: fid\n    scrape_interval: 15s\n  - job_name: slow\n", nil
}

// maxRule is a Grafana rule whose classic condition is the max of a
// minute of fv, scraped every 15s by the job fid.
const maxRule = `apiVersion: 1
groups:
  - orgId: 1
    name: g
    folder: f
    interval: 1m
    rules:
      - uid: cmax
        title: C max
        condition: C
        data:
          - {refId: A, datasourceUid: prom, relativeTimeRange: {from: 60, to: 0}, model: {expr: 'fv{job="fid"}', intervalMs: 1000, maxDataPoints: 43200, range: true}}
          - refId: C
            datasourceUid: __expr__
            model: {type: classic_conditions, conditions: [{evaluator: {type: gt, params: [50]}, operator: {type: and}, query: {params: [A]}, reducer: {type: max}}]}
`

// F2: a Grafana reduce the server works out is asked at the interval the
// series it reads are scraped at, as Prometheus's configuration says when
// --scrape-interval doesn't; where neither does, at Grafana's resolution,
// not the server's own, which steps over a spike Grafana's points see.
func TestReplayScrapeIntervals(t *testing.T) {
	now = func() time.Time { return t0 }
	rulesFile := write(t, "rules.yaml", maxRule)
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-1, title: Spike, start: 2026-11-01T22:30:00Z, end: 2026-11-01T22:40:00Z}\n")
	for _, c := range []struct {
		configured bool
		args       []string
		want       string
	}{
		{true, nil, `max_over_time(((fv{job="fid"}) >= -Inf)[1m7s500ms:15s])`},
		{true, []string{"--scrape-interval", "30s"}, `max_over_time(((fv{job="fid"}) >= -Inf)[1m15s:30s])`},
		{false, nil, `max_over_time(((fv{job="fid"}) >= -Inf)[1m500ms:1s])`},
	} {
		var mu sync.Mutex
		var asked []string
		h := scrapedHistory{&mu, &asked}
		newHistory = func(context.Context, *verdictFlags) (history, error) {
			if c.configured {
				return configuredScrapes{h}, nil
			}
			return h, nil
		}
		for _, mode := range [][]string{{"--incidents", incidents}, {"--check-replay"}} {
			asked = nil
			var stdout, stderr bytes.Buffer
			args := append(append([]string{"verdict", "--rules", rulesFile, "--prometheus", "http://p", "--since", "1h"}, c.args...), mode...)
			run(args, &stdout, &stderr)
			if !slices.Contains(asked, c.want) {
				t.Errorf("%v %v: asked %q, want %q\n%s", c.args, mode, asked, c.want, stderr.String())
			}
		}
	}
}
