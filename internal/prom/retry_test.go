package prom

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// quick makes lapilli's own waits short and their jitter j, for a test.
func quick(t *testing.T, j float64) {
	t.Helper()
	w, l, f, m := retryWait, limitWait, jitter, maxWaited
	retryWait, limitWait, jitter = time.Millisecond, time.Millisecond, func() float64 { return j }
	t.Cleanup(func() { retryWait, limitWait, jitter, maxWaited = w, l, f, m })
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
		{"Retry-After an hour: two minutes at most", &APIError{Status: 429, said: true, retryAfter: time.Hour},
			[]time.Duration{2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute},
			[]time.Duration{2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute}},
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
	srv, n := answers(t, refuse(429, "1"), ok)
	var counts Counts
	start := time.Now()
	got, err := ask(Counting(context.Background(), &counts), srv)
	took := time.Since(start)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %+v", err, got)
	}
	if took < time.Second || took > 3*time.Second {
		t.Errorf("took %s, want the second Retry-After said", took)
	}
	if n.Load() != 2 || counts.Requests.Load() != 2 || counts.Retries.Load() != 1 || counts.TooMany.Load() != 1 {
		t.Errorf("asked %d; counted %d requests, %d retries, %d 429s", n.Load(), counts.Requests.Load(), counts.Retries.Load(), counts.TooMany.Load())
	}
}

// TestRetryAfterDate: a 503 that says when, as an HTTP date, is waited for
// until then.
func TestRetryAfterDate(t *testing.T) {
	quick(t, 0)
	srv, _ := answers(t, func(w http.ResponseWriter, r *http.Request) {
		// A date is to the second: two seconds on is one to two away.
		refuse(503, time.Now().Add(2*time.Second).UTC().Format(http.TimeFormat))(w, r)
	}, ok)
	start := time.Now()
	if _, err := ask(context.Background(), srv); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 900*time.Millisecond || took > 4*time.Second {
		t.Errorf("took %s, want until the date Retry-After said", took)
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
}

// TestWaitsWithinContext: a server saying to wait longer than the run has
// left holds it no longer than the run: the wait ends with the context, and
// so does the wait it put on the client's other requests.
func TestWaitsWithinContext(t *testing.T) {
	quick(t, 0)
	srv, n := answers(t, refuse(429, "100"))
	c := New(srv.URL, "test")
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.QueryRange(ctx, "up", t0, t0.Add(time.Hour), time.Minute); err == nil {
		t.Fatal("want an error")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s, past the context's 300ms", took)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	start = time.Now()
	_, err := c.QueryRange(ctx2, "up", t0, t0.Add(time.Hour), time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second || n.Load() != 1 {
		t.Errorf("%v after %s and %d requests: want the client's wait cut short by the context, asking nothing", err, time.Since(start), n.Load())
	}
}

// TestWaitedAtMost: however a server says to wait, a request waits five
// minutes at most in all before it is given up (here, three seconds).
func TestWaitedAtMost(t *testing.T) {
	quick(t, 0)
	maxWaited = 3 * time.Second
	srv, n := answers(t, refuse(429, "2"))
	_, err := ask(context.Background(), srv)
	if err == nil || n.Load() != 2 || !strings.Contains(err.Error(), "asked 2 times over 2s") {
		t.Errorf("%v after %d requests: want given up after one wait of 2s", err, n.Load())
	}
}

// TestPushBackShared: a 429 one client of a Limiter gets lowers how many
// requests every client of it has in flight, and its Retry-After holds them
// all, though only one was told it.
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
	for wait := time.Now().Add(5 * time.Second); time.Now().Before(wait) && until.IsZero(); time.Sleep(5 * time.Millisecond) {
		l.mu.Lock()
		limit, until = l.limit, l.until
		l.mu.Unlock()
	}
	if limit != 1 || time.Until(until) < time.Second {
		t.Fatalf("limit %d, asked to wait until %s from now, after a 429 with one in flight: want 1, and about 2s", limit, time.Until(until))
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
