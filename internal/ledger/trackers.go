package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
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
// it has, and when, as the changes kept say. A task whose changes were
// pruned has neither.
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

// statusSet fills in who set each task's status, and when: the change that
// moved it to the status it has. Changes since that left the status as it
// was (a title, an assignee, a link) don't count.
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
	rows, err := tx.QueryContext(ctx, `SELECT what, by, at, CASE WHEN json_valid(after) THEN coalesce(json_extract(after, '$.status'), '') ELSE '' END
		FROM changes WHERE what IN (SELECT value FROM json_each(?)) ORDER BY id DESC`, string(b))
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
	return rows.Err()
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
