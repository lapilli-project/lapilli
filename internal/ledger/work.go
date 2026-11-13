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
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/schedule"
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
		// A team made, renamed, or owning by another pattern or labels may
		// change who owns a rule a team's window quiets.
		var name, rules, labels string
		if id != 0 {
			if err := tx.QueryRowContext(tx.ctx, "SELECT name, rules, labels FROM teams WHERE id = ?", id).Scan(&name, &rules, &labels); err != nil &&
				!errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if id == 0 || name != t.Name || rules != t.Rules || labels != labelsText(t.Labels) {
			if err := l.pendTeamWindows(tx); err != nil {
				return err
			}
		}
		var res sql.Result
		var err error
		if id == 0 {
			err = tx.QueryRowContext(tx.ctx, "INSERT INTO teams (name, rules, labels, note, work_hours, sleep_hours, tz, shifts, updated, updated_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
				t.Name, t.Rules, labelsText(t.Labels), t.Note, t.WorkHours, t.SleepHours, t.TZ, t.Shifts, now, by).Scan(&id)
		} else {
			res, err = tx.ExecContext(tx.ctx, "UPDATE teams SET name = ?, rules = ?, labels = ?, note = ?, work_hours = ?, sleep_hours = ?, tz = ?, shifts = ?, updated = ?, updated_by = ? WHERE id = ?",
				t.Name, t.Rules, labelsText(t.Labels), t.Note, t.WorkHours, t.SleepHours, t.TZ, t.Shifts, now, by, id)
		}
		if err != nil {
			if unique(err) {
				return invalid("there is a team called %s already", t.Name)
			}
			return err
		}
		if res != nil {
			if n, _ := res.RowsAffected(); n == 0 {
				return ErrNotFound
			}
		}
		return change(tx, by, "team "+t.Name+" kept", nil, t)
	})
	return id, err
}

// DeleteTeam deletes a team; what it owned is no one's. Its reports and
// quiet windows stop: they keep its id negated, which no team has, as a team
// made later may be given its id. Its editors are no longer.
func (l *Ledger) DeleteTeam(ctx context.Context, id int64, by string) error {
	return l.write(ctx, func(t *txn) error {
		var name string
		if err := t.QueryRowContext(t.ctx, "SELECT name FROM teams WHERE id = ?", id).Scan(&name); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		// Its windows are judged again: they quiet nothing once it is gone,
		// and the rules it owned may be another team's.
		if err := l.pendTeamWindows(t); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM teams WHERE id = ?", id); err != nil {
			return err
		}
		// What was its stops, and its editors go with it, in this write: a
		// team made later may be given its id.
		for _, q := range []string{"UPDATE reports SET team_id = -team_id WHERE team_id = ?", "UPDATE windows SET team_id = -team_id WHERE team_id = ?",
			"DELETE FROM grants WHERE team_id = ?"} {
			if _, err := t.ExecContext(t.ctx, q, id); err != nil {
				return err
			}
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
		// A team's editors keep the rule's team as it is: moving a rule in or
		// out of a team is an editor's. Which rules are the team's, the
		// server checks: the ledger doesn't know each rule's labels.
		var was int64
		if err := t.QueryRowContext(t.ctx, "SELECT coalesce(team_id, 0) FROM notes WHERE rule = ?", n.Rule).Scan(&was); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, limited := scopeOf(t.ctx); limited && was != n.TeamID {
			return ErrNotYourTeam
		}
		if was != n.TeamID {
			// Who owns the rule changed: a team's window may quiet it now,
			// or no longer.
			if err := l.pendTeamWindows(t); err != nil {
				return err
			}
		}
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
	// Every repeats the window every day, week or month, or by a cron
	// expression, by the calendar of the ledger's time zone; Start and End
	// are its first repeat. On says when, for month (15, last, first sat)
	// and cron (its five fields). Until is the last day one starts on, zero
	// for good.
	Every string    `json:"every,omitempty"`
	On    string    `json:"on,omitempty"`
	Until time.Time `json:"until,omitzero"`
	// TeamID is the team whose window it is: it excuses only the firings
	// of rules the team owns. 0 for everyone's; its team's id negated once
	// the team was removed, when it excuses nothing.
	TeamID  int64           `json:"team_id,omitempty"`
	Rules   string          `json:"rules,omitempty"`
	Labels  incident.Labels `json:"labels,omitempty"`
	Updated time.Time       `json:"updated"`
	By      string          `json:"by,omitempty"`
	re      *regexp.Regexp
	ruled   map[string]bool // whether re matched each rule's name
	loc     *time.Location
	sch     *schedule.Schedule // for month and cron
	own     *ownership         // who owns what, for a team's window
}

// Everies are how often a quiet window can repeat.
var Everies = []string{"day", "week", "month", "cron"}

// scheduled reports whether a window repeats by a schedule: every month, or
// by a cron expression.
func (w Window) scheduled() bool { return w.Every == "month" || w.Every == "cron" }

// schedule is when a window repeating every month, or by a cron
// expression, falls due: a month's at its start's time of day.
func (w Window) schedule(loc *time.Location) (schedule.Schedule, error) {
	on := strings.ToLower(strings.Join(strings.Fields(w.On), " "))
	var sch schedule.Schedule
	var err error
	if w.Every == "month" {
		s := w.Start.In(loc)
		if on == "" {
			if s.Day() > 28 {
				return sch, invalid("a window every month on the 29th to the 31st isn't every month: give last, or a day from 1 to 28")
			}
			on = strconv.Itoa(s.Day())
		}
		sch, err = schedule.Parse(fmt.Sprintf("monthly %s@%02d:%02d", on, s.Hour(), s.Minute()), 0)
	} else {
		if sch, err = schedule.Parse(on, 0); err == nil && !sch.Cron() {
			return sch, invalid("a window by a cron expression is five cron fields, such as 0 2 * * 6")
		}
	}
	var se schedule.Error
	if errors.As(err, &se) {
		return sch, invalid(se.Format, se.Args...)
	}
	return sch, err
}

// resolve reads a window's schedule, for one repeating every month or by a
// cron expression, and keeps it.
func (w *Window) resolve() {
	if !w.scheduled() || w.sch != nil {
		return
	}
	if sch, err := w.schedule(w.where()); err == nil {
		w.sch = &sch
	}
}

func (w Window) where() *time.Location {
	if w.loc == nil {
		return time.UTC
	}
	return w.loc
}

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
	loc := w.where()
	if w.scheduled() {
		// Every repeat lasts as long: t is in the last that started at or
		// before it, or in none, as an earlier one ends before that one
		// does; the last to start by the end of Until's day, past it. So
		// whatever the gap between two, one lookup tells.
		w.resolve()
		if w.sch == nil {
			return false
		}
		due := w.sch.Last(w.lastAt(t), loc)
		if due.IsZero() || due.Before(w.Start) {
			return false
		}
		return !t.After(due.Add(w.End.Sub(w.Start)))
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
	if !w.Until.IsZero() && w.scheduled() {
		// The last repeat starts on Until's day, at most.
		loc := w.where()
		w.resolve()
		u := w.Until.In(loc)
		if w.sch != nil {
			if last := w.sch.Last(time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, loc).Add(-time.Second), loc); !last.IsZero() {
				if end := last.Add(w.End.Sub(w.Start)); end.Before(hi) {
					hi = end
				}
			}
		}
	} else if !w.Until.IsZero() {
		// The last repeat starts on Until's day, at most; it ends by the
		// same time a period later, by the calendar.
		loc := w.where()
		if last := w.Until.In(loc).AddDate(0, 0, w.days()).Add(w.End.Sub(w.Start)); last.Before(hi) {
			hi = last
		}
	}
	if hi.Before(w.Start) {
		hi = w.Start
	}
	return [2]int64{w.Start.Unix(), hi.Unix()}
}

// Through is when the last of a window's repeats up to now ends: its end,
// for one that doesn't repeat; now, for one that repeats for good.
func (w Window) Through(now time.Time) time.Time { return unix(w.span(now)[1]) }

// maxWindow is the longest a quiet window can last.
const maxWindow = 31 * 24 * time.Hour

// MaxWindows is how many quiet windows the ledger keeps, the last
// EditorWindows of them for editors and admins to make; MaxTeamWindows,
// how many of a team's its editors may make. Each window is looked at for
// every firing that caught nothing it may excuse, each time stones are
// given, in the ledger's one write transaction: a team's editors, who may
// change the least, can't make that slow for everyone.
const (
	MaxWindows     = 500
	EditorWindows  = 50
	MaxTeamWindows = 20
)

// windowsLeft refuses a new quiet window past the limits, for a team's
// editors those of a team's editors.
func windowsLeft(t *txn, team int64) error {
	var own, all int
	if err := t.QueryRowContext(t.ctx, "SELECT count(*) FILTER (WHERE team_id = ?), count(*) FROM windows", team).Scan(&own, &all); err != nil {
		return err
	}
	_, limited := scopeOf(t.ctx)
	switch {
	case all >= MaxWindows:
		return invalid("the ledger keeps %d quiet windows at most: delete those no longer needed", MaxWindows)
	case limited && own >= MaxTeamWindows:
		return invalid("a team's editors keep %d of its quiet windows at most; an editor or an admin can keep more", MaxTeamWindows)
	case limited && all >= MaxWindows-EditorWindows:
		return invalid("the last %d quiet windows the ledger keeps are for editors and admins to make", EditorWindows)
	}
	return nil
}

// mayCover reports whether the window may cover a time in [lo, hi]: false
// only when it covers none. Every repeat by a schedule lasts as long, so
// when the last to start by hi (by the end of Until's day, when that is
// earlier) ends before lo, or starts before the window does, every other
// one does too. One schedule lookup a span, not one a firing: a schedule
// that falls due once a year looks back up to a year each time.
func (w Window) mayCover(lo, hi time.Time) bool {
	if !w.scheduled() {
		return true
	}
	loc := w.where()
	w.resolve()
	if w.sch == nil {
		return false
	}
	due := w.sch.Last(w.lastAt(hi), loc)
	return !due.IsZero() && !due.Before(w.Start) && !due.Add(w.End.Sub(w.Start)).Before(lo)
}

// lastAt is t, or the end of Until's day when that is earlier: a window
// repeating by its schedule repeats last on Until's day.
func (w Window) lastAt(t time.Time) time.Time {
	if w.Until.IsZero() {
		return t
	}
	loc := w.where()
	u := w.Until.In(loc)
	if end := time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, loc).Add(-time.Nanosecond); t.After(end) {
		return end
	}
	return t
}

// dueMost is how many repeats of a window a span works out at most: past
// it, each firing asks Covers.
const dueMost = 20_000

// repeats are the times a window repeating by its schedule falls due whose
// repeat may cover a time in [from, to], the earliest first, worked out
// once a span: Covers looks back, for each firing, to the last time the
// schedule fell due, up to a year for one that falls due once a year.
// They are the times Covers would find, in order: the last by to, and
// each one before it, back to the window's start or one ending before
// from. ok is false past dueMost.
func (w Window) repeats(from, to time.Time) (due []time.Time, ok bool) {
	loc := w.where()
	w.resolve()
	if w.sch == nil {
		return nil, true
	}
	floor := from.Add(-w.End.Sub(w.Start))
	for d := w.sch.Last(w.lastAt(to), loc); !d.IsZero() && !d.Before(w.Start) && !d.Before(floor); d = w.sch.Prev(d, loc) {
		if len(due) == dueMost {
			return nil, false
		}
		due = append(due, d)
	}
	slices.Reverse(due)
	return due, true
}

// Excusing says whether one of the quiet windows ws excuses a firing, as
// Excuses does, for the firings of a span, [from, to]. What can is worked
// out once for the span: for a window repeating by its schedule, the
// times it falls due whose repeat may cover a time in the span (none: it
// is passed over), which a firing that starts in the span is set beside
// by a search; and the windows are sorted, once for each rule and the
// team that owns its alert, into those whose pattern and team the firing's
// may be, so each firing is set beside those alone.
func Excusing(ws []Window, from, to time.Time) func(firing.Firing) bool {
	q := &excusing{from: from, to: to, by: map[excusingKey][]int{}}
	for _, w := range ws {
		if w.TeamID < 0 {
			continue // a removed team's window excuses nothing
		}
		w.resolve()
		var due []time.Time
		known, covers := false, true
		if w.scheduled() {
			due, known = w.repeats(from, to)
			covers = len(due) > 0
			if !known {
				covers = w.mayCover(from, to)
			}
		}
		q.ws = append(q.ws, w)
		q.due = append(q.due, due)
		q.known = append(q.known, known)
		q.covers = append(q.covers, covers)
		if w.TeamID > 0 {
			q.own = w.own
		}
	}
	return q.excuses
}

type excusingKey struct {
	rule string
	team int64
	// outside is a firing that started outside the span: every window
	// that may be its is looked at.
	outside bool
}

type excusing struct {
	from, to time.Time
	ws       []Window
	covers   []bool // whether each window may cover a time in the span
	// due are, for a window repeating by its schedule, known, the times
	// it falls due whose repeat may cover a time in the span, in order.
	due   [][]time.Time
	known []bool
	own   *ownership // nil when no window is a team's
	by    map[excusingKey][]int
}

func (q *excusing) excuses(f firing.Firing) bool {
	if len(q.ws) == 0 {
		return false
	}
	k := excusingKey{rule: f.Rule, outside: f.Start.Before(q.from) || f.Start.After(q.to)}
	if q.own != nil {
		k.team = q.own.of(f.Rule, f.Labels)
	}
	is, ok := q.by[k]
	if !ok {
		for i, w := range q.ws {
			if (k.outside || q.covers[i]) && (w.TeamID == 0 || w.TeamID == k.team) && (w.re == nil || w.re.MatchString(f.Rule)) {
				is = append(is, i)
			}
		}
		q.by[k] = is
	}
	for _, i := range is {
		w := q.ws[i]
		if k.outside || !q.known[i] {
			if w.Excuses(f) {
				return true
			}
			continue
		}
		if w.matches(f) && covered(q.due[i], w, f.Start) {
			return true
		}
	}
	return false
}

// covered reports whether t is in the repeat of w that started last by t,
// of those due, as Covers says. They are those by the end of Until's day
// already.
func covered(due []time.Time, w Window, t time.Time) bool {
	if t.Before(w.Start) {
		return false
	}
	i := sort.Search(len(due), func(i int) bool { return due[i].After(t) }) - 1
	return i >= 0 && !t.After(due[i].Add(w.End.Sub(w.Start)))
}

// Excuses reports whether the window excuses a firing that caught
// nothing: it started in the window, its rule matches, and it has each
// label the window names, with one of its values.
func (w Window) Excuses(f firing.Firing) bool {
	// Last for a repeating window: working out its repeat costs the most.
	return w.matches(f) && (w.Every == "" || w.Covers(f.Start))
}

// matches reports whether the window excuses a firing as Excuses does but
// for when a repeating window falls due: a window's own time, if it
// doesn't repeat, its pattern, its labels and its team.
func (w Window) matches(f firing.Firing) bool {
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
	// A team's window quiets only its team's rules; one whose team was
	// removed, none.
	switch {
	case w.TeamID < 0:
		return false
	case w.TeamID > 0 && (w.own == nil || w.own.of(f.Rule, f.Labels) != w.TeamID):
		return false
	}
	return true
}

// sameStones reports whether two windows excuse the same firings.
func sameStones(a, b Window) bool {
	return a.Start.Equal(b.Start) && a.End.Equal(b.End) && a.Every == b.Every && a.On == b.On && a.Until.Equal(b.Until) &&
		a.Rules == b.Rules && a.TeamID == b.TeamID && labelsText(a.Labels) == labelsText(b.Labels)
}

func (l *Ledger) windowsMeeting(ctx context.Context, q queryer, lo, hi int64) ([]Window, error) {
	// A window lasts maxWindow at most: one that started before that
	// can't meet [lo, hi], and the index skips it. A repeat starts on its
	// Until's day at the latest, which until_at holds the start of, and a
	// day can last 25 hours, so two days more.
	return l.windows(ctx, q, `WHERE (every = '' AND start_at <= ? AND start_at >= ? AND end_at >= ?)
		OR (every != '' AND start_at <= ? AND (until_at IS NULL OR until_at >= ?)) ORDER BY start_at, id`,
		hi, lo-int64(maxWindow/time.Second), lo, hi, lo-int64((maxWindow+48*time.Hour)/time.Second))
}

func (l *Ledger) windows(ctx context.Context, q queryer, where string, args ...any) ([]Window, error) {
	rows, err := q.QueryContext(ctx, "SELECT id, title, start_at, end_at, every, repeat_on, until_at, team_id, rules, labels, updated, updated_by FROM windows "+where, args...)
	if err != nil {
		return nil, err
	}
	var out []Window
	teams := false
	for rows.Next() {
		var w Window
		var start, end, at int64
		var until sql.NullInt64
		var labels string
		if err := rows.Scan(&w.ID, &w.Title, &start, &end, &w.Every, &w.On, &until, &w.TeamID, &w.Rules, &labels, &at, &w.By); err != nil {
			rows.Close()
			return nil, err
		}
		w.Start, w.End, w.Updated, w.Until, w.loc = unix(start), unix(end), unix(at), fromNull(until), l.loc
		if w.Labels, err = readLabels(labels); err != nil {
			rows.Close()
			return nil, err
		}
		w.re, _ = regexp.Compile(w.Rules)
		if w.Rules == "" {
			w.re = nil
		}
		w.ruled = map[string]bool{}
		w.resolve()
		teams = teams || w.TeamID > 0
		out = append(out, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil || !teams {
		return out, err
	}
	// Who owns what, read with the windows, for a team's.
	own, err := l.ownership(ctx, q)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].own = own
	}
	return out, nil
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
		return 0, invalid("a quiet window repeats every day, week or month, or by a cron expression")
	case w.Every == "day" && w.End.Sub(w.Start) >= 24*time.Hour,
		w.Every == "week" && w.End.Sub(w.Start) >= 7*24*time.Hour:
		return 0, invalid("a repeating quiet window is shorter than how often it repeats")
	case w.Every != "" && !w.Until.IsZero() && w.Until.Before(w.Start):
		return 0, invalid("a quiet window repeats until a day after it starts")
	case len(w.On) > 100:
		return 0, invalid("a quiet window's schedule is 100 characters at most")
	}
	if w.Every == "" {
		w.Until = time.Time{}
	}
	w.sch, w.On = nil, strings.TrimSpace(w.On)
	if !w.scheduled() {
		w.On = ""
	} else {
		sch, err := w.schedule(l.loc)
		if err != nil {
			return 0, err
		}
		// Its first repeat is a time it falls due; each repeat is shorter
		// than the shortest time between two.
		if !sch.Last(w.Start, l.loc).Equal(w.Start) {
			return 0, invalid("a quiet window that repeats starts at a time its schedule falls due: the next is %s", sch.Next(w.Start, l.loc).Format("2006-01-02 15:04"))
		}
		if gap := sch.Gap(w.Start, l.loc); gap > 0 && w.End.Sub(w.Start) >= gap {
			return 0, invalid("a repeating quiet window is shorter than how often it repeats")
		}
		if w.Every == "month" && w.On == "" {
			w.On = strconv.Itoa(w.Start.In(l.loc).Day())
		}
		w.On = strings.ToLower(strings.Join(strings.Fields(w.On), " "))
		w.sch = &sch
	}
	if w.TeamID < 0 {
		return 0, invalid("no such team")
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
		if err := inScope(t, w.TeamID); err != nil {
			return err
		}
		if w.TeamID != 0 {
			var x int
			if err := t.QueryRowContext(t.ctx, "SELECT 1 FROM teams WHERE id = ?", w.TeamID).Scan(&x); err != nil {
				return invalid("no such team")
			}
		}
		spans := [][2]int64{w.span(time.Now())}
		now := time.Now().Unix()
		if id == 0 {
			if err := windowsLeft(t, w.TeamID); err != nil {
				return err
			}
			if err := t.QueryRowContext(t.ctx, `INSERT INTO windows (title, start_at, end_at, every, repeat_on, until_at, team_id, rules, labels, updated, updated_by)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
				w.Title, w.Start.Unix(), w.End.Unix(), w.Every, w.On, nullable(w.Until), w.TeamID, w.Rules, labelsText(w.Labels), now, by).Scan(&id); err != nil {
				return err
			}
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
			if err := inScope(t, was[0].TeamID); err != nil {
				return err
			}
			// Moved to another of their teams, a team's window counts there:
			// an editor of two teams can't keep forty in one.
			if _, limited := scopeOf(t.ctx); limited && w.TeamID != was[0].TeamID {
				var own int
				if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM windows WHERE team_id = ?", w.TeamID).Scan(&own); err != nil {
					return err
				}
				if own >= MaxTeamWindows {
					return invalid("a team's editors keep %d of its quiet windows at most; an editor or an admin can keep more", MaxTeamWindows)
				}
			}
			spans = append(spans, was[0].span(time.Now()))
			if sameStones(was[0], w) { // a new title, say: no stone changes
				spans = nil
			}
			if _, err := t.ExecContext(t.ctx, `UPDATE windows SET title = ?, start_at = ?, end_at = ?, every = ?, repeat_on = ?, until_at = ?, team_id = ?,
				rules = ?, labels = ?, updated = ?, updated_by = ? WHERE id = ?`,
				w.Title, w.Start.Unix(), w.End.Unix(), w.Every, w.On, nullable(w.Until), w.TeamID, w.Rules, labelsText(w.Labels), now, by, id); err != nil {
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
		if err := inScope(t, was[0].TeamID); err != nil {
			return err
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

// JudgePending gives stones again over the spans pended, a week's worth at
// a time: one long span a week at a time, or many short ones, up to
// pendingPerWrite, in one write, the list left written back once.
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
			judged := int64(0)
			for n := 0; len(spans) > 0 && judged < week && n < pendingPerWrite; n++ {
				s := spans[0]
				hi := min(s[1], s[0]+week-1-judged)
				if err := l.judge(t, s[0], hi, o); err != nil {
					return err
				}
				judged += hi - s[0] + 1
				if hi >= s[1] {
					spans = spans[1:]
				} else {
					spans[0][0] = hi + 1
				}
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

// pendingPerWrite is how many spans JudgePending judges in one write at
// most.
const pendingPerWrite = 256

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
		sq += " AND status IN (" + l.d.texts("?") + ")"
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
		if id != 0 {
			if err := inScope(tx, was.TeamID); err != nil {
				return err
			}
		}
		t, err := edit(was)
		if err != nil {
			return err
		}
		if err := errors.Join(checkTask(&t), inScope(tx, t.TeamID)); err != nil {
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
			if err := tx.QueryRowContext(tx.ctx, `INSERT INTO tasks (title, kind, rule, incident, team_id, assignee, status, due, body, link, done_at,
				created, created_by, updated, updated_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
				t.Title, t.Kind, t.Rule, t.Incident, team, t.Assignee, t.Status, nullable(t.Due), t.Body, t.Link, done, now, by, now, by).Scan(&id); err != nil {
				return err
			}
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
		var team int64
		if err := t.QueryRowContext(t.ctx, "SELECT coalesce(team_id, 0) FROM tasks WHERE id = ?", id).Scan(&team); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := inScope(t, team); err != nil {
			return err
		}
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
		// A proposal made into it is made into none: its id isn't
		// another's again (schema 016), and no other task is the proposal's.
		if _, err := t.ExecContext(t.ctx, "UPDATE proposals SET task_id = NULL WHERE task_id = ?", id); err != nil {
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
	// Origin is "" for what people wrote here, OriginTracker for a comment
	// copied in from a task's issue; Ref the tracker's id for it, and
	// Author who wrote it there, as the tracker names them.
	Origin string `json:"origin,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Author string `json:"author,omitempty"`
}

// Comments are what was said about something, the earliest first.
func (l *Ledger) Comments(ctx context.Context, on, key string) ([]Comment, error) {
	return l.comments(ctx, "WHERE on_kind = ? AND on_key = ? ORDER BY created, id", on, key)
}

func (l *Ledger) comments(ctx context.Context, where string, args ...any) ([]Comment, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT id, on_kind, on_key, body, created, created_by, updated, origin, ref, author FROM comments "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		var c Comment
		var created, updated int64
		if err := rows.Scan(&c.ID, &c.On, &c.Key, &c.Body, &created, &c.By, &updated, &c.Origin, &c.Ref, &c.Author); err != nil {
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
			if err := t.QueryRowContext(t.ctx, "INSERT INTO comments (on_kind, on_key, body, created, created_by, updated) VALUES (?, ?, ?, ?, ?, ?) RETURNING id",
				c.On, c.Key, c.Body, now, by, now).Scan(&id); err != nil {
				return err
			}
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
