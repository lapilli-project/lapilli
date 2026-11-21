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
	"sync/atomic"
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

// ErrMutesFull is a mute of a rule's hour that holds as many rows as an
// hour of a rule holds (maxMutesPerRuleHour): passed over, not kept.
var ErrMutesFull = errors.New("as many mutes of the rule in the hour as are kept")

// maxMutesPerRuleHour is how many rows of mutes an hour of a rule holds:
// a thousand alerts each muted by one silence, or five hundred told of by
// a notification too. It bounds what a firing's page reads, with the disk.
const maxMutesPerRuleHour = 1000

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
	// As kept: JSON writes <, > and & and controls in six bytes each.
	if b, _ := json.Marshal(noLabelsIfNil(m.Labels)); len(b) > maxLabelBytes {
		return fmt.Errorf("%w: the muted alert: its labels are 4 KB at most in all, as kept", ErrBadMute)
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
// mute changes nothing. A new row of a rule's hour that holds
// maxMutesPerRuleHour already is passed over (errs, ErrMutesFull); one
// kept goes on being made longer. added is how many rows are new; err, a
// failure to write, after which nothing was kept. A row added is a page's,
// counted with the write; one only made longer isn't, and shows with the
// next.
func (l *Ledger) KeepMutes(ctx context.Context, ms []Mute) (added int, errs []error, err error) {
	errs = make([]error, len(ms))
	type row struct {
		key, rule, labels, kind, ref string
		hour, first, last            int64
		of                           []int // the mutes given it is of
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
			r.of = append(r.of, i)
			continue
		}
		b, _ := json.Marshal(noLabelsIfNil(m.Labels))
		r := &row{key: k, rule: m.Rule, labels: l.d.kept(string(b)), kind: m.Kind, ref: m.Ref, hour: hour, first: first, last: last, of: []int{i}}
		byKey[k+"/"+strconv.FormatInt(hour, 10)] = r
		order = append(order, r)
	}
	if len(order) == 0 {
		return 0, errs, nil
	}
	var full []*row
	err = l.writeAside(ctx, func(t *txn) error {
		added, full = 0, nil
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
		// How many rows each rule's hour holds, read once.
		held := map[ruleHour]int{}
		for _, r := range order {
			rh := ruleHour{r.rule, r.hour}
			n, ok := held[rh]
			if !ok {
				if err := t.QueryRowContext(t.ctx, "SELECT count(*) FROM mutes WHERE rule = ? AND hour = ?", r.rule, r.hour).Scan(&n); err != nil {
					return err
				}
			}
			var one int
			if n >= maxMutesPerRuleHour {
				// Full: only a row kept already is made longer.
				err := t.QueryRowContext(t.ctx, "SELECT 1 FROM mutes WHERE key = ? AND hour = ?", r.key, r.hour).Scan(&one)
				switch {
				case errors.Is(err, sql.ErrNoRows):
					full = append(full, r)
					held[rh] = n
					continue
				case err != nil:
					return err
				}
			} else {
				err := ins.QueryRowContext(t.ctx, r.key, r.hour, r.rule, r.labels, r.kind, r.ref, r.first, r.last).Scan(&one)
				switch {
				case err == nil:
					added++
					held[rh] = n + 1
					continue
				case !errors.Is(err, sql.ErrNoRows):
					return err
				}
			}
			held[rh] = n
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
	for _, r := range full {
		for _, i := range r.of {
			errs[i] = ErrMutesFull
		}
	}
	return added, errs, nil
}

// ruleHour is a rule's hour of mutes.
type ruleHour struct {
	rule string
	hour int64
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

// mutesOfFiring is how many mutes, by kind and ref, a firing is given at
// most, of those whose labels are its.
const mutesOfFiring = 20

// edgeHours is how many hours about a firing's start, and about its end,
// are read for its mutes at most.
const edgeHours = 6

// qMutesOf reads the mutes of a rule's hour, through mutes_rule: no more
// than the hour holds (maxMutesPerRuleHour), with room for replicas that
// added rows at once.
const qMutesOf = `SELECT labels, kind, ref, first_at, last_at FROM mutes WHERE rule = ? AND hour = ? LIMIT ?`

// muteRowsRead counts the rows of mutes MutedOf reads: for tests of what a
// page reads.
var muteRowsRead atomic.Int64

// MutedOf is, for each firing given, what muted it: the mutes of its rule
// whose labels are all the firing's and more (as a notification's alert's
// are), told of from within before its start to within after its end, or
// now while it is on. Only the hours about its start and its end are read,
// each of a rule once: a silence that mutes a firing no notification told
// of is told of as it starts, and goes on being told of until it ends.
// They come a silence first, then an inhibition, each the earliest first;
// one of a notification's that doesn't say which is left out where one
// that does is there. Firings nothing muted are left out.
func (l *Ledger) MutedOf(ctx context.Context, fs []Firing, within Within, now time.Time) (map[int64][]Mute, error) {
	type muteRow struct {
		labels      map[string]string
		kind, ref   string
		first, last int64
	}
	read := map[ruleHour][]muteRow{}
	labels := map[string]map[string]string{} // labels alike are read once
	rowsOf := func(rh ruleHour) ([]muteRow, error) {
		if rows, ok := read[rh]; ok {
			return rows, nil
		}
		var rows []muteRow
		err := l.eachRow(ctx, func(r scanner) error {
			muteRowsRead.Add(1)
			var x muteRow
			var text string
			if err := r.Scan(&text, &x.kind, &x.ref, &x.first, &x.last); err != nil {
				return err
			}
			if x.labels = labels[text]; x.labels == nil {
				if err := json.Unmarshal([]byte(text), &x.labels); err != nil {
					return err
				}
				labels[text] = x.labels
			}
			rows = append(rows, x)
			return nil
		}, qMutesOf, rh.rule, rh.hour, 2*maxMutesPerRuleHour)
		read[rh] = rows
		return rows, err
	}
	out := map[int64][]Mute{}
	for _, f := range fs {
		w := within(f.Rule)
		end := f.End
		if end.IsZero() {
			end = now
		}
		lo, hi := f.Start.Add(-w).Unix(), end.Add(w).Unix()
		if hi < lo {
			continue
		}
		// The hours about its start, and about its end, each once.
		var hours []int64
		for _, edge := range [][2]int64{{lo, f.Start.Add(w).Unix()}, {end.Add(-w).Unix(), hi}} {
			for h := floorHour(edge[0]); h <= floorHour(edge[1]) && h-floorHour(edge[0]) < edgeHours; h++ {
				if !slices.Contains(hours, h) {
					hours = append(hours, h)
				}
			}
		}
		var ms []Mute
		// Alerts with more labels than the firing's, one muted as
		// another, are one mute of it: from the first to the last.
		by := map[[2]string]int{}
		for _, h := range hours {
			rows, err := rowsOf(ruleHour{f.Rule, h})
			if err != nil {
				return nil, err
			}
			for _, x := range rows {
				if x.last < lo || x.first > hi || !among(f.Labels, x.labels) {
					continue
				}
				if i, ok := by[[2]string{x.kind, x.ref}]; ok {
					if t := unix(x.first); t.Before(ms[i].At) {
						ms[i].At = t
					}
					if t := unix(x.last); t.After(ms[i].Last) {
						ms[i].Last = t
					}
					continue
				}
				by[[2]string{x.kind, x.ref}] = len(ms)
				ms = append(ms, Mute{At: unix(x.first), Last: unix(x.last), Rule: f.Rule, Labels: x.labels, Kind: x.kind, Ref: x.ref})
			}
		}
		if len(ms) == 0 {
			continue
		}
		if slices.ContainsFunc(ms, func(m Mute) bool { return m.Kind != MutedEither }) {
			ms = slices.DeleteFunc(ms, func(m Mute) bool { return m.Kind == MutedEither })
		}
		rank := map[string]int{MutedSilence: 0, MutedInhibition: 1, MutedEither: 2}
		slices.SortFunc(ms, func(a, b Mute) int {
			return cmp.Or(cmp.Compare(rank[a.Kind], rank[b.Kind]), a.At.Compare(b.At), cmp.Compare(a.Ref, b.Ref))
		})
		out[f.ID] = ms[:min(len(ms), mutesOfFiring)]
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
