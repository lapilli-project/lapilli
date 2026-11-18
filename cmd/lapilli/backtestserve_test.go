package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// The backtest port's flags go together, and with what it needs: a trust
// file, read at start, that can be used; rules and a history to replay
// them against; writes taken. Each wrong is refused before serve starts,
// saying why.
func TestServeBacktestFlags(t *testing.T) {
	rules := write(t, "rules.yaml", "groups: []\n")
	token := write(t, "token", strings.Repeat("t", 40))
	trust := write(t, "trust.yaml", "trust:\n  - {name: plain, token_file: "+token+"}\n")
	bad := write(t, "bad.yaml", "trust:\n  - {name: plain, token_file: "+token+", claims: {sub: x}}\n")
	short := write(t, "short.yaml", "trust:\n  - {name: plain, token_file: "+write(t, "short", "short")+"}\n")
	incidentsToken := write(t, "incidents-token", strings.Repeat("t", 40))
	// A ledger of the test's own: a case that got past the flags would
	// otherwise open lapilli-ledger/ where the test runs.
	base := []string{"--prometheus", "http://127.0.0.1:1", "--rules", rules, "--ledger", t.TempDir()}
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--backtest-listen", ":8084"}, "needs --backtest-trust"},
		{[]string{"--backtest-trust", trust}, "go with --backtest-listen"},
		{[]string{"--backtest-share", "0.5"}, "go with --backtest-listen"},
		{[]string{"--backtest-listen", ":8084", "--backtest-trust", trust, "--backtest-share", "0.9"}, "0.05 to 0.75"},
		{[]string{"--backtest-listen", ":8084", "--backtest-trust", trust, "--backtest-share", "0.01"}, "0.05 to 0.75"},
		{[]string{"--backtest-listen", ":8084", "--backtest-trust", trust, "--backtest-queue", "0"}, "1 to 1000"},
		{[]string{"--backtest-listen", ":8084", "--backtest-trust", trust, "--backtest-timeout", "3h"}, "2h at most"},
		{[]string{"--backtest-listen", ":8084", "--backtest-trust", trust, "--backtest-timeout", "10s"}, "want a duration"},
		{[]string{"--backtest-listen", ":8084", "--backtest-trust", trust, "--read-only"}, "--read-only"},
		{[]string{"--backtest-listen", ":8084", "--backtest-trust", bad}, "plain token"},
		{[]string{"--backtest-listen", ":8084", "--backtest-trust", short}, "32 characters"},
		{[]string{"--backtest-listen", ":8084", "--backtest-trust", trust + ".missing"}, "--backtest-trust"},
		{[]string{"--backtest-listen", ":8084", "--backtest-trust", trust, "--incidents-listen", ":8082", "--incidents-token-file", incidentsToken}, "same token"},
		{[]string{"--backtest-listen", ":8084", "--backtest-trust", trust, "--loki", "https://loki.example.com", "--loki-token-file", token}, "same token"},
	} {
		// Read as serve reads them: one that got past would start serve.
		if _, err := readServe(append(append([]string{}, base...), c.args...), false); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%v: %v, want it to say %q", c.args, err, c.says)
		}
	}
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--alerts", "--prometheus", "http://127.0.0.1:1", "--backtest-listen", ":8084", "--backtest-trust", trust, "--ledger", t.TempDir()}, "--alerts"},
		{[]string{"--rules", rules, "--backtest-listen", ":8084", "--backtest-trust", trust, "--ledger", t.TempDir()}, "--prometheus"},
	} {
		if _, err := readServe(c.args, false); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%v: %v, want it to say %q", c.args, err, c.says)
		}
	}
	// The help says each.
	var out, errs strings.Builder
	run([]string{"serve", "-h"}, &out, &errs)
	for _, f := range []string{"--backtest-listen ADDR", "--backtest-trust FILE", "--backtest-share F", "--backtest-queue N", "--backtest-timeout DUR"} {
		if !strings.Contains(out.String(), f) {
			t.Errorf("serve -h doesn't say %s", f)
		}
	}
}

// The flags as serve takes them.
func TestServeBacktestFlagsRead(t *testing.T) {
	rules := write(t, "rules.yaml", "groups: []\n")
	token := write(t, "token", strings.Repeat("t", 40))
	trust := write(t, "trust.yaml", "trust:\n  - {name: plain, token_file: "+token+", hourly: 5}\n")
	st, err := readServe([]string{"--prometheus", "http://127.0.0.1:1", "--rules", rules, "--max-rate", "2", "--backtest-listen", ":8084", "--backtest-trust", trust,
		"--backtest-share", "0.5", "--backtest-queue", "40", "--backtest-timeout", "30m"}, false)
	if err != nil {
		t.Fatal(err)
	}
	c := st.cfg
	if c.BacktestListen != ":8084" || c.BacktestShare != 0.5 || c.BacktestQueue != 40 || c.BacktestTimeout.Minutes() != 30 || len(c.Trust) != 1 || c.Trust[0].Hourly != 5 {
		t.Errorf("%+v", c)
	}
}

// A side of a comparison whose files hold no rules --rules-select keeps is
// a side with none, as an empty directory is: the rules the other holds
// are added, and the run is a comparison, not an error. lapilli serve's
// backtests take it so.
func TestCompareNoneSelected(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	doc := func(team string) string {
		return "apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata: {name: r, labels: {team: " + team + "}}\nspec:\n  groups:\n  - name: g\n    rules:\n    - {alert: Added, expr: errors > 5}\n"
	}
	head, base := write(t, "head.yaml", doc("shop")), write(t, "base.yaml", doc("other"))
	var out, errs bytes.Buffer
	code := run([]string{"verdict", "--rules", head, "--compare", base, "--rules-select", "team=shop", "--changed-only", "--markdown", "--prometheus", "http://x"}, &out, &errs)
	if code != 0 || !strings.Contains(out.String(), `"added":1`) {
		t.Errorf("exit %d: %s\n%s", code, out.String(), errs.String())
	}
}

// Neither side kept by --rules-select, as a label's name mistyped makes
// it: an error, as it was before a side could be empty, never two sets
// alike.
func TestCompareNoneSelectedEither(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return fakeHistory{}, nil }
	now = func() time.Time { return t0 }
	doc := "apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata: {name: r, labels: {team: shop}}\nspec:\n  groups:\n  - name: g\n    rules:\n    - {alert: A, expr: errors > 5}\n"
	head, base := write(t, "head.yaml", doc), write(t, "base.yaml", doc)
	var out, errs bytes.Buffer
	code := run([]string{"verdict", "--rules", head, "--compare", base, "--rules-select", "tem=shop", "--changed-only", "--markdown", "--prometheus", "http://x"}, &out, &errs)
	if code != 1 || !strings.Contains(errs.String(), "keeps no document of rules") {
		t.Errorf("exit %d: %s\n%s", code, out.String(), errs.String())
	}
}
