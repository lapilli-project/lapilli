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
	"sort"
	"strings"
	"time"

	"github.com/lapilli-project/lapilli/internal/casefile"
	"github.com/lapilli-project/lapilli/internal/guard"
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
//
// A witness is run with the session's environment and nothing of the caller's: the case's
// kubeconfig, its metrics endpoint if it has one and none if it has none, an empty home, and every
// verb withheld that some agent is not offered. The caller's shell may name a Prometheus of its own,
// and a witness that reached that one would have reached nothing of the case.

// Reached is one evidence item of a served case, and whether its witness reaches it.
type Reached struct {
	Evidence casefile.Evidence
	// By is the witness as it was run: the command, or `promq` and the query. Empty if the item
	// names none.
	By string
	// Found says the witness ended well and the pattern is in what it printed.
	Found bool
	// Said is the end of what it printed, what it complained of and how it ended, when the item was
	// not found: for the one who has to find out why.
	Said string
}

// reachTimeout is how long one witness may take. A command against a frozen case answers in less
// than a second; one that has not ended in this long — one that follows a log, or watches — is
// stopped, and has reached nothing, whatever it printed first.
var reachTimeout = 60 * time.Second

// reachMost is how much of what a witness prints is kept to look in. What an agent is to read of a
// command is a few screens; a witness that prints more than this is no witness.
const reachMost = 64 << 20

// Reach runs the witness of every evidence item of the case being served, in the order the answer
// key has them. notOffered is the kubectl verbs to withhold, as a run withholds them from its agent
// (guard.NotOfferedEnv).
func (s *Session) Reach(ctx context.Context, notOffered []string) ([]Reached, error) {
	home := filepath.Join(s.Workdir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	env := []string{"HOME=" + home, guard.NotOfferedEnv + "=" + strings.Join(notOffered, " ")}
	names := make([]string, 0, len(s.Env))
	for name := range s.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names { // the kubeconfig, the PATH with the tools first on it, the metrics endpoint if there is one
		env = append(env, name+"="+s.Env[name])
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
			tool, r.By = "kubectl", strings.TrimSpace(e.Command)
		case e.Store == casefile.StoreMetrics && e.Query != "":
			tool, args, r.By = "promq", []string{e.Query}, "promq "+shellQuote(e.Query)
		default:
			out = append(out, r)
			continue
		}
		one, cancel := context.WithTimeout(ctx, reachTimeout)
		cmd := exec.CommandContext(one, filepath.Join(s.Workdir, "bin", tool), args...)
		printed, complained := &most{left: reachMost}, &most{left: 1 << 20}
		cmd.Env, cmd.Dir, cmd.Stdout, cmd.Stderr = env, home, printed, complained
		runErr := cmd.Run()
		late := one.Err() != nil && ctx.Err() == nil
		cancel()
		var exit *exec.ExitError
		if runErr != nil && !errors.As(runErr, &exit) { // the tool itself is not there to run: that is the session's fault, not the item's
			return nil, fmt.Errorf("running %s: %w", r.By, runErr)
		}
		// A witness reaches its item if it ended well and the item is in what it printed. One that
		// failed has printed, if anything, why — which may well repeat what it was asked.
		if r.Found = runErr == nil && !printed.over && rx.Match(printed.Bytes()); !r.Found {
			said := []string{lastOf(printed.String(), 600), lastOf(complained.String(), 600)}
			switch {
			case late:
				said = append(said, fmt.Sprintf("(it had not ended after %s, and was stopped)", reachTimeout))
			case runErr != nil:
				said = append(said, "("+runErr.Error()+")")
			case printed.over:
				said = append(said, fmt.Sprintf("(it printed more than %d MiB, and no more was read)", reachMost>>20))
			}
			if r.Said = strings.TrimSpace(strings.Join(said, "\n")); r.Said == "" {
				r.Said = "(it printed nothing)"
			}
			for strings.Contains(r.Said, "\n\n") {
				r.Said = strings.ReplaceAll(r.Said, "\n\n", "\n")
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// most keeps the first bytes written to it, up to a number, and says if there were more.
type most struct {
	bytes.Buffer
	left int
	over bool
}

func (m *most) Write(p []byte) (int, error) {
	if len(p) > m.left {
		m.Buffer.Write(p[:m.left])
		m.left, m.over = 0, true
		return len(p), nil
	}
	m.left -= len(p)
	return m.Buffer.Write(p)
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
