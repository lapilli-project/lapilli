package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/serve"
	"lapilli.dev/internal/verdict"
)

// TestUnreplayedShown: a rule whose replay failed as the history couldn't
// be read is named at the top of the report, not only at its foot; --json
// lists it with why, and one whose query was refused as written as not
// failed, as another run is refused too; the exit stays 0 unless
// --fail-unreplayed, which counts only the first.
func TestUnreplayedShown(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	rs := write(t, "rules.yaml", `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: errors > 5
        for: 5m
      - alert: Typo
        expr: broken(
      - alert: Turned
        expr: turned > 1
      - alert: Quiet
        expr: never
`)
	later := write(t, "later.yaml", `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: errors > 5 or turned
        for: 5m
      - alert: Typo
        expr: broken(
      - alert: Turned
        expr: turned > 1
      - alert: Quiet
        expr: never
`)
	refused := write(t, "refused.yaml", `groups:
  - name: shop
    rules:
      - alert: Typo
        expr: broken(
      - alert: Quiet
        expr: never
`)
	alarms := write(t, "alarms.json", `{"MetricAlarms": [{"AlarmName": "CheckoutErrors", "EvaluationInterval": 60,
  "EvaluationCriteria": {"PromQLCriteria": {"Query": "errors > 5", "PendingPeriod": 300, "RecoveryPeriod": 600}}},
  {"AlarmName": "cpu-high", "Namespace": "AWS/EC2", "MetricName": "CPUUtilization", "Threshold": 90}]}`)
	incidents := write(t, "incidents.yaml", `incidents:
  - {id: INC-7, title: Checkout down, start: "2026-11-01T21:00:00Z", end: "2026-11-01T22:00:00Z"}
`)
	verdictOf := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"verdict", "--prometheus", "http://p", "--incidents", incidents, "--tz", "UTC"}, args...), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	const turned = rules.HistoryUnread + "HTTP 429: slow down (asked 7 times over 3m2s)"

	code, out, _ := verdictOf("--rules", rs)
	top := "! 1 of 4 rules couldn't be replayed, and are left out of what follows:\n  Turned: " + turned + "\n"
	if code != 0 || !strings.Contains(out, top) || strings.Index(out, top) > strings.Index(out, "RULE") || strings.Contains(out[:strings.Index(out, "RULE")], "Typo") {
		t.Errorf("exit %d: want the rule named before the table:\n%s", code, out)
	}
	if !strings.Contains(out, "Could not replay 2 rules:\n  Turned: "+turned+"\n  Typo: query failed: HTTP 400: bad_data: parse error\n") {
		t.Errorf("the list at the foot is gone:\n%s", out)
	}

	code, out, _ = verdictOf("--rules", rs, "--json")
	var rep verdict.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 {
		t.Fatalf("exit %d: %v", code, err)
	}
	if len(rep.Unreplayed) != 2 || rep.Unreplayed[0] != (verdict.Unreplayed{Rule: "Turned", Reason: turned, Failed: true}) ||
		rep.Unreplayed[1] != (verdict.Unreplayed{Rule: "Typo", Reason: "query failed: HTTP 400: bad_data: parse error"}) {
		t.Errorf("unreplayed %+v", rep.Unreplayed)
	}

	// A metric alarm can't be replayed by any run: listed, but not as failed,
	// and not at the top.
	code, out, _ = verdictOf("--rules", alarms, "--json")
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 {
		t.Fatalf("exit %d: %v", code, err)
	}
	if len(rep.Unreplayed) != 1 || rep.Unreplayed[0].Rule != "cpu-high" || rep.Unreplayed[0].Failed {
		t.Errorf("unreplayed %+v", rep.Unreplayed)
	}
	for _, only := range []string{alarms, refused} {
		if code, out, _ = verdictOf("--rules", only, "--fail-unreplayed"); code != 0 || strings.Contains(out, "! ") {
			t.Errorf("exit %d with only rules no run replays:\n%s", code, out)
		}
	}

	code, _, errs := verdictOf("--rules", rs, "--fail-unreplayed")
	if code != 1 || !strings.Contains(errs, "1 of 4 rules couldn't be replayed, as the history couldn't be read (--fail-unreplayed)") {
		t.Errorf("--fail-unreplayed: exit %d\n%s", code, errs)
	}

	// Compared: a rule alike in both once; a changed rule by its side.
	code, out, _ = verdictOf("--rules", later, "--compare", rs)
	want := "! 2 rules couldn't be replayed, and are left out of what follows:\n  CheckoutErrors (after): " + turned + "\n  Turned: " + turned + "\n"
	if code != 0 || !strings.Contains(out, want) {
		t.Errorf("exit %d, want\n%s\nin\n%s", code, want, out)
	}
	code, out, _ = verdictOf("--rules", later, "--compare", rs, "--json")
	var cmp verdict.Comparison
	if err := json.Unmarshal([]byte(out), &cmp); err != nil || code != 0 {
		t.Fatalf("exit %d: %v", code, err)
	}
	if len(cmp.Unreplayed) != 3 || cmp.Unreplayed[0].Side != "after" || cmp.Unreplayed[1] != (verdict.Unreplayed{Rule: "Turned", Reason: turned, Failed: true}) ||
		cmp.Unreplayed[2].Rule != "Typo" || cmp.Unreplayed[2].Failed {
		t.Errorf("unreplayed %+v", cmp.Unreplayed)
	}
	if code, _, _ = verdictOf("--rules", later, "--compare", rs, "--fail-unreplayed"); code != 1 {
		t.Errorf("--compare --fail-unreplayed: exit %d", code)
	}
}

// TestProgress: a run past progressAfter says on stderr how it is going,
// and nothing of it on stdout; --progress=false says nothing; a 429 from
// the history is said once at the end.
func TestProgress(t *testing.T) {
	after, every := progressAfter, progressEvery
	progressAfter, progressEvery = 50*time.Millisecond, 50*time.Millisecond
	defer func() { progressAfter, progressEvery = after, every }()
	now = func() time.Time { return t0 }
	var asked atomic.Int64
	var quiet atomic.Bool // answers no 429
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		if asked.Add(1) == 1 && !quiet.Load() {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		time.Sleep(30 * time.Millisecond)
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer srv.Close()
	newHistory = func(context.Context, *verdictFlags) (history, error) { return prom.New(srv.URL, "test"), nil }
	rs := write(t, "rules.yaml", `groups:
  - name: shop
    rules:
      - {alert: A, expr: a > 1}
      - {alert: B, expr: b > 1}
      - {alert: C, expr: c > 1}
`)
	incidents := write(t, "incidents.yaml", `incidents:
  - {id: INC-7, title: Checkout down, start: "2026-11-01T21:00:00Z"}
`)
	for _, quiet := range []bool{false, true} {
		asked.Store(0)
		args := []string{"verdict", "--rules", rs, "--prometheus", srv.URL, "--incidents", incidents, "--since", "2d", "--parallel", "1", "--json"}
		if quiet {
			args = append(args, "--progress=false")
		}
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr.String())
		}
		said := strings.Contains(stderr.String(), "lapilli verdict: after ") && strings.Contains(stderr.String(), " of 3 rules replayed, ") &&
			strings.Contains(stderr.String(), " requests to the history, 1 asked again after a wait, 1 answered HTTP 429")
		if said == quiet {
			t.Errorf("--progress=false %v: stderr\n%s", quiet, stderr.String())
		}
		if !strings.Contains(stderr.String(), "the history answered 1 requests with HTTP 429") {
			t.Errorf("the 429 isn't said:\n%s", stderr.String())
		}
		if strings.Contains(stdout.String(), "lapilli verdict:") || !json.Valid(stdout.Bytes()) {
			t.Errorf("stdout isn't the JSON alone:\n%s", stdout.String())
		}
	}
	// No 429, nothing said of one.
	quiet.Store(true)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verdict", "--rules", rs, "--prometheus", srv.URL, "--incidents", incidents, "--since", "2d", "--progress=false"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Errorf("exit %d, stderr\n%s", code, stderr.String())
	}
}

func TestReadFlags(t *testing.T) {
	asServe := func(name, val string) (bool, error) { return false, nil }
	f, err := parseFlags([]string{"--rules", "r", "--prometheus", "http://p", "--max-rate", "0.5", "--loki", "http://l", "--loki-rules", "x"}, asServe)
	if err != nil || f.prometheus().MaxRate != 0.5 || f.lokiSource().MaxRate != 0.5 {
		t.Errorf("serve --max-rate: %v %+v", err, f)
	}
	for _, c := range []struct{ args, err string }{
		{"--max-rate 0", "want requests a second"},
		{"--max-rate -1", "want requests a second"},
		{"--max-rate NaN", "want requests a second"},
		{"--max-rate Inf", "want requests a second"},
		{"--max-rate 20000", "want requests a second"},
		{"--max-rate 1e-10", "want requests a second, from 0.001"},
		{"--max-rate 0.0009", "want requests a second, from 0.001"},
		{"--max-rate many", "want requests a second"},
		{"--progress=maybe", "want true or false"},
	} {
		if _, err := parseVerdictFlags(append([]string{"--rules", "r", "--prometheus", "http://p", "--incidents", "i"}, strings.Fields(c.args)...)); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: %v", c.args, err)
		}
	}
	for _, flag := range []string{"--progress=false", "--fail-unreplayed"} {
		if _, err := parseFlags([]string{"--rules", "r", "--prometheus", "http://p", flag}, asServe); err == nil || !strings.Contains(err.Error(), "lapilli serve doesn't take") {
			t.Errorf("serve %s: %v", flag, err)
		}
	}
	f, err = parseVerdictFlags([]string{"--rules", "r", "--prometheus", "http://p", "--incidents", "i", "--progress=false", "--fail-unreplayed", "--max-rate", "0.001"})
	if err != nil || !f.noProgress || !f.failUnreplayed || f.maxRate != 0.001 {
		t.Errorf("%v %+v", err, f)
	}
}

// TestGrafanaDataSourcesApart: each Loki data source behind --grafana is
// asked by a Limiter of its own, as Grafana passes on what each answers, so
// one that pushes back doesn't hold another; --max-rate is of them all.
func TestGrafanaDataSourcesApart(t *testing.T) {
	var noisy atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/datasources/proxy/uid/noisy/") {
			noisy.Add(1)
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many outstanding requests", http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer srv.Close()
	const q = `sum(count_over_time({app="a"}[1m]))`
	asked := func(f *verdictFlags) func(uid string, days int, wait time.Duration) error {
		l := f.logs(nil)
		return func(uid string, days int, wait time.Duration) error {
			h, err := l.For(rules.Rule{Logs: uid, Graph: &rules.Graph{}})
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			_, err = h.QueryRange(ctx, q, t0.Add(-time.Duration(days)*24*time.Hour), t0.Add(-time.Minute), time.Minute)
			return err
		}
	}
	ask := asked(&verdictFlags{grafana: srv.URL})
	if err := ask("noisy", 1, 30*time.Second); err == nil || noisy.Load() != 1 {
		t.Fatalf("noisy: %v after %d requests", err, noisy.Load())
	}
	start := time.Now()
	if err := ask("quiet", 6, 30*time.Second); err != nil || time.Since(start) > 10*time.Second {
		t.Errorf("quiet, beside noisy told to wait a minute: %v after %s", err, time.Since(start))
	}
	if err := ask("noisy", 1, 30*time.Second); !errors.Is(err, prom.ErrPastDeadline) || noisy.Load() != 1 {
		t.Errorf("noisy, told to wait a minute: %v after %d requests", err, noisy.Load())
	}

	// --max-rate 10: six requests to two data sources, together, a tenth of
	// a second apart.
	ask = asked(&verdictFlags{grafana: srv.URL, maxRate: 10})
	start = time.Now()
	var wg sync.WaitGroup
	for _, uid := range []string{"a", "b"} {
		wg.Go(func() {
			if err := ask(uid, 3, 30*time.Second); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if took := time.Since(start); took < 450*time.Millisecond {
		t.Errorf("6 requests at 10 a second, to two data sources, in %s", took)
	}
}

// TestProgressCounts: the rules a run replays are counted, each once, when
// replayed; what asks the history after (near misses, suggestions) isn't.
func TestProgressCounts(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	var total, done int64
	watched = func(p *replay.Progress, _ *prom.Counts) { total, done = p.Total.Load(), p.Done.Load() }
	defer func() { watched = func(*replay.Progress, *prom.Counts) {} }()
	rs := write(t, "rules.yaml", `groups:
  - name: shop
    rules:
      - alert: Flappy
        expr: flappy
      - alert: Errors
        expr: errors > 5
        for: 2h
      - alert: Quiet
        expr: never
`)
	incidents := write(t, "incidents.yaml", `incidents:
  - {id: INC-7, title: Checkout down, start: "2026-11-01T21:00:00Z", end: "2026-11-01T22:00:00Z"}
`)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"verdict", "--rules", rs, "--prometheus", "http://p", "--incidents", incidents, "--tz", "UTC", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"near_misses"`) || !strings.Contains(stdout.String(), `"suggestions"`) {
		t.Fatalf("want near misses and suggestions, which ask the history again:\n%s", stdout.String())
	}
	if total != 3 || done != 3 {
		t.Errorf("%d of %d rules replayed, want 3 of 3", done, total)
	}
}

// slowHistory is fakeHistory a little slow, so that a progress line is
// written while a run goes.
type slowHistory struct{ fakeHistory }

func (h slowHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	time.Sleep(5 * time.Millisecond)
	return h.fakeHistory.QueryRange(ctx, expr, start, end, step)
}

// TestStderrShared: a run writes on stderr while its progress line does,
// here as a report fails to go to Slack after the replay, and the two don't
// race (go test -race).
func TestStderrShared(t *testing.T) {
	after, every := progressAfter, progressEvery
	progressAfter, progressEvery = time.Millisecond, time.Millisecond
	defer func() { progressAfter, progressEvery = after, every }()
	newHistory = func(context.Context, *verdictFlags) (history, error) { return slowHistory{}, nil }
	now = func() time.Time { return t0 }
	rs := write(t, "rules.yaml", "groups:\n  - name: shop\n    rules:\n      - {alert: A, expr: errors > 5}\n      - {alert: B, expr: never}\n")
	hook := write(t, "hook", "not a webhook")
	incidents := write(t, "incidents.yaml", "incidents:\n  - {id: INC-7, title: Checkout down, start: 2026-11-01T21:00:00Z}\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"verdict", "--rules", rs, "--prometheus", "http://p", "--slack-webhook-file", hook, "--incidents", incidents, "--tz", "UTC"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "--slack-webhook-file: the file should hold the webhook's URL") ||
		!strings.Contains(stderr.String(), "lapilli verdict: after ") {
		t.Errorf("exit %d, stderr:\n%s", code, stderr.String())
	}
}

// TestProgressLine: a comparison counts replays, as a rule changed is
// replayed before and after.
func TestProgressLine(t *testing.T) {
	var p replay.Progress
	var c prom.Counts
	p.Total.Store(32)
	p.Done.Store(24)
	c.Requests.Store(81)
	if got := progressLine(&p, &c, 15*time.Second, true); got != "lapilli verdict: after 15s, 24 of 32 replays of the rules before and after done, 81 requests to the history" {
		t.Errorf("%q", got)
	}
	if got := progressLine(&p, &c, 15*time.Second, false); got != "lapilli verdict: after 15s, 24 of 32 rules replayed, 81 requests to the history" {
		t.Errorf("%q", got)
	}
}

// realHistory is newHistory as lapilli has it, which tests replace.
var realHistory = newHistory

// unreadRun is lapilli verdict on two rules against a Prometheus that
// answers every query with answer, its exit, stdout and stderr.
func unreadRun(t *testing.T, answer http.HandlerFunc, args ...string) (int, string, string) {
	t.Helper()
	newHistory = realHistory
	now = func() time.Time { return t0 }
	srv := httptest.NewServer(answer)
	defer srv.Close()
	rs := write(t, "rules.yaml", "groups:\n  - name: shop\n    rules:\n      - alert: A\n        expr: up > 0\n        for: 5m\n      - alert: B\n        expr: up > 1\n        for: 5m\n")
	inc := write(t, "incidents.yaml", "incidents:\n  - {id: INC-7, title: down, start: \"2026-11-01T21:00:00Z\", end: \"2026-11-01T22:00:00Z\"}\n")
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"verdict", "--prometheus", srv.URL, "--rules", rs, "--incidents", inc, "--tz", "UTC", "--since", "2h", "--progress=false"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestHistoryUnread: a history that doesn't let lapilli in, isn't where it
// was said to be, or sends it to sign in fails the replays for the run, as
// one that turns every query away does: the rules are named at the top of
// the report, and --fail-unreplayed exits 1. One that refuses the queries
// as written fails neither.
func TestHistoryUnread(t *testing.T) {
	answer := func(status int, header, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if k, v, ok := strings.Cut(header, ": "); ok {
				w.Header().Set(k, v)
			}
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		}
	}
	for _, c := range []struct {
		name   string
		answer http.HandlerFunc
		args   []string
		unread bool
	}{
		{"401", answer(401, "", `{"status":"error","errorType":"auth","error":"token expired"}`), nil, true},
		{"403", answer(403, "", `{"status":"error","error":"denied"}`), nil, true},
		{"a gateway's 403", answer(403, "Content-Type: application/json", `{"message":"Missing Authentication Token"}`), nil, true},
		// JSON of another shape than the API's, of 400 too, is a gateway's.
		{"a gateway's 400", answer(400, "Content-Type: application/json", `{"message":"Bad Request"}`), nil, true},
		{"404", answer(404, "Content-Type: text/plain", "404 page not found"), nil, true},
		{"407", answer(407, "Proxy-Authenticate: Basic", "<html>Proxy Authentication Required</html>"), nil, true},
		{"408", answer(408, "", "request timeout"), nil, true},
		{"a sign-in's redirect", answer(302, "Location: /login", `<a href="/login">Found</a>.`), nil, true},
		{"a page, of 200", answer(200, "Content-Type: text/html", "<html>Sign in</html>"), nil, true},
		{"no token file", answer(200, "", `{"status":"success","data":{"resultType":"matrix","result":[]}}`),
			[]string{"--bearer-token-file", "/nonexistent/lapilli-token"}, true},
		{"refused as written", answer(400, "", `{"status":"error","errorType":"bad_data","error":"parse error"}`), nil, false},
		{"not done", answer(501, "", `{"status":"error","error":"not implemented"}`), nil, false},
	} {
		code, out, errs := unreadRun(t, c.answer, append(c.args, "--fail-unreplayed")...)
		top := strings.Contains(out, "! 2 of 2 rules couldn't be replayed, and are left out of what follows:\n  A: "+rules.HistoryUnread)
		if c.unread && (code != 1 || !top || !strings.Contains(errs, "2 of 2 rules couldn't be replayed, as the history couldn't be read (--fail-unreplayed)")) {
			t.Errorf("%s: exit %d\n%s\n%s", c.name, code, errs, out)
		}
		if !c.unread && (code != 0 || strings.Contains(out, "! ") || !strings.Contains(out, "  A: query failed: HTTP ")) {
			t.Errorf("%s: exit %d\n%s\n%s", c.name, code, errs, out)
		}
	}

	// In a pull request's comment, a history that didn't let lapilli in
	// says only that; the job's log says why.
	base := write(t, "base.yaml", "groups:\n  - name: shop\n    rules:\n      - alert: A\n        expr: up > 0\n        for: 5m\n")
	code, out, errs := unreadRun(t, answer(401, "", `{"status":"error","error":"token expired for ci-reader"}`), "--compare", base, "--changed-only", "--markdown")
	if code != 0 || !strings.Contains(out, "- `B`, after: `"+UnreadHistory+"`") || strings.Contains(out, "ci-reader") ||
		!strings.Contains(out, "**1 of the rules that differ couldn't be replayed**") ||
		!strings.Contains(errs, "lapilli verdict: B (after) couldn't be replayed: "+rules.HistoryUnread+"HTTP 401: token expired for ci-reader") {
		t.Errorf("exit %d\n%s\n%s", code, out, errs)
	}
}

// TestWaitPastTimeout: a wait that would end past --timeout, the API's
// (Retry-After) or a turn by --max-rate, leaves the rules unfinished, as
// the deadline would: exit 1, saying to raise --timeout, at once rather
// than after the wait; and a turn by --max-rate isn't said to be the API's.
func TestWaitPastTimeout(t *testing.T) {
	var asked atomic.Int64
	start := time.Now()
	code, out, errs := unreadRun(t, func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"status":"error","error":"slow down"}`)
	}, "--timeout", "3s")
	if code != 1 || !strings.Contains(errs, "2 of 2 rules didn't finish within --timeout 3s; raise it") ||
		!strings.Contains(out, "  A: "+replay.Unfinished+"\n") || time.Since(start) > 2500*time.Millisecond {
		t.Errorf("told to wait a minute: exit %d after %s and %d requests\n%s\n%s", code, time.Since(start), asked.Load(), errs, out)
	}
	code, out, errs = unreadRun(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}, "--timeout", "3s", "--max-rate", "0.01")
	if code != 1 || !strings.Contains(errs, "rules didn't finish within --timeout 3s; raise it") || strings.Contains(out+errs, "the API said") {
		t.Errorf("--max-rate 0.01: exit %d\n%s\n%s", code, errs, out)
	}
}

// TestGrafanaCap: the data sources behind one Grafana, each asked by a
// Limiter of its own, are asked --parallel at once in all, not --parallel
// each, as a replay's pools of each would: one Grafana answers them all.
func TestGrafanaCap(t *testing.T) {
	var cur, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(20 * time.Millisecond)
		cur.Add(-1)
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer srv.Close()
	f := &verdictFlags{grafana: srv.URL, parallel: 4}
	l := f.logs(nil)
	var wg sync.WaitGroup
	for d := range 6 {
		h, err := l.For(rules.Rule{Logs: fmt.Sprintf("ds%d", d), Graph: &rules.Graph{}})
		if err != nil {
			t.Fatal(err)
		}
		// --parallel rules of each data source at once, as a replay has.
		for range f.parallel {
			wg.Go(func() {
				for range 3 {
					if _, err := h.QueryRange(context.Background(), `sum(count_over_time({app="a"}[1m]))`, t0.Add(-time.Hour), t0, time.Minute); err != nil {
						t.Error(err)
					}
				}
			})
		}
	}
	wg.Wait()
	if peak.Load() > 4 || peak.Load() < 2 {
		t.Errorf("6 data sources, --parallel 4: %d requests at once to the one Grafana", peak.Load())
	}
}

// TestLokiShared: --loki's queries, and its ruler, which lapilli serve
// reads each minute, are asked by one Limiter.
func TestLokiShared(t *testing.T) {
	f := &verdictFlags{loki: "http://loki:3100"}
	var cfg serve.Config
	f.lokiInto(&cfg)
	if l := f.lokiSource().Limit; l == nil || l != f.lokiSource().Limit || cfg.LokiLimit != l || cfg.Loki != f.loki {
		t.Errorf("serve's Limiter %p, the Loki's %p and %p", cfg.LokiLimit, l, f.lokiSource().Limit)
	}
}
