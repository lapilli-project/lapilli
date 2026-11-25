package source

import (
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/response"
	"lapilli.dev/internal/verdict"
)

// Responses finds each firing's response and whether it was useful
// (docs/design/responses.md) from the pages a paging tool sent and the
// changes made to the alert's target, as internal/response reads them. It
// is the verdict.Responder of lapilli verdict: internal/response reads the
// ledger's types, which read internal/verdict, so verdict holds the
// interface and this the work.
type Responses struct {
	// Evidence is what responses are found among.
	Evidence response.Evidence

	mu   sync.Mutex
	used map[response.PageKey]bool // the pages (by source and id) some firing had
}

var _ verdict.Responder = (*Responses)(nil)

// Respond is the firing's response, and whether it was useful, as
// internal/response decides them.
func (r *Responses) Respond(f firing.Firing, within time.Duration, now time.Time, white, judged bool, answer string) verdict.FiringResponse {
	resp := r.Evidence.Respond(response.Firing{Rule: f.Rule, Labels: f.Labels, Start: f.Start, End: f.End, On: f.End.IsZero()}, within, now)
	out := verdict.FiringResponse{Kind: kindOf(resp.Type)}
	switch u := response.Useful(white, judged, response.Answer(answer), resp, resp.Known); u {
	case response.IsUseful, response.NotUseful:
		b := u == response.IsUseful
		out.Useful = &b
	}
	if c := resp.Change; c != nil {
		labels := c.Labels
		if c.Kind != response.Deploy {
			labels = map[string]string{"namespace": c.Namespace, strings.ToLower(c.WorkloadKind): c.Workload}
		}
		out.Change = &verdict.ResponseChange{Kind: string(c.Kind), At: c.At, Labels: labels, Routine: resp.Type != response.Changed}
	}
	if p := resp.Page; p != nil {
		out.Page = &verdict.ResponsePage{ID: p.ID, Sent: p.Sent, Acknowledged: p.Acknowledged, Resolved: p.Resolved, ResolvedBy: p.ResolvedBy}
		r.mu.Lock()
		if r.used == nil {
			r.used = map[response.PageKey]bool{}
		}
		r.used[p.Key()] = true
		r.mu.Unlock()
	}
	return out
}

func kindOf(t response.Type) string {
	switch t {
	case response.Changed:
		return verdict.ResponseChanged
	case response.ChangedRoutine:
		return verdict.ResponseRoutine
	case response.Acknowledged:
		return verdict.ResponseAcknowledged
	case response.Unanswered:
		return verdict.ResponseUnanswered
	}
	return verdict.ResponseNone
}

// Totals are what was read, and how many pages no firing had.
func (r *Responses) Totals() verdict.ResponseTotals {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := verdict.ResponseTotals{Pages: r.Evidence.Pages.Len(), PagesUnmatched: r.Evidence.Pages.Unmatched(r.used)}
	if cs := r.Evidence.Changes; cs != nil {
		t.Changes, t.AutoscalersKnown, t.Rolled = len(cs.Changes()), cs.AutoscalersKnown, cs.Rolled
	}
	return t
}

// keeps reports whether a longer "for" would keep a useful firing: a firing
// of its rule replayed with it, of the same labels, that overlaps it and
// starts before the change its usefulness rests on; or that caught every
// incident it caught (docs/design/responses.md, "Which rules are noisy").
// alt are the longer "for"'s firings, judged against the same incidents, to
// of the period's end.
func keeps(j verdict.Judged, alt []verdict.Judged, to time.Time) bool {
	end := j.End
	if end.IsZero() {
		end = to
	}
	var change time.Time
	if j.Response != nil && j.Response.Kind == verdict.ResponseChanged && j.Response.Change != nil {
		change = j.Response.Change.At
	}
	for _, a := range alt {
		aend := a.End
		if aend.IsZero() {
			aend = to
		}
		if !change.IsZero() && sameLabels(a.Labels, j.Labels) && !a.Start.After(end) && !aend.Before(j.Start) && a.Start.Before(change) {
			return true
		}
		if j.White && len(j.Incidents) > 0 && a.White && containsAll(a.Incidents, j.Incidents) {
			return true
		}
		// A firing useful by a person's word, with nothing else to rest on:
		// the alert still firing then.
		if change.IsZero() && !j.White && sameLabels(a.Labels, j.Labels) && !a.Start.After(end) && !aend.Before(j.Start) {
			return true
		}
	}
	return false
}

func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
