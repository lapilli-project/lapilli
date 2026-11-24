package serve

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// Config.Clock is the time serve judges by, in place of time.Now, and
// Config.Listening is told where the UI's listener was bound, which then
// answers: what lapilli demo --serve pins, and says the URL of.
func TestClockAndListening(t *testing.T) {
	parallel(t)
	s0, _ := server(t, nil)
	cfg := s0.cfg
	s0.Close()
	at := time.Date(2026, 11, 16, 0, 15, 0, 0, time.UTC)
	bound := make(chan net.Addr, 1)
	cfg.Clock = func() time.Time { return at }
	cfg.Listening = func(a net.Addr) { bound <- a }
	s, err := New(context.Background(), cfg, &history{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if !s.now().Equal(at) {
		t.Errorf("serve's clock says %v, not %v", s.now(), at)
	}
	if from, to := s.lastWeek(); !from.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the last week is %v → %v", from, to)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	returned := false
	defer func() {
		cancel()
		if err := <-done; !returned && err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	var a net.Addr
	select {
	case a = <-bound:
	case err := <-done:
		returned = true
		done <- err // for the deferred receive
		t.Fatalf("Run returned before Listening was told where it listens: %v", err)
	}
	tcp, ok := a.(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() || tcp.Port == 0 {
		t.Fatalf("bound to %v", a)
	}
	resp, err := (&http.Client{Transport: &http.Transport{}}).Get("http://" + a.String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz at %v: %d", a, resp.StatusCode)
	}
}
