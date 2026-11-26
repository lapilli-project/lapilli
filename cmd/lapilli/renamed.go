package main

import (
	"fmt"
	"io"
	"strings"
)

// A flag renamed is still taken under its old name until the next major
// release (ROADMAP.md, "Toward 1.0"): the old name is read as the new, and
// stderr says so once. The commands parse their flags by hand, each in its
// own way; the old names are swapped for the new before any of them looks, so
// that none can come to take one differently.
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

// renameFlags gives args with each flag under an old name (--old or
// --old=value) under its new, saying so once on stderr; a flag given under
// both is an error.
func renameFlags(cmd string, args []string, table map[string]map[string]string, stderr io.Writer) ([]string, error) {
	renames := renamedIn(table, cmd)
	if len(renames) == 0 {
		return args, nil
	}
	nameOf := func(a string) string { n, _, _ := strings.Cut(a, "="); return n }
	given := map[string]bool{}
	for _, a := range args {
		given[nameOf(a)] = true
	}
	out := make([]string, len(args))
	said := map[string]bool{}
	for i, a := range args {
		name := nameOf(a)
		now, old := renames[name]
		if !old {
			out[i] = a
			continue
		}
		if given[now] {
			return nil, fmt.Errorf("%s and %s are the same flag: %s is %s now; give one", name, now, name, now)
		}
		if !said[name] {
			said[name] = true
			fmt.Fprintf(stderr, "lapilli %s: %s is %s now; %s will go in a major release\n", cmd, name, now, name)
		}
		out[i] = now + strings.TrimPrefix(a, name)
	}
	return out, nil
}
