package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// The answers to one range, a rule's query over the period, are held to
// --max-samples in all (prom.DefaultMaxSamples, 10,000,000 as built; 2,000
// here, as a run of some 3,600 is past it): past it, the rule isn't replayed,
// said with the bound and how to read it all the same; with a larger
// --max-samples, the same run reads.
func TestMaxSamples(t *testing.T) {
	was, wasHistory, wasNow := prom.DefaultMaxSamples, newHistory, now
	prom.DefaultMaxSamples = 2000
	// Other tests leave a history of their own, and a clock.
	newHistory = func(ctx context.Context, f *verdictFlags) (history, error) {
		return f.prometheus().History(ctx, "lapilli/test")
	}
	now = time.Now
	t.Cleanup(func() { prom.DefaultMaxSamples, newHistory, now = was, wasHistory, wasNow })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		r.ParseForm()
		start, _ := strconv.ParseInt(r.Form.Get("start"), 10, 64)
		end, _ := strconv.ParseInt(r.Form.Get("end"), 10, 64)
		step, _ := strconv.ParseFloat(r.Form.Get("step"), 64)
		result := []any{}
		if r.Form.Get("query") == "errors > 5" && step > 0 {
			for i := range 10 {
				var values [][]any
				for at := start; at <= end; at += int64(step) {
					values = append(values, []any{at, "6"})
				}
				result = append(result, map[string]any{"metric": map[string]string{"pod": fmt.Sprint("api-", i)}, "values": values})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}})
	}))
	t.Cleanup(srv.Close)
	file := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(file, []byte("groups:\n  - name: shop\n    rules:\n      - alert: CheckoutErrors\n        expr: errors > 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--prometheus", srv.URL, "--rules", file, "--since", "6h", "--scrape-interval", "1m", "--impact", "up == 0", "--json"}
	replayed := func(more ...string) (string, bool) {
		t.Helper()
		var out, errs bytes.Buffer
		if code := runVerdict(append(args, more...), &out, &errs); code != 0 {
			t.Fatalf("exit %d: %s", code, errs.String())
		}
		var rep struct {
			Rules []struct {
				Rule, Unsupported string
				Firings           int
			}
		}
		if err := json.Unmarshal(out.Bytes(), &rep); err != nil || len(rep.Rules) != 1 {
			t.Fatalf("%v: %s", err, out.String())
		}
		return rep.Rules[0].Unsupported, rep.Rules[0].Firings == 10
	}
	why, ok := replayed()
	if ok || !strings.Contains(why, "more than 2000 samples in all (--max-samples): read it at a longer --step, over a shorter range (--since), or with a larger --max-samples, each 1000000 more taking some 64 MB more at the peak") {
		t.Errorf("past the default: replayed %v, %q", ok, why)
	}
	if why, ok := replayed("--max-samples", "5000"); !ok || why != "" {
		t.Errorf("with --max-samples 5000: replayed %v, %q", ok, why)
	}
	var out, errs bytes.Buffer
	if code := runVerdict(append(args, "--max-samples", "999"), &out, &errs); code != 2 || !strings.Contains(errs.String(), "--max-samples 999: want a number of samples from 1000") {
		t.Errorf("--max-samples 999: exit %d, %s", code, errs.String())
	}
}

// --max-samples reaches every history a range is read from: --prometheus,
// --loki and a Grafana's Loki data sources; and lapilli serve takes it, as
// it takes lapilli verdict's flags.
func TestMaxSamplesEveryHistory(t *testing.T) {
	// Other tests leave a --loki of their own (newGrafanaLoki is as
	// built).
	wasLoki := newLoki
	newLoki = func(f *verdictFlags) history { return f.lokiSource().History("lapilli/test") }
	t.Cleanup(func() { newLoki = wasLoki })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/query_range") {
			http.NotFound(w, r)
			return
		}
		r.ParseForm()
		start, _ := strconv.ParseInt(r.Form.Get("start"), 10, 64)
		end, _ := strconv.ParseInt(r.Form.Get("end"), 10, 64)
		var values [][]any
		for at := start / 60 * 60; at <= end; at += 60 {
			if at >= start {
				values = append(values, []any{at, "1"})
			}
		}
		result := []any{}
		for i := range 10 {
			result = append(result, map[string]any{"metric": map[string]string{"pod": fmt.Sprint("api-", i)}, "values": values})
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}})
	}))
	t.Cleanup(srv.Close)
	f := &verdictFlags{promURL: srv.URL, loki: srv.URL, grafana: srv.URL, maxSamples: 1000}
	p, err := f.prometheus().History(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for name, h := range map[string]history{"--prometheus": p, "--loki": newLoki(f), "a Grafana's Loki": newGrafanaLoki(f, "loki", nil)} {
		if _, err := h.QueryRange(context.Background(), "x", t0, t0.Add(6*time.Hour), time.Minute); err == nil || !strings.Contains(err.Error(), "more than 1000 samples in all") {
			t.Errorf("%s: %v", name, err)
		}
	}
	var out, errs bytes.Buffer
	if code := run([]string{"serve", "--prometheus", srv.URL, "--max-samples", "999"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "--max-samples 999: want a number of samples from 1000") {
		t.Errorf("lapilli serve --max-samples 999: exit %d, %s", code, errs.String())
	}
}
