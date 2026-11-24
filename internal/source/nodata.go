package source

import (
	"context"
	"slices"
	"time"

	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/verdict"
)

// NoData sets verdict.RuleResult.NoData of each rule of rep that never
// fired and was replayed (silent, as verdict.Text counts them): what
// replay.NoData finds for the rules of that name. With one version, those
// of it over [from, to]. With several, those of each version over the span
// it was in effect within [from, to], the versions without the rule left
// out; the rule has NoData only when every span asked had none for some
// operand, and the expressions are those of the last span asked. Of rules
// sharing a name, every one must have none, and the expressions are theirs
// together. A rule that isn't in any version, or one a question fails for,
// is left as it is, and the first error is returned once the rest are
// done; the end of ctx stops it at once, with its error.
func NoData(ctx context.Context, h replay.History, versions []replay.Version, rep *verdict.Report, from, to time.Time, step time.Duration) error {
	if len(versions) == 0 {
		return nil
	}
	var failed error
	for i := range rep.Rules {
		rr := &rep.Rules[i]
		if rr.Unsupported != "" || rr.Firings+rr.Excused+rr.Incidents != 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		names, asked, err := noDataOf(ctx, h, versions, rr.Rule, from, to, step)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if failed == nil {
				failed = err
			}
			continue
		}
		if asked && len(names) > 0 {
			rr.NoData = names
		}
	}
	return failed
}

// noDataOf is the expressions with no data for the rule called name in
// versions, and whether any span was asked; none when some span had data.
func noDataOf(ctx context.Context, h replay.History, versions []replay.Version, name string, from, to time.Time, step time.Duration) (names []string, asked bool, err error) {
	for i, v := range versions {
		start, end := from, to
		if len(versions) > 1 {
			if v.From.After(start) {
				start = v.From
			}
			if i+1 < len(versions) && versions[i+1].From.Before(end) {
				end = versions[i+1].From
			}
			if !start.Before(end) {
				continue
			}
		}
		var here []string
		found, every := false, true
		for _, r := range v.Rules {
			if r.Name != name {
				continue
			}
			found = true
			got, err := replay.NoData(ctx, h, r, start, end, step)
			if err != nil {
				return nil, false, err
			}
			if len(got) == 0 {
				every = false
				break
			}
			for _, e := range got {
				if !slices.Contains(here, e) {
					here = append(here, e)
				}
			}
		}
		if !found {
			continue
		}
		asked = true
		if !every {
			return nil, true, nil
		}
		names = here
	}
	return names, asked, nil
}
