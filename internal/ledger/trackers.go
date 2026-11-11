package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Tasks kept in step with a tracker (docs/design/trackers.md): a job holding
// the tasks token reads tasks, and sets their link and status, and nothing
// else.

// TaskTracker is who a job keeping tasks in step with a tracker writes as.
// It isn't a person, so outcomes don't count it as use.
const TaskTracker = "tasks-token"

// maxLink is how long a task's link is, in bytes.
const maxLink = 2048

// TrackerQuery picks tasks for a tracker job: those with an issue or
// without one, at the statuses given, changed after UpdatedAfter (when not
// zero), by id, after After.
type TrackerQuery struct {
	Linked       bool
	Status       []string
	UpdatedAfter time.Time
	After        int64
	Limit        int
}

// TrackerTask is a task as a tracker job reads it: with who set the status
// it has, and when, as the changes kept say. A task whose change to that
// status was pruned has neither.
type TrackerTask struct {
	Task
	StatusBy string    `json:"status_by,omitempty"`
	StatusAt time.Time `json:"status_at,omitzero"`
}

// TrackerTasks are the tasks a tracker job asks for, by id, the lowest
// first, Limit at a time (500 at most).
func (l *Ledger) TrackerTasks(ctx context.Context, q TrackerQuery) ([]TrackerTask, error) {
	sq := "SELECT " + taskColumns + " FROM tasks WHERE id > ?"
	args := []any{q.After}
	if q.Linked {
		sq += " AND link != ''"
	} else {
		sq += " AND link = ''"
	}
	if len(q.Status) > 0 {
		b, _ := json.Marshal(q.Status)
		sq += " AND status IN (SELECT value FROM json_each(?))"
		args = append(args, string(b))
	}
	if !q.UpdatedAfter.IsZero() {
		sq += " AND updated > ?"
		args = append(args, q.UpdatedAfter.Unix())
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	sq += " ORDER BY id LIMIT ?"
	args = append(args, limit)
	// The tasks and their changes as of one moment.
	tx, err := l.r.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, sq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrackerTask
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, TrackerTask{Task: t})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, statusSet(ctx, tx, out)
}

// statusSetQuery is the changes noted for the tasks named (what: task N
// made, task N changed), the latest first, with the status each left.
const statusSetQuery = `SELECT what, by, at, CASE WHEN json_valid(after) THEN coalesce(json_extract(after, '$.status'), '') ELSE '' END
	FROM changes WHERE what IN (SELECT value FROM json_each(?)) ORDER BY id DESC`

// statusSet fills in who set each task's status, and when: the change that
// moved it to the status it has. Changes since that left the status as it
// was (a title, an assignee, a link) don't count. When the change that
// moved it was pruned, neither is known, and neither is given: a later
// edit at that status isn't taken for the move.
func statusSet(ctx context.Context, tx *sql.Tx, ts []TrackerTask) error {
	if len(ts) == 0 {
		return nil
	}
	index := map[string]int{}
	whats := make([]string, 0, 2*len(ts))
	for i, t := range ts {
		for _, w := range []string{fmt.Sprintf("task %d made", t.ID), fmt.Sprintf("task %d changed", t.ID)} {
			index[w] = i
			whats = append(whats, w)
		}
	}
	b, _ := json.Marshal(whats)
	rows, err := tx.QueryContext(ctx, statusSetQuery, string(b))
	if err != nil {
		return err
	}
	defer rows.Close()
	// The latest first: each change at the status the task has now moves
	// who set it back to that change's, until one at another status, or
	// the task's making (an id can be given again once its task is gone).
	settled := make([]bool, len(ts))
	for rows.Next() {
		var what, by, status string
		var at int64
		if err := rows.Scan(&what, &by, &at, &status); err != nil {
			return err
		}
		i := index[what]
		if settled[i] {
			continue
		}
		if status != ts[i].Status {
			settled[i] = true
			continue
		}
		ts[i].StatusBy, ts[i].StatusAt = by, unix(at)
		settled[i] = strings.HasSuffix(what, " made")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Not back to a change at another status, nor to its making: what
	// moved it there was pruned.
	for i := range ts {
		if !settled[i] {
			ts[i].StatusBy, ts[i].StatusAt = "", time.Time{}
		}
	}
	return nil
}

// TrackTask sets what a tracker job may set of a task: its link, an https
// URL set once (to the same again changes nothing), and its status. Nil
// leaves either as it is.
func (l *Ledger) TrackTask(ctx context.Context, id int64, link, status *string, by string) (int64, error) {
	if id == 0 {
		return 0, ErrNotFound
	}
	if link != nil {
		u, err := url.Parse(*link)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(*link) > maxLink {
			return 0, invalid("a task's link from a tracker is an https URL, up to %d bytes, with no user name in it", maxLink)
		}
	}
	if status != nil && !slices.Contains(TaskStatuses, *status) {
		return 0, invalid("status %q: want one of open, doing, done, dropped", *status)
	}
	// What is already so is not written again: not a change, and not
	// noted as one.
	was, err := l.TaskByID(ctx, id)
	if err != nil {
		return 0, err
	}
	if (link == nil || *link == was.Link) && (status == nil || *status == was.Status) {
		return id, nil
	}
	return l.putTask(ctx, id, func(was Task) (Task, error) {
		t := was
		if link != nil {
			// One issue for a task: a job doesn't move a task from one to
			// another.
			if was.Link != "" && was.Link != *link {
				return t, invalid("task %d is linked already: a job doesn't change a task's link", id)
			}
			t.Link = *link
		}
		if status != nil {
			t.Status = *status
		}
		return t, nil
	}, by)
}

// OriginTracker is the origin of a comment copied in from a task's issue.
const OriginTracker = "tracker"

// MaxCopied is how many comments copied in from its issue a task keeps.
const MaxCopied = 1000

var commentRef = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,100}$`)

// CopyComment keeps a comment from a task's issue, as the tasks token sends
// it: its text, who wrote it and when, as the tracker says, and the
// tracker's id for it. The same id again changes nothing: it reports
// whether it was kept. It is noted as the tracker job's, and isn't told:
// the tracker told people.
func (l *Ledger) CopyComment(ctx context.Context, task int64, body, author, ref string, at time.Time) (bool, error) {
	body = strings.TrimSpace(body)
	author = strings.TrimSpace(author)
	switch {
	case body == "":
		return false, invalid("a comment says something")
	case utf8.RuneCountInString(body) > 10000:
		return false, invalid("a comment is %d characters at most", 10000)
	case !textShows(body):
		return false, invalid("a comment's text is lines that show")
	case author == "" || !plain(author) || utf8.RuneCountInString(author) > 100:
		return false, invalid("a comment from a tracker names who wrote it, 100 characters at most, that show")
	case !commentRef.MatchString(ref):
		return false, invalid("a comment's ref is the tracker's id for it: letters, digits and ._:-, 100 at most")
	case !sane(at) || at.After(time.Now().Add(skew)):
		return false, invalid("a comment's time is when it was written, not after now")
	}
	key := fmt.Sprint(task)
	kept := false
	err := l.write(ctx, func(t *txn) error {
		var there, n int
		if err := t.QueryRowContext(t.ctx, "SELECT 1 FROM tasks WHERE id = ?", task).Scan(&there); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM comments WHERE on_kind = 'task' AND on_key = ? AND origin = ?", key, OriginTracker).Scan(&n); err != nil {
			return err
		}
		res, err := t.ExecContext(t.ctx, `INSERT OR IGNORE INTO comments (on_kind, on_key, body, created, created_by, updated, origin, ref, author)
			SELECT 'task', ?, ?, ?, ?, ?, ?, ?, ? WHERE ? < ?`, key, body, at.Unix(), TaskTracker, at.Unix(), OriginTracker, ref, author, n, MaxCopied)
		if err != nil {
			return err
		}
		if k, _ := res.RowsAffected(); k == 0 {
			if n >= MaxCopied {
				var dup int
				if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM comments WHERE on_kind = 'task' AND on_key = ? AND ref = ?", key, ref).Scan(&dup); err != nil {
					return err
				}
				if dup == 0 {
					return invalid("a task keeps %d comments copied in from its issue at most", MaxCopied)
				}
			}
			return nil
		}
		kept = true
		return change(t, TaskTracker, fmt.Sprintf("comment on task %d copied in from its issue", task), nil, nil)
	})
	return kept, err
}

// textShows reports whether a comment's text shows: lines of text, with
// tabs, and nothing that hides or reorders it.
func textShows(s string) bool {
	for line := range strings.SplitSeq(s, "\n") {
		if !plain(strings.ReplaceAll(strings.TrimSuffix(line, "\r"), "\t", " ")) {
			return false
		}
	}
	return true
}
