package main

import (
	"io"
	"strings"
	"testing"
)

// --config is read as serve starts, and refused with the line of what is
// wrong; --config-prune needs it; --proposals-expire is a duration of an
// hour or more, or 0; neither goes with --read-only, which writes nothing.
func TestServeConfigFlags(t *testing.T) {
	rulesFile := write(t, "rules.yaml", "groups:\n  - name: g\n    rules:\n      - {alert: A, expr: a}\n")
	bad := write(t, "config.yaml", "teams:\n  - name: payments\n    colour: red\n")
	good := write(t, "good.yaml", "teams:\n  - {name: payments, rules: ^A}\n")
	base := []string{"serve", "--rules", rulesFile, "--prometheus", "http://x"}
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--config", bad}, `line 3: unknown key "colour" in a team`},
		{[]string{"--config", "/no/such/config.yaml"}, "no such file"},
		{[]string{"--config-prune"}, "--config-prune deletes what --config no longer declares"},
		{[]string{"--config", good, "--read-only"}, "--read-only refuses writes"},
		{[]string{"--proposals-expire", "soon"}, "--proposals-expire soon: want a duration"},
		{[]string{"--proposals-expire", "10m"}, "--proposals-expire 10m: want a duration"},
		{[]string{"--proposals-expire", "30d", "--read-only"}, "--read-only refuses writes"},
	} {
		var errs strings.Builder
		if code := run(append(append([]string{}, base...), c.args...), io.Discard, &errs); code != 2 || !strings.Contains(errs.String(), c.says) {
			t.Errorf("%v: exit %d, want 2 saying %q:\n%s", c.args, code, c.says, errs.String())
		}
	}
	// What the flags take, given well.
	for _, args := range [][]string{
		{"--config", good, "--config-prune", "--proposals-expire", "30d"},
		{"--config", good, "--config-prune=false", "--proposals-expire", "0"},
	} {
		args = append([]string{"--rules", rulesFile, "--prometheus", "http://x"}, args...)
		if _, err := parseFlags(args, func(name, val string) (bool, error) {
			return name == "--config" || name == "--config-prune" || name == "--proposals-expire", nil
		}); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}
