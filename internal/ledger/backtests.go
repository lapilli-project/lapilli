package ledger

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"
)

// Pull request backtests, as lapilli serve's backtest port queues them and
// the leader runs them (docs/design/backtests.md): rows of the backtests
// table, and the tokens that started them (023.sql).

// The states of a backtest.
const (
	BacktestWaiting    = "waiting"
	BacktestRunning    = "running"
	BacktestDone       = "done"
	BacktestFailed     = "failed"
	BacktestSuperseded = "superseded"
	BacktestCancelled  = "cancelled"
)

const (
	// MaxBacktestBody is the most a backtest's request is, as sent, and so
	// the most it is read back as, gzipped or not.
	MaxBacktestBody = 4 << 20
	// MaxBacktestResult is the most a backtest's result is, its Markdown
	// and its JSON, and so the most it is read back as.
	MaxBacktestResult = 1 << 20
	// backtestsKept is how many backtests are kept at most, the oldest
	// ended going first; backtestKeep how long one is kept once it ended.
	backtestsKept = 200
	backtestKeep  = 24 * time.Hour
	// backtestAttempts is how often a backtest is started at most: one
	// whose leader stopped while it ran is put back to wait, once.
	backtestAttempts = 2
)

// BacktestCrashed is why a backtest failed that its leader stopped while
// running, twice.
const BacktestCrashed = "lapilli serve stopped twice while it ran it: it isn't run again"

// Backtest is a backtest asked for, as kept.
type Backtest struct {
	ID, Trust, Repository, Commit string
	// RepositoryID is the repository's id, as the ID token that asked
	// says it and its entry pins it: "" for a plain token's, for a CI
	// Lapilli doesn't know, and for a backtest asked before ids were kept
	// (031.sql).
	RepositoryID string
	// QueueBranch is, of a merge queue's run (GitHub's merge_group), the
	// branch the queue merges into, as the ref its ID token names says it
	// (033.sql): "" for a pull request's, and any other run's.
	QueueBranch string
	// Workflow and RunNumber are, of a GitHub Actions job's, the workflow
	// its run is of (its file, as the ID token's workflow_ref names it
	// before its @ref) and the run's number (run_number), which begins at 1
	// for a workflow's first run and increments with each new run, a
	// re-run keeping it: "", 0 for any other CI's (033.sql).
	Workflow              string
	RunNumber             int64
	PullRequest           int64
	State                 string
	Asked, Started, Ended time.Time
	// Rules are the rules that differ, on either side; Requests those it
	// was counted to ask of the history, Made those it asked.
	Rules, Requests, Made int
	// KeyHash is the SHA-256 of its key, in hex.
	KeyHash string
	// Cancel says its job cancelled it while it ran.
	Cancel bool
	// Error is why it failed, in Lapilli's words.
	Error string
	// Position is, of one waiting, how many wait before it, and one more
	// when another runs: 0 for none, as it runs next.
	Position int
}

// BacktestAsk is a backtest a CI job asks for: the row to keep, and its
// request as sent (Body, MaxBacktestBody at most), kept until it ends.
type BacktestAsk struct {
	Backtest
	Body []byte
	// Token is what the ID token that asked is known by, its jti or its
	// hash, "" for a plain token's; BodyHash the body's hash, which the
	// token is tied to; Until, when its row may go.
	Token, BodyHash string
	Until           time.Time
	// Queue is how many may wait in all, PerTrust how many of one entry.
	Queue, PerTrust int
	// Supersede says the pull request's number is the token's own (its
	// ref): one waiting for the same entry, repository, pull request and
	// QueueBranch, of another commit, is superseded (sameRepository), a
	// merge group's by a group of the same pull request the same queue
	// built again, never by a pull request's, nor a pull request's by it.
	// One of the same commit isn't: another job of the same run (a KEY of
	// its own), or the same run again. Of a Workflow, only one of an
	// earlier run of it is (a lower RunNumber, or one kept with no
	// Workflow): a run that asks late, or an earlier run run again, takes
	// no newer run's place, nor a run of another workflow. A number the
	// job said supersedes nothing.
	Supersede bool
}

// sameRepository is an SQL condition on a row of backtests being of the
// repository of the id ?1 and the name ?2: by its id, whatever it is
// named by now, or, for a row kept with no id (asked before 031, or by a
// token that says none), by its name. Neither an id nor a name of "" is
// any repository's: a name is kept as "" when it doesn't show as one.
func sameRepository(id, name string) string {
	return "(repository_id != '' AND repository_id = " + id + " OR repository_id = '' AND " + name + " != '' AND repository = " + name + ")"
}

var (
	// ErrBacktestToken is a token that started a backtest with another
	// body.
	ErrBacktestToken = errors.New("the token started another backtest")
	// ErrBacktestsFull is a queue with as many waiting as it takes, or as
	// many of the entry's.
	ErrBacktestsFull = errors.New("the backtests' queue is full")
)

// BacktestRoom says at now, before a request's body is read, whether
// trust's token started a backtest already (used; never, for a plain
// token's "") and whether the queue has no room for one more of trust
// (full): those waiting, in all against queue and trust's against
// perTrust, less those of the repository's pull request a newer commit
// would supersede (pullRequest, -1 for none; the repository by its id,
// or its name, as sameRepository says; of a merge queue's run, the
// queue's branch, queueBranch, "" for none). AskBacktest decides again,
// at the token's key, once the body is read.
func (l *Ledger) BacktestRoom(ctx context.Context, trust, token, repositoryID, repository, queueBranch string, pullRequest int64, queue, perTrust int, now time.Time) (used, full bool, err error) {
	if token != "" {
		if err := l.r.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM backtest_tokens WHERE trust = ? AND token = ? AND until_at >= ?)",
			trust, token, now.Unix()).Scan(&used); err != nil || used {
			return used, false, err
		}
	}
	var all, theirs int
	if err := l.r.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(CASE WHEN trust = ?3 THEN 1 ELSE 0 END), 0) FROM backtests
		WHERE state = ?4 AND NOT (trust = ?3 AND pull_request = ?5 AND queue_branch = ?6 AND `+sameRepository("?1", "?2")+`)`,
		repositoryID, repository, trust, BacktestWaiting, pullRequest, queueBranch).Scan(&all, &theirs); err != nil {
		return false, false, err
	}
	return false, all >= queue || theirs >= perTrust, nil
}

// AskBacktest queues a backtest at now: its row, and the token tied to its
// body, both or neither. A token that started one already gives that one
// back, again true, for the same body, and ErrBacktestToken for another:
// the token's key is the check, so that replicas given it at once agree.
// One waiting for the same entry, repository, pull request and queue's
// branch, of another commit and, of a workflow, of an earlier run of it,
// is superseded, and said in superseded: the repository by its id,
// or, for a row with none, its name (sameRepository). Past the queue's bounds,
// ErrBacktestsFull, and nothing is kept.
func (l *Ledger) AskBacktest(ctx context.Context, a BacktestAsk, now time.Time) (b Backtest, again bool, superseded int, err error) {
	if len(a.Body) > MaxBacktestBody {
		return Backtest{}, false, 0, fmt.Errorf("a backtest's request is %d MiB at most", MaxBacktestBody>>20)
	}
	body, err := gzBytes(a.Body)
	if err != nil {
		return Backtest{}, false, 0, err
	}
	err = l.write(ctx, func(t *txn) error {
		if err := pruneBacktests(t, now); err != nil {
			return err
		}
		if a.Token != "" {
			var one int
			err := t.QueryRowContext(t.ctx, `INSERT INTO backtest_tokens (trust, token, body_hash, backtest_id, until_at) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (trust, token) DO NOTHING RETURNING 1`, a.Trust, a.Token, a.BodyHash, a.ID, a.Until.Unix()).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				var hash, id string
				if err := t.QueryRowContext(t.ctx, "SELECT body_hash, backtest_id FROM backtest_tokens WHERE trust = ? AND token = ?", a.Trust, a.Token).Scan(&hash, &id); err != nil {
					return err
				}
				if hash != a.BodyHash {
					return ErrBacktestToken
				}
				again = true
				b, err = backtestTx(t, id)
				if errors.Is(err, sql.ErrNoRows) {
					// Kept no longer: the token started it all the same.
					return ErrBacktestToken
				}
				return err
			}
			if err != nil {
				return err
			}
		}
		if a.Supersede && a.PullRequest > 0 {
			res, err := t.ExecContext(t.ctx, `UPDATE backtests SET state = ?3, ended_at = ?4, body = NULL
				WHERE trust = ?5 AND pull_request = ?6 AND state = ?7 AND queue_branch = ?8 AND commit_sha != ?9
				AND (?10 = '' OR workflow = '' OR workflow = ?10 AND run_number < ?11) AND `+sameRepository("?1", "?2"),
				a.RepositoryID, a.Repository, BacktestSuperseded, now.Unix(), a.Trust, a.PullRequest, BacktestWaiting, a.QueueBranch, a.Commit,
				a.Workflow, a.RunNumber)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			superseded = int(n)
		}
		var all, theirs int
		if err := t.QueryRowContext(t.ctx, "SELECT count(*), coalesce(sum(CASE WHEN trust = ? THEN 1 ELSE 0 END), 0) FROM backtests WHERE state = ?",
			a.Trust, BacktestWaiting).Scan(&all, &theirs); err != nil {
			return err
		}
		if all >= a.Queue || theirs >= a.PerTrust {
			return ErrBacktestsFull
		}
		var n int64
		if err := t.QueryRowContext(t.ctx, "SELECT coalesce(max(n), 0) + 1 FROM backtests").Scan(&n); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, `INSERT INTO backtests (id, n, trust, repository, repository_id, queue_branch, workflow, run_number, pull_request, commit_sha, state, asked_at, rules, requests, key_hash, body)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, a.ID, n, a.Trust, a.Repository, a.RepositoryID, a.QueueBranch, a.Workflow, a.RunNumber, a.PullRequest, a.Commit, BacktestWaiting, now.Unix(),
			a.Rules, a.Requests, a.KeyHash, body); err != nil {
			return err
		}
		b, err = backtestTx(t, a.ID)
		return err
	})
	if err != nil {
		return Backtest{}, false, 0, err
	}
	return b, again, superseded, nil
}

// PruneBacktests lets go, at now, of what pruneBacktests does, as the
// leader's every round does whether or not the port is on: a ledger with
// none kept isn't written to.
func (l *Ledger) PruneBacktests(ctx context.Context, now time.Time) error {
	var some bool
	if err := l.r.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM backtests) OR EXISTS (SELECT 1 FROM backtest_tokens)").Scan(&some); err != nil || !some {
		return err
	}
	return l.writeAside(ctx, func(t *txn) error { return pruneBacktests(t, now) })
}

// pruneBacktests lets go of the tokens past their time, the backtests
// ended a day ago, and the oldest ended past backtestsKept.
func pruneBacktests(t *txn, now time.Time) error {
	if _, err := t.ExecContext(t.ctx, "DELETE FROM backtest_tokens WHERE until_at < ?", now.Unix()); err != nil {
		return err
	}
	if _, err := t.ExecContext(t.ctx, "DELETE FROM backtests WHERE ended_at > 0 AND ended_at < ?", now.Add(-backtestKeep).Unix()); err != nil {
		return err
	}
	// Room for one more, of backtestsKept: the oldest ended go.
	var n int
	if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM backtests").Scan(&n); err != nil {
		return err
	}
	if over := n - (backtestsKept - 1); over > 0 {
		_, err := t.ExecContext(t.ctx, "DELETE FROM backtests WHERE id IN (SELECT id FROM backtests WHERE ended_at > 0 ORDER BY n LIMIT ?)", over)
		return err
	}
	return nil
}

const backtestColumns = "id, trust, repository, repository_id, queue_branch, workflow, run_number, pull_request, commit_sha, state, asked_at, started_at, ended_at, rules, requests, asked, key_hash, cancel, error"

func scanBacktest(sc interface{ Scan(...any) error }) (Backtest, error) {
	var b Backtest
	var asked, started, ended int64
	var cancel int
	err := sc.Scan(&b.ID, &b.Trust, &b.Repository, &b.RepositoryID, &b.QueueBranch, &b.Workflow, &b.RunNumber, &b.PullRequest, &b.Commit, &b.State, &asked, &started, &ended,
		&b.Rules, &b.Requests, &b.Made, &b.KeyHash, &cancel, &b.Error)
	b.Asked, b.Cancel = unix(asked), cancel != 0
	if started > 0 {
		b.Started = unix(started)
	}
	if ended > 0 {
		b.Ended = unix(ended)
	}
	return b, err
}

// backtestTx is a backtest as a transaction reads it, with its position.
func backtestTx(t *txn, id string) (Backtest, error) {
	return backtestOf(t.ctx, t, id)
}

func backtestOf(ctx context.Context, q querier, id string) (Backtest, error) {
	b, err := scanBacktest(q.QueryRowContext(ctx, "SELECT "+backtestColumns+" FROM backtests WHERE id = ?", id))
	if err != nil || b.State != BacktestWaiting {
		return b, err
	}
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM backtests WHERE state = ? OR state = ? AND n < (SELECT n FROM backtests WHERE id = ?)`,
		BacktestRunning, BacktestWaiting, id).Scan(&b.Position)
	return b, err
}

// Backtest is the backtest kept with id; ok false for none.
func (l *Ledger) Backtest(ctx context.Context, id string) (Backtest, bool, error) {
	b, err := backtestOf(ctx, l.r, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Backtest{}, false, nil
	}
	return b, err == nil, err
}

// BacktestResult is what a backtest came to, as EndBacktest kept it,
// MaxBacktestResult at most read back; nil for none.
func (l *Ledger) BacktestResult(ctx context.Context, id string) ([]byte, error) {
	var b []byte
	err := l.r.QueryRowContext(ctx, "SELECT result FROM backtests WHERE id = ?", id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) || err == nil && b == nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ungzBytes(b, MaxBacktestResult)
}

// Backtests are the backtests kept, the latest asked first, limit at most:
// for the Run history page.
func (l *Ledger) Backtests(ctx context.Context, limit int) ([]Backtest, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT "+backtestColumns+" FROM backtests ORDER BY n DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Backtest
	for rows.Next() {
		b, err := scanBacktest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BacktestKeys are the key hashes of the backtests kept, by id, the latest
// asked first, limit at most: what lapilli serve's port tells a job's id
// and key from, a listing at a time, without reading a row of each id it
// is asked about.
func (l *Ledger) BacktestKeys(ctx context.Context, limit int) (map[string]string, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT id, key_hash FROM backtests ORDER BY n DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, err
		}
		out[id] = hash
	}
	return out, rows.Err()
}

// BacktestsWaiting are how many backtests wait, by entry.
func (l *Ledger) BacktestsWaiting(ctx context.Context) (map[string]int, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT trust, count(*) FROM backtests WHERE state = ? GROUP BY trust", BacktestWaiting)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var trust string
		var n int
		if err := rows.Scan(&trust, &n); err != nil {
			return nil, err
		}
		out[trust] = n
	}
	return out, rows.Err()
}

// StartBacktest takes the oldest backtest waiting, at now, as running,
// and gives it and its request; ok false for none waiting. One running
// still, as the leader before ended while it ran, waits again first, to be
// run from its start: this one is the one leader, and runs none else.
func (l *Ledger) StartBacktest(ctx context.Context, now time.Time) (b Backtest, body []byte, ok bool, err error) {
	var raw []byte
	err = l.writeAside(ctx, func(t *txn) error {
		if _, err := t.ExecContext(t.ctx, "UPDATE backtests SET state = ?, started_at = 0, asked = 0 WHERE state = ?", BacktestWaiting, BacktestRunning); err != nil {
			return err
		}
		// Started twice and stopped with its leader each time: a third
		// would likely stop the next. Failed, its request let go.
		if _, err := t.ExecContext(t.ctx, "UPDATE backtests SET state = ?, error = ?, ended_at = ?, body = NULL WHERE state = ? AND attempts >= ?",
			BacktestFailed, BacktestCrashed, now.Unix(), BacktestWaiting, backtestAttempts); err != nil {
			return err
		}
		var id string
		// The oldest of the entry that started one least lately: one
		// entry's many don't keep another's few waiting.
		err := t.QueryRowContext(t.ctx, `SELECT id, body FROM backtests b WHERE state = ?
			ORDER BY coalesce((SELECT max(s.started_at) FROM backtests s WHERE s.trust = b.trust), 0), n LIMIT 1`, BacktestWaiting).Scan(&id, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE backtests SET state = ?, started_at = ?, attempts = attempts + 1 WHERE id = ?", BacktestRunning, now.Unix(), id); err != nil {
			return err
		}
		ok = true
		b, err = backtestTx(t, id)
		return err
	})
	if err != nil || !ok {
		return Backtest{}, nil, false, err
	}
	body, err = ungzBytes(raw, MaxBacktestBody)
	return b, body, err == nil, err
}

// BacktestMade says how many requests a backtest running has asked, and
// gives whether its job cancelled it.
func (l *Ledger) BacktestMade(ctx context.Context, id string, made int) (cancel bool, err error) {
	err = l.writeAside(ctx, func(t *txn) error {
		if _, err := t.ExecContext(t.ctx, "UPDATE backtests SET asked = ? WHERE id = ? AND state = ?", made, id, BacktestRunning); err != nil {
			return err
		}
		var c int
		err := t.QueryRowContext(t.ctx, "SELECT cancel FROM backtests WHERE id = ?", id).Scan(&c)
		if errors.Is(err, sql.ErrNoRows) {
			cancel = true // let go of meanwhile
			return nil
		}
		cancel = c != 0
		return err
	})
	return cancel, err
}

// EndBacktest keeps what a backtest running came to, at now: its state
// (done, failed, cancelled), why it failed, the requests it asked, and
// its result (MaxBacktestResult at most, gzipped here); its request is let
// go. One no longer running, or no longer kept, is left as it is.
func (l *Ledger) EndBacktest(ctx context.Context, id, state, why string, made int, result []byte, now time.Time) error {
	if len(result) > MaxBacktestResult {
		return fmt.Errorf("a backtest's result is %d MiB at most", MaxBacktestResult>>20)
	}
	var z any // NULL for none
	if result != nil {
		b, err := gzBytes(result)
		if err != nil {
			return err
		}
		z = b
	}
	return l.write(ctx, func(t *txn) error {
		_, err := t.ExecContext(t.ctx, "UPDATE backtests SET state = ?, error = ?, asked = ?, result = ?, ended_at = ?, body = NULL WHERE id = ? AND state = ?",
			state, why, made, z, now.Unix(), id, BacktestRunning)
		return err
	})
}

// CancelBacktest cancels a backtest at now: one waiting ends so, one
// running is told to (BacktestMade gives it). It reports whether there was
// one to cancel.
func (l *Ledger) CancelBacktest(ctx context.Context, id string, now time.Time) (bool, error) {
	var done bool
	err := l.write(ctx, func(t *txn) error {
		res, err := t.ExecContext(t.ctx, "UPDATE backtests SET state = ?, ended_at = ?, body = NULL WHERE id = ? AND state = ?",
			BacktestCancelled, now.Unix(), id, BacktestWaiting)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			done = true
			return nil
		}
		res, err = t.ExecContext(t.ctx, "UPDATE backtests SET cancel = 1 WHERE id = ? AND state = ?", id, BacktestRunning)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		done = n > 0
		return nil
	})
	return done, err
}

func gzBytes(b []byte) ([]byte, error) {
	var out bytes.Buffer
	z := gzip.NewWriter(&out)
	if _, err := z.Write(b); err != nil {
		return nil, err
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ungzBytes is what b holds, gzipped, limit bytes at most: more, as a row
// written otherwise than here would hold, is an error, never read whole.
func ungzBytes(b []byte, limit int64) ([]byte, error) {
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer z.Close()
	out, err := io.ReadAll(io.LimitReader(z, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("a backtest's row holds more than %d bytes", limit)
	}
	return out, nil
}
