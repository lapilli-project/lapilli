package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

// TestUnreplayedShown: a rule whose replay failed is named at the top of
// the report, not only at its foot; --json lists it with why; the exit
// stays 0 unless --fail-unreplayed.
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
      - alert: Quiet
        expr: never
`)
	later := write(t, "later.yaml", `groups:
  - name: shop
    rules:
      - alert: CheckoutErrors
        expr: errors > 5 or broken
        for: 5m
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

	code, out, _ := verdictOf("--rules", rs)
	top := "! 1 of 3 rules couldn't be replayed, and are left out of what follows:\n  Typo: query failed: bad_data: parse error\n"
	if code != 0 || !strings.Contains(out, top) || strings.Index(out, top) > strings.Index(out, "RULE") {
		t.Errorf("exit %d: want the rule named before the table:\n%s", code, out)
	}
	if !strings.Contains(out, "Could not replay 1 rules:\n  Typo: query failed") {
		t.Errorf("the list at the foot is gone:\n%s", out)
	}

	code, out, _ = verdictOf("--rules", rs, "--json")
	var rep verdict.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 {
		t.Fatalf("exit %d: %v", code, err)
	}
	if len(rep.Unreplayed) != 1 || rep.Unreplayed[0] != (verdict.Unreplayed{Rule: "Typo", Reason: "query failed: bad_data: parse error", Failed: true}) {
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
	if code, out, _ = verdictOf("--rules", alarms, "--fail-unreplayed"); code != 0 || strings.Contains(out, "! ") {
		t.Errorf("exit %d with only a rule no run replays:\n%s", code, out)
	}

	code, _, errs := verdictOf("--rules", rs, "--fail-unreplayed")
	if code != 1 || !strings.Contains(errs, "1 of 3 rules couldn't be replayed, their queries failed (--fail-unreplayed)") {
		t.Errorf("--fail-unreplayed: exit %d\n%s", code, errs)
	}

	// Compared: a rule alike in both once; a changed rule by its side.
	code, out, _ = verdictOf("--rules", later, "--compare", rs)
	want := "! 2 rules couldn't be replayed, and are left out of what follows:\n  CheckoutErrors (after): query failed: bad_data: parse error\n  Typo: query failed: bad_data: parse error\n"
	if code != 0 || !strings.Contains(out, want) {
		t.Errorf("exit %d, want\n%s\nin\n%s", code, want, out)
	}
	code, out, _ = verdictOf("--rules", later, "--compare", rs, "--json")
	var cmp verdict.Comparison
	if err := json.Unmarshal([]byte(out), &cmp); err != nil || code != 0 {
		t.Fatalf("exit %d: %v", code, err)
	}
	if len(cmp.Unreplayed) != 2 || cmp.Unreplayed[0].Side != "after" || cmp.Unreplayed[1] != (verdict.Unreplayed{Rule: "Typo", Reason: "query failed: bad_data: parse error", Failed: true}) {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if asked.Add(1) == 1 {
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
	f, err = parseVerdictFlags([]string{"--rules", "r", "--prometheus", "http://p", "--incidents", "i", "--progress=false", "--fail-unreplayed"})
	if err != nil || !f.noProgress || !f.failUnreplayed {
		t.Errorf("%v %+v", err, f)
	}
}
