package ledger

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
)

// What TLS a connection string asks for is read from what pgx makes of
// it, the string never written again, so that lapilli reads every string
// as pgx does: to a server on another machine, one that checks the
// server's certificate is taken; pgx's prefer, its default, refused
// (ErrUnchecked); any other mode taken at its word, as a string, a service
// file or PGSSLMODE says it, and warned of. On this machine, as given.
func TestPostgresTLSDefault(t *testing.T) {
	t.Setenv("PGSSLMODE", "")
	dir := t.TempDir()
	svc := filepath.Join(dir, "pg_service.conf")
	if err := os.WriteFile(svc, []byte("[remote]\nhost=db.example\nsslmode=disable\nuser=u\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const (
		refused = "refused"
		checked = "checked"
		warned  = "warned"
		local   = "local"
	)
	for _, c := range []struct {
		conn, want, mode string
	}{
		{"postgres://u:p@db.example/x", refused, ""},
		{"host=db.example user=u password=p dbname=x", refused, ""},
		{"postgres://u:p@db.example,db2.example/x", refused, ""},
		{"postgres://u:p@db.example/x?sslmode=prefer", refused, ""},
		{"postgres://u:pa?ss@db.example/x", refused, ""},                     // a ? in the password, as pgx reads it
		{"postgres://u:p@[::1]:5432,db.example:5432/x", refused, ""},         // a bracketed host among others
		{"host=db.example user=u password='a sslmode=disable'", refused, ""}, // sslmode= inside a quoted value
		{"postgres://u:p@db.example/x?sslmode=verify-full", checked, ""},
		{"host=db.example sslmode=verify-ca user=u", checked, ""},
		{"postgres://u:p@db.example/x?options=-c%20lock_timeout%3D10s&application_name=my%20app&sslmode=verify-full", checked, ""},
		{"postgres://u:p@db.example/x?sslmode=require", warned, "require"},
		{"postgres://u:p@db.example/x?ssl=true", warned, "require"},
		{"postgres://u:p@db.example/x?sslmode=disable", warned, "disable"},
		{"postgres://u:p@db.example/x?sslmode=allow", warned, "allow"},
		{"postgres://u:p@db.example/x?a=1#&sslmode=disable", warned, "disable"},                // a fragment Go would strip, pgx reads
		{"postgres://u:p@db.example/x?sslmode=verify-full&sslmode=disable", warned, "disable"}, // pgx takes the last
		{"service=remote servicefile=" + svc, warned, "disable"},
		{"postgres://u:p@127.0.0.1:5432/x", local, ""},
		{"postgres://u:p@localhost/x", local, ""},
		{"host=/var/run/postgresql user=u", local, ""},
		{"postgres://u:p@127.0.0.1/x?sslmode=disable", local, ""},
	} {
		cfg, warn, err := pgConfig(c.conn)
		if c.want == refused {
			if !errors.Is(err, ErrUnchecked) || strings.Contains(err.Error(), "p@") || !strings.Contains(err.Error(), "db.example") {
				t.Errorf("%s: %v, want ErrUnchecked naming the host", c.conn, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.conn, err)
		}
		// Read as pgx reads it: nothing of the string changed.
		px, err := pgx.ParseConfig(c.conn)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Host != px.Host || cfg.Password != px.Password || cfg.Database != px.Database || cfg.User != px.User ||
			cfg.RuntimeParams["options"] != px.RuntimeParams["options"] || (cfg.TLSConfig == nil) != (px.TLSConfig == nil) || len(cfg.Fallbacks) != len(px.Fallbacks) {
			t.Errorf("%s: read otherwise than pgx reads it", c.conn)
		}
		switch c.want {
		case checked, local:
			if warn != "" {
				t.Errorf("%s: warned %q", c.conn, warn)
			}
		case warned:
			if !strings.Contains(warn, "sslmode="+c.mode) {
				t.Errorf("%s: warned %q, want sslmode=%s", c.conn, warn, c.mode)
			}
		}
	}
	cfg, _, err := pgConfig("postgres://u:p@db.example/x?options=-c%20lock_timeout%3D10s&application_name=my%20app&sslmode=verify-full")
	if err != nil || cfg.RuntimeParams["options"] != "-c lock_timeout=10s" || cfg.RuntimeParams["application_name"] != "my app" {
		t.Errorf("options and application_name: %v %q %q", err, cfg.RuntimeParams["options"], cfg.RuntimeParams["application_name"])
	}
	// What pgx refuses, or takes, of a string ending in a backslash: never
	// a password with anything of lapilli's after it.
	if cfg, _, err := pgConfig(`host=db.example user=u password=abc\`); err == nil || errors.Is(err, ErrUnchecked) {
		if cfg != nil && strings.Contains(cfg.Password, "sslmode") {
			t.Errorf("password %q", cfg.Password)
		}
	} else if !errors.Is(err, ErrBadURL) {
		t.Errorf("a trailing backslash: %v", err)
	}
	for mode, want := range map[string]string{"require": warned, "verify-full": checked, "prefer": refused} {
		t.Setenv("PGSSLMODE", mode)
		_, warn, err := pgConfig("postgres://u:p@db.example/x")
		switch {
		case want == refused && !errors.Is(err, ErrUnchecked), want != refused && err != nil,
			want == warned && warn == "", want == checked && warn != "":
			t.Errorf("PGSSLMODE=%s: %q %v", mode, warn, err)
		}
	}
}

// A server in the path that declines TLS and asks for the password in
// clear, where a connection string says nothing of sslmode, is sent none.
func TestPostgresTLSPasswordKept(t *testing.T) {
	t.Setenv("PGSSLMODE", "")
	was := onThisMachine
	onThisMachine = func(*pgx.ConnConfig) bool { return false } // as a server across a network
	defer func() { onThisMachine = was }()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				be := pgproto3.NewBackend(c, c)
				for {
					m, err := be.ReceiveStartupMessage()
					if err != nil {
						return
					}
					if _, ok := m.(*pgproto3.SSLRequest); ok {
						c.Write([]byte("N"))
						continue
					}
					if _, ok := m.(*pgproto3.GSSEncRequest); ok {
						c.Write([]byte("N"))
						continue
					}
					break
				}
				be.Send(&pgproto3.AuthenticationCleartextPassword{})
				if err := be.Flush(); err != nil {
					return
				}
				be.SetAuthType(pgproto3.AuthTypeCleartextPassword)
				if m, err := be.Receive(); err == nil {
					if p, ok := m.(*pgproto3.PasswordMessage); ok {
						got <- p.Password
					}
				}
			}(c)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for conn, want := range map[string]error{
		"postgres://lapilli:s3cretPassw0rd@" + ln.Addr().String() + "/ledger":                     ErrUnchecked,
		"postgres://lapilli:s3cretPassw0rd@" + ln.Addr().String() + "/ledger?sslmode=verify-full": ErrConnect,
	} {
		if _, err := OpenPostgres(ctx, conn, time.UTC); !errors.Is(err, want) {
			t.Errorf("a server that declines TLS: %v, want %v", err, want)
		}
	}
	select {
	case p := <-got:
		t.Errorf("the password went in clear: %q", p)
	case <-time.After(200 * time.Millisecond):
	}
}
