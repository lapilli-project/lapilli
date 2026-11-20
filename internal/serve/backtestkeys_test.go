package serve

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// btKeys under concurrent put, known, await, kick and drop, with listings
// that come and go, every tenth failing, and serve's life ending at the
// end: run with -race. Each pair put is known at once, and stays known
// across the listings that ran while it was put, failed or not; once
// serve's life ends, the listings stop.
func TestBTKeysConcurrent(t *testing.T) {
	life, end := context.WithCancel(context.Background())
	var mu sync.Mutex
	kept := map[string]string{}
	var lists atomic.Int64
	k := newBTKeys(life, func(ctx context.Context) (map[string]string, error) {
		if lists.Add(1)%10 == 0 {
			return nil, errors.New("a made-up ledger error")
		}
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]string, len(kept))
		for id, h := range kept {
			out[id] = h
		}
		return out, nil
	})
	k.every, k.within = 0, time.Second
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				id, h := fmt.Sprintf("%02d%030d", g, i), fmt.Sprintf("%064d", i)
				mu.Lock()
				kept[id] = h // kept by the ledger, then put, as postBacktest does
				mu.Unlock()
				k.put(id, h)
				if !k.known(id, h) {
					t.Errorf("%s not known once put", id)
					return
				}
				switch i % 4 {
				case 0:
					k.await(context.Background())
				case 1:
					k.kick()
				case 2:
					k.known(fmt.Sprintf("%032d", i), "x")
				case 3:
					gone := fmt.Sprintf("%02d%030d", g, 1000+i)
					k.put(gone, h)
					k.drop(gone)
				}
				if !k.known(id, h) {
					t.Errorf("%s not known after a listing", id)
					return
				}
			}
		}()
	}
	wg.Wait()
	end()
	k.kick() // asked for after serve's life ended: none runs
	for {
		k.mu.Lock()
		running := k.running
		k.mu.Unlock()
		if !running {
			break
		}
		runtime.Gosched()
	}
	if lists.Load() == 0 {
		t.Error("never listed")
	}
}
