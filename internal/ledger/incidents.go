package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	"lapilli.dev/internal/incident"
	"lapilli.dev/internal/verdict"
)

// Incident is an incident as the ledger keeps it.
type Incident struct {
	incident.Incident
	// Origin is where it came from: file (--incidents), ui, or impact.
	Origin string
	// State is an impact window's: "", confirmed or dismissed.
	State string
	// Counts says whether firings are judged against it.
	Counts  bool
	Updated time.Time
	By      string
	// CaughtBy are the rules that caught it, the first first.
	CaughtBy []verdict.Catch
}

// Errors from writing incidents.
var (
	ErrNotFound = errors.New("no such incident")
	// ErrNotYours is an incident the UI can't change: one from the
	// incidents file, which owns it, or an impact window.
	ErrNotYours = errors.New("this incident comes from elsewhere and is changed there")
	ErrTaken    = errors.New("that id is taken")
)

// maxIncident is the longest an incident written down can last: its
// stones are given again in one write.
const maxIncident = 31 * 24 * time.Hour

var keyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,63}$`)

// span is the firings an incident's stones can reach.
func span(in incident.Incident, o verdict.Options) (int64, int64) { return window(in, o) }

// SyncFile makes the incidents from the incidents file those given:
// changed, added and taken away. It reports whether any changed.
func (l *Ledger) SyncFile(ctx context.Context, incs []incident.Incident, o verdict.Options) (bool, error) {
	changed := false
	err := l.write(ctx, func(t *txn) error {
		have := map[string]incidentRow{}
		rows, err := t.QueryContext(t.ctx, "SELECT id, key, origin, title, start_at, end_at, labels FROM incidents WHERE origin = 'file'")
		if err != nil {
			return err
		}
		for rows.Next() {
			r, err := scanIncident(rows)
			if err != nil {
				rows.Close()
				return err
			}
			have[r.ID] = r
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var spans [][2]int64
		now := time.Now().Unix()
		for _, in := range incs {
			old, ok := have[in.ID]
			delete(have, in.ID)
			if ok && same(old.Incident, in) {
				continue
			}
			var origin string
			err := t.QueryRowContext(t.ctx, "SELECT origin FROM incidents WHERE key = ?", in.ID).Scan(&origin)
			if err == nil && origin != "file" {
				return fmt.Errorf("incidents file: %s: %w in the ledger", in.ID, ErrTaken)
			}
			if err := put(t, in, "file", "", now); err != nil {
				return err
			}
			if ok {
				spans = append(spans, pair(span(old.Incident, o)))
			}
			spans = append(spans, pair(span(in, o)))
		}
		for key, old := range have {
			if _, err := t.ExecContext(t.ctx, "DELETE FROM catches WHERE incident_id = ?", old.row); err != nil {
				return err
			}
			if _, err := t.ExecContext(t.ctx, "DELETE FROM incidents WHERE key = ?", key); err != nil {
				return err
			}
			spans = append(spans, pair(span(old.Incident, o)))
		}
		changed = len(spans) > 0
		return l.judgeSpans(t, spans, o)
	})
	return changed, err
}

func pair(a, b int64) [2]int64 { return [2]int64{a, b} }

// same reports whether two incidents are alike, whatever zones their times
// are written in.
func same(a, b incident.Incident) bool {
	x, _ := json.Marshal(orEmpty(a.Labels))
	y, _ := json.Marshal(orEmpty(b.Labels))
	return a.ID == b.ID && a.Title == b.Title && a.Start.Equal(b.Start) && a.End.Equal(b.End) && string(x) == string(y)
}

// put writes an incident, in place of the one with its id.
func put(t *txn, in incident.Incident, origin, by string, now int64) error {
	labels, err := json.Marshal(orEmpty(in.Labels))
	if err != nil {
		return err
	}
	_, err = t.ExecContext(t.ctx, `INSERT INTO incidents (key, origin, counts, title, start_at, end_at, labels, updated, updated_by)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET title = excluded.title, start_at = excluded.start_at, end_at = excluded.end_at,
			labels = excluded.labels, updated = excluded.updated, updated_by = excluded.updated_by`,
		in.ID, origin, in.Title, in.Start.Unix(), nullable(in.End), string(labels), now, by)
	return err
}

// judgeSpans gives stones again over each span, overlapping spans once.
func (l *Ledger) judgeSpans(t *txn, spans [][2]int64, o verdict.Options) error {
	for _, s := range merge(spans) {
		if err := l.judge(t, s[0], s[1], o); err != nil {
			return err
		}
	}
	return nil
}

func merge(spans [][2]int64) [][2]int64 {
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j][0] < spans[j-1][0]; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
	var out [][2]int64
	for _, s := range spans {
		if n := len(out); n > 0 && s[0] <= out[n-1][1] {
			out[n-1][1] = max(out[n-1][1], s[1])
			continue
		}
		out = append(out, s)
	}
	return out
}

// Put writes an incident from the UI, or changes one written there. With
// no id, it gets the next L-n. It returns the id.
func (l *Ledger) Put(ctx context.Context, in incident.Incident, by string, o verdict.Options) (string, error) {
	switch {
	case in.Start.IsZero():
		return "", errors.New("an incident needs a start")
	case !in.End.IsZero() && in.End.Before(in.Start):
		return "", errors.New("an incident can't end before it starts")
	case in.ID != "" && !keyRE.MatchString(in.ID):
		return "", errors.New("an id is letters, digits and . _ : / # -, up to 64")
	case !in.End.IsZero() && in.End.Sub(in.Start) > maxIncident:
		return "", errors.New("an incident lasts 31 days at most")
	case len(in.Title) > 500:
		return "", errors.New("an incident's title is 500 bytes at most")
	case len(in.Labels) > 32:
		return "", errors.New("an incident has 32 labels at most")
	}
	for k, vs := range in.Labels {
		if len(k) > 128 || len(vs) > 64 || slices.ContainsFunc(vs, func(v string) bool { return len(v) > 256 }) {
			return "", errors.New("an incident's label is a name of up to 128 bytes, with up to 64 values of up to 256")
		}
	}
	in.Candidate = false
	err := l.write(ctx, func(t *txn) error {
		if in.ID == "" {
			n, err := l.meta(t.ctx, t, "next_incident")
			if err != nil {
				return err
			}
			next, _ := strconv.Atoi(n)
			for {
				next++
				in.ID = "L-" + strconv.Itoa(next)
				var x int
				if err := t.QueryRowContext(t.ctx, "SELECT 1 FROM incidents WHERE key = ?", in.ID).Scan(&x); errors.Is(err, sql.ErrNoRows) {
					break
				} else if err != nil {
					return err
				}
			}
			if err := setMeta(t.ctx, t, "next_incident", strconv.Itoa(next)); err != nil {
				return err
			}
		}
		old, err := l.get(t, in.ID)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return err
		case old.origin != "ui":
			return ErrNotYours
		}
		if err := put(t, in, "ui", by, time.Now().Unix()); err != nil {
			return err
		}
		what := "incident " + in.ID + " written"
		spans := [][2]int64{pair(span(in, o))}
		if err == nil {
			what = "incident " + in.ID + " changed"
			spans = append(spans, pair(span(old.Incident, o)))
		}
		if err := change(t, by, what, oldOrNil(err, old), in); err != nil {
			return err
		}
		return l.judgeSpans(t, spans, o)
	})
	return in.ID, err
}

func oldOrNil(err error, old incidentRow) any {
	if err != nil {
		return nil
	}
	return old.Incident
}

func (l *Ledger) get(t *txn, key string) (incidentRow, error) {
	r, err := scanIncident(t.QueryRowContext(t.ctx, "SELECT id, key, origin, title, start_at, end_at, labels FROM incidents WHERE key = ?", key))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// Delete takes away an incident written in the UI.
func (l *Ledger) Delete(ctx context.Context, key, by string, o verdict.Options) error {
	return l.write(ctx, func(t *txn) error {
		old, err := l.get(t, key)
		if err != nil {
			return err
		}
		if old.origin != "ui" {
			return ErrNotYours
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM catches WHERE incident_id = ?", old.row); err != nil {
			return err
		}
		if _, err := t.ExecContext(t.ctx, "DELETE FROM incidents WHERE id = ?", old.row); err != nil {
			return err
		}
		if err := change(t, by, "incident "+key+" deleted", old.Incident, nil); err != nil {
			return err
		}
		a, b := span(old.Incident, o)
		return l.judge(t, a, b, o)
	})
}

// Decide confirms an impact window, writing it down as an incident with
// that title, or dismisses it, or takes back either. It returns the id of
// the incident a confirmation writes.
func (l *Ledger) Decide(ctx context.Context, key, state, title, by string, o verdict.Options) (string, error) {
	if state != "" && state != "confirmed" && state != "dismissed" {
		return "", fmt.Errorf("state %q: want confirmed, dismissed or none", state)
	}
	if len(title) > 500 {
		return "", errors.New("an incident's title is 500 bytes at most")
	}
	var made string
	err := l.write(ctx, func(t *txn) error {
		old, err := l.get(t, key)
		if err != nil {
			return err
		}
		if old.origin != "impact" {
			return fmt.Errorf("%s isn't an impact window", key)
		}
		var was string
		if err := t.QueryRowContext(t.ctx, "SELECT state FROM incidents WHERE id = ?", old.row).Scan(&was); err != nil {
			return err
		}
		if was == state {
			return nil // decided already, by a second press or someone else
		}
		if _, err := t.ExecContext(t.ctx, "UPDATE incidents SET state = ?, updated = ?, updated_by = ? WHERE id = ?",
			state, time.Now().Unix(), by, old.row); err != nil {
			return err
		}
		spans := [][2]int64{pair(span(old.Incident, o))}
		if state == "confirmed" {
			n, err := l.meta(t.ctx, t, "next_incident")
			if err != nil {
				return err
			}
			next, _ := strconv.Atoi(n)
			in := old.Incident
			in.Candidate = false
			if title != "" {
				in.Title = title
			}
			for {
				next++
				in.ID = "L-" + strconv.Itoa(next)
				var x int
				if err := t.QueryRowContext(t.ctx, "SELECT 1 FROM incidents WHERE key = ?", in.ID).Scan(&x); errors.Is(err, sql.ErrNoRows) {
					break
				} else if err != nil {
					return err
				}
			}
			if err := setMeta(t.ctx, t, "next_incident", strconv.Itoa(next)); err != nil {
				return err
			}
			if err := put(t, in, "ui", by, time.Now().Unix()); err != nil {
				return err
			}
			made = in.ID
		}
		what := "impact window " + key + " " + map[string]string{"": "reopened", "confirmed": "confirmed as " + made, "dismissed": "dismissed"}[state]
		if err := change(t, by, what, nil, nil); err != nil {
			return err
		}
		return l.judgeSpans(t, spans, o)
	})
	return made, err
}

// change notes a change made through the UI or API.
func change(t *txn, by, what string, before, after any) error {
	text := func(v any) string {
		if v == nil {
			return ""
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	_, err := t.ExecContext(t.ctx, "INSERT INTO changes (at, by, what, before, after) VALUES (?, ?, ?, ?, ?)",
		time.Now().Unix(), by, what, text(before), text(after))
	return err
}
