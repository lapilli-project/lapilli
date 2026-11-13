package main

// What a hostile or compromised Prometheus-compatible API, or a data source
// behind a Grafana, can make lapilli verdict write or wait for (0.34.1).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/rules"
)

// An answer of 200 lapilli can't read (a result type that isn't a matrix,
// or a sample that isn't a time and a number) is quoted in the error as
// the server wrote it; and, as prom.Refused takes it for the API refusing
// the query as written, --markdown keeps that reason in the pull request's
// comment, which more people read than the job's log. A server that echoes
// the bearer token there is quoted without it, as in every other error of
// the API's.
func TestTokenNotInPRComment(t *testing.T) {
	const token = "glsa_ci-reader-0123456789abcdef"
	was, wasNow := newHistory, now
	t.Cleanup(func() { newHistory, now = was, wasNow })
	newHistory = realHistory
	now = func() time.Time { return t0 }
	for _, c := range []struct{ name, body string }{
		{"result type", `{"status":"success","data":{"resultType":"%s","result":[]}}`},
		{"sample", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1,"%s"]]}]}}`},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, c.body, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}))
		tok := write(t, "token", token+"\n")
		rs := write(t, "rules.yaml", "groups:\n  - name: shop\n    rules:\n      - alert: A\n        expr: up > 0\n        for: 5m\n      - alert: B\n        expr: up > 1\n        for: 5m\n")
		base := write(t, "base.yaml", "groups:\n  - name: shop\n    rules:\n      - alert: A\n        expr: up > 0\n        for: 5m\n")
		var stdout, stderr bytes.Buffer
		code := run([]string{"verdict", "--prometheus", srv.URL, "--bearer-token-file", tok, "--rules", rs, "--compare", base,
			"--changed-only", "--markdown", "--since", "2h", "--tz", "UTC", "--progress=false", "--fail-unreplayed"}, &stdout, &stderr)
		srv.Close()
		if strings.Contains(stdout.String(), token) {
			t.Errorf("%s: the bearer token is in the pull request's comment (exit %d): %s", c.name, code, line(stdout.String(), token))
		}
		if !strings.Contains(stdout.String(), `[redacted]`) {
			t.Errorf("%s: the comment doesn't say what the API answered: %s", c.name, stdout.String())
		}
	}
}

// line is the line of s holding sub, or "".
func line(s, sub string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

// The rules a Prometheus says it has loaded (--loaded-rules) are named as
// it names them, and --metrics writes those names as label values: an ESC
// from the API would reach stdout as it is, an OSC 52 sequence setting the
// terminal's clipboard, or one retitling or clearing it. A rule is read
// with what doesn't show as U+FFFD (rules.Shown), and --metrics writes
// what doesn't show as \u{1b} besides (verdict.LabelValue), as every other
// output does.
func TestMetricsEscapes(t *testing.T) {
	was, wasNow := newHistory, now
	t.Cleanup(func() { newHistory, now = was, wasNow })
	newHistory = realHistory
	now = func() time.Time { return t0 }
	const name = "Up\x1b]52;c;Y3VybCBldmlsLmV4YW1wbGUgfCBzaA==\x07\x1b[2J"
	quoted, _ := json.Marshal(name)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/v1/rules") {
			fmt.Fprintf(w, `{"status":"success","data":{"groups":[{"name":"g","interval":60,"rules":[{"type":"alerting","name":%s,"query":"up > 0","duration":0,"labels":{},"annotations":{}}]}]}}`, quoted)
			return
		}
		r.ParseForm()
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer srv.Close()
	inc := write(t, "incidents.yaml", "incidents:\n  - {id: INC-7, title: down, start: \"2026-11-01T21:00:00Z\", end: \"2026-11-01T22:00:00Z\"}\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"verdict", "--prometheus", srv.URL, "--loaded-rules", "--incidents", inc, "--metrics",
		"--since", "2h", "--tz", "UTC", "--progress=false"}, &stdout, &stderr)
	t.Logf("exit %d, stderr %q", code, stderr.String())
	if i := strings.IndexByte(stdout.String(), 0x1b); i >= 0 {
		t.Errorf("--metrics wrote the API's ESC to stdout: %q", stdout.String()[max(i-30, 0):min(i+60, stdout.Len())])
	}
	if want := "lapilli_rule_firings{rule=\"Up\uFFFD]52;c;Y3VybCBldmlsLmV4YW1wbGUgfCBzaA==\uFFFD\uFFFD[2J\"} 0"; !strings.Contains(stdout.String(), want) {
		t.Errorf("--metrics doesn't name the rule as every other output does, %s:\n%s", want, stdout.String())
	}
}

// The data sources behind one Grafana share --parallel places
// (prom.NewCap), held for as long as a request is in flight. A data source
// that answers slowly, as a hostile or broken Loki behind the Grafana can,
// would hold every place, and a healthy data source's queries wait for its
// answers: the last place is kept for another (prom's fair).
func TestSlowDataSourceDoesntStarveTheRest(t *testing.T) {
	const hold = 2 * time.Second
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/uid/slow/") {
			select {
			case <-time.After(hold):
			case <-r.Context().Done():
			}
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer srv.Close()
	f := &verdictFlags{grafana: srv.URL, parallel: 4}
	l := f.logs(nil)
	slow, err := l.For(rules.Rule{Logs: "slow", Graph: &rules.Graph{}})
	if err != nil {
		t.Fatal(err)
	}
	fast, err := l.For(rules.Rule{Logs: "fast", Graph: &rules.Graph{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const q = `sum(count_over_time({app="a"}[1m]))`
	var wg sync.WaitGroup
	// --parallel rules of the slow data source, each asking again as a
	// replay's rule asks day after day.
	for range f.parallel {
		wg.Go(func() {
			for range 3 {
				slow.QueryRange(ctx, q, t0.Add(-time.Hour), t0, time.Minute)
			}
		})
	}
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if _, err := fast.QueryRange(ctx, q, t0.Add(-time.Hour), t0, time.Minute); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	cancel()
	wg.Wait()
	t.Logf("a healthy data source's one query, behind a data source holding each answer %s: %s", hold, took.Round(time.Millisecond))
	if took > hold/2 {
		t.Errorf("the healthy data source waited %s for the slow one's places", took.Round(time.Millisecond))
	}
}
