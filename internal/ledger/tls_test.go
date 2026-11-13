package ledger

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
)

// A connection string that says nothing of sslmode, to a server not on
// this machine, asks for verify-full, pgx's prefer anywhere else; one that
// says otherwise is taken at its word, and warned of when it checks no
// certificate across a network.
func TestPostgresTLSDefault(t *testing.T) {
	t.Setenv("PGSSLMODE", "")
	for _, c := range []struct {
		conn, mode string
		warn       bool
	}{
		{"postgres://u:p@db.example/x", "verify-full", false},
		{"host=db.example user=u password=p dbname=x", "verify-full", false},
		{"postgres://u:p@db.example,db2.example/x", "verify-full", false},
		{"postgres://u:p@127.0.0.1:5432/x", "prefer", false},
		{"postgres://u:p@localhost/x", "prefer", false},
		{"host=/var/run/postgresql user=u", "socket", false},
		{"postgres://u:p@db.example/x?sslmode=require", "require", true},
		{"postgres://u:p@db.example/x?sslmode=disable", "disable", true},
		{"host=db.example sslmode=verify-ca user=u", "verify-ca", false},
		{"postgres://u:p@127.0.0.1/x?sslmode=disable", "disable", false},
	} {
		cfg, warn, err := pgConfig(c.conn)
		if err != nil {
			t.Fatalf("%s: %v", c.conn, err)
		}
		plain := cfg.TLSConfig == nil
		for _, f := range cfg.Fallbacks {
			plain = plain || f.TLSConfig == nil
		}
		switch c.mode {
		case "verify-full":
			if plain || cfg.TLSConfig.InsecureSkipVerify || cfg.TLSConfig.ServerName == "" {
				t.Errorf("%s: not verify-full: %+v", c.conn, cfg.TLSConfig)
			}
		case "prefer":
			if cfg.TLSConfig == nil || !plain {
				t.Errorf("%s: not pgx's prefer", c.conn)
			}
		case "socket": // pgx's prefer over a Unix socket: no TLS
			if cfg.TLSConfig != nil {
				t.Errorf("%s: TLS over a socket", c.conn)
			}
		}
		if (warn != "") != c.warn || c.warn && !strings.Contains(warn, "sslmode="+c.mode) {
			t.Errorf("%s: warned %q", c.conn, warn)
		}
	}
	t.Setenv("PGSSLMODE", "require")
	if cfg, warn, _ := pgConfig("postgres://u:p@db.example/x"); cfg.TLSConfig == nil || !cfg.TLSConfig.InsecureSkipVerify || warn == "" {
		t.Errorf("PGSSLMODE=require: %+v %q", cfg.TLSConfig, warn)
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
	_, err = OpenPostgres(ctx, "postgres://lapilli:s3cretPassw0rd@"+ln.Addr().String()+"/ledger", time.UTC)
	if !errors.Is(err, ErrConnect) {
		t.Errorf("a server that declines TLS: %v", err)
	}
	select {
	case p := <-got:
		t.Errorf("the password went in clear: %q", p)
	case <-time.After(200 * time.Millisecond):
	}
}
