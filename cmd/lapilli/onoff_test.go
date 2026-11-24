package main

import (
	"io"
	"strings"
	"testing"
)

// TestOnOffFlags: a flag that is on or off is on by itself and as =true
// says, off as =false says, and refused with any other value, in lapilli
// verdict, impact suggest, serve and preflight alike: --json=false was
// --json, and --linked-only=false judged by the link alone.
func TestOnOffFlags(t *testing.T) {
	read := map[string]func(f *verdictFlags) bool{
		"--alerts":                func(f *verdictFlags) bool { return f.alerts },
		"--json":                  func(f *verdictFlags) bool { return f.json },
		"--csv":                   func(f *verdictFlags) bool { return f.csv },
		"--markdown":              func(f *verdictFlags) bool { return f.markdown },
		"--redact":                func(f *verdictFlags) bool { return f.redact },
		"--metrics":               func(f *verdictFlags) bool { return f.metrics },
		"--changed-only":          func(f *verdictFlags) bool { return f.changedOnly },
		"--fail-expectations":     func(f *verdictFlags) bool { return f.failExpect },
		"--expect-required":       func(f *verdictFlags) bool { return f.expectRequired },
		"--fail-missed":           func(f *verdictFlags) bool { return f.failMissed },
		"--check-replay":          func(f *verdictFlags) bool { return f.checkReplay },
		"--check-content":         func(f *verdictFlags) bool { return f.checkContent },
		"--loaded-rules":          func(f *verdictFlags) bool { return f.loadedRules },
		"--loki-loaded-rules":     func(f *verdictFlags) bool { return f.lokiLoaded },
		"--fail-unreplayed":       func(f *verdictFlags) bool { return f.failUnreplayed },
		"--linked-only":           func(f *verdictFlags) bool { return f.opts.LinkedOnly },
		"--grafana-history":       func(f *verdictFlags) bool { return !f.noGrafanaHistory },
		"--progress":              func(f *verdictFlags) bool { return !f.noProgress },
		"--check-no-data":         func(f *verdictFlags) bool { return !f.noCheckNoData },
		"--fail-nothing-replayed": func(f *verdictFlags) bool { return !f.noFailNothingReplayed },
	}
	for name, on := range read {
		for given, want := range map[string]bool{name: true, name + "=true": true, name + "=false": false} {
			f, err := readFlags([]string{given}, nil)
			if err != nil {
				t.Errorf("%s: %v", given, err)
				continue
			}
			if on(f) != want {
				t.Errorf("%s: on %v, want %v", given, on(f), want)
			}
		}
		for _, v := range []string{"yes", "no", "1", "0", "TRUE", "False", "t", ""} {
			if _, err := readFlags([]string{name + "=" + v}, nil); err == nil || !strings.Contains(err.Error(), name+"="+v+": want true or false") {
				t.Errorf("%s=%s: %v", name, v, err)
			}
		}
	}

	// The last one given decides, and --redact-key redacts whatever came.
	if f, err := readFlags([]string{"--json", "--json=false"}, nil); err != nil || f.json {
		t.Errorf("--json --json=false: %v %v", err, f)
	}
	key := write(t, "key", strings.Repeat("ab", 16))
	if f, err := readFlags([]string{"--redact-key", key}, nil); err != nil || !f.redact {
		t.Errorf("--redact-key: %v", err)
	}
	for _, args := range [][]string{{"--redact=false", "--redact-key", key}, {"--redact-key", key, "--redact=false"}} {
		if _, err := readFlags(args, nil); err == nil || !strings.Contains(err.Error(), "leave out --redact=false") {
			t.Errorf("%v: %v", args, err)
		}
	}

	// lapilli impact suggest.
	for given, want := range map[string]bool{"--json": true, "--json=true": true, "--json=false": false} {
		f, err := parseSuggestFlags([]string{"--prometheus", "http://p", given})
		if err != nil || f.json != want {
			t.Errorf("impact suggest %s: %v, json %v", given, err, f != nil && f.json)
		}
	}
	if _, err := parseSuggestFlags([]string{"--prometheus", "http://p", "--json=1"}); err == nil || !strings.Contains(err.Error(), "--json=1: want true or false") {
		t.Errorf("impact suggest --json=1: %v", err)
	}

	// serve's own, and preflight's.
	got := map[string]string{}
	asServe := func(name, val string) (bool, error) { got[name] = val; return true, nil }
	for _, name := range []string{"--read-only", "--notify-slack", "--changes", "--everyone-admin", "--oidc-bearer", "--config-prune", "--goal-tasks", "--expect-tasks", "--yield-on-429"} {
		for given, want := range map[string]string{name: "true", name + "=true": "true", name + "=false": "false"} {
			if _, err := readFlags([]string{given}, asServe); err != nil || got[name] != want {
				t.Errorf("serve %s: %v, %q", given, err, got[name])
			}
		}
		if _, err := readFlags([]string{name + "=1"}, asServe); err == nil || !strings.Contains(err.Error(), name+"=1: want true or false") {
			t.Errorf("serve %s=1: %v", name, err)
		}
	}
	for _, name := range []string{"--json", "--fail-unknown", "--offline"} {
		var errs strings.Builder
		if code := run([]string{"preflight", name + "=yes"}, io.Discard, &errs); code != 2 || !strings.Contains(errs.String(), name+"=yes: want true or false") {
			t.Errorf("preflight %s=yes: exit %d, %s", name, code, errs.String())
		}
	}
}
