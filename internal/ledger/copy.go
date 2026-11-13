package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// CopyReport is what a copy wrote, by table, and that it read the same
// back.
type CopyReport struct {
	Tables []CopiedTable
}

// CopiedTable is one table copied: how many rows, and the SHA-256 of what
// both ledgers hold, read back in the same order.
type CopiedTable struct {
	Name string
	Rows int64
	Sum  string
}

// ErrNotEmpty is a ledger to copy into that holds a ledger already.
var ErrNotEmpty = errors.New("the ledger to copy into isn't empty: copy into a new one, or one that was only opened")

// Copy copies every table of one ledger into another, which must be
// empty: as a new ledger is, or one only opened, which holds the zone it
// was opened in and nothing else. Both are at this lapilli's schema
// (opening one brings it there). It writes in one transaction, so a copy
// that fails leaves the other ledger empty; ids are kept, and the next a
// table gives comes after the highest any of its rows ever had. Before it
// commits, it reads both back, table by table, and refuses what differs,
// leaving the other ledger empty. The ledger copied from is read as of one
// moment, its ids given, rows and their read back alike: one written
// meanwhile, by a serve left running, has its later writes left out.
func Copy(ctx context.Context, from, to *Ledger, progress func(table string, rows int64)) (CopyReport, error) {
	var rep CopyReport
	names, err := from.tables(ctx)
	if err != nil {
		return rep, err
	}
	theirs, err := to.tables(ctx)
	if err != nil {
		return rep, err
	}
	if !slices.Equal(names, theirs) {
		return rep, fmt.Errorf("the two ledgers' tables differ (%d and %d): not the same schema", len(names), len(theirs))
	}
	cols := map[string][]column{}
	for _, n := range names {
		a, err := from.columns(ctx, n)
		if err != nil {
			return rep, err
		}
		b, err := to.columns(ctx, n)
		if err != nil {
			return rep, err
		}
		if !slices.EqualFunc(a, b, func(x, y column) bool { return x.name == y.name }) {
			return rep, fmt.Errorf("%s: the two ledgers' columns differ: not the same schema", n)
		}
		cols[n] = b
	}
	if err := empty(ctx, to, names); err != nil {
		return rep, err
	}
	// A table before those that refer to it.
	order, err := from.byReference(ctx, names)
	if err != nil {
		return rep, err
	}
	// One transaction reads the ledger copied from as of one moment (a
	// reader's is REPEATABLE READ in Postgres; SQLite's, from its first
	// read): the ids given, the rows, and the read back.
	src, err := from.r.BeginTx(ctx, nil)
	if err != nil {
		return rep, err
	}
	defer src.Rollback()
	// The ids given, past those kept: SQLite's AUTOINCREMENT, Postgres's
	// identity columns.
	given, err := from.idsGiven(ctx, src, names, cols)
	if err != nil {
		return rep, err
	}
	err = to.writeAside(ctx, func(t *txn) error {
		if _, err := t.ExecContext(t.ctx, "DELETE FROM meta"); err != nil {
			return err
		}
		for _, n := range order {
			k, err := copyTable(ctx, src, t, n, cols[n])
			if err != nil {
				return fmt.Errorf("%s: %w", n, err)
			}
			if progress != nil {
				progress(n, k)
			}
		}
		if err := t.setGiven(given, cols); err != nil {
			return err
		}
		for _, n := range names {
			a, k, err := checksum(ctx, src, n, cols[n])
			if err != nil {
				return err
			}
			b, k2, err := checksum(ctx, t, n, cols[n])
			if err != nil {
				return err
			}
			if a != b || k != k2 {
				return fmt.Errorf("%s: read back, the copy differs (%d rows, %d copied)", n, k, k2)
			}
			rep.Tables = append(rep.Tables, CopiedTable{n, k, a})
		}
		return nil
	})
	if err != nil {
		return CopyReport{}, err
	}
	return rep, nil
}

// rowsQuerier is what a table is read through: a ledger's readers, or a
// transaction.
type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// empty refuses a ledger that holds more than opening it wrote.
func empty(ctx context.Context, l *Ledger, names []string) error {
	var held []string
	for _, n := range names {
		var k int64
		q := "SELECT count(*) FROM " + n
		if n == "meta" {
			q += " WHERE key != 'zone'"
		}
		if err := l.r.QueryRowContext(ctx, q).Scan(&k); err != nil {
			return err
		}
		if k > 0 {
			held = append(held, fmt.Sprintf("%d in %s", k, n))
		}
	}
	if len(held) > 0 {
		return fmt.Errorf("%w (it holds rows: %s)", ErrNotEmpty, strings.Join(held, ", "))
	}
	return nil
}

// copyTable copies a table's rows, in the order of their keys.
func copyTable(ctx context.Context, src *sql.Tx, t *txn, table string, cols []column) (int64, error) {
	names := make([]string, len(cols))
	types := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.name
		types[i] = strings.ToUpper(c.ty)
	}
	rows, err := src.QueryContext(ctx, "SELECT "+strings.Join(names, ", ")+" FROM "+table+" ORDER BY "+orderOf(cols))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	out, err := t.inserting(table, names, types)
	if err != nil {
		return 0, err
	}
	var k int64
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			out.close()
			return 0, err
		}
		row := make([]any, len(vals))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				// Scanned bytes are the driver's until the next row.
				v = slices.Clone(b)
				if !binaryColumn(cols[i]) {
					v = string(b)
				}
			}
			row[i] = v
		}
		if copying != nil {
			copying(table, row)
		}
		if err := out.add(row...); err != nil {
			out.close()
			return 0, err
		}
		k++
	}
	if err := rows.Err(); err != nil {
		out.close()
		return 0, err
	}
	return k, out.close()
}

// copying, when set, is given each row as it is copied, and may change it:
// for tests of what reading back finds.
var copying func(table string, row []any)

// orderOf is an ORDER BY a table's rows read back the same in either
// database by: its key, or, for a table with none, every column. NULLs
// first, as SQLite puts them.
func orderOf(cols []column) string {
	var by []string
	for _, c := range cols {
		if c.pk {
			by = append(by, c.name)
		}
	}
	if len(by) == 0 {
		for _, c := range cols {
			by = append(by, c.name)
		}
	}
	for i := range by {
		by[i] += " NULLS FIRST"
	}
	return strings.Join(by, ", ")
}

// idsGiven are, for each table with an id of its own, the highest id it
// has given, its rows' and those deleted since, as far as the database
// keeps them: SQLite keeps those of AUTOINCREMENT's tables (tasks and
// comments), Postgres every identity's.
func (l *Ledger) idsGiven(ctx context.Context, q rowsQuerier, names []string, cols map[string][]column) (map[string]int64, error) {
	out := map[string]int64{}
	for _, n := range names {
		if !slices.ContainsFunc(cols[n], func(c column) bool { return c.name == "id" && c.pk }) {
			continue
		}
		var high sql.NullInt64
		if err := q.QueryRowContext(ctx, "SELECT max(id) FROM "+n).Scan(&high); err != nil {
			return nil, err
		}
		out[n] = high.Int64
		var seq sql.NullInt64
		// Postgres's sequences are read as they are now, past the moment
		// the rest is read as of: a next id later than it needs be.
		s := l.d.pick("SELECT seq FROM sqlite_sequence WHERE name = ?",
			`SELECT s.last_value FROM pg_sequences s
				WHERE to_regclass(quote_ident(s.schemaname) || '.' || quote_ident(s.sequencename)) = to_regclass(pg_get_serial_sequence(?, 'id'))`)
		if err := q.QueryRowContext(ctx, s, n).Scan(&seq); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		out[n] = max(out[n], seq.Int64)
	}
	return out, nil
}

// setGiven makes each table's next id come after the highest given.
func (t *txn) setGiven(given map[string]int64, cols map[string][]column) error {
	for _, n := range slices.Sorted(func(yield func(string) bool) {
		for n := range given {
			if !yield(n) {
				return
			}
		}
	}) {
		high := given[n]
		if high <= 0 {
			continue
		}
		if t.d == postgres {
			if _, err := t.ExecContext(t.ctx, "SELECT setval(pg_get_serial_sequence(?, 'id'), ?)", n, high); err != nil {
				return err
			}
			continue
		}
		// SQLite keeps the highest only for its AUTOINCREMENT tables; for
		// the rest, the highest row's is it.
		var auto int
		if err := t.QueryRowContext(t.ctx, t.d.pick("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ? AND sql LIKE '%AUTOINCREMENT%'", ""),
			n).Scan(&auto); err != nil {
			return err
		}
		if auto == 0 {
			continue
		}
		for _, q := range []string{t.d.pick("DELETE FROM sqlite_sequence WHERE name = ?1", ""), t.d.pick("INSERT INTO sqlite_sequence (name, seq) VALUES (?1, ?2)", "")} {
			if _, err := t.ExecContext(t.ctx, q, n, high); err != nil {
				return err
			}
		}
	}
	return nil
}

// checksum is the SHA-256 of a table's rows, read in the order of their
// keys, each value written with its kind: the same rows give the same sum
// in either database.
func checksum(ctx context.Context, q rowsQuerier, table string, cols []column) (string, int64, error) {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.name
	}
	rows, err := q.QueryContext(ctx, "SELECT "+strings.Join(names, ", ")+" FROM "+table+" ORDER BY "+orderOf(cols))
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	h := sha256.New()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var k int64
	var n [8]byte
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return "", 0, err
		}
		for i, v := range vals {
			// A text, compared as Postgres keeps it (pgText).
			if b, ok := v.([]byte); ok && !binaryColumn(cols[i]) {
				v = string(b)
			}
			if s, ok := v.(string); ok {
				v = pgText(s)
			}
			switch v := v.(type) {
			case nil:
				h.Write([]byte{'n'})
			case int64:
				h.Write([]byte{'i'})
				binary.BigEndian.PutUint64(n[:], uint64(v))
				h.Write(n[:])
			case float64:
				h.Write([]byte{'f'})
				binary.BigEndian.PutUint64(n[:], math.Float64bits(v))
				h.Write(n[:])
			case string:
				writeBytes(h, []byte(v))
			case []byte:
				writeBytes(h, v)
			default:
				return "", 0, fmt.Errorf("%s: a value of a kind not kept (%T)", table, v)
			}
		}
		k++
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), k, nil
}

// binaryColumn says a column holds bytes, not text.
func binaryColumn(c column) bool {
	return strings.EqualFold(c.ty, "BLOB") || strings.EqualFold(c.ty, "BYTEA")
}

func writeBytes(h interface{ Write([]byte) (int, error) }, b []byte) {
	h.Write([]byte{'b'})
	h.Write([]byte(strconv.Itoa(len(b)) + ":"))
	h.Write(b)
}
