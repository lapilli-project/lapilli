package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
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
	// incidents file or copied in from a tool, which own it, or an impact
	// window; or, for what copies them in, one a person wrote.
	ErrNotYours = errors.New("this incident comes from elsewhere and is changed there")
	ErrTaken    = errors.New("that id is taken")
	// ErrTooMany is a copy past maxCopied.
	ErrTooMany = errors.New("the ledger keeps 20,000 incidents copied in at most")
)

// Copier is who a job copying incidents from elsewhere writes as. It isn't a
// person: the tool it copies from has told people already, and a first copy of
// months of history shouldn't be told one line at a time.
const Copier = "incidents-token"

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
	var taken []string
	err := l.write(ctx, func(t *txn) error {
		taken = nil
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
			other, err := l.get(t, in.ID)
			switch {
			case err == nil && other.origin == "copy":
				// The file is whoever runs serve's, the copy a job's: a job
				// that got to an id first can't keep the file's out.
				if _, err := t.ExecContext(t.ctx, "UPDATE incidents SET origin = 'file' WHERE id = ?", other.row); err != nil {
					return err
				}
				spans = append(spans, pair(span(other.Incident, o)))
			case err == nil && other.origin != "file":
				// A person's, or an impact window: passed over, and the rest
				// of the file read.
				taken = append(taken, in.ID)
				continue
			case err != nil && !errors.Is(err, ErrNotFound):
				return err
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
	if err == nil && len(taken) > 0 {
		return changed, &TakenError{IDs: taken}
	}
	return changed, err
}

// TakenError is ids the incidents file has that the ledger holds from
// elsewhere: written by a person, or impact windows. The rest of the file was
// read.
type TakenError struct{ IDs []string }

func (e *TakenError) Error() string {
	return fmt.Sprintf("incidents file: %s: %v in the ledger by an incident written elsewhere; the rest of the file was read", strings.Join(e.IDs, ", "), ErrTaken)
}

func (e *TakenError) Unwrap() error { return ErrTaken }

func pair(a, b int64) [2]int64 { return [2]int64{a, b} }

// same reports whether two incidents are alike as the ledger keeps them:
// whatever zones their times are written in, to the second.
func same(a, b incident.Incident) bool {
	x, _ := json.Marshal(orEmpty(a.Labels))
	y, _ := json.Marshal(orEmpty(b.Labels))
	return a.ID == b.ID && a.Title == b.Title && a.Start.Unix() == b.Start.Unix() && a.End.IsZero() == b.End.IsZero() &&
		(a.End.IsZero() || a.End.Unix() == b.End.Unix()) && string(x) == string(y)
}

// marshal is JSON as written: Go's escaping of < > & would make a label
// full of them six times its size.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// put writes an incident, in place of the one with its id.
func put(t *txn, in incident.Incident, origin, by string, now int64) error {
	labels, err := marshal(orEmpty(in.Labels))
	if err != nil {
		return err
	}
	_, err = t.ExecContext(t.ctx, `INSERT INTO incidents (key, origin, counts, title, start_at, end_at, labels, updated, updated_by)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT `+t.d.pick("(key)", "(md5(key))")+` DO UPDATE SET title = excluded.title, start_at = excluded.start_at, end_at = excluded.end_at,
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

// Put writes an incident from the UI or the API, or changes one written
// there. With no id, it gets the next L-n. Written again as it is, it changes
// nothing: a job copying incidents from elsewhere every hour writes nothing, tells
// no one, and judges nothing again. It returns the id.
//
// What Copier writes is copied in from a tool, and is that tool's, as the
// incidents file's are the file's: Copier changes only those, and people only
// theirs.
func (l *Ledger) Put(ctx context.Context, in incident.Incident, by string, o verdict.Options) (string, error) {
	origin := originOf(by)
	stored, _ := marshal(orEmpty(in.Labels))
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
	case len(stored) > maxLabelBytes:
		return "", errors.New("an incident's labels are 4 KB at most in all, as JSON")
	case !sane(in.Start) || (!in.End.IsZero() && !sane(in.End)):
		return "", errors.New("an incident's times are from 1970 to 9999")
	}
	for k, vs := range in.Labels {
		if len(k) > 128 || len(vs) > 64 || slices.ContainsFunc(vs, func(v string) bool { return len(v) > 256 }) {
			return "", errors.New("an incident's label is a name of up to 128 bytes, with up to 64 values of up to 256")
		}
	}
	in.Candidate = false
	if in.ID != "" {
		if old, err := l.IncidentByID(ctx, in.ID); err == nil && old.Origin == origin && same(old.Incident, in) {
			return in.ID, nil
		}
	}
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
			if origin == "copy" {
				var n int
				if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM incidents WHERE origin = 'copy'").Scan(&n); err != nil {
					return err
				}
				if n >= maxCopied {
					return ErrTooMany
				}
			}
		case err != nil:
			return err
		case old.origin != origin:
			return ErrNotYours
		}
		if err := put(t, in, origin, by, time.Now().Unix()); err != nil {
			return err
		}
		what := "incident " + in.ID + " written"
		var spans [][2]int64
		// A new title judges nothing again: stones hang on times and labels.
		if err != nil || !sameWhereWhen(old.Incident, in) {
			spans = append(spans, pair(span(in, o)))
		}
		if err != nil && by != Copier { // a new one
			told := in
			told.Start, told.End = in.Start.UTC(), in.End.UTC()
			if err := l.tell(t, "incident.written", by, map[string]any{"incident": told}); err != nil {
				return err
			}
		}
		if err == nil {
			what = "incident " + in.ID + " changed"
			if len(spans) > 0 {
				spans = append(spans, pair(span(old.Incident, o)))
			}
		}
		// The tool keeps what its incidents were; the ledger notes only
		// that they changed.
		before, after := oldOrNil(err, old), any(in)
		if by == Copier {
			before, after = nil, nil
		}
		if err := change(t, by, what, before, after); err != nil {
			return err
		}
		return l.judgeSpans(t, spans, o)
	})
	return in.ID, err
}

// The bounds on what is written: see Put.
const maxLabelBytes = 4 << 10

// maxCopied is how many incidents copied in from tools the ledger keeps:
// years of a large organisation's, and a bound on what a token can add.
var maxCopied = 20000

// sane is a time a person could mean: from 1970 to 9999, in UTC, which every
// page, the API and SQLite read alike.
func sane(t time.Time) bool { y := t.UTC().Year(); return y >= 1970 && y <= 9999 }

// originOf is where what someone writes comes from: copied in, or written.
func originOf(by string) string {
	if by == Copier {
		return "copy"
	}
	return "ui"
}

// sameWhereWhen reports whether two incidents have the same times and labels,
// all their stones hang on.
func sameWhereWhen(a, b incident.Incident) bool {
	a.Title, b.Title = "", ""
	return same(a, b)
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

// Delete takes away an incident written in the UI, or by Copier one it
// copied in.
func (l *Ledger) Delete(ctx context.Context, key, by string, o verdict.Options) error {
	return l.write(ctx, func(t *txn) error {
		old, err := l.get(t, key)
		if err != nil {
			return err
		}
		if old.origin != originOf(by) {
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
		b, _ := marshal(v)
		return string(b)
	}
	_, err := t.ExecContext(t.ctx, "INSERT INTO changes (at, by, what, before, after) VALUES (?, ?, ?, ?, ?)",
		time.Now().Unix(), by, what, text(before), text(after))
	return err
}
