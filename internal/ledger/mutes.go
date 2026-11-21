package ledger

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// Mutes are the alerts a silence or an inhibition kept from a
// notification, as Alertmanager's event recorder tells of them
// (docs/design/mutes.md): why a firing paged no one. The recorder tells of
// a mute each time Alertmanager asks whether an alert is muted, every few
// seconds, so they are kept a row an alert, what muted it and an hour,
// from the first of them in the hour to the last. The firing each tells of
// is worked out when read, as a notification's is.

// What muted an alert.
const (
	MutedSilence    = "silence"    // a silence, Ref its ID
	MutedInhibition = "inhibition" // an inhibition, Ref the inhibiting alert's fingerprint
	MutedEither     = "either"     // one or the other: a notification's mutedAlerts don't say which
)

// maxMuteRef is how long a mute's Ref is, in bytes: Alertmanager's are a
// UUID and a 64-bit number.
const maxMuteRef = 64

// ErrBadMute is a mute the ledger won't keep.
var ErrBadMute = errors.New("not a mute the ledger keeps")

// Mute is an alert a silence or an inhibition kept from a notification.
type Mute struct {
	// At is when it was told of, and Last, when given, when it was last
	// told of in the same hour; read back, the first and the last of its
	// hours read.
	At, Last time.Time
	// Rule and Labels are the alert's, bounded as a notification's alert's.
	Rule   string
	Labels map[string]string
	// Kind is one of the Muted values; Ref, the silence's ID or the
	// inhibiting alert's fingerprint, "" where the event doesn't say.
	Kind, Ref string
}

// Validate checks a mute as KeepMutes would, before anything is kept.
func (m Mute) Validate() error {
	switch {
	case !sane(m.At):
		return fmt.Errorf("%w: it needs when it came", ErrBadMute)
	case !m.Last.IsZero() && (m.Last.Before(m.At) || m.Last.Unix()/3600 != m.At.Unix()/3600):
		return fmt.Errorf("%w: its last time is in its first's hour, and not before it", ErrBadMute)
	case m.Kind != MutedSilence && m.Kind != MutedInhibition && m.Kind != MutedEither:
		return fmt.Errorf("%w: muted by a silence, an inhibition or either, not %q", ErrBadMute, m.Kind)
	case !plain(m.Ref) || len(m.Ref) > maxMuteRef:
		return fmt.Errorf("%w: what muted it is named in %d bytes at most, with none that don't show", ErrBadMute, maxMuteRef)
	}
	if err := (Alert{Rule: m.Rule, Labels: m.Labels}).check("the muted alert"); err != nil {
		return fmt.Errorf("%w: %w", ErrBadMute, err)
	}
	return nil
}

// Key is what tells a mute apart from others in its hour: its alert, what
// muted it, and by what.
func (m Mute) Key() string {
	labels, _ := json.Marshal(noLabelsIfNil(m.Labels)) // keys sorted
	h := sha256.Sum256([]byte(m.Rule + "\x00" + string(labels) + "\x00" + m.Kind + "\x00" + m.Ref))
	return hex.EncodeToString(h[:16])
}

// KeepMutes keeps mutes, in one write: each is checked first, and one the
// ledger won't keep (errs, an ErrBadMute) doesn't keep out the others.
// Those of one alert, kind and ref in an hour are one row, from the first
// to the last: told of again, after a failure or by another replica, a
// mute changes nothing. added is how many rows are new; err, a failure to
// write, after which nothing was kept. A row added is a page's, counted
// with the write; one only made longer isn't, and shows with the next.
func (l *Ledger) KeepMutes(ctx context.Context, ms []Mute) (added int, errs []error, err error) {
	errs = make([]error, len(ms))
	type row struct {
		key, rule, labels, kind, ref string
		hour, first, last            int64
	}
	byKey := map[string]*row{}
	var order []*row
	for i, m := range ms {
		if errs[i] = m.Validate(); errs[i] != nil {
			continue
		}
		first, last := m.At.Unix(), m.At.Unix()
		if !m.Last.IsZero() {
			last = m.Last.Unix()
		}
		hour := first / 3600
		k := m.Key()
		if r := byKey[k+"/"+strconv.FormatInt(hour, 10)]; r != nil {
			r.first, r.last = min(r.first, first), max(r.last, last)
			continue
		}
		b, _ := json.Marshal(noLabelsIfNil(m.Labels))
		r := &row{key: k, rule: m.Rule, labels: l.d.kept(string(b)), kind: m.Kind, ref: m.Ref, hour: hour, first: first, last: last}
		byKey[k+"/"+strconv.FormatInt(hour, 10)] = r
		order = append(order, r)
	}
	if len(order) == 0 {
		return 0, errs, nil
	}
	err = l.writeAside(ctx, func(t *txn) error {
		added = 0
		ins, err := t.PrepareContext(t.ctx, `INSERT INTO mutes (key, hour, rule, labels, kind, ref, first_at, last_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (key, hour) DO NOTHING RETURNING 1`)
		if err != nil {
			return err
		}
		defer ins.Close()
		upd, err := t.PrepareContext(t.ctx, `UPDATE mutes SET first_at = CASE WHEN first_at > ?3 THEN ?3 ELSE first_at END,
			last_at = CASE WHEN last_at < ?4 THEN ?4 ELSE last_at END
			WHERE key = ?1 AND hour = ?2 AND (first_at > ?3 OR last_at < ?4)`)
		if err != nil {
			return err
		}
		defer upd.Close()
		for _, r := range order {
			var one int
			err := ins.QueryRowContext(t.ctx, r.key, r.hour, r.rule, r.labels, r.kind, r.ref, r.first, r.last).Scan(&one)
			switch {
			case err == nil:
				added++
				continue
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
			if _, err := upd.ExecContext(t.ctx, r.key, r.hour, r.first, r.last); err != nil {
				return err
			}
		}
		t.count = added > 0
		return nil
	})
	if err != nil {
		return 0, errs, err
	}
	return added, errs, nil
}

// MutesSince is how many rows of mutes were told of since a time: what
// the notifications port holds to a day's most.
func (l *Ledger) MutesSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := l.r.QueryRowContext(ctx, "SELECT count(*) FROM mutes WHERE last_at >= ?", since.Unix()).Scan(&n)
	return n, err
}

// Mutes are the rows of mutes last told of in [from, to), as kept: At
// the first of their hour, Last the last; the earliest first.
func (l *Ledger) Mutes(ctx context.Context, from, to time.Time) ([]Mute, error) {
	var out []Mute
	err := l.eachRow(ctx, func(r scanner) error {
		var m Mute
		var text string
		var first, last int64
		if err := r.Scan(&m.Rule, &text, &m.Kind, &m.Ref, &first, &last); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(text), &m.Labels); err != nil {
			return err
		}
		m.At, m.Last = unix(first), unix(last)
		out = append(out, m)
		return nil
	}, "SELECT rule, labels, kind, ref, first_at, last_at FROM mutes WHERE last_at >= ? AND last_at < ? ORDER BY first_at, id", from.Unix(), to.Unix())
	return out, err
}

// HasMutes reports whether the ledger keeps any mute.
func (l *Ledger) HasMutes(ctx context.Context) (bool, error) {
	var one int
	err := l.r.QueryRowContext(ctx, "SELECT 1 FROM mutes LIMIT 1").Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// mutesOfFiring is how many alerts, kinds and refs of a rule are read for
// one firing at most.
const mutesOfFiring = 1000

// qMutesOf reads the mutes of a rule told of over a span: through
// mutes_rule, by the hours, each alert, kind and ref once.
const qMutesOf = `SELECT labels, kind, ref, min(first_at), max(last_at) FROM mutes
	WHERE rule = ? AND hour >= ? AND hour <= ? AND last_at >= ? AND first_at <= ?
	GROUP BY labels, kind, ref ORDER BY min(first_at), kind, ref LIMIT ?`

// MutedOf is, for each firing given, what muted it: the mutes of its rule
// whose labels are all the firing's and more (as a notification's alert's
// are), told of from within before its start to within after its end, or
// now while it is on. They come a silence first, then an inhibition, each
// the earliest first; one of a notification's that doesn't say which is
// left out where one that does is there. Firings nothing muted are left
// out.
func (l *Ledger) MutedOf(ctx context.Context, fs []Firing, within Within, now time.Time) (map[int64][]Mute, error) {
	out := map[int64][]Mute{}
	labels := map[string]map[string]string{} // labels alike are read once
	for _, f := range fs {
		w := within(f.Rule)
		lo := f.Start.Add(-w).Unix()
		end := f.End
		if end.IsZero() {
			end = now
		}
		hi := end.Add(w).Unix()
		if hi < lo {
			continue
		}
		var ms []Mute
		// Alerts with more labels than the firing's, one muted as
		// another, are one mute of it: from the first to the last.
		by := map[[2]string]int{}
		err := l.eachRow(ctx, func(r scanner) error {
			var m Mute
			var text string
			var first, last int64
			if err := r.Scan(&text, &m.Kind, &m.Ref, &first, &last); err != nil {
				return err
			}
			if m.Labels = labels[text]; m.Labels == nil {
				if err := json.Unmarshal([]byte(text), &m.Labels); err != nil {
					return err
				}
				labels[text] = m.Labels
			}
			if !among(f.Labels, m.Labels) {
				return nil
			}
			if i, ok := by[[2]string{m.Kind, m.Ref}]; ok {
				if t := unix(first); t.Before(ms[i].At) {
					ms[i].At = t
				}
				if t := unix(last); t.After(ms[i].Last) {
					ms[i].Last = t
				}
				return nil
			}
			m.Rule, m.At, m.Last = f.Rule, unix(first), unix(last)
			by[[2]string{m.Kind, m.Ref}] = len(ms)
			ms = append(ms, m)
			return nil
		}, qMutesOf, f.Rule, floorHour(lo), floorHour(hi), lo, hi, mutesOfFiring)
		if err != nil {
			return nil, err
		}
		if len(ms) == 0 {
			continue
		}
		said := slices.ContainsFunc(ms, func(m Mute) bool { return m.Kind != MutedEither })
		if said {
			ms = slices.DeleteFunc(ms, func(m Mute) bool { return m.Kind == MutedEither })
		}
		rank := map[string]int{MutedSilence: 0, MutedInhibition: 1, MutedEither: 2}
		slices.SortStableFunc(ms, func(a, b Mute) int { return cmp.Compare(rank[a.Kind], rank[b.Kind]) })
		out[f.ID] = ms
	}
	return out, nil
}

// floorHour is the hour a Unix time is in, as mutes keep it.
func floorHour(t int64) int64 {
	if t < 0 {
		return (t - 3599) / 3600
	}
	return t / 3600
}
