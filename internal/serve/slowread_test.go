package serve

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
)

// smallBuffers is a listener whose connections send through a small
// buffer: a reader who doesn't read holds the writer sooner.
type smallBuffers struct{ net.Listener }

func (l smallBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetWriteBuffer(4096)
	}
	return c, err
}

// stalled asks for path over a connection whose reader reads nothing, its
// buffer small, without gzip.
func stalled(t *testing.T, addr, path string) net.Conn {
	t.Helper()
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) { syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096) })
	}}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: example.com\r\n\r\n", path)
	return c
}

// TestSlowReaders: a page too big to keep, asked by readers who don't read
// it, holds no page's turn while it goes to the network, so other pages are
// worked out still; and it goes for spillWithin at most, then the server
// gives it up.
func TestSlowReaders(t *testing.T) {
	s, _ := server(t, nil)
	day := now.Add(-48 * time.Hour)
	var fs []firing.Firing
	for i := 0; i < 300; i++ {
		at := day.Add(time.Duration(i) * time.Minute)
		fs = append(fs, firing.Firing{Rule: fmt.Sprintf("Rule%03d", i), Start: at, End: at.Add(time.Minute), Labels: map[string]string{"service": "x"}})
	}
	keep(t, s, day.Add(-time.Hour), day.Add(24*time.Hour), fs)
	wasOne, wasWait, wasWithin := cacheOne, renderWait, spillWithin
	defer func() { cacheOne, renderWait, spillWithin = wasOne, wasWait, wasWithin }()
	cacheOne, renderWait, spillWithin = 64<<10, 300*time.Millisecond, 2*time.Second

	srv := httptest.NewUnstartedServer(s.Handler())
	srv.Listener = smallBuffers{srv.Listener}
	srv.Start()
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	_, body, _ := get(t, s.Handler(), "/rules")
	if len(body) < 4*cacheOne {
		t.Fatalf("the rules list is %d bytes, too few to fill the buffers", len(body))
	}
	// An API's answer as big, read by a client as slow: not paced.
	_, api, _ := get(t, s.Handler(), "/api/v1/rules")
	if len(api) < 2*cacheOne {
		t.Fatalf("the rules API is %d bytes, too few to spill", len(api))
	}
	apiConn := stalled(t, addr, "/api/v1/rules")
	defer apiConn.Close()
	before := spills.Load()
	var conns []net.Conn
	for range pageRenders {
		conns = append(conns, stalled(t, addr, "/rules"))
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for began := time.Now(); spills.Load()-before < pageRenders; time.Sleep(10 * time.Millisecond) {
		if time.Since(began) > 20*time.Second {
			t.Fatalf("%d of %d spilled", spills.Load()-before, pageRenders)
		}
	}
	// Another page, not kept, is worked out.
	res, err := http.Get(srv.URL + "/runs")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("with %d readers not reading the rules: /runs %d", pageRenders, res.StatusCode)
	}
	// Past spillWithin, what wasn't read is given up: the page ends short.
	time.Sleep(spillWithin + 500*time.Millisecond)
	c := conns[0]
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, _ := io.ReadAll(bufio.NewReader(c))
	if !strings.HasPrefix(string(got), "HTTP/1.1 200") || strings.Contains(string(got), "</html>") {
		t.Errorf("a reader that didn't read got %d bytes, all of it %v", len(got), strings.Contains(string(got), "</html>"))
	}
	// The API's answer, read late, comes whole: its client has the
	// server's WriteTimeout, not a page's spillWithin.
	apiConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	whole, _ := io.ReadAll(bufio.NewReader(apiConn))
	if !strings.HasPrefix(string(whole), "HTTP/1.1 200") || !strings.Contains(string(whole), strings.TrimSpace(api)[len(strings.TrimSpace(api))-200:]) {
		t.Errorf("the API's answer, read late, came short: %d bytes of %d", len(whole), len(api))
	}
}

// TestPreviewTakesTurn: a quiet window's preview is worked out as a page
// is, a few at a time: with every turn taken it waits, then is answered
// 503, as a page is.
func TestPreviewTakesTurn(t *testing.T) {
	s, _, _ := rolesServer(t, nil)
	h := s.Handler()
	was := renderWait
	defer func() { renderWait = was }()
	renderWait = 50 * time.Millisecond
	if s.renders == nil {
		t.Fatal("no limit on pages worked out at once")
	}
	for range cap(s.renders) {
		s.renders <- struct{}{}
	}
	defer func() {
		for range cap(s.renders) {
			<-s.renders
		}
	}()
	f := windowForm("x", "2026-10-07T01:00", "2026-10-07T05:00", "preview", "1")
	if code, _ := as(t, h, http.MethodPost, "/windows", "ed@example.com", "", f); code != http.StatusServiceUnavailable {
		t.Errorf("a preview with every turn taken: %d, want 503", code)
	}
}
