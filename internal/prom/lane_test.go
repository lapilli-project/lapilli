package prom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestLaneShare: while the lane has work, it has its quarter of the rate
// whatever the runs ask, the runs asking all the while, a millisecond at a
// time, before the lane each time; the two together no more than the rate;
// and once the lane has none, the runs have the whole rate again: turns
// counted on a clock moved by hand.
func TestLaneShare(t *testing.T) {
	const rate, share, secs = 4.0, 0.25, 600
	whole := NewPace(rate)
	own := whole.Share(share)
	busy := true
	whole.Reserve(share, func() bool { return busy })
	if whole.Reserved() != share {
		t.Fatalf("reserved %g", whole.Reserved())
	}
	at := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	count := func(from, to int, lane bool) (l, r int) {
		for ms := from; ms < to; ms++ {
			now := at.Add(time.Duration(ms) * time.Millisecond)
			// The runs ask first, each time: they can't take the lane's
			// share.
			if _, ok := whole.take(now); ok {
				r++
			}
			if _, ok := own.take(now); lane && ok {
				l++
			}
		}
		return l, r
	}
	lane, runs := count(0, secs*1000, true)
	if want := int(secs * rate * share); lane < want || lane > want+1 {
		t.Errorf("the lane took %d turns in %ds beside runs asking all the while, want its share, %d", lane, secs, want)
	}
	if most := int(secs*rate) + 2; lane+runs > most {
		t.Errorf("the lane and the runs took %d turns in %ds together, past the rate's %d", lane+runs, secs, most)
	}
	// Within the clock's milliseconds: a turn 333.3 ms apart comes at 334.
	if least := int(secs * rate * (1 - share) * 0.995); runs < least {
		t.Errorf("the runs took %d turns in %ds, want the rest of the rate, %d at least", runs, secs, least)
	}
	busy = false
	if _, runs := count(secs*1000, 2*secs*1000, false); runs < int(secs*rate*0.995) {
		t.Errorf("the lane idle, the runs took %d turns in %ds, want the whole rate, %d", runs, secs, int(secs*rate))
	}
}

// TestLaneLimiter: the lane's Limiter waits for its own pace alone, which
// the runs don't take turns of: a second apart at a quarter of 4 a second,
// however often the runs asked meanwhile.
func TestLaneLimiter(t *testing.T) {
	clk := fakeTime(t)
	whole := NewPace(4)
	lane := NewLimiterPaced(whole.Share(0.25))
	runs := NewLimiterPaced(whole.Reserve(0.25, func() bool { return true }))
	if lane.Pace() == whole || runs.Pace() != whole || lane.Pace().Rate() != 1 {
		t.Fatal("the lane's pace isn't its share")
	}
	start := clk.now()
	for range 5 {
		if err := runs.take(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs.done(200, 0)
		if err := lane.take(context.Background()); err != nil {
			t.Fatal(err)
		}
		lane.done(200, 0)
	}
	if took := clk.now().Sub(start); took < 4*time.Second || took > 6*time.Second {
		t.Errorf("five each in %s of the clock: want the lane's a second apart, the runs' within", took)
	}
}

// TestLaneClient: Lane is a copy that asks through its own Limiter and
// sends a timeout with each range query; the client it was made of sends
// none, and keeps its Limiter.
func TestLaneClient(t *testing.T) {
	var mu sync.Mutex
	var timeouts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		timeouts = append(timeouts, r.PostForm.Get("timeout"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer srv.Close()
	runs := NewLimiter(0)
	c := New(srv.URL, "test", WithLimiter(runs))
	own := NewLimiter(0)
	lane := c.Lane(own, 2*time.Minute)
	if lane.Limiter() != own || c.Limiter() != runs {
		t.Fatal("Lane changed the client it was made of, or kept its Limiter")
	}
	from := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	for _, cl := range []*Client{c, lane} {
		if _, err := cl.QueryRange(context.Background(), "up", from, from.Add(time.Hour), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if len(timeouts) != 2 || timeouts[0] != "" || timeouts[1] != "120s" {
		t.Errorf("timeouts sent: %q, want none from the client, 120s from its lane", timeouts)
	}
}

// A Pace's rate, and its share, as made.
func TestPaceShare(t *testing.T) {
	p := NewPace(0.5)
	if r := p.Rate(); r != 0.5 {
		t.Errorf("rate %g", r)
	}
	if q := p.Share(0.25); q.Rate() != 0.125 {
		t.Errorf("a quarter of 0.5: %g", q.Rate())
	}
	var none *Pace
	if none.Rate() != 0 || none.Share(0.25) != nil || p.Share(0) != nil {
		t.Error("no rate has a share")
	}
}
