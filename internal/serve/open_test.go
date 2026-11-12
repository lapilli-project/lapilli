package serve

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// With no roles, everyone who reaches lapilli serve is an admin only when
// it listens on loopback alone, as before, or with --everyone-admin. The
// chart listened on :8080 with no roles: any pod in the cluster was an
// admin of the UI and the API, and could write incidents and settings.
// Beyond loopback, everyone now may only look, and the log says so at
// start.
func TestNoRolesClosedBeyondLoopback(t *testing.T) {
	for _, c := range []struct {
		listen        string
		everyoneAdmin bool
		roles         bool
		role, logged  string
	}{
		{":8080", false, false, "viewer", "everyone may only look"},
		{"0.0.0.0:8080", false, false, "viewer", "everyone may only look"},
		{"10.1.2.3:8080", false, false, "viewer", "everyone may only look"},
		{"[::]:8080", false, false, "viewer", "everyone may only look"},
		{"lapilli.example.com:8080", false, false, "viewer", "everyone may only look"},
		{"127.0.0.1:8080", false, false, "admin", "listens on this machine alone"},
		{"[::1]:8080", false, false, "admin", "listens on this machine alone"},
		{"localhost:8080", false, false, "admin", "listens on this machine alone"},
		{":8080", true, false, "admin", "--everyone-admin"},
		{":8080", false, true, "viewer", "roles:"},
	} {
		t.Run(c.listen, func(t *testing.T) {
			var logged bytes.Buffer
			s, _ := server(t, func(cfg *Config) {
				cfg.Listen, cfg.EveryoneAdmin = c.listen, c.everyoneAdmin
				if c.roles {
					cfg.UserHeader, cfg.Admins = "X-Forwarded-Email", []string{"ana@example.com"}
				}
			})
			// What New logged went nowhere: say it again, here.
			s.log = slog.New(slog.NewTextHandler(&logged, nil))
			s.logWho()
			if !strings.Contains(logged.String(), c.logged) {
				t.Errorf("logged %q, want %q", logged.String(), c.logged)
			}
			h := s.Handler()
			if _, body, _ := get(t, h, "/api/v1/me"); !strings.Contains(body, `"role":"`+c.role+`"`) {
				t.Errorf("/api/v1/me: %s, want %s", body, c.role)
			}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/incidents", strings.NewReader(`{"id":"INC-9","title":"Checkout down","start":"2026-11-02T07:00:00Z","end":"2026-11-02T07:30:00Z"}`))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			wrote := w.Code/100 == 2
			if wrote != (c.role == "admin") {
				t.Errorf("writing an incident as %s: %d %s", c.role, w.Code, w.Body)
			}
			if !wrote && !c.roles && !strings.Contains(w.Body.String(), "listens beyond this machine with no roles") {
				t.Errorf("refused without saying why: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestLoopbackOnly(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "127.1.2.3:80": true, "[::1]:8080": true, "localhost:8080": true, "LOCALHOST:1": true,
		":8080": false, "0.0.0.0:8080": false, "[::]:8080": false, "10.0.0.1:8080": false, "lapilli:8080": false,
		"": false, "127.0.0.1": false,
	} {
		if got := LoopbackOnly(addr); got != want {
			t.Errorf("%q: %v, want %v", addr, got, want)
		}
	}
}

// Without roles, everyone is an admin only where the listener is bound to
// loopback, whatever --listen said: localhost is what the resolver says,
// and /etc/hosts can make it another address. A listener bound beyond
// loopback is closed; --everyone-admin opens it anyway.
func TestOpenOnlyWhereBound(t *testing.T) {
	for _, c := range []struct {
		listen        string
		everyoneAdmin bool
		bound         net.Addr
		role          string
	}{
		{"localhost:8080", false, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}, "admin"},
		{"localhost:8080", false, &net.TCPAddr{IP: net.ParseIP("::1"), Port: 8080}, "admin"},
		{"localhost:8080", false, &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 8080}, "viewer"},
		{"127.0.0.1:8080", false, &net.TCPAddr{IP: net.ParseIP("192.168.1.2"), Port: 8080}, "viewer"},
		{":8080", false, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}, "viewer"},
		{"localhost:8080", false, &net.UnixAddr{Name: "/tmp/x", Net: "unix"}, "viewer"},
		{":8080", true, &net.TCPAddr{IP: net.IPv6unspecified, Port: 8080}, "admin"},
	} {
		s, _ := server(t, func(cfg *Config) { cfg.Listen, cfg.EveryoneAdmin = c.listen, c.everyoneAdmin })
		s.listening(c.bound)
		if _, body, _ := get(t, s.Handler(), "/api/v1/me"); !strings.Contains(body, `"role":"`+c.role+`"`) {
			t.Errorf("--listen %s bound to %s: %s, want %s", c.listen, c.bound, body, c.role)
		}
	}
	// As Run binds it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	s, _ := server(t, func(cfg *Config) { cfg.Listen = ln.Addr().String() })
	s.listening(ln.Addr())
	if _, body, _ := get(t, s.Handler(), "/api/v1/me"); !strings.Contains(body, `"role":"admin"`) {
		t.Errorf("bound to %s: %s", ln.Addr(), body)
	}
}
