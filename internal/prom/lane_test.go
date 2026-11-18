package prom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestLaneShare: a lane paced by a quarter of a rate and by the whole rate
// the runs are paced by takes no more than its quarter, the two together
// no more than the rate, and the runs, asking all the while, have the
// rest: turns counted on a clock moved by hand, a millisecond at a time.
func TestLaneShare(t *testing.T) {
	const rate, share, secs = 4.0, 0.25, 600
	whole := NewPace(rate)
	own := NewPace(rate * share)
	at := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	lane, runs := 0, 0
	for ms := 0; ms < secs*1000; ms++ {
		now := at.Add(time.Duration(ms) * time.Millisecond)
		// The lane asks first, each time: it is never kept back by the
		// runs, only by its paces.
		if _, ok := takeBoth(now, own, whole); ok {
			lane++
		}
		if _, ok := whole.take(now); ok {
			runs++
		}
	}
	if most := int(secs*rate*share) + 1; lane > most || lane < most-2 {
		t.Errorf("the lane took %d turns in %ds, want its share, %d", lane, secs, most)
	}
	if most := int(secs*rate) + 1; lane+runs > most {
		t.Errorf("the lane and the runs took %d turns in %ds together, past the rate's %d", lane+runs, secs, most)
	}
	if least := int(secs * rate * (1 - share)); runs < least {
		t.Errorf("the runs took %d turns in %ds, want the rest of the rate, %d at least", runs, secs, least)
	}
}

// TestLaneLimiter: a Limiter AlsoPaced waits for both of its paces' turns,
// as take asks them, and takes a turn of each: one the runs just took
// holds the lane's next back too.
func TestLaneLimiter(t *testing.T) {
	clk := fakeTime(t)
	whole := NewPace(4)                                  // every 250ms
	lane := NewLimiterPaced(NewPace(1)).AlsoPaced(whole) // every second
	runs := NewLimiterPaced(whole)
	if lane.Pace() == whole || runs.Pace() != whole {
		t.Fatal("Pace isn't the Limiter's own")
	}
	start := clk.now()
	for range 5 {
		if err := lane.take(context.Background()); err != nil {
			t.Fatal(err)
		}
		lane.done(200, 0)
	}
	if took := clk.now().Sub(start); took < 4*time.Second {
		t.Errorf("the lane asked 5 in %s: want a second apart, its own pace", took)
	}
	// The runs take the whole pace's next turn: the lane's own has come,
	// and it waits for the whole's.
	clk.move(time.Second)
	if err := runs.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs.done(200, 0)
	before := clk.now()
	if err := lane.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	lane.done(200, 0)
	if waited := clk.now().Sub(before); waited < 250*time.Millisecond {
		t.Errorf("the lane waited %s after the runs took the rate's turn: want the rate's next", waited)
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
