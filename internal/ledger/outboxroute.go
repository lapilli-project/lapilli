package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// Routes (docs/design/notifications.md, "Who is told what"): which of the
// places what happens may be told to each event goes to is serve's to say,
// by the routes --config declares. The ledger asks, as it writes the
// outbox's rows, and writes one for each place it is given back, so that a
// place no route names gets no row to send.

// Router picks, of the places an event may be told to, those it goes to:
// given the event's kind, what is told of it, as JSON, and the places, it
// gives back those of them to tell. It is asked in the write that tells,
// so it reads nothing and waits on nothing.
type Router func(event string, about []byte, dests []string) []string

// RouteBy sets the Router the outbox's rows are written by; nil, or
// never set, tells every place.
func (l *Ledger) RouteBy(r Router) {
	if r == nil {
		l.router.Store(nil)
		return
	}
	l.router.Store(&r)
}

// routed are the places of dests an event goes to.
func (l *Ledger) routed(event string, about []byte, dests []string) []string {
	r := l.router.Load()
	if r == nil || len(dests) == 0 {
		return dests
	}
	return (*r)(event, about, dests)
}

// routeTold keeps, of what is told of one event, what goes to the places
// the Router picks. Each place is told the same of an event, but for a
// digest's, which the Router leaves as it is: what the first is told is
// what the Router reads.
func (l *Ledger) routeTold(event string, told []Told) ([]Told, error) {
	if l.router.Load() == nil || len(told) == 0 {
		return told, nil
	}
	about, err := json.Marshal(told[0].About)
	if err != nil {
		return nil, err
	}
	dests := make([]string, len(told))
	for i, x := range told {
		dests[i] = x.Dest
	}
	keep := l.routed(event, about, dests)
	return slices.DeleteFunc(slices.Clone(told), func(x Told) bool { return !slices.Contains(keep, x.Dest) }), nil
}

// lastError is the error of a place's latest-tried notice still unsent,
// read as Standing reads them: by the outbox's index on its place, through
// its latest FailedCounted and one waiting, or else given up on, the one
// tried again last (its next try the latest) first.
const (
	lastErrorWaiting = `SELECT error FROM
	(SELECT error, next_at FROM outbox WHERE dest = ? AND sent_at IS NULL AND next_at < ? ORDER BY sent_at DESC, next_at DESC LIMIT ?) w
	WHERE error != '' ORDER BY next_at DESC LIMIT 1`
	lastErrorGivenUp = `SELECT error FROM
	(SELECT error FROM outbox WHERE dest = ? AND sent_at IS NULL AND next_at >= ? ORDER BY sent_at DESC, next_at DESC LIMIT ?) g
	WHERE error != '' LIMIT 1`
)

// LastError is why the latest-tried notice to dest still unsent failed:
// one waiting to be tried again, or else one given up on; "" for none.
// Bounded as Standing is, however many there are.
func (l *Ledger) LastError(ctx context.Context, dest string) (string, error) {
	for _, q := range []string{lastErrorWaiting, lastErrorGivenUp} {
		var why string
		err := l.r.QueryRowContext(ctx, q, dest, givenUpAt, FailedCounted+1).Scan(&why)
		if err == nil {
			return why, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	return "", nil
}

// maxPlaces is how many of the outbox's places UnsentPlaces looks through
// at most: a place is a destination's name, which the flags give, a few
// dozen at most.
const maxPlaces = 1000

// UnsentPlaces are the outbox's places whose names begin with prefix that
// hold a notice unsent, waiting or given up on, by name: those of a
// destination the flags may no longer give. Read a place at a time by the
// outbox's index on its place, the next name after the last and whether
// it has one unsent, never row by row, so that what is read is bounded by
// how many places there are, however many notices each holds. In the
// database's order of names, the prefix looked at in each.
func (l *Ledger) UnsentPlaces(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	after := ""
	for range maxPlaces {
		var dest string
		err := l.r.QueryRowContext(ctx, "SELECT dest FROM outbox WHERE dest > ? ORDER BY dest LIMIT 1", after).Scan(&dest)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		after = dest
		if !strings.HasPrefix(dest, prefix) {
			continue
		}
		var one int
		err = l.r.QueryRowContext(ctx, "SELECT 1 FROM outbox WHERE dest = ? AND sent_at IS NULL LIMIT 1", dest).Scan(&one)
		switch {
		case err == nil:
			out = append(out, dest)
		case !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
	}
	slices.Sort(out)
	return out, nil
}
