package main

import (
	"io"
	"strings"
	"testing"
)

// --config is a file serve can read; what is wrong in it is said once
// serve runs (TestConfigAtStart), not as a flag given wrong, so that a pod
// restarted on it serves. --config-prune needs it; --proposals-expire is a
// duration of an hour or more, or 0, 0s, 0d; neither goes with
// --read-only, which writes nothing.
func TestServeConfigFlags(t *testing.T) {
	parallel(t)
	rulesFile := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: A, expr: a}\n")
	good := write(t, "good.yaml", "teams:\n  - {name: payments, rules: ^A}\n")
	base := []string{"serve", "--rules", rulesFile, "--prometheus", "http://x"}
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--config", "/no/such/config.yaml"}, "no such file"},
		{[]string{"--config-prune"}, "--config-prune deletes what --config no longer declares"},
		{[]string{"--goal-tasks"}, "--goal-tasks makes tasks for the goals --config declares"},
		{[]string{"--config", good, "--read-only"}, "--read-only refuses writes"},
		{[]string{"--proposals-expire", "soon"}, "--proposals-expire soon: want a duration"},
		{[]string{"--proposals-expire", "10m"}, "--proposals-expire 10m: want a duration"},
		{[]string{"--proposals-expire", "30d", "--read-only"}, "--read-only refuses writes"},
		// 0s and 0d are off, as 0 is: what is refused is the rest.
		{[]string{"--proposals-expire", "0s", "--config-prune"}, "--config-prune deletes"},
		{[]string{"--proposals-expire", "0d", "--config-prune"}, "--config-prune deletes"},
	} {
		var errs strings.Builder
		if code := run(append(append([]string{}, base...), c.args...), io.Discard, &errs); code != 2 || !strings.Contains(errs.String(), c.says) {
			t.Errorf("%v: exit %d, want 2 saying %q:\n%s", c.args, code, c.says, errs.String())
		}
	}
	// What the flags take, given well.
	for _, args := range [][]string{
		{"--config", good, "--config-prune", "--proposals-expire", "30d"},
		{"--config", good, "--goal-tasks"},
		{"--config", good, "--config-prune=false", "--proposals-expire", "0"},
		{"--config", good, "--proposals-expire", "0s"},
		{"--config", good, "--proposals-expire", "0d"},
	} {
		args = append([]string{"--rules", rulesFile, "--prometheus", "http://x"}, args...)
		if _, err := parseFlags(args, func(name, val string) (bool, error) {
			return name == "--config" || name == "--config-prune" || name == "--goal-tasks" || name == "--proposals-expire", nil
		}); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// A --config file serve can read but not use isn't a flag given wrong:
// serve starts, on the ledger as it is, and says why on its pages, so that
// a pod restarted on it serves rather than loop. One it can't read is.
func TestConfigAtStart(t *testing.T) {
	parallel(t)
	bad := write(t, "config.yaml", "teams:\n  - name: payments\n    colour: red\n")
	empty := write(t, "empty.yaml", "")
	for _, f := range []string{bad, empty} {
		if err := configFlag(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if err := configFlag("/no/such/config.yaml"); err == nil {
		t.Error("a file that isn't there")
	}
}
