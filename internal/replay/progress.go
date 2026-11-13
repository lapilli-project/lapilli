package replay

import (
	"context"
	"sync/atomic"
)

// Progress counts the rules a run replays, as a progress line says them:
// those to replay, and those replayed.
type Progress struct {
	Total, Done atomic.Int64
}

type progressKey struct{}

// Tracking is ctx with p, which every Run and RunEach asked with it counts
// its rules into; with p nil, ctx counts into nothing.
func Tracking(ctx context.Context, p *Progress) context.Context {
	return context.WithValue(ctx, progressKey{}, p)
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
