package ledger

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// A column of a table, as the ledger's database describes it.
type column struct {
	name, ty string
	notNull  bool
	def      string // its default, "NULL" for none, as SQLite writes it
	pk       bool
}

// tables are the ledger's tables, by name: what a copy copies. Neither
// database's own bookkeeping is one: SQLite's sqlite_sequence, Postgres's
// schema_version.
func (l *Ledger) tables(ctx context.Context) ([]string, error) {
	q := l.d.pick("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite%' ORDER BY name",
		`SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'
			AND table_name != 'schema_version' ORDER BY table_name COLLATE "C"`)
	rows, err := l.r.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// columns are a table's columns, in their order.
func (l *Ledger) columns(ctx context.Context, table string) ([]column, error) {
	q := l.d.pick(`SELECT name, type, "notnull", coalesce(dflt_value, 'NULL'), pk > 0 FROM pragma_table_info(?) ORDER BY cid`,
		`SELECT c.column_name, c.data_type, c.is_nullable = 'NO', coalesce(c.column_default, 'NULL'),
			EXISTS (SELECT 1 FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
				WHERE i.indrelid = to_regclass(c.table_name) AND i.indisprimary AND a.attname = c.column_name)
		FROM information_schema.columns c WHERE c.table_schema = current_schema() AND c.table_name = ? ORDER BY c.ordinal_position`)
	rows, err := l.r.QueryContext(ctx, q, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.name, &c.ty, &c.notNull, &c.def, &c.pk); err != nil {
			return nil, err
		}
		// Postgres writes a default with its type: ''::text.
		if i := strings.LastIndex(c.def, "::"); i > 0 && strings.HasPrefix(c.def, "'") {
			c.def = c.def[:i]
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// byReference are the tables in an order each comes after those it refers
// to (REFERENCES).
func (l *Ledger) byReference(ctx context.Context, names []string) ([]string, error) {
	refers := map[string][]string{}
	for _, n := range names {
		rows, err := l.r.QueryContext(ctx, l.d.pick(`SELECT DISTINCT "table" FROM pragma_foreign_key_list(?)`,
			`SELECT DISTINCT p.relname FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_class p ON p.oid = c.confrelid
				WHERE c.contype = 'f' AND t.relname = ? AND t.relnamespace = to_regnamespace(current_schema())`), n)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return nil, err
			}
			if p != n {
				refers[n] = append(refers[n], p)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	var order []string
	placed := map[string]bool{}
	for len(order) < len(names) {
		before := len(order)
		for _, n := range names {
			if !placed[n] && !slices.ContainsFunc(refers[n], func(p string) bool { return !placed[p] }) {
				placed[n] = true
				order = append(order, n)
			}
		}
		if len(order) == before {
			return nil, errors.New("the tables refer to each other in a circle")
		}
	}
	return order, nil
}
