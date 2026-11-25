package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"lapilli.dev/internal/deploys"
	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/response"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// usesResponses reports whether what people did when a rule fired is read:
// --responses, --rollouts or --deploys (docs/design/responses.md).
func (f *verdictFlags) usesResponses() bool {
	return f.responses != "" || f.rollouts || f.deploys != ""
}

// validResponses checks the flags of responses against the others.
func (f *verdictFlags) validResponses() error {
	switch {
	case f.noisyBy == verdict.NoisyByUseful && !f.usesResponses():
		return errors.New("--noisy-by useful finds a rule noisy by what was done about its firings: give --responses, --rollouts or --deploys")
	case len(f.rolloutMatch) > 0 && !f.rollouts:
		return errors.New("--rollout-match names the label that ties a firing to the workload it is about: it goes with --rollouts")
	case f.routine != 0 && !f.usesResponses():
		return errors.New("--routine is the chance a change was routine: it goes with --responses, --rollouts or --deploys")
	case f.usesResponses() && (f.checkContent || f.checkReplay):
		return errors.New("--responses, --rollouts and --deploys judge firings: they don't go with --check-content or --check-replay")
	}
	return nil
}

// noisyByOf is what rules are found noisy by: --noisy-by, or useful once
// responses are read, else caught.
func (f *verdictFlags) noisyByOf() string {
	switch {
	case f.noisyBy != "":
		return f.noisyBy
	case f.usesResponses():
		return verdict.NoisyByUseful
	}
	return verdict.NoisyByCaught
}

// readResponses reads what people did, for the firings fs: the pages of
// --responses, the changes kube-state-metrics kept of the workloads they
// are about (--rollouts) and the deploys of --deploys. It sets what judges
// firings by them in f.opts. With none of the three it does nothing.
func (f *verdictFlags) readResponses(ctx context.Context, h history, fs []firing.Firing) error {
	if !f.usesResponses() {
		return nil
	}
	ev := response.Evidence{Match: f.rolloutMatch, Routine: f.routine}
	if ev.Routine == 0 {
		ev.Routine = response.DefaultRoutine
	}
	if f.responses != "" {
		pages, err := response.Load(f.responses)
		if err != nil {
			return fmt.Errorf("--responses %w", err)
		}
		ev.Pages = response.NewIndex(pages)
	}
	cs := response.NewChangeSet(f.opts.From, f.opts.To)
	if f.rollouts {
		if h == nil {
			return errors.New("--rollouts reads kube-state-metrics from the history: add --prometheus")
		}
		var namespaces []string
		var pods []response.Pod
		seen := map[string]bool{}
		for _, x := range fs {
			if !x.Active(f.opts.From, f.opts.To) {
				continue
			}
			ns := x.Labels["namespace"]
			if ns == "" {
				continue
			}
			if !seen[ns] {
				seen[ns] = true
				namespaces = append(namespaces, ns)
			}
			if pod := x.Labels["pod"]; pod != "" {
				pods = append(pods, response.Pod{Namespace: ns, Name: pod})
			}
		}
		sort.Strings(namespaces)
		slices.SortFunc(pods, func(a, b response.Pod) int {
			if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
				return c
			}
			return strings.Compare(a.Name, b.Name)
		})
		pods = slices.Compact(pods)
		r, err := response.ReadRollouts(ctx, h, f.opts.From, f.opts.To, f.step, namespaces, pods)
		if err != nil {
			return err
		}
		cs.AddRollouts(r)
	}
	if f.deploys != "" {
		ds, _, err := deploys.Load(f.deploys)
		if err != nil {
			return fmt.Errorf("--deploys %w", err)
		}
		cs.AddDeploys(ds)
	}
	ev.Changes = cs
	f.opts.Responder = &source.Responses{Evidence: ev}
	f.opts.ResponseStep = f.step
	f.opts.NoisyBy = f.noisyByOf()
	f.opts.Routine = ev.Routine
	return nil
}
