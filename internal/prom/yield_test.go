package prom

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

var day0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func askWith(ctx context.Context, c *Client) error {
	_, err := c.QueryRange(ctx, "up", day0, day0.Add(time.Hour), time.Minute)
	return err
}

// TestCountsByAPI: each request is counted under its kind of API, by the
// class of its answer, with the 429s, the requests asked again and the
// waits; none for a request that had no answer.
func TestCountsByAPI(t *testing.T) {
	fakeTime(t)
	quick(t, 0)
	srv, asked := answers(t, refuse(429, "3"), refuse(503, ""), ok)
	var n Counts
	ctx := Counting(context.Background(), &n)
	if err := askWith(ctx, New(srv.URL, "test")); err != nil || asked.Load() != 3 {
		t.Fatalf("%v, asked %d times", err, asked.Load())
	}
	p := n.Of("prometheus")
	if p.Answers["2xx"] != 1 || p.Answers["4xx"] != 1 || p.Answers["5xx"] != 1 || p.Answers["none"] != 0 || p.Answers["3xx"] != 0 {
		t.Errorf("answers %v", p.Answers)
	}
	if p.TooMany != 1 || p.Retries != 2 || p.Yields != 0 {
		t.Errorf("429s %d, asked again %d, yields %d", p.TooMany, p.Retries, p.Yields)
	}
	// Retry-After 3, and the 503's wait, as quick makes it: half a
	// millisecond.
	if p.Waited != 3*time.Second+500*time.Microsecond || p.Paced != 0 {
		t.Errorf("waited %s, paced %s", p.Waited, p.Paced)
	}
	if n.Requests.Load() != 3 || n.Retries.Load() != 2 || n.TooMany.Load() != 1 {
		t.Errorf("the run's counts: %d %d %d", n.Requests.Load(), n.Retries.Load(), n.TooMany.Load())
	}

	// A Loki, Grafana's data source proxy, and a request with no answer.
	lk, _ := answers(t, ok)
	if err := askWith(ctx, New(lk.URL, "test", Loki())); err != nil {
		t.Fatal(err)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if err := askWith(ctx, New(gone.URL, "test", CountedAs("grafana"))); err == nil {
		t.Fatal("a closed server answered")
	}
	if l, g := n.Of("loki"), n.Of("grafana"); l.Answers["2xx"] != 1 || g.Answers["none"] != 1 || g.Answers["2xx"] != 0 {
		t.Errorf("loki %v, grafana %v", l.Answers, g.Answers)
	}
	if got := n.Of("prometheus").Answers["2xx"]; got != 1 {
		t.Errorf("prometheus counted the others': %d", got)
	}
	if x := n.Of("https://example.com"); len(x.Answers) != 0 {
		t.Errorf("an API of no kind: %v", x)
	}
}

// TestYield: with a Yield, a 429 isn't asked again, nor is anything else
// of that API, until a Begin after its cool-down; other APIs are asked
// as ever, and without a Yield a 429 is asked again as before.
func TestYield(t *testing.T) {
	clk := fakeTime(t)
	quick(t, 0)
	srv, asked := answers(t, refuse(429, ""), ok)
	other, otherAsked := answers(t, ok)
	y := &Yield{For: time.Hour, Now: clk.now}
	var n Counts
	ctx := Yielding(Counting(context.Background(), &n), y)
	prom, loki := New(srv.URL, "test"), New(other.URL, "test", Loki())

	err := askWith(ctx, prom)
	var ye *Yielded
	if !errors.As(err, &ye) || ye.API != "prometheus" || !IsYielded(err) || Refused(err) || asked.Load() != 1 {
		t.Fatalf("%v, asked %d times", err, asked.Load())
	}
	if len(clk.waits()) != 0 {
		t.Errorf("waited %v before giving up", clk.waits())
	}
	if err := askWith(ctx, prom); !IsYielded(err) || asked.Load() != 1 {
		t.Errorf("yielded to, asked again: %v, %d", err, asked.Load())
	}
	if err := askWith(ctx, loki); err != nil || otherAsked.Load() != 1 {
		t.Errorf("another API: %v, %d", err, otherAsked.Load())
	}
	if got := y.Yielded(); !slices.Equal(got, []string{"prometheus"}) {
		t.Errorf("yielded to %v", got)
	}
	if p := n.Of("prometheus"); p.Yields != 1 || p.TooMany != 1 || p.Retries != 0 {
		t.Errorf("yields %d, 429s %d, asked again %d", p.Yields, p.TooMany, p.Retries)
	}

	// The cool-down: not before an hour has gone by.
	at := clk.now()
	if y.Until("prometheus") != at.Add(time.Hour) {
		t.Errorf("until %s", y.Until("prometheus"))
	}
	y.Begin(at.Add(30 * time.Minute))
	if err := askWith(ctx, prom); !IsYielded(err) || asked.Load() != 1 {
		t.Errorf("within the cool-down: %v, %d", err, asked.Load())
	}
	y.Begin(at.Add(time.Hour))
	if err := askWith(ctx, prom); err != nil || asked.Load() != 2 || len(y.Yielded()) != 0 {
		t.Errorf("after it: %v, %d, %v", err, asked.Load(), y.Yielded())
	}

	// Without a Yield, as ever: asked again after the 429.
	plain, plainAsked := answers(t, refuse(429, ""), ok)
	if err := askWith(context.Background(), New(plain.URL, "test")); err != nil || plainAsked.Load() != 2 {
		t.Errorf("without a Yield: %v, %d", err, plainAsked.Load())
	}
}

// TestYieldEndsWaits: a request waiting for its turn (here by a rate of
// one every 1,000 seconds) gives up at once when another of its run is
// answered 429, and isn't asked.
func TestYieldEndsWaits(t *testing.T) {
	sent, answer := make(chan struct{}), make(chan struct{})
	srv, asked := answers(t, func(w http.ResponseWriter, r *http.Request) {
		close(sent)
		<-answer
		w.WriteHeader(http.StatusTooManyRequests)
	}, ok)
	c := New(srv.URL, "test", WithLimiter(NewLimiter(MinRate)))
	ctx := Yielding(context.Background(), &Yield{})
	first := make(chan error)
	go func() { first <- askWith(ctx, c) }()
	<-sent
	second := make(chan error)
	go func() { second <- askWith(ctx, c) }()
	close(answer)
	if err := <-first; !IsYielded(err) {
		t.Errorf("the one answered 429: %v", err)
	}
	if err := <-second; !IsYielded(err) {
		t.Errorf("the one waiting: %v", err)
	}
	if asked.Load() != 1 {
		t.Errorf("asked %d times", asked.Load())
	}
}

// TestPacing: a catch-up's rate spaces each API's requests, apart from
// --max-rate, and its waits are counted; a turn past the deadline isn't
// waited for.
func TestPacing(t *testing.T) {
	clk := fakeTime(t)
	srv, asked := answers(t, ok)
	var n Counts
	ctx := Pacing(Counting(context.Background(), &n), 1)
	c := New(srv.URL, "test")
	for range 3 {
		if err := askWith(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if p := n.Of("prometheus"); p.Paced != 2*time.Second || p.Waited != 0 || asked.Load() != 3 {
		t.Errorf("paced %s, waited %s, asked %d", p.Paced, p.Waited, asked.Load())
	}
	// Another API has a pace of its own.
	lk, _ := answers(t, ok)
	if err := askWith(ctx, New(lk.URL, "test", Loki())); err != nil || n.Of("loki").Paced != 0 {
		t.Errorf("loki: %v, paced %s", err, n.Of("loki").Paced)
	}
	dctx, cancel := context.WithDeadline(ctx, clk.now().Add(100*time.Millisecond))
	defer cancel()
	if err := askWith(dctx, c); !errors.Is(err, ErrPastDeadline) || asked.Load() != 3 {
		t.Errorf("past the deadline: %v, asked %d", err, asked.Load())
	}
	if ctx := Pacing(context.Background(), 0); ctx.Value(pacesKey{}) != nil {
		t.Error("a rate of 0 paces")
	}
}
