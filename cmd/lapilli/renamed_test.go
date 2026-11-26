package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// renamedAll reads each name in names as a command's parser does, through
// flagNames.
func renamedAll(cmd string, names []string, table map[string]map[string]string, stderr *bytes.Buffer) ([]string, error) {
	rn := newFlagNames(cmd, table, stderr)
	var out []string
	for _, n := range names {
		now, err := rn.name(n)
		if err != nil {
			return nil, err
		}
		out = append(out, now)
	}
	return out, nil
}

// TestRenameFlags: a flag's old name is read as the new, said once on
// stderr; under both names it is an error naming both, in either order; a
// flag of another command is left alone, and what the table keeps is sound.
func TestRenameFlags(t *testing.T) {
	table := map[string]map[string]string{
		"verdict": {"--old-window": "--window"},
		"serve":   {"--old-listen": "--listen"},
	}
	const saidWindow = "lapilli verdict: --old-window is --window now; --old-window will go in a major release\n"
	for _, c := range []struct {
		cmd        string
		args, want []string
		said       string
	}{
		{"verdict", []string{"--old-window", "--rules", "--old-window"}, []string{"--window", "--rules", "--window"}, saidWindow},
		{"verdict", []string{"--old-listen"}, []string{"--old-listen"}, ""},
		{"serve", []string{"--old-listen", "--old-window"}, []string{"--listen", "--window"},
			"lapilli serve: --old-listen is --listen now; --old-listen will go in a major release\n" +
				"lapilli serve: --old-window is --window now; --old-window will go in a major release\n"},
		{"preflight", []string{"--old-listen"}, []string{"--listen"},
			"lapilli preflight: --old-listen is --listen now; --old-listen will go in a major release\n"},
		{"impact", []string{"--old-window", "--window"}, nil, ""},
		{"impact", []string{"--old-window"}, []string{"--window"}, "lapilli impact: --old-window is --window now; --old-window will go in a major release\n"},
		{"demo", []string{"--old-window"}, []string{"--old-window"}, ""},
	} {
		var stderr bytes.Buffer
		got, err := renamedAll(c.cmd, c.args, table, &stderr)
		if c.want == nil {
			if err == nil {
				t.Errorf("%s %q: no error", c.cmd, c.args)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) || stderr.String() != c.said {
			t.Errorf("%s %q: %q %v, said %q", c.cmd, c.args, got, err, stderr.String())
		}
	}
	for _, args := range [][]string{{"--old-window", "--window"}, {"--window", "--old-window"}} {
		_, err := renamedAll("verdict", args, table, new(bytes.Buffer))
		if err == nil || !strings.Contains(err.Error(), "--old-window") || !strings.Contains(err.Error(), "--window") {
			t.Errorf("%q: %v", args, err)
		}
	}
	for cmd, m := range renamedFlags {
		for old, now := range m {
			if old == now || !strings.HasPrefix(old, "--") || !strings.HasPrefix(now, "--") {
				t.Errorf("%s: %s to %s", cmd, old, now)
			}
		}
	}
}

// TestRenamedFlagValue: a value that is an old flag's name is a value, not
// that flag: it is neither renamed, nor said, nor taken for the flag given
// under both names. A name is read as one only where a parser looks for one.
func TestRenamedFlagValue(t *testing.T) {
	for _, c := range []struct {
		args []string
		path string
	}{
		{[]string{"--rules", "--extras"}, "--extras"},
		{[]string{"--rules=--extras"}, "--extras"},
		{[]string{"--extras-timeout", "1m", "--rules", "--extras"}, "--extras"},
		{[]string{"--extras", "1m", "--rules", "--extras-timeout"}, "--extras-timeout"},
	} {
		args := c.args
		var stderr bytes.Buffer
		f, err := readFlagsAs(newFlagNames("verdict", renamedFlags, &stderr), args, nil)
		if err != nil || len(f.rules) != 1 || f.rules[0].path != c.path {
			t.Errorf("%q: %+v %v", args, f, err)
			continue
		}
		wantSaid := ""
		if args[0] == "--extras" {
			wantSaid = "lapilli verdict: --extras is --extras-timeout now; --extras will go in a major release\n"
		}
		if stderr.String() != wantSaid {
			t.Errorf("%q: said %q", args, stderr.String())
		}
	}
	// The same through the command, and for serve and impact.
	for _, c := range [][]string{
		{"verdict", "--rules", "--extras"},
		{"serve", "--rules", "--notify-listen"},
		{"preflight", "--rules", "--notify-listen"},
		{"impact", "suggest", "--prometheus", "--gcp-project"},
	} {
		var out, errs bytes.Buffer
		run(c, &out, &errs)
		if strings.Contains(errs.String(), " is ") && strings.Contains(errs.String(), " now; ") {
			t.Errorf("%q: said a rename: %s", c, errs.String())
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
				got, err := renamedAll(c, []string{old}, renamedFlags, &stderr)
				if err != nil || len(got) != 1 || got[0] != now || !strings.Contains(stderr.String(), old+" is "+now+" now") {
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
