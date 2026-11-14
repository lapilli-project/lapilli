package ledger

// Several replicas of lapilli serve on one Postgres ledger
// (docs/design/replicas.md): the lock taken shared (postgres.go), a lease
// the replica that leads holds and renews, the writers' turn every write
// takes, and what each replica would otherwise keep alone, kept in the
// database: the generations pages are cached by, which weeks' firings
// changed, the claims on notices, the rate limits' buckets.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// The writers' turn is replica_gens's one row, locked FOR UPDATE: every
// write takes it first, on every replica, so writes take turns across them
// as they do in one process, and a write that reads what it then changes (a
// count against a limit, a value merged) reads what no other write is
// changing. A row lock needs the ledger's own privileges on the table: a
// role that may only connect can't hold it, as it could an advisory lock.
//
// writeClass is the first key of the advisory lock that makes an empty
// schema a ledger once, before schema_version is there to lock ("lapw");
// the second is the schema's, as the lock's.
const writeClass = 0x6c617077

// replicaTables are the replicas' own, made with the ledger in Postgres
// alone, as schema_version is: none of them is the ledger's, none is
// copied (tables), and SQLite has none. They are made, or changed, by a
// start that finds them older than replicasVersion (replicasMade), in
// replica_schema: one that finds them as they are makes nothing, since
// DDL waits for any session that read them (pg_dump, for its whole dump)
// and needs the tables' owner.
const replicaTables = `
CREATE TABLE IF NOT EXISTS replica_lease (
	one BIGINT PRIMARY KEY CHECK (one = 1),
	holder TEXT COLLATE "C" NOT NULL,
	term BIGINT NOT NULL,
	since TIMESTAMPTZ NOT NULL,
	until TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS replica_gens (
	one BIGINT PRIMARY KEY CHECK (one = 1),
	gen BIGINT NOT NULL DEFAULT 0,
	every_week BIGINT NOT NULL DEFAULT 0,
	recorded BIGINT NOT NULL DEFAULT 0,
	ids BIGINT NOT NULL DEFAULT 0
);
INSERT INTO replica_gens (one) VALUES (1) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS replica_signals (
	one BIGINT PRIMARY KEY CHECK (one = 1),
	wake BIGINT NOT NULL DEFAULT 0,
	rejudge BIGINT NOT NULL DEFAULT 0
);
INSERT INTO replica_signals (one) VALUES (1) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS replica_log (
	gen BIGINT PRIMARY KEY,
	nonce BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS replica_weeks (
	week BIGINT PRIMARY KEY,
	gen BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS replica_weeks_gen ON replica_weeks (gen);
CREATE TABLE IF NOT EXISTS replica_members (
	id TEXT COLLATE "C" PRIMARY KEY,
	host TEXT COLLATE "C" NOT NULL,
	version TEXT COLLATE "C" NOT NULL,
	started TIMESTAMPTZ NOT NULL,
	seen TIMESTAMPTZ NOT NULL,
	status TEXT COLLATE "C" NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS replica_buckets (
	name TEXT COLLATE "C" PRIMARY KEY,
	tokens DOUBLE PRECISION NOT NULL,
	at DOUBLE PRECISION NOT NULL
);
CREATE TABLE IF NOT EXISTS replica_once (
	key TEXT COLLATE "C" PRIMARY KEY,
	until BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS replica_once_until ON replica_once (until);
ALTER TABLE replica_members ADD COLUMN IF NOT EXISTS signs TEXT COLLATE "C" NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS replica_fence (
	holder TEXT COLLATE "C" NOT NULL,
	term BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS replica_schema (
	one BIGINT PRIMARY KEY CHECK (one = 1),
	version BIGINT NOT NULL
);
CREATE OR REPLACE FUNCTION replica_fenced() RETURNS trigger LANGUAGE plpgsql SET search_path FROM CURRENT AS $fenced$
BEGIN
	PERFORM 1 FROM replica_lease WHERE holder = NEW.holder AND term = NEW.term AND until > clock_timestamp() FOR SHARE;
	IF NOT FOUND THEN
		RAISE EXCEPTION 'this replica no longer leads: another does what is done once' USING ERRCODE = 'LP001';
	END IF;
	DELETE FROM replica_fence WHERE ctid = NEW.ctid;
	RETURN NULL;
END
$fenced$;
DO $made$ BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'replica_fenced' AND tgrelid = to_regclass('replica_fence')) THEN
		CREATE CONSTRAINT TRIGGER replica_fenced AFTER INSERT ON replica_fence DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW EXECUTE FUNCTION replica_fenced();
	END IF;
END
$made$;
`

// replicasVersion is replicaTables' version, raised with each change to
// them (TestReplicaTablesVersioned).
const replicasVersion = 1

// replicaNames are the replicas' tables, every one replicaTables makes.
var replicaNames = []string{"replica_lease", "replica_gens", "replica_signals", "replica_log", "replica_weeks",
	"replica_members", "replica_buckets", "replica_once", "replica_fence", "replica_schema"}

// replicasMade says the replicas' tables are as this lapilli makes them,
// or as a later one does: each is there, and replica_schema says
// replicasVersion or more. Read from the catalog, which a lock taken before
// (schema_version's) has the session read afresh.
func replicasMade(ctx context.Context, tx *sql.Tx) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name IN ('`+strings.Join(replicaNames, "', '")+`')`).Scan(&n); err != nil || n < len(replicaNames) {
		return false, err
	}
	var v int64
	switch err := tx.QueryRowContext(ctx, "SELECT version FROM replica_schema WHERE one = 1").Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return v >= replicasVersion, nil
}

// makeReplicas makes the replicas' tables, or changes them to
// replicasVersion, and says so in replica_schema.
func makeReplicas(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, replicaTables); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO replica_schema (one, version) VALUES (1, ?)
		ON CONFLICT (one) DO UPDATE SET version = greatest(replica_schema.version, excluded.version)`, replicasVersion)
	return err
}

// How the replicas share a ledger. Replaced in tests.
var (
	// leaseFor is how long a lease lasts once taken or renewed, by the
	// database's clock: a leader that dies or is cut off is followed
	// within leaseFor and a campaign (campaignEvery).
	leaseFor = 15 * time.Second
	// renewEvery is how often the leader renews it; one that fails is
	// tried again each retryEvery, until the lease would end.
	renewEvery = 5 * time.Second
	retryEvery = time.Second
	// leaseMargin is how much sooner than the lease's end, by its own
	// clock, the leader stops leading: it counts from before it asked, so
	// it stops before the database lets another take the lease, whatever
	// the round trip took.
	leaseMargin = 2 * time.Second
	// campaignEvery is how often a follower asks for the lease.
	campaignEvery = 2 * time.Second
	// refreshEvery is how often a replica reads what the others wrote, as
	// well as before each request it answers (Refresh).
	refreshEvery = time.Second
	// beatEvery is how often a replica says it lives, and the leader what
	// it is doing; liveFor, how long one is taken to live after.
	beatEvery = 2 * time.Second
	liveFor   = 10 * time.Second
	// claimFor is how long a notice claimed is its sender's alone.
	claimFor = 2 * time.Minute
	// turnWithin is how long a replica's write, and every statement in it,
	// waits for a lock (lock_timeout): the writers' turn above all, which no
	// write of the ledger's own holds for more than a second or so. One
	// that waits longer fails, and the replica says it isn't ready.
	turnWithin = 30 * time.Second
	// sweepEvery is how often the leader sweeps what is over: replicas gone
	// a day, claims and sign-ins' states past their time.
	sweepEvery = time.Minute
)

// sideWithin is how long a statement on the side pool (a claim, a notice
// noted, a bucket, a use once, a wake) waits on the database at most,
// whoever asked: two connections serve them all.
var sideWithin = 30 * time.Second

// side bounds a statement of the side pool's.
func side(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, sideWithin)
}

// ErrTurnWaited is a write that waited longer than turnWithin for the
// writers' turn, or a lock in it: a write of another replica's holds it,
// cut off in the middle, until the server ends its session.
var ErrTurnWaited = errors.New("a write waited too long for the ledger's writers' turn, which another replica's write holds")

// ReplicaTimings are how the replicas share a ledger: how long a lease
// lasts, how often it is renewed and tried again, how much sooner than its
// end the leader stops, how often a follower asks for it, reads what the
// others wrote and says it lives, how long one is taken to live after, how
// long a notice claimed is its sender's, how long a write waits for a lock
// (zero: as it is), and how often what is over is swept (zero: as it is).
type ReplicaTimings struct {
	Lease, Renew, Retry, Margin, Campaign, Refresh, Beat, Live, Claim, Turn, Sweep time.Duration
}

// SetReplicaTimings sets them, for tests, giving what they were. Not while
// a replica runs: a ledger opened after takes them.
func SetReplicaTimings(t ReplicaTimings) ReplicaTimings {
	was := ReplicaTimings{leaseFor, renewEvery, retryEvery, leaseMargin, campaignEvery, refreshEvery, beatEvery, liveFor, claimFor, turnWithin, sweepEvery}
	leaseFor, renewEvery, retryEvery, leaseMargin = t.Lease, t.Renew, t.Retry, t.Margin
	campaignEvery, refreshEvery, beatEvery, liveFor, claimFor = t.Campaign, t.Refresh, t.Beat, t.Live, t.Claim
	if t.Turn > 0 {
		turnWithin = t.Turn
	}
	if t.Sweep > 0 {
		sweepEvery = t.Sweep
	}
	return was
}

// ErrNotLeading is what the replica that led does once another may lead:
// nothing a leader does is done.
var ErrNotLeading = errors.New("this replica no longer leads: another does what is done once")

// replicas is what a replica of serve keeps of the others.
type replicas struct {
	// lease is the leader's own pool, for the lease alone, so that no write
	// holds a renewal up; side, the rest that takes no writers' turn:
	// claims, buckets, heartbeats. Each session takes the lock shared, as
	// the writer's does.
	lease, side *sql.DB
	// id names this replica among the others: its host and a random part.
	id, host string
	started  time.Time
	key      func() int32
	pw       *pgWriter

	mu      sync.Mutex
	version string
	status  func() []byte
	// signs is a fingerprint of the key this replica signs sessions with,
	// "" for none (Join).
	signs string

	// seen is replica_gens as last read or written, and nonce, the nonce
	// logged with its generation; said, replica_signals; both under
	// applyMu, held for memory alone. flight is the refresh under way, under
	// flightMu. idsAt is the generation the caches of ids hold to.
	applyMu  sync.Mutex
	seen     gens
	nonce    int64
	said     signals
	flightMu sync.Mutex
	flight   *flight
	// stuck is set while the last write waited longer than turnWithin for
	// the writers' turn, until one takes it.
	stuck atomic.Bool
	idsAt atomic.Int64
	// woken is told when another replica asks the leader to look at its
	// jobs now.
	woken chan struct{}

	lead   atomic.Pointer[leadership]
	leader atomic.Pointer[Leader]
	live   atomic.Int64
	// swept is when the leader last swept what is long gone.
	swept time.Time
}

// gens is replica_gens: the generation of the ledger, the last in which
// every week's firings may have changed, how often recorded rules may
// have, and the last in which ids were deleted or renamed.
type gens struct{ gen, every, recorded, ids int64 }

// signals is replica_signals: how often the leader was asked to wake, and
// to judge again. A row of their own, which a write's turn doesn't hold.
type signals struct{ wake, rejudge int64 }

// newReplicas readies what a replica keeps of the others, with pools of
// its own to the ledger's database.
func newReplicas(cfg pgx.ConnConfig, secrets []string, pw *pgWriter) (*replicas, error) {
	pool := func(n int) *sql.DB {
		c := stdlib.GetConnector(cfg, stdlib.OptionAfterConnect(pw.joined), stdlib.OptionBeforeConnect(pw.connecting))
		db := sql.OpenDB(pgConnector{c, secrets, cfg.ConnectTimeout + connectWithin})
		db.SetMaxOpenConns(n)
		db.SetMaxIdleConns(n)
		return db
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	if len(host) > 100 {
		host = host[:100]
	}
	if host == "" {
		host = "lapilli"
	}
	r := &replicas{lease: pool(1), side: pool(2), host: host, id: host + "-" + hex.EncodeToString(b), started: time.Now(), pw: pw,
		woken: make(chan struct{}, 1), key: func() int32 {
			pw.mu.Lock()
			defer pw.mu.Unlock()
			return pw.key
		}}
	return r, nil
}

// close says this replica is gone, and lets the lock go on each of its
// own sessions before they close, so that a process alone (a copy) waits
// for no server to notice.
func (r *replicas) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.side.ExecContext(ctx, "DELETE FROM replica_members WHERE id = ?", r.id)
	for _, db := range []*sql.DB{r.lease, r.side} {
		var conns []*sql.Conn
		for range db.Stats().OpenConnections {
			c, err := db.Conn(ctx)
			if err != nil {
				break
			}
			conns = append(conns, c)
			c.ExecContext(ctx, "SELECT pg_advisory_unlock_all()")
		}
		for _, c := range conns {
			c.Close()
		}
		db.Close()
	}
}

// Shared says the ledger is shared by serve's replicas: in Postgres, not
// opened alone.
func (l *Ledger) Shared() bool { return l.rep != nil }

// turn takes the writers' turn for a write's transaction, across the
// replicas, and reads the generations as they stand under it: what no
// other write changes until this one is done. One that waits longer than
// turnWithin gives up (ErrTurnWaited), and says so for readiness.
func (r *replicas) turn(ctx context.Context, tx *sql.Tx) (gens, error) {
	var g gens
	err := tx.QueryRowContext(ctx, "SELECT gen, every_week, recorded, ids FROM replica_gens WHERE one = 1 FOR UPDATE").
		Scan(&g.gen, &g.every, &g.recorded, &g.ids)
	switch {
	case lockWaited(err):
		r.stuck.Store(true)
		return g, fmt.Errorf("%w (%v)", ErrTurnWaited, turnWithin)
	case err != nil:
		return g, err
	}
	r.stuck.Store(false)
	// Read after the turn, in a statement of its own, which sees what a
	// migration that held it committed: a newer lapilli's schema is not
	// this one's to write.
	var version int
	if err := tx.QueryRowContext(ctx, "SELECT version FROM schema_version").Scan(&version); err != nil {
		return g, err
	}
	if want, err := sqliteFiles(); err == nil && version > want {
		r.pw.loseFor(fmt.Errorf("%w (schema %d; this one knows %d): stopping", ErrNewer, version, want))
		return g, ErrNewer
	}
	return g, nil
}

// lockWaited says a statement gave up waiting for a lock (lock_timeout).
func lockWaited(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "55P03"
}

// WritesWaited says this replica's last write waited longer than it may
// for the writers' turn (ErrTurnWaited), and none has taken it since:
// another session holds it (a replica cut off in a write, until the server
// ends its session; a migration). The turn is every replica's, so it is
// said (metrics, the status) rather than taken out of readiness, which
// would take every replica out at once: reads are answered meanwhile.
func (l *Ledger) WritesWaited() bool {
	return l.rep != nil && l.rep.stuck.Load()
}

// bump counts a write in the generations, in its transaction, for the
// other replicas to read: the weeks whose firings it changed, or all of
// them, recorded rules, ids deleted or renamed. It gives the write's
// generation, and the nonce logged with it (replica_log): a replica that
// saw a generation whose nonce is no longer the ledger's (a failover lost
// the commit, and another took its number) takes everything for changed.
func (r *replicas) bump(ctx context.Context, t *txn) (gens, int64, error) {
	var g gens
	nonce := mrand.Int64()
	err := t.QueryRowContext(ctx, `WITH g AS (UPDATE replica_gens SET gen = gen + 1,
				every_week = CASE WHEN ? = 1 THEN gen + 1 ELSE every_week END,
				recorded = recorded + ?,
				ids = CASE WHEN ? = 1 THEN gen + 1 ELSE ids END
			WHERE one = 1 RETURNING gen, every_week, recorded, ids),
		n AS (INSERT INTO replica_log (gen, nonce) SELECT gen, CAST(? AS BIGINT) FROM g ON CONFLICT (gen) DO UPDATE SET nonce = excluded.nonce)
		SELECT gen, every_week, recorded, ids FROM g`, t.all, t.recorded, t.idsChanged, nonce).
		Scan(&g.gen, &g.every, &g.recorded, &g.ids)
	if err != nil || len(t.weeks) == 0 || t.all {
		return g, nonce, err
	}
	weeks := slices.Compact(slices.Sorted(slices.Values(t.weeks)))
	b, err := json.Marshal(weeks)
	if err != nil {
		return g, nonce, err
	}
	_, err = t.ExecContext(ctx, `INSERT INTO replica_weeks (week, gen) SELECT CAST(value AS BIGINT), ? FROM jsonb_array_elements_text(CAST(? AS jsonb))
		ON CONFLICT (week) DO UPDATE SET gen = excluded.gen`, g.gen, string(b))
	return g, nonce, err
}

// counts says whether a write is counted in the generations: one a page
// shows, or one that changed what a replica keeps beside the ledger.
func (t *txn) counts(counted bool) bool {
	return counted || t.count || len(t.weeks) > 0 || t.all || t.recorded || t.idsChanged
}

// wrote is a write committed at generation after, from before, its bump's
// nonce: what was seen moves on with it when nothing else was written in
// between (what was is read by the next Refresh). Memory alone: it is
// called under the writer's mu, and waits on no read of the database.
func (r *replicas) wrote(before, after gens, nonce int64) {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	if r.seen == before {
		r.seen, r.nonce = after, nonce
	}
}

// refreshWithin is how long a refresh waits on the database at most,
// whoever asked for it.
var refreshWithin = 5 * time.Second

// flight is a refresh under way: when it began, and, once done, how it
// went.
type flight struct {
	began time.Time
	done  chan struct{}
	err   error
}

// Refresh reads what the other replicas wrote since this one last did,
// before it answers a request with what it keeps: the generation pages are
// cached by moves on, the weeks whose firings changed are taken for
// changed, and the ids cached are read again if any was deleted or
// renamed. One refresh is under way at a time, each within refreshWithin;
// one that began after this was asked is as good, and is waited for, as
// long as ctx lets. Nothing to do but in a ledger serve's replicas share.
func (l *Ledger) Refresh(ctx context.Context) error {
	r := l.rep
	if r == nil {
		return nil
	}
	asked := time.Now()
	for {
		r.flightMu.Lock()
		f := r.flight
		switch {
		case f != nil && !f.began.Before(asked):
			// Begun since this was asked: its answer is this one's.
		case f != nil && !f.over():
			// Begun before: what it reads may miss a write this asks for.
			// Waited for, then another.
			r.flightMu.Unlock()
			select {
			case <-f.done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		default:
			f = &flight{began: time.Now(), done: make(chan struct{})}
			r.flight = f
			go l.fly(f)
		}
		r.flightMu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (f *flight) over() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// fly is a refresh: what the ledger says, read within refreshWithin, then
// taken in, in memory alone.
func (l *Ledger) fly(f *flight) {
	defer close(f.done)
	r := l.rep
	ctx, cancel := context.WithTimeout(context.Background(), refreshWithin)
	defer cancel()
	r.applyMu.Lock()
	was, wasNonce := r.seen, r.nonce
	r.applyMu.Unlock()
	var g gens
	var sg signals
	var version int
	var nonce sql.NullInt64
	if f.err = l.r.QueryRowContext(ctx, `SELECT g.gen, g.every_week, g.recorded, g.ids, s.wake, s.rejudge, (SELECT version FROM schema_version),
			(SELECT nonce FROM replica_log WHERE gen = ?)
		FROM replica_gens g, replica_signals s`, was.gen).
		Scan(&g.gen, &g.every, &g.recorded, &g.ids, &sg.wake, &sg.rejudge, &version, &nonce); f.err != nil {
		return
	}
	if want, err := sqliteFiles(); err == nil && version > want {
		// A newer lapilli migrated the ledger under this one.
		l.pg.loseFor(fmt.Errorf("%w (schema %d; this one knows %d): stopping", ErrNewer, version, want))
		f.err = ErrNewer
		return
	}
	// The generation this replica saw is no longer the ledger's: a
	// failover lost commits, and others may have been made since at the
	// same generations. Every week is taken for changed.
	diverged := was.gen > 0 && (g.gen < was.gen || !nonce.Valid || nonce.Int64 != wasNonce)
	var weeks []int64
	if g.gen != was.gen && !diverged && g.every <= was.gen {
		rows, err := l.r.QueryContext(ctx, "SELECT week FROM replica_weeks WHERE gen > ?", was.gen)
		if err != nil {
			f.err = err
			return
		}
		for rows.Next() {
			var w int64
			if err := rows.Scan(&w); err != nil {
				rows.Close()
				f.err = err
				return
			}
			weeks = append(weeks, w)
		}
		rows.Close()
		if f.err = rows.Err(); f.err != nil {
			return
		}
	}
	var gNonce sql.NullInt64
	if g.gen != was.gen || diverged {
		// The nonce now at the generation read, for the next refresh.
		if f.err = l.r.QueryRowContext(ctx, "SELECT nonce FROM replica_log WHERE gen = ?", g.gen).Scan(&gNonce); f.err != nil && !errors.Is(f.err, sql.ErrNoRows) {
			return
		}
		f.err = nil
	}
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	if g.gen != was.gen || diverged {
		all := diverged || g.every > was.gen
		if g.ids != r.idsAt.Load() || diverged {
			l.forgetIDs()
			r.idsAt.Store(g.ids)
		}
		if g.recorded != was.recorded || diverged {
			l.recorded.Add(1)
		}
		// Taken for changed before the generation moves, as a write does:
		// a page read at the new one stands.
		next := l.gen.Load() + 1
		l.changed.Lock()
		if l.changed.by == nil {
			l.changed.by = map[int64]int64{}
		}
		for _, w := range weeks {
			l.changed.by[w] = next
		}
		if all {
			l.changed.all = next
		}
		l.changed.Unlock()
		l.gen.Add(1)
		// A write of this replica's own, since the read, keeps what it saw.
		if diverged || g.gen > r.seen.gen || r.seen == was {
			r.seen, r.nonce = g, gNonce.Int64
		}
	}
	if sg.wake != r.said.wake {
		select {
		case r.woken <- struct{}{}:
		default:
		}
	}
	r.said = sg
}

// forgetIDs drops the rules' and alerts' ids cached: another replica
// deleted or renamed some. Labels are kept by their text, which they are
// checked against.
func (l *Ledger) forgetIDs() {
	l.ids.Lock()
	clear(l.ids.rules)
	clear(l.ids.series)
	l.ids.rulesBytes, l.ids.seriesBytes = 0, 0
	l.ids.Unlock()
}

// leadership is a lease this replica holds: its term, and until when, by
// this process's own clock, it leads.
type leadership struct {
	r     *replicas
	term  int64
	since time.Time
	mu    sync.Mutex
	until time.Time
}

func (ld *leadership) valid() bool {
	ld.mu.Lock()
	defer ld.mu.Unlock()
	return time.Now().Before(ld.until)
}

func (ld *leadership) left() time.Duration {
	ld.mu.Lock()
	defer ld.mu.Unlock()
	return time.Until(ld.until)
}

func (ld *leadership) extend(to time.Time) {
	ld.mu.Lock()
	defer ld.mu.Unlock()
	if !ld.until.IsZero() && to.After(ld.until) {
		ld.until = to
	}
}

func (ld *leadership) end() {
	ld.mu.Lock()
	defer ld.mu.Unlock()
	ld.until = time.Time{}
}

type leaseKey struct{}

func leaseOf(ctx context.Context) *leadership {
	ld, _ := ctx.Value(leaseKey{}).(*leadership)
	return ld
}

// Lapsed is ctx with the lease it carries as a process frozen past it
// holds it: over by its clock, though nothing cancelled ctx, and the
// database may not have moved. For tests.
func Lapsed(ctx context.Context) context.Context {
	ld := leaseOf(ctx)
	if ld == nil {
		return ctx
	}
	return context.WithValue(ctx, leaseKey{}, &leadership{r: ld.r, term: ld.term, since: ld.since})
}

// WithoutLease is ctx without the lease it carries: what is written with it
// is written whether or not this replica still leads, as noting that what
// it sent was sent.
func WithoutLease(ctx context.Context) context.Context {
	return context.WithValue(ctx, leaseKey{}, (*leadership)(nil))
}

// Leads says whether what is done with ctx may be done as the leader's:
// a context a lease gave (Elect) while this replica still holds it, by its
// own clock; any other.
func Leads(ctx context.Context) bool {
	ld := leaseOf(ctx)
	return ld == nil || ld.valid()
}

// Leading says whether this process leads: a SQLite ledger's, or a
// Postgres one's opened alone, always.
func (l *Ledger) Leading() bool {
	if l.rep == nil {
		return true
	}
	ld := l.rep.lead.Load()
	return ld != nil && ld.valid()
}

// fenced checks, in a write made with a lease's context, that the lease is
// still this replica's in the database: a leader cut off, or stopped for
// longer than its lease, whose lease another took, writes nothing. As it
// begins, and again as it commits (arm).
func (r *replicas) fenced(t *txn, ld *leadership) error {
	if !ld.valid() {
		return ErrNotLeading
	}
	var n int
	if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM replica_lease WHERE holder = ? AND term = ? AND until > clock_timestamp()",
		r.id, ld.term).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotLeading
	}
	return nil
}

// arm has the lease checked again as the write commits: a row that a
// deferred trigger (replica_fenced) checks the lease for, holding it FOR
// SHARE for the commit alone, so that another replica's take, which waits
// for no write while it is open, waits for this commit, and one that came
// first refuses it. A leader whose host was suspended past its lease, or
// whose commit was held up, commits nothing after another's term began.
func (r *replicas) arm(t *txn, ld *leadership) error {
	_, err := t.ExecContext(t.ctx, "INSERT INTO replica_fence (holder, term) VALUES (?, ?)", r.id, ld.term)
	return err
}

// notLeading says a commit was refused by the lease's check (arm).
func notLeading(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "LP001"
}

// Leader is the replica that leads, as last read.
type Leader struct {
	ID, Host, Version string
	Term              int64
	Since             time.Time
	// Status is what it says of itself (Elected.Status).
	Status []byte
	// Self says it is this replica.
	Self bool
}

// Leader is the replica that leads, as last read, every beatEvery: false
// when none does, or none was read yet. A SQLite ledger's process, or a
// Postgres one's alone, is its own.
func (l *Ledger) Leader() (Leader, bool) {
	if l.rep == nil {
		return Leader{Self: true}, true
	}
	if ld := l.rep.lead.Load(); ld != nil && ld.valid() {
		l.rep.mu.Lock()
		v := l.rep.version
		l.rep.mu.Unlock()
		return Leader{ID: l.rep.id, Host: l.rep.host, Version: v, Term: ld.term, Since: ld.since, Self: true}, true
	}
	p := l.rep.leader.Load()
	if p == nil {
		return Leader{}, false
	}
	return *p, true
}

// Replicas is how many replicas live, this one with them, as last counted
// every beatEvery: 1 for a ledger no replicas share.
func (l *Ledger) Replicas() int {
	if l.rep == nil {
		return 1
	}
	return max(1, int(l.rep.live.Load()))
}

// Join says this replica lives, signing sessions with a key whose
// fingerprint is signs ("" for none), before it serves, and gives how many
// other live replicas sign them with another: a session one signs is none
// to the other. It writes its own first, then reads the others', so that of
// two started at once one at least sees the other.
func (l *Ledger) Join(ctx context.Context, signs string) (int, error) {
	ctx, cancel := side(ctx)
	defer cancel()
	if l.rep == nil {
		return 0, nil
	}
	r := l.rep
	r.mu.Lock()
	r.signs = signs
	version := r.version
	r.mu.Unlock()
	if _, err := r.side.ExecContext(ctx, `INSERT INTO replica_members (id, host, version, started, seen, signs) VALUES (?, ?, ?, ?, clock_timestamp(), ?)
		ON CONFLICT (id) DO UPDATE SET seen = clock_timestamp(), signs = excluded.signs`, r.id, r.host, version, r.started.UTC(), signs); err != nil {
		return 0, err
	}
	if signs == "" {
		return 0, nil
	}
	var n int
	err := r.side.QueryRowContext(ctx, `SELECT count(*) FROM replica_members
		WHERE id != ? AND seen > clock_timestamp() - make_interval(secs => ?) AND signs != '' AND signs != ?`,
		r.id, liveFor.Seconds(), signs).Scan(&n)
	return n, err
}

// ReplicaID names this replica among the others; "" for a ledger no
// replicas share.
func (l *Ledger) ReplicaID() string {
	if l.rep == nil {
		return ""
	}
	return l.rep.id
}

// Elected is what Elect is told of this replica.
type Elected struct {
	Version string
	// Status is what the replica says of itself while it leads, for the
	// others' pages: a JSON text, read each beatEvery.
	Status func() []byte
	// Stuck says the leader's work has made no progress for longer than it
	// may: then the lease isn't renewed, and is given up, for another.
	Stuck func() bool
}

// wedged says a write has held the writer for longer than any may (each
// is cut off at writeWithin): the replica can't write as the leader must.
func (l *Ledger) wedged() bool {
	h := l.heldSince.Load()
	return h != 0 && time.Since(time.Unix(0, h)) > writeWithin+time.Minute
}

// Elect takes part in choosing the replica that leads, until ctx is done.
// Each time this one is chosen, lead runs with a context that ends once it
// no longer leads (its lease not renewed in time, or ctx done) and carries
// the lease: a write made with it is made only while this replica still
// leads (fenced), and Hold says whether it does. Elect returns once ctx is
// done, lead has returned, and the lease is given up, for a follower to
// take at once. Meanwhile it reads what the other replicas wrote, every
// refreshEvery, and says that this one lives, every beatEvery. A SQLite
// ledger's process, or a Postgres one's alone, leads: lead runs once, with
// ctx.
func (l *Ledger) Elect(ctx context.Context, e Elected, lead func(context.Context)) {
	r := l.rep
	if r == nil {
		lead(ctx)
		return
	}
	r.mu.Lock()
	r.version, r.status = e.Version, e.Status
	r.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		every(ctx, refreshEvery, func() { l.Refresh(ctx) })
	}()
	go func() {
		defer wg.Done()
		every(ctx, beatEvery, func() { r.beat(ctx) })
	}()
	for ctx.Err() == nil {
		if (e.Stuck != nil && e.Stuck()) || l.wedged() {
			// A replica whose work, or writer, is stuck asks for no lease:
			// it would give it up again.
			select {
			case <-ctx.Done():
			case <-time.After(campaignEvery):
			}
			continue
		}
		tctx, cancel := context.WithTimeout(ctx, max(campaignEvery, time.Second))
		term, sent, err := r.take(tctx)
		cancel()
		if err == nil && term > 0 {
			l.leadFor(ctx, e, term, sent, lead)
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(campaignEvery):
		}
	}
	wg.Wait()
}

// every runs f now, and each d after, until ctx is done.
func every(ctx context.Context, d time.Duration, f func()) {
	for {
		f()
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}
}

// take asks for the lease: this replica's if none holds it, or the one
// that did let it lapse, with a term one more than the last. It gives the
// term, 0 when another holds it, and when it was asked.
func (r *replicas) take(ctx context.Context) (int64, time.Time, error) {
	sent := time.Now()
	var term int64
	err := r.lease.QueryRowContext(ctx, `INSERT INTO replica_lease (one, holder, term, since, until)
			VALUES (1, ?, 1, clock_timestamp(), clock_timestamp() + make_interval(secs => ?))
		ON CONFLICT (one) DO UPDATE SET holder = excluded.holder, term = replica_lease.term + 1, since = excluded.since, until = excluded.until
			WHERE replica_lease.until < clock_timestamp()
		RETURNING term`, r.id, leaseFor.Seconds()).Scan(&term)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, sent, nil
	}
	return term, sent, err
}

// renew renews the lease of a term this replica holds, before it lapses:
// false when it lapsed, or another holds it.
func (r *replicas) renew(ctx context.Context, term int64) (bool, error) {
	var n int64
	err := r.lease.QueryRowContext(ctx, `UPDATE replica_lease SET until = clock_timestamp() + make_interval(secs => ?)
		WHERE holder = ? AND term = ? AND until > clock_timestamp() RETURNING term`, leaseFor.Seconds(), r.id, term).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// resign gives the lease of a term up, for a follower to take at its next
// campaign.
func (r *replicas) resign(term int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.lease.ExecContext(ctx, "UPDATE replica_lease SET until = clock_timestamp() - interval '1 second' WHERE holder = ? AND term = ?", r.id, term)
}

// leadFor leads with a lease of a term taken when sent: lead runs with a
// context carrying it, renewed every renewEvery, and ended, at once, by
// this process's own clock, leaseMargin before the database could let
// another take it, when it can't be. Once lead has returned, the lease is
// given up.
func (l *Ledger) leadFor(ctx context.Context, e Elected, term int64, sent time.Time, lead func(context.Context)) {
	r := l.rep
	ld := &leadership{r: r, term: term, since: time.Now(), until: sent.Add(leaseFor - leaseMargin)}
	lctx, cancel := context.WithCancelCause(context.WithValue(ctx, leaseKey{}, ld))
	r.lead.Store(ld)
	done := make(chan struct{})
	go func() {
		defer close(done)
		lead(lctx)
	}()
	ends := time.NewTimer(ld.left())
	defer ends.Stop()
	next := time.NewTimer(renewEvery)
	defer next.Stop()
	// Each renewal is asked apart, so that one waiting on the database
	// holds up nothing here: the lease ends on time, by this clock.
	type renewal struct {
		asked time.Time
		ok    bool
		err   error
	}
	renewed := make(chan renewal, 1)
	asking, returned := false, false
	for running := true; running; {
		select {
		case <-ctx.Done():
			running = false
		case <-done:
			returned, running = true, false
		case <-ends.C:
			// Not renewed in time: another may take the lease soon.
			running = false
		case <-next.C:
			if (e.Stuck != nil && e.Stuck()) || l.wedged() {
				// Reaching the database isn't leading: a leader whose work is
				// stuck gives the lease up, for one whose isn't.
				running = false
				continue
			}
			if asking {
				next.Reset(retryEvery)
				continue
			}
			asking = true
			go func() {
				asked := time.Now()
				rctx, rcancel := context.WithTimeout(ctx, min(3*time.Second, max(ld.left(), time.Millisecond)))
				defer rcancel()
				ok, err := r.renew(rctx, term)
				renewed <- renewal{asked, ok, err}
			}()
		case rn := <-renewed:
			asking = false
			switch {
			case rn.err == nil && !rn.ok:
				running = false // lapsed, or another's
			case rn.err == nil:
				ld.extend(rn.asked.Add(leaseFor - leaseMargin))
				ends.Reset(ld.left())
				next.Reset(renewEvery)
			default:
				next.Reset(retryEvery)
			}
		}
	}
	ld.end()
	r.lead.CompareAndSwap(ld, nil)
	cancel(ErrNotLeading)
	if !returned {
		<-done
	}
	if asking {
		<-renewed // within its timeout
	}
	// Given up, whatever ended it: a renewal answered after this replica
	// stopped would keep the lease from the others for its length. A
	// lease another holds, or that lapsed, is left as it is.
	r.resign(term)
}

// beat says this replica lives, with what the leader says of itself, and
// reads how many live and which leads.
func (r *replicas) beat(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, beatEvery)
	defer cancel()
	r.mu.Lock()
	version, status := r.version, r.status
	r.mu.Unlock()
	said := ""
	ld := r.lead.Load()
	if ld != nil && ld.valid() && status != nil {
		said = string(status())
	}
	r.mu.Lock()
	signs := r.signs
	r.mu.Unlock()
	if _, err := r.side.ExecContext(ctx, `INSERT INTO replica_members (id, host, version, started, seen, status, signs) VALUES (?, ?, ?, ?, clock_timestamp(), ?, ?)
		ON CONFLICT (id) DO UPDATE SET seen = clock_timestamp(), version = excluded.version, status = excluded.status, signs = excluded.signs`,
		r.id, r.host, version, r.started.UTC(), said, signs); err != nil {
		return
	}
	var n int64
	if err := r.side.QueryRowContext(ctx, "SELECT count(*) FROM replica_members WHERE seen > clock_timestamp() - make_interval(secs => ?)",
		liveFor.Seconds()).Scan(&n); err == nil {
		r.live.Store(n)
	}
	var p Leader
	var st string
	err := r.side.QueryRowContext(ctx, `SELECT l.holder, l.term, l.since, coalesce(m.host, ''), coalesce(m.version, ''), coalesce(m.status, '')
		FROM replica_lease l LEFT JOIN replica_members m ON m.id = l.holder WHERE l.until > clock_timestamp()`).
		Scan(&p.ID, &p.Term, &p.Since, &p.Host, &p.Version, &st)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		r.leader.Store(nil)
	case err == nil:
		p.Status, p.Self = []byte(st), p.ID == r.id
		r.leader.Store(&p)
	}
	if ld != nil && ld.valid() && time.Since(r.swept) >= sweepEvery {
		// The leader sweeps what is over, each sweepEvery: replicas gone a
		// day, claims and sign-ins' states past their time.
		r.swept = time.Now()
		r.side.ExecContext(ctx, "DELETE FROM replica_members WHERE seen < clock_timestamp() - interval '1 day'")
		r.side.ExecContext(ctx, "DELETE FROM replica_once WHERE until < "+dbNow)
		// The nonces of all but the last generations: a replica that saw one
		// older reads everything again.
		r.side.ExecContext(ctx, "DELETE FROM replica_log WHERE gen < (SELECT gen FROM replica_gens) - 100000")
	}
}

// Woken is told when another replica asked the leader to look at its jobs
// now (Wake): nil for a ledger no replicas share.
func (l *Ledger) Woken() <-chan struct{} {
	if l.rep == nil {
		return nil
	}
	return l.rep.woken
}

// Wake asks the replica that leads to look at its jobs now, as a change
// made on any replica may ask. Nothing for a ledger no replicas share: its
// process is the leader.
func (l *Ledger) Wake(ctx context.Context) error {
	ctx, cancel := side(ctx)
	defer cancel()
	if l.rep == nil {
		return nil
	}
	_, err := l.rep.side.ExecContext(ctx, "UPDATE replica_signals SET wake = wake + 1 WHERE one = 1")
	return err
}

// AskRejudge asks the replica that leads to judge every firing again, as
// settings changed on any replica ask, and wakes it.
func (l *Ledger) AskRejudge(ctx context.Context) error {
	ctx, cancel := side(ctx)
	defer cancel()
	if l.rep == nil {
		return nil
	}
	_, err := l.rep.side.ExecContext(ctx, "UPDATE replica_signals SET rejudge = rejudge + 1, wake = wake + 1 WHERE one = 1")
	return err
}

// Rejudges is how often the leader was asked to judge again, as last read
// (Refresh): 0 for a ledger no replicas share.
func (l *Ledger) Rejudges() int64 {
	if l.rep == nil {
		return 0
	}
	l.rep.applyMu.Lock()
	defer l.rep.applyMu.Unlock()
	return l.rep.said.rejudge
}

// Claim takes the oldest notice due to dest at now after the id after that
// no one else has, for this sender alone, for claimFor: a round that
// claims after the last it claimed tries each notice once, as Due gives
// them. another sender, the leader that
// follows this one too, takes it only once that is past, unless it is
// noted sent or failed first. Each sender claims a notice at a time
// (FOR UPDATE SKIP LOCKED), so two never take one. Made with a lease's
// context, it is taken only while this replica still leads, in the
// database. For a ledger no replicas share, Due is what a process reads.
func (l *Ledger) Claim(ctx context.Context, dest string, now time.Time, after int64) (Notice, bool, error) {
	ctx, cancel := side(ctx)
	defer cancel()
	var n Notice
	if l.rep == nil {
		return n, false, errors.New("only a ledger serve's replicas share claims notices")
	}
	// From when it is taken, by the later of now and the database's
	// clock, which every replica reads: a claim given an old now (a round's
	// start) still lasts claimFor, and the claim and what frees it are read
	// by one clock.
	args := []any{now.Unix(), int64(claimFor.Seconds()), dest, after}
	fence := ""
	if ld := leaseOf(ctx); ld != nil && ld.r == l.rep {
		if !ld.valid() {
			return n, false, ErrNotLeading
		}
		fence = " AND EXISTS (SELECT 1 FROM replica_lease WHERE holder = ?5 AND term = ?6 AND until > clock_timestamp() FOR SHARE)"
		args = append(args, l.rep.id, ld.term)
	}
	var body string
	var created int64
	var sent sql.NullInt64
	err := l.rep.side.QueryRowContext(ctx, `UPDATE outbox SET next_at = greatest(CAST(?1 AS BIGINT), `+dbNow+`) + ?2 WHERE id = (SELECT id FROM outbox
			WHERE dest = ?3 AND id > ?4 AND sent_at IS NULL AND next_at <= greatest(CAST(?1 AS BIGINT), `+dbNow+`)
			ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)`+fence+`
		RETURNING id, dest, event, body, created, attempts, sent_at, error`, args...).
		Scan(&n.ID, &n.Dest, &n.Event, &body, &created, &n.Attempts, &sent, &n.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return n, false, l.rep.stillLeads(ctx, leaseOf(ctx))
	}
	if err != nil {
		return n, false, err
	}
	n.Body, n.Created, n.SentAt = json.RawMessage(body), unix(created), fromNull(sent)
	return n, true, nil
}

// ClaimFor is how long a notice claimed is its sender's alone.
func ClaimFor() time.Duration { return claimFor }

// ClaimOnce takes what key names for this replica alone for d, by the
// database's clock: false while another holds it. Made with a lease's
// context, it is taken only while this replica still leads. For a ledger
// no replicas share, always true: its process is the only one.
func (l *Ledger) ClaimOnce(ctx context.Context, key string, d time.Duration) (bool, error) {
	ctx, cancel := side(ctx)
	defer cancel()
	if l.rep == nil {
		return true, nil
	}
	args := []any{"claim:" + key, d.Seconds()}
	fence := ""
	if ld := leaseOf(ctx); ld != nil && ld.r == l.rep {
		if !ld.valid() {
			return false, ErrNotLeading
		}
		fence = " AND EXISTS (SELECT 1 FROM replica_lease WHERE holder = ?3 AND term = ?4 AND until > clock_timestamp() FOR SHARE)"
		args = append(args, l.rep.id, ld.term)
	}
	var one int
	err := l.rep.side.QueryRowContext(ctx, `INSERT INTO replica_once (key, until)
			SELECT ?1, CAST(extract(epoch FROM clock_timestamp()) + ?2 AS BIGINT) WHERE TRUE`+fence+`
		ON CONFLICT (key) DO UPDATE SET until = excluded.until WHERE replica_once.until < CAST(extract(epoch FROM clock_timestamp()) AS BIGINT)
		RETURNING 1`, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, l.rep.stillLeads(ctx, leaseOf(ctx))
	}
	return err == nil, err
}

// stillLeads says, when a claim made with a lease took nothing, whether
// that was for the lease being another's: ErrNotLeading.
func (r *replicas) stillLeads(ctx context.Context, ld *leadership) error {
	if ld == nil || ld.r != r {
		return nil
	}
	var n int
	if err := r.side.QueryRowContext(ctx, "SELECT count(*) FROM replica_lease WHERE holder = ? AND term = ? AND until > clock_timestamp()",
		r.id, ld.term).Scan(&n); err != nil {
		return err
	}
	if n == 0 || !ld.valid() {
		return ErrNotLeading
	}
	return nil
}

// Unclaim gives up what ClaimOnce took, for another to take at once.
func (l *Ledger) Unclaim(ctx context.Context, key string) error {
	ctx, cancel := side(ctx)
	defer cancel()
	if l.rep == nil {
		return nil
	}
	_, err := l.rep.side.ExecContext(ctx, "DELETE FROM replica_once WHERE key = ?", "claim:"+key)
	return err
}

// maxOnce is how many uses once the replicas keep at most, swept at their
// time: past it, Once refuses, as a process's own set did past its bound.
// Replaced in tests.
var maxOnce = 100_000

// ErrOnceFull is Once with maxOnce kept: what would be used once is
// refused until some are swept.
var ErrOnceFull = errors.New("the uses once the replicas keep are as many as they may be: refused until those over are swept")

// Once says whether key is new to every replica, keeping it for d: a
// sign-in's state used once, wherever its callback lands. For a ledger no
// replicas share, an error: its process keeps what it used.
func (l *Ledger) Once(ctx context.Context, key string, d time.Duration) (bool, error) {
	ctx, cancel := side(ctx)
	defer cancel()
	if l.rep == nil {
		return false, errors.New("only a ledger serve's replicas share what was used once")
	}
	var kept int
	if err := l.rep.side.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM replica_once LIMIT ?) AS kept", maxOnce).Scan(&kept); err != nil {
		return false, err
	}
	if kept >= maxOnce {
		return false, ErrOnceFull
	}
	var one int
	err := l.rep.side.QueryRowContext(ctx, `INSERT INTO replica_once (key, until) VALUES (?1, CAST(extract(epoch FROM clock_timestamp()) + ?2 AS BIGINT))
		ON CONFLICT (key) DO UPDATE SET until = excluded.until WHERE replica_once.until < CAST(extract(epoch FROM clock_timestamp()) AS BIGINT)
		RETURNING 1`, "once:"+key, d.Seconds()).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Take takes n from a rate limit's bucket that every replica shares,
// rate a second and burst at most, by the database's clock: false when it
// holds less. For a ledger no replicas share, an error: each process keeps
// its own.
func (l *Ledger) Take(ctx context.Context, name string, rate, burst, n float64) (bool, error) {
	ctx, cancel := side(ctx)
	defer cancel()
	if l.rep == nil {
		return false, errors.New("only a ledger serve's replicas share rate limits")
	}
	if n > burst {
		return false, nil
	}
	var left float64
	const now = "CAST(extract(epoch FROM clock_timestamp()) AS DOUBLE PRECISION)"
	const rate2, burst3, n4 = "CAST(?2 AS DOUBLE PRECISION)", "CAST(?3 AS DOUBLE PRECISION)", "CAST(?4 AS DOUBLE PRECISION)"
	const filled = "least(" + burst3 + ", replica_buckets.tokens + (" + now + " - replica_buckets.at) * " + rate2 + ")"
	err := l.rep.side.QueryRowContext(ctx, `INSERT INTO replica_buckets (name, tokens, at) VALUES (?1, `+burst3+` - `+n4+`, `+now+`)
		ON CONFLICT (name) DO UPDATE SET tokens = `+filled+` - `+n4+`, at = `+now+`
		WHERE `+filled+` >= `+n4+`
		RETURNING tokens`, name, rate, burst, n).Scan(&left)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Give gives one back to a bucket Take took from, up to burst.
func (l *Ledger) Give(ctx context.Context, name string, burst float64) error {
	ctx, cancel := side(ctx)
	defer cancel()
	if l.rep == nil {
		return nil
	}
	_, err := l.rep.side.ExecContext(ctx, "UPDATE replica_buckets SET tokens = least(CAST(? AS DOUBLE PRECISION), tokens + 1) WHERE name = ?", burst, name)
	return err
}

// MetaOnce is the value kept for lapilli serve under key, keeping value
// there first if there is none: the replicas that start at once each read
// the one kept.
func (l *Ledger) MetaOnce(ctx context.Context, key, value string) (string, error) {
	var kept string
	err := l.writeAside(ctx, func(tx *txn) error {
		old, err := l.meta(ctx, tx, "x."+key)
		if err != nil {
			return err
		}
		if old != "" {
			kept = old
			return nil
		}
		kept = value
		return setMeta(ctx, tx, "x."+key, value)
	})
	return kept, err
}
