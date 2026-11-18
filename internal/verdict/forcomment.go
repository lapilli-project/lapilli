package verdict

import (
	"cmp"
	"slices"
	"strings"

	"lapilli.dev/internal/rules"
)

// UnreadHistory is, in a comment on a pull request, why a rule whose query
// the history didn't answer couldn't be replayed: rules.HistoryUnread,
// without why, which the job's log says.
const UnreadHistory = "query failed: the history couldn't be read; the job's log says why"

// ForComment readies the reasons rules couldn't be replayed for a comment
// on a pull request, which more people read than the job's log: lapilli
// verdict --markdown's, and lapilli serve's backtests'. A query that failed
// as the history couldn't be read (rules.HistoryUnread: not reached, not
// let in, turned away, answered by a proxy's page), which is all but the
// history refusing the query as written (prom.Refused, the same line
// --fail-unreplayed draws), says unread in its place, its whole reason
// given to said, with the rule and the side, before or after. The rest is
// as scrub gives it: where the history is taken out of it.
func ForComment(c *Comparison, unread string, scrub func(string) string, said func(rule, side, why string)) {
	for _, rep := range []*Report{c.Before, c.After} {
		if rep == nil {
			continue
		}
		side := "after"
		if rep == c.Before {
			side = "before"
		}
		for i := range rep.Rules {
			r := &rep.Rules[i]
			switch {
			case r.Unsupported == "" || r.Unsupported == rules.Unfinished:
			case strings.HasPrefix(r.Unsupported, rules.HistoryUnread):
				if said != nil {
					said(r.Rule, side, r.Unsupported)
				}
				r.Unsupported = unread
			default:
				r.Unsupported = scrub(r.Unsupported)
			}
		}
	}
}

// RecordingChanges are the recording rules that differ between was, those
// in place, and is, those proposed: by what each records and its group,
// changed when both have it otherwise, added or removed when one hasn't.
func RecordingChanges(was, is []rules.Recording) []RecordingChange {
	type id struct{ record, group string }
	keys := func(rs []rules.Recording) map[id][]string {
		m := map[id][]string{}
		for _, r := range rs {
			k := id{r.Record, r.Group}
			m[k] = append(m[k], r.Key())
		}
		for k := range m {
			slices.Sort(m[k])
		}
		return m
	}
	before, after := keys(was), keys(is)
	var out []RecordingChange
	for k, b := range before {
		a, ok := after[k]
		switch {
		case !ok:
			out = append(out, RecordingChange{Record: k.record, Group: k.group, Change: "removed"})
		case !slices.Equal(a, b):
			out = append(out, RecordingChange{Record: k.record, Group: k.group, Change: "changed"})
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			out = append(out, RecordingChange{Record: k.record, Group: k.group, Change: "added"})
		}
	}
	slices.SortFunc(out, func(a, b RecordingChange) int {
		return cmp.Or(strings.Compare(a.Record, b.Record), strings.Compare(a.Group, b.Group))
	})
	return out
}
