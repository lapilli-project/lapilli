package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/signin/idptest"
)

// updatePreflightOff writes testdata/preflightoff afresh. It was run once,
// on main as it was before lapilli preflight read the backtest lane
// (cd32f629), so that lapilli preflight without --backtest-listen is held
// to the release before it, byte for byte: off, nothing of the lane
// changes what it says (docs/design/preflight.md, "As built (step 3)").
var updatePreflightOff = flag.Bool("update-preflight-off", false, "write testdata/preflightoff afresh")

// preflightOffNormal is a report with what one run of the test gives
// otherwise than the next stood in for: the directory the files are in,
// how long the history took to answer, and the free space beside the
// ledger. The text wraps its lines by their words' lengths, so the item
// that says how long is put back on one line first (preflightOffReach).
var (
	preflightOffReach = regexp.MustCompile(`(?m)^(ok +history\.reach .*)\n {29}([^ wf].*|w[^h].*|f[^i].*)$`)
	preflightOffTook  = regexp.MustCompile(`answered vector\(1\) in (under a millisecond|[0-9.]+[µnm]?s)`)
	preflightOffFree  = regexp.MustCompile(`[0-9.]+ (GB|MB|kB|bytes) free`)
)

func preflightOffNormal(s, dir string) string {
	s = strings.ReplaceAll(s, dir, "<dir>")
	s = preflightOffReach.ReplaceAllString(s, "$1 $2")
	s = preflightOffTook.ReplaceAllString(s, "answered vector(1) in <took>")
	return preflightOffFree.ReplaceAllString(s, "<free> free")
}

// Without --backtest-listen, lapilli preflight says what the release
// before said, to the byte: offline with many of serve's flags, and
// against a stand-in Prometheus and identity provider beside a ledger of
// this release's schema, as text and as JSON.
func TestPreflightOffGolden(t *testing.T) {
	alone(t)
	was := now
	now = func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = was })
	// The files are named from the directory they are in, so that the
	// report, which wraps its lines, wraps them alike wherever the test
	// runs.
	dir := t.TempDir()
	golden, err := filepath.Abs(filepath.Join("testdata", "preflightoff"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	put := func(name, body string) string {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return name
	}
	var rules strings.Builder
	rules.WriteString("groups:\n  - name: shop\n    interval: 1m\n    rules:\n")
	for i := range 12 {
		fmt.Fprintf(&rules, "      - alert: Errors%d\n        expr: rate(errors_total{job=\"svc%d\"}[5m]) > 1\n        for: 5m\n", i, i)
	}
	rules.WriteString("      - alert: Slow\n        expr: avg_over_time(latency_seconds{job=\"svc\"}[1h]) > 2\n        for: 1d\n")
	rules.WriteString("      - record: job:errors:rate5m\n        expr: sum by (job) (rate(errors_total[5m]))\n")
	rulesFile := put("rules.yaml", rules.String())
	incidents := put("incidents.yaml", "incidents: []\n")
	incidentsToken := put("incidents-token", "incidents-token-0123456789")
	firingsToken := put("firings-token", "firings-token-0123456789")
	common := []string{"--rules", rulesFile, "--max-rate", "0.5", "--catch-up-hours", "01:00-06:00", "--catch-up-rate", "0.5",
		"--impact", `sum(rate(checkout_errors_total[5m])) > 1`, "--guard", `rate(prometheus_rule_evaluation_failures_total[5m]) > 0`,
		"--incidents", incidents, "--incidents-listen", "127.0.0.1:9091", "--incidents-token-file", incidentsToken,
		"--firings-token-file", firingsToken, "--max-samples", "100000", "--parallel", "2", "--check-content=false"}

	prometheus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		form := r.PostForm
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/query_range" && form.Get("query") == "vector(1)":
			matrix(w, fmt.Sprintf(`{"metric":{},"values":[[%s,"1"]]}`, form.Get("start")))
		case r.Method == "POST" && r.URL.Path == "/api/v1/query_range" && strings.Contains(form.Get("query"), `count_over_time(ALERTS{alertstate="firing"}[1d])`):
			matrix(w, fmt.Sprintf(`{"metric":{"alertname":"Errors3"},"values":[[%s,"120"]]}`, form.Get("start")))
		default:
			t.Errorf("Prometheus was sent %s %s %v", r.Method, r.URL.Path, url.Values(form))
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(prometheus.Close)
	idp := idptest.New(t, "lapilli", "", time.Now)
	ledgerDir := "ledger"
	os.MkdirAll(ledgerDir, 0o750)
	l, err := ledger.Open(context.Background(), filepath.Join(ledgerDir, "ledger.db"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()

	cases := []struct {
		name string
		args []string
	}{
		{"offline", append([]string{"--offline", "--prometheus", "https://prometheus.example.com", "--loki", "https://loki.example.com",
			"--oidc-issuer", "https://idp.example.com", "--oidc-client-id", "lapilli",
			"--external-url", "https://lapilli.example.com", "--admins", "ops@example.com", "--ledger", ledgerDir}, common...)},
		{"offline-changes", append([]string{"--offline", "--prometheus", "https://prometheus.example.com",
			"--alarms", "cw=https://monitoring.eu-west-1.amazonaws.com", "--changes", "--changes-aws-role", "arn:aws:iam::123456789012:role/lapilli-rules",
			"--changes-in", "cw:Queue*", "--user-header", "X-Email", "--admins", "ops@example.com", "--ledger", ledgerDir}, common...)},
		{"online", append([]string{"--prometheus", prometheus.URL, "--oidc-issuer", idp.URL, "--oidc-client-id", "lapilli",
			"--external-url", "https://lapilli.example.com", "--admins", "ops@example.com", "--ledger", ledgerDir}, common...)},
	}
	for _, c := range cases {
		for _, form := range []string{"text", "json"} {
			args := append([]string{"preflight"}, c.args...)
			if form == "json" {
				args = append(args, "--json")
			}
			var out, errOut bytes.Buffer
			code := run(args, &out, &errOut)
			got := fmt.Sprintf("exit %d\n%s", code, preflightOffNormal(out.String(), dir))
			if errOut.Len() > 0 {
				got += "stderr:\n" + preflightOffNormal(errOut.String(), dir)
			}
			file := filepath.Join(golden, c.name+"."+form)
			if *updatePreflightOff {
				os.MkdirAll(golden, 0o750)
				if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("%s: lapilli preflight without --backtest-listen says otherwise than the release before:\n--- got\n%s\n--- want\n%s", file, got, want)
			}
		}
	}
}
