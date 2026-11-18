package main

import (
	"strings"
	"testing"
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
	base := []string{"--prometheus", "http://127.0.0.1:1", "--rules", rules}
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
	} {
		var out, errs strings.Builder
		if code := run(append(append([]string{"serve"}, base...), c.args...), &out, &errs); code != 2 || !strings.Contains(errs.String(), c.says) {
			t.Errorf("%v: %d %s, want 2 saying %q", c.args, code, errs.String(), c.says)
		}
	}
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--alerts", "--prometheus", "http://127.0.0.1:1", "--backtest-listen", ":8084", "--backtest-trust", trust}, "--alerts"},
		{[]string{"--rules", rules, "--backtest-listen", ":8084", "--backtest-trust", trust}, "--prometheus"},
	} {
		var out, errs strings.Builder
		if code := run(append([]string{"serve"}, c.args...), &out, &errs); code != 2 || !strings.Contains(errs.String(), c.says) {
			t.Errorf("%v: %d %s, want 2 saying %q", c.args, code, errs.String(), c.says)
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
