package serve

// The replicas' guards their reviews asked for, in serve: sign-in across
// replicas, readiness, the digest after a takeover, a rejudge asked on a
// follower, and how the leader steps down.

import (
	"context"
	"errors"
	"fmt"
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

	"github.com/jackc/pgx/v5"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/signin"
)

// pgConn is a connection to a server's Postgres ledger, from outside it.
func pgConn(t *testing.T, s *Server) *pgx.Conn {
	t.Helper()
	b, err := os.ReadFile(s.cfg.LedgerURLFile)
	if err != nil {
		t.Fatal(err)
	}
	c, err := pgx.Connect(context.Background(), strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// A sign-in that signs no one in writes nothing to the ledger, however
// many there are, as no one need be signed in to make them; one that signs
// someone in keeps its state once, and its callback replayed to another
// replica is refused there.
func TestReplicasSignInStatesWriteNothingUnsigned(t *testing.T) {
	s, idp := oidcServer(t, nil)
	if !s.l.Shared() {
		t.Skip("the ledgers are SQLite's")
	}
	h := s.Handler()
	c := pgConn(t, s)
	count := func() int {
		var n int
		c.QueryRow(context.Background(), "SELECT count(*) FROM replica_once WHERE key LIKE 'once:signin:%'").Scan(&n)
		return n
	}
	for i := range 200 {
		b := newBrowser(t, h)
		w := b.do("GET", "/auth/login?next=/", nil)
		loc, _ := url.Parse(w.Header().Get("Location"))
		if w = b.do("GET", "/auth/callback?state="+url.QueryEscape(loc.Query().Get("state"))+"&error=access_denied", nil); w.Code != http.StatusForbidden {
			t.Fatalf("%d: callback %d", i, w.Code)
		}
	}
	if n := count(); n != 0 {
		t.Errorf("200 sign-ins that signed no one in kept %d states in the ledger", n)
	}
	// One that signs someone in, its callback replayed to another replica.
	cfg := s.cfg
	cfg.OIDC = freshOIDC(s)
	other, err := New(context.Background(), cfg, &history{}, s.log)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.now = s.now
	b := newBrowser(t, h)
	w := b.do("GET", "/auth/login?next=/", nil)
	back := idp.Authorize(t, w.Header().Get("Location"), claimsOf("ed@example.com"))
	saved := map[string]*http.Cookie{}
	for k, v := range b.cookies {
		cp := *v
		saved[k] = &cp
	}
	if w := b.do("GET", back, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("signed in: %d", w.Code)
	}
	if n := count(); n != 1 {
		t.Errorf("a sign-in kept %d states", n)
	}
	replay := newBrowser(t, other.Handler())
	replay.cookies = saved
	if w := replay.do("GET", back, nil); w.Code == http.StatusSeeOther {
		t.Error("a callback replayed to another replica signed in again")
	}
}

// Replicas compare the keys they sign sessions with. A replica whose key
// is newer than the live replicas' joins, as a rollout of a new key starts
// one, and every replica says how many keys sign sessions until the older
// go; one whose key is older than another live, as a pod the rollout
// replaces would be, started again, is refused.
func TestReplicasSessionKeysRotate(t *testing.T) {
	quickReplicas(t)
	s, _ := oidcServer(t, nil)
	if !s.l.Shared() {
		t.Skip("the ledgers are SQLite's")
	}
	ectx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.l.Elect(ectx, ledger.Elected{Version: "test"}, func(c context.Context) { <-c.Done() })
	until(t, 5*time.Second, "the first lives", func() bool { return s.l.Replicas() == 1 })
	newKey := filepath.Join(t.TempDir(), "key2")
	os.WriteFile(newKey, []byte(strings.Repeat("z", 40)), 0o600)
	cfg := s.cfg
	cfg.OIDC, cfg.SessionKey = freshOIDC(s), newKey
	b, err := New(context.Background(), cfg, &history{}, s.log)
	if err != nil {
		t.Fatalf("a replica with a newer key: %v", err)
	}
	defer b.Close()
	go b.l.Elect(ectx, ledger.Elected{Version: "test"}, func(c context.Context) { <-c.Done() })
	until(t, 5*time.Second, "the replicas count two keys", func() bool { return s.l.SessionKeys() == 2 })
	if _, body, _ := get(t, s.MetricsHandler(), "/metrics"); !strings.Contains(body, "lapilli_serve_session_keys 2") {
		t.Error("the metrics don't say two keys sign sessions")
	}
	if line := s.leadership(); !strings.Contains(line, "sign sessions with 2 keys") {
		t.Errorf("the status doesn't say two keys sign sessions: %q", line)
	}
	cfg.OIDC, cfg.SessionKey = freshOIDC(s), s.cfg.SessionKey
	if c, err := New(context.Background(), cfg, &history{}, s.log); err == nil {
		c.Close()
		t.Fatal("a replica with the older key joined, a newer one live")
	} else if !strings.Contains(err.Error(), "newer than this one's") {
		t.Errorf("refused, saying: %v", err)
	}
}

// The UI's port, which answers anyone whatever the host, says ok and no
// more; the metrics' port says which replica leads.
func TestReplicasReadinessSaysLittle(t *testing.T) {
	f := sharedLedger(t)
	rc := newSink(t)
	a := replica(t, f, rc, nil)
	if err := a.l.Record(context.Background(), ledger.Found{From: now.Add(-time.Hour), To: now,
		Run: ledger.Run{Kind: "hourly", Started: now, Finished: now}}, a.cfg.Options); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/readyz", nil)
	r.Host = "elsewhere.invalid"
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "ok\n" {
		t.Errorf("the UI's port: %d %q, want ok alone", w.Code, w.Body.String())
	}
	if code, body, _ := get(t, a.MetricsHandler(), "/readyz"); code != http.StatusOK || !strings.Contains(body, "replica "+a.l.ReplicaID()) {
		t.Errorf("the metrics' port: %d %q", code, body)
	}
}

// The writers' turn is one row for every replica: while a session holds it
// past turnWithin (a replica cut off in a write, until the server ends its
// session), every replica's writes wait, but each is still ready, as each
// answers its pages. Each says its writes wait, on the metrics' port, until
// a write takes the turn again.
func TestReplicasReadyWhileWritesWait(t *testing.T) {
	f := sharedLedger(t)
	q := quickReplicas(t)
	q.Turn = 500 * time.Millisecond
	ledger.SetReplicaTimings(q)
	rc := newSink(t)
	a, b := replica(t, f, rc, nil), replica(t, f, rc, nil)
	ctx := context.Background()
	if err := a.l.Record(ctx, ledger.Found{From: now.Add(-time.Hour), To: now,
		Run: ledger.Run{Kind: "hourly", Started: now, Finished: now}}, a.cfg.Options); err != nil {
		t.Fatal(err)
	}
	c := pgConn(t, a)
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT 1 FROM replica_gens WHERE one = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	for i, s := range []*Server{a, b} {
		if err := s.l.SetMeta(ctx, "k", "v"); !errors.Is(err, ledger.ErrTurnWaited) {
			t.Fatalf("replica %d, a write behind a turn held: %v", i, err)
		}
		code, body, _ := get(t, s.MetricsHandler(), "/readyz")
		if code != http.StatusOK || !strings.Contains(body, "writes wait") {
			t.Errorf("replica %d while writes wait: %d %q, want ready, saying writes wait", i, code, body)
		}
		if code, _ := api(t, s.Handler(), "GET", "/api/v1/runs", ""); code != http.StatusOK {
			t.Errorf("replica %d's pages while writes wait: %d", i, code)
		}
		if _, body, _ := get(t, s.MetricsHandler(), "/metrics"); !strings.Contains(body, "lapilli_serve_ledger_writes_waiting 1") {
			t.Errorf("replica %d's metrics don't say its writes wait", i)
		}
	}
	tx.Rollback(ctx)
	if err := a.l.SetMeta(ctx, "k", "w"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := get(t, a.MetricsHandler(), "/readyz"); code != http.StatusOK || strings.Contains(body, "writes wait") {
		t.Errorf("the turn taken again: %d %q", code, body)
	}
	if _, body, _ := get(t, a.MetricsHandler(), "/metrics"); !strings.Contains(body, "lapilli_serve_ledger_writes_waiting 0") {
		t.Error("the metrics still say writes wait, the turn taken again")
	}
}

// A leader that finds the digest's post claimed by another (the leader
// before it, whose claim runs on) tries again once the claim is over, not
// an hour after.
func TestReplicasDigestAfterAClaim(t *testing.T) {
	f := sharedLedger(t)
	quickReplicas(t)
	rc := newSink(t)
	a, b := replica(t, f, rc, nil), replica(t, f, rc, nil)
	ctx := context.Background()
	raw := ""
	if err := b.look(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.l.ClaimOnce(ctx, "digest-post:2950", time.Hour); !ok || err != nil {
		t.Fatal(ok, err)
	}
	select {
	case <-b.wake:
	default:
	}
	if err := b.postDigest(ctx, digestIn{On: true, Dests: []string{"slack"}}, "2950", now.Add(-7*24*time.Hour), now, b.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if rc.slack.Load() != 0 {
		t.Fatal("posted while another held the claim")
	}
	if !b.slackTried.IsZero() {
		t.Error("the next try waits an hour")
	}
	if err := a.l.Unclaim(ctx, "digest-post:2950"); err != nil {
		t.Fatal(err)
	}
	if err := b.postDigest(ctx, digestIn{On: true, Dests: []string{"slack"}}, "2950", now.Add(-7*24*time.Hour), now, b.cfg.Options); err != nil {
		t.Fatal(err)
	}
	if rc.slack.Load() != 1 {
		t.Errorf("the claim over, posted %d times", rc.slack.Load())
	}
}

// Settings changed on a follower ask the leader to judge again through
// the ledger alone: the follower, leading later, doesn't judge again what
// was judged.
func TestReplicasRejudgeAskedOnAFollower(t *testing.T) {
	f := sharedLedger(t)
	rc := newSink(t)
	a, b := replica(t, f, rc, nil), replica(t, f, rc, nil)
	ctx := context.Background()
	b.askRejudge(ctx)
	if b.rejudge.Load() {
		t.Error("the follower keeps the ask for itself, to judge again once it leads")
	}
	if _, asked := a.rejudgeAsked(ctx); !asked {
		t.Fatal("the leader isn't asked")
	}
	a.jobs(ctx, false)
	if _, asked := b.rejudgeAsked(ctx); asked {
		t.Error("asked still, once judged")
	}
}

// What the leader's work fails with as its lease ends is stepping down,
// not a failure that stops serve.
func TestReplicasSteppingDown(t *testing.T) {
	f := sharedLedger(t)
	quickReplicas(t)
	rc := newSink(t)
	a := replica(t, f, rc, nil)
	got := make(chan context.Context, 1)
	ectx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.l.Elect(ectx, ledger.Elected{Version: "test"}, func(c context.Context) {
		got <- c
		<-c.Done()
	})
	var lctx context.Context
	select {
	case lctx = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("never led")
	}
	for _, c := range []struct {
		ctx  context.Context
		err  error
		down bool
	}{
		{lctx, ledger.ErrNotLeading, true},
		{ledger.Lapsed(context.WithoutCancel(lctx)), fmt.Errorf("a write: %w", errors.New("x")), true},
		{lctx, errors.New("the rules' history doesn't read"), false},
	} {
		if got := steppingDown(c.ctx, c.err); got != c.down {
			t.Errorf("%v: stepping down %v, want %v", c.err, got, c.down)
		}
	}
}

// With replicas, a session is refused when who signed out can't be read:
// another replica may have kept a sign-out this one hasn't read.
func TestReplicasSignOutsUnreadRefuse(t *testing.T) {
	s, _ := oidcServer(t, nil)
	if !s.l.Shared() {
		t.Skip("the ledgers are SQLite's")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !s.signedOut(ctx, "ed@example.com", s.now().UnixMilli()) {
		t.Error("a session taken though who signed out couldn't be read")
	}
	if s.signedOut(context.Background(), "ed@example.com", s.now().UnixMilli()) {
		t.Error("a session refused, who signed out read")
	}
}

// A sign-out made while who signed out can't be read (here the request's
// context is gone; a database that answers the write but not the read
// alike) is kept: at once on this replica, which refuses a copy of the
// cookie, and in the ledger once it can be, for the others.
func TestReplicasSignOutKeptWhenUnread(t *testing.T) {
	was := keepAgain
	keepAgain = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond}
	t.Cleanup(func() { keepAgain = was })
	s, idp := oidcServer(t, nil)
	if !s.l.Shared() {
		t.Skip("the ledgers are SQLite's")
	}
	h := s.Handler()
	b := newBrowser(t, h)
	w := b.do("GET", "/auth/login?next=/", nil)
	back := idp.Authorize(t, w.Header().Get("Location"), claimsOf("ed@example.com"))
	if w := b.do("GET", back, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("signed in: %d", w.Code)
	}
	copyOf := map[string]*http.Cookie{}
	for k, v := range b.cookies {
		c := *v
		copyOf[k] = &c
	}
	other := newBrowser(t, h)
	other.cookies = copyOf
	if code := other.do("GET", "/api/v1/runs", nil).Code; code != http.StatusOK {
		t.Fatalf("a copy of the cookie before the sign-out: %d", code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("POST", "/auth/logout", nil).WithContext(ctx)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	s.logout(httptest.NewRecorder(), r)
	if code := other.do("GET", "/api/v1/runs", nil).Code; code != http.StatusUnauthorized {
		t.Errorf("a copy of the cookie after a sign-out made while sign-outs couldn't be read: %d, want 401", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := s.l.Meta(context.Background(), metaSignedOut)
		if err == nil && strings.Contains(raw, "ed@example.com") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sign-out never reached the ledger, for the other replicas: %q %v", raw, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A stranger the identity provider signs in, whom no role lets in, keeps
// nothing in the ledger: the state is used once among the replicas only
// for someone a role lets in. With a provider anyone can sign in to,
// strangers can't fill it, and refuse everyone their sign-in.
func TestReplicasStrangersKeepNoSignInStates(t *testing.T) {
	s, idp := oidcServer(t, nil)
	if !s.l.Shared() {
		t.Skip("the ledgers are SQLite's")
	}
	h := s.Handler()
	c := pgConn(t, s)
	signIn := func(who string) *browser {
		b := newBrowser(t, h)
		w := b.do("GET", "/auth/login?next=/", nil)
		back := idp.Authorize(t, w.Header().Get("Location"), claimsOf(who))
		b.do("GET", back, nil)
		return b
	}
	count := func() int {
		var n int
		if err := c.QueryRow(context.Background(), "SELECT count(*) FROM replica_once WHERE key LIKE 'once:signin:%'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for i := range 20 {
		b := signIn("mallory@elsewhere.invalid")
		if i == 0 {
			if code := b.do("GET", "/api/v1/runs", nil).Code; code != http.StatusForbidden {
				t.Fatalf("the stranger's session on the API: %d, want 403", code)
			}
		}
	}
	if n := count(); n > 0 {
		t.Errorf("a stranger no role lets in kept %d sign-in states in the ledger", n)
	}
	signIn("ed@example.com")
	if n := count(); n != 1 {
		t.Errorf("someone a role lets in kept %d sign-in states, want 1", n)
	}
}

// With the ledger's uses once at their bound, a sign-in is still let in,
// its state checked by this replica's own set: no one is refused their
// sign-in until the uses over are swept.
func TestReplicasSignInWhenStatesFull(t *testing.T) {
	s, idp := oidcServer(t, nil)
	if !s.l.Shared() {
		t.Skip("the ledgers are SQLite's")
	}
	c := pgConn(t, s)
	if _, err := c.Exec(context.Background(), `INSERT INTO replica_once (key, until)
		SELECT 'once:test:' || n, 9999999999 FROM generate_series(1, 100000) AS n`); err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t, s.Handler())
	w := b.do("GET", "/auth/login?next=/", nil)
	back := idp.Authorize(t, w.Header().Get("Location"), claimsOf("ed@example.com"))
	if w := b.do("GET", back, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("a sign-in with the uses once at their bound: %d", w.Code)
	}
	if code := b.do("GET", "/api/v1/runs", nil).Code; code != http.StatusOK {
		t.Errorf("the session signed in with the uses once at their bound: %d", code)
	}
}

// A leader whose work makes no progress for longer than stepWithin, in a
// round, gives its lease up: stuck says so. A job begun is progress, and
// so is a request asked of a history, or a rule replayed, in the leader's
// context.
func TestReplicasStuckWork(t *testing.T) {
	was := stepWithin
	if was != 15*time.Minute {
		t.Errorf("stepWithin is %v: replicas.md and the CHANGELOG say 15 min", was)
	}
	stepWithin = 100 * time.Millisecond
	t.Cleanup(func() { stepWithin = was })
	f := sharedLedger(t)
	rc := newSink(t)
	a := replica(t, f, rc, nil)
	ctx := context.Background()
	if a.stuck() {
		t.Fatal("idle, taken for stuck")
	}
	a.still(ctx)
	if a.stuck() {
		t.Fatal("a job begun, taken for stuck")
	}
	time.Sleep(2 * stepWithin)
	if !a.stuck() {
		t.Error("a job begun and no progress since, past stepWithin, not taken for stuck")
	}
	a.still(ctx)
	for range 6 {
		time.Sleep(stepWithin / 2)
		a.asked.Requests.Add(1)
		if a.stuck() {
			t.Fatal("a request asked each half of stepWithin, taken for stuck")
		}
	}
	for range 6 {
		time.Sleep(stepWithin / 2)
		a.replayed.Done.Add(1)
		if a.stuck() {
			t.Fatal("a rule replayed each half of stepWithin, taken for stuck")
		}
	}
	time.Sleep(2 * stepWithin)
	if !a.stuck() {
		t.Error("no progress since the last rule replayed, past stepWithin, not taken for stuck")
	}
	a.idle()
	if a.stuck() {
		t.Error("idle again, taken for stuck")
	}
	// One that stops leading in the middle of a round has no step under
	// way: else, past stepWithin, it would be taken for stuck, and ask for
	// no lease again.
	a.still(ctx)
	gone, cancel := context.WithCancel(ctx)
	cancel()
	a.lead(gone, make(chan error, 1))
	time.Sleep(2 * stepWithin)
	if a.stuck() {
		t.Error("a replica that stopped leading in the middle of a round taken for stuck")
	}
}

// Two Postgres ledgers in one process each have a version 1 of their own:
// what a version holds is parsed for its ledger, not taken from another's
// kept under the same id.
func TestReplicasVersionsParsedPerLedger(t *testing.T) {
	rc := newSink(t)
	ctx := context.Background()
	for i, name := range []string{"Checkout", "Search"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte("groups:\n  - name: shop\n    rules:\n      - alert: "+name+"\n        expr: errors > 5\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		s := replica(t, sharedLedger(t), rc, func(c *Config) { c.RulesPath = dir })
		raw := ""
		if err := s.look(ctx, &raw); err != nil {
			t.Fatal(err)
		}
		vs, err := s.versions(ctx, now.Add(-time.Hour), now)
		if err != nil || len(vs) != 1 || len(vs[0].Rules) != 1 || vs[0].Rules[0].Name != name {
			t.Errorf("ledger %d's version: %+v %v, want its rule %s", i, vs, err, name)
		}
	}
}

// tardy is a history that answers each request after a while, with
// nothing, counting them.
type tardy struct {
	d     time.Duration
	asked atomic.Int64
}

func (h *tardy) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	h.asked.Add(1)
	time.Sleep(h.d)
	return nil, nil
}

// A round longer than stepWithin, in a piece of a run, isn't taken for
// stuck while it makes progress: each rule replayed is, in the leader's
// context. Here each day's piece replays twelve rules, one at a time, each
// answered after 100 ms, while stepWithin is 500 ms; the leader keeps its
// lease, at the same term, to the round's end.
func TestReplicasSlowRoundIsNoStall(t *testing.T) {
	was := stepWithin
	stepWithin = 500 * time.Millisecond
	t.Cleanup(func() { stepWithin = was })
	f := sharedLedger(t)
	quickReplicas(t)
	rc := newSink(t)
	dir := t.TempDir()
	var rs strings.Builder
	rs.WriteString("groups:\n  - name: shop\n    rules:\n")
	for i := range 12 {
		fmt.Fprintf(&rs, "      - alert: Slow%d\n        expr: slow%d > 1\n", i, i)
	}
	if err := os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(rs.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	change := func(c *Config) { c.RulesPath, c.Parallel, c.Impact = dir, 1, "" }
	h := &tardy{d: 100 * time.Millisecond}
	a := replicaOn(t, f, rc, h, change)
	running(t, a)
	until(t, 10*time.Second, "a leads", a.l.Leading)
	began := time.Now()
	p, _ := a.l.Leader()
	b := replicaOn(t, f, rc, h, change)
	running(t, b)
	for !a.ready.Load() {
		if b.l.Leading() {
			t.Fatalf("another led %v into a slow round that made progress all along", time.Since(began))
		}
		if time.Since(began) > 30*time.Second {
			t.Fatal("the round never ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
	took := time.Since(began)
	if n := h.asked.Load(); n < 36 || took < 2*stepWithin {
		a.status.Lock()
		what, why, runs := a.status.errWhat, a.status.err, a.status.runs
		a.status.Unlock()
		t.Fatalf("the round asked %d times over %v, short of a slow round: %s %s, runs %v", n, took, what, why, runs)
	}
	if q, ok := a.l.Leader(); !ok || !q.Self || q.Term != p.Term {
		t.Errorf("after a slow round: %+v %v, want this replica leading at term %d still", q, ok, p.Term)
	}
}

// stalling is a history that stops answering once held, as one cut off
// does, whatever its context says, until let go; stalled is when it was
// first asked while held.
type stalling struct {
	history
	held    chan struct{}
	mu      sync.Mutex
	stalled time.Time
}

func (h *stalling) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	select {
	case <-h.held:
	default:
		h.mu.Lock()
		if h.stalled.IsZero() {
			h.stalled = time.Now()
		}
		h.mu.Unlock()
		<-h.held
	}
	return h.history.QueryRange(ctx, expr, start, end, step)
}

// A leader whose job is stuck (here, its history stopped answering, and
// its request doesn't return) gives its lease up stepWithin after its last
// progress, at its next renewal, and another leads at its next campaign:
// the bound replicas.md gives.
func TestReplicasStuckJobHandsOver(t *testing.T) {
	was := stepWithin
	stepWithin = time.Second
	t.Cleanup(func() { stepWithin = was })
	f := sharedLedger(t)
	q := quickReplicas(t)
	rc := newSink(t)
	h := &stalling{held: make(chan struct{})}
	var let sync.Once
	t.Cleanup(func() { let.Do(func() { close(h.held) }) })
	a := replicaOn(t, f, rc, h, nil)
	running(t, a)
	until(t, 10*time.Second, "a leads", a.l.Leading)
	b := replicaOn(t, f, rc, h, nil)
	running(t, b)
	until(t, 10*time.Second, "a's job is stuck", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return !h.stalled.IsZero()
	})
	until(t, 10*time.Second, "b leads", b.l.Leading)
	h.mu.Lock()
	took := time.Since(h.stalled)
	h.mu.Unlock()
	let.Do(func() { close(h.held) })
	if bound := stepWithin + q.Renew + q.Campaign + 400*time.Millisecond; took > bound {
		t.Errorf("b led %v after a's job was stuck, past %v", took, bound)
	}
	t.Logf("b led %v after a's job was stuck (stepWithin %v)", took, stepWithin)
}

// freshOIDC is a server's sign-in settings, for another server: what
// signing in readies is the server's own.
func freshOIDC(s *Server) *signin.OIDC {
	return &signin.OIDC{Issuer: s.cfg.OIDC.Issuer, ClientID: s.cfg.OIDC.ClientID, SecretFile: s.cfg.OIDC.SecretFile,
		Scopes: s.cfg.OIDC.Scopes, Claims: s.cfg.OIDC.Claims}
}
