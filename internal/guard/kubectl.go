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
//     identity, flags that read or write local files. A refusal tells the agent why, so it can go on.
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

	// The verb, and the word after it: the first arguments that are neither flags nor the values of
	// the few flags that may stand in front of them.
	var words []string
	for i := 0; i < len(head); i++ {
		switch {
		case separate[head[i]]:
			i++
		case !strings.HasPrefix(head[i], "-"):
			words = append(words, head[i])
		}
	}
	verb := ""
	if len(words) > 0 {
		verb = words[0]
	}

	// Flags first: "--context would point kubectl away" is a better thing to be told than that the
	// context's name is not a verb.
	for _, a := range head {
		if name, ok := long(a, elsewhere); ok {
			return nil, refuse("--%s would point kubectl away from the case", name)
		}
		if name, ok := long(a, local); ok {
			return nil, refuse("--%s reads or writes files on this machine", name)
		}
		if len(a) < 2 || a[0] != '-' || a[1] == '-' {
			continue
		}
		// A group of short flags, read the way kubectl reads it: letter by letter, until one takes
		// the rest as its value. `-As https://…` is --all-namespaces and --server.
		for _, ch := range a[1:] {
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

	if verb != "" {
		allowed, ok := readOnly[verb]
		if !ok {
			return nil, refuse("%q is not a read-only verb (%s). If it is the value of a flag, put the verb first", verb, strings.Join(verbs(), ", "))
		}
		if allowed != nil && (len(words) < 2 || !contains(allowed, words[1])) {
			return nil, refuse("of `kubectl %s`, only %s are allowed", verb, strings.Join(allowed, ", "))
		}
	}

	out := append([]string{}, head...)
	out = append(out, "--kubeconfig="+pin.Kubeconfig, "--context="+pin.Context, "--cluster="+pin.Cluster, "--server="+pin.Server)
	if pin.User != "" {
		out = append(out, "--user="+pin.User)
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

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
