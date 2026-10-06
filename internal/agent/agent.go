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
}

// Runner runs one investigation. env is laid over the process environment; workdir is an empty
// directory, so the agent has nothing to stumble on.
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
	return run(ctx, prompt, env, workdir, opt)
}

// environ lays env over the process environment and removes the names in drop.
func environ(env map[string]string, drop ...string) []string {
	skip := map[string]bool{}
	for k := range env {
		skip[k] = true
	}
	for _, k := range drop {
		skip[k] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); !skip[name] {
			out = append(out, kv)
		}
	}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
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
