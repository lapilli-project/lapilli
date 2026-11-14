package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/ledger/ledgertest"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/verdict"
)

// quickReplicas shortens how the replicas share a ledger for a test: a
// lease of 1.5 s, renewed each 300 ms, a follower asking each 100 ms, a
// notice claimed for 3 s.
func quickReplicas(t *testing.T) ledger.ReplicaTimings {
	t.Helper()
	q := ledger.ReplicaTimings{Lease: 1500 * time.Millisecond, Renew: 300 * time.Millisecond, Retry: 100 * time.Millisecond,
		Margin: 300 * time.Millisecond, Campaign: 100 * time.Millisecond, Refresh: 100 * time.Millisecond, Beat: 150 * time.Millisecond,
		Live: time.Second, Claim: 3 * time.Second}
	was := ledger.SetReplicaTimings(q)
	t.Cleanup(func() { ledger.SetReplicaTimings(was) })
	return q
}

// sharedLedger is a file naming a Postgres schema of the test's own, for
// several servers to share; the test is skipped without one.
func sharedLedger(t *testing.T) string {
	t.Helper()
	f := ledgertest.File(t)
	if f == "" {
		t.Skip("no " + ledgertest.Env + ": replicas share a Postgres ledger")
	}
	return f
}

// sink counts what it is sent: webhook notices by their id, and the
// digest's posts to Slack.
type sink struct {
	*httptest.Server
	mu     sync.Mutex
	ids    map[string]int
	events map[string]int
	slack  atomic.Int64
}

func newSink(t *testing.T) *sink {
	rc := &sink{ids: map[string]int{}, events: map[string]int{}}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/slack" {
			rc.slack.Add(1)
			return
		}
		var n struct{ ID, Event string }
		json.Unmarshal(b, &n)
		rc.mu.Lock()
		rc.ids[n.ID]++
		rc.events[n.Event]++
		rc.mu.Unlock()
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *sink) got() (ids, twice int) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, n := range rc.ids {
		if n > 1 {
			twice++
		}
	}
	return len(rc.ids), twice
}

// replica is a server sharing a ledger with others, its own rules beside
// it, sending what happens to rc.
func replica(t *testing.T, ledgerFile string, rc *sink, change func(*Config)) *Server {
	t.Helper()
	dir := t.TempDir()
	rules := filepath.Join(dir, "rules")
	os.MkdirAll(rules, 0o755)
	os.WriteFile(filepath.Join(rules, "shop.yaml"), []byte(ruleFile), 0o644)
	hook, slack := filepath.Join(dir, "hook"), filepath.Join(dir, "slack")
	os.WriteFile(hook, []byte(rc.URL+"/hook"), 0o600)
	os.WriteFile(slack, []byte(rc.URL+"/slack"), 0o600)
	cfg := Config{
		RulesPath: rules, Impact: "impact", ImpactFor: 5 * time.Minute,
		Options: verdict.Options{Lead: 15 * time.Minute, RefireWithin: time.Hour, UnknownLen: time.Hour, Match: []string{"service"}},
		Step:    time.Minute, Loc: time.UTC, LedgerURLFile: ledgerFile,
		Every: time.Hour, Settle: time.Hour, Backfill: 3 * 24 * time.Hour, Watch: time.Minute,
		Digest: Weekly{time.Monday, 9, 0}, Timeout: time.Minute, Parallel: 4, Listen: "127.0.0.1:0", Version: "test",
		AllowedHosts: []string{"example.com"}, UserHeader: "X-Forwarded-Email", NotifyWebhook: hook, Slack: slack,
	}
	if change != nil {
		change(&cfg)
	}
	s, err := New(context.Background(), cfg, &history{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	t.Cleanup(func() { s.Close() })
	return s
}

// running runs a server until the test ends, or stop: stop waits for Run
// to return.
func running(t *testing.T, s *Server) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("Run didn't return within 30s of its context's end")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// until waits up to d for ok, and fails the test with what.
func until(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !ok() {
		if time.Now().After(end) {
			t.Fatalf("%s: not within %v", what, d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// leaderOf is the server that leads, nil for none, failing the test when
// two do.
func leaderOf(t *testing.T, ss []*Server) *Server {
	t.Helper()
	var l *Server
	for _, s := range ss {
		if s.l.Leading() {
			if l != nil {
				t.Fatalf("two replicas lead at once")
			}
			l = s
		}
	}
	return l
}

// task makes a task through a server's API: a notice for the webhook.
func task(t *testing.T, s *Server, title string) {
	t.Helper()
	if code, body := api(t, s.Handler(), "POST", "/api/v1/tasks", `{"title": "`+title+`", "rule": "Errors"}`); code != http.StatusCreated {
		t.Fatalf("task: %d %s", code, body)
	}
}

// unsent is how many notices the ledger holds unsent.
func unsent(t *testing.T, s *Server) int {
	t.Helper()
	n, err := s.l.Unsent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Three replicas on one ledger: one runs the backfill, the digest and the
// outbox, each once; the notices of what people did on any of them go out
// once each; and the pages, the API and the readiness of each are the
// others' too.
func TestReplicasRunEachJobOnce(t *testing.T) {
	f := sharedLedger(t)
	quickReplicas(t)
	rc := newSink(t)
	ss := []*Server{replica(t, f, rc, nil), replica(t, f, rc, nil), replica(t, f, rc, nil)}
	for _, s := range ss {
		running(t, s)
	}
	ctx := context.Background()
	until(t, 30*time.Second, "a replica leads and runs the backfill", func() bool {
		c, _ := ss[0].l.Cursor(ctx)
		return leaderOf(t, ss) != nil && c.Equal(now)
	})
	until(t, 30*time.Second, "the digest goes", func() bool { return rc.slack.Load() > 0 })
	for i, s := range ss {
		for j := range 5 {
			task(t, s, fmt.Sprintf("task %d.%d", i, j))
		}
	}
	until(t, 30*time.Second, "the notices go", func() bool { return unsent(t, ss[1]) == 0 })
	time.Sleep(time.Second) // anything sent twice would be by now
	ids, twice := rc.got()
	rc.mu.Lock()
	tasks := rc.events["task.created"]
	rc.mu.Unlock()
	if tasks != 15 || twice != 0 {
		t.Errorf("15 tasks made on three replicas: %d told, %d of %d notices twice", tasks, twice, ids)
	}
	if n := rc.slack.Load(); n != 1 {
		t.Errorf("the digest was posted %d times", n)
	}
	// Each run once: no two of a kind over the same span.
	runs, err := ss[2].l.Runs(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, r := range runs {
		seen[fmt.Sprint(r.Kind, r.From.Unix(), r.To.Unix())]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("run %s recorded %d times", k, n)
		}
	}
	if len(seen) == 0 {
		t.Error("no runs")
	}
	// Each replica's readiness says which leads, and its metrics whether
	// it does.
	lead := leaderOf(t, ss)
	for _, s := range ss {
		until(t, 5*time.Second, "who leads, read", func() bool { p, ok := s.l.Leader(); return ok && p.ID == lead.l.ReplicaID() })
		code, body, _ := get(t, s.MetricsHandler(), "/readyz")
		want := "follows " + lead.l.ReplicaID()
		if s == lead {
			want = "leads, term"
		}
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("/readyz: %d %q, want %q", code, body, want)
		}
		_, m, _ := get(t, s.MetricsHandler(), "/metrics")
		gauge := "lapilli_serve_leader 0"
		if s == lead {
			gauge = "lapilli_serve_leader 1"
		}
		if !strings.Contains(m, gauge) || !strings.Contains(m, "lapilli_serve_replicas 3") {
			t.Errorf("/metrics wants %q and three replicas", gauge)
		}
	}
}

// proxy passes a replica's connections to the database through, until it
// is cut: then what either side sends is lost, and connections made after
// are kept silent, as when the replica is cut off from the database.
type proxy struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
	dead  atomic.Bool
}

// throughProxy is a file naming the ledger f names, through a proxy.
func throughProxy(t *testing.T, f string) (*proxy, string) {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{ln: ln}
	up := u.Host
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s, err := net.Dial("tcp", up)
			if err != nil {
				c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, s)
			p.mu.Unlock()
			pipe := func(dst, src net.Conn) {
				buf := make([]byte, 32<<10)
				for {
					n, err := src.Read(buf)
					if n > 0 && !p.dead.Load() {
						if _, err := dst.Write(buf[:n]); err != nil {
							return
						}
					}
					if err != nil {
						if !p.dead.Load() {
							dst.Close()
						}
						return
					}
				}
			}
			go pipe(s, c)
			go pipe(c, s)
		}
	}()
	t.Cleanup(p.drop)
	u.Host = ln.Addr().String()
	out := filepath.Join(t.TempDir(), "ledger-url")
	os.WriteFile(out, []byte(u.String()+"\n"), 0o600)
	return p, out
}

// cut keeps every connection silent from now on.
func (p *proxy) cut() { p.dead.Store(true) }

// drop ends every connection, and takes no more: the process died.
func (p *proxy) drop() {
	p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
}

// A leader cut off from the database, or killed, is followed by another
// replica within its lease and a campaign; it stops leading first, by its
// own clock; and no notice goes twice, those made while it was gone among
// them.
func TestReplicasTakeOver(t *testing.T) {
	for _, how := range []string{"cut off", "killed"} {
		t.Run(how, func(t *testing.T) {
			f := sharedLedger(t)
			q := quickReplicas(t)
			rc := newSink(t)
			p, via := throughProxy(t, f)
			a := replica(t, via, rc, nil)
			stopA := running(t, a)
			until(t, 30*time.Second, "the first replica leads", func() bool { return a.l.Leading() })
			b, c := replica(t, f, rc, nil), replica(t, f, rc, nil)
			running(t, b)
			running(t, c)
			ss := []*Server{a, b, c}
			for i := range 5 {
				task(t, b, fmt.Sprintf("before %d", i))
			}
			until(t, 30*time.Second, "the notices before go", func() bool { return unsent(t, b) == 0 })
			gone := time.Now()
			switch how {
			case "cut off":
				p.cut()
			case "killed":
				p.drop()
				go stopA()
			}
			var stopped, next time.Time
			until(t, q.Lease+5*q.Campaign+2*time.Second, "another replica leads", func() bool {
				if stopped.IsZero() && !a.l.Leading() {
					stopped = time.Now()
				}
				if lead := leaderOf(t, ss); lead != nil && lead != a {
					next = time.Now()
					return true
				}
				return false
			})
			if stopped.IsZero() || !stopped.Before(next) {
				t.Errorf("the old leader stopped at %v, the new one led at %v", stopped, next)
			}
			if d := stopped.Sub(gone); d > q.Lease-q.Margin+200*time.Millisecond {
				t.Errorf("the old leader stopped %v after it was %s, want within %v", d, how, q.Lease-q.Margin)
			}
			if d := next.Sub(gone); d > q.Lease+3*q.Campaign+time.Second {
				t.Errorf("taken over %v after the leader was %s", d, how)
			}
			t.Logf("%s: the old leader stopped after %v, another led after %v (lease %v)", how,
				stopped.Sub(gone).Round(time.Millisecond), next.Sub(gone).Round(time.Millisecond), q.Lease)
			for i := range 5 {
				task(t, c, fmt.Sprintf("after %d", i))
			}
			until(t, 30*time.Second, "the notices after go", func() bool { return unsent(t, c) == 0 })
			time.Sleep(q.Claim) // a notice claimed twice would be sent by now
			ids, twice := rc.got()
			rc.mu.Lock()
			tasks := rc.events["task.created"]
			rc.mu.Unlock()
			if tasks != 10 || twice != 0 {
				t.Errorf("ten tasks, across the takeover: %d told, %d of %d notices twice", tasks, twice, ids)
			}
			if n := rc.slack.Load(); n != 1 {
				t.Errorf("the digest posted %d times", n)
			}
		})
	}
}

// A leader whose lease another took in the database, while its clock says
// it still leads (it was frozen), sends nothing more: no notice it would
// claim, no digest; and what it would write as the leader's is refused.
func TestReplicasFormerLeaderSendsNothing(t *testing.T) {
	f := sharedLedger(t)
	quickReplicas(t)
	rc := newSink(t)
	a := replica(t, f, rc, nil)
	ctx := context.Background()
	got := make(chan context.Context, 1)
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.l.Elect(ectx, ledger.Elected{Version: "test"}, func(lctx context.Context) {
		got <- lctx
		<-lctx.Done()
	})
	var lctx context.Context
	select {
	case lctx = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("never led")
	}
	task(t, a, "due")
	db := ledgerDB(t, a)
	defer db.Close()
	if _, err := db.ExecContext(ctx, "UPDATE replica_lease SET holder = 'another', term = term + 1, until = clock_timestamp() + interval '1 minute'"); err != nil {
		t.Fatal(err)
	}
	if err := a.sendDue(lctx, "webhook", time.Now()); err == nil {
		t.Error("sending, the lease another's: no error")
	}
	if err := a.postDigest(lctx, digestIn{On: true, Dests: []string{"slack"}}, "2950", now.Add(-7*24*time.Hour), now, a.cfg.Options); err == nil {
		t.Error("posting the digest, the lease another's: no error")
	}
	if ids, _ := rc.got(); ids != 0 || rc.slack.Load() != 0 {
		t.Errorf("a former leader sent %d notices and %d digests", ids, rc.slack.Load())
	}
	if err := a.l.SetMeta(lctx, "k", "v"); err == nil {
		t.Error("a former leader's write went through")
	}
	if unsent(t, a) != 1 {
		t.Error("the notice isn't there for the leader to send")
	}
}

// A leader whose lease ended by its clock does none of the leader's jobs,
// before anything cancels its work (a process frozen past its lease, a
// renewal under way): none is begun, Prometheus isn't asked, nothing is
// recorded.
func TestReplicasNoJobsOnceTheLeaseEnded(t *testing.T) {
	f := sharedLedger(t)
	quickReplicas(t)
	rc := newSink(t)
	a := replica(t, f, rc, nil)
	ctx := context.Background()
	got := make(chan context.Context, 1)
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.l.Elect(ectx, ledger.Elected{Version: "test"}, func(lctx context.Context) {
		got <- lctx
		<-lctx.Done()
	})
	var lctx context.Context
	select {
	case lctx = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("never led")
	}
	raw := ""
	if err := a.look(lctx, &raw); err != nil {
		t.Fatal(err)
	}
	h := a.h.(*history)
	frozen := ledger.Lapsed(context.WithoutCancel(lctx))
	if ledger.Leads(frozen) || frozen.Err() != nil {
		t.Fatal("not as a frozen leader holds it")
	}
	h.Lock()
	h.asked = 0
	h.Unlock()
	a.jobs(frozen, true)
	h.Lock()
	asked := h.asked
	h.Unlock()
	if asked != 0 {
		t.Errorf("a leader whose lease ended asked Prometheus %d times", asked)
	}
	if c, _ := a.l.Cursor(ctx); !c.IsZero() {
		t.Errorf("and recorded a run, to %v", c)
	}
}

// What one replica writes shows on another's pages and API at once: each
// request reads what the others wrote first, so a page kept for an older
// generation isn't served.
func TestReplicasPagesShowOthersWrites(t *testing.T) {
	f := sharedLedger(t)
	rc := newSink(t)
	a, b := replica(t, f, rc, nil), replica(t, f, rc, nil)
	ha, hb := a.Handler(), b.Handler()
	if code, body, _ := get(t, ha, "/api/v1/incidents"); code != http.StatusOK || strings.Contains(body, "INC-9") {
		t.Fatalf("before: %d %s", code, body)
	}
	get(t, ha, "/incidents") // kept for this generation
	if code, body := api(t, hb, "POST", "/api/v1/incidents", `{"id": "INC-9", "title": "Cart slow", "start": "2026-11-02T08:00:00Z", "labels": {"service": ["cart"]}}`); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("written on the other: %d %s", code, body)
	}
	if _, body, _ := get(t, ha, "/api/v1/incidents"); !strings.Contains(body, "INC-9") {
		t.Errorf("another replica's incident isn't in the API:\n%s", body)
	}
	if _, body, _ := get(t, ha, "/incidents"); !strings.Contains(body, "Cart slow") {
		t.Error("another replica's incident isn't on the page kept")
	}
	// A task, and its count on the other's page of tasks.
	task(t, a, "tune Errors")
	if _, body, _ := get(t, hb, "/tasks"); !strings.Contains(body, "tune Errors") {
		t.Error("another replica's task isn't on the page")
	}
}

// What a person turns on one replica reaches the leader on another: the
// admin's pause, a change to the settings (judged again), the forms shown,
// the sign-in's state used, the rate limits.
func TestReplicasShareWhatPeopleDo(t *testing.T) {
	f := sharedLedger(t)
	rc := newSink(t)
	a, b := replica(t, f, rc, nil), replica(t, f, rc, nil)
	ctx := context.Background()
	// Paused on b: a, which would carry changes out, finds it so.
	if a.changesPaused() {
		t.Fatal("paused at start")
	}
	if err := b.l.SetSwitch(ctx, pausedKey, true, "ana", "changes to the rules paused"); err != nil {
		t.Fatal(err)
	}
	if err := a.l.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !a.changesPaused() {
		t.Error("a pause on another replica doesn't reach this one")
	}
	// Settings changed on b: a judges again, once.
	if _, asked := a.rejudgeAsked(ctx); asked {
		t.Fatal("asked at start")
	}
	b.askRejudge(ctx)
	if _, asked := a.rejudgeAsked(ctx); !asked {
		t.Error("settings changed on another replica don't ask this one to judge again")
	}
	a.jobs(ctx, false)
	if _, asked := a.rejudgeAsked(ctx); asked {
		t.Error("still asked once judged again")
	}
	// A form shown by one is good on the other.
	p := ledger.Proposal{ID: 7, State: ledger.StateApproved, Updated: now}
	if a.formKey(p, "ana", "approve") != b.formKey(p, "ana", "approve") {
		t.Error("a form one replica shows is refused by the other")
	}
	// A sign-in's state is used once among them.
	if !a.usedOnce(ctx, "state-1", now.Add(time.Hour).Unix()) || b.usedOnce(ctx, "state-1", now.Add(time.Hour).Unix()) {
		t.Error("a sign-in's state used twice, once on each replica")
	}
	// The incidents token's burst is theirs, not each one's.
	n := 0
	for i := range 100 {
		s := a
		if i%2 == 1 {
			s = b
		}
		if s.tokenRate.allow(time.Now()) {
			n++
		}
	}
	if n < 40 || n > 42 {
		t.Errorf("a burst of 40 shared by two replicas let %d through", n)
	}
}

// A replica that follows shows what the leader is doing, as it says, on
// its pages.
func TestReplicasShowTheLeadersStatus(t *testing.T) {
	f := sharedLedger(t)
	quickReplicas(t)
	rc := newSink(t)
	a, b := replica(t, f, rc, nil), replica(t, f, rc, nil)
	running(t, a)
	until(t, 30*time.Second, "a leads", func() bool { return a.l.Leading() })
	running(t, b)
	until(t, 30*time.Second, "the first jobs done", func() bool { return a.ready.Load() })
	time.Sleep(200 * time.Millisecond)
	a.status.set("replaying %s → %s", "2026-11-03 10:00", "2026-11-03 11:00")
	a.fail("a run", fmt.Errorf("prometheus said no"))
	until(t, 5*time.Second, "the follower shows the leader's status", func() bool {
		v := b.statusView("en")
		return v.Doing == "replaying 2026-11-03 10:00 → 2026-11-03 11:00" && strings.Contains(v.Err, "prometheus said no")
	})
	if _, body, _ := get(t, b.Handler(), "/runs"); !strings.Contains(body, "prometheus said no") {
		t.Error("the follower's runs page doesn't say the leader's error")
	}
}

// A replica that can't reach the ledger's database isn't ready, so that the
// others answer for it; while it can, its readiness says who leads.
func TestReplicasReadyNeedsTheDatabase(t *testing.T) {
	f := sharedLedger(t)
	rc := newSink(t)
	p, via := throughProxy(t, f)
	a := replica(t, via, rc, nil)
	ctx := context.Background()
	if err := a.l.Record(ctx, ledger.Found{From: now.Add(-time.Hour), To: now,
		Run: ledger.Run{Kind: "hourly", Started: now, Finished: now}}, a.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := get(t, a.MetricsHandler(), "/readyz"); code != http.StatusOK || !strings.Contains(body, "no replica leads now") {
		t.Fatalf("reaching the database: %d %q", code, body)
	}
	p.drop()
	until(t, 10*time.Second, "not ready without the database", func() bool {
		code, body, _ := get(t, a.MetricsHandler(), "/readyz")
		return code == http.StatusServiceUnavailable && strings.Contains(body, "can't reach the ledger's database")
	})
}

// Replicas sign people in with one key: a second with a sign-in of its own
// and no --session-key-file (a key of its own) is refused while one that
// signs with the key lives, and one with the key joins.
func TestReplicasSignInNeedsOneKey(t *testing.T) {
	quickReplicas(t)
	s, _ := oidcServer(t, nil)
	if !s.l.Shared() {
		t.Skip("the ledgers are SQLite's")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.l.Elect(ctx, ledger.Elected{Version: "test"}, func(c context.Context) { <-c.Done() })
	until(t, 5*time.Second, "the first replica lives", func() bool { return s.l.Replicas() == 1 })
	cfg := s.cfg
	cfg.OIDC = freshOIDC(s)
	key := cfg.SessionKey
	cfg.SessionKey = ""
	if b, err := New(context.Background(), cfg, &history{}, s.log); err == nil {
		b.Close()
		t.Fatal("a second replica, signing sessions with a key of its own, joined")
	} else if !strings.Contains(err.Error(), "--session-key-file") {
		t.Errorf("refused, saying: %v", err)
	}
	cfg.SessionKey = key
	cfg.OIDC = freshOIDC(s)
	b, err := New(context.Background(), cfg, &history{}, s.log)
	if err != nil {
		t.Fatalf("with the key: %v", err)
	}
	b.Close()
}

// dense is a history whose rules fire often: each of 100 series is true
// for 10 minutes each hour, so a backfill writes thousands of firings a day.
type dense struct{}

func (dense) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	if expr == "impact" {
		return nil, nil
	}
	var out []prom.Series
	for i := range 100 {
		s := prom.Series{Labels: map[string]string{"service": fmt.Sprintf("svc%d", i%5), "pod": fmt.Sprintf("p%d", i)}}
		for t := start.Truncate(step); !t.After(end); t = t.Add(step) {
			if (t.Unix()/60-int64(i))%60 < 10 {
				s.Samples = append(s.Samples, prom.Sample{T: t, V: 1})
			}
		}
		if len(s.Samples) > 0 {
			out = append(out, s)
		}
	}
	return out, nil
}

// writeWaitBound is how long a write on any replica may wait while the
// leader works: a run's piece, a day of firings while it backfills, is a
// write of its own, and no leader's job holds the writers' turn longer.
const writeWaitBound = time.Second

// A write on a follower or on the leader waits on no leader's job for long
// while the leader backfills four weeks of dense firings, as a new ledger's
// first start does: each day is a write of its own.
func TestReplicasWritesDontWaitOnTheBackfill(t *testing.T) {
	f := sharedLedger(t)
	quickReplicas(t)
	rc := newSink(t)
	a := replica(t, f, rc, func(c *Config) { c.Backfill = 28 * 24 * time.Hour })
	a.h = dense{}
	b := replica(t, f, rc, func(c *Config) { c.Backfill = 28 * 24 * time.Hour })
	b.h = dense{}
	running(t, a)
	until(t, 30*time.Second, "a leads", func() bool { return a.l.Leading() })
	running(t, b)
	ctx := context.Background()
	var longest time.Duration
	n, during := 0, 0
	end := time.Now().Add(2 * time.Minute)
	for time.Now().Before(end) {
		c, _ := a.l.Cursor(ctx)
		caughtUp := c.Equal(now)
		for _, s := range []*Server{b, a} {
			began := time.Now()
			task(t, s, fmt.Sprintf("during %d", n))
			longest = max(longest, time.Since(began))
			n++
			if !caughtUp {
				during++
			}
		}
		if caughtUp {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	runs, err := a.l.Runs(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	firings := 0
	for _, r := range runs {
		firings += r.Firings
	}
	t.Logf("%d writes, %d of them while %d runs wrote %d firings: the longest waited %v", n, during, len(runs), firings, longest.Round(time.Millisecond))
	if longest > writeWaitBound {
		t.Errorf("a write waited %v while the leader backfilled, more than %v", longest, writeWaitBound)
	}
	if during < 10 || len(runs) < 20 {
		t.Errorf("the backfill was over before it could be written beside: %d writes during it, %d runs", during, len(runs))
	}
}
