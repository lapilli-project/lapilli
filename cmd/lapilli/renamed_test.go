package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// TestRenameFlags: a flag by its old name is the new, in either form, said
// once on stderr; under both names it is an error naming both; a flag of
// another command is left alone, and the table kept renames nothing.
func TestRenameFlags(t *testing.T) {
	table := map[string]map[string]string{
		"verdict": {"--old-window": "--window"},
		"serve":   {"--old-listen": "--listen"},
	}
	for _, c := range []struct {
		cmd        string
		args, want []string
		said       string
	}{
		{"verdict", []string{"--old-window", "7d", "--rules=a"}, []string{"--window", "7d", "--rules=a"},
			"lapilli verdict: --old-window is --window now; --old-window will go in a major release\n"},
		{"verdict", []string{"--old-window=7d", "--old-window=8d"}, []string{"--window=7d", "--window=8d"},
			"lapilli verdict: --old-window is --window now; --old-window will go in a major release\n"},
		{"verdict", []string{"--old-listen", "x"}, []string{"--old-listen", "x"}, ""},
		{"serve", []string{"--old-listen=:1", "--old-window=1d"}, []string{"--listen=:1", "--window=1d"},
			"lapilli serve: --old-listen is --listen now; --old-listen will go in a major release\n" +
				"lapilli serve: --old-window is --window now; --old-window will go in a major release\n"},
		{"preflight", []string{"--old-listen=:1"}, []string{"--old-listen=:1"}, ""},
		{"impact", []string{"suggest", "--old-window", "1d"}, []string{"suggest", "--window", "1d"},
			"lapilli impact: --old-window is --window now; --old-window will go in a major release\n"},
	} {
		var stderr bytes.Buffer
		got, err := renameFlags(c.cmd, c.args, table, &stderr)
		if c.cmd == "preflight" {
			// preflight takes serve's and verdict's flags.
			c.want, c.said = []string{"--listen=:1"}, "lapilli preflight: --old-listen is --listen now; --old-listen will go in a major release\n"
		}
		if err != nil || !reflect.DeepEqual(got, c.want) || stderr.String() != c.said {
			t.Errorf("%s %q: %q %v, said %q", c.cmd, c.args, got, err, stderr.String())
		}
	}
	for _, args := range [][]string{{"--old-window=1d", "--window", "2d"}, {"--window=1d", "--old-window", "2d"}} {
		_, err := renameFlags("verdict", args, table, new(bytes.Buffer))
		if err == nil || !strings.Contains(err.Error(), "--old-window") || !strings.Contains(err.Error(), "--window") {
			t.Errorf("%q: %v", args, err)
		}
	}
	// The table kept has no rename yet; nothing it names is taken twice.
	for cmd, m := range renamedFlags {
		for old, now := range m {
			if old == now || !strings.HasPrefix(old, "--") || !strings.HasPrefix(now, "--") {
				t.Errorf("%s: %s to %s", cmd, old, now)
			}
		}
	}
}

// TestRenamedFlagRuns: through the command, a flag by its old name works as
// the new, and says so; --help says the new name only.
func TestRenamedFlagRuns(t *testing.T) {
	was := renamedFlags
	defer func() { renamedFlags = was }()
	renamedFlags = map[string]map[string]string{"demo": {"--old-json": "--json"}}
	var oldOut, oldErr, newOut, newErr, help bytes.Buffer
	if code := run([]string{"demo", "--old-json"}, &oldOut, &oldErr); code != 0 {
		t.Fatalf("%d: %s", code, oldErr.String())
	}
	if code := run([]string{"demo", "--json"}, &newOut, &newErr); code != 0 {
		t.Fatalf("%d: %s", code, newErr.String())
	}
	want := "lapilli demo: --old-json is --json now; --old-json will go in a major release\n"
	if oldOut.String() != newOut.String() || oldErr.String() != want+newErr.String() {
		t.Errorf("the old name gave another answer than the new: stderr %q", oldErr.String())
	}
	run([]string{"demo", "--help"}, &help, &help)
	if strings.Contains(help.String(), "--old-json") {
		t.Error("--help names the old flag")
	}
	var out, errs bytes.Buffer
	if code := run([]string{"demo", "--old-json", "--json"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "--old-json and --json are the same flag") {
		t.Errorf("both names: %d %q", code, errs.String())
	}
}

// TestRenamedFlagsTable: each flag renamed before this release is taken
// under its old name, through the command that has it, as under the new.
func TestRenamedFlagsTable(t *testing.T) {
	for cmd, m := range map[string]map[string]string{
		"verdict": {"--bearer-token-file": "--prometheus-token-file", "--extras": "--extras-timeout", "--expect-required": "--fail-no-expectation",
			"--gcp-project": "--google-project", "--redact-key": "--redact-key-file"},
		"serve": {"--notify-listen": "--notifications-listen", "--notify-token-file": "--notifications-token-file",
			"--notify-grafana-secret-file": "--notifications-grafana-secret-file", "--notify-sns": "--notifications-sns",
			"--incidents-listen": "--jobs-listen", "--changes-paused": "--changes-pause-file", "--changes-token-file": "--changes-recipe-token-file",
			"--webhook-dest-secret": "--webhook-dest-secret-file", "--oidc-hosted-domain": "--oidc-hosted-domains"},
	} {
		for old, now := range m {
			for _, c := range []string{cmd, "preflight"} {
				var stderr bytes.Buffer
				got, err := renameFlags(c, []string{old + "=x"}, renamedFlags, &stderr)
				if err != nil || len(got) != 1 || got[0] != now+"=x" || !strings.Contains(stderr.String(), old+" is "+now+" now") {
					t.Errorf("%s %s: %q %v %q", c, old, got, err, stderr.String())
				}
			}
		}
	}
	for _, old := range []string{"--notifications", "--depends-count-missed"} {
		for cmd := range renamedFlags {
			if _, ok := renamedIn(renamedFlags, cmd)[old]; ok {
				t.Errorf("%s keeps %s, which was renamed before it was released", cmd, old)
			}
		}
	}
}
