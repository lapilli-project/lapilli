package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// What the ledger keeps of a rule by its name (docs/design/ui.md, Orphans):
// a note, tasks, comments and proposals. A rule deleted or renamed leaves
// them under a name no rule in effect has; the Orphans page lists those
// names, and moves what they keep to a rule in effect (a rename), or
// deletes it.

// RuleKept is what the ledger keeps of a rule's name: whether it has a
// note; its tasks, and of them those open or being done; the comments on
// it; and its proposals not finished with.
type RuleKept struct {
	Rule             string
	Note             bool
	Tasks, OpenTasks int
	Comments         int
	Proposals        []int64
}

// KeptNames gives fn each rule name the ledger keeps a note, a task, a
// comment or a proposal not finished with of, once, in byte order, until
// fn returns false: read as they come, one at a time, however many there
// are.
func (l *Ledger) KeptNames(ctx context.Context, fn func(rule string) bool) error {
	rows, err := l.r.QueryContext(ctx, `SELECT rule FROM notes
		UNION SELECT rule FROM tasks WHERE rule != ''
		UNION SELECT on_key FROM comments WHERE on_kind = 'rule'
		UNION SELECT rule FROM proposals WHERE state IN (`+marks(len(unfinished))+`)
		ORDER BY 1`, anys(unfinished)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if !fn(name) {
			return nil
		}
	}
	return rows.Err()
}

// KeptOf is what the ledger keeps of each rule name given, in their order.
func (l *Ledger) KeptOf(ctx context.Context, names []string) ([]RuleKept, error) {
	if len(names) == 0 {
		return nil, nil
	}
	at := map[string]int{}
	out := make([]RuleKept, len(names))
	for i, n := range names {
		at[n], out[i] = i, RuleKept{Rule: n}
	}
	b, _ := json.Marshal(names)
	in := l.d.texts("?")
	// The four reads as of one moment.
	tx, err := l.r.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	each := func(q string, scan func(rows *sql.Rows, k *RuleKept) error) error {
		rows, err := tx.QueryContext(ctx, q, string(b))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var k RuleKept
			if err := scan(rows, &k); err != nil {
				return err
			}
			name = k.Rule
			i, ok := at[name]
			if !ok {
				continue
			}
			o := &out[i]
			o.Note = o.Note || k.Note
			o.Tasks += k.Tasks
			o.OpenTasks += k.OpenTasks
			o.Comments += k.Comments
			o.Proposals = append(o.Proposals, k.Proposals...)
		}
		return rows.Err()
	}
	if err := each("SELECT rule FROM notes WHERE rule IN ("+in+")", func(rows *sql.Rows, k *RuleKept) error {
		k.Note = true
		return rows.Scan(&k.Rule)
	}); err != nil {
		return nil, err
	}
	if err := each(`SELECT rule, count(*), count(*) FILTER (WHERE status IN ('open', 'doing')) FROM tasks WHERE rule IN (`+in+`) GROUP BY rule`,
		func(rows *sql.Rows, k *RuleKept) error { return rows.Scan(&k.Rule, &k.Tasks, &k.OpenTasks) }); err != nil {
		return nil, err
	}
	if err := each(`SELECT on_key, count(*) FROM comments WHERE on_kind = 'rule' AND on_key IN (`+in+`) GROUP BY on_key`,
		func(rows *sql.Rows, k *RuleKept) error { return rows.Scan(&k.Rule, &k.Comments) }); err != nil {
		return nil, err
	}
	// Each proposal not finished with, the earliest made first.
	rows, err := tx.QueryContext(ctx, `SELECT rule, id FROM proposals WHERE rule IN (`+in+`) AND state IN (`+marks(len(unfinished))+`) ORDER BY id`,
		append([]any{string(b)}, anys(unfinished)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var id int64
		if err := rows.Scan(&name, &id); err != nil {
			return nil, err
		}
		if i, ok := at[name]; ok {
			out[i].Proposals = append(out[i].Proposals, id)
		}
	}
	return out, rows.Err()
}

// ErrSameRule is what a rule keeps moved to itself.
var ErrSameRule = errors.New("that is the same rule: pick the rule it is now")

// MoveRuleKept moves what the ledger keeps of a rule's name to another
// name, as for a rule renamed: its tasks and the comments on it, and its
// note. Where the other has a note too, its own is kept, and what it
// leaves empty is filled from the one moved. Its proposals stay: each was
// made against the rule as it was. Noted in the activity log, with what was
// moved.
func (l *Ledger) MoveRuleKept(ctx context.Context, from, to, by string) error {
	if from == to {
		return ErrSameRule
	}
	if !ruleName(from) || !ruleName(to) {
		return invalid("a note is on a rule")
	}
	return l.write(ctx, func(t *txn) error {
		if _, limited := scopeOf(t.ctx); limited {
			return ErrNotYourTeam
		}
		moved := map[string]any{"to": to}
		was, err := noteOf(t, from)
		if err != nil {
			return err
		}
		if was != nil {
			now, err := noteOf(t, to)
			if err != nil {
				return err
			}
			if was.TeamID != 0 || now != nil && now.TeamID != 0 {
				// Who owns a rule may change: a team's window may quiet it now.
				if err := l.pendTeamWindows(t); err != nil {
					return err
				}
			}
			if now == nil {
				if _, err := t.ExecContext(t.ctx, "UPDATE notes SET rule = ? WHERE rule = ?", to, from); err != nil {
					return err
				}
			} else {
				fill := func(a *string, b string) {
					if *a == "" {
						*a = b
					}
				}
				fill(&now.Status, was.Status)
				fill(&now.Runbook, was.Runbook)
				fill(&now.Note, was.Note)
				if now.TeamID == 0 {
					now.TeamID = was.TeamID
				}
				team := sql.NullInt64{Int64: now.TeamID, Valid: now.TeamID != 0}
				if _, err := t.ExecContext(t.ctx, "UPDATE notes SET team_id = ?, status = ?, runbook = ?, note = ?, updated = ?, updated_by = ? WHERE rule = ?",
					team, now.Status, now.Runbook, now.Note, wallNow().Unix(), by, to); err != nil {
					return err
				}
				if _, err := t.ExecContext(t.ctx, "DELETE FROM notes WHERE rule = ?", from); err != nil {
					return err
				}
			}
			moved["note"] = was
		}
		res, err := t.ExecContext(t.ctx, "UPDATE tasks SET rule = ? WHERE rule = ?", to, from)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		moved["tasks"] = n
		if res, err = t.ExecContext(t.ctx, "UPDATE comments SET on_key = ? WHERE on_kind = 'rule' AND on_key = ?", to, from); err != nil {
			return err
		}
		n2, _ := res.RowsAffected()
		moved["comments"] = n2
		if was == nil && n == 0 && n2 == 0 {
			return ErrNotFound // nothing kept by that name
		}
		return change(t, by, "what rule "+from+" kept moved to "+to, nil, moved)
	})
}

// DeleteRuleKept deletes what the ledger keeps of a rule's name: its note,
// its tasks with the comments on them, and the comments on it. Its
// proposals stay. Each task deleted is noted as DeleteTask notes it, and
// the whole in the activity log.
func (l *Ledger) DeleteRuleKept(ctx context.Context, rule, by string) error {
	if !ruleName(rule) {
		return invalid("a note is on a rule")
	}
	return l.write(ctx, func(t *txn) error {
		if _, limited := scopeOf(t.ctx); limited {
			return ErrNotYourTeam
		}
		was, err := noteOf(t, rule)
		if err != nil {
			return err
		}
		if was != nil && was.TeamID != 0 {
			if err := l.pendTeamWindows(t); err != nil {
				return err
			}
		}
		rows, err := t.QueryContext(t.ctx, "SELECT id FROM tasks WHERE rule = ? ORDER BY id", rule)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			// As DeleteTask deletes one.
			if _, err := t.ExecContext(t.ctx, "DELETE FROM tasks WHERE id = ?", id); err != nil {
				return err
			}
			if _, err := t.ExecContext(t.ctx, "DELETE FROM comments WHERE on_kind = 'task' AND on_key = ?", fmt.Sprint(id)); err != nil {
				return err
			}
			if _, err := t.ExecContext(t.ctx, "UPDATE proposals SET task_id = NULL WHERE task_id = ?", id); err != nil {
				return err
			}
			if err := change(t, by, fmt.Sprintf("task %d deleted", id), nil, nil); err != nil {
				return err
			}
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM notes WHERE rule = ?", rule); err != nil {
			return err
		}
		res, err := t.ExecContext(t.ctx, "DELETE FROM comments WHERE on_kind = 'rule' AND on_key = ?", rule)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if was == nil && len(ids) == 0 && n == 0 {
			return ErrNotFound // nothing kept by that name
		}
		return change(t, by, "what rule "+rule+" kept deleted", nil, map[string]any{"note": was, "tasks": len(ids), "comments": n})
	})
}

// ruleName says a name could be a rule's the ledger keeps something of: a
// note's, at most 500 bytes, with nothing that doesn't show.
func ruleName(s string) bool { return s != "" && len(s) <= 500 && plain(s) }

// noteOf is a rule's note, read in a write; nil for none.
func noteOf(t *txn, rule string) (*Note, error) {
	var n Note
	var at int64
	err := t.QueryRowContext(t.ctx, "SELECT rule, coalesce(team_id, 0), status, runbook, note, updated, updated_by FROM notes WHERE rule = ?", rule).
		Scan(&n.Rule, &n.TeamID, &n.Status, &n.Runbook, &n.Note, &at, &n.By)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	n.Updated = unix(at)
	return &n, nil
}
