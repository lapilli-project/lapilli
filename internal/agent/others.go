package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Environment variables the command adapter hands to its command.
const (
	EnvPrompt     = "LAPILLI_CASE_PROMPT"
	EnvTranscript = "LAPILLI_CASE_TRANSCRIPT"
	EnvModel      = "LAPILLI_CASE_MODEL"
)

// Command runs any agent that can be started from a command line. The command is given to /bin/sh. It
// receives the prompt in $LAPILLI_CASE_PROMPT and must write a Transcript as JSON to the path in
// $LAPILLI_CASE_TRANSCRIPT. KUBECONFIG, PATH and PROM_URL are already set for the case; HOME is an
// empty directory and the rest of the operator's environment is absent unless passed by name.
func Command(ctx context.Context, prompt string, env map[string]string, workdir string, opt Options) (*Transcript, error) {
	if opt.Command == "" {
		return nil, errors.New("the command agent needs --agent-command")
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	home, remove, err := emptyHome()
	if err != nil {
		return nil, err
	}
	defer remove()
	out := filepath.Join(home, "transcript.json")
	full := map[string]string{EnvPrompt: prompt, EnvTranscript: out, EnvModel: opt.Model}
	for k, v := range env {
		full[k] = v
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", opt.Command)
	cmd.Dir, cmd.Env = workdir, environment(full, home, opt.PassEnv)
	contain(cmd)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	started := time.Now()
	cmd.Run()

	t := &Transcript{Agent: "command", Model: opt.Model}
	raw, err := os.ReadFile(out)
	switch {
	case err != nil:
		msg := tail(output.String(), 500)
		if msg == "" {
			msg = "no transcript written"
		}
		t.Error = &msg
	case json.Unmarshal(raw, t) != nil:
		t.Error = failure("the transcript is not valid JSON in the documented shape")
	}
	if t.Agent == "" {
		t.Agent = "command"
	}
	t.Usage.Seconds = seconds(started)
	return t, nil
}

// Holmes runs HolmesGPT (`holmes ask`). Its own bash allow-list is already read-only. It has toolsets
// for far more than Kubernetes — cloud CLIs, observability backends — and switches them on when it
// finds their credentials, which is the reason an agent's environment is built and its home is empty:
// the key for its model is passed by name (--pass-env OPENAI_API_KEY), and nothing else is there.
func Holmes(ctx context.Context, prompt string, env map[string]string, workdir string, opt Options) (*Transcript, error) {
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	home, remove, err := emptyHome()
	if err != nil {
		return nil, err
	}
	defer remove()
	out := filepath.Join(home, "holmes.json")
	args := []string{"ask", prompt, "--no-interactive", "--json-output-file", out}
	if opt.Model != "" {
		args = append(args, "--model", opt.Model)
	}
	full := map[string]string{}
	for k, v := range env {
		full[k] = v
	}
	if opt.Temperature != "" {
		full["TEMPERATURE"] = opt.Temperature
	}
	if url := env["PROM_URL"]; url != "" {
		cfg, err := yaml.Marshal(map[string]any{"toolsets": map[string]any{"prometheus/metrics": map[string]any{"enabled": true, "config": map[string]any{"prometheus_url": url}}}})
		if err != nil {
			return nil, err
		}
		path := filepath.Join(home, "holmes-config.yaml")
		if err := os.WriteFile(path, cfg, 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--config", path)
	}
	cmd := exec.CommandContext(ctx, "holmes", args...)
	cmd.Dir, cmd.Env = workdir, environment(full, home, opt.PassEnv)
	contain(cmd)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	started := time.Now()
	cmd.Run()

	raw, _ := os.ReadFile(out)
	t := parseHolmes(raw)
	t.Model, t.Usage.Seconds = opt.Model, seconds(started)
	if t.Answer == "" {
		msg := tail(output.String(), 500)
		if msg == "" {
			msg = "no result"
		}
		t.Error = &msg
	}
	return t, nil
}

// parseHolmes reads the file `holmes ask --json-output-file` writes.
//
// A tool result there is an envelope — status, error, the command that ran, and `data`, the text the
// model was actually shown. The step's output is that text, unwrapped. Keeping the envelope instead
// would store the text JSON-encoded inside JSON, where `"client":"x"` reads `\"client\":\"x\"`, and an
// evidence pattern written for what a tool prints would miss it for this one agent and no other.
func parseHolmes(raw []byte) *Transcript {
	var data struct {
		Result    string   `json:"result"`
		LLMCalls  *int64   `json:"num_llm_calls"`
		Tokens    *int64   `json:"total_tokens"`
		Cost      *float64 `json:"total_cost"`
		ToolCalls []struct {
			Name        string          `json:"tool_name"`
			Description string          `json:"description"`
			Result      json.RawMessage `json:"result"`
		} `json:"tool_calls"`
	}
	json.Unmarshal(raw, &data)
	t := &Transcript{Agent: "holmes", Answer: data.Result, Usage: Usage{LLMCalls: data.LLMCalls, Tokens: data.Tokens, CostUSD: data.Cost}}
	for _, c := range data.ToolCalls {
		if c.Name == "TodoWrite" { // the agent's own notes, not an observation
			continue
		}
		step := Step{Tool: c.Name, Input: c.Description, Output: string(c.Result)}
		var text string
		var envelope struct {
			Status     string          `json:"status"`
			Error      string          `json:"error"`
			Data       json.RawMessage `json:"data"`
			Invocation string          `json:"invocation"`
			Params     json.RawMessage `json:"params"`
		}
		if json.Unmarshal(c.Result, &text) == nil {
			step.Output = text
		} else if json.Unmarshal(c.Result, &envelope) == nil && (envelope.Status != "" || len(envelope.Data) > 0) {
			step.Error = envelope.Status == "error"
			step.Output = ""
			if json.Unmarshal(envelope.Data, &text) == nil {
				step.Output = text
			} else if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
				step.Output = string(envelope.Data)
			}
			if envelope.Error != "" && !strings.Contains(step.Output, envelope.Error) {
				step.Output = strings.TrimSpace(step.Output + "\n" + envelope.Error)
			}
			// What was asked, beside the tool's description of it: the command line for a shell tool,
			// the parameters for the others.
			switch {
			case envelope.Invocation != "" && !strings.HasPrefix(envelope.Invocation, "#!"):
				step.Input += "\n" + envelope.Invocation
			case len(envelope.Params) > 0 && string(envelope.Params) != "null":
				step.Input += "\n" + string(envelope.Params)
			}
		}
		t.Steps = append(t.Steps, step)
	}
	return t
}
