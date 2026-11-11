package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"lapilli.dev/internal/schedule"
)

// Reports people schedule themselves (docs/design/reports.md): when, over
// what span, for whom, where to, and what each holds.

// MaxScheduled is how many reports the ledger keeps.
const MaxScheduled = 50

// ReportSections are what a report can hold, in the order it holds them;
// DefaultSections, what one holds when none are picked.
var (
	ReportSections  = []string{"summary", "noise", "missed", "woken", "notifications", "replay", "sources", "tasks", "content", "suggestions"}
	DefaultSections = []string{"summary", "noise", "missed", "woken"}
)

// Scheduled is a report people schedule.
type Scheduled struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// TeamID is the team it is for; 0 for everyone.
	TeamID int64 `json:"team_id,omitempty"`
	// Schedule is a preset or a cron expression, as given; Span, a cron
	// expression's span, such as 24h, "" for the time since the one before.
	Schedule string `json:"schedule"`
	Span     string `json:"span,omitempty"`
	// Zone is ledger or team: whose zone its times are counted in.
	Zone     string   `json:"zone"`
	Sections []string `json:"sections"`
	// Dests are the names of the destinations it goes to.
	Dests     []string  `json:"dests"`
	Enabled   bool      `json:"enabled"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by,omitempty"`
	Updated   time.Time `json:"updated"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// When is its schedule, read.
func (r Scheduled) When() (schedule.Schedule, error) {
	span, err := r.span()
	if err != nil {
		return schedule.Schedule{}, err
	}
	return schedule.Parse(r.Schedule, span)
}

// span is its span: a duration such as 24h or 90m, or days, such as 7d.
func (r Scheduled) span() (time.Duration, error) {
	if r.Span == "" {
		return 0, nil
	}
	bad := invalid("a span is a duration such as 24h or 7d")
	if len(r.Span) > 20 {
		return 0, bad
	}
	if days, ok := strings.CutSuffix(r.Span, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 1 || n > 92 {
			return 0, bad
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(r.Span)
	if err != nil {
		return 0, bad
	}
	return d, nil
}

// spanText is a span as a person writes it: 7d or 24h, not 168h0m0s.
func spanText(d time.Duration) string {
	switch {
	case d == 0:
		return ""
	case d%(24*time.Hour) == 0 && d > 24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	t := strings.TrimSuffix(d.String(), "0s")
	if strings.HasSuffix(t, "h0m") {
		t = strings.TrimSuffix(t, "0m")
	}
	return t
}

var (
	reportName = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,99}$`)
	destName   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$|^team:[\p{L}\p{N}][\p{L}\p{N} ._-]{0,63}$`)
)

// ValidDest reports whether a name can name a destination: lowercase
// letters, digits and . _ -, or team:NAME.
func ValidDest(name string) bool { return destName.MatchString(name) }

// check checks a report before it is kept.
func (r *Scheduled) check() error {
	r.Name = strings.TrimSpace(r.Name)
	if !reportName.MatchString(r.Name) {
		return invalid("a report's name is letters, digits, spaces and . _ -, up to 100")
	}
	if _, err := r.When(); err != nil {
		var se schedule.Error
		if errors.As(err, &se) {
			return invalid(se.Format, se.Args...)
		}
		return err
	}
	span, _ := r.span()
	r.Span = spanText(span)
	switch r.Zone {
	case "":
		r.Zone = "ledger"
	case "ledger":
	case "team":
		if r.TeamID == 0 {
			return invalid("a report counted in its team's zone is a team's")
		}
	default:
		return invalid("a report's zone is ledger or team")
	}
	if len(r.Sections) == 0 {
		r.Sections = slices.Clone(DefaultSections)
	}
	seen := map[string]bool{}
	for _, x := range r.Sections {
		if !slices.Contains(ReportSections, x) || seen[x] {
			return invalid("a report's sections are some of %s, each once", strings.Join(ReportSections, ", "))
		}
		seen[x] = true
	}
	// In the order a report holds them, whatever order they were given in.
	slices.SortFunc(r.Sections, func(a, b string) int {
		return slices.Index(ReportSections, a) - slices.Index(ReportSections, b)
	})
	if len(r.Dests) == 0 || len(r.Dests) > 10 {
		return invalid("a report goes to 1 to 10 destinations")
	}
	seen = map[string]bool{}
	for _, d := range r.Dests {
		if !ValidDest(d) || seen[d] {
			return invalid("a destination is named as --report-dest names it, or webhook, slack or team:NAME, each once")
		}
		seen[d] = true
	}
	return nil
}

// PutScheduled keeps a report: a new one when its ID is 0. at is when, by
// the server's clock: a report isn't sent for a time it fell due before it
// was last changed.
func (l *Ledger) PutScheduled(ctx context.Context, r Scheduled, by string, at time.Time) (int64, error) {
	if err := r.check(); err != nil {
		return 0, err
	}
	span, _ := r.span()
	sections, _ := json.Marshal(r.Sections)
	dests, _ := json.Marshal(r.Dests)
	id := r.ID
	err := l.write(ctx, func(t *txn) error {
		if r.TeamID != 0 {
			var ok bool
			if err := t.QueryRowContext(t.ctx, "SELECT EXISTS (SELECT 1 FROM teams WHERE id = ?)", r.TeamID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return invalid("there is no team %d", r.TeamID)
			}
		}
		now := at.Unix()
		var res sql.Result
		var err error
		if id == 0 {
			var n int
			if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM reports").Scan(&n); err != nil {
				return err
			}
			if n >= MaxScheduled {
				return invalid("the ledger keeps %d reports at most", MaxScheduled)
			}
			res, err = t.ExecContext(t.ctx, `INSERT INTO reports (name, team_id, schedule, span, zone, sections, dests, enabled, created, created_by, updated, updated_by)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				r.Name, r.TeamID, r.Schedule, int64(span/time.Second), r.Zone, string(sections), string(dests), r.Enabled, now, by, now, by)
		} else {
			res, err = t.ExecContext(t.ctx, `UPDATE reports SET name = ?, team_id = ?, schedule = ?, span = ?, zone = ?, sections = ?, dests = ?, enabled = ?,
				updated = ?, updated_by = ? WHERE id = ?`,
				r.Name, r.TeamID, r.Schedule, int64(span/time.Second), r.Zone, string(sections), string(dests), r.Enabled, now, by, id)
		}
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return invalid("there is a report called %s already", r.Name)
			}
			return err
		}
		if id == 0 {
			id, _ = res.LastInsertId()
		} else if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return change(t, by, "report "+r.Name+" kept", nil, r)
	})
	return id, err
}

// DeleteScheduled deletes a report.
func (l *Ledger) DeleteScheduled(ctx context.Context, id int64, by string) error {
	return l.write(ctx, func(t *txn) error {
		var name string
		if err := t.QueryRowContext(t.ctx, "SELECT name FROM reports WHERE id = ?", id).Scan(&name); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM reports WHERE id = ?", id); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM meta WHERE key = ?", ReportSentKey(id)); err != nil {
			return err
		}
		return change(t, by, "report "+name+" deleted", nil, nil)
	})
}

// ReportSentKey is the key TellOnce keeps a report's last span by.
func ReportSentKey(id int64) string { return "report." + strconv.FormatInt(id, 10) }

// Told is the value TellOnce last told of key for; "" for none.
func (l *Ledger) Told(ctx context.Context, key string) (string, error) {
	return l.meta(ctx, l.r, key)
}

const reportCols = "id, name, team_id, schedule, span, zone, sections, dests, enabled, created, created_by, updated, updated_by"

func scanScheduled(rows interface{ Scan(...any) error }) (Scheduled, error) {
	var r Scheduled
	var span, created, updated int64
	var sections, dests string
	if err := rows.Scan(&r.ID, &r.Name, &r.TeamID, &r.Schedule, &span, &r.Zone, &sections, &dests, &r.Enabled,
		&created, &r.CreatedBy, &updated, &r.UpdatedBy); err != nil {
		return r, err
	}
	r.Span, r.Created, r.Updated = spanText(time.Duration(span)*time.Second), unix(created), unix(updated)
	if err := json.Unmarshal([]byte(sections), &r.Sections); err != nil {
		return r, err
	}
	return r, json.Unmarshal([]byte(dests), &r.Dests)
}

// Scheduleds are the reports kept, by name.
func (l *Ledger) Scheduleds(ctx context.Context) ([]Scheduled, error) {
	rows, err := l.r.QueryContext(ctx, "SELECT "+reportCols+" FROM reports ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Scheduled
	for rows.Next() {
		r, err := scanScheduled(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ScheduledByID is one report.
func (l *Ledger) ScheduledByID(ctx context.Context, id int64) (Scheduled, error) {
	r, err := scanScheduled(l.r.QueryRowContext(ctx, "SELECT "+reportCols+" FROM reports WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}
