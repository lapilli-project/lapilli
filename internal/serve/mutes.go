package serve

import (
	"context"
	"strconv"
	"sync"
	"time"

	"lapilli.dev/internal/ledger"
)

// Mutes, from Alertmanager's event recorder (docs/design/mutes.md): an
// alert a silence or an inhibition kept from a notification. The recorder
// tells of one each time Alertmanager asks whether an alert is muted, at
// each flush of its group and each time its API lists alerts, so a muted
// alert is told of every few seconds; the ledger keeps it a row an hour,
// and this replica writes one again only when it makes the row a minute
// longer, or more.

// muteSlack is how much longer a mute told of again makes the row kept
// before it is written.
const muteSlack = time.Minute

// mutesRemembered is how many rows of mutes a replica remembers keeping:
// past it, it forgets them all, and writes each once again.
const mutesRemembered = 20_000

// maxMutesPerDay is how many rows of mutes are kept in a day (UTC) at
// most: a token's holder can't fill the disk with them. A muted alert
// makes one an hour.
var maxMutesPerDay = 200_000

// mutesKept are the rows of mutes this replica kept lately, by key and
// hour: from when to when, Unix seconds.
type mutesKept struct {
	sync.Mutex
	rows map[string][2]int64
	day  struct {
		start, counted time.Time
		n              int
		warned         bool
	}
}

// fresh are the mutes of a request, one for each row they make, those that
// change what this replica kept leaving out: told of within muteSlack of
// the row kept, they change nothing a page shows.
func (k *mutesKept) fresh(ms []ledger.Mute) []ledger.Mute {
	if len(ms) == 0 {
		return nil
	}
	k.Lock()
	defer k.Unlock()
	byKey := map[string]int{}
	var out []ledger.Mute
	for _, m := range ms {
		id := m.Key() + "/" + strconv.FormatInt(m.At.Unix()/3600, 10)
		if i, ok := byKey[id]; ok {
			if m.At.Before(out[i].At) {
				out[i].At = m.At
			}
			if m.At.After(out[i].Last) {
				out[i].Last = m.At
			}
			continue
		}
		byKey[id] = len(out)
		m.Last = m.At
		out = append(out, m)
	}
	slack := int64(muteSlack / time.Second)
	kept := out[:0]
	for _, m := range out {
		id := m.Key() + "/" + strconv.FormatInt(m.At.Unix()/3600, 10)
		if r, ok := k.rows[id]; ok && m.At.Unix() >= r[0]-slack && m.Last.Unix() <= r[1]+slack {
			continue
		}
		kept = append(kept, m)
	}
	return kept
}

// kept remembers the rows written.
func (k *mutesKept) kept(ms []ledger.Mute) {
	k.Lock()
	defer k.Unlock()
	if k.rows == nil || len(k.rows)+len(ms) > mutesRemembered {
		k.rows = map[string][2]int64{}
	}
	for _, m := range ms {
		id := m.Key() + "/" + strconv.FormatInt(m.At.Unix()/3600, 10)
		r, ok := k.rows[id]
		if !ok {
			r = [2]int64{m.At.Unix(), m.Last.Unix()}
		}
		k.rows[id] = [2]int64{min(r[0], m.At.Unix()), max(r[1], m.Last.Unix())}
	}
}

// keepMutes keeps the rows of mutes a request makes, within the day's
// most: past it, they are passed over until the next day (UTC), and that
// is logged once; notifications go on being kept.
func (s *Server) keepMutes(ctx context.Context, ms []ledger.Mute) error {
	k := &s.port.mutes
	k.Lock()
	now := s.now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	// With replicas, counted again from the ledger each minute, as the
	// alerts of a day are (dayRoom).
	if !k.day.start.Equal(start) || s.l.Shared() && time.Since(k.day.counted) >= time.Minute {
		n, err := s.l.MutesSince(ctx, start)
		if err != nil {
			k.Unlock()
			return err
		}
		if !k.day.start.Equal(start) {
			k.day.warned = false
		}
		k.day.start, k.day.n, k.day.counted = start, n, time.Now()
	}
	if k.day.n+len(ms) > maxMutesPerDay {
		if !k.day.warned {
			k.day.warned = true
			s.log.Warn("passing over mutes until tomorrow (UTC): as many were kept today as are in a day", "kept", k.day.n, "most", maxMutesPerDay)
		}
		k.Unlock()
		return nil
	}
	k.Unlock()
	added, _, err := s.l.KeepMutes(ctx, ms)
	if err != nil {
		return err
	}
	k.Lock()
	k.day.n += added
	k.Unlock()
	k.kept(ms)
	return nil
}
