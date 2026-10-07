package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/lapilli-project/lapilli/internal/guard"
)

var claudeSystem = "You are an SRE investigating an incident in a Kubernetes cluster. You have read-only access through the Bash tool: kubectl (" + strings.Join(claudeKubectl, ", ") + ") " +
	"and `promq '<PromQL>' [--range 30m] [--step 15s]` for metrics when a metrics endpoint exists. Plain commands only; pipes to grep, head, tail, sort, uniq, wc, cut and jq are allowed. " +
	"Do not ask questions. Investigate until you can state the root cause, then answer with: the root cause, the evidence that supports it, and what you ruled out."

// claudeKubectl is the kubectl Claude Code is offered: what the guard lets through, by the guard's
// own list, less what is not about the incident and is answered differently on a frozen case than on
// a cluster — the client's own configuration, who is asking, the API's documentation, the server's
// address (docs/design-case.md §8). Offering those would give the live condition answers the frozen
// one cannot give. It was a list of five verbs written by hand, and in round 38 it refused `kubectl
// rollout history` twelve times: a read the guard allows.
var claudeKubectl = func() []string {
	var out []string
	for _, c := range guard.ReadCommands() {
		switch verb, _, _ := strings.Cut(c, " "); verb {
		case "config", "auth", "explain", "cluster-info":
		default:
			out = append(out, c)
		}
	}
	return out
}()

// claudeAllowed is what Claude Code may run: that kubectl, promq, and text filters.
var claudeAllowed = func() []string {
	var out []string
	for _, c := range claudeKubectl {
		out = append(out, "Bash(kubectl "+c+")", "Bash(kubectl "+c+" *)")
	}
	return append(out, "Bash(promq *)", "Bash(grep *)", "Bash(head *)", "Bash(tail *)", "Bash(sort *)", "Bash(uniq *)", "Bash(wc *)", "Bash(cut *)", "Bash(jq *)")
}()

// claudeResult is the last record of Claude Code's stream-json output.
type claudeResult struct {
	Result       string   `json:"result"`
	NumTurns     *int64   `json:"num_turns"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
	Usage        struct {
		Input         int64 `json:"input_tokens"`
		Output        int64 `json:"output_tokens"`
		CacheRead     int64 `json:"cache_read_input_tokens"`
		CacheCreation int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// parseClaudeStream turns Claude Code's stream-json into ordered steps and the final result record.
// Lines that are not JSON, and records it does not know, are skipped: the format grows.
func parseClaudeStream(r io.Reader) ([]Step, claudeResult) {
	var (
		steps []Step
		index = map[string]int{} // tool_use id -> position in steps
		final claudeResult
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	for sc.Scan() {
		var event struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &event) != nil {
			continue
		}
		if event.Type == "result" {
			json.Unmarshal(sc.Bytes(), &final)
			continue
		}
		var blocks []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
			Input     struct {
				Command string `json:"command"`
			} `json:"input"`
		}
		if json.Unmarshal(event.Message.Content, &blocks) != nil { // a user message may be a plain string
			continue
		}
		for _, b := range blocks {
			switch {
			case event.Type == "assistant" && b.Type == "tool_use":
				index[b.ID] = len(steps)
				steps = append(steps, Step{Tool: "bash", Input: b.Input.Command})
			case event.Type == "user" && b.Type == "tool_result":
				if i, ok := index[b.ToolUseID]; ok {
					steps[i].Output, steps[i].Error = toolResultText(b.Content), b.IsError
				}
			}
		}
	}
	return steps, final
}

// toolResultText reads a tool result, which is either a string or a list of text blocks.
func toolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &parts)
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = p.Text
	}
	return strings.Join(texts, " ")
}

// ClaudeCode runs Claude Code headless with only the Bash tool, and only the commands in claudeAllowed:
// kubectl's read verbs, promq, and text filters to pipe them through. That list is Claude Code's to
// enforce, not ours. Tested with 2.1.291 in this mode: `head -1 /etc/hosts`, `grep -c x /etc/hosts`
// and `/usr/bin/head …` were all refused, so the filters do not read files outside the (empty) working
// directory — but that is its behaviour, observed once, and not a property this adapter guarantees.
// `sort -o` can write a file into that directory.
//
// It uses the logged-in session; to use a key instead, pass ANTHROPIC_API_KEY by name (--pass-env).
func ClaudeCode(ctx context.Context, prompt string, env map[string]string, workdir string, opt Options) (*Transcript, error) {
	model := opt.Model
	if model == "" {
		model = "haiku"
	}
	budget, timeout := opt.BudgetUSD, opt.Timeout
	if budget <= 0 {
		budget = 1.0
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := append([]string{"-p", prompt, "--model", model, "--tools", "Bash", "--allowedTools"}, claudeAllowed...)
	args = append(args, "--permission-mode", "dontAsk", "--setting-sources", "project", "--strict-mcp-config", "--no-session-persistence",
		"--system-prompt", claudeSystem, "--output-format", "stream-json", "--verbose", "--max-budget-usd", strconv.FormatFloat(budget, 'f', -1, 64))
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = workdir
	// The real home directory, because that is where Claude Code keeps its login; what it may run from
	// there is bounded by the allow-list above. No API key unless the operator passes one by name.
	cmd.Env = environment(env, os.Getenv("HOME"), opt.PassEnv)
	contain(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	started := time.Now()
	runErr := cmd.Run()
	steps, final := parseClaudeStream(&stdout)
	tokens := final.Usage.Input + final.Usage.Output + final.Usage.CacheRead + final.Usage.CacheCreation
	t := &Transcript{Agent: "claude-code", Model: model, Answer: final.Result, Steps: steps,
		Usage: Usage{LLMCalls: final.NumTurns, CostUSD: final.TotalCostUSD, Seconds: seconds(started)}}
	if final.NumTurns != nil || tokens > 0 { // a run that never reached its result record reported no usage
		t.Usage.Tokens = &tokens
	}
	if final.Result == "" {
		msg := tail(stderr.String(), 500)
		if msg == "" && runErr != nil {
			msg = runErr.Error()
		}
		if msg == "" {
			msg = "no result"
		}
		t.Error = &msg
	}
	return t, nil
}
