package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/graph"
	"lapilli.dev/internal/verdict"
)

// dependsFlags are --depends and the flags that go with it
// (docs/design/dependencies.md).
type dependsFlags struct {
	// depends is --depends: the expression; dependsFrom and dependsTo the
	// labels --depends-labels names, for the caller and the callee.
	depends                string
	dependsFrom, dependsTo string
	dependsMatch           string
	dependsDepth           int
	dependsDirection       string
	dependsStep            time.Duration
	dependsGiven           []string // the flags that go with --depends, as given
	dependsLabelsSet       bool
	// dependsCountMissed is --depends-count-catches: a catch made only by
	// the service graph counts for --fail-missed and for spare rules.
	dependsCountMissed bool
}

// dependsFlag reads one of the --depends flags.
func (f *dependsFlags) dependsFlag(name, val string) error {
	f.dependsGiven = append(f.dependsGiven, name)
	switch name {
	case "--depends":
		if strings.TrimSpace(val) == "" {
			return errors.New("--depends: want an expression whose series name a caller and a callee")
		}
		if f.depends != "" {
			return errors.New("--depends is given twice: join them in one expression")
		}
		f.depends = val
	case "--depends-labels":
		from, to, ok := strings.Cut(val, ",")
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if !ok || from == "" || to == "" || from == to || strings.Contains(to, ",") {
			return fmt.Errorf("--depends-labels %q: want FROM,TO, two different labels, the caller's then the callee's", val)
		}
		f.dependsFrom, f.dependsTo, f.dependsLabelsSet = from, to, true
	case "--depends-match":
		if val == "" {
			return errors.New("--depends-match: want a label of --match")
		}
		f.dependsMatch = val
	case "--depends-depth":
		n, err := strconv.Atoi(val)
		if err != nil || n < 1 || n > verdict.DependsMaxDepth {
			return fmt.Errorf("--depends-depth %s: want a number of calls, 1 to %d", val, verdict.DependsMaxDepth)
		}
		f.dependsDepth = n
	case "--depends-direction":
		if val != verdict.DependsDown && val != verdict.DependsBoth {
			return fmt.Errorf("--depends-direction %s: want down or both", val)
		}
		f.dependsDirection = val
	case "--depends-step":
		d, err := parseDuration(val)
		if err != nil || d < graph.MinStep || d > graph.MaxStep {
			return fmt.Errorf("--depends-step %s: want a duration like 1h, from %s to %s (the expression counts calls over the hour)", val, verdict.Duration(graph.MinStep), verdict.Duration(graph.MaxStep))
		}
		f.dependsStep = d
	}
	return nil
}

// validDepends checks --depends against the flags it goes with, and fills
// in its defaults: the label of the match, the step.
func (f *verdictFlags) validDepends() error {
	if f.depends == "" {
		if f.dependsCountMissed {
			return errors.New("--depends-count-catches goes with --depends: the expression that says who calls whom")
		}
		if len(f.dependsGiven) > 0 {
			return fmt.Errorf("%s goes with --depends: the expression that says who calls whom", f.dependsGiven[0])
		}
		return nil
	}
	switch {
	case !f.dependsLabelsSet:
		return errors.New("--depends needs --depends-labels FROM,TO: the labels of its series that name the caller and the callee")
	case len(f.opts.Match) == 0:
		return errors.New("--depends needs a --match label: the services' names are values of it")
	case f.dependsMatch != "" && !slices.Contains(f.opts.Match, f.dependsMatch):
		return fmt.Errorf("--depends-match %s: want one of the labels --match gives (%s)", f.dependsMatch, strings.Join(f.opts.Match, ", "))
	case f.promURL == "" && f.gcpProj == "":
		return errors.New("--depends reads history: add " + historyAt)
	}
	if f.dependsMatch == "" {
		f.dependsMatch = f.opts.Match[0]
	}
	if f.dependsDepth == 0 {
		f.dependsDepth = verdict.DependsDefaultDepth
	}
	if f.dependsDirection == "" {
		f.dependsDirection = verdict.DependsDown
	}
	if f.dependsStep == 0 {
		f.dependsStep = graph.DefaultStep
	}
	return nil
}

// readDepends reads the service graph (--depends) over the period and the
// longest lead before it, and gives it to the options. An hour of it with
// more edges than are kept leaves it with none, which the report says.
func (f *verdictFlags) readDepends(ctx context.Context, h history) error {
	if f.depends == "" {
		return nil
	}
	q := graph.Query{Expr: f.depends, From: f.dependsFrom, To: f.dependsTo, Step: f.dependsStep}
	// The edges of the hour before an incident count (dependencies.md):
	// from the period's start, an hour back at least.
	from := f.opts.From.Add(-max(f.opts.MaxLead(), time.Hour))
	g, err := graph.Read(ctx, h, q, from, f.opts.To)
	if err != nil {
		return fmt.Errorf("--depends: %w", err)
	}
	f.opts.Depends = &verdict.Dependencies{Graph: g, Label: f.dependsMatch, Depth: f.dependsDepth, Direction: f.dependsDirection, CountCatches: f.dependsCountMissed}
	return nil
}
