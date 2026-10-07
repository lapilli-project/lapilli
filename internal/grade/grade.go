// Package grade grades an investigation in two halves that are kept apart on purpose.
//
// Process checks are deterministic and need no model: did the investigation actually retrieve the
// decisive evidence, and is every entity the answer cites something the agent really saw. They are
// computed from the transcript alone.
//
// The outcome — does the answer say the right thing, and does it avoid blaming a decoy — needs
// reading. No judge is embedded here. Blind packets are written and verdicts read back, so the judge
// can be a person, a model, or several of each, and never knows which run produced an answer.
package grade

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lapilli-project/lapilli/internal/agent"
	"github.com/lapilli-project/lapilli/internal/casefile"
)

var (
	ipPattern  = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	podPattern = regexp.MustCompile(`\b[a-z][a-z0-9-]*-[a-z0-9]{8,10}-[a-z0-9]{5}\b`) // deployment-replicaset-pod
)

// Process is the deterministic half of a grade.
type Process struct {
	EvidenceRetrieved  map[string]bool `json:"evidence_retrieved"` // per decisive pattern: did any step show it
	EvidenceAll        bool            `json:"evidence_all"`
	SpecificityNamed   bool            `json:"specificity_named"`   // the answer names the one population the problem is confined to
	DecoysMentioned    []string        `json:"decoys_mentioned"`    // mentioned, which is not the same as blamed
	CitedEntities      int             `json:"cited_entities"`      // pod names and IP addresses in the answer
	UngroundedEntities []string        `json:"ungrounded_entities"` // cited in the answer, never observed in a step
	Steps              int             `json:"steps"`
	FailedSteps        int             `json:"failed_steps"`
	UsedMetrics        bool            `json:"used_metrics"`
}

// Check computes the process checks for one transcript.
func Check(c *casefile.Case, t *agent.Transcript) Process {
	// What the agent observed is what its tools returned. What it typed does not count: a command that
	// greps for a guess contains the guess, and a query that names the culprit is not an answer from the
	// store. (The prototype counted both; on its 28 recorded runs the two readings grade identically.)
	var seen strings.Builder
	p := Process{EvidenceRetrieved: map[string]bool{}, EvidenceAll: true, DecoysMentioned: []string{}, UngroundedEntities: []string{}, Steps: len(t.Steps)}
	for _, s := range t.Steps {
		seen.WriteString(s.Output + "\n")
		if s.Error {
			p.FailedSteps++
		}
		if strings.Contains(s.Input, "promq") || strings.Contains(strings.ToLower(s.Tool), "prometheus") {
			p.UsedMetrics = true
		}
	}
	observed := seen.String()
	for _, pattern := range c.Patterns("") {
		found := regexp.MustCompile(pattern).MatchString(observed) // Load has already compiled every pattern once
		p.EvidenceRetrieved[pattern] = found
		p.EvidenceAll = p.EvidenceAll && found
	}
	answer := strings.ToLower(t.Answer)
	p.SpecificityNamed = strings.Contains(answer, strings.ToLower(c.Specificity))
	for _, d := range c.Decoys {
		if strings.Contains(answer, strings.ToLower(d)) {
			p.DecoysMentioned = append(p.DecoysMentioned, d)
		}
	}
	cited := map[string]bool{}
	for _, m := range podPattern.FindAllString(t.Answer, -1) {
		cited[m] = true
	}
	for _, m := range ipPattern.FindAllString(t.Answer, -1) {
		if net.ParseIP(m) != nil { // four groups of digits are an address only if each is a byte: "1.4.2.300" is not
			cited[m] = true
		}
	}
	p.CitedEntities = len(cited)
	for m := range cited {
		if !strings.Contains(observed, m) {
			p.UngroundedEntities = append(p.UngroundedEntities, m)
		}
	}
	sort.Strings(p.UngroundedEntities)
	return p
}

// Run is one recorded investigation of one case.
type Run struct {
	RunID       string           `json:"run_id"`
	Case        string           `json:"case"`
	Condition   string           `json:"condition"`    // "frozen" or "live"
	RuleVersion int              `json:"rule_version"` // the grading rule Process was computed under
	Transcript  agent.Transcript `json:"transcript"`
	Process     Process          `json:"process"`
}

// LoadRuns reads every run record under a results directory: <results>/<case>/<run>.json.
func LoadRuns(results string) ([]Run, error) {
	files, err := filepath.Glob(filepath.Join(results, "*", "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	runs := make([]Run, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var r Run
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// Save writes the record to <results>/<case>/<name>.json.
func (r *Run) Save(results, name string) error {
	dir := filepath.Join(results, r.Case)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".json"), append(raw, '\n'), 0o644)
}

// Packet is what an outcome judge sees: the question, the key, and an answer with nothing that says
// which agent, model or condition produced it.
type Packet struct {
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Expected []string `json:"expected"`
	MustNot  []string `json:"must_not"`
	Answer   string   `json:"answer"`
}

// Packets builds blind packets and the key that maps them back to runs. The key must not be shown to
// the judge. The same seed gives the same ids and order, which is also why the seed of a real judging
// should not be guessable: with it and the list of runs, anyone can rebuild the key.
func Packets(runs []Run, cases map[string]*casefile.Case, seed int64) ([]Packet, map[string]string, error) {
	rnd := rand.New(rand.NewSource(seed))
	out, key := make([]Packet, 0, len(runs)), map[string]string{}
	for _, r := range runs {
		c, ok := cases[r.Case]
		if !ok {
			return nil, nil, fmt.Errorf("run %s is of case %q, which was not given", r.RunID, r.Case)
		}
		id := fmt.Sprintf("%06x", rnd.Intn(1<<24))
		for key[id] != "" {
			id = fmt.Sprintf("%06x", rnd.Intn(1<<24))
		}
		answer := r.Transcript.Answer
		if answer == "" {
			answer = "(the agent produced no answer)"
		}
		out = append(out, Packet{ID: id, Question: c.Prompt, Expected: c.Expected, MustNot: c.MustNot, Answer: answer})
		key[id] = r.RunID
	}
	rnd.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, key, nil
}

// RuleVersion is the version of the grading rule: the process checks in Check and JudgeInstructions,
// together (docs/case-grading.md). Results under different versions do not compare.
const RuleVersion = 0

// JudgeInstructions is the rule an outcome judge is given. It is the rule the recorded verdicts of
// 2026-10-06 and of round 38 were given under, word for word; what else a judge was told — which file
// to read, how to reply — is kept beside its verdicts (test/fixtures/case-runs/…/judge-*.txt).
const JudgeInstructions = `Grade each packet independently of every other packet.

1. For each statement in ` + "`expected`" + `: is it clearly conveyed by the answer as a finding? Same meaning counts; wording may differ. A statement that appears only as one unconfirmed possibility among several, or only as a suggestion for further checking, is NOT conveyed. If a statement contains specific values (a number, a file path, a setting, a node or workload name), the answer must give those specifics or unmistakable equivalents — a vaguer version ("some node-level problem", "resource limits") is NOT conveyed.
2. For each statement in ` + "`must_not`" + `: does the answer do it? Mentioning the item in order to rule it out is NOT doing it. Presenting it as the root cause, as a contributing cause, or as the fix IS doing it.
3. Verdict: PASS only if every expected statement is conveyed and no must_not statement is done. Otherwise FAIL.

Do not try to infer anything about how or where an answer was produced; it is irrelevant.

Write {"<id>": {"verdict": "PASS"|"FAIL", "expected": [true|false, one per expected statement, in order], "must_not": [true|false, one per must_not statement, in order; true means the answer does it], "reason": "<one sentence, quoting the decisive phrase or naming what is missing>"}}, covering every id exactly once.`

// Verdict is a judge's ruling on one packet. Only Verdict is read by the report; the rest is the
// judge's account of it: per expected statement whether it was conveyed, per must_not statement
// whether the answer does it.
type Verdict struct {
	Verdict  string `json:"verdict"`
	Expected []bool `json:"expected"`
	MustNot  []bool `json:"must_not"`
	Reason   string `json:"reason"`
}

// Row is one line of a report: a (case, agent, model, condition) group, its outcome pass count where
// the runs were judged, and the process checks beside it.
type Row struct {
	Case      string `json:"case"`
	Agent     string `json:"agent"`
	Model     string `json:"model"`
	Condition string `json:"condition"`
	Runs      int    `json:"runs"`
	// Errored counts runs that ended without an answer: the agent hit its limit, or its provider was
	// down. They stay in every denominator — no answer is not a right answer — and are shown so that
	// an outage is not read as an agent that cannot investigate.
	Errored          int     `json:"errored"`
	OutcomePass      *int    `json:"outcome_pass"` // nil: not judged
	Judged           int     `json:"judged"`       // how many of the runs have a verdict
	EvidenceAll      int     `json:"evidence_all"`
	SpecificityNamed int     `json:"specificity_named"`
	UngroundedRuns   int     `json:"ungrounded_runs"`
	MeanSteps        float64 `json:"mean_steps"`
	MeanCostUSD      float64 `json:"mean_cost_usd"`
}

// Summarize groups runs into report rows. verdicts are keyed by packet id and key maps packet ids to
// run ids; either may be nil, and then no outcome is reported.
func Summarize(runs []Run, verdicts map[string]Verdict, key map[string]string) []Row {
	byRun := map[string]Verdict{}
	for id, v := range verdicts {
		if run, ok := key[id]; ok {
			byRun[run] = v
		}
	}
	type group struct{ c, agent, model, condition string }
	groups := map[group][]Run{}
	var order []group
	for _, r := range runs {
		g := group{r.Case, r.Transcript.Agent, r.Transcript.Model, r.Condition}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], r)
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		return a.c+"\x00"+a.agent+"\x00"+a.model+"\x00"+a.condition < b.c+"\x00"+b.agent+"\x00"+b.model+"\x00"+b.condition
	})
	rows := make([]Row, 0, len(order))
	for _, g := range order {
		row := Row{Case: g.c, Agent: g.agent, Model: g.model, Condition: g.condition, Runs: len(groups[g])}
		var steps, cost float64
		judged, passed := 0, 0
		for _, r := range groups[g] {
			if v, ok := byRun[r.RunID]; ok {
				judged++
				if v.Verdict == "PASS" {
					passed++
				}
			}
			if r.Transcript.Error != nil {
				row.Errored++
			}
			if r.Process.EvidenceAll {
				row.EvidenceAll++
			}
			if r.Process.SpecificityNamed {
				row.SpecificityNamed++
			}
			if len(r.Process.UngroundedEntities) > 0 {
				row.UngroundedRuns++
			}
			steps += float64(r.Process.Steps)
			if r.Transcript.Usage.CostUSD != nil {
				cost += *r.Transcript.Usage.CostUSD
			}
		}
		if row.Judged = judged; judged > 0 {
			row.OutcomePass = &passed
		}
		n := float64(row.Runs)
		row.MeanSteps, row.MeanCostUSD = math.Round(steps/n*10)/10, math.Round(cost/n*1000)/1000
		rows = append(rows, row)
	}
	return rows
}
