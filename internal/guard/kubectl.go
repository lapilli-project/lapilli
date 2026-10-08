// Package guard decides what the real kubectl is run with when an agent under evaluation calls it.
//
// The machine that runs an evaluation usually also holds credentials to real clusters; the one this was
// written on has a production cluster as the current context of its default kubeconfig. An agent that
// was allowed `kubectl get *` must not be able to read that cluster by adding a flag, and must not be
// able to change the one it was given.
//
// Two controls, and the second does not depend on the first being complete:
//
//   - What is refused. Verbs that are not read-only, flags that name another cluster or another
//     identity, flags and output formats that read or write local files, a raw path, and any flag in
//     a place where kubectl would read the command differently than it is read here. A refusal tells
//     the agent why, so it can go on.
//   - Where kubectl is pointed. The kubeconfig, context, cluster, user and server of the case are
//     appended to every invocation. kubectl takes the last value of a flag, so a spelling of --server
//     the refusals missed is overridden rather than obeyed.
//
// It is a guard, not a sandbox: an agent that may run arbitrary programs can run the real kubectl by
// its path. The guard is for agents whose harness lets them run `kubectl` and little else.
package guard

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ExitRefused is the exit code of a refused invocation.
const ExitRefused = 97

// Pin is the one place kubectl may talk to: the current context of one kubeconfig.
type Pin struct {
	Kubeconfig, Context, Cluster, User, Server string
}

// LoadPin reads the current context of a kubeconfig file.
func LoadPin(kubeconfig string) (Pin, error) {
	raw, err := os.ReadFile(kubeconfig)
	if err != nil {
		return Pin{}, err
	}
	var doc struct {
		Current  string `yaml:"current-context"`
		Contexts []struct {
			Name    string `yaml:"name"`
			Context struct {
				Cluster string `yaml:"cluster"`
				User    string `yaml:"user"`
			} `yaml:"context"`
		} `yaml:"contexts"`
		Clusters []struct {
			Name    string `yaml:"name"`
			Cluster struct {
				Server string `yaml:"server"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return Pin{}, fmt.Errorf("%s: %w", kubeconfig, err)
	}
	pin := Pin{Kubeconfig: kubeconfig, Context: doc.Current}
	for _, c := range doc.Contexts {
		if c.Name == doc.Current {
			pin.Cluster, pin.User = c.Context.Cluster, c.Context.User
		}
	}
	for _, c := range doc.Clusters {
		if c.Name == pin.Cluster {
			pin.Server = c.Cluster.Server
		}
	}
	if pin.Context == "" || pin.Cluster == "" || pin.Server == "" {
		return Pin{}, fmt.Errorf("%s: no current context with a cluster and a server; kubectl would have nowhere certain to go", kubeconfig)
	}
	return pin, nil
}

// readOnly are the verbs an investigation needs. A value lists the sub-verbs allowed; nil allows all.
var readOnly = map[string][]string{
	"get": nil, "describe": nil, "logs": nil, "events": nil, "top": nil, "explain": nil,
	"api-resources": nil, "api-versions": nil, "version": nil, "cluster-info": nil,
	"auth":    {"can-i", "whoami"},
	"rollout": {"status", "history"},
	"config":  {"current-context", "get-contexts"}, // names only; `config view --raw` prints credentials
}

// elsewhere are the flags that name another cluster or another identity.
var elsewhere = []string{"kubeconfig", "context", "cluster", "server", "user", "token", "as", "as-group", "as-uid",
	"certificate-authority", "client-certificate", "client-key", "tls-server-name", "insecure-skip-tls-verify",
	"username", "password", "proxy-url"}

// local are the flags that read or write files on the machine the agent runs on.
var local = []string{"filename", "kustomize", "output-directory", "cache-dir", "profile", "profile-output", "log-file", "log-dir"}

// separate are the flags that may stand before the verb with their value as the next argument.
var separate = map[string]bool{"-n": true, "--namespace": true, "-v": true, "--v": true, "--request-timeout": true}

// takesValue are the short flags that consume the rest of their group everywhere kubectl defines them.
const takesValue = "nolLvc"

// verbIndex is where the verb stands: the first argument that is neither a flag nor the value of one
// of the few flags that may stand in front of it. -1 if there is none.
func verbIndex(head []string) int {
	for i := 0; i < len(head); i++ {
		switch {
		case separate[head[i]]:
			i++
		case !strings.HasPrefix(head[i], "-"):
			return i
		}
	}
	return -1
}

// Verb is the verb of a kubectl command line, found as Kubectl finds it, or "" if it has none.
func Verb(args []string) string {
	for i, a := range args {
		if a == "--" {
			args = args[:i]
			break
		}
	}
	if at := verbIndex(args); at >= 0 {
		return args[at]
	}
	return ""
}

// NotOfferedEnv names verbs a run does not offer its agent though the guard would let them through,
// separated by spaces. Whoever starts the agent sets it; the agent's kubectl refuses them. It is
// here, and not in a list of commands the agent's own harness matches as text, because here a
// command is read as kubectl reads it: `kubectl -n shop auth can-i get pods` is an `auth`, wherever
// its namespace stands and whatever word comes after.
const NotOfferedEnv = "LAPILLI_KUBECTL_NOT_OFFERED"

// NotOffered refuses a command whose verb is one of those named, and lets any other be.
func NotOffered(args []string, named string) error {
	verb := Verb(args)
	for _, withheld := range strings.Fields(named) {
		if verb == withheld {
			return refuse("`kubectl %s` is not offered in this run", verb)
		}
	}
	return nil
}

func refuse(format string, a ...any) error {
	return errors.New("kubectl refused by lapilli-case: " + fmt.Sprintf(format, a...))
}

func long(arg string, names []string) (string, bool) {
	if !strings.HasPrefix(arg, "--") {
		return "", false
	}
	name, _, _ := strings.Cut(arg[2:], "=")
	for _, n := range names {
		if name == n {
			return n, true
		}
	}
	return "", false
}

// before reports whether a flag may stand in front of the verb, and whether its value is the next
// argument. Only kubectl's own flags may: those it reads the same way whatever the command. Any other
// flag there is one kubectl does not know yet, and it takes the next word for its value — so in
// `kubectl -l version delete pods` the command is delete, and "version" is a label.
func before(arg string) (ok, valueFollows bool) {
	if separate[arg] {
		return true, true
	}
	if arg == "-h" || arg == "--help" {
		return true, false
	}
	for name := range separate {
		if strings.HasPrefix(name, "--") && strings.HasPrefix(arg, name+"=") { // --namespace=shop
			return true, false
		}
		if !strings.HasPrefix(name, "--") && strings.HasPrefix(arg, name) && len(arg) > len(name) { // -nshop
			return true, false
		}
	}
	return false, false
}

// readsFile reports whether an output format takes its template from a file on this machine:
// go-template-file, templatefile, jsonpath-file, custom-columns-file. A template with nothing to fill
// in is printed as it stands, so `-o go-template-file=<any file>` is a way to read that file.
func readsFile(format string) bool {
	name, _, _ := strings.Cut(format, "=")
	return strings.HasSuffix(name, "file")
}

// Kubectl returns the arguments to run the real kubectl with, or the reason it is not run.
// kubeconfigEnv is the KUBECONFIG the caller has.
func Kubectl(args []string, kubeconfigEnv string, pin Pin) ([]string, error) {
	if kubeconfigEnv != pin.Kubeconfig {
		return nil, refuse("KUBECONFIG is not the case's")
	}
	head, tail, dashed := args, []string(nil), false
	for i, a := range args {
		if a == "--" {
			head, tail, dashed = args[:i], args[i+1:], true
			break
		}
	}

	verbAt := verbIndex(head)
	verb := ""
	if verbAt >= 0 {
		verb = head[verbAt]
	}

	// Flags first: "--context would point kubectl away" is a better thing to be told than that the
	// context's name is not a verb.
	for i, a := range head {
		next := ""
		if i+1 < len(head) {
			next = head[i+1]
		}
		if name, ok := long(a, elsewhere); ok {
			return nil, refuse("--%s would point kubectl away from the case", name)
		}
		if name, ok := long(a, local); ok {
			return nil, refuse("--%s reads or writes files on this machine", name)
		}
		if _, ok := long(a, []string{"raw"}); ok && verb == "get" {
			return nil, refuse("--raw asks for a path, and a path can be a proxy to a node, a pod or a service: a request sent, not something read")
		}
		if format, ok := strings.CutPrefix(a, "--output="); ok && readsFile(format) || a == "--output" && readsFile(next) {
			return nil, refuse("that output format reads a file on this machine")
		}
		if len(a) < 2 || a[0] != '-' || a[1] == '-' {
			continue
		}
		// A group of short flags, read the way kubectl reads it: letter by letter, until one takes
		// the rest as its value. `-As https://…` is --all-namespaces and --server.
		for j := 1; j < len(a); j++ {
			ch := rune(a[j])
			if ch == 'o' { // -ojson, -o=json, -o json
				format := strings.TrimPrefix(a[j+1:], "=")
				if format == "" {
					format = next
				}
				if readsFile(format) {
					return nil, refuse("that output format reads a file on this machine")
				}
			}
			if ch == '=' || strings.ContainsRune(takesValue, ch) {
				break
			}
			switch {
			case ch == 's':
				return nil, refuse("-s in %s would point kubectl away from the case", a)
			case ch == 'k', ch == 'f' && verb != "logs": // `logs -f` follows; everywhere else -f names a file
				return nil, refuse("-%c in %s reads files on this machine", ch, a)
			}
		}
	}

	// What stands before the verb, and between a verb and the word that says what it does, is where
	// the command kubectl runs can differ from the one read here.
	for i := 0; i < len(head) && (verbAt < 0 || i < verbAt); i++ {
		ok, valueFollows := before(head[i])
		if !ok {
			return nil, refuse("%s stands before the verb, where kubectl would take the next word for its value. Put the verb first", head[i])
		}
		if valueFollows {
			i++
		}
	}
	sub := ""
	if verbAt >= 0 && verbAt+1 < len(head) {
		sub = head[verbAt+1]
	}
	if verbAt >= 0 { // whatever stands where the verb stands — an empty word too — is a read, or the command is refused
		allowed, ok := readOnly[verb]
		if !ok {
			return nil, refuse("%q is not a read-only verb (%s)", verb, strings.Join(verbs(), ", "))
		}
		if allowed != nil && !contains(allowed, sub) {
			return nil, refuse("of `kubectl %s`, only %s are allowed, as the word that follows it", verb, strings.Join(allowed, ", "))
		}
	}

	out := append([]string{}, head...)
	out = append(out, "--kubeconfig="+pin.Kubeconfig, "--context="+pin.Context, "--cluster="+pin.Cluster, "--server="+pin.Server)
	if pin.User != "" {
		out = append(out, "--user="+pin.User)
	}
	// `rollout status` waits for a rollout to finish, and in an incident — live or frozen — the one
	// being asked about is the one that will not. It is told to say where the rollout stands and
	// return; given last, so that it holds whatever the caller wrote. Seen once: an agent still
	// inside this command six minutes on.
	if verb == "rollout" && sub == "status" {
		out = append(out, "--watch=false")
	}
	if dashed {
		out = append(append(out, "--"), tail...)
	}
	return out, nil
}

// Environ removes what could redirect kubectl without an argument.
func Environ(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "KUBERNETES_") || (strings.HasPrefix(name, "KUBECTL_") && name != "KUBECTL_COMMAND_HEADERS") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func verbs() []string {
	out := make([]string, 0, len(readOnly))
	for v := range readOnly {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// ReadCommands lists what the guard lets through, as the words that follow `kubectl`: "get", "rollout
// history". An agent's own harness that keeps a list of what it may run builds it from this, so that
// it does not refuse a question the guard would have allowed.
func ReadCommands() []string {
	var out []string
	for verb, subs := range readOnly {
		if subs == nil {
			out = append(out, verb)
		}
		for _, sub := range subs {
			out = append(out, verb+" "+sub)
		}
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
