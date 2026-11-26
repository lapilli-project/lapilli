package main

import (
	"fmt"
	"io"
)

// A flag renamed is still taken under its old name until the next major
// release (ROADMAP.md, "Toward 1.0"): the old name is read as the new, and
// stderr says so once. The commands parse their flags by hand, each in its
// own way; each hands a name it has read to flagNames.name, which gives it
// back as the new, so that none can come to take one differently, and a value
// is never taken for a name.
//
// renamedFlags are the flags renamed since 1.0's promise began, by the
// command that takes them: old name to new, each with its two dashes. A flag
// a command shares with another is listed under the one that has it: serve
// and impact take verdict's, preflight serve's. docs/upgrading.md lists
// them. Neither the config file nor an environment variable stands for a
// flag, so none has an old name to take; a rename of a key in --config would
// be taken in the file's reader, in internal/serve, as these are here.
var renamedFlags = map[string]map[string]string{
	"verdict": {
		"--bearer-token-file": "--prometheus-token-file",
		"--extras":            "--extras-timeout",
		"--expect-required":   "--fail-no-expectation",
		"--gcp-project":       "--google-project",
		"--redact-key":        "--redact-key-file",
	},
	"serve": {
		"--notify-listen":              "--notifications-listen",
		"--notify-token-file":          "--notifications-token-file",
		"--notify-grafana-secret-file": "--notifications-grafana-secret-file",
		"--notify-sns":                 "--notifications-sns",
		"--incidents-listen":           "--jobs-listen",
		"--changes-paused":             "--changes-pause-file",
		"--changes-token-file":         "--changes-recipe-token-file",
		"--webhook-dest-secret":        "--webhook-dest-secret-file",
		"--oidc-hosted-domain":         "--oidc-hosted-domains",
	},
	"preflight": {},
	"ledger":    {},
	"impact":    {},
	"demo":      {},
}

// flagsOf is the flags a command's renames are looked up in: its own, and
// those of the commands whose flags it takes.
var flagsOf = map[string][]string{
	"serve":     {"serve", "verdict"},
	"preflight": {"preflight", "serve", "verdict"},
	"impact":    {"impact", "verdict"},
}

// renamedIn is the renames that apply to a command.
func renamedIn(table map[string]map[string]string, cmd string) map[string]string {
	from := flagsOf[cmd]
	if from == nil {
		from = []string{cmd}
	}
	all := map[string]string{}
	for i := len(from) - 1; i >= 0; i-- {
		for old, now := range table[from[i]] {
			all[old] = now
		}
	}
	return all
}

// flagNames takes a flag by its old name as its new, for one run of a
// command: the name is translated where a command's parser reads a name, so
// that a value (a path that happens to be "--extras") is never touched. It
// says once on stderr that a name is renamed, and refuses a flag given under
// both names. A nil *flagNames renames nothing.
type flagNames struct {
	cmd     string
	renames map[string]string // old to new
	was     map[string]string // new to old
	stderr  io.Writer
	said    map[string]bool
	gaveOld map[string]bool // by the new name
	gaveNew map[string]bool
}

// newFlagNames is the renames of cmd's flags in table, for one run.
func newFlagNames(cmd string, table map[string]map[string]string, stderr io.Writer) *flagNames {
	renames := renamedIn(table, cmd)
	if len(renames) == 0 {
		return nil
	}
	r := &flagNames{cmd: cmd, renames: renames, was: map[string]string{}, stderr: stderr,
		said: map[string]bool{}, gaveOld: map[string]bool{}, gaveNew: map[string]bool{}}
	for old, now := range renames {
		r.was[now] = old
	}
	return r
}

// name is a flag's name, as given, under its new name; it errors when the
// flag is given under both.
func (r *flagNames) name(given string) (string, error) {
	if r == nil {
		return given, nil
	}
	if now, old := r.renames[given]; old {
		if r.gaveNew[now] {
			return "", sameFlag(given, now)
		}
		r.gaveOld[now] = true
		if !r.said[given] {
			r.said[given] = true
			fmt.Fprintf(r.stderr, "lapilli %s: %s is %s now; %s will go in a major release\n", r.cmd, given, now, given)
		}
		return now, nil
	}
	if old, renamed := r.was[given]; renamed {
		if r.gaveOld[given] {
			return "", sameFlag(old, given)
		}
		r.gaveNew[given] = true
	}
	return given, nil
}

func sameFlag(old, now string) error {
	return fmt.Errorf("%s and %s are the same flag: %s is %s now; give one", old, now, old, now)
}
