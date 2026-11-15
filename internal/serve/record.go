package serve

import (
	"context"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// A span's record is kept when an incident its firings are judged against
// started in it, or in the two weeks before its end (verdict.RecordKept):
// without one, a firing that caught nothing can't be told from one nobody
// wrote down. No rule is noisy over such a span, and its precision isn't
// known. The tallies read when the incidents started once, with them, and
// say it of each week and of the weeks a page shows.

// readRecord reads, for t's weeks, when the incidents that keep their record
// started: from two weeks before the first to the end of the last.
func (s *Server) readRecord(ctx context.Context, t *tallies) error {
	first, last := t.weeks[0], t.weeks[len(t.weeks)-1]
	starts, err := s.l.IncidentStarts(ctx, s.l.Midnight(first).Add(-verdict.RecordLookback), s.l.Midnight(last+7))
	if err != nil {
		return err
	}
	t.starts, t.mid, t.at = starts, s.l.Midnight, s.now()
	t.kept = make(map[int64]bool, len(t.weeks))
	for _, w := range t.weeks {
		to := t.mid(w + 7)
		if now := t.until(t.at); to.After(now) {
			to = now // the week under way: to now
		}
		t.kept[w] = t.recorded(t.mid(w), to)
	}
	return nil
}

// recordBefore are the incidents that keep the record of [from, to) and
// started before from: what verdict.Judge is given beside the period's own,
// so that it says what the ledger's report says (ledger.RecordKept). It
// judges no firing against them.
func (s *Server) recordBefore(ctx context.Context, from, to time.Time) ([]incident.Incident, error) {
	before, err := s.l.Incidents(ctx, verdict.RecordFrom(from, to), from, false)
	out := make([]incident.Incident, len(before))
	for i, in := range before {
		out[i] = in.Incident
	}
	return out, err
}

// recorded reports whether the record of [from, to) is kept, by the
// incidents the tallies read.
func (t *tallies) recorded(from, to time.Time) bool { return verdict.RecordKept(from, to, t.starts) }

// keptOver reports whether the record of the last n of t's weeks is kept,
// to when they end.
func (t *tallies) keptOver(n int) bool {
	return t.recorded(t.mid(t.weeks[len(t.weeks)-n]), t.until(t.at))
}
