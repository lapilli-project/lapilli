package source

import (
	"context"
	"time"

	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/verdict"
)

// NoData sets verdict.RuleResult.NoData of each rule of rep that never
// fired and was replayed (silent, as verdict.Text counts them): what
// replay.NoData finds for the rule of that name in the last of versions. A
// rule that isn't in it, or whose question fails, is left as it is, and the
// first error is returned once the rest are done; the end of ctx stops it
// at once, with its error.
func NoData(ctx context.Context, h replay.History, versions []replay.Version, rep *verdict.Report, from, to time.Time, step time.Duration) error {
	if len(versions) == 0 {
		return nil
	}
	last := versions[len(versions)-1].Rules
	var failed error
	for i := range rep.Rules {
		rr := &rep.Rules[i]
		if rr.Unsupported != "" || rr.Firings+rr.Excused+rr.Incidents != 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, r := range last {
			if r.Name != rr.Rule {
				continue
			}
			names, err := replay.NoData(ctx, h, r, from, to, step)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if failed == nil {
					failed = err
				}
			} else {
				rr.NoData = names
			}
			break
		}
	}
	return failed
}
