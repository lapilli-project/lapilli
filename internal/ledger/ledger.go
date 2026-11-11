// Package ledger keeps what lapilli serve finds: firings, incidents and
// versions of the rules, in SQLite.
//
// It keeps each firing's stones, given against every incident it has, and
// each rule's stones by day and by week, so that reading twelve weeks costs
// about what reading one does. A write gives stones again only where they
// can have changed: the firings that start near what it changed.
package ledger

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema/*.sql
var schema embed.FS

// Ledger is a ledger open for reading and writing. Its methods may be
// called at once from several goroutines; writes take turns.
type Ledger struct {
	w, r *sql.DB
	loc  *time.Location
	// mu is held by every write, start to commit.
	mu sync.Mutex
	// gen counts writes, for whoever caches what it reads.
	gen atomic.Int64
	// changed is, by week, the generation the firings in it last changed
	// in, since the ledger was opened; all, the last in which every week's
	// may have.
	changed struct {
		sync.Mutex
		by  map[int64]int64
		all int64
	}
	// tellTo are where what happens is to be sent, by the outbox: none,
	// and nothing is written there.
	tellTo atomic.Pointer[[]string]
	// recorded counts the writes that may have changed recorded rules or
	// their alerts; shared keeps what was worked out from them at a count.
	recorded atomic.Int64
	shared   struct {
		sync.Mutex
		at int64
		m  map[string]map[string]string
	}
	// ids caches rule and series ids, and series labels; written under mu,
	// only after the transaction that made them commits.
	ids struct {
		sync.RWMutex
		rules  map[string]int64
		series map[seriesKey]int64
		labels map[int64]labelsOf
	}
}

// labelsOf are a series' labels, and the text they were read from: an id
// a series pruned or rolled back had can come back as another's.
type labelsOf struct {
	text string
	m    map[string]string
}

type seriesKey struct {
	rule   int64
	labels string
}

// ErrNewer is a ledger written by a newer lapilli, which this one won't
// touch.
var ErrNewer = errors.New("the ledger was written by a newer lapilli")

// Open opens the ledger at path, making it if it isn't there, in the time
// zone its days and weeks are counted in. Opened in another zone than
// before, it counts them again.
func Open(ctx context.Context, path string, loc *time.Location) (*Ledger, error) {
	u := url.URL{Scheme: "file", Opaque: path}
	pragmas := "_pragma=busy_timeout(10000)&_pragma=temp_store(MEMORY)&_pragma=foreign_keys(1)"
	w, err := sql.Open("sqlite", u.String()+"?"+pragmas+
		"&_pragma=auto_vacuum(INCREMENTAL)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	// One connection writes: SQLite takes one writer at a time anyway, and
	// this way a write waits here rather than failing as busy.
	w.SetMaxOpenConns(1)
	w.SetConnMaxIdleTime(0)
	r, err := sql.Open("sqlite", u.String()+"?"+pragmas+"&_pragma=query_only(1)")
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	l := &Ledger{w: w, r: r, loc: loc}
	l.ids.rules = map[string]int64{}
	l.ids.series = map[seriesKey]int64{}
	l.ids.labels = map[int64]labelsOf{}
	if err := l.migrate(ctx); err != nil {
		l.Close()
		return nil, err
	}
	if err := l.zone(ctx); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Close closes the ledger.
func (l *Ledger) Close() error {
	return errors.Join(l.r.Close(), l.w.Close())
}

// Gen changes whenever anything in the ledger does.
func (l *Ledger) Gen() int64 { return l.gen.Load() }

// Changed is the generation in which the firings of the week from the
// Monday week last changed, or might have: what was read of the week at that
// generation or later still stands.
func (l *Ledger) Changed(week int64) int64 {
	l.changed.Lock()
	defer l.changed.Unlock()
	return max(l.changed.by[week], l.changed.all)
}

// ChangedAny is the generation in which the firings of any week last
// changed, or might have: what was read of firings at that generation or
// later still stands. Writes that change no firing, such as a notification
// kept, leave it.
func (l *Ledger) ChangedAny() int64 {
	l.changed.Lock()
	defer l.changed.Unlock()
	g := l.changed.all
	for _, v := range l.changed.by {
		g = max(g, v)
	}
	return g
}

// Location is the time zone the ledger counts days and weeks in.
func (l *Ledger) Location() *time.Location { return l.loc }

// migrate brings the schema up to date, one numbered file at a time.
func (l *Ledger) migrate(ctx context.Context) error {
	files, err := fs.Glob(schema, "schema/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(files)
	var have int
	if err := l.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&have); err != nil {
		return err
	}
	if have > len(files) {
		return fmt.Errorf("%w (schema %d; this one knows %d)", ErrNewer, have, len(files))
	}
	for i := have; i < len(files); i++ {
		b, err := schema.ReadFile(files[i])
		if err != nil {
			return err
		}
		tx, err := l.w.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(b)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", files[i], err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// zone counts days and weeks again when the ledger was last opened in
// another time zone.
func (l *Ledger) zone(ctx context.Context) error {
	was, err := l.meta(ctx, l.w, "zone")
	if err != nil || was == l.loc.String() {
		return err
	}
	return l.write(ctx, func(tx *txn) error {
		rows, err := tx.QueryContext(ctx, "SELECT id, start_at FROM firings")
		if err != nil {
			return err
		}
		type fix struct{ id, day int64 }
		var fixes []fix
		for rows.Next() {
			var id, start int64
			if err := rows.Scan(&id, &start); err != nil {
				rows.Close()
				return err
			}
			fixes = append(fixes, fix{id, l.day(start)})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		st, err := tx.PrepareContext(ctx, "UPDATE firings SET day = ? WHERE id = ?")
		if err != nil {
			return err
		}
		defer st.Close()
		for _, f := range fixes {
			if _, err := st.ExecContext(ctx, f.day, f.id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM days; DELETE FROM weeks"); err != nil {
			return err
		}
		var lo, hi sql.NullInt64
		if err := tx.QueryRowContext(ctx, "SELECT min(start_at), max(start_at) FROM firings").Scan(&lo, &hi); err != nil {
			return err
		}
		if lo.Valid {
			if err := l.tally(tx, lo.Int64, hi.Int64); err != nil {
				return err
			}
		}
		return setMeta(ctx, tx, "zone", l.loc.String())
	})
}

// txn is a write in progress. Ids it makes are kept apart until it
// commits, so a write that fails leaves the caches as they were.
type txn struct {
	*sql.Tx
	ctx    context.Context
	rules  map[string]int64
	series map[seriesKey]int64
	// gone are names a rule was known by that it no longer is: named
	// apart, it gave its name to another.
	gone map[string]bool
	// recorded says it may have changed recorded rules or their alerts.
	recorded bool
	// weeks are those whose firings it changed; all, that it may have
	// changed any week's.
	weeks []int64
	all   bool
	// versioned are the names the versions of the rules hold, read once.
	versioned map[string]bool
}

// versionNames are the names any version of the rules holds, read the
// first time they are asked for in a transaction.
func (t *txn) versionNames() (map[string]bool, error) {
	if t.versioned != nil {
		return t.versioned, nil
	}
	rows, err := t.QueryContext(t.ctx, "SELECT DISTINCT n.value FROM versions v, json_each(v.names) n")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		m[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	t.versioned = m
	return m, nil
}

// write runs f in a transaction, one write at a time, and counts it.
func (l *Ledger) write(ctx context.Context, f func(*txn) error) error {
	if err := l.writeAside(ctx, f); err != nil {
		return err
	}
	l.gen.Add(1)
	return nil
}

// writeAside is a write that changes nothing a page shows, such as noting a
// notice sent, so pages kept for the generation stay.
func (l *Ledger) writeAside(ctx context.Context, f func(*txn) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	t := &txn{Tx: tx, ctx: ctx, rules: map[string]int64{}, series: map[seriesKey]int64{}}
	if err := f(t); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if t.recorded {
		l.recorded.Add(1)
	}
	if len(t.weeks) > 0 || t.all {
		// The generation this write is counted in, or will be.
		g := l.gen.Load() + 1
		l.changed.Lock()
		if l.changed.by == nil {
			l.changed.by = map[int64]int64{}
		}
		for _, w := range t.weeks {
			l.changed.by[w] = g
		}
		if t.all {
			l.changed.all = g
		}
		l.changed.Unlock()
	}
	l.ids.Lock()
	for k := range t.gone {
		delete(l.ids.rules, k)
	}
	for k, v := range t.rules {
		l.ids.rules[k] = v
	}
	for k, v := range t.series {
		l.ids.series[k] = v
	}
	l.ids.Unlock()
	return nil
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (l *Ledger) meta(ctx context.Context, q querier, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func setMeta(ctx context.Context, tx *txn, key, value string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value", key, value)
	return err
}

// Cursor is when the last run that finished found firings up to; zero
// before the first.
func (l *Ledger) Cursor(ctx context.Context) (time.Time, error) {
	v, err := l.meta(ctx, l.r, "cursor")
	if err != nil || v == "" {
		return time.Time{}, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return unix(n), err
}

// Meta reads a value kept for lapilli serve, "" when there is none.
func (l *Ledger) Meta(ctx context.Context, key string) (string, error) {
	return l.meta(ctx, l.r, "x."+key)
}

// SetMeta keeps a value for lapilli serve.
func (l *Ledger) SetMeta(ctx context.Context, key, value string) error {
	return l.write(ctx, func(tx *txn) error { return setMeta(ctx, tx, "x."+key, value) })
}

// SetSwitch keeps a switch a person turned for lapilli serve, and notes in
// the changes who turned it which way: what says so.
func (l *Ledger) SetSwitch(ctx context.Context, key string, on bool, by, what string) error {
	v := "0"
	if on {
		v = "1"
	}
	return l.write(ctx, func(tx *txn) error {
		if err := setMeta(ctx, tx, "x."+key, v); err != nil {
			return err
		}
		return change(tx, by, what, nil, nil)
	})
}

// day is the date a time falls on in the ledger's zone, as days since
// 1970-01-01.
func (l *Ledger) day(t int64) int64 {
	y, m, d := time.Unix(t, 0).In(l.loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
}

// midnight is when a day starts in the ledger's zone: its first instant.
// Where the clocks skip midnight (America/Santiago, Atlantic/Azores), Go
// gives the hour before it, which is the day before: the day starts an hour
// on, when the clocks jump.
func (l *Ledger) midnight(day int64) int64 {
	y, m, d := time.Unix(day*86400, 0).UTC().Date()
	t := time.Date(y, m, d, 0, 0, 0, 0, l.loc)
	for l.day(t.Unix()) < day {
		t = t.Add(time.Hour)
	}
	return t.Unix()
}

// Day is the date t falls on in the ledger's zone, as days since
// 1970-01-01.
func (l *Ledger) Day(t time.Time) int64 { return l.day(t.Unix()) }

// Midnight is when a day starts.
func (l *Ledger) Midnight(day int64) time.Time { return unix(l.midnight(day)) }

// Week is the day its Monday falls on.
func Week(day int64) int64 { return day - (day+3)%7 }

func unix(t int64) time.Time { return time.Unix(t, 0).UTC() }

func nullable(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

func fromNull(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return unix(n.Int64)
}

func gz(v any) ([]byte, error) {
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	if err := json.NewEncoder(z).Encode(v); err != nil {
		return nil, err
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func ungz(b []byte, v any) error {
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer z.Close()
	return json.NewDecoder(io.LimitReader(z, 256<<20)).Decode(v)
}
