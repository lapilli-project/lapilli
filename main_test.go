package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, c := range []struct {
		args   string
		code   int
		stdout string
		stderr string
	}{
		{args: "", code: 2, stderr: "lapilli ssh [ssh options]"},
		{args: "version", code: 0, stdout: "lapilli "},
		{args: "--help", code: 0, stdout: "Run \"lapilli ssh -h\""},
		{args: "help ssh", code: 0, stdout: "ssh options passed through"},
		{args: "ssh -h", code: 0, stdout: "ssh options passed through"},
		{args: "deploy@build", code: 2, stderr: "Did you mean: lapilli ssh deploy@build"},
		{args: "--json", code: 2, stderr: `unknown command "--json"`},
		{args: "ssh --nope h", code: 2, stderr: "lapilli ssh: unknown flag --nope"},
		{args: "ssh -L 8080:localhost:80 h", code: 2, stderr: "-L is not supported by lapilli ssh"},
		{args: "ssh --log testdata/logs/rejected.log", code: 1, stdout: "Keys, in the order ssh tried them:"},
		{args: "ssh --log testdata/logs/ok_file.log", code: 0, stdout: "✓"},
		{args: "ssh --json --log testdata/logs/too_many.log", code: 1, stdout: `"code": "too-many-keys"`},
	} {
		var stdout, stderr bytes.Buffer
		code := run(strings.Fields(c.args), &stdout, &stderr)
		if code != c.code {
			t.Errorf("lapilli %s: exit %d, want %d\nstdout: %s\nstderr: %s", c.args, code, c.code, stdout.String(), stderr.String())
		}
		if c.stdout != "" && !strings.Contains(stdout.String(), c.stdout) {
			t.Errorf("lapilli %s: stdout lacks %q:\n%s", c.args, c.stdout, stdout.String())
		}
		if c.stderr != "" && !strings.Contains(stderr.String(), c.stderr) {
			t.Errorf("lapilli %s: stderr lacks %q:\n%s", c.args, c.stderr, stderr.String())
		}
	}
}
