package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseClaudeStreamPairsEachCallWithItsResult(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`not json at all`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"looking"},{"type":"tool_use","id":"a","input":{"command":"kubectl get pods -A"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"b","input":{"command":"kubectl logs x"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b","content":[{"type":"text","text":"line one"},{"type":"text","text":"line two"}],"is_error":true}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"NAME READY"}]}}`,
		`{"type":"user","message":{"content":"a plain string, which is also allowed"}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"nobody","content":"orphan"}]}}`,
		`{"type":"result","result":"the cache is full","num_turns":5,"total_cost_usd":0.12,"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":300,"cache_creation_input_tokens":4000}}`,
	}, "\n")
	steps, final := parseClaudeStream(strings.NewReader(stream))
	want := []Step{
		{Tool: "bash", Input: "kubectl get pods -A", Output: "NAME READY"},
		{Tool: "bash", Input: "kubectl logs x", Output: "line one line two", Error: true},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Errorf("steps\n got %+v\nwant %+v", steps, want)
	}
	if final.Result != "the cache is full" || *final.NumTurns != 5 || *final.TotalCostUSD != 0.12 || final.Usage.CacheCreation != 4000 {
		t.Errorf("final = %+v", final)
	}
}

// The shapes are HolmesGPT 0.42.0's, taken from a real run.
func TestParseHolmesUnwrapsWhatTheModelWasShown(t *testing.T) {
	out := parseHolmes([]byte(`{"result":"answer","num_llm_calls":3,"total_tokens":1200,"total_cost":0.5,"tool_calls":[
		{"tool_name":"TodoWrite","description":"plan","result":"x"},
		{"tool_name":"legacy","description":"a plain string result","result":"plain text"},
		{"tool_name":"execute_prometheus_range_query","description":"Prometheus: Query (rate)","result":{"schema_version":"robusta:v1.0.0","status":"success","error":null,
			"data":"{\"status\":\"success\",\"data\":{\"result\":[{\"metric\":{\"client\":\"catalog-indexer\",\"code\":\"200\"},\"values\":[[1791299926.999,\"0.91\"]]}]}}",
			"invocation":null,"params":{"query":"rate(thumb_requests_total[5m])"}}},
		{"tool_name":"bash","description":"kubectl logs x --until=y","result":{"status":"error","error":"Error: Command failed: unknown flag: --until",
			"data":"kubectl logs x --until=y\nError: unknown flag: --until","invocation":"kubectl logs x --until=y","params":{"command":"kubectl logs x --until=y"}}},
		{"tool_name":"fetch_pod_logs","description":"Fetch Logs (pod=x)","result":{"status":"no_data","error":null,"data":null,"invocation":null,"params":null}},
		{"tool_name":"kubernetes_tabular_query","description":"kubectl get pods","result":{"status":"success","data":"NAME READY","invocation":"#!/bin/bash\n# a generated wrapper script","params":{"kind":"pods"}}}]}`))
	if out.Answer != "answer" || *out.Usage.LLMCalls != 3 || *out.Usage.Tokens != 1200 || *out.Usage.CostUSD != 0.5 || len(out.Steps) != 5 {
		t.Fatalf("%+v", out)
	}
	want := []Step{
		{Tool: "legacy", Input: "a plain string result", Output: "plain text"},
		// The model saw JSON; the step holds that JSON once, not JSON inside JSON.
		{Tool: "execute_prometheus_range_query", Input: "Prometheus: Query (rate)\n" + `{"query":"rate(thumb_requests_total[5m])"}`,
			Output: `{"status":"success","data":{"result":[{"metric":{"client":"catalog-indexer","code":"200"},"values":[[1791299926.999,"0.91"]]}]}}`},
		{Tool: "bash", Input: "kubectl logs x --until=y\nkubectl logs x --until=y", Error: true,
			Output: "kubectl logs x --until=y\nError: unknown flag: --until\nError: Command failed: unknown flag: --until"},
		{Tool: "fetch_pod_logs", Input: "Fetch Logs (pod=x)"},
		{Tool: "kubernetes_tabular_query", Input: "kubectl get pods\n" + `{"kind":"pods"}`, Output: "NAME READY"},
	}
	if !reflect.DeepEqual(out.Steps, want) {
		for i := range want {
			if i < len(out.Steps) && !reflect.DeepEqual(out.Steps[i], want[i]) {
				t.Errorf("step %d\n got %+v\nwant %+v", i, out.Steps[i], want[i])
			}
		}
	}
	if empty := parseHolmes(nil); empty.Answer != "" || len(empty.Steps) != 0 {
		t.Errorf("a missing output file gave %+v", empty)
	}
}

func TestCommandAdapterHandsOverThePromptAndReadsTheTranscript(t *testing.T) {
	env := map[string]string{"KUBECONFIG": "/case/kubeconfig"}
	script := `printf '{"model":"m1","answer":"%s via %s","steps":[{"tool":"bash","input":"kubectl get pods","output":"ok","error":false}],"usage":{"tokens":7}}' "$` + EnvPrompt + `" "$KUBECONFIG" > "$` + EnvTranscript + `"`
	got, err := Command(context.Background(), "why", env, t.TempDir(), Options{Command: script, Model: "asked"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "command" || got.Model != "m1" || got.Answer != "why via /case/kubeconfig" || len(got.Steps) != 1 || *got.Usage.Tokens != 7 || got.Error != nil {
		t.Errorf("%+v", got)
	}

	silent, err := Command(context.Background(), "why", env, t.TempDir(), Options{Command: "echo I wrote nothing; exit 3"})
	if err != nil || silent.Error == nil || !strings.Contains(*silent.Error, "I wrote nothing") || silent.Answer != "" {
		t.Errorf("a command that wrote no transcript gave %+v, %v", silent, err)
	}
	if _, err := Command(context.Background(), "why", env, t.TempDir(), Options{}); err == nil {
		t.Error("the command agent ran without a command")
	}

	started := time.Now()
	// The sleep is a grandchild holding the output pipe, which is what a real agent's kubectl would be.
	slow, err := Command(context.Background(), "why", env, t.TempDir(), Options{Command: "sleep 30; echo unreachable", Timeout: 300 * time.Millisecond})
	if err != nil || slow.Error == nil || time.Since(started) > 4*time.Second {
		t.Errorf("a run past its time limit gave %+v, %v after %s", slow, err, time.Since(started))
	}
}

func TestTheAgentsEnvironmentIsBuiltNotInherited(t *testing.T) {
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("HOME", "/home/operator")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "cloud")
	t.Setenv("DATADOG_API_KEY", "observability")
	t.Setenv("OPENAI_API_KEY", "model")
	t.Setenv("KUBECONFIG", "/home/operator/.kube/config")
	read := func(env []string) map[string]string {
		got := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			if _, twice := got[k]; twice {
				t.Errorf("%s appears twice; which one wins depends on the platform", k)
			}
			got[k] = v
		}
		return got
	}

	got := read(environment(map[string]string{"KUBECONFIG": "/case/kubeconfig", "PATH": "/case/bin:/usr/bin"}, "/tmp/empty-home", nil))
	want := map[string]string{"LANG": "C.UTF-8", "HOME": "/tmp/empty-home", "KUBECONFIG": "/case/kubeconfig", "PATH": "/case/bin:/usr/bin"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for _, secret := range []string{"AWS_SECRET_ACCESS_KEY", "DATADOG_API_KEY", "OPENAI_API_KEY"} {
		if _, present := got[secret]; present {
			t.Errorf("%s reached the agent without being named", secret)
		}
	}

	// What the operator names is passed, and naming HOME hands over the real one.
	got = read(environment(map[string]string{"KUBECONFIG": "/case/kubeconfig"}, "/tmp/empty-home", []string{"OPENAI_API_KEY", "HOME", "NOT_SET_ANYWHERE"}))
	if got["OPENAI_API_KEY"] != "model" || got["HOME"] != "/home/operator" || got["KUBECONFIG"] != "/case/kubeconfig" {
		t.Errorf("%v", got)
	}
	if _, present := got["NOT_SET_ANYWHERE"]; present {
		t.Error("a variable that is not set was invented")
	}
	// Naming KUBECONFIG must not undo the case's: what the case adds is laid on last.
	if got = read(environment(map[string]string{"KUBECONFIG": "/case/kubeconfig"}, "/tmp/empty-home", []string{"KUBECONFIG"})); got["KUBECONFIG"] != "/case/kubeconfig" {
		t.Errorf("the operator's KUBECONFIG replaced the case's: %q", got["KUBECONFIG"])
	}
}

func TestTheCommandAgentRunsInAnEmptyHomeWithoutTheOperatorsSecrets(t *testing.T) {
	t.Setenv("HOME", "/home/operator")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "cloud")
	t.Setenv("MODEL_KEY", "model")
	script := `printf '{"answer":"home=%s aws=%s key=%s files=%s"}' "$HOME" "${AWS_SECRET_ACCESS_KEY:-absent}" "${MODEL_KEY:-absent}" "$(ls -A "$HOME" | wc -l | tr -d ' ')" > "$` + EnvTranscript + `"`
	got, err := Command(context.Background(), "why", nil, t.TempDir(), Options{Command: script, PassEnv: []string{"MODEL_KEY"}})
	if err != nil || got.Error != nil {
		t.Fatalf("%+v %v", got, err)
	}
	if strings.Contains(got.Answer, "/home/operator") || !strings.Contains(got.Answer, "aws=absent key=model files=0") {
		t.Errorf("the agent saw: %s", got.Answer)
	}
}

func TestATranscriptReadsTheNullsOlderRecordsCarry(t *testing.T) {
	var tr Transcript
	if err := json.Unmarshal([]byte(`{"agent":"holmes","model":null,"answer":"a","steps":[{"tool":null,"input":"i","output":"o","error":false}],"usage":{"llm_calls":null,"tokens":12,"cost_usd":null,"seconds":3.5},"error":null}`), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Model != "" || tr.Usage.LLMCalls != nil || *tr.Usage.Tokens != 12 || tr.Error != nil || tr.Steps[0].Tool != "" {
		t.Errorf("%+v", tr)
	}
	// A run that failed before its first tool call is recorded with an empty list of steps, not a null:
	// the record is read by more than this program.
	failed, err := Run(context.Background(), "command", "p", nil, t.TempDir(), Options{Command: "exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(failed); !strings.Contains(string(raw), `"steps":[]`) || failed.Error == nil {
		t.Errorf("a failed run was recorded as %s", raw)
	}
	if _, err := Run(context.Background(), "nobody", "p", nil, t.TempDir(), Options{}); err == nil || !strings.Contains(err.Error(), "claude-code, command, holmes") {
		t.Errorf("an unknown agent gave %v", err)
	}
}

// Claude Code keeps its own list of what it may run. It is built from the guard's, so that it cannot
// refuse a read the guard allows — which a list written by hand did, in round 38.
func TestClaudeCodeMayRunWhatTheGuardAllows(t *testing.T) {
	allowed := strings.Join(claudeAllowed, "\n")
	for _, c := range []string{"get", "describe", "logs", "events", "top", "explain", "api-resources", "rollout history", "rollout status", "auth can-i", "config current-context"} {
		if !strings.Contains(allowed, "Bash(kubectl "+c+" *)") || !strings.Contains(allowed, "Bash(kubectl "+c+")\n") || !strings.Contains(claudeSystem, c) {
			t.Errorf("%q is not in what Claude Code may run or is told it may run", c)
		}
	}
	for _, c := range []string{"exec", "delete", "apply", "port-forward", "rollout restart", "rollout undo", "config view", "debug", "run", "cp"} {
		if strings.Contains(allowed, "kubectl "+c) {
			t.Errorf("%q is in what Claude Code may run", c)
		}
	}
	if !strings.Contains(allowed, "Bash(promq *)") || !strings.Contains(allowed, "Bash(jq *)") {
		t.Error("promq and the text filters are gone from the list")
	}
}
