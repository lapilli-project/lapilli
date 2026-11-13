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
	"unicode"
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
		sq += " AND status IN (" + l.d.texts("?") + ")"
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
	return out, statusSet(ctx, l.d, tx, out)
}

// statusSetQuery is the changes noted for the tasks named (what: task N
// made, task N changed), the latest first, with the status each left.
// What a change was left as is ” or the JSON change() wrote: in Postgres,
// which can't say whether a text is JSON before 16, an object is read as
// one. There, changes_what is a hash index, as what can be longer than a
// btree holds (an incident's id in it), searched for an array's texts.
func statusSetQuery(d dialect) string {
	return d.pick(`SELECT what, by, at, CASE WHEN json_valid(after) THEN coalesce(json_extract(after, '$.status'), '') ELSE '' END
	FROM changes WHERE what IN (SELECT value FROM json_each(?)) ORDER BY id DESC`,
		`SELECT what, by, at, CASE WHEN left(after, 1) = '{' THEN coalesce(after::jsonb ->> 'status', '') ELSE '' END
	FROM changes WHERE what = ANY (ARRAY(`+d.texts("?")+`)) ORDER BY id DESC`)
}

// statusSet fills in who set each task's status, and when: the change that
// moved it to the status it has. Changes since that left the status as it
// was (a title, an assignee, a link) don't count. When the change that
// moved it was pruned, neither is known, and neither is given: a later
// edit at that status isn't taken for the move.
func statusSet(ctx context.Context, d dialect, tx *sql.Tx, ts []TrackerTask) error {
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
	rows, err := tx.QueryContext(ctx, statusSetQuery(d), string(b))
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
// whether it was kept, which past the comments a task keeps it isn't. It
// is noted as the tracker job's, and isn't told: the tracker told people.
//
// What doesn't show is kept as what does, not refused, so the comment is
// kept under its id and isn't sent again each run, ahead of those after
// it: in the text, each character that doesn't show as written (AsSent)
// is U+FFFD; a name that doesn't show is "someone", and one too long is
// cut. A time after now is now: the tracker's clock, or this one's, is a
// little off.
func (l *Ledger) CopyComment(ctx context.Context, task int64, body, author, ref string, at time.Time) (bool, error) {
	body = strings.TrimSpace(shownText(body))
	author = strings.TrimSpace(author)
	switch {
	case author == "" || Hidden(author) || !utf8.ValidString(author):
		author = "someone"
	case utf8.RuneCountInString(author) > maxAuthor:
		author = string([]rune(author)[:maxAuthor-1]) + "…"
	}
	switch {
	case body == "":
		return false, invalid("a comment says something")
	case utf8.RuneCountInString(body) > 10000:
		return false, invalid("a comment is %d characters at most", 10000)
	case !commentRef.MatchString(ref):
		return false, invalid("a comment's ref is the tracker's id for it: letters, digits and ._:-, 100 at most")
	case !sane(at):
		return false, invalid("a comment's time is when it was written")
	}
	if now := time.Now(); at.After(now) {
		at = now
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
		res, err := t.ExecContext(t.ctx, t.d.pick(`INSERT OR IGNORE INTO comments (on_kind, on_key, body, created, created_by, updated, origin, ref, author)
			SELECT 'task', ?, ?, ?, ?, ?, ?, ?, ? WHERE ? < ?`, `INSERT INTO comments (on_kind, on_key, body, created, created_by, updated, origin, ref, author)
			SELECT 'task', ?, ?, ?, ?, ?, ?, ?, ? WHERE CAST(? AS BIGINT) < CAST(? AS BIGINT) ON CONFLICT DO NOTHING`), key, body, at.Unix(), TaskTracker, at.Unix(), OriginTracker, ref, author, n, MaxCopied)
		if err != nil {
			return err
		}
		if k, _ := res.RowsAffected(); k == 0 {
			// Kept already, or the task keeps as many as it does: either
			// way, nothing more, and nothing for the job to fail on.
			return nil
		}
		kept = true
		return change(t, TaskTracker, fmt.Sprintf("comment on task %d copied in from its issue", task), nil, nil)
	})
	return kept, err
}

// maxAuthor is the longest name a comment copied in is kept with.
const maxAuthor = 100

// AsSent reports whether CopyComment keeps a comment's text and its
// author's name as they were sent.
func AsSent(body, author string) bool {
	author = strings.TrimSpace(author)
	return textShows(body) && author != "" && !Hidden(author) && utf8.ValidString(author) && utf8.RuneCountInString(author) <= maxAuthor
}

// textShows reports whether a comment's text shows as written: no control
// character but tabs and line breaks, and nothing that turns text around
// (the marks and embeddings, isolates and overrides of bidirectional
// text, U+061C, U+200E, U+200F, U+202A to U+202E, U+2066 to U+2069).
// Other format characters are people's writing: a zero-width joiner in an
// emoji, a soft hyphen.
func textShows(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if turns(r) {
			return false
		}
	}
	return true
}

// turns reports whether a character of a comment's text doesn't show as
// written, or turns the text around it (textShows).
func turns(r rune) bool {
	switch {
	case r == '\n' || r == '\r' || r == '\t':
		return false
	case unicode.IsControl(r), r == 0x061C, r == 0x200E || r == 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// shownText is a comment's text with each character that doesn't show as
// written (textShows) as U+FFFD, which does.
func shownText(s string) string {
	return strings.Map(func(r rune) rune {
		if turns(r) {
			return utf8.RuneError
		}
		return r
	}, strings.ToValidUTF8(s, "\uFFFD"))
}

// MarkPosted notes that a comment people wrote on a task was posted to the
// task's issue, as the tracker's comment ref: the job reads it back, and
// doesn't post it again, however many comments the issue has since. Set
// once: the same again changes nothing.
func (l *Ledger) MarkPosted(ctx context.Context, task, comment int64, ref string) error {
	if !commentRef.MatchString(ref) {
		return invalid("a comment's ref is the tracker's id for it: letters, digits and ._:-, 100 at most")
	}
	return l.write(ctx, func(t *txn) error {
		var origin, was string
		err := t.QueryRowContext(t.ctx, "SELECT origin, ref FROM comments WHERE id = ? AND on_kind = 'task' AND on_key = ?", comment, fmt.Sprint(task)).Scan(&origin, &was)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		case origin != "":
			return invalid("only a comment people wrote here is posted to the tracker")
		case was == ref:
			return nil
		case was != "":
			return invalid("the comment was posted already, as another")
		}
		// A ref is one comment's on a task: another's, copied in or posted,
		// is said, not left to the index to refuse.
		var other int64
		err = t.QueryRowContext(t.ctx, "SELECT id FROM comments WHERE on_kind = 'task' AND on_key = ? AND ref = ?", fmt.Sprint(task), ref).Scan(&other)
		switch {
		case err == nil:
			return fmt.Errorf("the tracker's comment %s is comment %d's on this task: %w", ref, other, ErrTaken)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE comments SET ref = ? WHERE id = ?", ref, comment); err != nil {
			return err
		}
		return change(t, TaskTracker, fmt.Sprintf("comment %d posted to task %d's issue", comment, task), nil, nil)
	})
}
