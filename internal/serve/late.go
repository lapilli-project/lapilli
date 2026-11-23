package serve

import (
	"context"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/verdict"
)

// An alert after the incident (docs/how-it-judges.md): a firing whose
// condition held in an incident, but whose "for" and how late its metrics
// arrive ran past its end, is that incident's alert, too late. The ledger
// works it out as it is read (ledger.LateOf); the pages say it as lapilli
// verdict's report does: on an incident nothing caught in time, the rules
// that fired after its end, and on a rule, how many of its black firings
// came after one.

// lateCache keeps what lateOf worked out, by the ledger's generation and
// the span: the incidents page, a rule's page and the rules ask for it.
type lateCache struct {
	sync.Mutex
	gen int64
	by  map[[2]int64]ledger.Late
}

// lateOf are the firings of [from, to) that came after an incident was
// over, and the rules that fired after each incident that started in it,
// judged with o, how long each rule's condition held taken from the
// versions in effect over the span (withHeld).
func (s *Server) lateOf(ctx context.Context, from, to time.Time, o verdict.Options) (ledger.Late, error) {
	gen, key := s.l.Gen(), [2]int64{from.Unix(), to.Unix()}
	s.lates.Lock()
	if s.lates.gen == gen && s.lates.by != nil {
		if x, ok := s.lates.by[key]; ok {
			s.lates.Unlock()
			return x, nil
		}
	}
	s.lates.Unlock()
	o, err := s.withHeld(ctx, o, from, to)
	if err != nil {
		return ledger.Late{}, err
	}
	x, err := s.l.LateOf(ctx, from, to, o, false)
	if err != nil {
		return x, err
	}
	s.lateCut(x.Cut, from, to)
	s.lates.Lock()
	if s.lates.gen != gen || s.lates.by == nil || len(s.lates.by) >= 64 {
		s.lates.gen, s.lates.by = gen, map[[2]int64]ledger.Late{}
	}
	s.lates.by[key] = x
	s.lates.Unlock()
	return x, nil
}

// lateEach is how many of the rules that fired after an incident was over
// its line names, as lapilli verdict's report does.
const lateEach = 3

// afterText says, of an incident nothing caught in time, the rules that
// fired once it was over, each by how long after its end: "Errors +1m,
// Disk +3m". With none, "".
func afterText(l string, in ledger.Incident, fired []verdict.Catch, unknown time.Duration) string {
	end := in.EndOr(unknown).Sub(in.Start)
	var parts []string
	for i, c := range fired {
		if i == lateEach {
			parts = append(parts, tr(l, "and %d more", len(fired)-lateEach))
			break
		}
		parts = append(parts, c.Rule+" "+verdict.Delay(c.Delay-end))
	}
	return strings.Join(parts, ", ")
}
