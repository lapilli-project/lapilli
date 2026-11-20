package ledger

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// What checking each rule's expectations found (docs/design/expectations.md,
// steps 2 and 4): expectations are declared, not made in the UI, and the ledger
// keeps what checking them found (expectation_checks, 027.sql), which the
// leader writes after each round, and the pages, the digest and the API
// read.

// ExpectCheck is what checking one expectation of one rule found in a week.
type ExpectCheck struct {
	Rule string `json:"rule"`
	// Expect is the hash of what it is held to (ExpectHash).
	Expect string `json:"expect"`
	// Week is the week's Monday, as days: of a catch, the week its
	// incident started in, 0 while the incident isn't written down; of a
	// scenario, the week what it found was found in.
	Week int64 `json:"week"`
	// What is "budget week", "budget asleep", "budget refires", "catches
	// INC-43" or "scenario tests/restarts.yaml"; Held what it is held to
	// ("10", "within 5m", "caught", "passes"); Selector what holds the rule
	// ("rule: Restarts"); Source where it is declared (file, config,
	// annotation).
	What     string `json:"what"`
	Held     string `json:"held"`
	Selector string `json:"selector"`
	Source   string `json:"source"`
	// Result is met, broken, not known or open (the week under way, a
	// budget not over yet); Why, of not known and open, why.
	Result string `json:"result"`
	Why    string `json:"why,omitempty"`
	// N is a budget's count, nil for a catch; Value what decided it as a
	// person reads it.
	N     *int   `json:"n"`
	Value string `json:"value"`
	// At is a catch's incident's start; Version, of a catch judged again
	// on a new version of the rule, what the rule was then, and of a
	// scenario what it was run on. Detail is, of a scenario, what promtool
	// said of it failing, or why it couldn't be run.
	At      time.Time `json:"at,omitzero"`
	Version string    `json:"version,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	// Current says it is held now; Checked, when it was last written.
	Current bool      `json:"current"`
	Checked time.Time `json:"checked"`
	// Told is the week a notice of it broken was last told in, 0 for
	// none; Task the task made of it, Tasked the week it was made in.
	Told   int64 `json:"-"`
	Task   int64 `json:"task,omitempty"`
	Tasked int64 `json:"-"`
}

// ExpectTasksBy is who a task --expect-tasks makes is made by.
const ExpectTasksBy = GoalTasksBy

// The results of a check.
const (
	ExpectMet     = "met"
	ExpectBroken  = "broken"
	ExpectUnknown = "not known"
	ExpectOpen    = "open"
)

// Catch says it is of a catch, not a budget.
func (c ExpectCheck) Catch() bool { return strings.HasPrefix(c.What, "catches ") }

// Scenario says it is of a scenario: a promtool test file.
func (c ExpectCheck) Scenario() bool { return strings.HasPrefix(c.What, "scenario ") }

// Weekless says it isn't a budget's week: a catch, of its incident's
// week, or a scenario, of the week what it found was found in; one row a
// rule and an expectation, held whatever its week.
func (c ExpectCheck) Weekless() bool { return c.Catch() || c.Scenario() }

// Key is what tells it apart: its rule, expectation and week.
func (c ExpectCheck) Key() ExpectKey { return ExpectKey{c.Rule, c.Expect, c.Week} }

// ExpectKey is a row of expectation_checks: a rule, an expectation and a
// week.
type ExpectKey struct {
	Rule, Expect string
	Week         int64
}

// same says two checks found the same, held by the same, whatever was
// told of them or made of them.
func (c ExpectCheck) same(d ExpectCheck) bool {
	return c.What == d.What && c.Held == d.Held && c.Selector == d.Selector && c.Source == d.Source && c.Result == d.Result &&
		c.Why == d.Why && (c.N == nil) == (d.N == nil) && (c.N == nil || *c.N == *d.N) && c.Value == d.Value && c.At.Equal(d.At) &&
		c.Version == d.Version && c.Detail == d.Detail && c.Current == d.Current
}

// ExpectHash is the hash an expectation is kept by: of what holds the
// rule, what it is held to and the bound, 32 hex digits.
func ExpectHash(selector, what, held string) string {
	h := sha256.Sum256([]byte(selector + "\n" + what + "\n" + held))
	return hex.EncodeToString(h[:16])
}

// ExpectQuery picks checks: of one rule (Rule, when HasRule), the current
// ones alone (Current), the broken alone (Broken), and of budgets the
// weeks from From on (catches and scenarios whatever their week).
type ExpectQuery struct {
	Rule    string
	HasRule bool
	Current bool
	Broken  bool
	From    int64
}

const expectCols = "rule, expect, week, what, held, selector, source, result, why, n, value, at, version, detail, current, checked, told, task, tasked"

// ExpectChecks are the checks q picks, by rule, expectation and week.
func (l *Ledger) ExpectChecks(ctx context.Context, q ExpectQuery) ([]ExpectCheck, error) {
	var where []string
	var args []any
	if q.HasRule {
		where = append(where, l.d.pick("rule = ?", "md5(rule) = md5(?) AND rule = ?"))
		args = append(args, q.Rule)
		if l.d == postgres {
			args = append(args, q.Rule)
		}
	}
	if q.Broken {
		// Along expectation_checks_result: the broken of the weeks from
		// From on, and the catches and scenarios, whose week is their
		// incident's, or when what they found was found.
		where = append(where, "result = 'broken' AND (week >= ? OR what LIKE 'catches %' OR what LIKE 'scenario %')")
		args = append(args, q.From)
	} else if q.From != 0 {
		where = append(where, "(week >= ? OR what LIKE 'catches %' OR what LIKE 'scenario %')")
		args = append(args, q.From)
	}
	if q.Current {
		where = append(where, "current = 1")
	}
	s := "SELECT " + expectCols + " FROM expectation_checks"
	if len(where) > 0 {
		s += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := l.r.QueryContext(ctx, s+" ORDER BY rule, what, held, week", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExpectCheck
	for rows.Next() {
		var c ExpectCheck
		var n sql.NullInt64
		var at, checked, current int64
		if err := rows.Scan(&c.Rule, &c.Expect, &c.Week, &c.What, &c.Held, &c.Selector, &c.Source, &c.Result, &c.Why, &n, &c.Value, &at, &c.Version,
			&c.Detail, &current, &checked, &c.Told, &c.Task, &c.Tasked); err != nil {
			return nil, err
		}
		if n.Valid {
			v := int(n.Int64)
			c.N = &v
		}
		if at != 0 {
			c.At = unix(at)
		}
		c.Current, c.Checked = current == 1, unix(checked)
		out = append(out, c)
	}
	return out, rows.Err()
}

// MaxExpectWrites is the most rows KeepExpectChecks writes at once: the
// rest are written by the rounds after, so that no round holds the
// ledger's one writer long.
var MaxExpectWrites = 20000

// KeepExpectChecks keeps what a round found, against what the ledger held
// (old, as read before it): a row that changed is written, one held no
// more is kept not current, a budget's week before from is let go, and a
// catch keeps one row, of its incident's week, as a scenario keeps one, of
// the week it was found in. What was told of a row, and
// the task made of it, stay; a row turned met is told again when it is
// next broken, in a later week than it was told in (this). It writes
// nothing when nothing changed, and MaxExpectWrites rows at most: it
// returns how many it left for the next round.
func (l *Ledger) KeepExpectChecks(ctx context.Context, found, old []ExpectCheck, from, this int64, now time.Time) (left int, err error) {
	was := make(map[ExpectKey]ExpectCheck, len(old))
	for _, c := range old {
		was[c.Key()] = c
	}
	is := make(map[ExpectKey]bool, len(found))
	caught := map[[2]string]bool{} // the catches and scenarios found, by rule and expectation
	var put []ExpectCheck
	for _, c := range found {
		c.Current = true
		is[c.Key()] = true
		if c.Weekless() {
			caught[[2]string{c.Rule, c.Expect}] = true
		}
		if o, ok := was[c.Key()]; ok && o.same(c) {
			continue
		}
		put = append(put, c)
	}
	var gone, stale []ExpectKey
	for _, k := range expectKeys(was) {
		o := was[k]
		switch {
		case is[k]:
		case !o.Weekless() && o.Week < from,
			o.Weekless() && (caught[[2]string{o.Rule, o.Expect}] || !o.Current && o.Checked.Before(now.Add(-13*7*24*time.Hour))):
			// Out of the weeks checked; a catch's row of another week
			// than its incident's now, a scenario's of another than it was
			// found in; one held no more for 13 weeks.
			gone = append(gone, k)
		case o.Current:
			stale = append(stale, k)
		}
	}
	if len(put)+len(gone)+len(stale) == 0 {
		return 0, nil
	}
	budget := MaxExpectWrites
	take := func(n int) int {
		n = min(n, budget)
		budget -= n
		return n
	}
	n := take(len(put))
	left = len(put) - n
	put = put[:n]
	n = take(len(gone))
	left += len(gone) - n
	gone = gone[:n]
	n = take(len(stale))
	left += len(stale) - n
	stale = stale[:n]
	err = l.write(ctx, func(t *txn) error {
		const each = 100
		for len(put) > 0 {
			part := put[:min(len(put), each)]
			put = put[len(part):]
			if err := putExpectChecks(t, part, this, now); err != nil {
				return err
			}
		}
		byKey := t.d.pick("rule = ? AND expect = ? AND week = ?", "md5(rule) = md5(?1) AND rule = ?1 AND expect = ?2 AND week = ?3")
		for _, k := range gone {
			if _, err := t.ExecContext(t.ctx, "DELETE FROM expectation_checks WHERE "+byKey, k.Rule, k.Expect, k.Week); err != nil {
				return err
			}
		}
		for _, k := range stale {
			if _, err := t.ExecContext(t.ctx, "UPDATE expectation_checks SET current = 0, checked = ?1 WHERE "+t.d.pick("rule = ?2 AND expect = ?3 AND week = ?4",
				"md5(rule) = md5(?2) AND rule = ?2 AND expect = ?3 AND week = ?4"), now.Unix(), k.Rule, k.Expect, k.Week); err != nil {
				return err
			}
		}
		return nil
	})
	return left, err
}

// putExpectChecks writes checks, each a row made or made again current,
// what was told of it and the task made of it kept; one turned met is told
// again when it is next broken, in a later week than this.
func putExpectChecks(t *txn, part []ExpectCheck, this int64, now time.Time) error {
	var vals []string
	var args []any
	for _, c := range part {
		vals = append(vals, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)")
		var n any
		if c.N != nil {
			n = int64(*c.N)
		}
		args = append(args, c.Rule, c.Expect, c.Week, c.What, c.Held, c.Selector, c.Source, c.Result, c.Why, n, c.Value, unixOr0(c.At), c.Version,
			c.Detail, now.Unix())
	}
	args = append(args, this)
	_, err := t.ExecContext(t.ctx, `INSERT INTO expectation_checks (rule, expect, week, what, held, selector, source, result, why, n, value, at, version, detail, current, checked)
		VALUES `+strings.Join(vals, ", ")+`
		ON CONFLICT `+t.d.pick("(rule, expect, week)", "(md5(rule), expect, week)")+` DO UPDATE SET what = excluded.what, held = excluded.held,
			selector = excluded.selector, source = excluded.source, result = excluded.result, why = excluded.why, n = excluded.n,
			value = excluded.value, at = excluded.at, version = excluded.version, detail = excluded.detail, current = 1, checked = excluded.checked,
			told = CASE WHEN excluded.result = 'met' AND expectation_checks.told < ? THEN 0 ELSE expectation_checks.told END`, args...)
	return err
}

// KeepScenarioRun keeps one scenario's check at once, apart from the
// round's (KeepExpectChecks): as its run begins, what it is run on
// (Version) and that it hasn't finished, so that a run that never ends (the
// process killed as promtool takes its memory) is kept so by the next
// leader, which runs it again only once what it is run on changes; as it
// ends, what it found. One row, written alone, never while promtool runs.
func (l *Ledger) KeepScenarioRun(ctx context.Context, c ExpectCheck, this int64, now time.Time) error {
	if !c.Scenario() {
		return errors.New("only a scenario's check is kept as it runs")
	}
	return l.write(ctx, func(t *txn) error { return putExpectChecks(t, []ExpectCheck{c}, this, now) })
}

// expectKeys are a map's keys in one order, so that what a round leaves
// for the next is the same each time.
func expectKeys(m map[ExpectKey]ExpectCheck) []ExpectKey {
	return slices.SortedFunc(maps.Keys(m), func(a, b ExpectKey) int {
		return cmp.Or(strings.Compare(a.Rule, b.Rule), strings.Compare(a.Expect, b.Expect), cmp.Compare(a.Week, b.Week))
	})
}

// TellExpectBroken puts what is told of expectations broken in the
// outbox, each notice an expectation.broken to each place the routes send
// it to, and marks each row told in week this, in one write, whether the
// routes sent it anywhere or not, as goal.missed is: a row marked told in
// this week already, as by another leader a moment before, is told no
// more.
func (l *Ledger) TellExpectBroken(ctx context.Context, keys []ExpectKey, this int64, told [][]Told) error {
	routed := make([][]Told, len(told))
	for i, ts := range told {
		var err error
		if routed[i], err = l.routeTold("expectation.broken", ts); err != nil {
			return err
		}
	}
	told = routed
	return l.writeAside(ctx, func(t *txn) error {
		now := wallNow()
		byKey := t.d.pick("rule = ? AND expect = ? AND week = ?", "md5(rule) = md5(?1) AND rule = ?1 AND expect = ?2 AND week = ?3")
		for i, k := range keys {
			var was int64
			err := t.QueryRowContext(t.ctx, "SELECT told FROM expectation_checks WHERE "+byKey, k.Rule, k.Expect, k.Week).Scan(&was)
			if errors.Is(err, sql.ErrNoRows) || err == nil && was >= this {
				continue
			} else if err != nil {
				return err
			}
			for _, x := range told[i] {
				about := map[string]any{"event": "expectation.broken", "at": now.UTC()}
				maps.Copy(about, x.About)
				body, err := json.Marshal(about)
				if err != nil {
					return err
				}
				if _, err := t.ExecContext(t.ctx, t.d.pick("INSERT INTO outbox (dest, event, body, created, next_at) VALUES (?, ?, ?, ?, ?)", outboxDue),
					x.Dest, "expectation.broken", string(body), now.Unix(), now.Unix()); err != nil {
					return err
				}
			}
			if _, err := t.ExecContext(t.ctx, "UPDATE expectation_checks SET told = ? WHERE "+t.d.pick("rule = ? AND expect = ? AND week = ?",
				"md5(rule) = md5(?2) AND rule = ?2 AND expect = ?3 AND week = ?4"), this, k.Rule, k.Expect, k.Week); err != nil {
				return err
			}
		}
		return nil
	})
}

// ErrExpectTaskOpen is a task about an expectation made while one is open
// about it, of any week: the one open is returned.
var ErrExpectTaskOpen = errors.New("a task about this expectation is open already")

// ErrExpectNotBroken is a task about an expectation made of a week it
// isn't broken in, as the ledger last checked it.
var ErrExpectNotBroken = errors.New("this expectation isn't broken in that week, as it was last checked")

// MakeExpectTask makes a task of a row checked broken, in one write: none
// while one made of the rule's expectation, of any week, is open (its id
// is returned, with ErrExpectTaskOpen); none of a row not broken, or not
// current (ErrExpectNotBroken); else the task, kept on the row with the
// week it was made in (this).
func (l *Ledger) MakeExpectTask(ctx context.Context, task Task, k ExpectKey, this int64, by string) (int64, error) {
	var id int64
	err := l.write(ctx, func(t *txn) error {
		id = 0
		var result string
		var current int64
		err := t.QueryRowContext(t.ctx, "SELECT result, current FROM expectation_checks WHERE "+t.d.pick("rule = ? AND expect = ? AND week = ?",
			"md5(rule) = md5(?1) AND rule = ?1 AND expect = ?2 AND week = ?3"), k.Rule, k.Expect, k.Week).Scan(&result, &current)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrExpectNotBroken
		case err != nil:
			return err
		case result != ExpectBroken || current != 1:
			return ErrExpectNotBroken
		}
		var open int64
		err = t.QueryRowContext(t.ctx, `SELECT k.id FROM expectation_checks c JOIN tasks k ON k.id = c.task
			WHERE `+t.d.pick("c.rule = ?", "md5(c.rule) = md5(?1) AND c.rule = ?1")+` AND c.expect = `+t.d.pick("?", "?2")+` AND c.task != 0 AND k.status IN ('open', 'doing')
			ORDER BY k.id LIMIT 1`, k.Rule, k.Expect).Scan(&open)
		switch {
		case err == nil:
			id = open
			return ErrExpectTaskOpen
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if id, err = l.putTaskIn(t, 0, func(Task) (Task, error) { return task, nil }, by); err != nil {
			return err
		}
		_, err = t.ExecContext(t.ctx, "UPDATE expectation_checks SET task = ?, tasked = ? WHERE "+t.d.pick("rule = ? AND expect = ? AND week = ?",
			"md5(rule) = md5(?3) AND rule = ?3 AND expect = ?4 AND week = ?5"), id, this, k.Rule, k.Expect, k.Week)
		return err
	})
	return id, err
}

// OpenExpectTasks are, of the tasks made of expectations, those open or
// being done, by their row.
func (l *Ledger) OpenExpectTasks(ctx context.Context) (map[ExpectKey]int64, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT c.rule, c.expect, c.week, k.id FROM expectation_checks c JOIN tasks k ON k.id = c.task
		WHERE c.task != 0 AND k.status IN ('open', 'doing')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[ExpectKey]int64{}
	for rows.Next() {
		var k ExpectKey
		var id int64
		if err := rows.Scan(&k.Rule, &k.Expect, &k.Week, &id); err != nil {
			return nil, err
		}
		out[k] = id
	}
	return out, rows.Err()
}

// IncidentsByKey are the incidents, counted ones alone, of those ids the
// ledger has, with the rules that caught each: what an expectation's
// catches name. Each id found through the index on the incidents' ids.
func (l *Ledger) IncidentsByKey(ctx context.Context, keys []string) (map[string]Incident, error) {
	out := map[string]Incident{}
	for len(keys) > 0 {
		part := keys[:min(len(keys), 500)]
		keys = keys[len(part):]
		var marks []string
		var args []any
		for i, k := range part {
			marks = append(marks, l.d.pick("?", "md5(?"+strconv.Itoa(i+1)+")"))
			args = append(args, k)
		}
		in := strings.Join(marks, ", ")
		where := l.d.pick("i.key IN ("+in+")", "md5(i.key) IN ("+in+") AND i.key IN ("+strings.ReplaceAll(strings.ReplaceAll(in, "md5(", ""), ")", "")+")")
		incs, err := l.incidents(ctx, "SELECT "+incidentCols+" FROM incidents i WHERE "+where+" AND i.counts = 1", args...)
		if err != nil {
			return nil, err
		}
		for _, in := range incs {
			out[in.ID] = in
		}
	}
	return out, nil
}
