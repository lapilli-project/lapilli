package ledger

import (
	"database/sql"
	"strings"
)

// rowsOut writes rows of one statement in a transaction: in SQLite, a row
// at a time through a prepared statement, which costs nothing to send; in
// Postgres, many rows a statement, as each statement waits a round trip to
// the server. The rows are written by flush at the latest, and must be
// before the transaction reads what they change.
type rowsOut struct {
	t    *txn
	one  *sql.Stmt // SQLite's
	head string    // Postgres's: the statement up to its rows
	tail string    // and after them
	row  string    // one row's parameters, cast to their types
	cols int
	// byID says the rows set columns by id, given last and sent first.
	byID bool
	args []any
}

// pgRows is how many rows a Postgres statement takes at most: well under
// its 65,535 parameters.
const pgRows = 1000

// inserting writes rows into a table's columns, each an integer (bigint)
// or a text.
func (t *txn) inserting(table string, cols []string, types []string) (*rowsOut, error) {
	q := "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES "
	if t.d == postgres {
		return &rowsOut{t: t, head: q, row: castRow(types), cols: len(cols)}, nil
	}
	st, err := t.PrepareContext(t.ctx, q+"("+strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")+")")
	if err != nil {
		return nil, err
	}
	return &rowsOut{t: t, one: st, cols: len(cols)}, nil
}

// updating sets columns of a table's rows by their id: each row is the id,
// then the columns' values, each an integer.
func (t *txn) updating(table string, cols []string) (*rowsOut, error) {
	set := make([]string, len(cols))
	for i, c := range cols {
		set[i] = c + " = ?"
	}
	if t.d == postgres {
		for i, c := range cols {
			set[i] = c + " = v." + c
		}
		types := make([]string, len(cols)+1)
		for i := range types {
			types[i] = "BIGINT"
		}
		return &rowsOut{t: t, head: "UPDATE " + table + " SET " + strings.Join(set, ", ") + " FROM (VALUES ",
			tail: ") AS v (id, " + strings.Join(cols, ", ") + ") WHERE " + table + ".id = v.id", row: castRow(types), cols: len(cols) + 1, byID: true}, nil
	}
	st, err := t.PrepareContext(t.ctx, "UPDATE "+table+" SET "+strings.Join(set, ", ")+" WHERE id = ?")
	if err != nil {
		return nil, err
	}
	return &rowsOut{t: t, one: st, cols: len(cols) + 1}, nil
}

// deleting deletes a table's rows by their ids.
func (t *txn) deleting(table string) (*rowsOut, error) {
	if t.d == postgres {
		return &rowsOut{t: t, head: "DELETE FROM " + table + " WHERE id IN (", tail: ")", row: "CAST(? AS BIGINT)", cols: 1}, nil
	}
	st, err := t.PrepareContext(t.ctx, "DELETE FROM "+table+" WHERE id = ?")
	if err != nil {
		return nil, err
	}
	return &rowsOut{t: t, one: st, cols: 1}, nil
}

// castRow is a row of parameters, each cast to its type.
func castRow(types []string) string {
	ps := make([]string, len(types))
	for i, ty := range types {
		ps[i] = "CAST(? AS " + ty + ")"
	}
	return "(" + strings.Join(ps, ", ") + ")"
}

// add writes a row, or keeps it for the next statement. For updating, the
// id comes last, as SQLite's statement takes it.
func (b *rowsOut) add(args ...any) error {
	if b.one != nil {
		_, err := b.one.ExecContext(b.t.ctx, args...)
		return err
	}
	if b.byID {
		// The id first, as the rows of VALUES name it.
		args = append([]any{args[len(args)-1]}, args[:len(args)-1]...)
	}
	b.args = append(b.args, args...)
	if len(b.args) >= pgRows*b.cols {
		return b.flush()
	}
	return nil
}

// flush writes the rows kept.
func (b *rowsOut) flush() error {
	if b.one != nil || len(b.args) == 0 {
		return nil
	}
	n := len(b.args) / b.cols
	var q strings.Builder
	q.Grow(len(b.head) + n*(len(b.row)+2) + len(b.tail))
	q.WriteString(b.head)
	for i := range n {
		if i > 0 {
			q.WriteString(", ")
		}
		q.WriteString(b.row)
	}
	q.WriteString(b.tail)
	_, err := b.t.ExecContext(b.t.ctx, q.String(), b.args...)
	b.args = b.args[:0]
	return err
}

// close writes what is kept, and lets go of the statement.
func (b *rowsOut) close() error {
	if b.one != nil {
		return b.one.Close()
	}
	return b.flush()
}
