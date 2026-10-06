// Package agent runs an agent on a prompt inside a prepared environment and brings back what it did in
// one shape, the Transcript. Graders read only that shape, so a new agent needs an adapter and nothing
// else.
package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Step is one tool call: what the agent asked for and what came back.
type Step struct {
	Tool   string `json:"tool"`
	Input  string `json:"input"`
	Output string `json:"output"`
	Error  bool   `json:"error"`
}

// Usage is what the run cost. A field the agent does not report stays nil rather than zero.
type Usage struct {
	LLMCalls *int64   `json:"llm_calls"`
	Tokens   *int64   `json:"tokens"`
	CostUSD  *float64 `json:"cost_usd"`
	Seconds  float64  `json:"seconds"`
}

// Transcript is an investigation: the final answer and every step that led to it, in order.
type Transcript struct {
	Agent  string  `json:"agent"`
	Model  string  `json:"model"`
	Answer string  `json:"answer"`
	Steps  []Step  `json:"steps"`
	Usage  Usage   `json:"usage"`
	Error  *string `json:"error"` // set when the run produced no answer
}

// Options are the knobs an adapter may honour.
type Options struct {
	Model       string
	Command     string        // the command adapter's command line
	Temperature string        // some models accept only their default
	BudgetUSD   float64       // a ceiling per run, where the agent supports one
	Timeout     time.Duration // per run
	// PassEnv names variables of the operator's environment the agent may see: the key for its
	// model, most often. Nothing that looks like a credential is passed unless it is named here.
	PassEnv []string
}

// Runner runs one investigation. env is what the case adds to the agent's environment (KUBECONFIG,
// PATH, PROM_URL); workdir is an empty directory, so the agent has nothing to stumble on.
type Runner func(ctx context.Context, prompt string, env map[string]string, workdir string, opt Options) (*Transcript, error)

var adapters = map[string]Runner{"claude-code": ClaudeCode, "holmes": Holmes, "command": Command}

// Names lists the available adapters.
func Names() []string {
	out := make([]string, 0, len(adapters))
	for n := range adapters {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Run dispatches to an adapter.
func Run(ctx context.Context, name, prompt string, env map[string]string, workdir string, opt Options) (*Transcript, error) {
	run, ok := adapters[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q; available: %s", name, strings.Join(Names(), ", "))
	}
	t, err := run(ctx, prompt, env, workdir, opt)
	if err == nil && t.Steps == nil {
		t.Steps = []Step{} // a run that never called a tool has no steps, which is not the same as no record of them
	}
	return t, err
}

// harmless are the variables every agent gets from the operator's environment: locale, terminal,
// time zone, temporary directory, who is running, and how to reach the network.
var harmless = []string{"LANG", "LC_ALL", "LC_CTYPE", "TERM", "TZ", "TMPDIR", "USER", "LOGNAME", "SHELL",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}

// environment is what an agent runs with, and it is built rather than inherited.
//
// The machine that runs an evaluation holds more than a kubeconfig: cloud credentials, tokens for the
// observability backends an agent has toolsets for, a home directory with a logged-in CLI for each.
// The kubectl guard covers kubectl. This covers the rest: the agent sees the harmless variables, the
// ones the operator named, what the case adds, and the home directory it is given — an empty one,
// unless the adapter needs the real one or the operator passes HOME.
func environment(env map[string]string, home string, pass []string) []string {
	vars := map[string]string{}
	for _, name := range append(append([]string{}, harmless...), pass...) {
		if v, ok := os.LookupEnv(name); ok {
			vars[name] = v
		}
	}
	if _, passed := vars["HOME"]; !passed {
		vars["HOME"] = home
	}
	for k, v := range env {
		vars[k] = v
	}
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// emptyHome makes a home directory with nothing in it, and returns how to remove it again.
func emptyHome() (string, func(), error) {
	dir, err := os.MkdirTemp("", "lapilli-agent-home-")
	return dir, func() { os.RemoveAll(dir) }, err
}

// contain makes a time limit mean something: the agent runs in its own process group, and when the
// limit passes the whole group is killed — the shell, the agent, and the kubectl it left running.
func contain(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func seconds(since time.Time) float64 {
	return float64(time.Since(since).Round(100*time.Millisecond)) / float64(time.Second)
}

func failure(msg string) *string { return &msg }
