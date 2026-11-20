package serve

import (
	"context"
	"errors"
	"sync"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/rules"
)

// triesAnHour are the proposals a reader may ask for in an hour that read
// the rules again (propose), made or refused: three times as many as they
// may make (ledger.ProposalsAnHour), with the agents acting for them. A
// refusal after the rules are read again (it has that for already, it
// isn't replayed, its history is too short to retire it) costs that read,
// of Grafana, a ruler or a cloud's alarms among them, and isn't a
// proposal the ledger counts. Kept in memory, a replica's own.
const triesAnHour = 3 * ledger.ProposalsAnHour

// errTooManyTries is a proposal asked for past triesAnHour.
var errTooManyTries = errors.New("you have asked for as many proposals in the last hour as anyone may, made or refused: try again later")

// tries are, by reader, when each of their tries in the last hour was. An
// agent's are counted apart from the person's it acts for, so that an
// agent asking again and again uses up its own, not theirs; people who
// share a name ("anonymous", where no one is named) share theirs, as they
// share the twenty proposals an hour.
type tries struct {
	sync.Mutex
	at map[string][]time.Time
	// limit is how many an hour a reader may: triesAnHour when 0.
	limit int
}

// most is how many tries an hour a reader may.
func (t *tries) most() int {
	if t.limit > 0 {
		return t.limit
	}
	return triesAnHour
}

// triesKey is whose tries a try is: who's, or who's agents'.
func triesKey(who string, agent bool) string {
	if agent {
		return ledger.FoldName(who) + "\x00agent"
	}
	return ledger.FoldName(who)
}

// inHour are who's tries in the hour before now.
func (t *tries) inHour(key string, now time.Time) []time.Time {
	var kept []time.Time
	for _, x := range t.at[key] {
		if now.Sub(x) < time.Hour {
			kept = append(kept, x)
		}
	}
	return kept
}

// may reports whether who (or who's agents) may try now, counting nothing.
func (t *tries) may(who string, agent bool, now time.Time) bool {
	t.Lock()
	defer t.Unlock()
	return len(t.inHour(triesKey(who, agent), now)) < t.most()
}

// take counts a try of who's (or who's agents') at now, and reports
// whether they may: false, and nothing counted, past triesAnHour in the
// hour before now.
func (t *tries) take(who string, agent bool, now time.Time) bool {
	t.Lock()
	defer t.Unlock()
	if t.at == nil {
		t.at = map[string][]time.Time{}
	}
	key := triesKey(who, agent)
	kept := t.inHour(key, now)
	if len(kept) >= t.most() {
		t.at[key] = kept
		return false
	}
	t.at[key] = append(kept, now)
	if len(t.at) > 1000 {
		// Readers with none in the hour are forgotten, now and then.
		for k, v := range t.at {
			if now.Sub(v[len(v)-1]) >= time.Hour {
				delete(t.at, k)
			}
		}
	}
	return true
}

// mayPropose reports why a proposal by who (agent, for an agent acting for
// them) would be refused before anything is read for it: the ledger's
// (one unfinished for the rule, the hour's twenty) or the tries used up.
// propose asks it first; a bulk proposal, and the single form, before they
// wait for the heavy turn too, so that one refused waits for nothing.
func (s *Server) mayPropose(ctx context.Context, rule, by string, agent bool) error {
	// A team's editor, of their team's rules alone (bounds.go).
	if err := s.mayProposeHere(ctx, rule); err != nil {
		return err
	}
	return s.mayProposeBy(ctx, rule, by, agent)
}

// mayProposeAdd is mayPropose of a rule added: a team's editor's when the
// team its name and labels would give it is theirs (addOwners), as no rule
// in effect has its name for mayProposeHere to ask of.
func (s *Server) mayProposeAdd(ctx context.Context, a rules.Added, by string, agent bool) error {
	if _, err := s.addOwners(ctx, a.Alert, a.Labels); err != nil {
		return err
	}
	return s.mayProposeBy(ctx, a.Alert, by, agent)
}

// mayProposeBy is mayPropose past the team: the ledger's, and the tries.
func (s *Server) mayProposeBy(ctx context.Context, rule, by string, agent bool) error {
	if err := s.l.MayPropose(ctx, rule, by); err != nil {
		return err
	}
	if by != ledger.Proposer && !s.tries.may(by, agent, time.Now()) {
		return errTooManyTries
	}
	return nil
}
