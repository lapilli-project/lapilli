package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The activity log (docs/design/serveops.md, "The activity log"): every
// change kept in changes, read back by who made it, its kind and its
// period, the latest first, a page at a time, months after it was made.
// Nothing new is kept for it: the kinds are told apart by how each change
// is noted, as every write notes it.

// LogEntry is a change as the activity log reads it back, with its id, which
// a page goes on from (ActivityCursor), and its kind (KindOfChange).
type LogEntry struct {
	ID            int64
	At            time.Time
	By, Kind      string
	What          string
	Before, After string
}

// ActivityKinds are the kinds of change the activity log tells apart, in
// the order it offers them, each by how its changes are noted: what they
// start with. A change that starts with none of them is of KindOther.
var ActivityKinds = []struct {
	Kind     string
	Prefixes []string
}{
	{"setting", []string{"setting "}},
	{"incident", []string{"incident ", "impact window "}},
	{"verdict", []string{"verdict on ", "verdicts: "}},
	{"proposal", []string{"proposal #"}},
	{"task", []string{"task "}},
	{"comment", []string{"comment "}},
	{"note", []string{"note on "}},
	{"window", []string{"quiet window "}},
	{"marked", []string{"marked window "}},
	{"report", []string{"report ", "the weekly digest"}},
	{"team", []string{"team "}},
	{"role", []string{RoleGiven, RoleTaken}},
	{"goal", []string{"goal "}},
	{"firings", []string{"firings"}},
	{"cleanup", []string{"what rule "}},
}

// KindOther is the kind of a change that is none of ActivityKinds'.
const KindOther = "other"

// KindOfChange is the kind of a change, by what it is noted as.
func KindOfChange(what string) string {
	for _, k := range ActivityKinds {
		for _, p := range k.Prefixes {
			// firings is a whole what, not a prefix.
			if p == "firings" && what == p || p != "firings" && strings.HasPrefix(what, p) {
				return k.Kind
			}
		}
	}
	return KindOther
}

// KnownKind reports whether kind is one the activity log tells apart.
func KnownKind(kind string) bool {
	if kind == KindOther {
		return true
	}
	for _, k := range ActivityKinds {
		if k.Kind == kind {
			return true
		}
	}
	return false
}

// likeOf is the LIKE pattern a prefix is matched by: none of the prefixes
// holds % or _, which a pattern would take as wildcards.
func likeOf(p string) string {
	if p == "firings" {
		return p
	}
	return p + "%"
}

// ActivityCursor is where a page of the activity log ends: the time and id
// of its last change. The next page starts after it.
type ActivityCursor struct{ At, ID int64 }

// String is the cursor as a page's link carries it: AT.ID.
func (c ActivityCursor) String() string {
	if c.ID == 0 {
		return ""
	}
	return fmt.Sprintf("%d.%d", c.At, c.ID)
}

// ParseActivityCursor reads a cursor as String writes it.
func ParseActivityCursor(v string) (ActivityCursor, error) {
	if v == "" {
		return ActivityCursor{}, nil
	}
	at, id, ok := strings.Cut(v, ".")
	a, err1 := strconv.ParseInt(at, 10, 64)
	i, err2 := strconv.ParseInt(id, 10, 64)
	if !ok || err1 != nil || err2 != nil || i <= 0 {
		return ActivityCursor{}, invalid("before %q: want where a page ended, as its link gives it", v)
	}
	return ActivityCursor{a, i}, nil
}

// ActivityQuery picks changes: by whom (By, the name as noted, whole), of
// which kind ("" for any), made from Since and before Until (each zero for
// no bound), after a page that ended at Before; Limit of them at most.
type ActivityQuery struct {
	By           string
	Kind         string
	Since, Until time.Time
	Before       ActivityCursor
	Limit        int
}

// Activity are the changes q picks, the latest first. The kinds are told
// apart by LIKE on fixed prefixes, the bounds by the index on at, so that a
// read of one page reads no more than it must, on SQLite and Postgres
// alike.
func (l *Ledger) Activity(ctx context.Context, q ActivityQuery) ([]LogEntry, error) {
	if q.Kind != "" && !KnownKind(q.Kind) {
		return nil, invalid("kind %q: want one of the activity log's kinds", q.Kind)
	}
	var where []string
	var args []any
	if q.By != "" {
		where, args = append(where, "by = ?"), append(args, q.By)
	}
	if q.Kind != "" {
		var likes []string
		for _, k := range ActivityKinds {
			if k.Kind != q.Kind && q.Kind != KindOther {
				continue
			}
			for _, p := range k.Prefixes {
				likes, args = append(likes, "what LIKE ?"), append(args, likeOf(p))
			}
		}
		cond := "(" + strings.Join(likes, " OR ") + ")"
		if q.Kind == KindOther {
			cond = "NOT " + cond
		}
		where = append(where, cond)
	}
	if !q.Since.IsZero() {
		where, args = append(where, "at >= ?"), append(args, q.Since.Unix())
	}
	if !q.Until.IsZero() {
		where, args = append(where, "at < ?"), append(args, q.Until.Unix())
	}
	if q.Before.ID != 0 {
		where, args = append(where, "(at < ? OR (at = ? AND id < ?))"), append(args, q.Before.At, q.Before.At, q.Before.ID)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	query := "SELECT id, at, by, what, before, after FROM changes"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY at DESC, id DESC LIMIT ?"
	rows, err := l.r.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		var c LogEntry
		var at int64
		if err := rows.Scan(&c.ID, &at, &c.By, &c.What, &c.Before, &c.After); err != nil {
			return nil, err
		}
		c.At, c.Kind = unix(at), KindOfChange(c.What)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Cursor is where a page that ends with c goes on from.
func (c LogEntry) Cursor() ActivityCursor { return ActivityCursor{c.At.Unix(), c.ID} }

// ChangeSubject is what a change is of, as its what and after say it: the
// rules (a verdict's, a note's, a comment's on a rule, what a rule kept,
// the rules of a bulk verdict's firings), a task, or a proposal (0 for
// none). The activity log's team is the team of these.
func ChangeSubject(what, after string) (rules []string, task, proposal int64) {
	num := func(s string) int64 {
		end := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' })
		if end < 0 {
			end = len(s)
		}
		n, _ := strconv.ParseInt(s[:end], 10, 64)
		return n
	}
	switch {
	case strings.HasPrefix(what, "verdict on "):
		if rule, _, _, ok := verdictOn(what); ok {
			return []string{rule}, 0, 0
		}
	case strings.HasPrefix(what, "verdicts: "):
		var a struct {
			Firings []struct {
				Rule string `json:"rule"`
			} `json:"firings"`
		}
		if json.Unmarshal([]byte(after), &a) == nil {
			seen := map[string]bool{}
			for _, f := range a.Firings {
				if f.Rule != "" && !seen[f.Rule] {
					seen[f.Rule] = true
					rules = append(rules, f.Rule)
				}
			}
		}
		return rules, 0, 0
	case strings.HasPrefix(what, "note on "):
		rest := strings.TrimPrefix(what, "note on ")
		for _, end := range []string{" taken away", " kept"} {
			if r, ok := strings.CutSuffix(rest, end); ok {
				return []string{r}, 0, 0
			}
		}
	case strings.HasPrefix(what, "comment on rule "):
		return []string{strings.TrimPrefix(what, "comment on rule ")}, 0, 0
	case strings.HasPrefix(what, "comment on task "):
		return nil, num(strings.TrimPrefix(what, "comment on task ")), 0
	case strings.HasPrefix(what, "comment ") && strings.Contains(what, " posted to task "):
		_, rest, _ := strings.Cut(what, " posted to task ")
		return nil, num(rest), 0
	case strings.HasPrefix(what, "what rule "):
		rest := strings.TrimPrefix(what, "what rule ")
		if i := strings.Index(rest, " kept "); i >= 0 {
			return []string{rest[:i]}, 0, 0
		}
	case strings.HasPrefix(what, "task "):
		return nil, num(strings.TrimPrefix(what, "task ")), 0
	case strings.HasPrefix(what, "proposal #"):
		return nil, 0, num(strings.TrimPrefix(what, "proposal #"))
	}
	return nil, 0, 0
}
