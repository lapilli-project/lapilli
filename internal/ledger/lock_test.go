package ledger

import (
	"bytes"
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

	"lapilli.dev/internal/firing"
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
	// another's, and so is a function before pg_catalog's, here an md5
	// that would make every label set one (series_labels).
	third := ledgertest.URL(t)
	tu, _ := url.Parse(third)
	fresh := tu.Query().Get("search_path")
	for _, s := range []string{"CREATE TABLE " + empty + ".changes (id int)",
		"CREATE FUNCTION " + empty + ".md5(text) RETURNS text LANGUAGE sql IMMUTABLE AS $$SELECT 'same'$$"} {
		if _, err := c.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	q = tu.Query()
	q.Set("search_path", fresh+","+empty+",pg_catalog")
	tu.RawQuery = q.Encode()
	l, err := OpenPostgres(ctx, tu.String(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.SetSwitch(ctx, "paused", true, "ana", "paused"); err != nil {
		t.Fatal(err)
	}
	f := Found{From: t0, To: t0.Add(time.Hour), Run: Run{Kind: "test", Started: t0.Add(time.Hour), Finished: t0.Add(time.Hour), From: t0, To: t0.Add(time.Hour)},
		Firings: []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "a"}, Start: t0.Add(time.Minute)},
			{Rule: "Errors", Labels: map[string]string{"service": "b"}, Start: t0.Add(time.Minute)}}}
	if err := l.Record(ctx, f, opts); err != nil {
		t.Errorf("two alerts of a rule, with another's md5 on the path: %v", err)
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
// network fails past a proxy that keeps the connection up. Connections made
// after go through. With onLock set, the next connection to ask for the
// writer's lock is cut as the answer comes back, the server having run the
// question (its CommandComplete): the server gives the lock, and its
// answer is lost (cutLock says it was).
type partition struct {
	ln      net.Listener
	mu      sync.Mutex
	dead    []*atomic.Bool
	kept    []net.Conn
	onLock  atomic.Bool
	cutLock atomic.Bool
}

// newPartition starts a partition before the server a test's URL names,
// and gives the URL through it, without TLS so that it can be read.
func newPartition(t *testing.T, u string) (*partition, string) {
	t.Helper()
	pu, _ := url.Parse(u)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &partition{ln: ln}
	go p.run(pu.Host)
	t.Cleanup(p.close)
	via := *pu
	via.Host = ln.Addr().String()
	q := via.Query()
	q.Set("sslmode", "disable")
	via.RawQuery = q.Encode()
	return p, via.String()
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
		dead, asked := &atomic.Bool{}, &atomic.Bool{}
		p.mu.Lock()
		p.dead = append(p.dead, dead)
		p.kept = append(p.kept, s, c)
		p.mu.Unlock()
		// What a statement that read one row answers: CommandComplete,
		// "SELECT 1".
		done := []byte("C\x00\x00\x00\x0dSELECT 1\x00")
		pipe := func(dst, src net.Conn, toServer bool) {
			b := make([]byte, 64<<10)
			var tail []byte
			for {
				n, err := src.Read(b)
				if n > 0 && !dead.Load() {
					// What this read ends, maybe begun in the last.
					seen := append(tail, b[:n]...)
					has := func(x []byte) bool {
						i := bytes.LastIndex(seen, x)
						return i >= 0 && i+len(x) > len(tail)
					}
					tail = append([]byte(nil), seen[max(0, len(seen)-32):]...)
					switch {
					case !p.onLock.Load():
					case toServer && has([]byte("pg_try_advisory_lock")):
						asked.Store(true)
					case !toServer && asked.Load() && has(done) && p.onLock.CompareAndSwap(true, false):
						dead.Store(true)
						p.cutLock.Store(true)
						continue
					}
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
		go pipe(s, c, true)
		go pipe(c, s, false)
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

// quick shortens the ledger's deadlines on its server for a test.
func quick(t *testing.T) {
	c, a, w := connectWithin, aliveWithin, writeWithin
	connectWithin, aliveWithin, writeWithin = 2*time.Second, time.Second, 3*time.Second
	t.Cleanup(func() { connectWithin, aliveWithin, writeWithin = c, a, w })
}

// lockHolder is the session that holds a ledger's lock, 0 for none, as
// another connection sees it.
func lockHolder(ctx context.Context, c *pgx.Conn, l *Ledger) int64 {
	var pid int64
	c.QueryRow(ctx, `SELECT coalesce(max(pid), 0) FROM pg_locks WHERE locktype = 'advisory' AND granted AND objsubid = 2
		AND classid::bigint = $1 AND objid::bigint = $2`, int64(lockClass), int64(l.pg.key)).Scan(&pid)
	return pid
}

// A connection made again asks for the lock, the server gives it, and the
// network fails before the answer comes back: the session holds the lock
// on the server still, and the process never heard. The connection gives
// up within connectWithin; the next finds the process's own session
// holding the lock, ends it, and goes on, as no other process ran.
func TestPostgresOwnSessionFromAReconnect(t *testing.T) {
	u := ledgertest.URL(t)
	if u == "" {
		t.Skip("no " + ledgertest.Env)
	}
	quick(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	p, via := newPartition(t, u)
	l, err := OpenPostgres(ctx, via, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	direct, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close(ctx)
	first := int64(l.pg.pid)
	p.onLock.Store(true)
	if _, err := direct.Exec(ctx, "SELECT pg_terminate_backend($1, 5000)", first); err != nil {
		t.Fatal(err)
	}
	// A write or two: one finds the session gone, the next connects again.
	for i := 0; !p.cutLock.Load(); i++ {
		if i == 3 {
			t.Fatal("the writer never connected again")
		}
		began := time.Now()
		if err := l.SetMeta(ctx, "w", "1"); err == nil && p.cutLock.Load() {
			t.Fatal("a write went through a connection whose lock's answer was lost")
		}
		if took := time.Since(began); took > writeWithin+connectWithin {
			t.Fatalf("a write waited %v", took)
		}
	}
	mine := lockHolder(ctx, direct, l)
	if mine == 0 || mine == first {
		t.Fatalf("the lock is held by %d: the session the answer was lost from didn't take it", mine)
	}
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
	if h := lockHolder(ctx, direct, l); h == mine || h == 0 {
		t.Errorf("the lock is held by %d, the reconnect's session was %d", h, mine)
	}
}

// A connection that a proxy keeps open while nothing gets through (what is
// past it gone, or a server that stopped answering) holds nothing up for
// long: a write's statement gives up within writeWithin, a connection taken
// from the pool after a second within aliveWithin, and another is made, the
// writer's lock taken again from the session the server still keeps. TCP
// would hold either for as long as it holds the connection.
func TestPostgresSilentConnection(t *testing.T) {
	u := ledgertest.URL(t)
	if u == "" {
		t.Skip("no " + ledgertest.Env)
	}
	quick(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	p, via := newPartition(t, u)
	l, err := OpenPostgres(ctx, via, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Notes(ctx); err != nil {
		t.Fatal(err)
	}
	// A write whose second statement gets no answer.
	wctx, wcancel := context.WithTimeout(ctx, 15*time.Second)
	defer wcancel()
	began := time.Now()
	err = l.write(wctx, func(tx *txn) error {
		if _, err := tx.ExecContext(tx.ctx, "SELECT 1"); err != nil {
			return err
		}
		p.cut()
		_, err := tx.ExecContext(tx.ctx, "SELECT 2")
		return err
	})
	if took := time.Since(began); err == nil || took > writeWithin+2*time.Second {
		t.Errorf("a write without an answer: %v after %v, want an error within %v", err, took, writeWithin)
	}
	// The next connects again, and goes on.
	began = time.Now()
	var err2 error
	for range 3 {
		if err2 = l.SetMeta(wctx, "y", "1"); err2 == nil {
			break
		}
	}
	if err2 != nil || l.pg.isLost() {
		t.Fatalf("after a silent connection: %v (lost: %v)", err2, l.pg.isLost())
	}
	if took := time.Since(began); took > writeWithin+connectWithin+2*time.Second {
		t.Errorf("connected again in %v", took)
	}
	// A reader's connection, silent since the cut, taken again after a
	// second: given up within aliveWithin, and the read made on another.
	time.Sleep(aliveWithin + 200*time.Millisecond)
	rctx, rcancel := context.WithTimeout(ctx, 15*time.Second)
	defer rcancel()
	began = time.Now()
	if _, err := l.Notes(rctx); err != nil {
		t.Errorf("a read after its connection went silent: %v", err)
	}
	if took := time.Since(began); took > aliveWithin+connectWithin+2*time.Second {
		t.Errorf("a read after its connection went silent took %v, want within %v", took, aliveWithin)
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
	// A session that lives on without the lock (let go, here by hand)
	// doesn't hold it either.
	b.mu.Lock()
	_, err = b.w.ExecContext(ctx, "SELECT pg_advisory_unlock(?, ?)", int32(lockClass), b.pg.key)
	b.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Hold(ctx); !errors.Is(err, ErrLockLost) {
		t.Errorf("a session that let the lock go: %v", err)
	}
}

// A pid is given again once its session ends, maybe to another process's
// session, which may take the ledger: a session of this process's own is
// its pid and when it began, and another's that has the pid is not one.
// This process finds the ledger lost, and leaves the other's session be.
func TestPostgresPIDGivenAgain(t *testing.T) {
	u := ledgertest.URL(t)
	if u == "" {
		t.Skip("no " + ledgertest.Env)
	}
	quick(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := OpenPostgres(ctx, u, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	direct, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close(ctx)
	if _, err := direct.Exec(ctx, "SELECT pg_terminate_backend($1, 5000)", int64(a.pg.pid)); err != nil {
		t.Fatal(err)
	}
	b, err := OpenPostgres(ctx, u, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	// As if b's session had the pid of one of a's that ended: its pid, and
	// another start.
	a.pg.mu.Lock()
	a.pg.own[session{b.pg.pid, time.Now().Add(-time.Hour).UnixMicro()}] = true
	a.pg.mu.Unlock()
	for range 2 {
		a.SetMeta(ctx, "w", "1")
	}
	if !a.pg.isLost() {
		t.Error("a process whose lock another took doesn't find it lost")
	}
	if err := b.Hold(ctx); err != nil {
		t.Errorf("the process that took the ledger: %v", err)
	}
	if h := lockHolder(ctx, direct, b); h != int64(b.pg.pid) {
		t.Errorf("the lock is held by %d, the other process's session is %d", h, b.pg.pid)
	}
}
