package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// quotaAPI is a Prometheus that answers HTTP 429 while full is set, and
// an empty matrix otherwise; it counts what it was asked, by path and
// query, and total counts the queries (not the configuration, read once
// a day as lapilli does without it).
type quotaAPI struct {
	full  atomic.Bool
	mu    sync.Mutex
	asked map[string]int
}

func (q *quotaAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	q.mu.Lock()
	q.asked[r.URL.Path+" "+r.Form.Get("query")]++
	q.mu.Unlock()
	if q.full.Load() {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
}

func (q *quotaAPI) total() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for k, v := range q.asked {
		if strings.HasPrefix(k, "/api/v1/query_range ") {
			n += v
		}
	}
	return n
}

// TestYieldOn429: with --yield-on-429, a run answered 429 asks the API
// nothing more, not even again after a wait: it stops after the piece it
// was in, the rule it couldn't replay is missed and says why, nothing is
// set beside what really fired, and /metrics counts it. The next run
// replays the rule from where it was missed.
func TestYieldOn429(t *testing.T) {
	ctx := context.Background()
	q := &quotaAPI{asked: map[string]int{}}
	q.full.Store(true)
	api := httptest.NewServer(q)
	t.Cleanup(api.Close)
	s, _ := server(t, func(c *Config) { c.Impact, c.Yield, c.CheckReplay = "", true, true })
	s.h = prom.New(api.URL, "test")
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	if n := q.total(); n != 1 {
		t.Errorf("asked %d times, want once: %v", n, q.asked)
	}
	// One piece of the three days' backfill, and no more.
	if c, _ := s.l.Cursor(ctx); !c.Equal(now.Add(-2 * 24 * time.Hour)) {
		t.Errorf("cursor %s", c)
	}
	runs, err := s.l.Runs(ctx, 10)
	if err != nil || len(runs) != 1 || runs[0].Error != "" || !strings.Contains(runs[0].Failed["Errors"], "yielded") {
		t.Fatalf("%v %+v", err, runs)
	}
	if m, _ := s.l.Missed(ctx); m["Errors"].IsZero() {
		t.Errorf("not missed: %v", m)
	}
	_, body, _ := get(t, s.Handler(), "/metrics")
	for _, want := range []string{`lapilli_serve_history_requests_total{api="prometheus",answer="4xx"} 1`, `lapilli_serve_history_requests_total{api="prometheus",answer="2xx"} 0`,
		`lapilli_serve_history_throttled_total{api="prometheus"} 1`, `lapilli_serve_history_yields_total{api="prometheus"} 1`,
		`lapilli_serve_history_retries_total{api="prometheus"} 0`, `lapilli_serve_history_wait_seconds_total{api="prometheus",reason="api"} 0`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in /metrics:\n%s", want, body)
		}
	}
	if strings.Contains(body, `api="loki"`) {
		t.Errorf("an API not asked is in /metrics")
	}

	// The quota back: the next run replays from the cursor, the rule from
	// where it was missed, and catches up.
	q.full.Store(false)
	s.jobs(ctx, true)
	if c, _ := s.l.Cursor(ctx); !c.Equal(now) {
		t.Errorf("cursor %s", c)
	}
	if m, _ := s.l.Missed(ctx); len(m) != 0 {
		t.Errorf("still missed: %v", m)
	}
	if q.asked[`/api/v1/query_range ALERTS{alertstate="firing"}`] == 0 {
		t.Errorf("not set beside what really fired once caught up: %v", q.asked)
	}
}

// TestYieldFor: --yield-for leaves the API alone after a 429: a run in the
// meantime asks nothing, and is kept as held, once.
func TestYieldFor(t *testing.T) {
	ctx := context.Background()
	q := &quotaAPI{asked: map[string]int{}}
	q.full.Store(true)
	api := httptest.NewServer(q)
	t.Cleanup(api.Close)
	s, _ := server(t, func(c *Config) { c.Impact, c.Yield, c.YieldFor = "", true, 2*time.Hour })
	s.h = prom.New(api.URL, "test")
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	at := now
	s.now = func() time.Time { return at }
	s.jobs(ctx, true)
	q.full.Store(false)
	for range 2 {
		at = at.Add(45 * time.Minute)
		s.jobs(ctx, true)
	}
	runs, _ := s.l.Runs(ctx, 10)
	if q.total() != 1 || len(runs) != 2 || runs[0].Kind != heldKind || !strings.Contains(runs[0].Error, "--yield-for") {
		t.Fatalf("asked %v: %+v", q.asked, runs)
	}
	at = now.Add(2 * time.Hour)
	s.jobs(ctx, true)
	if c, _ := s.l.Cursor(ctx); q.total() == 1 || !c.Equal(at) {
		t.Errorf("after --yield-for: asked %d, cursor %s", q.total(), c)
	}
}

// guardHistory is a history whose guard returns a series while up says so, or
// fails while broken does; the rest is history's.
type guardHistory struct {
	history
	mu     sync.Mutex
	up     func(n int) bool // by how many times the guard was asked before
	broken bool
	asked  int
}

func (g *guardHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if expr != "evaluation_failures > 0" {
		return g.history.QueryRange(ctx, expr, start, end, step)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n := g.asked
	g.asked++
	if g.broken {
		return nil, &prom.APIError{Status: 503, Msg: "down"}
	}
	if !start.Equal(end) {
		return nil, errors.New("the guard is asked at one time")
	}
	if g.up(n) {
		return []prom.Series{{Labels: map[string]string{"job": "ruler"}, Samples: []prom.Sample{{T: start, V: 1}}}}, nil
	}
	return nil, nil
}

// TestGuard: while --guard returns anything, or can't be read, runs wait,
// kept as held once for each reason, not as failed; it is asked between
// the days of a catch-up as well, and the run waits there.
func TestGuard(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, func(c *Config) { c.Guard = "evaluation_failures > 0" })
	g := &guardHistory{up: func(int) bool { return true }}
	s.h = g
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	s.jobs(ctx, true)
	s.jobs(ctx, true)
	runs, _ := s.l.Runs(ctx, 10)
	if c, _ := s.l.Cursor(ctx); !c.IsZero() || len(runs) != 1 || runs[0].Kind != heldKind || !strings.Contains(runs[0].Error, "--guard returned") || g.history.asked != 0 {
		t.Fatalf("cursor %s, asked %d: %+v", c, g.history.asked, runs)
	}
	if n, _ := s.l.FailedRuns(ctx, now.Add(-time.Hour)); n != 0 {
		t.Errorf("held runs counted as failed: %d", n)
	}
	h := s.Handler()
	_, body, _ := get(t, h, "/metrics")
	if !strings.Contains(body, `lapilli_serve_runs_total{kind="backfill",result="held"} 2`) {
		t.Errorf("/metrics:\n%s", body)
	}
	for path, want := range map[string]string{"/": "The first run waits: --guard returned series", "/runs": "status-warn"} {
		if _, body, _ := get(t, h, path); !strings.Contains(body, want) {
			t.Errorf("%s: no %q", path, want)
		}
	}

	// Unread, it holds too, and says why.
	g.broken = true
	s.jobs(ctx, true)
	if runs, _ := s.l.Runs(ctx, 1); !strings.Contains(runs[0].Error, "--guard couldn't be read") || !strings.Contains(runs[0].Error, "HTTP 503") {
		t.Errorf("%+v", runs[0])
	}

	// Quiet for the first day of the catch-up, and not after: the run
	// waits after that day.
	g.broken = false
	g.mu.Lock()
	g.asked, g.up = 0, func(n int) bool { return n > 0 }
	g.mu.Unlock()
	s.jobs(ctx, true)
	if c, _ := s.l.Cursor(ctx); !c.Equal(now.Add(-2 * 24 * time.Hour)) {
		t.Errorf("cursor %s", c)
	}
	if runs, _ := s.l.Runs(ctx, 1); runs[0].Kind != heldKind {
		t.Errorf("%+v", runs[0])
	}
	g.mu.Lock()
	g.up = func(int) bool { return false }
	g.mu.Unlock()
	s.jobs(ctx, true)
	if c, _ := s.l.Cursor(ctx); !c.Equal(now) {
		t.Errorf("cursor %s", c)
	}
}

// TestCatchUpHours: a catch-up waits for --catch-up-hours, then catches up
// in them; a run that isn't behind runs at any hour.
func TestCatchUpHours(t *testing.T) {
	ctx := context.Background()
	w, err := ParseWindow("01:00-03:00")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := server(t, func(c *Config) { c.CatchUpHours = &w })
	raw := ""
	if err := s.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	at := now
	s.now = func() time.Time { return at }
	s.jobs(ctx, true)
	runs, _ := s.l.Runs(ctx, 10)
	if c, _ := s.l.Cursor(ctx); !c.IsZero() || len(runs) != 1 || runs[0].Kind != heldKind || !strings.Contains(runs[0].Error, "--catch-up-hours 01:00-03:00") {
		t.Fatalf("outside the hours: cursor %s, %+v", c, runs)
	}
	at = day.Add(2 * time.Hour)
	s.jobs(ctx, true)
	if c, _ := s.l.Cursor(ctx); !c.Equal(at) {
		t.Errorf("in the hours: cursor %s", c)
	}
	at = day.Add(5 * time.Hour) // three hours on: not behind
	s.jobs(ctx, true)
	if c, _ := s.l.Cursor(ctx); !c.Equal(at) {
		t.Errorf("the hourly run, outside the hours: cursor %s", c)
	}
	at = day.Add(12 * time.Hour) // seven hours on: behind
	s.jobs(ctx, true)
	at = day.Add(13 * time.Hour) // and an hour later, held once
	s.jobs(ctx, true)
	if c, _ := s.l.Cursor(ctx); !c.Equal(day.Add(5 * time.Hour)) {
		t.Errorf("a catch-up, outside the hours: cursor %s", c)
	}
	if runs, _ := s.l.Runs(ctx, 2); runs[0].Kind != heldKind || runs[1].Kind == heldKind {
		t.Errorf("held twice for the same reason: %+v", runs)
	}
}

// TestCatchUpRate: while catching up, a run asks each API at
// --catch-up-rate: at one every 1,000 seconds, a second request's turn
// would come past a run's deadline, and it isn't asked. Caught up, it is.
func TestCatchUpRate(t *testing.T) {
	q := &quotaAPI{asked: map[string]int{}}
	api := httptest.NewServer(q)
	t.Cleanup(api.Close)
	s, _ := server(t, func(c *Config) { c.CatchUpRate = prom.MinRate })
	c := prom.New(api.URL, "test")
	ask := func(cursor time.Time) error {
		ctx, cancel := context.WithTimeout(s.asking(context.Background(), cursor, now), 10*time.Minute)
		defer cancel()
		for range 2 {
			if _, err := c.QueryRange(ctx, "up", now.Add(-time.Hour), now, time.Minute); err != nil {
				return err
			}
		}
		return nil
	}
	if err := ask(now.Add(-24 * time.Hour)); !errors.Is(err, prom.ErrPastDeadline) || q.total() != 1 {
		t.Errorf("catching up: %v, asked %d", err, q.total())
	}
	if err := ask(now.Add(-time.Hour)); err != nil || q.total() != 3 {
		t.Errorf("caught up: %v, asked %d", err, q.total())
	}
}

func TestWindow(t *testing.T) {
	for _, c := range []struct {
		in    string
		len   time.Duration
		has   []int // hours in it
		hasnt []int
	}{
		{"01:00-06:00", 5 * time.Hour, []int{1, 5}, []int{0, 6, 12, 23}},
		{"22:00-04:30", 6*time.Hour + 30*time.Minute, []int{22, 23, 0, 4}, []int{5, 12, 21}},
		{"00:00-24:00", 24 * time.Hour, []int{0, 12, 23}, nil},
	} {
		w, err := ParseWindow(c.in)
		if err != nil || w.Len() != c.len || w.String() != c.in {
			t.Errorf("%s: %v %s %s", c.in, err, w.Len(), w)
			continue
		}
		for _, h := range c.has {
			if !w.Has(time.Date(2026, 11, 3, h, 10, 0, 0, time.UTC), time.UTC) {
				t.Errorf("%s: not at %d:10", c.in, h)
			}
		}
		for _, h := range c.hasnt {
			if w.Has(time.Date(2026, 11, 3, h, 50, 0, 0, time.UTC), time.UTC) {
				t.Errorf("%s: at %d:50", c.in, h)
			}
		}
	}
	// In --tz: 01:00 in Seoul is 16:00 UTC.
	w, _ := ParseWindow("01:00-02:00")
	seoul := time.FixedZone("KST", 9*3600)
	if !w.Has(time.Date(2026, 11, 3, 16, 30, 0, 0, time.UTC), seoul) || w.Has(time.Date(2026, 11, 3, 1, 30, 0, 0, time.UTC), seoul) {
		t.Error("not in --tz")
	}
	for _, bad := range []string{"", "01:00", "1-6", "01:00-01:00", "25:00-01:00", "01:60-02:00", "24:30-01:00", "01:0-02:00"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("%q read", bad)
		}
	}
}
