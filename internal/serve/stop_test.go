package serve

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// freeAddr is an address on this machine nothing listens on, for a server
// to listen on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// Told to stop with --shutdown-delay, serve is no longer ready, and goes
// on answering on its ports, each answer closing its connection, until the
// delay is out; a request under way when it was told finishes as it would
// have, its context not ended by the stop. A rolling restart's pod stopped
// listening at once, while the Service's endpoints still sent it requests,
// and ended the requests it had (test/e2e/replicas.sh, now and then).
func TestRunDrains(t *testing.T) {
	ui, metrics := freeAddr(t), freeAddr(t)
	s, _ := server(t, func(c *Config) { c.Listen, c.MetricsListen, c.ShutdownDelay = ui, metrics, time.Hour })
	defer func(was func(time.Duration) <-chan time.Time) { waitDrain = was }(waitDrain)
	out := make(chan time.Time)
	asked := make(chan time.Duration, 1)
	waitDrain = func(d time.Duration) <-chan time.Time {
		asked <- d
		return out
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	ran := make(chan error, 1)
	go func() { ran <- s.Run(ctx) }()
	get := func(addr, path string) (int, string, bool, error) {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			return 0, "", false, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, string(b), resp.Close, nil
	}
	until(t, 30*time.Second, "serving", func() bool {
		c, _, _, err := get(metrics, "/healthz")
		return err == nil && c == http.StatusOK
	})
	// A task under way: its request sent but for the end of its body.
	conn, err := net.Dial("tcp", ui)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"title": "drained", "rule": "Errors"}`
	head := "POST /api/v1/tasks HTTP/1.1\r\nHost: " + ui + "\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
	if _, err := io.WriteString(conn, head+body[:10]); err != nil {
		t.Fatal(err)
	}
	stop()
	until(t, 30*time.Second, "not ready once told to stop", func() bool {
		c, said, _, err := get(metrics, "/readyz")
		return err == nil && c == http.StatusServiceUnavailable && strings.Contains(said, "stopping")
	})
	if d := <-asked; d != time.Hour {
		t.Errorf("waited out %v, want --shutdown-delay", d)
	}
	// Meanwhile a request is answered, its connection closed after.
	for _, p := range []string{"/healthz", "/api/v1/runs"} {
		if c, said, closed, err := get(ui, p); err != nil || c != http.StatusOK || !closed {
			t.Errorf("%s while stopping: %d %v, closed %v: %s", p, c, err, closed, said)
		}
	}
	// On Postgres, the lease given up meanwhile, for another replica.
	if s.l.Shared() && s.l.Leading() {
		t.Error("still leading while it answers on")
	}
	// The task under way finishes.
	if _, err := io.WriteString(conn, body[10:]); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	said, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("the task under way when told to stop: %d %s", resp.StatusCode, said)
	}
	close(out)
	select {
	case err := <-ran:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("never stopped")
	}
	if _, _, _, err := get(ui, "/healthz"); err == nil {
		t.Error("still answering once stopped")
	}
}

// Without --shutdown-delay, serve told to stop closes its ports at once,
// ready to the last.
func TestRunStopsAtOnce(t *testing.T) {
	s, _ := server(t, func(c *Config) { c.Listen = freeAddr(t) })
	defer func(was func(time.Duration) <-chan time.Time) { waitDrain = was }(waitDrain)
	waited := false
	waitDrain = func(d time.Duration) <-chan time.Time {
		waited = true
		return time.After(0)
	}
	ctx, stop := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- s.Run(ctx) }()
	until(t, 30*time.Second, "serving", func() bool {
		resp, err := http.Get("http://" + s.cfg.Listen + "/healthz")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil
	})
	stop()
	select {
	case err := <-ran:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("never stopped")
	}
	if waited || s.stopping.Load() {
		t.Error("waited for a --shutdown-delay not given")
	}
}

// A request whose context ended before it was done, as serve stops, is
// answered 503, to be asked again: an answer of nothing was a 200, and a
// write that wasn't done read as done, a notification as kept.
func TestCanceledIsNotDone(t *testing.T) {
	s := notifyServer(t, nil)
	for _, p := range []string{"/api/v1/tasks", "/tasks"} {
		w := httptest.NewRecorder()
		s.oops(w, httptest.NewRequest(http.MethodPost, p, nil), fmt.Errorf("adding a task: %w", context.Canceled))
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), stoppedText) {
			t.Errorf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
	ended, end := context.WithCancel(context.Background())
	end()
	r := httptest.NewRequestWithContext(ended, http.MethodPost, "/v1/alertmanager", bytes.NewReader(notifyFixture(t, "alertmanager.json")))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+notifyToken)
	w := httptest.NewRecorder()
	s.NotifyHandler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || len(kept(t, s)) != 0 {
		t.Errorf("a notification whose request ended: %d %s, %d kept", w.Code, w.Body.String(), len(kept(t, s)))
	}
}
