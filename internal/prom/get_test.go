package prom

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

// Get asks again after an answer that says to, as a query does, and every
// try waits its turn by the Limiter: at --max-rate, a retry too is spaced
// from the request before it, by the clock the package tells time by.
func TestGetPaced(t *testing.T) {
	quick(t, 0)
	f := fakeTime(t)
	var mu sync.Mutex
	var at []time.Time
	hit := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			at = append(at, f.now())
			mu.Unlock()
			h(w, r)
		}
	}
	srv, n := answers(t, hit(refuse(http.StatusServiceUnavailable, "")), hit(refuse(http.StatusBadGateway, "")),
		hit(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":true}`)) }))
	c := New(srv.URL, "test", WithLimiter(NewLimiter(0.5))) // every 2s
	body, err := c.Get(context.Background(), "/api/x?y=1", 1<<10)
	if err != nil || string(body) != `{"ok":true}` || n.Load() != 3 {
		t.Fatalf("%q %v, asked %d times", body, err, n.Load())
	}
	for i := 1; i < len(at); i++ {
		if d := at[i].Sub(at[i-1]); d < 2*time.Second {
			t.Errorf("try %d came %s after the one before, within the 2s --max-rate 0.5 spaces them", i+1, d)
		}
	}
	// An answer longer than asked for isn't read.
	srv, _ = answers(t, func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 2048)) })
	if _, err := New(srv.URL, "test").Get(context.Background(), "/", 1024); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a long answer: %v", err)
	}
	// A status that isn't 2xx and doesn't say to ask again is an APIError.
	srv, n = answers(t, refuse(http.StatusForbidden, ""))
	var ae *APIError
	if _, err := New(srv.URL, "test").Get(context.Background(), "/", 1024); !errors.As(err, &ae) || ae.Status != http.StatusForbidden || n.Load() != 1 {
		t.Errorf("403: %v, asked %d times", err, n.Load())
	}
}
