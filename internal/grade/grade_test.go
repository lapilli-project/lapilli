package grade

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lapilli-project/lapilli/internal/agent"
	"github.com/lapilli-project/lapilli/internal/casefile"
)

const (
	casesDir = "../../cases"
	runsDir  = "../../test/fixtures/case-runs"
)

func loadCases(t *testing.T) map[string]*casefile.Case {
	t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(casesDir, "*", casefile.SpecName))
	cases := map[string]*casefile.Case{}
	for _, d := range dirs {
		c, err := casefile.Load(d)
		if err != nil {
			t.Fatal(err)
		}
		cases[c.ID] = c
	}
	if len(cases) == 0 {
		t.Fatal("no cases found")
	}
	return cases
}

func loadRecorded(t *testing.T, batch string) []Run {
	t.Helper()
	runs, err := LoadRuns(filepath.Join(runsDir, batch))
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func demoCase() *casefile.Case {
	return &casefile.Case{ID: "demo", Prompt: "why?", Expected: []string{"the cache is full"}, MustNot: []string{"blames the network policy"},
		Specificity: "report-worker", Decoys: []string{"NetworkPolicy", "LOG_LEVEL"},
		Evidence: []casefile.Evidence{{Pattern: `conn-table active=40/40`, Store: casefile.StoreKubernetes}, {Pattern: `CACHE_CONN_MODE`, Store: casefile.StoreKubernetes}}}
}

func TestCheckSeparatesWhatWasSeenFromWhatWasSaid(t *testing.T) {
	transcript := &agent.Transcript{
		Answer: "report-worker-7d9f8c6b54-abcde at 10.244.1.7 holds every slot; the NetworkPolicy is not involved. cache-5f6d7c8b-zzzzz and 10.9.9.9 are also affected.",
		Steps: []agent.Step{
			{Tool: "bash", Input: "kubectl logs deploy/cache", Output: "conn-table active=40/40 from 10.244.1.7"},
			{Tool: "bash", Input: "kubectl get pods", Output: "report-worker-7d9f8c6b54-abcde Running", Error: true},
			// Typing the evidence, or an address, is not observing it.
			{Tool: "bash", Input: "kubectl get cm -A -o yaml | grep -c CACHE_CONN_MODE; ping 10.9.9.9", Output: "0"},
			{Tool: "bash", Input: "promq 'up'", Output: "up{} 1"},
		},
	}
	got := Check(demoCase(), transcript)
	want := Process{
		EvidenceRetrieved:  map[string]bool{`conn-table active=40/40`: true, `CACHE_CONN_MODE`: false},
		EvidenceAll:        false,
		SpecificityNamed:   true,
		DecoysMentioned:    []string{"NetworkPolicy"},
		CitedEntities:      4,
		UngroundedEntities: []string{"10.9.9.9", "cache-5f6d7c8b-zzzzz"}, // named in the answer, returned by no tool
		Steps:              4,
		FailedSteps:        1,
		UsedMetrics:        true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
}

// The recorded runs were graded by the prototype this package replaces. Recomputing every process
// check from the stored transcript and getting the stored result back is what says the port grades
// the same way — including where Go's regular expressions could have differed from Python's.
func TestCheckReproducesEveryRecordedGrade(t *testing.T) {
	cases := loadCases(t)
	total := 0
	for _, batch := range []string{"2026-10-06-hard-cases", "2026-10-06-s3-extra"} {
		for _, r := range loadRecorded(t, batch) {
			total++
			if got := Check(cases[r.Case], &r.Transcript); !reflect.DeepEqual(got, r.Process) {
				t.Errorf("%s/%s\n got %+v\nwant %+v", batch, r.RunID, got, r.Process)
			}
		}
	}
	if total != 28 {
		t.Errorf("%d recorded runs were found, want 28", total)
	}
}

// The numbers docs/design-review-round37.md reports, recomputed from the records and the verdicts.
func TestSummarizeReproducesTheReportedExperiment(t *testing.T) {
	batch := filepath.Join(runsDir, "2026-10-06-hard-cases")
	var verdicts map[string]Verdict
	var key map[string]string
	for path, into := range map[string]any{"verdicts.json": &verdicts, "key.json": &key} {
		raw, err := os.ReadFile(filepath.Join(batch, path))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	runs := loadRecorded(t, "2026-10-06-hard-cases")
	pass, evidence, passWithEvidence, passWithout := map[string]int{}, 0, 0, 0
	for _, row := range Summarize(runs, verdicts, key) {
		if row.OutcomePass == nil || row.Runs != 3 {
			t.Fatalf("row %+v is not three judged runs", row)
		}
		pass[row.Condition] += *row.OutcomePass
	}
	byRun := map[string]Verdict{}
	for id, v := range verdicts {
		byRun[key[id]] = v
	}
	for _, r := range runs {
		passed := byRun[r.RunID].Verdict == "PASS"
		switch {
		case r.Process.EvidenceAll:
			evidence++
			if passed {
				passWithEvidence++
			}
		case passed:
			passWithout++
		}
	}
	if pass["live"] != 3 || pass["frozen"] != 3 {
		t.Errorf("outcome passes = %v, want 3 of 9 in each condition", pass)
	}
	if evidence != 9 || passWithEvidence != 6 || passWithout != 0 {
		t.Errorf("%d runs retrieved all decisive evidence and %d of them passed; %d passed without it; want 9, 6 and 0", evidence, passWithEvidence, passWithout)
	}
	if rows := Summarize(runs, nil, nil); rows[0].OutcomePass != nil {
		t.Error("an unjudged batch reported an outcome")
	}
}

func TestPacketsAreBlindAndRepeatable(t *testing.T) {
	cases := loadCases(t)
	runs := loadRecorded(t, "2026-10-06-hard-cases")
	packets, key, err := Packets(runs, cases, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) != len(runs) || len(key) != len(runs) {
		t.Fatalf("%d packets and %d keys for %d runs", len(packets), len(key), len(runs))
	}
	raw, _ := json.Marshal(packets)
	for _, tell := range []string{"frozen", "live", "haiku", "claude-code", "run_id", "condition"} {
		if strings.Contains(string(raw), `"`+tell) || strings.Contains(string(raw), tell+`-claude`) {
			t.Errorf("the packets carry %q, which tells a judge where an answer came from", tell)
		}
	}
	inOrder := true
	for i, p := range packets {
		if key[p.ID] != runs[i].RunID {
			inOrder = false
		}
	}
	if inOrder {
		t.Error("the packets are in run order; a judge could read the condition off the position")
	}
	again, againKey, _ := Packets(runs, cases, 7)
	if !reflect.DeepEqual(packets, again) || !reflect.DeepEqual(key, againKey) {
		t.Error("the same seed gave different packets")
	}
	if other, _, _ := Packets(runs, cases, 8); reflect.DeepEqual(packets, other) {
		t.Error("a different seed gave the same packets")
	}
	if _, _, err := Packets(runs, map[string]*casefile.Case{}, 7); err == nil {
		t.Error("a run of a case that was not given was accepted")
	}
	if p, _, _ := Packets([]Run{{RunID: "demo/x-1", Case: "demo"}}, map[string]*casefile.Case{"demo": demoCase()}, 0); p[0].Answer == "" {
		t.Error("a run with no answer gave the judge an empty string to grade")
	}
}

func TestARunRecordSurvivesSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	cost, calls := 0.25, int64(4)
	in := Run{RunID: "demo/frozen-x-1", Case: "demo", Condition: "frozen",
		Transcript: agent.Transcript{Agent: "x", Model: "m", Answer: "a", Steps: []agent.Step{{Tool: "bash", Input: "i", Output: "o"}}, Usage: agent.Usage{LLMCalls: &calls, CostUSD: &cost, Seconds: 1.5}}}
	in.Process = Check(demoCase(), &in.Transcript)
	if err := in.Save(dir, "frozen-x-1"); err != nil {
		t.Fatal(err)
	}
	out, err := LoadRuns(dir)
	if err != nil || len(out) != 1 || !reflect.DeepEqual(out[0], in) {
		t.Errorf("loaded %+v (%v), saved %+v", out, err, in)
	}
}
