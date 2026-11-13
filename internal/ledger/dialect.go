package ledger

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// dialect is the database a ledger is in. The statements are SQLite's,
// parameters and all, and Postgres takes most of them as they are (its
// parameters are numbered for it, rebind in postgres.go). Where the two
// differ, the statement is given twice, with pick, or made by one of the
// methods below: TestDialectSeam fails on SQLite's own words anywhere else.
type dialect int

const (
	sqlite dialect = iota
	postgres
)

// kept is a label set's JSON, or another text Postgres reads as jsonb, as
// the ledger's database keeps it: in Postgres, its NULs U+FFFD (pgJSON).
// What is told apart by its text, a label set above all, is told apart by
// this, so that what is asked for is what is kept.
func (d dialect) kept(s string) string {
	if d == postgres {
		return pgJSON(s)
	}
	return s
}

// pick is the statement for the ledger's database: SQLite's, or
// Postgres's.
func (d dialect) pick(sqlite, pg string) string {
	if d == postgres {
		return pg
	}
	return sqlite
}

// text is the text a JSON object holds under a key, compared byte by byte
// as SQLite compares it; NULL where it holds none. In SQLite it is the
// expression the indexes on alarm ids (series_alarm, notified_alarm) are
// made on, when expr is labels; in Postgres, alarmPrefix of it is.
func (d dialect) text(expr, key string) string {
	if d == postgres {
		return "((" + expr + "::jsonb ->> '" + key + "') COLLATE \"C\")"
	}
	return "json_extract(" + expr + ", '$." + key + "')"
}

// alarmPrefix is what series_alarm and notified_alarm hold of an alarm's
// id, v: in Postgres its first 500 characters, as a btree holds some 2,700
// bytes at most and an alert's labels are 4 KB; in SQLite, v. Equal ids
// have equal prefixes, and an id from 'projects/' to 'projects0' a prefix
// there too: a condition on the prefix finds through the index the rows
// one on the id keeps.
func (d dialect) alarmPrefix(v string) string {
	if d == postgres {
		return "left(" + v + ", 500)"
	}
	return v
}

// texts are the texts a JSON list holds, given as a parameter, for IN:
// IN (SELECT ...).
func (d dialect) texts(param string) string {
	if d == postgres {
		return "SELECT value COLLATE \"C\" FROM jsonb_array_elements_text(CAST(" + param + " AS jsonb))"
	}
	return "SELECT value FROM json_each(" + param + ")"
}

// ints are the integers a JSON list holds, given as a parameter, for IN.
func (d dialect) ints(param string) string {
	if d == postgres {
		return "SELECT CAST(value AS BIGINT) FROM jsonb_array_elements_text(CAST(" + param + " AS jsonb))"
	}
	return "SELECT value FROM json_each(" + param + ")"
}

// keys is a table of the keys and values of a JSON object of texts, named
// as, with columns key and value, for a FROM.
func (d dialect) keys(expr, as string) string {
	if d == postgres {
		return "LATERAL (SELECT key COLLATE \"C\" AS key, value COLLATE \"C\" AS value FROM jsonb_each_text(" + expr + "::jsonb)) " + as
	}
	return "json_each(" + expr + ") " + as
}

// leftKeys is keys, LEFT JOINed: an object with none gives one row of
// NULLs.
func (d dialect) leftKeys(expr, as string) string {
	if d == postgres {
		return "LEFT JOIN " + d.keys(expr, as) + " ON true"
	}
	return "LEFT JOIN " + d.keys(expr, as)
}

// elems is a table of the texts a JSON list holds, named as, with the
// column value, for a FROM.
func (d dialect) elems(expr, as string) string {
	if d == postgres {
		return "LATERAL (SELECT value COLLATE \"C\" AS value FROM jsonb_array_elements_text(" + expr + "::jsonb)) " + as
	}
	return "json_each(" + expr + ") " + as
}

// greatest is the greater of two that are never NULL (SQLite's max of
// two is NULL when one is; Postgres's greatest isn't).
func (d dialect) greatest(a, b string) string {
	if d == postgres {
		return "greatest(" + a + ", " + b + ")"
	}
	return "max(" + a + ", " + b + ")"
}

// limit is what LIMIT ? is given for n rows at most: below zero, every
// row, which SQLite takes -1 for and Postgres NULL.
func (d dialect) limit(n int) any {
	if n < 0 && d == postgres {
		return nil
	}
	return n
}

// strpos is where b first is in a, from 1; 0 when it isn't.
func (d dialect) strpos(a, b string) string {
	if d == postgres {
		return "strpos(" + a + ", " + b + ")"
	}
	return "instr(" + a + ", " + b + ")"
}

// unique says a write was refused by a unique key or index, in either
// database.
func unique(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "23505"
	}
	return strings.Contains(err.Error(), "UNIQUE")
}
