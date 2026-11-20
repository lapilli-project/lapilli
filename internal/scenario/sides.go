package scenario

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// MaxRuns is the most times Sides runs promtool for one comparison: each
// test file against each group, once whatever the rules it holds; the rest
// are not known.
var MaxRuns = 200

// Why a scenario of Sides is not known.
const (
	whyNotProm    = "not a rule in Prometheus's format, which promtool runs"
	whyNoRule     = "the rule's definition isn't known"
	whyTestUnread = "its test file can't be read"
	whyNoGroup    = "its group couldn't be written"
	whyTooMany    = "past the scenarios one backtest runs"
)

// Sides runs a comparison's scenarios (verdict.ScenarioRunner): each test
// file, read inside the directory of the expectations file that names it
// (After, the one for the rules proposed; Before, --expect-before's),
// against the rule's group as its side has it, with Promtool. Each group
// and test file is run once (by what they hold), MaxRuns at most.
type Sides struct {
	Ctx      context.Context
	Promtool Promtool
	// Rules and Recorded are each side's rules, before and after, under the
	// names they are judged by.
	Rules    [2][]rules.Rule
	Recorded [2][]rules.Recording
	// After and Before are the directories of the expectations files.
	After, Before *os.Root

	groups map[[2]string][]byte // by side and group
	of     [2]map[string]string // a side's rules' groups, by name
	ran    map[[32]byte]verdict.ScenarioResult
	runs   int
}

func sideOf(side string) int {
	if side == "after" {
		return 1
	}
	return 0
}

// RunScenario runs path against the group of rule on side.
func (s *Sides) RunScenario(side string, fromBefore bool, rule, path string) verdict.ScenarioResult {
	i := sideOf(side)
	if s.of[i] == nil {
		s.of[i] = map[string]string{}
		for _, r := range s.Rules[i] {
			if _, ok := s.of[i][r.Name]; !ok {
				s.of[i][r.Name] = GroupKey(r.Source, r.Namespace, r.Group)
			}
		}
	}
	key, ok := s.of[i][rule]
	if !ok {
		return verdict.ScenarioResult{State: verdict.ExpectUnknown, Why: whyNoRule}
	}
	var def rules.Rule
	for _, r := range s.Rules[i] {
		if r.Name == rule {
			def = r
			break
		}
	}
	if !Runs(def) {
		return verdict.ScenarioResult{State: verdict.ExpectUnknown, Why: whyNotProm}
	}
	root := s.After
	if fromBefore {
		root = s.Before
	}
	b, err := ReadTest(root, path)
	if err != nil {
		return verdict.ScenarioResult{State: verdict.ExpectUnknown, Why: whyTestUnread, Said: err.Error(), Unrun: true}
	}
	sum := sha256.Sum256(b)
	test := hex.EncodeToString(sum[:8])
	t, err := Rewrite(b)
	if err != nil {
		return verdict.ScenarioResult{State: verdict.ExpectUnknown, Why: whyTestUnread, Said: rules.Shown(err.Error()), Test: test, Unrun: true}
	}
	g, err := s.group(i, key)
	if err != nil {
		return verdict.ScenarioResult{State: verdict.ExpectUnknown, Why: whyNoGroup, Test: test, Unrun: true}
	}
	run := sha256.Sum256(fmt.Appendf(nil, "%d\x00%s\x00%s", len(g), g, t))
	if x, ok := s.ran[run]; ok {
		x.Test = test
		return x
	}
	if s.runs >= MaxRuns {
		return verdict.ScenarioResult{State: verdict.ExpectUnknown, Why: whyTooMany, Test: test, Unrun: true}
	}
	s.runs++
	ctx := s.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	x := Judged(s.Promtool.Run(ctx, g, t))
	if s.ran == nil {
		s.ran = map[[32]byte]verdict.ScenarioResult{}
	}
	s.ran[run] = x
	x.Test = test
	return x
}

// TestEdited says a scenario's test file differs between the sides
// (verdict.TestEditor), by what it holds: before's path read in the
// directory of --expect-before's file when fromBefore says so, else in
// --expect's; after's in --expect's. One that can be read on neither side
// isn't; one read on one side alone is.
func (s *Sides) TestEdited(fromBefore bool, before, after string) bool {
	root := s.After
	if fromBefore {
		root = s.Before
	}
	b, errB := ReadTest(root, before)
	a, errA := ReadTest(s.After, after)
	if errB != nil || errA != nil {
		return (errB == nil) != (errA == nil)
	}
	return !bytes.Equal(a, b)
}

var _ verdict.TestEditor = (*Sides)(nil)

// Judged is a run's result as an expectation's: passed met, failed broken
// (what promtool said of it), unrun not known, and so.
func Judged(r Result) verdict.ScenarioResult {
	switch r.State {
	case Passed:
		return verdict.ScenarioResult{State: verdict.ExpectMet}
	case Failed:
		return verdict.ScenarioResult{State: verdict.ExpectBroken, Said: r.Said}
	}
	return verdict.ScenarioResult{State: verdict.ExpectUnknown, Why: r.Why, Unrun: true}
}

// group is a side's group of that key, written once.
func (s *Sides) group(side int, key string) ([]byte, error) {
	k := [2]string{fmt.Sprint(side), key}
	if g, ok := s.groups[k]; ok {
		return g, nil
	}
	var alerts []rules.Rule
	var group, source string
	for _, r := range s.Rules[side] {
		if GroupKey(r.Source, r.Namespace, r.Group) == key {
			alerts = append(alerts, r)
			group, source = r.Group, r.Source
		}
	}
	var recorded []rules.Recording
	for _, r := range s.Recorded[side] {
		if r.Group == group && r.Source == source {
			recorded = append(recorded, r)
		}
	}
	g, err := Group(alerts, recorded)
	if err != nil {
		return nil, err
	}
	if s.groups == nil {
		s.groups = map[[2]string][]byte{}
	}
	s.groups[k] = g
	return g, nil
}

// Close closes the directories the test files are read in.
func (s *Sides) Close() {
	for _, r := range []*os.Root{s.After, s.Before} {
		if r != nil {
			r.Close()
		}
	}
}
