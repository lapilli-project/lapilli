package serve

// The test servers a script reads pages from (TestA11yServe, TestDemoServe):
// everyone is an admin on them, so they listen on this machine alone, and
// stop when told to, the ledger they made taken away with them.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// serveAddr is where the variable env says a test server is to listen:
// it skips the test when it is unset, and fails it, before anything is
// made, when it isn't loopback alone.
func serveAddr(t *testing.T, env string) string {
	t.Helper()
	addr := os.Getenv(env)
	if addr == "" {
		t.Skip(env + " isn't set")
	}
	if !LoopbackOnly(addr) {
		t.Fatalf("%s=%s: a test server, on which everyone is an admin, listens on loopback alone, such as 127.0.0.1:18431", env, addr)
	}
	return addr
}

// loopbackListener listens at addr, which must be this machine's alone: a
// loopback address, or localhost bound to one.
func loopbackListener(addr string) (net.Listener, error) {
	if !LoopbackOnly(addr) {
		return nil, fmt.Errorf("%s: a test server, on which everyone is an admin, listens on loopback alone, such as 127.0.0.1:18431", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		ln.Close()
		return nil, fmt.Errorf("%s is bound to %s, not loopback", addr, ln.Addr())
	}
	return ln, nil
}

// serveTest serves h for s at addr, on loopback alone, the server told
// where it listens, until the process is told to stop (SIGINT, SIGTERM):
// then it returns, and the test's cleanup takes its ledger away.
func serveTest(t *testing.T, s *Server, addr string, h http.Handler) {
	t.Helper()
	ln, err := loopbackListener(addr)
	if err != nil {
		t.Fatal(err)
	}
	s.listening(ln.Addr())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Minute}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	t.Logf("serving on %s", ln.Addr())
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}

// TestLoopbackListener: a test server listens on loopback alone; asked to
// listen anywhere else, it refuses before it listens.
func TestLoopbackListener(t *testing.T) {
	ln, err := loopbackListener("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0", "192.0.2.1:0", "lapilli.example:0"} {
		ln, err := loopbackListener(addr)
		if err == nil {
			ln.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "listens on loopback alone") {
			t.Errorf("%s: not refused before listening: %v", addr, err)
		}
	}
}
