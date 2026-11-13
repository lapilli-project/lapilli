package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
)

// pausedLoki is a Loki whose queries are answered 429, Retry-After a
// minute, and whose ruler is counted; and the Limiter of its clients, told
// to wait by a query's answer.
func pausedLoki(t *testing.T) (*httptest.Server, *atomic.Int64, *prom.Limiter) {
	t.Helper()
	var ruler atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/query_range" {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many outstanding requests", http.StatusTooManyRequests)
			return
		}
		ruler.Add(1)
		fmt.Fprint(w, `{"status":"success","data":{"groups":[]}}`)
	}))
	t.Cleanup(srv.Close)
	l := prom.NewLimiter(0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := (Loki{URL: srv.URL, Limit: l}).History("test").QueryRange(ctx, `sum(count_over_time({a="b"}[1m]))`, t0, t0.Add(time.Hour), time.Minute); err == nil {
		t.Fatal("want the 429")
	}
	return srv, &ruler, l
}

// TestLokiRulerShares: a Loki's ruler is read by the Limiter its queries
// are asked by: told to wait by an answer to a query past the deadline, it
// isn't asked; asked as a read lapilli does without, it isn't asked while
// told to wait, and fails at once.
func TestLokiRulerShares(t *testing.T) {
	srv, ruler, l := pausedLoki(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := LokiPlaces(ctx, Loki{URL: srv.URL, Limit: l}, "test"); !errors.Is(err, prom.ErrPastDeadline) || ruler.Load() != 0 {
		t.Errorf("%v, the ruler asked %d times", err, ruler.Load())
	}
	start := time.Now()
	if _, err := LokiPlacesOnce(context.Background(), Loki{URL: srv.URL, Limit: l}, "test"); err == nil || ruler.Load() != 0 || time.Since(start) > 5*time.Second {
		t.Errorf("once: %v after %s, the ruler asked %d times", err, time.Since(start), ruler.Load())
	}
}

// TestLokiClientGivesUp: a Loki's client, and a Grafana data source's,
// gives up on a request after five minutes, as prom's does.
func TestLokiClientGivesUp(t *testing.T) {
	if noRedirects.Timeout != 5*time.Minute {
		t.Errorf("timeout %s", noRedirects.Timeout)
	}
}
