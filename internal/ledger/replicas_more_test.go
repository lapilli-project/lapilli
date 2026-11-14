package ledger

// The replicas' guards the reviews of 0.35's replicas asked for: what a
// role that may only connect can do to them, writes and claims at the
// lease's end, refreshes and side statements that wait, failovers, and a
// leader whose work is stuck.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lapilli.dev/internal/firing"
)

// connectOnly is a connection as a role made for the test, which has
// nothing on the ledger but the CONNECT every role has on a new database.
func connectOnly(t *testing.T, u string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	b := make([]byte, 4)
	rand.Read(b)
	role := fmt.Sprintf("lt_role_%x", b)
	admin, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(ctx) })
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'lt-role-pw'"); err != nil {
		t.Fatal(err)
	}
	pu, _ := url.Parse(u)
	pu.User = url.UserPassword(role, "lt-role-pw")
	c, err := pgx.Connect(ctx, pu.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Close(ctx)
		admin.Exec(ctx, "DROP ROLE "+role)
	})
	return c
}

// A role that may only connect can't hold the writers' turn: it is a row
// lock, which needs the ledger's own privileges. Its advisory lock of the
// turn's old key holds nothing, and every replica writes on.
func TestReplicasConnectOnlyCantHoldTheTurn(t *testing.T) {
	u := replicaURL(t)
	ctx := context.Background()
	a, b := openReplica(t, u), openReplica(t, u)
	schema, _ := url.Parse(u)
	other := connectOnly(t, u)
	if _, err := other.Exec(ctx, "SELECT pg_advisory_lock($1, $2)", int32(writeClass), a.pg.key); err != nil {
		t.Fatal(err)
	}
	tx, err := other.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT 1 FROM "+schema.Query().Get("search_path")+".replica_gens FOR UPDATE"); err == nil {
		t.Fatal("a role that may only connect locked the writers' turn")
	}
	for i, l := range []*Ledger{a, b} {
		wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := l.SetMeta(wctx, "k", "v")
		cancel()
		if err != nil {
			t.Errorf("replica %d, with the turn's old key held by a role that may only connect: %v", i, err)
		}
	}
}

// A write that waits too long for the writers' turn (another replica's
// write holds it, cut off) fails within turnWithin, and the replica says
// its writes wait until it takes the turn again.
func TestReplicasTurnWaitedSaysSo(t *testing.T) {
	u := replicaURL(t)
	was := turnWithin
	turnWithin = 500 * time.Millisecond
	t.Cleanup(func() { turnWithin = was })
	ctx := context.Background()
	a := openReplica(t, u)
	if err := a.SetMeta(ctx, "k", "1"); err != nil {
		t.Fatal(err)
	}
	holder, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM replica_gens WHERE one = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	began := time.Now()
	err = a.SetMeta(wctx, "k", "2")
	if !errors.Is(err, ErrTurnWaited) || time.Since(began) > 3*time.Second {
		t.Errorf("a write behind a turn held: %v after %v, want ErrTurnWaited within %v", err, time.Since(began), turnWithin)
	}
	rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
	if !a.WritesWait(rctx) {
		t.Error("the replica doesn't say its writes wait")
	}
	rcancel()
	tx.Rollback(ctx)
	rctx, rcancel = context.WithTimeout(ctx, 2*time.Second)
	defer rcancel()
	if a.WritesWait(rctx) {
		t.Error("the replica says its writes wait, the turn free again")
	}
}

// A session that waits for the ledger's lock alone holds nothing and wrote
// nothing: a new replica is refused, saying which session waits and why; a
// running replica whose connection dropped can't connect again meanwhile,
// and doesn't take the ledger for lost; once the session goes, it writes.
func TestReplicasAWaiterTakesNothing(t *testing.T) {
	u := replicaURL(t)
	quick(t)
	ctx := context.Background()
	a := openReplica(t, u)
	if err := a.SetMeta(ctx, "k", "1"); err != nil {
		t.Fatal(err)
	}
	// Its side pool's session holds the lock too, as a running replica's
	// do: ending the writer's alone doesn't hand the waiter the lock.
	if err := a.Wake(ctx); err != nil {
		t.Fatal(err)
	}
	other := connectOnly(t, u)
	wctx, wcancel := context.WithCancel(ctx)
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		other.Exec(wctx, "SELECT pg_advisory_lock($1, $2)", int32(lockClass), a.pg.key)
	}()
	admin, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	within(t, 5*time.Second, "the session waits", func() bool {
		var n int
		admin.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND mode = 'ExclusiveLock'").Scan(&n)
		return n == 1
	})
	_, err = OpenPostgres(ctx, u, time.UTC)
	var waits *LockWaitError
	if !errors.As(err, &waits) || waits.PID == 0 || !strings.Contains(err.Error(), "waits to hold the ledger's lock alone") {
		t.Errorf("a new replica, while a session waits for the lock alone: %v", err)
	}
	if _, err := admin.Exec(ctx, "SELECT pg_terminate_backend($1)", int64(a.pg.pid)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	for range 2 {
		a.SetMeta(ctx, "k", "2")
	}
	if a.pg.isLost() {
		t.Fatalf("taken for lost, though no process ever held the ledger alone: %v", a.LostWhy())
	}
	wcancel()
	<-waited
	var werr error
	for range 3 {
		if werr = a.SetMeta(ctx, "k", "3"); werr == nil {
			break
		}
	}
	if werr != nil || a.pg.isLost() {
		t.Errorf("once the session went: %v (lost %v)", werr, a.pg.isLost())
	}
}

// A notice claimed by a round that began long ago is claimed from when it
// is taken, by the database's clock: no other sender takes it at once.
func TestReplicasClaimFromAnOldRound(t *testing.T) {
	u := replicaURL(t)
	fastLease(t)
	ctx := context.Background()
	a, b := openReplica(t, u), openReplica(t, u)
	if err := a.TellOnce(ctx, "k", "1", "test.event", []Told{{Dest: "webhook", About: map[string]any{"n": 1}}}); err != nil {
		t.Fatal(err)
	}
	round := time.Now().Add(-claimFor - 30*time.Second)
	n1, ok, err := a.Claim(ctx, "webhook", round, 0)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if n2, ok, err := b.Claim(ctx, "webhook", time.Now(), 0); err != nil || ok && n2.ID == n1.ID {
		t.Errorf("a notice claimed by a round that began long ago, claimed again at once: %v %v", ok, err)
	}
}

// A notice is due by the database's clock, which every replica reads, not
// by the clock of the replica that told it.
func TestReplicasNoticesDueByTheDatabase(t *testing.T) {
	u := replicaURL(t)
	was := wallNow
	wallNow = func() time.Time { return time.Now().Add(time.Hour) } // a replica an hour ahead
	t.Cleanup(func() { wallNow = was })
	ctx := context.Background()
	a := openReplica(t, u)
	if err := a.TellOnce(ctx, "k", "1", "test.event", []Told{{Dest: "webhook", About: map[string]any{"n": 1}}}); err != nil {
		t.Fatal(err)
	}
	var late int64
	if err := a.r.QueryRowContext(ctx, "SELECT next_at - CAST(extract(epoch FROM clock_timestamp()) AS BIGINT) FROM outbox").Scan(&late); err != nil {
		t.Fatal(err)
	}
	if late > 5 {
		t.Errorf("a notice told by a replica an hour ahead is due %d s from now", late)
	}
}

// A leader's write whose lease another takes while it runs commits
// nothing: the lease is checked again as it commits. A take isn't held up
// by the write while it is open.
func TestReplicasFenceAtCommit(t *testing.T) {
	u := replicaURL(t)
	fastLease(t)
	ctx := context.Background()
	a, b := openReplica(t, u), openReplica(t, u)
	got := make(chan context.Context, 1)
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.Elect(ectx, Elected{Version: "test"}, func(lctx context.Context) {
		got <- lctx
		<-lctx.Done()
	})
	var lctx context.Context
	select {
	case lctx = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("never led")
	}
	ld := leaseOf(lctx)
	admin, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var took time.Duration
	err = a.write(context.WithValue(context.Background(), leaseKey{}, ld), func(t2 *txn) error {
		// The lease lapses in the database, and another takes it, while this
		// write is open; the leader's clock didn't move (a suspended host).
		if _, err := admin.Exec(ctx, "UPDATE replica_lease SET until = clock_timestamp() - interval '1 second'"); err != nil {
			return err
		}
		tctx, c := context.WithTimeout(ctx, 2*time.Second)
		defer c()
		began := time.Now()
		if term, _, err := b.rep.take(tctx); err != nil || term == 0 {
			return fmt.Errorf("the other's take: %d %v", term, err)
		}
		took = time.Since(began)
		ld.mu.Lock()
		ld.until = time.Now().Add(time.Hour)
		ld.mu.Unlock()
		return setMeta(t2.ctx, t2, "x.late", "the former leader's")
	})
	if !errors.Is(err, ErrNotLeading) {
		t.Errorf("a write committed after another took the lease: %v", err)
	}
	if v, _ := a.Meta(ctx, "late"); v != "" {
		t.Errorf("kept: %q", v)
	}
	if took > time.Second {
		t.Errorf("the take waited %v for the former leader's open write", took)
	}
}

// A refresh that waits on the database (here, a table it reads locked by
// another session, as a database that stopped answering holds it) holds
// nothing up past its own deadline: another caller waits no longer than
// its own context; the refresh itself gives up within refreshWithin; a
// write that commits waits for it not at all.
func TestReplicasStuckRefreshHoldsNothing(t *testing.T) {
	u := replicaURL(t)
	was := refreshWithin
	refreshWithin = 2 * time.Second
	t.Cleanup(func() { refreshWithin = was })
	ctx := context.Background()
	a := openReplica(t, u)
	if err := a.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.SetMeta(ctx, "k", "1"); err != nil {
		t.Fatal(err)
	}
	holder, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// What a refresh reads and no write does.
	if _, err := tx.Exec(ctx, "LOCK TABLE replica_signals IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	stuck := make(chan error, 1)
	began := time.Now()
	go func() { stuck <- a.Refresh(ctx) }()
	time.Sleep(200 * time.Millisecond)
	// Another caller, with 500ms.
	rctx, rcancel := context.WithTimeout(ctx, 500*time.Millisecond)
	asked := time.Now()
	a.Refresh(rctx)
	rcancel()
	if took := time.Since(asked); took > time.Second {
		t.Errorf("a refresh with 500ms took %v behind one that waits", took)
	}
	// A write commits meanwhile, at once.
	wbegan := time.Now()
	if err := a.SetMeta(ctx, "k", "2"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(wbegan); took > time.Second {
		t.Errorf("a write took %v while a refresh waited", took)
	}
	select {
	case err := <-stuck:
		if took := time.Since(began); err == nil || took > refreshWithin+time.Second {
			t.Errorf("the refresh that waits: %v after %v, want an error within %v", err, took, refreshWithin)
		}
	case <-time.After(refreshWithin + 3*time.Second):
		t.Fatalf("the refresh that waits still waits after %v", time.Since(began))
	}
	tx.Rollback(ctx)
	if err := a.Refresh(ctx); err != nil {
		t.Errorf("a refresh once the table is free: %v", err)
	}
}

// What is noted on the side pool (a notice sent or failed, a bucket, a
// claim given up) has a deadline of its own, whatever its context: here
// each waits on a row another session locked, as on a database that
// stopped answering. Two connections serve them all.
func TestReplicasSideHasDeadlines(t *testing.T) {
	u := replicaURL(t)
	was := sideWithin
	sideWithin = time.Second
	t.Cleanup(func() { sideWithin = was })
	ctx := context.Background()
	a := openReplica(t, u)
	if err := a.TellOnce(ctx, "k", "1", "test.event", []Told{{Dest: "webhook", About: map[string]any{"n": 1}}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.Take(ctx, "b", 1, 10, 1); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := a.ClaimOnce(ctx, "c", time.Hour); !ok || err != nil {
		t.Fatal(ok, err)
	}
	holder, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, q := range []string{"SELECT 1 FROM outbox FOR UPDATE", "SELECT 1 FROM replica_buckets FOR UPDATE", "SELECT 1 FROM replica_once FOR UPDATE"} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	posted := WithoutLease(context.WithoutCancel(ctx))
	for name, f := range map[string]func(context.Context) error{
		"Sent":    func(c context.Context) error { return a.Sent(c, 1) },
		"Failed":  func(c context.Context) error { return a.Failed(c, 1, "x") },
		"Take":    func(c context.Context) error { _, err := a.Take(c, "b", 1, 10, 1); return err },
		"Unclaim": func(c context.Context) error { return a.Unclaim(c, "c") },
	} {
		done := make(chan error, 1)
		began := time.Now()
		go func() { done <- f(posted) }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s, its row locked: no error", name)
			}
		case <-time.After(sideWithin + 3*time.Second):
			t.Errorf("%s, its row locked, still waits after %v", name, time.Since(began))
		}
	}
}

// A replica whose migration waits for the advisory lock that makes an
// empty schema a ledger, while another makes it, finds the ledger made once
// it has the lock: the catalog asked afresh, not what the session cached
// as it connected, and not made again.
func TestReplicasBootstrapSeesTheOthersLedger(t *testing.T) {
	u := replicaURL(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var schema string
	if err := admin.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	// As the first replica does: the lock, then the ledger made.
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)", int32(writeClass), keyOf32(schema)); err != nil {
		t.Fatal(err)
	}
	opened := make(chan error, 1)
	go func() {
		l, err := OpenPostgres(ctx, u, time.UTC)
		if err == nil {
			l.Close()
		}
		opened <- err
	}()
	within(t, 10*time.Second, "the replica waits for the lock", func() bool {
		var n int
		admin.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND classid::bigint = $1", int64(writeClass)).Scan(&n)
		return n == 1
	})
	files, ns, err := pgFiles()
	if err != nil {
		t.Fatal(err)
	}
	b, err := pgSchema.ReadFile(files[ns[0]])
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"CREATE TABLE schema_version (version BIGINT NOT NULL, writers BIGINT NOT NULL DEFAULT 0); INSERT INTO schema_version (version) VALUES (0)",
		string(b), fmt.Sprintf("UPDATE schema_version SET version = %d", ns[0])} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-opened; err != nil {
		t.Errorf("a replica that waited while another made the ledger: %v", err)
	}
}

// A wake asked while the leader is in a write of its own is not taken in
// with it: the leader is woken at its next refresh.
func TestReplicasWakeDuringALeadersWrite(t *testing.T) {
	u := replicaURL(t)
	ctx := context.Background()
	a, b := openReplica(t, u), openReplica(t, u)
	if err := a.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Woken():
	default:
	}
	inside, release, errc := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		errc <- a.write(ctx, func(t *txn) error {
			close(inside)
			<-release
			return setMeta(t.ctx, t, "x.k", "v")
		})
	}()
	<-inside
	r0 := a.Rejudges()
	if err := b.AskRejudge(ctx); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Woken():
	default:
		t.Error("asked to wake during the leader's own write, and never woken")
	}
	if a.Rejudges() != r0+1 {
		t.Errorf("asked to judge again: %d, from %d", a.Rejudges(), r0)
	}
}

// A write of an older replica that waits behind a newer lapilli's
// migration finds the newer schema once it has the turn, and writes
// nothing: the replica stops.
func TestReplicasStopAtANewerMigration(t *testing.T) {
	u := replicaURL(t)
	ctx := context.Background()
	a := openReplica(t, u)
	if err := a.SetMeta(ctx, "k", "1"); err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// As migrateStep does: schema_version locked, the turn, the schema on.
	for _, q := range []string{"LOCK TABLE schema_version IN SHARE ROW EXCLUSIVE MODE", "SELECT 1 FROM replica_gens WHERE one = 1 FOR UPDATE",
		"UPDATE schema_version SET version = version + 1"} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	errc := make(chan error, 1)
	go func() { errc <- a.SetMeta(ctx, "old", "the older replica's") }()
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; !errors.Is(err, ErrNewer) {
		t.Errorf("an older replica's write after a newer lapilli migrated the ledger: %v", err)
	}
	if v, _ := a.Meta(ctx, "old"); v != "" {
		t.Errorf("written: %q", v)
	}
	select {
	case <-a.Lost():
	default:
		t.Error("the older replica goes on")
	}
}

// A failover that lost commits, then more made at the same generations
// before a replica's next refresh: the replica takes every week for
// changed, not only those it reads as new.
func TestReplicasFailoverSeenThrough(t *testing.T) {
	u := replicaURL(t)
	ctx := context.Background()
	a, b := openReplica(t, u), openReplica(t, u)
	w1, w2 := Week(a.Day(t0)), Week(a.Day(t0))+70
	fs := func(at time.Time) []firing.Firing {
		return []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at.Add(time.Hour), End: at.Add(2 * time.Hour)}}
	}
	record(t, a, t0, t0.Add(24*time.Hour), fs(t0))
	record(t, a, t0.Add(70*24*time.Hour), t0.Add(71*24*time.Hour), fs(t0.Add(70*24*time.Hour)))
	a.SetMeta(ctx, "lost1", "1")
	a.SetMeta(ctx, "lost2", "1")
	if err := b.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	g0 := b.Gen()
	admin, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	// The failover: the last two commits gone, their log with them.
	if _, err := admin.Exec(ctx, "DELETE FROM replica_log WHERE gen > (SELECT gen - 2 FROM replica_gens); UPDATE replica_gens SET gen = gen - 2"); err != nil {
		t.Fatal(err)
	}
	record(t, a, t0, t0.Add(24*time.Hour), fs(t0.Add(3*time.Hour)))
	a.SetMeta(ctx, "k", "v")
	record(t, a, t0.Add(70*24*time.Hour), t0.Add(71*24*time.Hour), fs(t0.Add(70*24*time.Hour+3*time.Hour)))
	if err := b.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if b.Changed(w1) <= g0 || b.Changed(w2) <= g0 {
		t.Errorf("after a failover hidden by later writes: week 1 changed at %d, week 2 at %d, from %d", b.Changed(w1), b.Changed(w2), g0)
	}
}

// A replica that cached an alert's id writes beside another replica that
// deleted it: the cache is dropped under the writers' turn, before the
// write uses it, without a refresh in between.
func TestReplicasIDsDroppedUnderTheTurn(t *testing.T) {
	u := replicaURL(t)
	ctx := context.Background()
	a, b := openReplica(t, u), openReplica(t, u)
	f := func(at time.Time) []firing.Firing {
		return []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: at.Add(time.Hour), End: at.Add(2 * time.Hour)}}
	}
	record(t, b, t0, t0.Add(24*time.Hour), f(t0))
	if err := a.Prune(ctx, t0.Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := b.Record(ctx, Found{From: t0.Add(24 * time.Hour), To: t0.Add(48 * time.Hour), Firings: f(t0.Add(24 * time.Hour)),
		Run: Run{Kind: "test"}}, opts); err != nil {
		t.Fatalf("a write with an alert's id another replica deleted: %v", err)
	}
	var n int
	if err := b.r.QueryRowContext(ctx, "SELECT count(*) FROM firings f JOIN series s ON s.id = f.series_id").Scan(&n); err != nil || n != 1 {
		t.Errorf("firings on an alert that is there: %d %v", n, err)
	}
}

// A sender claims the next notice at once when another holds the first,
// rather than wait for it.
func TestReplicasClaimSkipsWhatIsHeld(t *testing.T) {
	u := replicaURL(t)
	ctx := context.Background()
	a := openReplica(t, u)
	if err := a.TellOnce(ctx, "k", "1", "test.event", []Told{{Dest: "webhook", About: map[string]any{"n": 1}}, {Dest: "webhook", About: map[string]any{"n": 2}}}); err != nil {
		t.Fatal(err)
	}
	holder, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var first int64
	if err := tx.QueryRow(ctx, "SELECT id FROM outbox ORDER BY id LIMIT 1 FOR UPDATE").Scan(&first); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	n, ok, err := a.Claim(cctx, "webhook", time.Now(), 0)
	if err != nil || !ok || n.ID == first {
		t.Errorf("a claim while another holds the first notice: %v %v %v, want the next at once", n.ID, ok, err)
	}
}

// A leader whose work is stuck, or whose writer is held past any write's
// deadline, gives its lease up, though it reaches the database: another
// leads.
func TestReplicasStuckLeaderGivesUp(t *testing.T) {
	u := replicaURL(t)
	fastLease(t)
	for _, how := range []string{"its work", "its writer"} {
		t.Run(how, func(t *testing.T) {
			a, b := openReplica(t, u), openReplica(t, u)
			var mu sync.Mutex
			stuck := false
			ls := &leads{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go a.Elect(ctx, Elected{Version: "test", Stuck: func() bool { mu.Lock(); defer mu.Unlock(); return stuck }}, ls.lead(0, a))
			within(t, 5*time.Second, "a leads", func() bool { return ls.current() == 0 })
			elect(t, b, ls, 1)
			switch how {
			case "its work":
				mu.Lock()
				stuck = true
				mu.Unlock()
			case "its writer":
				a.heldSince.Store(time.Now().Add(-writeWithin - 2*time.Minute).UnixNano())
				defer a.heldSince.Store(0)
			}
			within(t, renewEvery+3*campaignEvery+time.Second, "another leads", func() bool { return ls.current() == 1 })
			ls.apart(t)
		})
	}
}

// What is over is swept by the leader: a use once past its time, at once,
// not a day after; and the uses once kept are bounded.
func TestReplicasSweepAndBoundOnce(t *testing.T) {
	u := replicaURL(t)
	fastLease(t)
	was := sweepEvery
	sweepEvery = 200 * time.Millisecond
	t.Cleanup(func() { sweepEvery = was })
	ctx := context.Background()
	a := openReplica(t, u)
	if _, err := a.Once(ctx, "over", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := a.w.ExecContext(ctx, "UPDATE replica_once SET until = until - 5"); err != nil {
		t.Fatal(err)
	}
	elect(t, a, &leads{}, 0)
	within(t, 5*time.Second, "the use once over is swept", func() bool {
		var n int
		a.r.QueryRowContext(ctx, "SELECT count(*) FROM replica_once").Scan(&n)
		return n == 0
	})
	m := maxOnce
	maxOnce = 3
	t.Cleanup(func() { maxOnce = m })
	for i := range 3 {
		if ok, err := a.Once(ctx, fmt.Sprint("k", i), time.Hour); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	if ok, err := a.Once(ctx, "one more", time.Hour); ok || !errors.Is(err, ErrOnceFull) {
		t.Errorf("past the bound: %v %v", ok, err)
	}
}
