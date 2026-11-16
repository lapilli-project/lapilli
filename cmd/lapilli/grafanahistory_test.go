package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// errorsHistory answers a Grafana rule's query of errors at 10 from 3h to
// 2h before t0; GRAFANA_ALERTS, which the Grafana doesn't write, it keeps
// empty.
type errorsHistory struct{}

func (errorsHistory) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if !strings.Contains(expr, "errors") || strings.Contains(expr, "GRAFANA_ALERTS") {
		return nil, nil
	}
	s := prom.Series{Labels: map[string]string{"service": "checkout"}}
	for t := t0.Add(-3 * time.Hour); !t.After(t0.Add(-2 * time.Hour)); t = t.Add(step) {
		if !t.Before(start) && !t.After(end) {
			s.Samples = append(s.Samples, prom.Sample{T: t, V: 10})
		}
	}
	return []prom.Series{s}, nil
}

const gErrorsRule = `apiVersion: 1
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
`

// verdict --check-replay reads what Grafana's rules in files really fired
// from the Grafana --check-grafana names, its alert state history kept in
// annotations, with the token --grafana-token-file holds: from a day
// before the period, so one on since before it is seen; its error alerts
// left out and said.
func TestCheckReplayGrafanaHistory(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return errorsHistory{}, nil }
	now = func() time.Time { return t0 }
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		switch r.URL.Path {
		case "/api/frontend/settings":
			io.WriteString(w, `{"unifiedAlerting":{"alertStateHistoryBackend":"multiple","alertStateHistoryPrimary":"annotations"}}`)
		case "/api/annotations":
			note := func(id int, rule, from, to string, at time.Time) map[string]any {
				return map[string]any{"id": id, "alertName": rule, "prevState": from, "newState": to, "time": at.UnixMilli(),
					"tags": []string{"alertname:" + rule, "grafana_folder:Ops", "service:checkout"}}
			}
			json.NewEncoder(w).Encode([]any{
				note(6, "GErrors", "Error", "Normal", t0.Add(-50*time.Minute)),
				note(5, "GErrors", "Normal", "Error", t0.Add(-time.Hour)),
				note(4, "GErrors", "Alerting", "Normal", t0.Add(-2*time.Hour+time.Minute)),
				note(3, "GErrors", "Normal", "Alerting", t0.Add(-3*time.Hour)),
				note(2, "Old", "Alerting", "Normal", t0.Add(-7*24*time.Hour+time.Hour)),
				// In the Error state from before the period, all through it.
				note(7, "Stuck", "Normal", "Error", t0.Add(-7*24*time.Hour-time.Hour)),
				note(1, "Old", "Normal", "Alerting", t0.Add(-7*24*time.Hour-2*time.Hour)),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	rulesFile := write(t, "grafana.yaml", gErrorsRule+strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(gErrorsRule[strings.Index(gErrorsRule, "      - uid:"):], "gerrors", "stuck"), "GErrors", "Stuck"), "expr: errors", "expr: stuck"))
	token := write(t, "token", "glsa_viewer\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"verdict", "--rules", rulesFile, "--prometheus", "http://x", "--check-replay", "--check-grafana", strings.Replace(srv.URL, "http://", "HTTP://", 1),
		"--grafana-token-file", token, "--tz", "UTC"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Replay beside Grafana's alert state history (annotations)", "1 firings alike, 0 only in the replay, 0 only in what really fired: 100% alike.",
		"Grafana's error alerts of GErrors (1), Stuck (1)", "Really fired, but no rule given has the name: Old."} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "GRAFANA_ALERTS") {
		t.Errorf("GRAFANA_ALERTS read:\n%s", out)
	}
	// Whether it reaches back past the period; then a week a request at
	// most: two.
	from := fmt.Sprint(t0.Add(-8 * 24 * time.Hour).UnixMilli())
	if len(asked) != 4 || !strings.Contains(asked[1], "from=1&") || !strings.Contains(asked[2], "from="+from) || !strings.HasSuffix(asked[2], " Bearer glsa_viewer") {
		t.Errorf("asked %v", asked)
	}
	if !strings.Contains(stderr.String(), "--check-grafana is plain http") {
		t.Errorf("plain http not said: %s", stderr.String())
	}
}

func TestCheckGrafanaFlags(t *testing.T) {
	rulesFile := write(t, "grafana.yaml", gErrorsRule)
	token := write(t, "token", "glsa_viewer")
	base := []string{"--rules", rulesFile, "--prometheus", "http://x"}
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"verdict", "--check-grafana", "http://g", "--incidents", "x"}, "--check-grafana goes with --check-replay"},
		{[]string{"verdict", "--check-replay", "--check-grafana", "g.example"}, "want Grafana's URL"},
		{[]string{"verdict", "--check-replay", "--check-grafana", "https://u:p@g.example"}, "want Grafana's URL"},
		{[]string{"verdict", "--check-replay", "--check-grafana", "http://g", "--check-series", "x"}, "give one"},
		{[]string{"verdict", "--check-content", "--check-grafana", "http://g"}, "doesn't go with --check-replay"},
		{[]string{"serve", "--check-replay=false", "--check-grafana", "http://g"}, "--check-grafana goes with --check-replay"},
		{[]string{"serve", "--check-grafana", "http://g", "--check-series", "x"}, "give one"},
		{[]string{"verdict", "--check-replay", "--grafana-token-file", token}, "--grafana-token-file goes with --grafana or --check-grafana"},
	} {
		var errs strings.Builder
		args := append(append([]string{c.args[0]}, base...), c.args[1:]...)
		if code := run(args, io.Discard, &errs); code != 2 || !strings.Contains(errs.String(), c.says) {
			t.Errorf("%v: exit %d, want 2 saying %q:\n%s", c.args, code, c.says, errs.String())
		}
	}
	for _, args := range [][]string{
		{"verdict", "--grafana", "http://g", "--check-grafana", "http://g", "--prometheus", "http://x", "--check-replay"},
		{"verdict", "--loaded-rules", "--check-grafana", "http://g", "--prometheus", "http://x", "--check-replay"},
	} {
		if code := run(args, io.Discard, io.Discard); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	// With the Grafana's token, and in serve, beside --rules.
	f, err := parseFlags(append(base, "--check-replay", "--check-grafana", "https://g.example/", "--grafana-token-file", token), nil)
	if err != nil || f.historyGrafana() != "https://g.example" || f.grafanaTk != token {
		t.Errorf("%+v %v", f, err)
	}
	if f, err := parseFlags(append(base, "--check-grafana", "https://g.example"), func(string, string) (bool, error) { return false, nil }); err != nil || f.historyGrafana() == "" {
		t.Errorf("serve: %v", err)
	}
	// --grafana's own history, unless --check-series names series.
	if f, err := parseFlags([]string{"--grafana", "https://g.example", "--prometheus", "http://x", "--check-replay"}, nil); err != nil || f.historyGrafana() != "https://g.example" {
		t.Errorf("--grafana: %v", err)
	}
	if f, err := parseFlags([]string{"--grafana", "https://g.example", "--prometheus", "http://x", "--check-replay", "--check-series", "x"}, nil); err != nil || f.historyGrafana() != "" {
		t.Errorf("--check-series: %v", err)
	}
}

// A history that can't be read leaves Grafana's rules unchecked, said
// why, and the others checked as before: a token that may not read
// annotations (403), a Grafana whose settings can't be read (then
// GRAFANA_ALERTS, as before), annotations before Grafana 12.4, which hold
// no labels, or a history that holds nothing from before the period.
func TestCheckReplayGrafanaHistoryFails(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	prometheus := write(t, "rules.yaml", "groups:\n  - name: shop\n    rules:\n      - {alert: CheckoutErrors, expr: errors > 5, for: 5m}\n")
	grafana := write(t, "grafana.yaml", gErrorsRule)
	token := write(t, "token", "glsa_viewer")
	for _, c := range []struct {
		name               string
		settings, notes    int
		body, says, stderr string
	}{
		{"refused", 200, 403, "", "GErrors: not checked: Grafana's alert state history can't be used", ""},
		{"settings refused", 403, 200, "[]", "", "what Grafana's rules really fired is read from GRAFANA_ALERTS"},
		{"before 12.4", 200, 200, `[{"id":1,"alertName":"GErrors","prevState":"Normal","newState":"Alerting","time":1792900000000}]`, "hold no labels, as Grafana before 12.4 writes them", ""},
		{"from after", 200, 200, "[]", "holds no change of state from before the period", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/frontend/settings":
					w.WriteHeader(c.settings)
					io.WriteString(w, `{"buildInfo":{"version":"12.4.14"},"unifiedAlerting":{"alertStateHistoryBackend":"annotations"}}`)
				default:
					w.WriteHeader(c.notes)
					io.WriteString(w, c.body)
				}
			}))
			defer srv.Close()
			var stdout, stderr bytes.Buffer
			code := run([]string{"verdict", "--rules", prometheus, "--rules", grafana, "--prometheus", "http://x", "--check-replay", "--check-grafana", srv.URL,
				"--grafana-token-file", token, "--tz", "UTC"}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit %d: %s\n%s", code, stdout.String(), stderr.String())
			}
			out := stdout.String()
			if !strings.Contains(out, "1 firings alike, 0 only in the replay, 0 only in what really fired") {
				t.Errorf("the Prometheus rule isn't checked as before:\n%s", out)
			}
			if c.says != "" && !strings.Contains(out, c.says) {
				t.Errorf("no %q in:\n%s", c.says, out)
			}
			if c.stderr != "" && !strings.Contains(stderr.String(), c.stderr) {
				t.Errorf("no %q in:\n%s", c.stderr, stderr.String())
			}
		})
	}
}

// Read from annotations, the replay's labels are set beside them as their
// tags keep them: an instance's port with an underscore; a rule's title
// with a colon, by its name there.
func TestCheckReplayGrafanaHistoryTagged(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return portHistory{}, nil }
	now = func() time.Time { return t0 }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/frontend/settings":
			io.WriteString(w, `{"buildInfo":{"version":"12.4.14"},"unifiedAlerting":{"alertStateHistoryBackend":"annotations"}}`)
		default:
			note := func(id int, inst, from, to string, at time.Time) map[string]any {
				return map[string]any{"id": id, "alertName": "Up: shop", "prevState": from, "newState": to, "time": at.UnixMilli(),
					"tags": []string{"alertname:Up_ shop", "instance:" + inst}}
			}
			json.NewEncoder(w).Encode([]any{
				note(4, "10.0.0.2_9100", "Alerting", "Normal", t0.Add(-2*time.Hour+time.Minute)),
				note(3, "10.0.0.1_9100", "Alerting", "Normal", t0.Add(-2*time.Hour+time.Minute)),
				note(2, "10.0.0.2_9100", "Normal", "Alerting", t0.Add(-3*time.Hour)),
				note(1, "10.0.0.1_9100", "Normal", "Alerting", t0.Add(-3*time.Hour)),
			})
		}
	}))
	defer srv.Close()
	rule := strings.NewReplacer("uid: gerrors", "uid: up", "title: GErrors", `title: "Up: shop"`).Replace(gErrorsRule)
	var stdout, stderr bytes.Buffer
	code := run([]string{"verdict", "--rules", write(t, "grafana.yaml", rule), "--prometheus", "http://x", "--check-replay", "--check-grafana", srv.URL, "--tz", "UTC"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "2 firings alike, 0 only in the replay, 0 only in what really fired: 100% alike.") {
		t.Errorf("exit %d:\n%s\n%s", code, stdout.String(), stderr.String())
	}
}

// portHistory is errorsHistory with two instances, each with its port.
type portHistory struct{}

func (portHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	ss, err := errorsHistory{}.QueryRange(ctx, expr, start, end, step)
	if len(ss) == 0 {
		return ss, err
	}
	a, b := ss[0], prom.Series{Labels: map[string]string{"instance": "10.0.0.2:9100"}, Samples: ss[0].Samples}
	a.Labels = map[string]string{"instance": "10.0.0.1:9100"}
	return []prom.Series{a, b}, err
}
