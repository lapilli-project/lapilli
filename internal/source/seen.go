package source

import (
	"context"
	"slices"
	"strings"
	"time"

	"lapilli.dev/internal/replay"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/verdict"
)

// What a named impact signal reads (docs/design/signals.md, "The record,
// signal by signal"): what ties the firings it could have seen, beside
// what it returned, for lapilli serve's record by it and lapilli
// verdict's alike.

// SeenFrom is what an expression reads over [lo, hi], of the labels
// named: each selector it reads (rules.Selectors) counted by those labels,
// one request a selector, at a step of an hour, or coarser over a long
// span, as where its data begins is looked for (FirstSample); a
// selector that only narrows another the expression reads counted through
// its stand-in (rules.StandIns: http_requests_total{code!=""} for
// http_requests_total{code=~"5.."}), as only counting the 5xx would see
// only the services that had one. A series counted without a label, as one
// of a label the expression makes (label_replace, a probe's target
// relabelled), takes the signal to have every value of it (verdict.Seen);
// so does an expression that reads no selector. A look that finds no series
// at all learns nothing.
func SeenFrom(ctx context.Context, h replay.History, expr string, labels []string, lo, hi time.Time) (*verdict.Seen, error) {
	seen := verdict.NewSeen(labels)
	if len(labels) == 0 {
		return seen, nil
	}
	sels, err := seenSelectors(expr)
	if err != nil {
		return nil, err
	}
	if len(sels) == 0 {
		for _, l := range labels {
			seen.Every(l)
		}
		return seen, nil
	}
	step := max(time.Hour, (hi.Sub(lo)/1000).Truncate(time.Minute)+time.Minute)
	by := strings.Join(labels, ", ")
	for _, sel := range sels {
		series, err := h.QueryRange(ctx, "count by ("+by+") ("+sel+")", lo, hi, step)
		if err != nil {
			return nil, err
		}
		for _, se := range series {
			if len(se.Samples) > 0 {
				seen.Add(se.Labels)
			}
		}
	}
	return seen, nil
}

// seenSelectors are the selectors an expression's series are counted by
// for what it reads (SeenFrom): those it reads, each that only narrows
// another in place of its stand-ins, each once, in order.
func seenSelectors(expr string) ([]string, error) {
	sels, err := rules.Selectors(expr)
	if err != nil {
		return nil, err
	}
	stands, err := rules.StandIns(expr)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sel := range sels {
		in := false
		for _, x := range stands {
			if x.For == sel {
				in = true
				if !slices.Contains(out, x.Sel) {
					out = append(out, x.Sel)
				}
			}
		}
		if !in && !slices.Contains(out, sel) {
			out = append(out, sel)
		}
	}
	return out, nil
}
