package prom

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// quick makes lapilli's own waits short and their jitter j, for a test.
func quick(t *testing.T, j float64) {
	t.Helper()
	w, l, f, m := retryWait, limitWait, jitter, maxWaited
	retryWait, limitWait, jitter = time.Millisecond, time.Millisecond, func() float64 { return j }
	t.Cleanup(func() { retryWait, limitWait, jitter, maxWaited = w, l, f, m })
}

// fakeClock is a clock that moves on only as much as is slept, or as a
// test moves it, from a whole second; it keeps what was slept.
type fakeClock struct {
	mu    sync.Mutex
	at    time.Time
	slept []time.Duration
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.at
}

func (f *fakeClock) move(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.at = f.at.Add(d)
}

func (f *fakeClock) waits() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.slept)
}

// fakeTime makes the package tell the time and sleep by a fakeClock, for a
// test: nothing it waits takes time.
func fakeTime(t *testing.T) *fakeClock {
	t.Helper()
	f := &fakeClock{at: time.Now().Truncate(time.Second)}
	c, s := clock, sleep
	clock = f.now
	sleep = func(ctx context.Context, d time.Duration) error {
		f.mu.Lock()
		f.at = f.at.Add(max(d, 0))
		f.slept = append(f.slept, d)
		f.mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() { clock, sleep = c, s })
	return f
}

// answers is a server that answers with each of its handlers in turn, the
// last one from then on, and counts the requests.
func answers(t *testing.T, hs ...http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		hs[min(i, len(hs)-1)](w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func ok(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"api"},"values":[[%s,"1"]]}]}}`, r.Form.Get("start"))
}

// refuse answers status, with Retry-After when after isn't empty, and a
// body as a gateway in front of the API writes one.
func refuse(status int, after string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if after != "" {
			w.Header().Set("Retry-After", after)
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, "<html><body><h1>%d %s</h1></body></html>", status, http.StatusText(status))
	}
}

func ask(ctx context.Context, srv *httptest.Server) ([]Series, error) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return New(srv.URL, "test").QueryRange(ctx, "up", t0, t0.Add(time.Hour), time.Minute)
}

func TestRetryAfterRead(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		status int
		header string
		want   time.Duration
		said   bool
	}{
		{429, "", 0, false},
		{429, "7", 7 * time.Second, true},
		{503, " 30 ", 30 * time.Second, true},
		{429, "0", 0, true},
		{429, now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{429, "Thu, 13 Nov 2026 08:00:00 GMT", 0, true}, // gone by: no wait
		{429, "99999999999999999999", maxRetryAfter, true},
		{429, "9999999999", maxRetryAfter, true}, // past a Duration, in nanoseconds
		{429, "1234567", 1234567 * time.Second, true},
		{429, "-5", 0, false},
		{429, "+5", 0, false},
		{429, "1.5", 0, false},
		{429, "soon", 0, false},
		{500, "7", 0, false}, // said only on 429 and 503
		{502, "7", 0, false},
	} {
		resp := &http.Response{StatusCode: c.status, Header: http.Header{}}
		if c.header != "" {
			resp.Header.Set("Retry-After", c.header)
		}
		if got, said := retryAfter(resp, now); got != c.want || said != c.said {
			t.Errorf("%d Retry-After %q: %s %v, want %s %v", c.status, c.header, got, said, c.want, c.said)
		}
	}
}

func TestBackoff(t *testing.T) {
	jit := 0.0
	old := jitter
	jitter = func() float64 { return jit }
	defer func() { jitter = old }()
	schedule := func(e *APIError) []time.Duration {
		var ws []time.Duration
		for retry := 0; retry < 8; retry++ {
			w, again := e.backoff(retry)
			if !again {
				return ws
			}
			ws = append(ws, w)
		}
		return ws
	}
	s := time.Second
	for _, c := range []struct {
		name      string
		e         *APIError
		low, high []time.Duration
	}{
		{"429 without Retry-After: up to a minute, 90 s to 3 minutes in all", &APIError{Status: 429},
			[]time.Duration{2 * s, 4 * s, 8 * s, 16 * s, 30 * s, 30 * s}, []time.Duration{4 * s, 8 * s, 16 * s, 32 * s, 60 * s, 60 * s}},
		{"5xx: as before, each less up to half", &APIError{Status: 502},
			[]time.Duration{s, 2 * s, 4 * s, 8 * s}, []time.Duration{2 * s, 4 * s, 8 * s, 16 * s}},
		{"503 without Retry-After: as 5xx", &APIError{Status: 503},
			[]time.Duration{s, 2 * s, 4 * s, 8 * s}, []time.Duration{2 * s, 4 * s, 8 * s, 16 * s}},
		// As often as it says, within five minutes (TestWaitedAtMost): eight
		// here.
		{"Retry-After 20s, plus up to a quarter", &APIError{Status: 429, said: true, retryAfter: 20 * s},
			[]time.Duration{20 * s, 20 * s, 20 * s, 20 * s, 20 * s, 20 * s, 20 * s, 20 * s}, []time.Duration{25 * s, 25 * s, 25 * s, 25 * s, 25 * s, 25 * s, 25 * s, 25 * s}},
		{"Retry-After 0: a second at least", &APIError{Status: 503, said: true},
			[]time.Duration{s, s, s, s, s, s, s, s}, []time.Duration{s + s/4, s + s/4, s + s/4, s + s/4, s + s/4, s + s/4, s + s/4, s + s/4}},
		{"Retry-After an hour: two minutes at most, plus up to a quarter", &APIError{Status: 429, said: true, retryAfter: time.Hour},
			[]time.Duration{2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute},
			[]time.Duration{150 * s, 150 * s, 150 * s, 150 * s, 150 * s, 150 * s, 150 * s, 150 * s}},
	} {
		jit = 0
		low := schedule(c.e)
		jit = 0.9999999
		high := schedule(c.e)
		round := func(ws []time.Duration) []time.Duration {
			out := make([]time.Duration, len(ws))
			for i, w := range ws {
				out[i] = w.Round(10 * time.Millisecond)
			}
			return out
		}
		if fmt.Sprint(round(low)) != fmt.Sprint(c.low) || fmt.Sprint(round(high)) != fmt.Sprint(c.high) {
			t.Errorf("%s: waits %v to %v, want %v to %v", c.name, round(low), round(high), c.low, c.high)
		}
	}
}

// TestRetryAfterSeconds: a 429 that says to wait a second is waited a
// second for, and counted.
func TestRetryAfterSeconds(t *testing.T) {
	quick(t, 0)
	clk := fakeTime(t)
	srv, n := answers(t, refuse(429, "1"), ok)
	var counts Counts
	got, err := ask(Counting(context.Background(), &counts), srv)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %+v", err, got)
	}
	if w := clk.waits(); len(w) != 1 || w[0] != time.Second {
		t.Errorf("waited %v, want the second Retry-After said", w)
	}
	if n.Load() != 2 || counts.Requests.Load() != 2 || counts.Retries.Load() != 1 || counts.TooMany.Load() != 1 {
		t.Errorf("asked %d; counted %d requests, %d retries, %d 429s", n.Load(), counts.Requests.Load(), counts.Retries.Load(), counts.TooMany.Load())
	}
}

// TestRetryAfterDate: a 503 that says when, as an HTTP date, is waited for
// until then.
func TestRetryAfterDate(t *testing.T) {
	quick(t, 0)
	clk := fakeTime(t)
	srv, _ := answers(t, func(w http.ResponseWriter, r *http.Request) {
		refuse(503, clk.now().Add(2*time.Second).UTC().Format(http.TimeFormat))(w, r)
	}, ok)
	if _, err := ask(context.Background(), srv); err != nil {
		t.Fatal(err)
	}
	if w := clk.waits(); len(w) != 1 || w[0] != 2*time.Second {
		t.Errorf("waited %v, want until the date Retry-After said", w)
	}
}

// Test429WithoutRetryAfter: a 429 that doesn't say how long is asked again
// six times, then given up, saying how often it was asked.
func Test429WithoutRetryAfter(t *testing.T) {
	quick(t, 1)
	srv, n := answers(t, refuse(429, ""), refuse(429, ""), refuse(429, ""), ok)
	if _, err := ask(context.Background(), srv); err != nil || n.Load() != 4 {
		t.Fatalf("%v after %d requests", err, n.Load())
	}
	srv, n = answers(t, refuse(429, ""))
	_, err := ask(context.Background(), srv)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 429 || n.Load() != 7 || !strings.Contains(err.Error(), "asked 7 times") {
		t.Errorf("%v after %d requests: want HTTP 429 after 7", err, n.Load())
	}
}

// Test5xx: a 5xx is asked again four times, as before; a 4xx, the query's
// fault, isn't.
func Test5xx(t *testing.T) {
	quick(t, 1)
	srv, n := answers(t, refuse(502, ""))
	if _, err := ask(context.Background(), srv); err == nil || n.Load() != 5 || !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("%v after %d requests: want HTTP 502 after 5", err, n.Load())
	}
	srv, n = answers(t, refuse(503, ""), refuse(500, ""), ok)
	if _, err := ask(context.Background(), srv); err != nil || n.Load() != 3 {
		t.Errorf("%v after %d requests", err, n.Load())
	}
	srv, n = answers(t, refuse(400, "1"))
	if _, err := ask(context.Background(), srv); err == nil || n.Load() != 1 {
		t.Errorf("%v after %d requests: a 400 isn't asked again", err, n.Load())
	}
	// 501 says the API doesn't do what was asked: asked again, it won't.
	srv, n = answers(t, refuse(501, ""), ok)
	if _, err := ask(context.Background(), srv); err == nil || n.Load() != 1 {
		t.Errorf("%v after %d requests: a 501 isn't asked again", err, n.Load())
	}
}

// TestRetriesCountedApart: 429s and 5xx are each asked again as often as
// their own kind is: four 429s don't use up the 5xx's.
func TestRetriesCountedApart(t *testing.T) {
	quick(t, 0)
	srv, n := answers(t, refuse(429, ""), refuse(429, ""), refuse(429, ""), refuse(429, ""), refuse(502, ""), refuse(502, ""), ok)
	if _, err := ask(context.Background(), srv); err != nil || n.Load() != 7 {
		t.Errorf("%v after %d requests", err, n.Load())
	}
	srv, n = answers(t, refuse(502, ""), refuse(502, ""), refuse(502, ""), refuse(502, ""), refuse(429, ""), ok)
	if _, err := ask(context.Background(), srv); err != nil || n.Load() != 6 {
		t.Errorf("%v after %d requests", err, n.Load())
	}
}

// TestDroppedAskedAgain: a connection the API drops before it answers is
// asked again, as a 5xx is.
func TestDroppedAskedAgain(t *testing.T) {
	quick(t, 0)
	drop := func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		c.Close()
	}
	srv, n := answers(t, drop, ok)
	if _, err := ask(context.Background(), srv); err != nil || n.Load() != 2 {
		t.Errorf("%v after %d requests", err, n.Load())
	}
	srv, n = answers(t, drop)
	if _, err := ask(context.Background(), srv); err == nil || n.Load() != 5 || Refused(err) {
		t.Errorf("%v after %d requests: want given up after 5, as after 5xx", err, n.Load())
	}
}

// TestWaitsWithinContext: a server saying to wait longer than the run has
// left holds it no longer than the run: a wait that would end past the
// context's deadline isn't waited, the answer is the error, and the wait it
// put on the client's other requests ends with their context.
func TestWaitsWithinContext(t *testing.T) {
	quick(t, 0)
	srv, n := answers(t, refuse(429, "100"))
	c := New(srv.URL, "test")
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := c.QueryRange(ctx, "up", t0, t0.Add(time.Hour), time.Minute)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 429 || time.Since(start) > 10*time.Second || n.Load() != 1 ||
		!errors.Is(err, ErrPastDeadline) || !strings.HasPrefix(err.Error(), "the API said to wait past the deadline, after HTTP 429: ") {
		t.Fatalf("%v after %s and %d requests: want the 429 at once, past the deadline", err, time.Since(start), n.Load())
	}
	// The client's wait ends past the deadline: given up at once, asking
	// nothing.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	start = time.Now()
	_, err = c.QueryRange(ctx2, "up", t0, t0.Add(time.Hour), time.Minute)
	if !errors.Is(err, ErrPastDeadline) || Refused(err) || time.Since(start) > 10*time.Second || n.Load() != 1 {
		t.Errorf("%v after %s and %d requests: want the client's wait not waited, asking nothing", err, time.Since(start), n.Load())
	}
	// Without a deadline, it ends with the context.
	ctx3, cancel3 := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel3)
	if _, err = c.QueryRange(ctx3, "up", t0, t0.Add(time.Hour), time.Minute); !errors.Is(err, context.Canceled) || n.Load() != 1 {
		t.Errorf("%v after %d requests: want the client's wait cut short by the context, asking nothing", err, n.Load())
	}
	if _, in, _ := c.limit.state(); in != 0 {
		t.Errorf("%d in flight at the end", in)
	}
}

// TestWaitedAtMost: however a server says to wait, a request is asked
// again for five minutes at most after it was first turned away: told two
// minutes each time, it is asked three times.
func TestWaitedAtMost(t *testing.T) {
	quick(t, 0)
	fakeTime(t)
	srv, n := answers(t, refuse(429, "120"))
	_, err := ask(context.Background(), srv)
	if err == nil || n.Load() != 3 || !strings.Contains(err.Error(), "asked 3 times over 4m0s") {
		t.Errorf("%v after %d requests: want given up after 2 waits of 2m", err, n.Load())
	}
}

// TestWaitedCountsAll: the five minutes count the API's answers and the
// waits for a turn, not only lapilli's own waits: a server that answers
// each request after 100s, saying to wait none, is asked four times; and a
// request held by a wait another's answer asked for is pushed back from
// then.
func TestWaitedCountsAll(t *testing.T) {
	quick(t, 0)
	clk := fakeTime(t)
	srv, n := answers(t, func(w http.ResponseWriter, r *http.Request) {
		clk.move(100 * time.Second)
		refuse(429, "0")(w, r)
	})
	if _, err := ask(context.Background(), srv); err == nil || n.Load() != 4 {
		t.Errorf("%v after %d requests: want given up after 4", err, n.Load())
	}

	// Told two minutes each time, a is asked at 0, 2m and 4m, and leaves
	// the client waiting until 6m.
	l := NewLimiter(0)
	srv, n = answers(t, refuse(429, "120"))
	a, b := New(srv.URL, "a", WithLimiter(l)), New(srv.URL, "b", WithLimiter(l))
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := a.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute); err == nil || n.Load() != 3 {
		t.Fatalf("a: %v after %d requests", err, n.Load())
	}
	// b, asking at 4m, waits for its turn till 6m: pushed back from 4m, it
	// is asked at 6m and 8m, not at 10m.
	_, err := b.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute)
	if err == nil || n.Load() != 5 {
		t.Errorf("b: %v after %d requests in all: want b asked twice", err, n.Load())
	}
	// c is held past its five minutes before it is asked at all.
	l.mu.Lock()
	l.until = clk.now().Add(maxWaited + time.Second)
	l.mu.Unlock()
	if _, err = b.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute); !errors.Is(err, errNoTurn) || n.Load() != 5 || Refused(err) {
		t.Errorf("c: %v after %d requests in all: want given up, unasked", err, n.Load())
	}
	if _, in, _ := l.state(); in != 0 {
		t.Errorf("%d in flight at the end", in)
	}
}

// TestNoTurnGivesTheLastAnswer: a request asked again that doesn't get its
// turn within its five minutes fails with the answer that turned it away.
func TestNoTurnGivesTheLastAnswer(t *testing.T) {
	quick(t, 0)
	maxWaited = 300 * time.Millisecond
	l := NewLimiter(0)
	l.limit, l.changed = 1, time.Now()
	held := make(chan struct{})
	srv, n := answers(t, func(w http.ResponseWriter, r *http.Request) {
		// Another takes the only place while this one waits to ask again.
		go func() {
			if err := l.take(context.Background()); err != nil {
				t.Error(err)
			}
			close(held)
		}()
		refuse(502, "")(w, r)
	}, ok)
	c := New(srv.URL, "test", WithLimiter(l))
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	start := time.Now()
	_, err := c.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute)
	<-held
	var ae *APIError
	// Asked once: the turn it didn't get isn't counted as an ask.
	if !errors.As(err, &ae) || ae.Status != 502 || n.Load() != 1 || time.Since(start) > 10*time.Second || strings.Contains(err.Error(), "asked") {
		t.Errorf("%v after %d requests and %s: want the 502", err, n.Load(), time.Since(start))
	}
	l.done(200, 0)
	if _, in, q := l.state(); in != 0 || q != 0 {
		t.Errorf("%d in flight, %d waiting at the end", in, q)
	}
}

// TestPastDeadlineGivesTheLastAnswer: a request asked again whose turn
// would come past its context's deadline, as another's answer said to wait,
// fails then, past the deadline, with the answer that turned it away.
func TestPastDeadlineGivesTheLastAnswer(t *testing.T) {
	quick(t, 0)
	l := NewLimiter(0)
	srv, n := answers(t, func(w http.ResponseWriter, r *http.Request) {
		// Another's answer, meanwhile, says to wait an hour.
		l.mu.Lock()
		l.until = time.Now().Add(time.Hour)
		l.mu.Unlock()
		refuse(502, "")(w, r)
	}, ok)
	c := New(srv.URL, "test", WithLimiter(l))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	start := time.Now()
	_, err := c.QueryRange(ctx, "up", t0, t0.Add(time.Hour), time.Minute)
	var ae *APIError
	if !errors.Is(err, ErrPastDeadline) || !errors.As(err, &ae) || ae.Status != 502 || n.Load() != 1 || time.Since(start) > 10*time.Second ||
		err.Error() != "the API said to wait past the deadline, after HTTP 502: not a Prometheus API response: <html><body><h1>502 Bad Gateway</h1></body></html>" {
		t.Errorf("%v after %d requests and %s: want the 502, past the deadline", err, n.Load(), time.Since(start))
	}
}

// TestPushBackShared: a 429 one client of a Limiter gets halves how many
// requests every client of it may have in flight, and its Retry-After holds
// them all, though only one was told it.
func TestPushBackShared(t *testing.T) {
	quick(t, 0)
	l := NewLimiter(0)
	srv, n := answers(t, refuse(429, "2"), ok)
	a, b := New(srv.URL, "a", WithLimiter(l)), New(srv.URL, "b", WithLimiter(l))
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	done := make(chan error)
	go func() {
		_, err := a.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute)
		done <- err
	}()
	var limit int
	var until time.Time
	for wait := time.Now().Add(30 * time.Second); time.Now().Before(wait) && until.IsZero(); time.Sleep(5 * time.Millisecond) {
		l.mu.Lock()
		limit, until = l.limit, l.until
		l.mu.Unlock()
	}
	if limit != 1 || time.Until(until) < time.Second {
		t.Fatalf("limit %d, asked to wait until %s from now, after a 429 to one asked alone: want 1, and about 2s", limit, time.Until(until))
	}
	start := time.Now()
	if _, err := b.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < time.Second {
		t.Errorf("b asked after %s, within the wait a's answer said", took)
	}
	if err := <-done; err != nil || n.Load() != 3 {
		t.Errorf("%v after %d requests", err, n.Load())
	}
}

// TestWaitAtLeastASecond: a Retry-After of 0 holds the client's other
// requests a second, so that it can't bring them all straight back.
func TestWaitAtLeastASecond(t *testing.T) {
	clk := fakeTime(t)
	srv, _ := answers(t, refuse(429, "0"))
	c := New(srv.URL, "test")
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if _, _, err := c.do(context.Background(), req, true, maxBody); err != nil {
		t.Fatal(err)
	}
	if d := c.limit.until.Sub(clk.now()); d != time.Second {
		t.Errorf("the client waits %s, want a second", d)
	}
}

// TestBestEffortReads: the configuration and the targets, which lapilli
// does without, are asked once, and their answers don't change how the
// API is asked: a 503 halves nothing, a Retry-After holds nothing.
func TestBestEffortReads(t *testing.T) {
	quick(t, 0)
	for _, a := range []http.HandlerFunc{refuse(503, ""), refuse(429, "30"), refuse(502, "")} {
		srv, n := answers(t, a)
		c := New(srv.URL, "test")
		var counts Counts
		ctx := Counting(context.Background(), &counts)
		start := time.Now()
		if _, err := c.StatusConfig(ctx); err == nil {
			t.Error("want an error")
		}
		if _, err := c.Targets(ctx); err == nil {
			t.Error("want an error")
		}
		limit, in, _ := c.limit.state()
		if n.Load() != 2 || limit != 64 || in != 0 || !c.limit.until.IsZero() || time.Since(start) > 10*time.Second {
			t.Errorf("asked %d times in %s; then limit %d, %d in flight, waiting until %s", n.Load(), time.Since(start), limit, in, c.limit.until)
		}
		// Nor is a 429 counted as one the run asked fewer at once after.
		if counts.Requests.Load() != 2 || counts.TooMany.Load() != 0 {
			t.Errorf("counted %d requests, %d answered 429", counts.Requests.Load(), counts.TooMany.Load())
		}
	}
}

// TestErrorText: the API's text in an error is cut to a few hundred bytes,
// at a rune, and without the token or the signature sent, which a server
// may echo back.
func TestErrorText(t *testing.T) {
	huge := strings.Repeat("é", 1<<19)
	srv, _ := answers(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"status":"error","errorType":"bad_data","error":%q}`, huge)
	})
	_, err := ask(context.Background(), srv)
	if err == nil || len(err.Error()) > 400 || !utf8.ValidString(err.Error()) || !strings.HasSuffix(err.Error(), "é…") {
		t.Errorf("%d bytes: %.80q", len(err.Error()), err)
	}

	srv, _ = answers(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprintf(w, `{"status":"error","error":"forbidden for %s; signed %s; also Bearer other-token"}`,
			r.Header.Get("Authorization"), r.Header.Get("X-Amz-Security-Token"))
	})
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, o := range []Option{
		WithToken(func(context.Context) (string, error) { return "s3cr3t-token", nil }),
		WithSigner(func(r *http.Request) error {
			r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261113/us-east-1/aps/aws4_request, SignedHeaders=host, Signature=0123456789abcdef")
			r.Header.Set("X-Amz-Security-Token", "session-s3cr3t")
			return nil
		}),
	} {
		_, err := New(srv.URL, "test", o).QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute)
		for _, secret := range []string{"s3cr3t-token", "AKIDEXAMPLE", "0123456789abcdef", "session-s3cr3t", "other-token"} {
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Errorf("%v", err)
			}
		}
	}
	// Nor is a signature cut across where a text that isn't JSON is cut.
	srv, _ = answers(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprintf(w, "%s%s", strings.Repeat("x", 160), r.Header.Get("Authorization"))
	})
	sign := WithSigner(func(r *http.Request) error {
		r.Header.Set("Authorization", "Signature keyId=\"k\",signature=\"0123456789abcdefSECRET\"")
		return nil
	})
	if _, err := New(srv.URL, "test", sign).QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute); err == nil || strings.Contains(err.Error(), "012345") {
		t.Errorf("%v", err)
	}
	// One made elsewhere is said without a bearer token too.
	if e := (&APIError{Status: 401, Msg: "no: bearer abc.def"}).Error(); e != "HTTP 401: no: Bearer [redacted]" {
		t.Errorf("%s", e)
	}
}

// TestUnreadableSample: a sample the server writes unreadably is quoted in
// the error, whatever it holds.
func TestUnreadableSample(t *testing.T) {
	srv, _ := answers(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{\"status\":\"success\",\"data\":{\"resultType\":\"matrix\",\"result\":[{\"metric\":{},\"values\":[[\"\u009b31m\u202eRED\",\"1\"]]}]}}")
	})
	_, err := ask(context.Background(), srv)
	if err == nil || strings.ContainsAny(err.Error(), "\u009b\u202e") || !strings.Contains(err.Error(), `\u009b`) {
		t.Errorf("%q", err)
	}
}

// TestRefused: the API's answer to the query as written, which another
// run is given too, against a failure of the run, the history not read.
func TestRefused(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&APIError{Status: 400, Msg: "bad_data: parse error"}, true},
		{&APIError{Status: 422, Msg: "execution: many-to-many matching not allowed"}, true},
		{&APIError{Status: 501, Msg: "not implemented"}, true},
		{&APIError{Status: 200, Msg: "the answer left series out: result truncated"}, true},
		{&APIError{Status: 503, Msg: "query processing would load too many samples into memory"}, true},
		{&unreadable{msg: "want a matrix, got \"vector\""}, true},
		// A page a proxy wrote, of 400 too, isn't the API's word.
		{&APIError{Status: 400, Msg: "not a Prometheus API response: <html>", page: true}, false},
		{&APIError{Status: 200, Msg: "not a Prometheus API response: <html>", page: true}, false},
		{&APIError{Status: 302, Msg: "not a Prometheus API response: <a href=\"/login\">Found</a>", page: true}, false},
		{&APIError{Status: 401, Msg: "token expired"}, false},
		{&APIError{Status: 403}, false},
		{&APIError{Status: 404}, false},
		{&APIError{Status: 407}, false},
		{&APIError{Status: 408}, false},
		{&APIError{Status: 429}, false},
		{&APIError{Status: 503}, false},
		{fmt.Errorf("%w (asked 7 times over 3m0s)", &APIError{Status: 502}), false},
		{context.DeadlineExceeded, false},
		{context.Canceled, false},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, false},
		{&net.DNSError{Err: "no such host", Name: "prom"}, false},
		{io.ErrUnexpectedEOF, false},
		{errNoTurn, false},
		{&pastDeadline{why: waitPast, last: &APIError{Status: 429}}, false},
		{&credsError{os.ErrNotExist}, false},
		{&credsError{&APIError{Status: 503, Msg: "the token endpoint"}}, false},
		{&credsError{&APIError{Status: 400, Msg: "invalid_grant"}}, false},
		{nil, false},
	} {
		if got := Refused(c.err); got != c.want {
			t.Errorf("%v: %v, want %v", c.err, got, c.want)
		}
	}
}

// TestSteady: a reason as a ledger keeps it, without how often its request
// was asked, which retried writes at its end; one a server wrote elsewhere
// is left, as taking it out could join a secret it cut in two.
func TestSteady(t *testing.T) {
	for in, want := range map[string]string{
		"query failed: the history couldn't be read: HTTP 429: slow down (asked 7 times over 3m2s)": "query failed: the history couldn't be read: HTTP 429: slow down",
		"HTTP 503: down (asked 3 times over 1h2m3s)":                                                "HTTP 503: down",
		"HTTP 503: down (asked 3 times over 1.5s)":                                                  "HTTP 503: down",
		"HTTP 403: denied for s3cr (asked 1 times over 1s)3t-token":                                 "HTTP 403: denied for s3cr (asked 1 times over 1s)3t-token",
		"HTTP 403: Bearer s3cr3t-0123 (asked 2 times over 1s)":                                      "HTTP 403: Bearer [redacted]",
	} {
		if got := Steady(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// TestEveryPath: a Loki, a client that signs each request (as --auth aws
// does) and one that sends a token (azure, google, a token file) are asked
// again alike, each request signed or given its token afresh; and the
// rules a Prometheus has loaded too.
func TestEveryPath(t *testing.T) {
	quick(t, 0)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	lokiOK := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			t.Errorf("Loki asked at %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}
	srv, n := answers(t, refuse(429, "1"), lokiOK)
	if _, err := New(srv.URL, "test", Loki()).QueryRange(context.Background(), `sum(count_over_time({app="a"}[1m]))`, t0, t0.Add(time.Hour), time.Minute); err != nil || n.Load() != 2 {
		t.Errorf("Loki: %v after %d requests", err, n.Load())
	}

	var signed, tokens atomic.Int64
	var answered atomic.Value
	srv, n = answers(t, refuse(503, "1"), func(w http.ResponseWriter, r *http.Request) {
		answered.Store(r.Header.Get("X-Signed"))
		ok(w, r)
	})
	sign := WithSigner(func(r *http.Request) error {
		r.Header.Set("X-Signed", fmt.Sprint(signed.Add(1)))
		return nil
	})
	if _, err := New(srv.URL, "test", sign).QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute); err != nil || signed.Load() != 2 || answered.Load() != "2" {
		t.Errorf("signed: %v, %d signatures, answered the one signed %v", err, signed.Load(), answered.Load())
	}

	srv, n = answers(t, refuse(429, "1"), func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"groups":[]}}`)
	})
	token := WithToken(func(context.Context) (string, error) { return fmt.Sprint(tokens.Add(1)), nil })
	if _, err := New(srv.URL, "test", token).Rules(context.Background()); err != nil || n.Load() != 2 || tokens.Load() != 2 {
		t.Errorf("rules: %v after %d requests, %d tokens", err, n.Load(), tokens.Load())
	}
}

// TestBestEffortGivesUp: a read lapilli does without is waited for 30
// seconds at most, whatever the run's own time; and isn't asked at all
// while the API said to wait, nor waits for it.
func TestBestEffortGivesUp(t *testing.T) {
	was := bestEffortTime
	bestEffortTime = 100 * time.Millisecond
	t.Cleanup(func() { bestEffortTime = was })
	srv, n := answers(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	c := New(srv.URL, "test")
	start := time.Now()
	if _, err := c.StatusConfig(context.Background()); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("%v after %s: want given up after its 30 seconds", err, time.Since(start))
	}
	c.limit.mu.Lock()
	c.limit.until = time.Now().Add(time.Hour)
	c.limit.mu.Unlock()
	start = time.Now()
	if _, err := c.Targets(context.Background()); !errors.Is(err, errPaused) || n.Load() != 1 || time.Since(start) > 5*time.Second {
		t.Errorf("%v after %s and %d requests: want not asked, at once", err, time.Since(start), n.Load())
	}
	if _, err := c.RulesOnce(context.Background()); !errors.Is(err, errPaused) || n.Load() != 1 {
		t.Errorf("rules: %v after %d requests", err, n.Load())
	}
	if _, in, _ := c.limit.state(); in != 0 {
		t.Errorf("%d in flight", in)
	}
}

// reset is a transport whose first answers are connections reset, as a
// load balancer shedding load sends them, then the server's.
type reset struct {
	left atomic.Int64
	next http.RoundTripper
}

func (r *reset) RoundTrip(req *http.Request) (*http.Response, error) {
	if r.left.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	}
	return r.next.RoundTrip(req)
}

// TestResetPushesBack: a connection reset is asked again, as a 5xx is, and
// pushes back as a 503 does: those asked at once are halved.
func TestResetPushesBack(t *testing.T) {
	quick(t, 0)
	srv, n := answers(t, ok)
	rt := &reset{next: http.DefaultTransport}
	rt.left.Store(1)
	c := New(srv.URL, "test", WithHTTPClient(&http.Client{Transport: rt}))
	if _, err := ask2(c); err != nil || n.Load() != 1 {
		t.Fatalf("%v after %d answered: want asked again after the reset", err, n.Load())
	}
	if limit, in, _ := c.limit.state(); limit != 1 || in != 0 {
		t.Errorf("limit %d, %d in flight, after a reset to one asked alone: want 1", limit, in)
	}
	// Unheeded, as a read lapilli does without is, it changes nothing.
	rt.left.Store(1)
	c = New(srv.URL, "test", WithHTTPClient(&http.Client{Transport: rt}))
	if _, err := c.StatusConfig(context.Background()); err == nil {
		t.Error("want the reset")
	}
	if limit, _, _ := c.limit.state(); limit != 64 {
		t.Errorf("limit %d after a reset to a read done without", limit)
	}
}

func ask2(c *Client) ([]Series, error) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return c.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute)
}

// TestBigPastTheCut: an answer too large, said past the bytes an error
// keeps of the API's text, is still asked again for a shorter range.
func TestBigPastTheCut(t *testing.T) {
	var asked atomic.Int64
	srv, _ := answers(t, func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		r.ParseForm()
		if r.Form.Get("start") != r.Form.Get("end") {
			w.WriteHeader(422)
			fmt.Fprintf(w, `{"status":"error","error":"%s: response too large"}`, strings.Repeat("x", 400))
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	})
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := New(srv.URL, "t").QueryRange(context.Background(), "up", t0, t0.Add(4*time.Minute), time.Minute); err != nil || asked.Load() < 3 {
		t.Errorf("%v after %d requests: want halved till answered", err, asked.Load())
	}
}

// TestHiddenSecrets: a token echoed back with a character that doesn't
// show in it, or a password a URL held, which net/http sends as Basic, is
// taken out of an error like any other.
func TestHiddenSecrets(t *testing.T) {
	const tok = "s3cr3t-token-0123456789"
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, sep := range []string{"\u200b", "\u2060", "\x1b", "\u00ad"} {
		srv, _ := answers(t, func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			w.WriteHeader(403)
			fmt.Fprintf(w, `{"status":"error","error":"denied for %s"}`, got[:4]+sep+got[4:])
		})
		c := New(srv.URL, "t", WithToken(func(context.Context) (string, error) { return tok, nil }))
		if _, err := c.QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute); err == nil || strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), "s3cr") {
			t.Errorf("%q: %q", sep, err)
		}
	}
	srv, _ := answers(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprintf(w, `{"status":"error","error":"bad credentials %s, hunter2-password"}`, r.Header.Get("Authorization"))
	})
	u := strings.Replace(srv.URL, "http://", "http://admin:hunter2-password@", 1)
	_, err := New(u, "t").QueryRange(context.Background(), "up", t0, t0.Add(time.Hour), time.Minute)
	if err == nil || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte("admin:hunter2-password"))) || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("%q", err)
	}
}

// TestBearerWords: a bearer token is taken out wherever it is; the word
// "bearer" before a word isn't.
func TestBearerWords(t *testing.T) {
	for in, want := range map[string]string{
		"missing bearer token":                     "missing bearer token",
		"missing bearer token.":                    "missing bearer token.",
		"Bearer realm: invalid":                    "Bearer realm: invalid",
		"no: bearer abc.def":                       "no: Bearer [redacted]",
		"got Bearer eyJhbGciOi.eyJzdWIi.c2ln, no":  "got Bearer [redacted], no",
		"bearer abcdefghijklmnopqrstuvwxyz denied": "Bearer [redacted] denied",
		"bearer Zm9vYmFy==":                        "Bearer [redacted]",
	} {
		if got := redact(in, nil); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// TestClipAtRune: cut where a rune is in two, a text is cut before it; and
// is no longer than asked, "…" included.
func TestClipAtRune(t *testing.T) {
	s := "a" + strings.Repeat("é", 10)
	for n := 4; n < 12; n++ {
		if got := clip(s, n); !utf8.ValidString(got) || len(got) > n || !strings.HasSuffix(got, "…") {
			t.Errorf("clip %d: %q, %d bytes", n, got, len(got))
		}
	}
	if got := clip(s, len(s)); got != s {
		t.Errorf("%q", got)
	}
}
