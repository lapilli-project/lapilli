package source

import (
	"context"
	"slices"
	"time"

	"lapilli.dev/internal/replay"
)

// The jobs a selector's series come from, for the range check
// (docs/design/content.md, "A range over a selector that names no job"):
// asked of the history, count by (job) of the selector over the ten
// minutes before at, a point every five, as one bounded query each.

// How the jobs are asked: MaxAsk selectors at most each time, for
// JobsBudget in all; maxJobsOf jobs kept of a selector; jobsWait, how long
// one query may take.
const (
	MaxAsk     = 200
	JobsBudget = 30 * time.Second
	maxJobsOf  = 1000
	jobsWait   = 10 * time.Second
)

// JobsQuery is what the jobs a selector's series come from are asked by:
// count by (job) of it, a series a job. The selector is one plain
// selector, as content.RangeUse.Selector writes it.
func JobsQuery(selector string) string { return "count by (job) (" + selector + ")" }

// JobsOf asks the history which jobs each selector's series come from, at
// most MaxAsk of them, one query each, in turn, for JobsBudget at most or
// until ctx ends.
// A selector whose query fails, or gives no job, isn't in what it gives:
// its range is counted as before. failed is how many failed, and not
// asked those ctx ended before.
func JobsOf(ctx context.Context, h replay.History, sels []string, at time.Time) (jobs map[string][]string, failed, notAsked int) {
	ctx, cancel := context.WithTimeout(ctx, JobsBudget)
	defer cancel()
	jobs = map[string][]string{}
	for i, sel := range sels {
		if i >= MaxAsk || ctx.Err() != nil {
			return jobs, failed, len(sels) - i
		}
		one, cancel := context.WithTimeout(ctx, jobsWait)
		series, err := h.QueryRange(one, JobsQuery(sel), at.Add(-10*time.Minute), at, 5*time.Minute)
		cancel()
		if err != nil {
			failed++
			continue
		}
		var js []string
		for _, s := range series {
			if j := s.Labels["job"]; j != "" && len(s.Samples) > 0 && !slices.Contains(js, j) && len(js) < maxJobsOf {
				js = append(js, j)
			}
		}
		if len(js) > 0 {
			slices.Sort(js)
			jobs[sel] = js
		}
	}
	return jobs, failed, 0
}
