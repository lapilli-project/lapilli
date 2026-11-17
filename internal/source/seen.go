package source

import (
	"context"
	"regexp"
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
// span, as where its data begins is looked for (FirstSample). A selector
// that only narrows another the expression reads is counted through its
// stand-in (rules.StandIns: http_requests_total{code!=""} for
// http_requests_total{code=~"5.."}), as only counting the 5xx would see
// only the services that had one; and not at all when the selector its
// stand-in is made of is counted itself, whose series hold the stand-in's.
//
// Label by label (docs/design/signals.md, "The record, signal by
// signal"): the values the selectors' series have; a selector whose series
// lack a label adds nothing for it (a gate, and on() up{job="api"}); and
// when none of their series carries a label, which the expression makes
// (label_replace, a probe's target relabelled), the signal is taken to
// have every value of it, as it is of every label of an expression that
// reads no selector, and of a name PromQL can't count by. A look that
// finds no series at all learns nothing. A selector that fails adds
// nothing, the others' counts kept: complete is false, and the look is
// to be tried again.
func SeenFrom(ctx context.Context, h replay.History, expr string, labels []string, lo, hi time.Time) (seen *verdict.Seen, complete bool, err error) {
	seen = verdict.NewSeen(labels)
	var by []string
	for _, l := range labels {
		if labelName.MatchString(l) {
			by = append(by, l)
		} else {
			seen.Every(l)
		}
	}
	if len(by) == 0 {
		return seen, true, nil
	}
	sels, err := seenSelectors(expr)
	if err != nil {
		return nil, false, err
	}
	if len(sels) == 0 {
		for _, l := range by {
			seen.Every(l)
		}
		return seen, true, nil
	}
	step := max(time.Hour, (hi.Sub(lo)/1000).Truncate(time.Minute)+time.Minute)
	carried, any := map[string]bool{}, false
	complete = true
	for _, sel := range sels {
		series, qerr := h.QueryRange(ctx, "count by ("+strings.Join(by, ", ")+") ("+sel+")", lo, hi, step)
		if qerr != nil {
			complete, err = false, qerr
			continue
		}
		for _, se := range series {
			if len(se.Samples) == 0 {
				continue
			}
			any = true
			for _, l := range by {
				if v, ok := se.Labels[l]; ok {
					carried[l] = true
					seen.AddValue(l, v)
				}
			}
		}
	}
	if complete && any {
		for _, l := range by {
			if !carried[l] {
				seen.Every(l)
			}
		}
	}
	return seen, complete, err
}

// labelName is what PromQL counts by unquoted.
var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// seenSelectors are the selectors an expression's series are counted by
// for what it reads (SeenFrom): those it reads, each that only narrows
// another in place of its stand-ins, but for a stand-in made of a selector
// counted itself, each once, in order.
func seenSelectors(expr string) ([]string, error) {
	sels, err := rules.Selectors(expr)
	if err != nil {
		return nil, err
	}
	stands, err := rules.StandIns(expr)
	if err != nil {
		return nil, err
	}
	narrows := func(sel string) bool { return slices.ContainsFunc(stands, func(x rules.StandIn) bool { return x.For == sel }) }
	var out []string
	for _, sel := range sels {
		if !narrows(sel) {
			if !slices.Contains(out, sel) {
				out = append(out, sel)
			}
			continue
		}
		for _, x := range stands {
			if x.For != sel || slices.Contains(out, x.Sel) {
				continue
			}
			// Its stand-in's series are some of the selector it is made
			// of: counting that one counts them.
			if slices.Contains(sels, x.Of) && !narrows(x.Of) {
				continue
			}
			out = append(out, x.Sel)
		}
	}
	return out, nil
}
