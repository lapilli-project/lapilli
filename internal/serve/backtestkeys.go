package serve

import (
	"context"
	"crypto/subtle"
	"strings"
	"sync"
	"time"
)

// backtestKeysListed is how many backtests' keys a listing reads at most,
// the latest asked first: more than the ledger keeps, 200 ended and 1,000
// waiting at most (--backtest-queue), and the one running.
const backtestKeysListed = 2048

// btKeys are the key hashes of the backtests kept, by id, as this process
// knows them: each it queued, and each the ledger held when last listed. A
// job's GET or DELETE is told from them in memory, its id and key: one
// whose key is right is read from the ledger, and what anyone guessing
// does stands nowhere in its way. A pair not known waits for a listing
// begun after it came, so that a backtest queued through another replica
// is found; listings are one at a time, every apart at least, however
// many wait.
type btKeys struct {
	// list reads the ledger's backtests' key hashes, by id; every is the
	// least time between two listings' starts, within how long a pair not
	// known waits for one.
	list          func(context.Context) (map[string]string, error)
	every, within time.Duration
	// life ends the listings with serve.
	life context.Context

	mu     sync.Mutex
	hashes map[string]string
	// during are the backtests queued here while a listing ran, nil when
	// none runs: kept beside what it read, which may be from before them.
	during map[string]string
	// begun and ended count the listings begun and ended, in turn: one a
	// pair waits for has its number, wanted the highest one asked for.
	begun, ended, wanted uint64
	running              bool
	last                 time.Time
	err                  error // the latest listing's
	// changed is closed when a listing ends, and made anew.
	changed chan struct{}
}

func newBTKeys(life context.Context, list func(context.Context) (map[string]string, error)) *btKeys {
	return &btKeys{list: list, every: time.Second, within: 2 * time.Second, life: life,
		hashes: map[string]string{}, changed: make(chan struct{})}
}

// put keeps a backtest queued here. Past what a listing reads, a listing
// is begun, which keeps those the ledger keeps.
func (k *btKeys) put(id, hash string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.hashes[id] = hash
	if k.during != nil {
		k.during[id] = hash
	}
	if len(k.hashes) > backtestKeysListed {
		k.want()
	}
}

// drop lets go of a backtest the ledger no longer keeps.
func (k *btKeys) drop(id string) {
	k.mu.Lock()
	delete(k.hashes, id)
	k.mu.Unlock()
}

// known says whether hash is the key hash of id's backtest, as kept here,
// compared in constant time.
func (k *btKeys) known(id, hash string) bool {
	k.mu.Lock()
	want, ok := k.hashes[id]
	k.mu.Unlock()
	if !ok {
		want = strings.Repeat("0", 64)
	}
	return subtle.ConstantTimeCompare([]byte(hash), []byte(want)) == 1 && ok
}

// begunNow is how many listings have begun: a listing whose number is
// past it begins after now (listedSince).
func (k *btKeys) begunNow() uint64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.begun
}

// listedSince reports whether a listing begun after begun did, as
// begunNow gave it, has ended well.
func (k *btKeys) listedSince(begun uint64) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.ended > begun && k.err == nil
}

// kick asks for a listing, and doesn't wait for it.
func (k *btKeys) kick() {
	k.mu.Lock()
	k.want()
	k.mu.Unlock()
}

// await waits, within k.within and ctx, for a listing begun after it was
// called to end: false when none did, and the listing's error.
func (k *btKeys) await(ctx context.Context) (bool, error) {
	k.mu.Lock()
	need := k.want()
	k.mu.Unlock()
	t := time.NewTimer(k.within)
	defer t.Stop()
	for {
		k.mu.Lock()
		ended, err, changed := k.ended, k.err, k.changed
		k.mu.Unlock()
		if ended >= need {
			return true, err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return false, nil
		case <-t.C:
			return false, nil
		}
	}
}

// want asks, with k.mu held, for a listing begun after now, and gives its
// number; the listings run, one at a time, while one is wanted.
func (k *btKeys) want() uint64 {
	need := k.begun + 1
	k.wanted = max(k.wanted, need)
	if !k.running {
		k.running = true
		go k.listings()
	}
	return need
}

// listings lists the ledger's keys while a listing is wanted, every apart
// at least, and ends with serve.
func (k *btKeys) listings() {
	for {
		k.mu.Lock()
		if k.begun >= k.wanted || k.life.Err() != nil {
			k.running = false
			k.mu.Unlock()
			return
		}
		pause := time.Until(k.last.Add(k.every))
		k.mu.Unlock()
		if pause > 0 {
			t := time.NewTimer(pause)
			select {
			case <-t.C:
			case <-k.life.Done():
				t.Stop()
			}
			continue
		}
		k.mu.Lock()
		k.begun++
		n := k.begun
		k.last = time.Now()
		k.during = map[string]string{}
		k.mu.Unlock()
		ctx, cancel := context.WithTimeout(k.life, 30*time.Second)
		m, err := k.list(ctx)
		cancel()
		k.mu.Lock()
		if err == nil {
			if m == nil {
				m = map[string]string{}
			}
			for id, h := range k.during {
				m[id] = h
			}
			k.hashes = m
		}
		k.during, k.ended, k.err = nil, n, err
		close(k.changed)
		k.changed = make(chan struct{})
		k.mu.Unlock()
	}
}
