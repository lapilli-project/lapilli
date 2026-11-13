package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/ledger/ledgertest"
)

// byDesign is where a Postgres ledger's schema differs from SQLite's on
// purpose, as describe says each, "" for none.
var byDesign = map[string][2]string{}

// A ledger in Postgres has SQLite's tables, columns and indexes: the same
// names, kinds (integer, text, bytes), NOT NULLs, defaults, keys, foreign
// keys and what a row's deletion does to them, CHECKs, and indexes with
// their expressions and WHERE, but for byDesign.
func TestPostgresSchemaSame(t *testing.T) {
	u := ledgertest.URL(t)
	if u == "" {
		t.Skip("no " + ledgertest.Env)
	}
	ctx := context.Background()
	lite, err := Open(ctx, t.TempDir()+"/ledger.db", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer lite.Close()
	pg, err := OpenPostgres(ctx, u, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	a, err := describe(ctx, lite)
	if err != nil {
		t.Fatal(err)
	}
	b, err := describe(ctx, pg)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range sortedKeys(a, b) {
		want := [2]string{a[k], a[k]}
		if d, ok := byDesign[k]; ok {
			want = d
		}
		if a[k] != want[0] || b[k] != want[1] {
			t.Errorf("%s:\n  SQLite   %q\n  Postgres %q", k, a[k], b[k])
		}
	}
	// Each kind is there to compare: a describe that found none of one
	// would find them alike.
	for _, kind := range []string{"fk ", "check ", "unique ", "index "} {
		if !slices.ContainsFunc(sortedKeys(a), func(k string) bool { return strings.HasPrefix(k, kind) }) {
			t.Errorf("no %s in SQLite's schema", kind)
		}
	}
	for _, k := range []string{"index firings_refired", "index series_alarm", "check grants", "fk notified(notification_id)"} {
		if a[k] == "" {
			t.Errorf("%s not described", k)
		}
	}
}

func sortedKeys(ms ...map[string]string) []string {
	var ks []string
	for _, m := range ms {
		for k := range m {
			if !slices.Contains(ks, k) {
				ks = append(ks, k)
			}
		}
	}
	slices.Sort(ks)
	return ks
}

// describe is a ledger's schema, by table and column, key, foreign key,
// CHECK and index, in words both databases give alike.
func describe(ctx context.Context, l *Ledger) (map[string]string, error) {
	out := map[string]string{}
	kind := func(ty string) string {
		switch ty = strings.ToLower(ty); {
		case ty == "integer" && l.d == sqlite, ty == "bigint" && l.d == postgres:
			return "integer"
		case strings.Contains(ty, "int"):
			return ty + ", narrower than SQLite's"
		case ty == "text":
			return "text"
		case ty == "blob" || ty == "bytea":
			return "bytes"
		}
		return ty
	}
	tables, err := l.tables(ctx)
	if err != nil {
		return nil, err
	}
	for _, tb := range tables {
		cols, err := l.columns(ctx, tb)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, c := range cols {
			names = append(names, c.name)
			out[tb+"."+c.name] = kind(c.ty) + " notnull=" + strconv.FormatBool(c.notNull || c.pk) + " default=" + c.def
		}
		out[tb] = strings.Join(names, ", ")
	}
	// A text compared other than byte by byte, in Postgres, isn't SQLite's.
	if l.d == postgres {
		rows, err := l.r.QueryContext(ctx, `SELECT table_name, column_name FROM information_schema.columns
			WHERE table_schema = current_schema() AND data_type = 'text' AND collation_name IS DISTINCT FROM 'C'`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var tb, c string
			if err := rows.Scan(&tb, &c); err != nil {
				rows.Close()
				return nil, err
			}
			out[tb+"."+c] += " collated as the database is"
		}
		rows.Close()
	}
	for _, f := range []func(context.Context, *Ledger, []string, map[string]string) error{indexes, foreignKeys, checks} {
		if err := f(ctx, l, tables, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

var (
	sqliteJSON = regexp.MustCompile(`json_extract\(([a-z_.]+),'\$\.([a-z_]+)'\)`)
	pgJSON     = regexp.MustCompile(`\(*([a-z_.]+)\)*->>'([a-z_]+)'`)
	pgCast     = regexp.MustCompile(`::[a-z]+`)
	stoodFor   = []*regexp.Regexp{regexp.MustCompile(`^md5\((.*)\)$`), regexp.MustCompile(`^"left"\((.*), 500\)$`)}
)

// normal is an SQL expression in words both databases give alike: in
// lower case, with no spaces, casts, COLLATE "C" or parentheses, a JSON
// object's text under a key as labels.key, <> as !=, and ILIKE as LIKE,
// which SQLite's is (017.sql).
func normal(s string) string {
	s = pgCast.ReplaceAllString(strings.ToLower(s), "")
	s = strings.Join(strings.Fields(s), "")
	s = strings.ReplaceAll(s, `collate"c"`, "")
	s = sqliteJSON.ReplaceAllString(s, "$1.$2")
	s = pgJSON.ReplaceAllString(s, "$1.$2")
	return strings.NewReplacer("<>", "!=", "~~*", "like", "(", "", ")", "").Replace(s)
}

// key is an index's key in words both databases give alike: a text kept
// by its md5, or its first 500 characters, which a btree holds where it
// can't hold the text (017.sql), stands for the text.
func key(s string) string {
	for _, re := range stoodFor {
		s = re.ReplaceAllString(s, "$1")
	}
	return normal(s)
}

// indexes are a ledger's indexes: one unique, of every row (a key, a
// UNIQUE, or a unique index), by its table and keys; every other, by name,
// with its table, keys, whether it is unique, its WHERE and its kind.
func indexes(ctx context.Context, l *Ledger, tables []string, out map[string]string) error {
	type index struct {
		table, name, where, using string
		unique                    bool
		keys                      []string
	}
	var all []index
	if l.d == postgres {
		rows, err := l.r.QueryContext(ctx, `SELECT t.relname, i.relname, x.indisunique, coalesce(pg_get_expr(x.indpred, x.indrelid, true), ''), a.amname,
				ARRAY(SELECT pg_get_indexdef(x.indexrelid, k, true) FROM generate_series(1, x.indnkeyatts) k)
			FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid JOIN pg_class t ON t.oid = x.indrelid JOIN pg_am a ON a.oid = i.relam
			WHERE t.relnamespace = to_regnamespace(current_schema()) AND t.relname != 'schema_version'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x index
			var keys []byte
			if err := rows.Scan(&x.table, &x.name, &x.unique, &x.where, &x.using, &keys); err != nil {
				return err
			}
			for _, k := range pgArray(string(keys)) {
				x.keys = append(x.keys, key(k))
			}
			all = append(all, x)
		}
		if err := rows.Err(); err != nil {
			return err
		}
	} else {
		for _, tb := range tables {
			rows, err := l.r.QueryContext(ctx, `SELECT il.name, il."unique", coalesce(m.sql, '') FROM pragma_index_list(?) il
				LEFT JOIN sqlite_master m ON m.type = 'index' AND m.name = il.name`, tb)
			if err != nil {
				return err
			}
			for rows.Next() {
				x := index{table: tb, using: "btree"}
				var text string
				if err := rows.Scan(&x.name, &x.unique, &text); err != nil {
					rows.Close()
					return err
				}
				if text != "" {
					x.keys, x.where = sqliteIndex(text)
				}
				all = append(all, x)
			}
			rows.Close()
			for i := range all {
				if all[i].table != tb || all[i].keys != nil {
					continue
				}
				cols, err := l.r.QueryContext(ctx, "SELECT name FROM pragma_index_info(?) ORDER BY seqno", all[i].name)
				if err != nil {
					return err
				}
				for cols.Next() {
					var k string
					cols.Scan(&k)
					all[i].keys = append(all[i].keys, k)
				}
				cols.Close()
			}
		}
		// SQLite's INTEGER PRIMARY KEY is the rowid, in no index.
		for _, tb := range tables {
			var pk string
			if err := l.r.QueryRowContext(ctx, `SELECT coalesce((SELECT name FROM pragma_table_info(?) WHERE pk = 1 AND lower(type) = 'integer'
				AND (SELECT count(*) FROM pragma_table_info(?) WHERE pk > 0) = 1), '')`, tb, tb).Scan(&pk); err != nil {
				return err
			}
			if pk != "" {
				all = append(all, index{table: tb, unique: true, keys: []string{pk}})
			}
		}
	}
	for _, x := range all {
		on := x.table + "(" + strings.Join(x.keys, ", ") + ")"
		if x.unique && x.where == "" && x.using == "btree" || x.unique && x.name == "" {
			out["unique "+on] = "unique"
			continue
		}
		v := on + " unique=" + strconv.FormatBool(x.unique)
		if x.where != "" {
			v += " where=" + normal(x.where)
		}
		out["index "+x.name] = v + " using=" + x.using
	}
	return nil
}

// sqliteIndex is the keys and WHERE of SQLite's CREATE INDEX.
func sqliteIndex(text string) ([]string, string) {
	open := strings.Index(text, "(")
	depth, end := 0, len(text)
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 {
			end = i
			break
		}
	}
	var keys []string
	depth, from := 0, open+1
	for i := open + 1; i <= end; i++ {
		switch {
		case text[i] == '(':
			depth++
		case text[i] == ')' && depth > 0:
			depth--
		case depth == 0 && (text[i] == ',' || i == end):
			keys = append(keys, key(text[from:i]))
			from = i + 1
		}
	}
	where := ""
	if _, w, ok := strings.Cut(text[end+1:], "WHERE"); ok {
		where = w
	}
	return keys, where
}

// pgArray is a Postgres text[] as text, {a,"b c"}, split.
func pgArray(s string) []string {
	var out []string
	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	for s != "" {
		var e string
		if s[0] == '"' {
			i := 1
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' {
					i++
				}
			}
			e = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(s[1:i])
			s = strings.TrimPrefix(s[min(i+1, len(s)):], ",")
		} else {
			e, s, _ = strings.Cut(s, ",")
		}
		out = append(out, e)
	}
	return out
}

// foreignKeys are a ledger's foreign keys, by table and columns, with what
// they refer to and what deleting or changing it does.
func foreignKeys(ctx context.Context, l *Ledger, tables []string, out map[string]string) error {
	put := func(tb, from, ref, to, del, upd string) {
		out["fk "+tb+"("+from+")"] = ref + "(" + to + ") on delete " + strings.ToLower(del) + ", on update " + strings.ToLower(upd)
	}
	if l.d == postgres {
		action := map[string]string{"a": "no action", "r": "restrict", "c": "cascade", "n": "set null", "d": "set default"}
		rows, err := l.r.QueryContext(ctx, `SELECT t.relname, r.relname,
				array_to_string(ARRAY(SELECT a.attname FROM unnest(c.conkey) WITH ORDINALITY k (n, i) JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.n ORDER BY k.i), ', '),
				array_to_string(ARRAY(SELECT a.attname FROM unnest(c.confkey) WITH ORDINALITY k (n, i) JOIN pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = k.n ORDER BY k.i), ', '),
				c.confdeltype, c.confupdtype
			FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_class r ON r.oid = c.confrelid
			WHERE c.contype = 'f' AND t.relnamespace = to_regnamespace(current_schema())`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tb, ref, from, to, del, upd string
			if err := rows.Scan(&tb, &ref, &from, &to, &del, &upd); err != nil {
				return err
			}
			put(tb, from, ref, to, action[del], action[upd])
		}
		return rows.Err()
	}
	for _, tb := range tables {
		type fk struct {
			ref, del, upd string
			from, to      []string
		}
		byID := map[int]*fk{}
		var ids []int
		rows, err := l.r.QueryContext(ctx, `SELECT id, "table", "from", coalesce("to", ''), on_delete, on_update FROM pragma_foreign_key_list(?) ORDER BY id, seq`, tb)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int
			var ref, from, to, del, upd string
			if err := rows.Scan(&id, &ref, &from, &to, &del, &upd); err != nil {
				rows.Close()
				return err
			}
			if byID[id] == nil {
				byID[id] = &fk{ref: ref, del: del, upd: upd}
				ids = append(ids, id)
			}
			byID[id].from = append(byID[id].from, from)
			byID[id].to = append(byID[id].to, to)
		}
		rows.Close()
		for _, id := range ids {
			f := byID[id]
			if f.to[0] == "" { // the table's key
				f.to = nil
				if err := eachIn(ctx, l.r, func(c string) { f.to = append(f.to, c) }, "SELECT name FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk", f.ref); err != nil {
					return err
				}
			}
			put(tb, strings.Join(f.from, ", "), f.ref, strings.Join(f.to, ", "), f.del, f.upd)
		}
	}
	return nil
}

func eachIn(ctx context.Context, db *sql.DB, f func(string), q string, args ...any) error {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		f(s)
	}
	return rows.Err()
}

// checks are a ledger's CHECKs, by table, in words both databases give
// alike, sorted.
func checks(ctx context.Context, l *Ledger, tables []string, out map[string]string) error {
	by := map[string][]string{}
	if l.d == postgres {
		rows, err := l.r.QueryContext(ctx, `SELECT t.relname, pg_get_constraintdef(c.oid, true) FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid
			WHERE c.contype = 'c' AND t.relnamespace = to_regnamespace(current_schema())`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tb, def string
			if err := rows.Scan(&tb, &def); err != nil {
				return err
			}
			by[tb] = append(by[tb], normal(strings.TrimPrefix(def, "CHECK ")))
		}
		if err := rows.Err(); err != nil {
			return err
		}
	} else {
		for _, tb := range tables {
			var text string
			if err := l.r.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", tb).Scan(&text); err != nil {
				return err
			}
			rest := text
			for {
				i := strings.Index(strings.ToUpper(rest), "CHECK (")
				if i < 0 {
					break
				}
				rest = rest[i+len("CHECK "):]
				depth, end := 0, len(rest)
				for j := range len(rest) {
					switch rest[j] {
					case '(':
						depth++
					case ')':
						depth--
					}
					if depth == 0 {
						end = j
						break
					}
				}
				by[tb] = append(by[tb], normal(rest[:end+1]))
				rest = rest[end+1:]
			}
		}
	}
	for tb, cs := range by {
		slices.Sort(cs)
		out["check "+tb] = fmt.Sprint(cs)
	}
	return nil
}
