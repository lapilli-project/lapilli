package replay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lapilli-project/lapilli/internal/casefile"
)

// That an evidence item is in the frozen copy is checked when a case is frozen and again in CI: the
// pattern is looked for in the snapshot's files, or in what a query prints. That says the evidence
// exists. It does not say that anything an agent can run prints it — a snapshot holds files no
// command of a replay serves, and a replay prints what a cluster prints, which is not the file.
//
// So an item can name its witness: for the Kubernetes store one `kubectl` command, for the metrics
// store one query. Reach runs each through what an agent of a served case is handed — the kubectl
// on its PATH, which is the guard in front of the frozen API, and `promq` — and looks for the
// pattern in what comes out.

// Reached is one evidence item of a served case, and whether its witness reaches it.
type Reached struct {
	Evidence casefile.Evidence
	// By is the witness as it was run: the command, or `promq` and the query. Empty if the item
	// names none.
	By string
	// Found says the pattern is in what the witness printed.
	Found bool
	// Said is the end of what it printed, and why it stopped if it failed: for the one who has to
	// find out why an item is not reached.
	Said string
}

// reachTimeout is how long one witness may take. A command against a frozen case answers in less
// than a second; one that does not answer in this long reaches nothing.
const reachTimeout = 60 * time.Second

// Reach runs the witness of every evidence item of the case being served, in the order the answer
// key has them.
func (s *Session) Reach(ctx context.Context) ([]Reached, error) {
	env := os.Environ()
	for k, v := range s.Env { // the session's own, after and so over the caller's
		env = append(env, k+"="+v)
	}
	var out []Reached
	for _, e := range s.Case.Evidence {
		r := Reached{Evidence: e}
		rx, err := regexp.Compile(e.Pattern)
		if err != nil {
			return nil, err
		}
		var tool string
		var args []string
		switch {
		case e.Store == casefile.StoreKubernetes && e.Command != "":
			if args, err = e.KubectlArgs(); err != nil {
				return nil, err
			}
			tool, r.By = "kubectl", e.Command
		case e.Store == casefile.StoreMetrics && e.Query != "":
			tool, args, r.By = "promq", []string{e.Query}, "promq "+shellQuote(e.Query)
		default:
			out = append(out, r)
			continue
		}
		one, cancel := context.WithTimeout(ctx, reachTimeout)
		cmd := exec.CommandContext(one, filepath.Join(s.Workdir, "bin", tool), args...)
		var printed, complained bytes.Buffer
		cmd.Env, cmd.Stdout, cmd.Stderr = env, &printed, &complained
		runErr := cmd.Run()
		cancel()
		var exit *exec.ExitError
		if runErr != nil && !errors.As(runErr, &exit) { // it could not be run at all: that is the session's fault, not the item's
			return nil, fmt.Errorf("running %s: %w", r.By, runErr)
		}
		// What it printed is what an agent reads, whatever it exited with: the pattern is looked for there.
		if r.Found = rx.Match(printed.Bytes()); !r.Found {
			r.Said = lastOf(printed.String(), 600)
			if tail := lastOf(complained.String(), 600); tail != "" {
				r.Said = strings.TrimSpace(r.Said + "\n" + tail)
			}
			if runErr != nil {
				r.Said = strings.TrimSpace(r.Said + "\n(" + runErr.Error() + ")")
			}
			if r.Said == "" {
				r.Said = "(it printed nothing)"
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// lastOf is the end of a text, no longer than n bytes and beginning at a line.
func lastOf(text string, n int) string {
	text = strings.TrimSpace(text)
	if len(text) <= n {
		return text
	}
	text = text[len(text)-n:]
	if i := strings.IndexByte(text, '\n'); i >= 0 && i < len(text)-1 {
		text = text[i+1:]
	}
	return "… " + text
}
