package serve

import (
	"errors"
	"sync"
	"time"

	"lapilli.dev/internal/ledger"
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

// tries are, by reader, when each of their tries in the last hour was.
type tries struct {
	sync.Mutex
	at map[string][]time.Time
}

// take counts a try of who's at now, and reports whether they may: false,
// and nothing counted, past triesAnHour in the hour before now.
func (t *tries) take(who string, now time.Time) bool {
	t.Lock()
	defer t.Unlock()
	if t.at == nil {
		t.at = map[string][]time.Time{}
	}
	who = ledger.FoldName(who)
	var kept []time.Time
	for _, x := range t.at[who] {
		if now.Sub(x) < time.Hour {
			kept = append(kept, x)
		}
	}
	if len(kept) >= triesAnHour {
		t.at[who] = kept
		return false
	}
	t.at[who] = append(kept, now)
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
