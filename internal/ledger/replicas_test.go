package ledger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger/ledgertest"
)

// fastLease shortens the replicas' timings for a test: a lease of 1.5 s,
// renewed each 300 ms, a follower asking each 100 ms.
func fastLease(t *testing.T) {
	t.Helper()
	was := []time.Duration{leaseFor, renewEvery, retryEvery, leaseMargin, campaignEvery, refreshEvery, beatEvery, liveFor, claimFor}
	leaseFor, renewEvery, retryEvery, leaseMargin = 1500*time.Millisecond, 300*time.Millisecond, 100*time.Millisecond, 300*time.Millisecond
	campaignEvery, refreshEvery, beatEvery, liveFor, claimFor = 100*time.Millisecond, 100*time.Millisecond, 200*time.Millisecond, time.Second, 2*time.Second
	t.Cleanup(func() {
		leaseFor, renewEvery, retryEvery, leaseMargin = was[0], was[1], was[2], was[3]
		campaignEvery, refreshEvery, beatEvery, liveFor, claimFor = was[4], was[5], was[6], was[7], was[8]
	})
}

// replicaURL is a schema of the test's own, or the test is skipped.
func replicaURL(t *testing.T) string {
	t.Helper()
	u := ledgertest.URL(t)
	if u == "" {
		t.Skip("no " + ledgertest.Env + ": replicas share a Postgres ledger")
	}
	return u
}

func openReplica(t *testing.T, u string) *Ledger {
	t.Helper()
	l, err := OpenPostgres(context.Background(), u, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// within waits up to d for ok, failing the test with what after.
func within(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !ok() {
		if time.Now().After(end) {
			t.Fatalf("%s: not within %v", what, d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Replicas started at once on an empty schema make the ledger once, each
// migration under the writers' turn: none finds tables half made.
func TestReplicasStartTogether(t *testing.T) {
	u := replicaURL(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := OpenPostgres(context.Background(), u, time.UTC)
			if err == nil {
				l.Close()
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d: %v", i, err)
		}
	}
}

// A write that reads what it changes reads what no other replica's write
// is changing: each takes the writers' turn. Values merged lose nothing,
// and a limit counted holds, with three replicas writing at once.
func TestReplicasTakeTurnsWriting(t *testing.T) {
	u := replicaURL(t)
	ctx := context.Background()
	ls := []*Ledger{openReplica(t, u), openReplica(t, u), openReplica(t, u)}
	var wg sync.WaitGroup
	for _, l := range ls {
		for range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 20 {
					if err := l.UpdateMeta(ctx, "n", func(old string) string {
						n, _ := strconv.Atoi(old)
						return strconv.Itoa(n + 1)
					}); err != nil {
						t.Error(err)
						return
					}
				}
			}()
		}
	}
	wg.Wait()
	if v, _ := ls[0].Meta(ctx, "n"); v != "180" {
		t.Errorf("180 merged by three replicas at once: %s", v)
	}
	// A team's editors keep MaxTeamWindows of its windows at most, made on
	// any replica.
	team, err := ls[0].PutTeam(ctx, Team{Name: "shop", Rules: ".*"}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	scoped := WithTeams(ctx, []int64{team})
	var made atomic.Int64
	for _, l := range ls {
		for g := range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range MaxTeamWindows {
					start := t0.Add(time.Duration(g*100+i) * time.Hour)
					_, err := l.PutWindow(scoped, Window{Title: fmt.Sprintf("w%d", i), Start: start, End: start.Add(time.Hour), TeamID: team}, "bo", opts)
					if err == nil {
						made.Add(1)
					}
				}
			}()
		}
	}
	wg.Wait()
	ws, err := ls[1].Windows(ctx, t0.Add(-time.Hour), t0.Add(400*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if made.Load() != MaxTeamWindows || len(ws) != MaxTeamWindows {
		t.Errorf("a team's editors kept %d windows (%d said made), at most %d", len(ws), made.Load(), MaxTeamWindows)
	}
}

// A write on one replica shows on another's pages: the generation moves,
// the weeks whose firings changed are taken for changed, those that didn't
// aren't, and ids another deleted are no longer cached.
func TestReplicasSeeEachOthersWrites(t *testing.T) {
	u := replicaURL(t)
	ctx := context.Background()
	a, b := openReplica(t, u), openReplica(t, u)
	week, other := Week(a.Day(t0)), Week(a.Day(t0))+14
	if err := b.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	g0 := b.Gen()
	record(t, a, t0, t0.Add(24*time.Hour), []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: t0.Add(time.Hour), End: t0.Add(2 * time.Hour)}})
	if b.Gen() != g0 {
		t.Fatal("the generation moved before another replica's write was read")
	}
	if err := b.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if b.Gen() <= g0 || b.Changed(week) <= g0 || b.ChangedAny() <= g0 {
		t.Errorf("another replica's firings: gen %d, the week changed at %d, any at %d, from %d", b.Gen(), b.Changed(week), b.ChangedAny(), g0)
	}
	if b.Changed(other) > g0 {
		t.Errorf("a week no one wrote taken for changed: %d", b.Changed(other))
	}
	// A write a page shows, of no firing.
	g1 := b.Gen()
	if err := a.SetMeta(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := b.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if b.Gen() <= g1 || b.Changed(week) > g1 {
		t.Errorf("another replica's setting: gen %d from %d, the week %d", b.Gen(), g1, b.Changed(week))
	}
	// b caches the alert's id as it writes it again; a prunes it away.
	record(t, b, t0.Add(24*time.Hour), t0.Add(48*time.Hour), []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: t0.Add(25 * time.Hour), End: t0.Add(26 * time.Hour)}})
	b.ids.RLock()
	cached := len(b.ids.series)
	b.ids.RUnlock()
	if cached == 0 {
		t.Fatal("nothing cached to forget")
	}
	if err := a.Prune(ctx, t0.Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := b.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	b.ids.RLock()
	cached = len(b.ids.series)
	b.ids.RUnlock()
	if cached != 0 {
		t.Errorf("%d alerts another replica deleted still cached", cached)
	}
	// And b writes them anew, on ids it reads back.
	record(t, b, t0.Add(48*time.Hour), t0.Add(72*time.Hour), []firing.Firing{{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: t0.Add(49 * time.Hour), End: t0.Add(50 * time.Hour)}})
}

// A request asked after another replica's write sees it, whether or not a
// refresh was under way when it was asked: one that began before is not
// taken for one after.
func TestReplicasRefreshAfterAsked(t *testing.T) {
	u := replicaURL(t)
	ctx := context.Background()
	a, b := openReplica(t, u), openReplica(t, u)
	for i := range 20 {
		if err := a.SetMeta(ctx, "k", strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
		g := b.Gen()
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := b.Refresh(ctx); err != nil {
					t.Error(err)
				}
				if b.Gen() <= g {
					t.Errorf("write %d: a refresh asked after it didn't read it", i)
				}
			}()
		}
		wg.Wait()
	}
}

// leads keeps, for a test, when each replica led.
type leads struct {
	mu   sync.Mutex
	runs []leadRun
}

type leadRun struct {
	who        int
	term       int64
	start, end time.Time
	ctx        context.Context
}

// lead is an Elect's lead for replica who: it leads until told not to.
func (ls *leads) lead(who int, l *Ledger) func(context.Context) {
	return func(ctx context.Context) {
		ld := leaseOf(ctx)
		ls.mu.Lock()
		i := len(ls.runs)
		ls.runs = append(ls.runs, leadRun{who: who, term: ld.term, start: time.Now(), ctx: ctx})
		ls.mu.Unlock()
		<-ctx.Done()
		ls.mu.Lock()
		ls.runs[i].end = time.Now()
		ls.mu.Unlock()
	}
}

// current is the replica that leads now, -1 for none.
func (ls *leads) current() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for _, r := range ls.runs {
		if r.end.IsZero() {
			return r.who
		}
	}
	return -1
}

func (ls *leads) all() []leadRun {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return append([]leadRun(nil), ls.runs...)
}

// apart checks no two replicas led at once, and each term is later than
// the one before.
func (ls *leads) apart(t *testing.T) {
	t.Helper()
	runs := ls.all()
	for i := 1; i < len(runs); i++ {
		a, b := runs[i-1], runs[i]
		if a.end.IsZero() || b.start.Before(a.end) {
			t.Errorf("replica %d led from %v while %d led until %v", b.who, b.start, a.who, a.end)
		}
		if b.term <= a.term {
			t.Errorf("term %d followed term %d", b.term, a.term)
		}
	}
}

// elect has a replica take part in the election until the test ends or
// stop is called.
func elect(t *testing.T, l *Ledger, ls *leads, who int) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Elect(ctx, Elected{Version: "test", Status: func() []byte { return []byte(`{"who":` + strconv.Itoa(who) + `}`) }}, ls.lead(who, l))
	}()
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Elect didn't return within 10s of its context's end")
		}
	}
	t.Cleanup(stop)
	return stop
}

// One replica of three leads at a time; one that stops gives the lease up,
// and another leads within a campaign; the replicas know which leads.
func TestReplicasOneLeads(t *testing.T) {
	u := replicaURL(t)
	fastLease(t)
	ls := &leads{}
	reps := []*Ledger{openReplica(t, u), openReplica(t, u), openReplica(t, u)}
	stops := make([]func(), len(reps))
	for i, l := range reps {
		stops[i] = elect(t, l, ls, i)
	}
	within(t, 5*time.Second, "a replica leads", func() bool { return ls.current() >= 0 })
	// For two seconds, never two.
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		n := 0
		for _, l := range reps {
			if l.Leading() {
				n++
			}
		}
		if n > 1 {
			t.Fatalf("%d replicas lead at once", n)
		}
	}
	first := ls.current()
	for i, l := range reps {
		within(t, 2*time.Second, "who leads, read", func() bool {
			p, ok := l.Leader()
			return ok && p.ID == reps[first].ReplicaID() && p.Self == (i == first) && (p.Self || string(p.Status) == `{"who":`+strconv.Itoa(first)+`}`)
		})
		if n := l.Replicas(); n != 3 {
			t.Errorf("replica %d counts %d replicas", i, n)
		}
	}
	// It stops: another leads within a campaign or two.
	stopped := time.Now()
	stops[first]()
	within(t, 3*campaignEvery+time.Second, "another leads once the leader stopped", func() bool { c := ls.current(); return c >= 0 && c != first })
	t.Logf("handed over in %v", time.Since(stopped).Round(time.Millisecond))
	ls.apart(t)
}

// A leader cut off from the database stops leading by its own clock,
// before its lease lapses in the database, and another takes the lease
// once it has: within leaseFor and a campaign of the cut. What the old
// one writes with its lease after is refused.
func TestReplicasTakeOverFromACutOffLeader(t *testing.T) {
	u := replicaURL(t)
	fastLease(t)
	quick(t)
	p, via := newPartition(t, u)
	ls := &leads{}
	a := openReplica(t, via)
	elect(t, a, ls, 0)
	within(t, 5*time.Second, "the first replica leads", func() bool { return ls.current() == 0 })
	b, c := openReplica(t, u), openReplica(t, u)
	elect(t, b, ls, 1)
	elect(t, c, ls, 2)
	time.Sleep(3 * renewEvery) // renewed a few times
	cut := time.Now()
	p.down.Store(true)
	p.cut()
	within(t, leaseFor+3*campaignEvery+time.Second, "another leads once the leader is cut off", func() bool { c := ls.current(); return c > 0 })
	took := time.Since(cut)
	runs := ls.all()
	old, next := runs[0], runs[len(runs)-1]
	if old.end.IsZero() {
		t.Fatal("the old leader still leads")
	}
	if stopped := old.end.Sub(cut); stopped > leaseFor-leaseMargin+100*time.Millisecond {
		t.Errorf("the old leader stopped %v after the cut, want within %v", stopped, leaseFor-leaseMargin)
	}
	if !next.start.After(old.end) {
		t.Errorf("the new leader began at %v, before the old stopped at %v", next.start, old.end)
	}
	if took > leaseFor+3*campaignEvery+500*time.Millisecond {
		t.Errorf("took over in %v, want within %v", took, leaseFor+2*campaignEvery)
	}
	t.Logf("cut off: the old leader stopped after %v, another led after %v (lease %v)", old.end.Sub(cut).Round(time.Millisecond), took.Round(time.Millisecond), leaseFor)
	if a.Leading() || Leads(old.ctx) {
		t.Error("the old leader takes itself for the leader")
	}
	if err := a.SetMeta(old.ctx, "late", "1"); !errors.Is(err, ErrNotLeading) {
		t.Errorf("the old leader's write with its lease: %v", err)
	}
	if err := a.Hold(old.ctx); !errors.Is(err, ErrNotLeading) {
		t.Errorf("Hold, with the old lease: %v", err)
	}
	ls.apart(t)
}

// A write made with a lease is refused once the lease is another's in the
// database, though this replica's clock says it still leads (a process
// frozen past its lease, whose renewals another's term overtook); and once
// this replica's clock says the lease is over, though the database hasn't
// moved (frozen between its last check and its commit).
func TestReplicasFencedWrites(t *testing.T) {
	u := replicaURL(t)
	ctx := context.Background()
	a := openReplica(t, u)
	got := make(chan context.Context, 1)
	release := make(chan struct{})
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.Elect(ectx, Elected{Version: "test"}, func(lctx context.Context) {
		got <- lctx
		select {
		case <-release:
		case <-lctx.Done():
		}
	})
	var lctx context.Context
	select {
	case lctx = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("never led")
	}
	if err := a.SetMeta(lctx, "k", "1"); err != nil {
		t.Fatalf("a write while leading: %v", err)
	}
	if ok, err := a.ClaimOnce(lctx, "digest:1", time.Minute); !ok || err != nil {
		t.Fatalf("a claim while leading: %v %v", ok, err)
	}
	ld := leaseOf(lctx)
	// Frozen by this process's clock: its lease over, the database not yet.
	ld.mu.Lock()
	was := ld.until
	ld.until = time.Now().Add(-time.Millisecond)
	ld.mu.Unlock()
	if err := a.SetMeta(lctx, "k", "2"); !errors.Is(err, ErrNotLeading) {
		t.Errorf("a write once this replica's clock ended its lease: %v", err)
	}
	ld.mu.Lock()
	ld.until = was
	ld.mu.Unlock()
	// Its lease over while the write was under way: checked again before
	// it commits.
	err := a.write(lctx, func(t *txn) error {
		ld.mu.Lock()
		ld.until = time.Now().Add(-time.Millisecond)
		ld.mu.Unlock()
		return setMeta(t.ctx, t, "x.k", "2")
	})
	if !errors.Is(err, ErrNotLeading) {
		t.Errorf("a write whose lease ended before it committed: %v", err)
	}
	ld.mu.Lock()
	ld.until = was
	ld.mu.Unlock()
	// Another's in the database.
	admin, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "UPDATE replica_lease SET holder = 'another', term = term + 1, until = clock_timestamp() + interval '1 minute'"); err != nil {
		t.Fatal(err)
	}
	if !Leads(lctx) {
		t.Fatal("this replica's clock ended the lease already")
	}
	if err := a.SetMeta(lctx, "k", "3"); !errors.Is(err, ErrNotLeading) {
		t.Errorf("a write once the lease is another's: %v", err)
	}
	if ok, err := a.ClaimOnce(lctx, "digest:2", time.Minute); ok || !errors.Is(err, ErrNotLeading) {
		t.Errorf("a claim once the lease is another's: %v %v", ok, err)
	}
	if v, _ := a.Meta(ctx, "k"); v != "1" {
		t.Errorf("written: %q", v)
	}
	// A write that isn't the leader's goes on.
	if err := a.SetMeta(ctx, "k", "4"); err != nil {
		t.Errorf("a write of a follower's: %v", err)
	}
	close(release)
}

// Notices are claimed one at a time, each by one sender: two leaders for a
// moment, each with senders of their own, never take one notice twice. One
// claimed and never noted is claimed again once its claim is over.
func TestReplicasClaimNoticesOnce(t *testing.T) {
	u := replicaURL(t)
	fastLease(t)
	ctx := context.Background()
	a, b := openReplica(t, u), openReplica(t, u)
	var told []Told
	for i := range 200 {
		told = append(told, Told{Dest: "webhook", About: map[string]any{"n": i}})
	}
	if err := a.TellOnce(ctx, "k", "1", "test.event", told); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for _, l := range []*Ledger{a, b, a, b, a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, ok, err := l.Claim(ctx, "webhook", now, 0)
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				seen[n.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != 200 {
		t.Errorf("%d notices claimed, of 200", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("notice %d claimed %d times", id, n)
		}
	}
	if _, ok, _ := b.Claim(ctx, "webhook", now, 0); ok {
		t.Error("a notice claimed again within its claim")
	}
	// Once the claims are over, those never noted sent are claimed again;
	// those noted aren't.
	var some int64
	for id := range seen {
		some = id
		break
	}
	for id := range seen {
		if id != some {
			if err := a.Sent(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	later := now.Add(claimFor + time.Second)
	n, ok, err := b.Claim(ctx, "webhook", later, 0)
	if err != nil || !ok || n.ID != some {
		t.Errorf("the notice claimed and never noted, once its claim is over: %v %v %v, want %d", n.ID, ok, err, some)
	}
	if _, ok, _ := a.Claim(ctx, "webhook", later, 0); ok {
		t.Error("a notice noted sent claimed again")
	}
}

// A rate limit's bucket is the replicas', and a use once is once among
// them.
func TestReplicasShareLimits(t *testing.T) {
	u := replicaURL(t)
	ctx := context.Background()
	a, b := openReplica(t, u), openReplica(t, u)
	var took, first atomic.Int64
	var wg sync.WaitGroup
	for _, l := range []*Ledger{a, b, a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				ok, err := l.Take(ctx, "test", 0, 10, 1)
				if err != nil {
					t.Error(err)
				}
				if ok {
					took.Add(1)
				}
				if ok, err := l.Once(ctx, "state", time.Minute); err != nil {
					t.Error(err)
				} else if ok {
					first.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if took.Load() != 10 {
		t.Errorf("a burst of 10 shared: %d taken", took.Load())
	}
	if first.Load() != 1 {
		t.Errorf("used once: %d times", first.Load())
	}
	if err := a.Give(ctx, "test", 10); err != nil {
		t.Fatal(err)
	}
	if ok, _ := b.Take(ctx, "test", 0, 10, 1); !ok {
		t.Error("one given back isn't taken")
	}
}

// A change made on any replica wakes the leader, and settings changed
// there ask it to judge again.
func TestReplicasWakeTheLeader(t *testing.T) {
	u := replicaURL(t)
	ctx := context.Background()
	a, b := openReplica(t, u), openReplica(t, u)
	if err := a.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	r0 := a.Rejudges()
	if err := b.AskRejudge(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Woken():
	default:
		t.Error("not woken")
	}
	if a.Rejudges() != r0+1 {
		t.Errorf("asked to judge again: %d, from %d", a.Rejudges(), r0)
	}
	if err := b.Wake(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Woken():
	default:
		t.Error("not woken")
	}
	if a.Rejudges() != r0+1 {
		t.Errorf("a wake asked to judge again: %d", a.Rejudges())
	}
	// Kept once, whichever replica keeps it first.
	got := make([]string, 4)
	var wg sync.WaitGroup
	for i, l := range []*Ledger{a, b, a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = l.MetaOnce(ctx, "id", strconv.Itoa(i))
		}()
	}
	wg.Wait()
	for _, g := range got {
		if g != got[0] || g == "" {
			t.Errorf("kept once: %q", got)
			break
		}
	}
}

// A replica that finds a newer lapilli migrated the ledger gives it up.
func TestReplicasStopOnANewerSchema(t *testing.T) {
	u := replicaURL(t)
	ctx := context.Background()
	a := openReplica(t, u)
	admin, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "UPDATE schema_version SET version = version + 1"); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(ctx); !errors.Is(err, ErrNewer) {
		t.Fatalf("a newer schema: %v", err)
	}
	select {
	case <-a.Lost():
	default:
		t.Fatal("not lost")
	}
	if !errors.Is(a.LostWhy(), ErrNewer) {
		t.Errorf("why: %v", a.LostWhy())
	}
}
