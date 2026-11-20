// Package scenario runs the scenarios expectations hold rules to
// (docs/design/expectations.md, step 4): promtool's own test files, each
// run as promtool test rules runs it, against one group of rules as Lapilli
// has it.
//
// What a test file may make promtool read is Lapilli's to say, not the
// file's: the group is written to a file Lapilli makes, in a directory of
// its own, and the test file's rule_files is rewritten to name that file
// alone, so no test file can have promtool read another path (whose text
// would come back in what promtool says, and in a pull request's comment).
// promtool is executed directly, never through a shell, at the path the
// operator gave (--promtool), with fixed arguments, an emptied environment
// but for a TMPDIR inside that directory, a time limit, and a cap on what
// it says that is kept; in a process group of its own, killed whole at the
// time limit. promtool evaluates PromQL on its own storage in memory and
// runs nothing of the file's: running it is reading what a pull request
// holds, not running it. What it evaluates can still take memory without
// bound (an annotation's template expanded at each evaluation of an alert
// that fires), so on Linux a cap can be set on it (Memory): Lapilli starts
// itself again under a name of its own, which sets RLIMIT_DATA and executes
// promtool in its place with the same fixed arguments and environment
// (cap_linux.go).
package scenario

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/rules"
)

// DefaultTimeout is how long one run of promtool may take; DefaultOutput,
// how much of what it says is kept.
const (
	DefaultTimeout = 30 * time.Second
	DefaultOutput  = 64 << 10
)

// The files a run's directory holds: the test file, rewritten, and the
// group it tests. Fixed names, so that neither the test file's path nor
// the rule's name is ever an argument promtool reads as a flag.
const (
	testName  = "test.yaml"
	rulesName = "rules.yaml"
)

// Promtool is promtool as the operator gave it.
type Promtool struct {
	// Path is where promtool is, absolute (Resolve).
	Path string
	// Timeout is how long a run may take, DefaultTimeout when 0; Output,
	// how many bytes of what it says are kept, DefaultOutput when 0.
	Timeout time.Duration
	Output  int
	// Memory is how much memory a run may take, in bytes: promtool's
	// RLIMIT_DATA, where one can be set (CanCap); 0 for no cap. Past it,
	// promtool's allocations fail, it stops, and the run is unrun.
	Memory uint64
}

// MinMemory is the least Memory may be: promtool, a Go program, maps tens
// of MiB as it starts, before reading a test.
const MinMemory = 64 << 20

// The memory cap's wrapper: Lapilli started again under capName, told
// promtool's path and the cap by capPath and capMemory, which it alone
// reads; it exits capFailed, saying "lapilli: " and why, when it can't
// start promtool under the cap.
const (
	capName   = "lapilli-promtool-capped"
	capPath   = "LAPILLI_PROMTOOL_CAPPED_PATH"
	capMemory = "LAPILLI_PROMTOOL_CAPPED_MEMORY"
	capFailed = 125
)

// capped says a run is capped: a cap given, and one can be set here.
func (p Promtool) capped() bool { return p.Memory > 0 && CanCap }

// command is how a run in dir starts promtool: directly, at its path, with
// test rules test.yaml and TMPDIR alone; capped, through Lapilli started
// again under capName, which sets the cap and executes promtool in its
// place, with those alone. Either way in a process group of its own, which
// is killed whole when ctx ends.
func (p Promtool) command(ctx context.Context, dir, tmp string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, p.Path, "test", "rules", testName)
	cmd.Dir = dir
	// Nothing of Lapilli's own environment (its credentials' variables
	// among it): promtool's teststorage writes under TMPDIR, inside the
	// run's directory.
	cmd.Env = []string{"TMPDIR=" + tmp}
	if p.capped() {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		cmd.Path, cmd.Err = self, nil
		cmd.Args = []string{capName, "test", "rules", testName}
		cmd.Env = []string{"TMPDIR=" + tmp, capPath + "=" + p.Path, capMemory + "=" + strconv.FormatUint(p.Memory, 10)}
	}
	ownGroup(cmd)
	return cmd, nil
}

// Resolve finds promtool as --promtool names it: a path, or a name looked
// for on PATH, made absolute, so that a run in a directory of its own
// starts the same file. It must be a file that can be executed.
func Resolve(name string) (string, error) {
	if name == "" {
		return "", errors.New("give the path of promtool")
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	if p, err = filepath.Abs(p); err != nil {
		return "", err
	}
	fi, err := os.Stat(p)
	switch {
	case err != nil:
		return "", err
	case !fi.Mode().IsRegular():
		return "", fmt.Errorf("%s isn't a file", p)
	case fi.Mode()&0o111 == 0:
		return "", fmt.Errorf("%s can't be executed", p)
	}
	return p, nil
}

// The results of a run.
const (
	Passed = "passed"
	Failed = "failed"
	// Unrun is a run that couldn't tell either: it timed out, crashed,
	// couldn't start, or answered what promtool doesn't.
	Unrun = "unrun"
)

// Result is what a run came to: Passed, Failed or Unrun; Said, of a run
// that failed, what promtool said of it (the lines after FAILED:, the test
// file's and the rules' own words); Why, of one unrun, why, in Lapilli's
// words.
type Result struct {
	State string
	Said  string
	Why   string
}

// maxSaid is how much of what promtool said of a failure is given back:
// the first lines of it, as a person reads them.
const maxSaid = 2000

// Run runs promtool test rules on a test file, as Rewrite made it, against
// a group of rules, as Group wrote it: in a directory of its own, removed
// after, holding the two under fixed names, from which promtool is
// started directly with nothing in its environment but TMPDIR, there too;
// stopped at the time limit, or when ctx ends; what it says kept to the
// cap. It passed when it exits 0 saying SUCCESS; failed when it exits 1
// saying FAILED:, as promtool says a test that doesn't hold, a test file
// it can't read, or rules it can't load; else it is unrun: a time out, a
// crash, an exit of another status, or an answer promtool doesn't give,
// or, capped, more memory taken than it may.
func (p Promtool) Run(ctx context.Context, group, test []byte) Result {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	most := p.Output
	if most <= 0 {
		most = DefaultOutput
	}
	dir, err := os.MkdirTemp("", "lapilli-scenario-")
	if err != nil {
		return Result{State: Unrun, Why: "no directory to run it in"}
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return Result{State: Unrun, Why: "no directory to run it in"}
	}
	for name, b := range map[string][]byte{rulesName: group, testName: test} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return Result{State: Unrun, Why: "its files couldn't be written"}
		}
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, err := p.command(rctx, dir, tmp)
	if err != nil {
		return Result{State: Unrun, Why: "promtool couldn't be started"}
	}
	out := &capped{most: most}
	cmd.Stdout, cmd.Stderr = out, out
	// Once killed, what holds its output open is waited for this long at
	// most.
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	switch {
	case rctx.Err() != nil && ctx.Err() != nil:
		return Result{State: Unrun, Why: "stopped: the run's time ran out"}
	case rctx.Err() != nil:
		return Result{State: Unrun, Why: fmt.Sprintf("promtool took longer than %s", timeout)}
	}
	said := out.b.Bytes()
	var exit *exec.ExitError
	switch {
	case err == nil && bytes.Contains(said, []byte("  SUCCESS")) && !bytes.Contains(said, []byte("FAILED:")):
		return Result{State: Passed}
	case errors.As(err, &exit) && exit.ExitCode() == 1 && bytes.Contains(said, []byte("FAILED:")):
		return Result{State: Failed, Said: failure(said)}
	case p.capped() && errors.As(err, &exit) && exit.ExitCode() == capFailed && bytes.HasPrefix(said, []byte("lapilli: ")):
		return Result{State: Unrun, Why: "promtool couldn't be started"}
	case p.capped() && errors.As(err, &exit) && (bytes.Contains(said, []byte("out of memory")) || bytes.Contains(said, []byte("cannot allocate memory")) ||
		bytes.Contains(said, []byte("pthread_create failed"))):
		// Go's runtime says so as an allocation past RLIMIT_DATA fails;
		// built with cgo, as a thread's stack, 8 MiB each, can't be
		// mapped.
		return Result{State: Unrun, Why: fmt.Sprintf("promtool took more memory than it may (%d MiB)", p.Memory>>20)}
	case errors.As(err, &exit) && exit.ExitCode() < 0:
		return Result{State: Unrun, Why: "promtool crashed"}
	case errors.As(err, &exit):
		return Result{State: Unrun, Why: fmt.Sprintf("promtool exited %d, saying what it doesn't say of a test", exit.ExitCode())}
	case err != nil:
		return Result{State: Unrun, Why: "promtool couldn't be started"}
	}
	return Result{State: Unrun, Why: "promtool's answer isn't one it gives"}
}

// failure is what promtool said of a failure: the lines after FAILED:, as
// they show, maxSaid bytes of them at most.
func failure(said []byte) string {
	_, rest, _ := bytes.Cut(said, []byte("FAILED:"))
	var lines []string
	n := 0
	for _, l := range strings.Split(string(rest), "\n") {
		l = strings.TrimRight(l, " \t\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		l = rules.Shown(l)
		if n+len(l) > maxSaid {
			lines = append(lines, "…")
			break
		}
		n += len(l) + 1
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}

// capped keeps the first most bytes written to it, and takes the rest
// without keeping it: promtool may say megabytes of a failure.
type capped struct {
	b    bytes.Buffer
	most int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.most - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
