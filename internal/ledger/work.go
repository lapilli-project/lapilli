package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// What people keep beside what the runs find: teams, notes on rules, quiet
// windows, follow-ups and comments. Every write is noted in changes.

// ErrInvalid is something written that can't be kept as it is. Format and
// Args are kept apart, so the message can be shown in the reader's
// language.
type ErrInvalid struct {
	Format string
	Args   []any
}

func (e ErrInvalid) Error() string { return fmt.Sprintf(e.Format, e.Args...) }

func invalid(format string, args ...any) error { return ErrInvalid{format, args} }

// ErrNotAuthor is a comment someone else wrote.
var ErrNotAuthor = errors.New("only who wrote a comment can change it")

// checkText checks a text is at most most characters; tooLong says so
// with %d for most.
func checkText(tooLong, s string, most int) error {
	if utf8.RuneCountInString(s) > most {
		return invalid(tooLong, most)
	}
	return nil
}

// checkRE compiles a rule pattern, and refuses one that would take long to
// match: Go's are linear in what they match, but can be long programs.
func checkRE(s string) (*regexp.Regexp, error) {
	if s == "" {
		return nil, nil
	}
	if len(s) > 200 {
		return nil, invalid("a rule pattern is 200 bytes at most")
	}
	re, err := regexp.Compile(s)
	if err != nil {
		return nil, invalid("rule pattern %q: %v", s, err)
	}
	p, err := syntax.Parse(s, syntax.Perl)
	if err == nil {
		var prog *syntax.Prog
		if prog, err = syntax.Compile(p.Simplify()); err == nil && len(prog.Inst) > 1000 {
			return nil, invalid("rule pattern %q is too complex to match quickly", s)
		}
	}
	return re, nil
}

func checkLabels(l incident.Labels) error {
	if len(l) > 16 {
		return invalid("16 labels at most")
	}
	for k, vs := range l {
		if k == "" || len(k) > 128 || len(vs) > 64 || slices.ContainsFunc(vs, func(v string) bool { return v == "" || len(v) > 256 }) {
			return invalid("a label is a name of up to 128 bytes, with up to 64 values of up to 256")
		}
	}
	return nil
}

func labelsText(l incident.Labels) string {
	b, _ := json.Marshal(orEmpty(l))
	return string(b)
}

func readLabels(s string) (incident.Labels, error) {
	var l incident.Labels
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		return nil, err
	}
	if len(l) == 0 {
		return nil, nil
	}
	return l, nil
}

// Team is a team: it owns the rules whose name matches Rules, or whose
// labels have the values in Labels, and the incidents with those values.
type Team struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Rules  string          `json:"rules,omitempty"`
	Labels incident.Labels `json:"labels,omitempty"`
	Note   string          `json:"note,omitempty"`
	// WorkHours, SleepHours and TZ are the team's own hours, as
	// --work-hours, --sleep-hours and --tz take them: its rules' firings are
	// counted asleep or off hours by them. Each empty is the server's.
	WorkHours  string `json:"work_hours,omitempty"`
	SleepHours string `json:"sleep_hours,omitempty"`
	TZ         string `json:"tz,omitempty"`
	// Shifts, as --shifts takes them, are hours that take turns in place of
	// those: each turn's hours left out are the team's own, or the
	// server's. A team's own hours or shifts replace the server's shifts.
	Shifts  string    `json:"shifts,omitempty"`
	Updated time.Time `json:"updated"`
	By      string    `json:"by,omitempty"`
}

// Hours are the team's own hours, what it doesn't set taken from server's;
// nil when it sets none of them, and its rules go by the server's.
func (t Team) Hours(server *verdict.Hours) (*verdict.Hours, error) {
	if t.WorkHours == "" && t.SleepHours == "" && t.TZ == "" && t.Shifts == "" {
		return nil, nil
	}
	h := *verdict.DefaultHours(time.UTC)
	if server != nil {
		h = *server
	}
	h.Shifts = nil
	if len(t.TZ) > 64 || len(t.WorkHours) > 100 || len(t.SleepHours) > 100 {
		return nil, invalid("a team's time zone is 64 characters at most, and its hours 100")
	}
	if len(t.Shifts) > 1000 {
		return nil, invalid("a team's shifts are 1000 characters at most")
	}
	if t.TZ != "" {
		// Local is wherever the server runs, not the team.
		loc, err := time.LoadLocation(t.TZ)
		if err != nil || t.TZ == "Local" {
			return nil, invalid("%q isn't a time zone; want one like Europe/Berlin", t.TZ)
		}
		h.Loc = loc
	}
	if t.WorkHours != "" {
		if err := h.ParseWork(t.WorkHours); err != nil {
			return nil, invalid("%q aren't working hours; want days and hours, like mon-fri@08:00-18:00, or none", t.WorkHours)
		}
	}
	if t.SleepHours != "" {
		if err := h.ParseSleep(t.SleepHours); err != nil {
			return nil, invalid("%q aren't hours asleep; want hours like 22:00-08:00, or none", t.SleepHours)
		}
	}
	if t.Shifts != "" {
		sh, err := verdict.ParseShifts(t.Shifts, &h)
		if err != nil {
			why := err.Error()
			if se := (*verdict.ShiftsError)(nil); errors.As(err, &se) {
				why = se.Why
			}
			return nil, invalid("%q aren't shifts (%s); want hours that take turns, like every=1w from=2026-11-02T09:00; tz=Asia/Seoul; tz=Europe/Berlin", t.Shifts, why)
		}
		h.Shifts = sh
	}
	return &h, nil
}

var teamName = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,63}$`)

// Teams are every team, by name.
func (l *Ledger) Teams(ctx context.Context) ([]Team, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT id, name, rules, labels, note, work_hours, sleep_hours, tz, shifts, updated, updated_by FROM teams ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Team
	for rows.Next() {
		var t Team
		var labels string
		var at int64
		if err := rows.Scan(&t.ID, &t.Name, &t.Rules, &labels, &t.Note, &t.WorkHours, &t.SleepHours, &t.TZ, &t.Shifts, &at, &t.By); err != nil {
			return nil, err
		}
		if t.Labels, err = readLabels(labels); err != nil {
			return nil, err
		}
		t.Updated = unix(at)
		out = append(out, t)
	}
	return out, rows.Err()
}

// PutTeam makes a team, or changes the one with t's ID, and returns its ID.
func (l *Ledger) PutTeam(ctx context.Context, t Team, by string) (int64, error) {
	t.Name = strings.TrimSpace(t.Name)
	if !teamName.MatchString(t.Name) {
		return 0, invalid("a team's name is letters, digits, spaces and . _ -, up to 64")
	}
	if _, err := checkRE(t.Rules); err != nil {
		return 0, err
	}
	if err := errors.Join(checkLabels(t.Labels), checkText("a note is %d characters at most", t.Note, 5000)); err != nil {
		return 0, err
	}
	if t.Rules == "" && len(t.Labels) == 0 {
		return 0, invalid("a team owns rules by a name pattern, labels, or both")
	}
	t.WorkHours, t.SleepHours, t.TZ, t.Shifts = strings.TrimSpace(t.WorkHours), strings.TrimSpace(t.SleepHours), strings.TrimSpace(t.TZ), strings.TrimSpace(t.Shifts)
	if _, err := t.Hours(nil); err != nil {
		return 0, err
	}
	id := t.ID
	err := l.write(ctx, func(tx *txn) error {
		now := time.Now().Unix()
		var res sql.Result
		var err error
		if id == 0 {
			res, err = tx.ExecContext(tx.ctx, "INSERT INTO teams (name, rules, labels, note, work_hours, sleep_hours, tz, shifts, updated, updated_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				t.Name, t.Rules, labelsText(t.Labels), t.Note, t.WorkHours, t.SleepHours, t.TZ, t.Shifts, now, by)
		} else {
			res, err = tx.ExecContext(tx.ctx, "UPDATE teams SET name = ?, rules = ?, labels = ?, note = ?, work_hours = ?, sleep_hours = ?, tz = ?, shifts = ?, updated = ?, updated_by = ? WHERE id = ?",
				t.Name, t.Rules, labelsText(t.Labels), t.Note, t.WorkHours, t.SleepHours, t.TZ, t.Shifts, now, by, id)
		}
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return invalid("there is a team called %s already", t.Name)
			}
			return err
		}
		if id == 0 {
			id, _ = res.LastInsertId()
		} else if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return change(tx, by, "team "+t.Name+" kept", nil, t)
	})
	return id, err
}

// DeleteTeam deletes a team; what it owned is no one's.
func (l *Ledger) DeleteTeam(ctx context.Context, id int64, by string) error {
	return l.write(ctx, func(t *txn) error {
		var name string
		if err := t.QueryRowContext(t.ctx, "SELECT name FROM teams WHERE id = ?", id).Scan(&name); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM teams WHERE id = ?", id); err != nil {
			return err
		}
		return change(t, by, "team "+name+" deleted", nil, nil)
	})
}

// Note is what people keep about a rule.
type Note struct {
	Rule string `json:"rule"`
	// TeamID is the team that owns it, if not the one the teams' matchers
	// give; 0 for that one.
	TeamID int64 `json:"team_id,omitempty"`
	// Status is what is to become of it: keep, tune, retire, or "".
	Status  string    `json:"status,omitempty"`
	Runbook string    `json:"runbook,omitempty"`
	Note    string    `json:"note,omitempty"`
	Updated time.Time `json:"updated"`
	By      string    `json:"by,omitempty"`
}

// Statuses are what can become of a rule.
var Statuses = []string{"keep", "tune", "retire"}

// Notes are the notes on rules, by rule.
func (l *Ledger) Notes(ctx context.Context) (map[string]Note, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT rule, coalesce(team_id, 0), status, runbook, note, updated, updated_by FROM notes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Note{}
	for rows.Next() {
		var n Note
		var at int64
		if err := rows.Scan(&n.Rule, &n.TeamID, &n.Status, &n.Runbook, &n.Note, &at, &n.By); err != nil {
			return nil, err
		}
		n.Updated = unix(at)
		out[n.Rule] = n
	}
	return out, rows.Err()
}

// PutNote keeps a note on a rule; an empty one is taken away.
func (l *Ledger) PutNote(ctx context.Context, n Note, by string) error {
	if n.Rule == "" || len(n.Rule) > 500 {
		return invalid("a note is on a rule")
	}
	if n.Status != "" && !slices.Contains(Statuses, n.Status) {
		return invalid("status %q: want keep, tune, retire or none", n.Status)
	}
	if n.Runbook != "" {
		u, err := url.Parse(n.Runbook)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(n.Runbook) > 500 {
			return invalid("a runbook is an http or https URL, up to 500 bytes")
		}
		if u.User != nil {
			return invalid("a link has no user name in it: one that looks like a site's and goes to another")
		}
	}
	if err := checkText("a note is %d characters at most", n.Note, 5000); err != nil {
		return err
	}
	return l.write(ctx, func(t *txn) error {
		if n.TeamID == 0 && n.Status == "" && n.Runbook == "" && n.Note == "" {
			if _, err := t.ExecContext(t.ctx, "DELETE FROM notes WHERE rule = ?", n.Rule); err != nil {
				return err
			}
			return change(t, by, "note on "+n.Rule+" taken away", nil, nil)
		}
		team := sql.NullInt64{Int64: n.TeamID, Valid: n.TeamID != 0}
		if team.Valid {
			var x int
			if err := t.QueryRowContext(t.ctx, "SELECT 1 FROM teams WHERE id = ?", n.TeamID).Scan(&x); err != nil {
				return invalid("no such team")
			}
		}
		if _, err := t.ExecContext(t.ctx, `INSERT INTO notes (rule, team_id, status, runbook, note, updated, updated_by) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (rule) DO UPDATE SET team_id = excluded.team_id, status = excluded.status, runbook = excluded.runbook,
				note = excluded.note, updated = excluded.updated, updated_by = excluded.updated_by`,
			n.Rule, team, n.Status, n.Runbook, n.Note, time.Now().Unix(), by); err != nil {
			return err
		}
		return change(t, by, "note on "+n.Rule+" kept", nil, n)
	})
}

// Window is a quiet window: a deploy, maintenance, a load test. A firing
// that starts in it, of a rule whose name matches Rules and with labels
// that have the values in Labels, and catches nothing, is excused.
type Window struct {
	ID    int64     `json:"id"`
	Title string    `json:"title"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Every repeats the window every day or every week, by the calendar of
	// the ledger's time zone; Until is the last day one starts on, zero for
	// good.
	Every   string          `json:"every,omitempty"`
	Until   time.Time       `json:"until,omitzero"`
	Rules   string          `json:"rules,omitempty"`
	Labels  incident.Labels `json:"labels,omitempty"`
	Updated time.Time       `json:"updated"`
	By      string          `json:"by,omitempty"`
	re      *regexp.Regexp
	ruled   map[string]bool // whether re matched each rule's name
	loc     *time.Location
}

// Everies are how often a quiet window can repeat.
var Everies = []string{"day", "week"}

func (w Window) days() int {
	if w.Every == "week" {
		return 7
	}
	return 1
}

func civil(t time.Time) int64 {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
}

// Covers reports whether a time falls in the window, or in one of its
// repeats.
func (w Window) Covers(t time.Time) bool {
	if w.Every == "" {
		return !t.Before(w.Start) && !t.After(w.End)
	}
	if t.Before(w.Start) {
		return false
	}
	loc := w.loc
	if loc == nil {
		loc = time.UTC
	}
	s, at, long := w.Start.In(loc), t.In(loc), w.End.Sub(w.Start)
	k := int((civil(at) - civil(s)) / int64(w.days()))
	for _, n := range []int{k, k - 1} {
		if n < 0 {
			continue
		}
		from := s.AddDate(0, 0, n*w.days())
		if !w.Until.IsZero() && civil(from) > civil(w.Until.In(loc)) {
			continue
		}
		if !t.Before(from) && !t.After(from.Add(long)) {
			return true
		}
	}
	return false
}

// span is the firing starts a window can excuse, up to now: its own, or
// from its first repeat to the last before now.
func (w Window) span(now time.Time) [2]int64 {
	if w.Every == "" {
		return [2]int64{w.Start.Unix(), w.End.Unix()}
	}
	hi := now
	if !w.Until.IsZero() {
		// The last repeat starts on Until's day, at most; it ends by the
		// same time a period later, by the calendar.
		loc := w.loc
		if loc == nil {
			loc = time.UTC
		}
		if last := w.Until.In(loc).AddDate(0, 0, w.days()).Add(w.End.Sub(w.Start)); last.Before(hi) {
			hi = last
		}
	}
	if hi.Before(w.Start) {
		hi = w.Start
	}
	return [2]int64{w.Start.Unix(), hi.Unix()}
}

// maxWindow is the longest a quiet window can last.
const maxWindow = 31 * 24 * time.Hour

// Excuses reports whether the window excuses a firing that caught
// nothing: it started in the window, its rule matches, and it has each
// label the window names, with one of its values.
func (w Window) Excuses(f firing.Firing) bool {
	if w.Every == "" && (f.Start.Before(w.Start) || f.Start.After(w.End)) {
		return false
	}
	if w.re != nil {
		// Each window meets the same few rules over and over.
		ok, seen := w.ruled[f.Rule]
		if !seen {
			ok = w.re.MatchString(f.Rule)
			if w.ruled != nil {
				w.ruled[f.Rule] = ok
			}
		}
		if !ok {
			return false
		}
	}
	for k, vs := range w.Labels {
		if v, ok := f.Labels[k]; !ok || !slices.Contains(vs, v) {
			return false
		}
	}
	// Last for a repeating window: working out its repeat costs the most.
	return w.Every == "" || w.Covers(f.Start)
}

// sameStones reports whether two windows excuse the same firings.
func sameStones(a, b Window) bool {
	return a.Start.Equal(b.Start) && a.End.Equal(b.End) && a.Every == b.Every && a.Until.Equal(b.Until) && a.Rules == b.Rules &&
		labelsText(a.Labels) == labelsText(b.Labels)
}

func (l *Ledger) windowsMeeting(ctx context.Context, q queryer, lo, hi int64) ([]Window, error) {
	// A window lasts maxWindow at most: one that started before that
	// can't meet [lo, hi], and the index skips it.
	return l.windows(ctx, q, `WHERE (every = '' AND start_at <= ? AND start_at >= ? AND end_at >= ?)
		OR (every != '' AND start_at <= ? AND (until_at IS NULL OR until_at >= ?)) ORDER BY start_at, id`,
		hi, lo-int64(maxWindow/time.Second), lo, hi, lo-int64(maxWindow/time.Second))
}

func (l *Ledger) windows(ctx context.Context, q queryer, where string, args ...any) ([]Window, error) {
	rows, err := q.QueryContext(ctx, "SELECT id, title, start_at, end_at, every, until_at, rules, labels, updated, updated_by FROM windows "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Window
	for rows.Next() {
		var w Window
		var start, end, at int64
		var until sql.NullInt64
		var labels string
		if err := rows.Scan(&w.ID, &w.Title, &start, &end, &w.Every, &until, &w.Rules, &labels, &at, &w.By); err != nil {
			return nil, err
		}
		w.Start, w.End, w.Updated, w.Until, w.loc = unix(start), unix(end), unix(at), fromNull(until), l.loc
		if w.Labels, err = readLabels(labels); err != nil {
			return nil, err
		}
		w.re, _ = regexp.Compile(w.Rules)
		if w.Rules == "" {
			w.re = nil
		}
		w.ruled = map[string]bool{}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Windows are the quiet windows that meet [from, to], the earliest first.
func (l *Ledger) Windows(ctx context.Context, from, to time.Time) ([]Window, error) {
	return l.windowsMeeting(ctx, l.r, from.Unix(), to.Unix())
}

// WindowByID is one quiet window.
func (l *Ledger) WindowByID(ctx context.Context, id int64) (Window, error) {
	ws, err := l.windows(ctx, l.r, "WHERE id = ?", id)
	if err != nil {
		return Window{}, err
	}
	if len(ws) == 0 {
		return Window{}, ErrNotFound
	}
	return ws[0], nil
}

// PutWindow makes a quiet window, or changes the one with w's ID, gives the
// firings it meets, before and after, their stones again, and returns its
// ID.
func (l *Ledger) PutWindow(ctx context.Context, w Window, by string, o verdict.Options) (int64, error) {
	w.Title = strings.TrimSpace(w.Title)
	switch {
	case w.Title == "":
		return 0, invalid("a quiet window needs a title")
	case w.Start.IsZero() || w.End.IsZero() || !w.End.After(w.Start):
		return 0, invalid("a quiet window needs a start, and an end after it")
	case w.End.Sub(w.Start) > maxWindow:
		return 0, invalid("a quiet window lasts 31 days at most")
	case w.Every != "" && !slices.Contains(Everies, w.Every):
		return 0, invalid("a quiet window repeats every day or every week")
	case w.Every == "day" && w.End.Sub(w.Start) >= 24*time.Hour,
		w.Every == "week" && w.End.Sub(w.Start) >= 7*24*time.Hour:
		return 0, invalid("a repeating quiet window is shorter than how often it repeats")
	case w.Every != "" && !w.Until.IsZero() && w.Until.Before(w.Start):
		return 0, invalid("a quiet window repeats until a day after it starts")
	}
	if w.Every == "" {
		w.Until = time.Time{}
	}
	if _, err := checkRE(w.Rules); err != nil {
		return 0, err
	}
	if err := errors.Join(checkLabels(w.Labels), checkText("a title is %d characters at most", w.Title, 200)); err != nil {
		return 0, err
	}
	id := w.ID
	err := l.write(ctx, func(t *txn) error {
		w.loc = l.loc
		spans := [][2]int64{w.span(time.Now())}
		now := time.Now().Unix()
		if id == 0 {
			res, err := t.ExecContext(t.ctx, `INSERT INTO windows (title, start_at, end_at, every, until_at, rules, labels, updated, updated_by)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				w.Title, w.Start.Unix(), w.End.Unix(), w.Every, nullable(w.Until), w.Rules, labelsText(w.Labels), now, by)
			if err != nil {
				return err
			}
			id, _ = res.LastInsertId()
			w.ID = id
			kept, err := l.windows(t.ctx, t, "WHERE id = ?", id)
			if err != nil || len(kept) == 0 {
				return errors.Join(err, ErrNotFound)
			}
			if err := l.tell(t, "window.kept", by, map[string]any{"window": kept[0]}); err != nil {
				return err
			}
		} else {
			was, err := l.windows(t.ctx, t, "WHERE id = ?", id)
			if err != nil {
				return err
			}
			if len(was) == 0 {
				return ErrNotFound
			}
			spans = append(spans, was[0].span(time.Now()))
			if sameStones(was[0], w) { // a new title, say: no stone changes
				spans = nil
			}
			if _, err := t.ExecContext(t.ctx, `UPDATE windows SET title = ?, start_at = ?, end_at = ?, every = ?, until_at = ?, rules = ?, labels = ?,
				updated = ?, updated_by = ? WHERE id = ?`,
				w.Title, w.Start.Unix(), w.End.Unix(), w.Every, nullable(w.Until), w.Rules, labelsText(w.Labels), now, by, id); err != nil {
				return err
			}
		}
		if err := change(t, by, "quiet window "+w.Title+" kept", nil, w); err != nil {
			return err
		}
		return pend(t, spans)
	})
	if err != nil {
		return 0, err
	}
	l.judgeSoon(ctx, o)
	return id, nil
}

// judgeBudget is how long a write that leaves spans pending judges them
// before it returns: a short window is done by then; the rest of a long
// one, the worker carries on.
const judgeBudget = 2 * time.Second

func (l *Ledger) judgeSoon(ctx context.Context, o verdict.Options) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), judgeBudget)
	defer cancel()
	l.JudgePending(ctx, o)
}

// DeleteWindow deletes a quiet window, and gives the firings it excused
// their stones again.
func (l *Ledger) DeleteWindow(ctx context.Context, id int64, by string, o verdict.Options) error {
	err := l.write(ctx, func(t *txn) error {
		was, err := l.windows(t.ctx, t, "WHERE id = ?", id)
		if err != nil {
			return err
		}
		if len(was) == 0 {
			return ErrNotFound
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM windows WHERE id = ?", id); err != nil {
			return err
		}
		if err := change(t, by, "quiet window "+was[0].Title+" deleted", nil, nil); err != nil {
			return err
		}
		return pend(t, [][2]int64{was[0].span(time.Now())})
	})
	if err != nil {
		return err
	}
	l.judgeSoon(ctx, o)
	return nil
}

// pend notes spans whose firings are to be given stones again: a window's
// can be long, and are judged a week at a time after the write, each week
// in a write of its own. Noted in the same write as the window, a span
// isn't lost if lapilli stops halfway.
func pend(t *txn, spans [][2]int64) error {
	// Before the oldest firing kept there is nothing to judge: a window
	// that started years ago is judged from there.
	var oldest sql.NullInt64
	if err := t.QueryRowContext(t.ctx, "SELECT MIN(start_at) FROM firings").Scan(&oldest); err != nil {
		return err
	}
	var some [][2]int64
	for _, s := range spans {
		if oldest.Valid && s[1] >= oldest.Int64 {
			some = append(some, [2]int64{max(s[0], oldest.Int64), s[1]})
		}
	}
	if spans = some; len(spans) == 0 {
		return nil
	}
	v, err := (&Ledger{}).meta(t.ctx, t, "pending")
	if err != nil {
		return err
	}
	var have [][2]int64
	if v != "" {
		if err := json.Unmarshal([]byte(v), &have); err != nil {
			return err
		}
	}
	b, err := json.Marshal(merge(append(have, spans...)))
	if err != nil {
		return err
	}
	return setMeta(t.ctx, t, "pending", string(b))
}

// JudgePending gives stones again over the spans pended, a week at a time.
func (l *Ledger) JudgePending(ctx context.Context, o verdict.Options) error {
	const week = 7 * 86400
	for {
		var done bool
		err := l.write(ctx, func(t *txn) error {
			v, err := l.meta(t.ctx, t, "pending")
			if err != nil || v == "" || v == "null" || v == "[]" {
				done = true
				return err
			}
			var spans [][2]int64
			if err := json.Unmarshal([]byte(v), &spans); err != nil {
				return err
			}
			if len(spans) == 0 {
				done = true
				return nil
			}
			s := spans[0]
			hi := min(s[1], s[0]+week-1)
			if err := l.judge(t, s[0], hi, o); err != nil {
				return err
			}
			if hi >= s[1] {
				spans = spans[1:]
			} else {
				spans[0][0] = hi + 1
			}
			b, err := json.Marshal(spans)
			if err != nil {
				return err
			}
			return setMeta(t.ctx, t, "pending", string(b))
		})
		if err != nil || done {
			return err
		}
	}
}

// Task is a follow-up: something someone is to do about a rule or an
// incident.
type Task struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	// Kind is what is to be done: tune, write, retire, decide, or other.
	Kind     string    `json:"kind,omitempty"`
	Rule     string    `json:"rule,omitempty"`
	Incident string    `json:"incident,omitempty"`
	TeamID   int64     `json:"team_id,omitempty"`
	Assignee string    `json:"assignee,omitempty"`
	Status   string    `json:"status"`
	Due      time.Time `json:"due,omitzero"`
	Body     string    `json:"body,omitempty"`
	// Link is the task's issue in a tracker, if it has one.
	Link string `json:"link,omitempty"`
	// Done is when it was done, while it is.
	Done      time.Time `json:"done,omitzero"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by,omitempty"`
	Updated   time.Time `json:"updated"`
	By        string    `json:"by,omitempty"`
}

// Kinds and TaskStatuses are what a task can be, and be at.
var (
	Kinds        = []string{"tune", "write", "retire", "decide", "other"}
	TaskStatuses = []string{"open", "doing", "done", "dropped"}
)

// TaskQuery picks tasks: of these statuses (any when none), about a rule
// or an incident, of a team, the latest changed first, Limit of them.
type TaskQuery struct {
	Status   []string
	Rule     string
	Incident string
	TeamID   int64
	Limit    int
}

func scanTask(s scanner) (Task, error) {
	var t Task
	var due, done sql.NullInt64
	var created, updated int64
	err := s.Scan(&t.ID, &t.Title, &t.Kind, &t.Rule, &t.Incident, &t.TeamID, &t.Assignee, &t.Status, &due, &t.Body, &t.Link,
		&done, &created, &t.CreatedBy, &updated, &t.By)
	t.Due, t.Done, t.Created, t.Updated = fromNull(due), fromNull(done), unix(created), unix(updated)
	return t, err
}

const taskColumns = `id, title, kind, rule, incident, coalesce(team_id, 0), assignee, status, due, body, link, done_at, created, created_by, updated, updated_by`

// Tasks are the tasks a query picks.
func (l *Ledger) Tasks(ctx context.Context, q TaskQuery) ([]Task, error) {
	sq := "SELECT " + taskColumns + " FROM tasks WHERE 1 = 1"
	var args []any
	if len(q.Status) > 0 {
		b, _ := json.Marshal(q.Status)
		sq += " AND status IN (SELECT value FROM json_each(?))"
		args = append(args, string(b))
	}
	if q.Rule != "" {
		sq += " AND rule = ?"
		args = append(args, q.Rule)
	}
	if q.Incident != "" {
		sq += " AND incident = ?"
		args = append(args, q.Incident)
	}
	if q.TeamID != 0 {
		sq += " AND team_id = ?"
		args = append(args, q.TeamID)
	}
	sq += " ORDER BY updated DESC, id DESC LIMIT ?"
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
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

// TaskByID is one task.
func (l *Ledger) TaskByID(ctx context.Context, id int64) (Task, error) {
	t, err := scanTask(l.r.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// PutTask makes a task, or changes the one with t's ID, and returns its ID.
func (l *Ledger) PutTask(ctx context.Context, t Task, by string) (int64, error) {
	return l.putTask(ctx, t.ID, func(Task) (Task, error) { return t, nil }, by)
}

// PatchTask changes only the fields patch, a task as JSON, has, to the task
// as it is when written: a change made meanwhile isn't undone.
func (l *Ledger) PatchTask(ctx context.Context, id int64, patch []byte, by string) (int64, error) {
	if id == 0 {
		return 0, ErrNotFound
	}
	return l.putTask(ctx, id, func(was Task) (Task, error) {
		t := was
		if err := json.Unmarshal(patch, &t); err != nil {
			return t, invalid("want JSON: %v", err)
		}
		t.ID = id
		return t, nil
	}, by)
}

// putTask makes the task edit gives (from a zero Task when id is 0), or
// changes the task with id to what edit gives from it, in one write.
func (l *Ledger) putTask(ctx context.Context, id int64, edit func(was Task) (Task, error), by string) (int64, error) {
	err := l.write(ctx, func(tx *txn) error {
		var was Task
		if id != 0 {
			var err error
			was, err = scanTask(tx.QueryRowContext(tx.ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = ?", id))
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			} else if err != nil {
				return err
			}
		}
		t, err := edit(was)
		if err != nil {
			return err
		}
		if err := checkTask(&t); err != nil {
			return err
		}
		now := time.Now().Unix()
		team := sql.NullInt64{Int64: t.TeamID, Valid: t.TeamID != 0}
		if team.Valid {
			var x int
			if err := tx.QueryRowContext(tx.ctx, "SELECT 1 FROM teams WHERE id = ?", t.TeamID).Scan(&x); err != nil {
				return invalid("no such team")
			}
		}
		// When a task was done: kept while it stays done, for the digest.
		done := sql.NullInt64{Int64: now, Valid: t.Status == "done"}
		if was.Status == "done" && done.Valid && !was.Done.IsZero() {
			done.Int64 = was.Done.Unix()
		}
		what := fmt.Sprintf("task %d changed", id)
		if id == 0 {
			res, err := tx.ExecContext(tx.ctx, `INSERT INTO tasks (title, kind, rule, incident, team_id, assignee, status, due, body, link, done_at,
				created, created_by, updated, updated_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				t.Title, t.Kind, t.Rule, t.Incident, team, t.Assignee, t.Status, nullable(t.Due), t.Body, t.Link, done, now, by, now, by)
			if err != nil {
				return err
			}
			id, _ = res.LastInsertId()
			what = fmt.Sprintf("task %d made", id)
		} else if _, err := tx.ExecContext(tx.ctx, `UPDATE tasks SET title = ?, kind = ?, rule = ?, incident = ?, team_id = ?, assignee = ?,
			status = ?, due = ?, body = ?, link = ?, done_at = ?, updated = ?, updated_by = ? WHERE id = ?`,
			t.Title, t.Kind, t.Rule, t.Incident, team, t.Assignee, t.Status, nullable(t.Due), t.Body, t.Link, done, now, by, id); err != nil {
			return err
		}
		// What is told is the task as kept, as the API gives it.
		kept, err := scanTask(tx.QueryRowContext(tx.ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = ?", id))
		if err != nil {
			return err
		}
		if was.ID == 0 {
			if err := l.tell(tx, "task.created", by, map[string]any{"task": kept}); err != nil {
				return err
			}
		}
		if was.Assignee != kept.Assignee && kept.Assignee != "" {
			if err := l.tell(tx, "task.assigned", by, map[string]any{"task": kept, "was": was.Assignee}); err != nil {
				return err
			}
		}
		// Not what a tracker job moves as its issue was moved: the tracker
		// has told people, and whoever holds the job's token can't fill
		// the outbox.
		if was.ID != 0 && was.Status != kept.Status && by != TaskTracker {
			if err := l.tell(tx, "task.status", by, map[string]any{"task": kept, "was": was.Status}); err != nil {
				return err
			}
		}
		return change(tx, by, what, nil, kept)
	})
	return id, err
}

// checkTask fills in a task's defaults, and refuses one that can't be kept.
func checkTask(t *Task) error {
	t.Title = strings.TrimSpace(t.Title)
	if t.Status == "" {
		t.Status = "open"
	}
	if t.Kind == "" {
		t.Kind = "other"
	}
	switch {
	case t.Title == "":
		return invalid("a task needs a title")
	case !slices.Contains(Kinds, t.Kind):
		return invalid("kind %q: want one of %s", t.Kind, strings.Join(Kinds, ", "))
	case !slices.Contains(TaskStatuses, t.Status):
		return invalid("status %q: want one of %s", t.Status, strings.Join(TaskStatuses, ", "))
	}
	if strings.ContainsFunc(t.Assignee, unicode.IsControl) {
		return invalid("an assignee is a name, on one line")
	}
	if t.Link != "" {
		u, err := url.Parse(t.Link)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(t.Link) > maxLink {
			return invalid("a task's link is an http or https URL, up to %d bytes", maxLink)
		}
		if u.User != nil {
			return invalid("a link has no user name in it: one that looks like a site's and goes to another")
		}
	}
	return errors.Join(checkText("a title is %d characters at most", t.Title, 200), checkText("a task's text is %d characters at most", t.Body, 10000),
		checkText("an assignee is %d characters at most", t.Assignee, 200), checkText("a rule's name is %d characters at most", t.Rule, 500),
		checkText("an incident's id is %d characters at most", t.Incident, 64))
}

// DeleteTask deletes a task and its comments.
func (l *Ledger) DeleteTask(ctx context.Context, id int64, by string) error {
	return l.write(ctx, func(t *txn) error {
		res, err := t.ExecContext(t.ctx, "DELETE FROM tasks WHERE id = ?", id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM comments WHERE on_kind = 'task' AND on_key = ?", fmt.Sprint(id)); err != nil {
			return err
		}
		return change(t, by, fmt.Sprintf("task %d deleted", id), nil, nil)
	})
}

// Comment is something said about a rule, an incident or a task.
type Comment struct {
	ID      int64     `json:"id"`
	On      string    `json:"on"`  // rule, incident or task
	Key     string    `json:"key"` // its name or ID
	Body    string    `json:"body"`
	Created time.Time `json:"created"`
	By      string    `json:"by,omitempty"`
	Updated time.Time `json:"updated"`
}

// Comments are what was said about something, the earliest first.
func (l *Ledger) Comments(ctx context.Context, on, key string) ([]Comment, error) {
	return l.comments(ctx, "WHERE on_kind = ? AND on_key = ? ORDER BY created, id", on, key)
}

func (l *Ledger) comments(ctx context.Context, where string, args ...any) ([]Comment, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT id, on_kind, on_key, body, created, created_by, updated FROM comments "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		var c Comment
		var created, updated int64
		if err := rows.Scan(&c.ID, &c.On, &c.Key, &c.Body, &created, &c.By, &updated); err != nil {
			return nil, err
		}
		c.Created, c.Updated = unix(created), unix(updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutComment says something, or changes what was said with c's ID: only
// its author can, unless it was said anonymously. It returns the ID.
func (l *Ledger) PutComment(ctx context.Context, c Comment, by string) (int64, error) {
	c.Body = strings.TrimSpace(c.Body)
	switch {
	case c.Body == "":
		return 0, invalid("a comment says something")
	case utf8.RuneCountInString(c.Body) > 10000:
		return 0, invalid("a comment is %d characters at most", 10000)
	case c.ID == 0 && !slices.Contains([]string{"rule", "incident", "task"}, c.On):
		return 0, invalid("a comment is on a rule, an incident or a task")
	case c.ID == 0 && (c.Key == "" || len(c.Key) > 500):
		return 0, invalid("a comment is on something")
	}
	id := c.ID
	err := l.write(ctx, func(t *txn) error {
		now := time.Now().Unix()
		if id == 0 {
			// A comment is on something there: what it names is told on.
			q := map[string]string{"rule": "SELECT 1 FROM rules WHERE name = ?", "incident": "SELECT 1 FROM incidents WHERE key = ?",
				"task": "SELECT 1 FROM tasks WHERE CAST(id AS TEXT) = ?"}[c.On]
			var there int
			if err := t.QueryRowContext(t.ctx, q, c.Key).Scan(&there); errors.Is(err, sql.ErrNoRows) {
				return invalid("there is no %s %q to comment on", c.On, c.Key)
			} else if err != nil {
				return err
			}
			res, err := t.ExecContext(t.ctx, "INSERT INTO comments (on_kind, on_key, body, created, created_by, updated) VALUES (?, ?, ?, ?, ?, ?)",
				c.On, c.Key, c.Body, now, by, now)
			if err != nil {
				return err
			}
			id, _ = res.LastInsertId()
			c.ID, c.Created, c.By, c.Updated = id, unix(now), by, unix(now)
			if err := l.tell(t, "comment.written", by, map[string]any{"comment": c}); err != nil {
				return err
			}
			return change(t, by, fmt.Sprintf("comment on %s %s", c.On, c.Key), nil, nil)
		}
		if err := l.author(t, id, by); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE comments SET body = ?, updated = ? WHERE id = ?", c.Body, now, id); err != nil {
			return err
		}
		return change(t, by, fmt.Sprintf("comment %d changed", id), nil, nil)
	})
	return id, err
}

func (l *Ledger) author(t *txn, id int64, by string) error {
	var was string
	if err := t.QueryRowContext(t.ctx, "SELECT created_by FROM comments WHERE id = ?", id).Scan(&was); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if was != by && was != "anonymous" {
		return ErrNotAuthor
	}
	return nil
}

// DeleteComment takes back what its author said.
func (l *Ledger) DeleteComment(ctx context.Context, id int64, by string) error {
	return l.write(ctx, func(t *txn) error {
		if err := l.author(t, id, by); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM comments WHERE id = ?", id); err != nil {
			return err
		}
		return change(t, by, fmt.Sprintf("comment %d deleted", id), nil, nil)
	})
}

// OpenTasks are, by team ID (0 for none), how many tasks are open or being
// done.
func (l *Ledger) OpenTasks(ctx context.Context) (map[int64]int, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT coalesce(team_id, 0), count(*) FROM tasks WHERE status IN ('open', 'doing') GROUP BY 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// CommentByID is one comment.
func (l *Ledger) CommentByID(ctx context.Context, id int64) (Comment, error) {
	cs, err := l.comments(ctx, "WHERE id = ?", id)
	if err != nil {
		return Comment{}, err
	}
	if len(cs) == 0 {
		return Comment{}, ErrNotFound
	}
	return cs[0], nil
}

// Overdue tells, once for each due day, of every task still open or being
// done whose due day has passed in the last week (not, when telling is
// first turned on, of each one ever left late). It returns how many it
// told of.
func (l *Ledger) Overdue(ctx context.Context, today time.Time) (int, error) {
	n := 0
	err := l.writeAside(ctx, func(t *txn) error {
		rows, err := t.QueryContext(t.ctx, "SELECT "+taskColumns+` FROM tasks WHERE status IN ('open', 'doing')
			AND due < ? AND due >= ? AND (overdue_told IS NULL OR overdue_told != due)`, today.Unix(), today.In(l.loc).AddDate(0, 0, -7).Unix())
		if err != nil {
			return err
		}
		var late []Task
		for rows.Next() {
			x, err := scanTask(rows)
			if err != nil {
				rows.Close()
				return err
			}
			late = append(late, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, x := range late {
			if _, err := t.ExecContext(t.ctx, "UPDATE tasks SET overdue_told = due WHERE id = ?", x.ID); err != nil {
				return err
			}
			if err := l.tell(t, "task.overdue", "", map[string]any{"task": x}); err != nil {
				return err
			}
		}
		n = len(late)
		return nil
	})
	return n, err
}

// TaskCounts are how the tasks stood at to: open or being done, of those
// due before to, and done between from and to.
func (l *Ledger) TaskCounts(ctx context.Context, from, to time.Time) (open, overdue, done int, err error) {
	return l.TeamTaskCounts(ctx, from, to, 0)
}

// TeamTaskCounts is TaskCounts for one team's tasks; everyone's with no team.
func (l *Ledger) TeamTaskCounts(ctx context.Context, from, to time.Time, team int64) (open, overdue, done int, err error) {
	err = l.r.QueryRowContext(ctx, `SELECT
		COUNT(CASE WHEN status IN ('open', 'doing') THEN 1 END),
		COUNT(CASE WHEN status IN ('open', 'doing') AND due < ?1 THEN 1 END),
		COUNT(CASE WHEN status = 'done' AND done_at >= ?2 AND done_at < ?1 THEN 1 END)
		FROM tasks WHERE ?3 = 0 OR team_id = ?3`, to.Unix(), from.Unix(), team).Scan(&open, &overdue, &done)
	return
}
