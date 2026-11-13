package ledger

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lapilli.dev/internal/ledger/ledgertest"
)

// A search_path whose first schema holds no ledger and a later one does
// would have the lock keyed by the first and the statements write the
// later one's tables: a second writer of a ledger another holds. It is
// refused. With no ledger on the path, the ledger is made in the first
// schema, and its statements read that schema alone.
func TestPostgresSearchPath(t *testing.T) {
	u := ledgertest.URL(t)
	if u == "" {
		t.Skip("no " + ledgertest.Env)
	}
	ctx := context.Background()
	first, err := OpenPostgres(ctx, u, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	pu, _ := url.Parse(u)
	held := pu.Query().Get("search_path")
	ou, _ := url.Parse(ledgertest.URL(t))
	empty := ou.Query().Get("search_path")
	q := pu.Query()
	q.Set("search_path", empty+","+held)
	pu.RawQuery = q.Encode()
	if second, err := OpenPostgres(ctx, pu.String(), time.UTC); err == nil {
		second.Close()
		t.Fatal("a second process opened the ledger through a search_path naming an empty schema first")
	} else if !strings.Contains(err.Error(), `finds the ledger in schema "`+held+`"`) {
		t.Errorf("refused, saying: %v", err)
	}
	c, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var w int64
	if err := c.QueryRow(ctx, "SELECT writers FROM schema_version").Scan(&w); err != nil || w != 1 {
		t.Errorf("writers: %d %v", w, err)
	}
	// Made in the first schema of a path that holds no ledger, which is
	// all its statements read: a table of the same name further on is
	// another's.
	third := ledgertest.URL(t)
	tu, _ := url.Parse(third)
	fresh := tu.Query().Get("search_path")
	if _, err := c.Exec(ctx, "CREATE TABLE "+empty+".changes (id int)"); err != nil {
		t.Fatal(err)
	}
	q = tu.Query()
	q.Set("search_path", fresh+","+empty)
	tu.RawQuery = q.Encode()
	l, err := OpenPostgres(ctx, tu.String(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.SetSwitch(ctx, "paused", true, "ana", "paused"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := c.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema = $1", fresh).Scan(&n); err != nil || n < 30 {
		t.Errorf("tables made in %s: %d %v", fresh, n, err)
	}
	if err := c.QueryRow(ctx, "SELECT count(*) FROM "+fresh+".changes").Scan(&n); err != nil || n != 1 {
		t.Errorf("the change written in %s: %d %v", fresh, n, err)
	}
}

// partition is a TCP proxy to a server. cut cuts every connection open
// now: what either side sends is lost, and neither side is told, as when a
// network fails. Connections made after go through.
type partition struct {
	ln   net.Listener
	mu   sync.Mutex
	dead []*atomic.Bool
	kept []net.Conn
}

func (p *partition) run(up string) {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		s, err := net.Dial("tcp", up)
		if err != nil {
			c.Close()
			continue
		}
		dead := &atomic.Bool{}
		p.mu.Lock()
		p.dead = append(p.dead, dead)
		p.kept = append(p.kept, s, c)
		p.mu.Unlock()
		pipe := func(dst, src net.Conn) {
			b := make([]byte, 64<<10)
			for {
				n, err := src.Read(b)
				if n > 0 && !dead.Load() {
					if _, werr := dst.Write(b[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					if !dead.Load() {
						dst.Close()
					}
					return
				}
			}
		}
		go pipe(s, c)
		go pipe(c, s)
	}
}

func (p *partition) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.dead {
		d.Store(true)
	}
}

func (p *partition) close() {
	p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.kept {
		s.Close()
	}
}

// A connection made again takes the lock, and the network fails before it
// is ready: the session it made holds the lock on the server still. The
// next connection finds this process's own session holding it, ends it,
// and goes on, as no other process ran.
func TestPostgresOwnSessionFromAReconnect(t *testing.T) {
	u := ledgertest.URL(t)
	if u == "" {
		t.Skip("no " + ledgertest.Env)
	}
	ctx := context.Background()
	pu, _ := url.Parse(u)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &partition{ln: ln}
	go p.run(pu.Host)
	defer p.close()
	via := *pu
	via.Host = ln.Addr().String()
	l, err := OpenPostgres(ctx, via.String(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	direct, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close(ctx)
	holder := func() int64 {
		var pid int64
		direct.QueryRow(ctx, `SELECT coalesce(max(pid), 0) FROM pg_locks WHERE locktype = 'advisory' AND granted AND objsubid = 2
			AND classid::bigint = $1 AND objid::bigint = $2`, int64(lockClass), int64(l.pg.key)).Scan(&pid)
		return pid
	}
	first := int64(l.pg.pid)
	direct.Exec(ctx, "SELECT pg_terminate_backend($1, 5000)", first)
	l.SetMeta(ctx, "w", "1") // finds the session gone
	done := make(chan error, 1)
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go func() { done <- l.SetMeta(c, "x", "1") }()
	mine := int64(0)
	for range 3000 {
		if h := holder(); h != 0 && h != first {
			mine = h
			p.cut()
			break
		}
		time.Sleep(time.Millisecond)
	}
	if mine == 0 {
		t.Fatal("the connection made again never took the lock")
	}
	<-done
	var err2 error
	for range 3 {
		if err2 = l.SetMeta(ctx, "y", "1"); err2 == nil {
			break
		}
	}
	select {
	case <-l.Lost():
		t.Fatalf("taken for lost, though no other process ran: %v", err2)
	default:
	}
	if err2 != nil {
		t.Fatalf("after its own session from the reconnect was ended: %v", err2)
	}
	if h := holder(); h == mine || h == 0 {
		t.Errorf("the lock is held by %d, the reconnect's session was %d", h, mine)
	}
}

// A process whose writer's session went, and another took the ledger,
// doesn't hold it: Hold says so before it sends what a reader found due,
// and never says it holds it again.
func TestPostgresHold(t *testing.T) {
	u := ledgertest.URL(t)
	if u == "" {
		t.Skip("no " + ledgertest.Env)
	}
	ctx := context.Background()
	a, err := OpenPostgres(ctx, u, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Hold(ctx); err != nil {
		t.Fatalf("held: %v", err)
	}
	c, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, "SELECT pg_terminate_backend($1, 5000)", int64(a.pg.pid)); err != nil {
		t.Fatal(err)
	}
	b, err := OpenPostgres(ctx, u, time.UTC)
	if err != nil {
		t.Fatalf("another process: %v", err)
	}
	defer b.Close()
	for i := range 3 {
		if err := a.Hold(ctx); err == nil {
			t.Fatalf("try %d: a process whose lock another took holds it", i)
		} else if i > 0 && !errors.Is(err, ErrLockLost) {
			t.Errorf("try %d: %v", i, err)
		}
	}
	if err := b.Hold(ctx); err != nil {
		t.Errorf("the one that took it: %v", err)
	}
}
