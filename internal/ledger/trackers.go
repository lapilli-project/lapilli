package ledger

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
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
// without one, at the statuses given, by id, after After.
type TrackerQuery struct {
	Linked bool
	Status []string
	After  int64
	Limit  int
}

// TrackerTasks are the tasks a tracker job asks for, by id, the lowest
// first, Limit at a time (500 at most).
func (l *Ledger) TrackerTasks(ctx context.Context, q TrackerQuery) ([]Task, error) {
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
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	sq += " ORDER BY id LIMIT ?"
	args = append(args, limit)
	rows, err := l.r.QueryContext(ctx, sq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
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
