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

func TestParseHolmesKeepsObservationsAndDropsItsNotes(t *testing.T) {
	out := parseHolmes([]byte(`{"result":"answer","num_llm_calls":3,"total_tokens":1200,"total_cost":0.5,"tool_calls":[
		{"tool_name":"TodoWrite","description":"plan","result":"x"},
		{"tool_name":"kubectl_logs","description":"kubectl logs x","result":"plain text"},
		{"tool_name":"prometheus/metrics","description":"query","result":{"status":"error","error":"bad query"}}]}`))
	if out.Answer != "answer" || *out.Usage.LLMCalls != 3 || *out.Usage.Tokens != 1200 || *out.Usage.CostUSD != 0.5 || len(out.Steps) != 2 {
		t.Fatalf("%+v", out)
	}
	if s := out.Steps[0]; s.Tool != "kubectl_logs" || s.Output != "plain text" || s.Error {
		t.Errorf("first step = %+v", s)
	}
	if s := out.Steps[1]; !s.Error || !strings.Contains(s.Output, "bad query") {
		t.Errorf("second step = %+v", s)
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

func TestEnvironOverlaysAndDrops(t *testing.T) {
	t.Setenv("LAPILLI_TEST_KEEP", "kept")
	t.Setenv("LAPILLI_TEST_DROP", "secret")
	t.Setenv("LAPILLI_TEST_OVER", "old")
	got := map[string]string{}
	for _, kv := range environ(map[string]string{"LAPILLI_TEST_OVER": "new"}, "LAPILLI_TEST_DROP") {
		k, v, _ := strings.Cut(kv, "=")
		if _, twice := got[k]; twice {
			t.Errorf("%s appears twice; which one wins depends on the platform", k)
		}
		got[k] = v
	}
	if got["LAPILLI_TEST_KEEP"] != "kept" || got["LAPILLI_TEST_OVER"] != "new" {
		t.Errorf("%v", got)
	}
	if _, present := got["LAPILLI_TEST_DROP"]; present {
		t.Error("a dropped variable was passed on")
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
	if _, err := Run(context.Background(), "nobody", "p", nil, t.TempDir(), Options{}); err == nil || !strings.Contains(err.Error(), "claude-code, command, holmes") {
		t.Errorf("an unknown agent gave %v", err)
	}
}
