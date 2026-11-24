package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
)

// ask is a backtest asked for, for the tests, each of a commit of its own.
func ask(id, trust string, pr int64, token string) BacktestAsk {
	return BacktestAsk{Backtest: Backtest{ID: id, Trust: trust, Repository: "example/alerts", PullRequest: pr, Commit: "commit-" + id, Rules: 2, Requests: 80, KeyHash: "k-" + id},
		Body: []byte(`{"after":[]}` + id), Token: token, BodyHash: "h-" + id, Until: t0.Add(2 * time.Hour), Queue: 20, PerTrust: 3, Supersede: pr > 0}
}

// A backtest waits, is taken by the leader, says what it asked, and ends
// with what it came to, its request let go: each step as kept, in SQLite
// or, with LAPILLI_TEST_POSTGRES, in Postgres.
func TestBacktestLife(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	a, again, sup, err := l.AskBacktest(ctx, ask("b1", "alerts", 7, "jti-1"), t0)
	if err != nil || again || sup != 0 || a.State != BacktestWaiting || a.Position != 0 || a.Requests != 80 {
		t.Fatalf("%+v %t %d %v", a, again, sup, err)
	}
	b, _, _, err := l.AskBacktest(ctx, ask("b2", "alerts", 8, "jti-2"), t0.Add(time.Second))
	if err != nil || b.Position != 1 {
		t.Fatalf("the second asked: %+v %v", b, err)
	}
	got, body, ok, err := l.StartBacktest(ctx, t0.Add(time.Minute))
	if err != nil || !ok || got.ID != "b1" || got.State != BacktestRunning || string(body) != `{"after":[]}b1` {
		t.Fatalf("started %+v %q %t %v", got, body, ok, err)
	}
	if w, _, _ := l.Backtest(ctx, "b2"); w.Position != 1 {
		t.Errorf("b2 waits behind one running: position %d", w.Position)
	}
	if cancel, err := l.BacktestMade(ctx, "b1", 40); err != nil || cancel {
		t.Fatal(cancel, err)
	}
	if err := l.EndBacktest(ctx, "b1", BacktestDone, "", 80, []byte(`{"markdown":"x"}`), t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	done, ok, err := l.Backtest(ctx, "b1")
	if err != nil || !ok || done.State != BacktestDone || done.Made != 80 || !done.Ended.Equal(t0.Add(2*time.Minute)) || done.KeyHash != "k-b1" {
		t.Fatalf("%+v %t %v", done, ok, err)
	}
	if res, err := l.BacktestResult(ctx, "b1"); err != nil || string(res) != `{"markdown":"x"}` {
		t.Fatalf("%q %v", res, err)
	}
	var left []byte
	l.r.QueryRowContext(ctx, "SELECT body FROM backtests WHERE id = ?", "b1").Scan(&left)
	if left != nil {
		t.Error("the request is kept once it ended")
	}
	// Ending one that no longer runs changes nothing.
	if err := l.EndBacktest(ctx, "b1", BacktestFailed, "late", 1, nil, t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := l.Backtest(ctx, "b1"); again.State != BacktestDone {
		t.Errorf("ended twice: %+v", again)
	}
	if n, err := l.BacktestsWaiting(ctx); err != nil || n["alerts"] != 1 {
		t.Errorf("waiting %v %v", n, err)
	}
	if _, ok, _ := l.Backtest(ctx, "nope"); ok {
		t.Error("a backtest no one asked for")
	}
	list, err := l.Backtests(ctx, 10)
	if err != nil || len(list) != 2 || list[0].ID != "b2" {
		t.Errorf("%+v %v", list, err)
	}
}

// A token starts one backtest: with the body it came with again, the same
// one, again; with another, ErrBacktestToken, and nothing is kept. Its key
// is the check: replicas given it at once with two bodies start one of
// them, whichever writes first.
func TestBacktestTokenOnce(t *testing.T) {
	ctx := context.Background()
	where := place(t)
	l, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, _, _, err := l.AskBacktest(ctx, ask("b1", "alerts", 7, "jti-1"), t0); err != nil {
		t.Fatal(err)
	}
	same := ask("b9", "alerts", 7, "jti-1")
	same.BodyHash = "h-b1"
	b, again, _, err := l.AskBacktest(ctx, same, t0.Add(time.Second))
	if err != nil || !again || b.ID != "b1" {
		t.Fatalf("the same body again: %+v %t %v", b, again, err)
	}
	if _, _, _, err := l.AskBacktest(ctx, ask("b2", "alerts", 7, "jti-1"), t0.Add(time.Second)); !errors.Is(err, ErrBacktestToken) {
		t.Fatalf("another body: %v", err)
	}
	if _, ok, _ := l.Backtest(ctx, "b2"); ok {
		t.Error("a backtest kept for a token used before")
	}
	// The same jti under another entry is another token.
	if _, _, _, err := l.AskBacktest(ctx, ask("b3", "gitlab", 7, "jti-1"), t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// At once, from two ledgers on the same database (two replicas'), or
	// two writers of one: one starts.
	other := l
	if strings.HasPrefix(where, "postgres") {
		if other, err = openAt(ctx, where, time.UTC); err != nil {
			t.Fatal(err)
		}
		defer other.Close()
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, led := range []*Ledger{l, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, errs[i] = led.AskBacktest(ctx, ask(fmt.Sprint("race", i), "alerts", int64(20+i), "jti-race"), t0.Add(2*time.Second))
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) || !errors.Is(errors.Join(errs...), ErrBacktestToken) {
		t.Errorf("one token, two bodies, at once: %v", errs)
	}
}

// The queue holds Queue at most, PerTrust of an entry, and one of a pull
// request: a newer commit's supersedes the one waiting. Past them nothing
// is kept, and the token that asked is free to ask again.
// BacktestRoom says before a body is read what AskBacktest would say of
// the token and the queue: a token that started one, an entry with as many
// waiting as it may have, less those a newer commit of the pull request
// would supersede, and the queue full in all.
func TestBacktestRoom(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	room := func(trust, token string, pr int64, queue int, at time.Time) (bool, bool) {
		t.Helper()
		used, full, err := l.BacktestRoom(ctx, trust, token, "", "example/alerts", "", pr, queue, 3, at)
		if err != nil {
			t.Fatal(err)
		}
		return used, full
	}
	if used, full := room("alerts", "t0", -1, 20, t0); used || full {
		t.Errorf("an empty queue: used %v, full %v", used, full)
	}
	for i := range 3 {
		if _, _, _, err := l.AskBacktest(ctx, ask(fmt.Sprint("a", i), "alerts", int64(i+1), fmt.Sprint("t", i)), t0); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		trust, token string
		pr           int64
		queue        int
		at           time.Time
		used, full   bool
	}{
		{"alerts", "t1", -1, 20, t0, true, false},                    // its token started one
		{"alerts", "t1", -1, 20, t0.Add(3 * time.Hour), false, true}, // kept no longer, and the entry's three wait
		{"alerts", "t9", -1, 20, t0, false, true},                    // the entry's three wait
		{"alerts", "t9", 2, 20, t0, false, false},                    // a newer commit of 2 would supersede it
		{"alerts", "t9", 7, 20, t0, false, true},                     // of another pull request
		{"gitlab", "t1", -1, 20, t0, false, false},                   // another entry's token, and room
		{"gitlab", "g1", -1, 3, t0, false, true},                     // the queue full in all
		{"plain", "", -1, 20, t0, false, false},                      // a plain token
	} {
		if used, full := room(c.trust, c.token, c.pr, c.queue, c.at); used != c.used || full != c.full {
			t.Errorf("%+v: used %v, full %v", c, used, full)
		}
	}
	// As AskBacktest says.
	if _, _, _, err := l.AskBacktest(ctx, ask("a9", "alerts", 7, "t9"), t0); !errors.Is(err, ErrBacktestsFull) {
		t.Errorf("AskBacktest of the entry's fourth: %v", err)
	}
}

func TestBacktestQueue(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := range 3 {
		if _, _, _, err := l.AskBacktest(ctx, ask(fmt.Sprint("a", i), "alerts", int64(i+1), fmt.Sprint("t", i)), t0); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := l.AskBacktest(ctx, ask("a3", "alerts", 4, "t3"), t0); !errors.Is(err, ErrBacktestsFull) {
		t.Fatalf("a fourth of one entry: %v", err)
	}
	// A pull request's number the job said, not its token, supersedes
	// nothing.
	said := ask("a5", "alerts", 1, "t5")
	said.Supersede = false
	if _, _, sup, err := l.AskBacktest(ctx, said, t0); !errors.Is(err, ErrBacktestsFull) || sup != 0 {
		t.Errorf("a number the job said: %d %v", sup, err)
	}
	// The token that was refused didn't start anything, and may ask again.
	newer := ask("a4", "alerts", 2, "t3")
	b, _, sup, err := l.AskBacktest(ctx, newer, t0.Add(time.Second))
	if err != nil || sup != 1 || b.Position != 2 {
		t.Fatalf("a newer commit of pull request 2: %+v %d %v", b, sup, err)
	}
	if old, _, _ := l.Backtest(ctx, "a1"); old.State != BacktestSuperseded || old.Ended.IsZero() {
		t.Errorf("the older commit: %+v", old)
	}
	small := ask("g1", "gitlab", 1, "g1")
	small.Queue = 3
	if _, _, _, err := l.AskBacktest(ctx, small, t0); !errors.Is(err, ErrBacktestsFull) {
		t.Errorf("past the queue's bound: %v", err)
	}
	// A plain token's asks are tied to none.
	for i := range 2 {
		p := ask(fmt.Sprint("p", i), "plain", 0, "")
		if _, again, _, err := l.AskBacktest(ctx, p, t0); err != nil || again {
			t.Fatal(again, err)
		}
	}
}

// What is kept goes: a token's row after its time, a backtest a day after
// it ended, the oldest ended past 200; one waiting stays. A backtest the
// leader before was running when it ended waits again, and is run first.
// A job cancels one waiting at once, one running by telling it.
func TestBacktestKept(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	end := func(id string, at time.Time) {
		t.Helper()
		if _, _, ok, err := l.StartBacktest(ctx, at); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := l.EndBacktest(ctx, id, BacktestDone, "", 1, nil, at); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 205 {
		id := fmt.Sprintf("e%03d", i)
		a := ask(id, "alerts", 0, "")
		a.Queue, a.PerTrust = 1000, 1000
		if _, _, _, err := l.AskBacktest(ctx, a, t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
		end(id, t0.Add(time.Duration(i)*time.Second))
	}
	if _, _, _, err := l.AskBacktest(ctx, ask("w", "alerts", 0, "jti-w"), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	all, _ := l.Backtests(ctx, 1000)
	if len(all) != 200 || all[0].ID != "w" || all[len(all)-1].ID != "e006" {
		t.Fatalf("%d kept, from %s to %s", len(all), all[0].ID, all[len(all)-1].ID)
	}
	// Their keys' hashes, by id, the latest asked first, as many as asked.
	keys, err := l.BacktestKeys(ctx, 3)
	if err != nil || !maps.Equal(keys, map[string]string{"w": "k-w", "e204": "k-e204", "e203": "k-e203"}) {
		t.Errorf("the latest keys: %v %v", keys, err)
	}
	if keys, err := l.BacktestKeys(ctx, 2048); err != nil || len(keys) != 200 || keys["e006"] != "k-e006" {
		t.Errorf("the keys of all kept: %d %v", len(keys), err)
	}
	// A day on, the ended go; the one waiting and its token's row stay
	// until theirs.
	if _, _, _, err := l.AskBacktest(ctx, ask("x", "alerts", 0, ""), t0.Add(25*time.Hour+time.Second)); err != nil {
		t.Fatal(err)
	}
	all, _ = l.Backtests(ctx, 1000)
	if ids := idsOf(all); !slices.Equal(ids, []string{"x", "w"}) {
		t.Errorf("a day on: %v", ids)
	}
	var tokens int
	l.r.QueryRowContext(ctx, "SELECT count(*) FROM backtest_tokens").Scan(&tokens)
	if tokens != 0 {
		t.Errorf("%d tokens kept past their time", tokens)
	}
	// Running when its leader ended: waiting again, run first.
	b, _, _, err := l.StartBacktest(ctx, t0.Add(26*time.Hour))
	if err != nil || b.ID != "w" {
		t.Fatal(b, err)
	}
	if b, _, _, err := l.StartBacktest(ctx, t0.Add(27*time.Hour)); err != nil || b.ID != "w" || !b.Started.Equal(t0.Add(27*time.Hour)) {
		t.Fatalf("the next leader: %+v %v", b, err)
	}
	if ok, err := l.CancelBacktest(ctx, "w", t0.Add(27*time.Hour)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if cancel, err := l.BacktestMade(ctx, "w", 3); err != nil || !cancel {
		t.Errorf("a running one cancelled isn't told: %t %v", cancel, err)
	}
	if ok, err := l.CancelBacktest(ctx, "x", t0.Add(27*time.Hour)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if x, _, _ := l.Backtest(ctx, "x"); x.State != BacktestCancelled {
		t.Errorf("one waiting cancelled: %+v", x)
	}
	if ok, _ := l.CancelBacktest(ctx, "x", t0.Add(27*time.Hour)); ok {
		t.Error("cancelled twice")
	}
}

func idsOf(bs []Backtest) []string {
	var out []string
	for _, b := range bs {
		out = append(out, b.ID)
	}
	return out
}

// A row holding more than was kept there, as one written otherwise than
// by AskBacktest and EndBacktest would, is read no further than the bound:
// a request past 4 MiB, a result past 1 MiB, once ungzipped.
func TestBacktestReadBounded(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, _, _, err := l.AskBacktest(ctx, ask("big", "alerts", 0, ""), t0); err != nil {
		t.Fatal(err)
	}
	huge, _ := gzBytes(bytes.Repeat([]byte("x"), MaxBacktestBody+1))
	if _, err := l.w.ExecContext(ctx, "UPDATE backtests SET body = ?, result = ? WHERE id = ?", huge, huge, "big"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := l.StartBacktest(ctx, t0); err == nil {
		t.Error("a request past 4 MiB was read")
	}
	if _, err := l.BacktestResult(ctx, "big"); err == nil {
		t.Error("a result past 1 MiB was read")
	}
	if _, _, _, err := l.AskBacktest(ctx, BacktestAsk{Backtest: Backtest{ID: "over"}, Body: make([]byte, MaxBacktestBody+1)}, t0); err == nil {
		t.Error("a request past 4 MiB was kept")
	}
	if err := l.EndBacktest(ctx, "big", BacktestDone, "", 0, make([]byte, MaxBacktestResult+1), t0); err == nil {
		t.Error("a result past 1 MiB was kept")
	}
}

// A ledger at 022, in SQLite or, with LAPILLI_TEST_POSTGRES, in Postgres,
// migrates to 023 with its firings judged the same, and no backtest: one
// can then be asked for.
func TestMigrate023(t *testing.T) {
	ctx := context.Background()
	where := place(t)
	l, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	record(t, l, t0, t0.Add(24*time.Hour), []firing.Firing{
		{Rule: "Errors", Labels: map[string]string{"service": "checkout"}, Start: t0.Add(2 * time.Hour), End: t0.Add(2*time.Hour + 10*time.Minute)}})
	if _, err := l.SyncFile(ctx, []incident.Incident{{ID: "INC-1", Start: t0.Add(2 * time.Hour), End: t0.Add(3 * time.Hour),
		Labels: incident.Labels{"service": {"checkout"}}}}, opts); err != nil {
		t.Fatal(err)
	}
	before, err := l.Report(ctx, t0, t0.Add(24*time.Hour), opts, true)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if strings.HasPrefix(where, "postgres") {
		c, err := pgx.Connect(ctx, where)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(slices.Concat(undo032, undo031, undo030, undo029, undo028, undo027, undo026, undo025, undo024, undo023), "UPDATE schema_version SET version = 22") {
			if _, err := c.Exec(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		c.Close(ctx)
	} else {
		l, err := Open(ctx, where, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(slices.Concat(undo032, undo031, undo030, undo029, undo028, undo027, undo026, undo025, undo024, undo023), "PRAGMA user_version = 22") {
			if _, err := l.w.ExecContext(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		l.Close()
	}
	l2, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatalf("migrating to 023: %v", err)
	}
	defer l2.Close()
	after, err := l2.Report(ctx, t0, t0.Add(24*time.Hour), opts, true)
	if err != nil || len(after.Firings) != 1 || after.Rules[0].White != before.Rules[0].White {
		t.Errorf("the firings after migrating: %+v %v", after, err)
	}
	if bs, err := l2.Backtests(ctx, 10); err != nil || len(bs) != 0 {
		t.Errorf("%+v %v", bs, err)
	}
	if _, _, _, err := l2.AskBacktest(ctx, ask("b1", "alerts", 1, "j"), t0); err != nil {
		t.Error(err)
	}
	var v int
	if strings.HasPrefix(where, "postgres") {
		l2.r.QueryRowContext(ctx, "SELECT version FROM schema_version").Scan(&v)
	} else {
		l2.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	}
	if want := schemaVersion(t); v != want || want < 23 {
		t.Errorf("migrated to %d, want %d", v, want)
	}
}

// A backtest started twice whose leader stopped each time isn't started a
// third: it fails, saying so. And the next taken is of the entry that
// started one least lately: one entry's many don't keep another's few
// waiting.
func TestBacktestAttemptsAndTurns(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, _, _, err := l.AskBacktest(ctx, ask("crash", "alerts", 0, ""), t0); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if b, _, ok, err := l.StartBacktest(ctx, t0.Add(time.Duration(i)*time.Minute)); err != nil || !ok || b.ID != "crash" {
			t.Fatalf("start %d: %+v %t %v", i+1, b, ok, err)
		}
	}
	if b, _, ok, err := l.StartBacktest(ctx, t0.Add(3*time.Minute)); err != nil || ok {
		t.Fatalf("a third start: %+v %t %v", b, ok, err)
	}
	if c, _, _ := l.Backtest(ctx, "crash"); c.State != BacktestFailed || c.Error != BacktestCrashed {
		t.Errorf("after two: %+v", c)
	}
	for i := range 3 {
		a := ask(fmt.Sprint("a", i), "many", 0, "")
		if _, _, _, err := l.AskBacktest(ctx, a, t0.Add(time.Duration(10+i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := l.AskBacktest(ctx, ask("b0", "few", 0, ""), t0.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var order []string
	for i := range 4 {
		b, _, ok, err := l.StartBacktest(ctx, t0.Add(time.Duration(30+i)*time.Minute))
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		order = append(order, b.ID)
		if err := l.EndBacktest(ctx, b.ID, BacktestDone, "", 0, nil, t0.Add(time.Duration(30+i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(order, []string{"a0", "b0", "a1", "a2"}) {
		t.Errorf("taken in the order %v", order)
	}
}

// PruneBacktests lets go of what ended a day ago, as the leader's rounds
// do, whether or not the port is on.
func TestPruneBacktests(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.PruneBacktests(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := l.AskBacktest(ctx, ask("old", "alerts", 0, "jti"), t0); err != nil {
		t.Fatal(err)
	}
	l.StartBacktest(ctx, t0)
	l.EndBacktest(ctx, "old", BacktestDone, "", 0, nil, t0)
	if err := l.PruneBacktests(ctx, t0.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := l.Backtest(ctx, "old"); ok {
		t.Error("a backtest ended a day ago is kept")
	}
}

// askOf is ask, of a repository by its id and its name.
func askOf(id, repoID, repo string, pr int64) BacktestAsk {
	a := ask(id, "alerts", pr, "t-"+id)
	a.RepositoryID, a.Repository = repoID, repo
	return a
}

// A newer commit's backtest takes the place of the one waiting for its
// repository by the repository's id: one renamed since is still itself,
// and two repositories an entry pins never take each other's place,
// whatever they are named, one name after the other or none kept. A
// backtest kept with no id, asked before ids were kept, is matched by its
// repository's name, as it was.
func TestBacktestSupersedeByID(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	asked := func(a BacktestAsk, want int, what string) {
		t.Helper()
		a.Queue, a.PerTrust = 50, 50
		if _, _, sup, err := l.AskBacktest(ctx, a, t0); err != nil || sup != want {
			t.Errorf("%s: %d superseded, want %d: %v", what, sup, want, err)
		}
	}
	room := func(repoID, repo string, pr int64, perTrust int) bool {
		t.Helper()
		_, full, err := l.BacktestRoom(ctx, "alerts", "t-room", repoID, repo, "", pr, 50, perTrust, t0)
		if err != nil {
			t.Fatal(err)
		}
		return full
	}
	waiting := func() int {
		t.Helper()
		by, err := l.BacktestsWaiting(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return by["alerts"]
	}
	// Two repositories of one entry, pull request 5 of each, the second
	// under the first's name: renamed, and its name taken.
	asked(askOf("a1", "111", "example/alerts", 5), 0, "the first")
	asked(askOf("b1", "222", "example/alerts", 5), 0, "another repository of its name and pull request")
	if room("222", "example/alerts", 5, 2) || !room("333", "example/alerts", 5, 2) {
		t.Errorf("the room for a newer commit: its own one waiting would go, and no other's")
	}
	// The first, renamed, takes its own one's place, and not the other's.
	asked(askOf("a2", "111", "example/monitoring", 5), 1, "the first repository, renamed")
	if b, _, _ := l.Backtest(ctx, "a1"); b.State != BacktestSuperseded || b.RepositoryID != "111" {
		t.Errorf("the renamed repository's older commit: %+v", b)
	}
	if b, _, _ := l.Backtest(ctx, "b1"); b.State != BacktestWaiting {
		t.Errorf("the other repository's: %+v", b)
	}
	// Neither name kept: by their ids all the same.
	asked(askOf("c1", "111", "", 6), 0, "a name not kept")
	asked(askOf("d1", "222", "", 6), 0, "another repository's, its name not kept either")
	asked(askOf("c2", "111", "", 6), 1, "the first's newer commit, its name not kept")
	// One kept with no id, as before 031: by its name; not by a name of "".
	asked(askOf("o1", "", "example/alerts", 7), 0, "one kept with no id")
	asked(askOf("o2", "", "", 8), 0, "one kept with no id and no name")
	asked(askOf("n2", "111", "", 8), 0, "a newer commit with no name, of one with no id")
	asked(askOf("n3", "333", "example/other", 7), 0, "a newer commit of another name, of one with no id")
	if !room("999", "example/other", 7, waiting()) || room("999", "example/alerts", 7, waiting()) {
		t.Errorf("the room for a newer commit of one kept with no id: by its name")
	}
	asked(askOf("n1", "111", "example/alerts", 7), 1, "a newer commit, of one with no id, by its name")
	// And one with an id isn't matched by its name alone.
	asked(askOf("p1", "", "example/monitoring", 5), 0, "a token with no id, of the name of one kept with an id")
}

// A ledger at 030 with a backtest waiting, in SQLite or, with
// LAPILLI_TEST_POSTGRES, in Postgres, migrates to 031 with it as it was,
// its repository's id none: a newer commit takes its place by its
// repository's name, and is kept with its id.
func TestMigrate031(t *testing.T) {
	ctx := context.Background()
	where := place(t)
	l, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := l.AskBacktest(ctx, askOf("old", "", "example/alerts", 7), t0); err != nil {
		t.Fatal(err)
	}
	was, _, err := l.Backtest(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	l.Close()

	// Back to 030.
	if strings.HasPrefix(where, "postgres") {
		c, err := pgx.Connect(ctx, where)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(slices.Concat(undo032, undo031), "UPDATE schema_version SET version = 30") {
			if _, err := c.Exec(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		c.Close(ctx)
	} else {
		l, err := Open(ctx, where, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(slices.Concat(undo032, undo031), "PRAGMA user_version = 30") {
			if _, err := l.w.ExecContext(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		l.Close()
	}

	l2, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatalf("migrating to 031: %v", err)
	}
	defer l2.Close()
	if b, _, err := l2.Backtest(ctx, "old"); err != nil || b != was || b.RepositoryID != "" || b.State != BacktestWaiting {
		t.Errorf("after migrating: %+v, not %+v: %v", b, was, err)
	}
	// Another repository of the entry's doesn't take its place; its own
	// newer commit does, by its name.
	if _, _, sup, err := l2.AskBacktest(ctx, askOf("other", "222", "example/elsewhere", 7), t0); err != nil || sup != 0 {
		t.Errorf("another repository's pull request 7: %d %v", sup, err)
	}
	b, _, sup, err := l2.AskBacktest(ctx, askOf("new", "111", "example/alerts", 7), t0)
	if err != nil || sup != 1 || b.RepositoryID != "111" {
		t.Errorf("its newer commit after migrating: %+v %d %v", b, sup, err)
	}
	if old, _, _ := l2.Backtest(ctx, "old"); old.State != BacktestSuperseded {
		t.Errorf("the older commit: %+v", old)
	}
}

// askQueue is askOf, of a merge queue's run into branch: "" for a pull
// request's.
func askQueue(id, branch string, pr int64) BacktestAsk {
	a := askOf(id, "111", "example/alerts", pr)
	a.QueueBranch = branch
	return a
}

// A merge group's backtest takes the place of the one waiting for the
// same entry, repository, pull request and queue's branch: a group the
// queue built again. Never one of another pull request, of another
// branch's queue, nor a pull request's own backtest of the same number,
// whose newer commit takes no group's place either, even of one entry.
// BacktestRoom counts alike.
func TestBacktestSupersedeQueue(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	asked := func(a BacktestAsk, want int, what string) {
		t.Helper()
		a.Queue, a.PerTrust = 50, 50
		if _, _, sup, err := l.AskBacktest(ctx, a, t0); err != nil || sup != want {
			t.Errorf("%s: %d superseded, want %d: %v", what, sup, want, err)
		}
	}
	room := func(branch string, pr int64, perTrust int) bool {
		t.Helper()
		_, full, err := l.BacktestRoom(ctx, "alerts", "t-room", "111", "example/alerts", branch, pr, 50, perTrust, t0)
		if err != nil {
			t.Fatal(err)
		}
		return full
	}
	asked(askQueue("pr5", "", 5), 0, "pull request 5")
	asked(askQueue("g5", "main", 5), 0, "pull request 5's merge group, beside its own backtest")
	asked(askQueue("g6", "main", 6), 0, "another pull request's group")
	asked(askQueue("r5", "release", 5), 0, "pull request 5's group in another branch's queue")
	// Four wait: a group of 5 into main built again would leave three.
	if room("main", 5, 4) || room("", 5, 4) || !room("main", 7, 4) || !room("release/2.x", 5, 4) || !room("", 8, 4) {
		t.Errorf("the room for a group built again: its own one waiting would go, and no other's")
	}
	asked(askQueue("g5b", "main", 5), 1, "pull request 5's group, built again")
	asked(askQueue("pr5b", "", 5), 1, "pull request 5's newer commit")
	want := map[string]string{"pr5": BacktestSuperseded, "g5": BacktestSuperseded, "g6": BacktestWaiting, "r5": BacktestWaiting,
		"g5b": BacktestWaiting, "pr5b": BacktestWaiting}
	for id, state := range want {
		b, ok, err := l.Backtest(ctx, id)
		branch := map[bool]string{true: "", false: map[string]string{"g": "main", "r": "release"}[id[:1]]}[strings.HasPrefix(id, "pr")]
		if err != nil || !ok || b.State != state || b.QueueBranch != branch {
			t.Errorf("%s: %+v, want %s of %q: %v", id, b, state, branch, err)
		}
	}
	// A number the job said supersedes nothing, a group's as a pull
	// request's.
	said := askQueue("g5c", "main", 5)
	said.Supersede = false
	asked(said, 0, "pull request 5's group, by the job's word")
	// Two jobs of one run, each a KEY of its own, of the same commit: the
	// second takes no place of the first's, a group's as a pull request's.
	for _, branch := range []string{"main", ""} {
		first, second := askQueue("key-prom-"+branch, branch, 9), askQueue("key-loki-"+branch, branch, 9)
		second.Commit = first.Commit
		asked(first, 0, "the first job's, into "+branch)
		asked(second, 0, "the second job's, of the same commit, into "+branch)
		if b, _, _ := l.Backtest(ctx, first.ID); b.State != BacktestWaiting {
			t.Errorf("the first job's, into %q: %+v", branch, b)
		}
	}
}

// askRun is askQueue, of a GitHub Actions run: of workflow, its number
// run.
func askRun(id, branch string, pr int64, workflow string, run int64) BacktestAsk {
	a := askQueue(id, branch, pr)
	a.Workflow, a.RunNumber = workflow, run
	return a
}

// Of a workflow, a backtest takes the place only of one of an earlier run
// of it: a run that asks late (it waited for a runner while the queue
// built its group again, or a newer commit was pushed), or an earlier run
// run again, keeping its number, takes no newer run's place, a merge
// group's as a pull request's; nor does a run of another workflow. One
// kept with no workflow (asked before 032) is taken the place of as
// before, and so are those of a CI whose tokens say no run.
func TestBacktestSupersedeLaterRun(t *testing.T) {
	ctx := context.Background()
	l, err := openAt(ctx, place(t), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	asked := func(a BacktestAsk, want int, what string) {
		t.Helper()
		a.Queue, a.PerTrust = 50, 50
		if _, _, sup, err := l.AskBacktest(ctx, a, t0); err != nil || sup != want {
			t.Errorf("%s: %d superseded, want %d: %v", what, sup, want, err)
		}
	}
	const wf, other = "example/alerts/.github/workflows/backtest.yml", "example/alerts/.github/workflows/loki.yml"
	for _, branch := range []string{"main", ""} {
		pr := map[string]int64{"main": 5, "": 6}[branch]
		asked(askRun("new-"+branch, branch, pr, wf, 11), 0, "run 11, into "+branch)
		// Run 10 asks late, and run 10 again: neither takes run 11's place.
		asked(askRun("late-"+branch, branch, pr, wf, 10), 0, "run 10, late, into "+branch)
		asked(askRun("again-"+branch, branch, pr, wf, 10), 0, "run 10 run again, into "+branch)
		// Another workflow's run 99 takes no place of this one's.
		asked(askRun("other-"+branch, branch, pr, other, 99), 0, "another workflow's run, into "+branch)
		// Run 12 takes the place of each earlier run of its workflow.
		asked(askRun("newest-"+branch, branch, pr, wf, 12), 3, "run 12, into "+branch)
		for id, state := range map[string]string{"new-": BacktestSuperseded, "late-": BacktestSuperseded, "again-": BacktestSuperseded,
			"other-": BacktestWaiting, "newest-": BacktestWaiting} {
			if b, ok, err := l.Backtest(ctx, id+branch); err != nil || !ok || b.State != state {
				t.Errorf("%s%s: %+v, want %s: %v", id, branch, b, state, err)
			}
		}
		if b, _, _ := l.Backtest(ctx, "newest-"+branch); b.Workflow != wf || b.RunNumber != 12 {
			t.Errorf("kept %q, run %d", b.Workflow, b.RunNumber)
		}
	}
	// Kept with no workflow, as before 032: taken the place of by a run.
	asked(askQueue("before", "main", 7), 0, "one kept with no workflow")
	asked(askRun("run", "main", 7, wf, 1), 1, "a run's, after it")
	// A CI whose tokens say no run: by the commit alone, as before.
	asked(askQueue("no-run", "main", 8), 0, "of no run")
	asked(askQueue("no-run-newer", "main", 8), 1, "of no run, another commit")
}

// A ledger at 031 with a backtest waiting, in SQLite or, with
// LAPILLI_TEST_POSTGRES, in Postgres, migrates to 032 with it as it was,
// of no merge queue: a pull request's newer commit takes its place, and a
// merge group of the same number doesn't.
func TestMigrate032(t *testing.T) {
	ctx := context.Background()
	where := place(t)
	l, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := l.AskBacktest(ctx, askOf("old", "111", "example/alerts", 7), t0); err != nil {
		t.Fatal(err)
	}
	was, _, err := l.Backtest(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	l.Close()

	// Back to 031.
	if strings.HasPrefix(where, "postgres") {
		c, err := pgx.Connect(ctx, where)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(slices.Clone(undo032), "UPDATE schema_version SET version = 31") {
			if _, err := c.Exec(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		c.Close(ctx)
	} else {
		l, err := Open(ctx, where, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range append(slices.Clone(undo032), "PRAGMA user_version = 31") {
			if _, err := l.w.ExecContext(ctx, q); err != nil {
				t.Fatal(q, err)
			}
		}
		l.Close()
	}

	l2, err := openAt(ctx, where, time.UTC)
	if err != nil {
		t.Fatalf("migrating to 032: %v", err)
	}
	defer l2.Close()
	if b, _, err := l2.Backtest(ctx, "old"); err != nil || b != was || b.QueueBranch != "" || b.State != BacktestWaiting {
		t.Errorf("after migrating: %+v, not %+v: %v", b, was, err)
	}
	if _, _, sup, err := l2.AskBacktest(ctx, askQueue("group", "main", 7), t0); err != nil || sup != 0 {
		t.Errorf("pull request 7's merge group: %d %v", sup, err)
	}
	if _, _, sup, err := l2.AskBacktest(ctx, askOf("new", "111", "example/alerts", 7), t0); err != nil || sup != 1 {
		t.Errorf("its newer commit after migrating: %d %v", sup, err)
	}
	if old, _, _ := l2.Backtest(ctx, "old"); old.State != BacktestSuperseded {
		t.Errorf("the older commit: %+v", old)
	}
}
