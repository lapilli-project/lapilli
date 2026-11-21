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

// ask is a backtest asked for, for the tests.
func ask(id, trust string, pr int64, token string) BacktestAsk {
	return BacktestAsk{Backtest: Backtest{ID: id, Trust: trust, Repository: "example/alerts", PullRequest: pr, Commit: "0123abcd", Rules: 2, Requests: 80, KeyHash: "k-" + id},
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
		used, full, err := l.BacktestRoom(ctx, trust, token, "example/alerts", pr, queue, 3, at)
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
		for _, q := range append(slices.Concat(undo029, undo028, undo027, undo026, undo025, undo024, undo023), "UPDATE schema_version SET version = 22") {
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
		for _, q := range append(slices.Concat(undo029, undo028, undo027, undo026, undo025, undo024, undo023), "PRAGMA user_version = 22") {
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
