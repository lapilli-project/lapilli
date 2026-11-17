package source

import (
	"context"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/replay"
)

// FirstSamples are, of each selector expr reads and each stand-in
// (ledger.SelectorsOf), when it first had data in [lo, hi]: the earliest
// sample of any of its series, so that a series added late moves nothing
// (count over all of them, at a step of an hour, or coarser over a long
// span: one request a selector). atStart says lo is where the look begins:
// a selector with data there may have had it before (AtStart). Where an
// impact signal's data begins, for lapilli serve's record by it and
// lapilli verdict's alike.
func FirstSamples(ctx context.Context, h replay.History, expr string, lo, hi time.Time, atStart bool) ([]ledger.SelectorData, error) {
	sels, err := ledger.SelectorsOf(expr)
	if err != nil {
		return nil, err
	}
	out := make([]ledger.SelectorData, 0, len(sels))
	for _, sel := range sels {
		x, err := FirstSample(ctx, h, sel.Sel, lo, hi, atStart)
		if err != nil {
			return nil, err
		}
		x.For, x.Of, x.To = sel.For, sel.Of, hi
		out = append(out, x)
	}
	return out, nil
}

// FirstSample is when one selector first had data in [lo, hi], as
// FirstSamples looks.
func FirstSample(ctx context.Context, h replay.History, sel string, lo, hi time.Time, atStart bool) (ledger.SelectorData, error) {
	x := ledger.SelectorData{Sel: sel}
	if !hi.After(lo) {
		return x, nil
	}
	step := max(time.Hour, (hi.Sub(lo)/1000).Truncate(time.Minute)+time.Minute)
	series, err := h.QueryRange(ctx, "count("+sel+")", lo, hi, step)
	if err != nil {
		return x, err
	}
	for _, se := range series {
		for _, smp := range se.Samples {
			if x.First.IsZero() || smp.T.Before(x.First) {
				x.First = smp.T
			}
		}
	}
	x.AtStart = atStart && !x.First.IsZero() && !x.First.After(lo)
	return x, nil
}
