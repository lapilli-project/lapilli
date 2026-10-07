package freeze

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/lapilli-project/lapilli/internal/guard"
)

// The collector takes the log of each of a pod's containers and of none of its init containers. A
// migration that failed says why in an init container's log, and a pod stuck in Init:CrashLoopBackOff
// has no other log to read: without it the case has the symptom and not the evidence. So after the
// collector has run, the logs it left out are fetched and put where it would have put them, each line
// with the kubelet's time on it, as it writes its own.

// leftOut is one log the collector did not take.
type leftOut struct {
	Namespace, Pod, Container string
	Previous                  bool // the log of the run before this one
	path                      string
}

func (l leftOut) String() string {
	if l.Previous {
		return l.Namespace + "/" + l.Pod + "/" + l.Container + " (previous)"
	}
	return l.Namespace + "/" + l.Pod + "/" + l.Container
}

// logTimeout is how long one log may take to come. A pod on a node that no longer answers costs the
// API server its own timeout to say so, and a freeze should not wait on each of them for ever.
const logTimeout = 2 * time.Minute

// logsLeftOut reads the pods of a snapshot and lists the logs that should be in it and are not: of
// every init and ephemeral container that has run, and of its previous run if it has had one.
//
// A pod is taken from where the collector filed it — namespaces/<namespace>/v1/pod/<name>.yaml — and
// only if the file says the same: what is read here goes into a path and onto a command line.
func logsLeftOut(snapshot string) ([]leftOut, error) {
	namespaces, err := os.ReadDir(filepath.Join(snapshot, "namespaces"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []leftOut
	for _, ns := range namespaces {
		dir := filepath.Join(snapshot, "namespaces", ns.Name(), "v1", "pod")
		files, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) || !ns.IsDir() {
			continue
		} else if err != nil {
			return nil, err
		}
		for _, file := range files {
			name, isYAML := strings.CutSuffix(file.Name(), ".yaml")
			if !isYAML || file.IsDir() || !plainName(name) || !plainName(ns.Name()) {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, file.Name()))
			if err != nil {
				return nil, err
			}
			type status struct {
				Name      string `yaml:"name"`
				State     map[string]any
				LastState map[string]any `yaml:"lastState"`
			}
			var pod struct {
				Metadata struct{ Name, Namespace string }
				Status   struct {
					Init      []status `yaml:"initContainerStatuses"`
					Ephemeral []status `yaml:"ephemeralContainerStatuses"`
				}
			}
			if yaml.Unmarshal(raw, &pod) != nil || pod.Metadata.Name != name || pod.Metadata.Namespace != ns.Name() {
				continue // not the pod the collector filed here: nothing to say about its logs
			}
			for _, c := range append(pod.Status.Init, pod.Status.Ephemeral...) {
				if !plainName(c.Name) {
					continue
				}
				ran := c.State["running"] != nil || c.State["terminated"] != nil || c.LastState["terminated"] != nil
				for _, log := range []struct {
					name   string
					wanted bool
				}{{"current.log", ran}, {"previous.log", c.LastState["terminated"] != nil}} {
					path := filepath.Join(dir, name, c.Name, log.name)
					if _, err := os.Stat(path); log.wanted && err != nil { // not there, or not to be had: asked for, and if it cannot be written that is said
						out = append(out, leftOut{ns.Name(), name, c.Name, log.name == "previous.log", path})
					}
				}
			}
		}
	}
	return out, nil
}

// plainName says a name is one the API would have accepted: it goes into a path and onto a command
// line, and what is read here is a file.
func plainName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for i, r := range name {
		letter := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if !letter && (i == 0 || r != '-' && r != '.') {
			return false
		}
	}
	return !strings.Contains(name, "..")
}

// errRefused is a kubectl that would not run the command at all, which no other log will fare
// better with: the freeze stops.
var errRefused = errors.New("refused")

// takeLogsLeftOut fetches them, and returns the ones it could not get. A log that cannot be fetched
// is left out still — the kubelet may have thrown it away, and a case with one log fewer is a case —
// but it is not left out in silence.
func takeLogsLeftOut(ctx context.Context, snapshot string, fetch func(context.Context, leftOut, io.Writer) error) (taken int, missing []string, err error) {
	wanted, err := logsLeftOut(snapshot)
	if err != nil {
		return 0, nil, err
	}
	for _, l := range wanted {
		err := func() error {
			if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
				return err
			}
			to, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return err
			}
			within, cancel := context.WithTimeout(ctx, logTimeout)
			defer cancel()
			err = fetch(within, l, to)
			if cerr := to.Close(); err == nil {
				err = cerr
			}
			if err != nil { // half a log is not a log, and a directory for none is not left behind
				os.Remove(l.path)
				os.Remove(filepath.Dir(l.path))
			}
			return err
		}()
		switch {
		case errors.Is(err, errRefused):
			return taken, missing, err
		case err != nil:
			missing = append(missing, l.String())
		default:
			taken++
		}
	}
	return taken, missing, nil
}

// logArgs is the command that fetches a log the way the collector's own are kept: with the kubelet's
// time on every line.
func logArgs(kubeconfig string, l leftOut) []string {
	args := []string{"--kubeconfig", kubeconfig, "logs", l.Pod, "-n", l.Namespace, "-c", l.Container, "--timestamps"}
	if l.Previous {
		args = append(args, "--previous")
	}
	return args
}

// kubectlLogs fetches a log with the kubectl on the path.
func kubectlLogs(kubeconfig string) (func(context.Context, leftOut, io.Writer) error, error) {
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		return nil, fmt.Errorf("kubectl is needed to take the logs of init containers, which the collector leaves out: %w", err)
	}
	return func(ctx context.Context, l leftOut, to io.Writer) error {
		var said bytes.Buffer
		cmd := exec.CommandContext(ctx, kubectl, logArgs(kubeconfig, l)...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
		cmd.Stdout, cmd.Stderr = to, &said
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == guard.ExitRefused {
			// The first kubectl on the path is the guard of a case that is being served from this
			// shell: it lets through reads of that case and nothing else.
			return fmt.Errorf("%w: the kubectl on the path (%s) is a served case's guard, and it will not read the cluster being frozen: %s. "+
				"Run freeze from a shell that does not have a case's bin directory on its PATH", errRefused, kubectl, strings.TrimSpace(said.String()))
		}
		return err
	}, nil
}
