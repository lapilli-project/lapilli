package replay

import (
	"context"
	"sync/atomic"
)

// Progress counts the rules a run replays, as a progress line says them:
// those to replay, and those replayed; and, once they are replayed, the
// step after them it is at.
type Progress struct {
	Total, Done atomic.Int64
	step        atomic.Pointer[Step]
}

// Step is one of the steps after the replays, such as looking for near
// misses, as a progress line says it: what it does, and how far it is, in
// Unit, of Total, or of at most Total when AtMost.
type Step struct {
	Name, Unit  string
	AtMost      bool
	Total, Done atomic.Int64
}

// Begin says the run ctx tracks is now at a step after the replays, of
// total pieces of work (none: it can't say), and returns it to count them
// done. With no Progress in ctx, the step is counted, but by nothing.
func Begin(ctx context.Context, name, unit string, total int, atMost bool) *Step {
	s := &Step{Name: name, Unit: unit, AtMost: atMost}
	s.Total.Store(int64(total))
	if p, _ := ctx.Value(stepKey{}).(*Progress); p != nil {
		p.step.Store(s)
	}
	return s
}

// Did counts n of the step's pieces done.
func (s *Step) Did(n int) {
	if s != nil {
		s.Done.Add(int64(n))
	}
}

// Now is the step after the replays the run is at, nil until one begins.
func (p *Progress) Now() *Step {
	if p == nil {
		return nil
	}
	return p.step.Load()
}

type progressKey struct{}

// stepKey keeps the Progress the steps after the replays are said in: a
// run that stops counting replays (Tracking with nil) still says its steps.
type stepKey struct{}

// Tracking is ctx with p, which every Run and RunEach asked with it counts
// its rules into; with p nil, ctx counts into nothing, but the steps after
// the replays are still said in the Progress ctx had.
func Tracking(ctx context.Context, p *Progress) context.Context {
	ctx = context.WithValue(ctx, progressKey{}, p)
	if p != nil {
		ctx = context.WithValue(ctx, stepKey{}, p)
	}
	return ctx
}

func progressOf(ctx context.Context) *Progress {
	p, _ := ctx.Value(progressKey{}).(*Progress)
	return p
}

func (p *Progress) add(n int) {
	if p != nil {
		p.Total.Add(int64(n))
	}
}

func (p *Progress) ran() {
	if p != nil {
		p.Done.Add(1)
	}
}
