package serve

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/ledger"
)

// The marked windows' pages over a busy ledger, by what they read and
// hold, not by time: twelve weeks of firings, a sale each Friday, a launch
// each morning, and a week of windows at the bounds (256 occurrences). The
// page reads the tallies of the days and weeks its occurrences cover, never
// the firings, so its rows are fewer than the firings by far; its heap
// peak, the least of three runs, is bounded likewise. The summary and the
// rules list inside a kind read alike.
func TestMarkedPagesScale(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	ctx := context.Background()
	s, _ := server(t, nil)
	o := s.cfg.Options
	r := rand.New(rand.NewPCG(5, 6))
	start := s.l.Midnight(ledger.Week(s.l.Day(now)) - 7*weeks)
	services := []string{"checkout", "search", "cart", "auth", "billing", "media"}
	var incs []incident.Incident
	for i := 0; i < weeks*20; i++ {
		at := start.Add(time.Duration(r.IntN(weeks*7*24*60)) * time.Minute)
		incs = append(incs, incident.Incident{ID: fmt.Sprintf("INC-%d", i), Start: at, End: at.Add(45 * time.Minute),
			Labels: incident.Labels{"service": {services[r.IntN(len(services))]}}})
	}
	if _, err := s.l.SyncFile(ctx, incs, o); err != nil {
		t.Fatal(err)
	}
	fri := start.Add(4*24*time.Hour + 18*time.Hour)
	ms := []ledger.Marked{
		{Kind: "sale", Title: "Friday sale", Start: fri, End: fri.Add(4 * time.Hour), Every: "week"},
		{Kind: "launch", Title: "Morning launch", Start: start.Add(9 * time.Hour), End: start.Add(11 * time.Hour), Every: "day", Labels: map[string]string{"service": "search"}},
	}
	busy := start.Add(7 * 7 * 24 * time.Hour)
	for i := range ledger.MarkedWeek - 16 {
		at := busy.Add(time.Duration(i) * 40 * time.Minute)
		ms = append(ms, ledger.Marked{Kind: fmt.Sprint("flash-", i%4), Title: fmt.Sprint("flash ", i), Start: at, End: at.Add(30 * time.Minute)})
	}
	if _, err := s.l.SyncMarked(ctx, ms, o, ledger.MarkedRead{}); err != nil {
		t.Fatal(err)
	}
	total := 0
	for d := start; d.Before(now); d = d.Add(24 * time.Hour) {
		var fs []firing.Firing
		for i := 0; i < 600; i++ {
			at := d.Add(time.Duration(r.IntN(24*60)) * time.Minute)
			fs = append(fs, firing.Firing{Rule: fmt.Sprintf("Rule%03d", r.IntN(200)), Labels: map[string]string{"service": services[r.IntN(len(services))]},
				Start: at, End: at.Add(time.Duration(1+r.IntN(30)) * time.Minute)})
		}
		total += len(fs)
		if err := s.l.Record(ctx, ledger.Found{From: d, To: d.Add(24*time.Hour - time.Second), Firings: fs,
			Run: ledger.Run{Kind: "backfill", Started: d, Finished: d}}, o); err != nil {
			t.Fatal(err)
		}
	}
	read := 0
	markedRows = func(n int) { read += n }
	defer func() { markedRows = nil }()
	h := s.Handler()
	for _, path := range []string{"/marked", "/marked/1", "/marked/3", "/api/v1/summary?team=*", "/rules?weeks=12&during=sale", "/rules?weeks=12&outside=sale", "/api/v1/marked"} {
		read = 0
		var code, size int
		peak := leastPeakServe(func() {
			s.cache.Lock()
			s.cache.gen = "" // worked out afresh
			s.cache.Unlock()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			code, size = w.Code, w.Body.Len()
		})
		t.Logf("%-32s %d %8d bytes, %6d rows read (of %d firings), heap peak +%d KiB", path, code, size, read/3, total, peak>>10)
		if code != 200 {
			t.Errorf("%s: %d", path, code)
		}
		// The tallies of twelve weeks and the four before, and their
		// incidents, once a run: fewer than a fifth of the firings.
		if read/3 > total/5 {
			t.Errorf("%s read %d rows of tallies and incidents, over a fifth of the %d firings", path, read/3, total)
		}
		if peak > 64<<20 {
			t.Errorf("%s: heap peak +%d MiB, over 64 MiB", path, peak>>20)
		}
	}
}

// leastPeakServe runs f three times and gives the least the heap rose above
// the live heap while it ran (as internal/prom's leastPeak): what f itself
// holds at once, as nearly as sampling tells.
func leastPeakServe(f func()) uint64 {
	least := ^uint64(0)
	for range 3 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		before := m.HeapAlloc
		stop, done := make(chan struct{}), make(chan struct{})
		var peak uint64
		go func() {
			defer close(done)
			var m runtime.MemStats
			for {
				runtime.ReadMemStats(&m)
				peak = max(peak, m.HeapAlloc)
				select {
				case <-stop:
					return
				case <-time.After(time.Millisecond):
				}
			}
		}()
		f()
		close(stop)
		<-done
		least = min(least, max(peak, before)-before)
	}
	return least
}
