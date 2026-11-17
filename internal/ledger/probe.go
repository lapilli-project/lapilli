package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"modernc.org/sqlite/vfs"
)

// Probed is what a probe of a ledger read of it, writing nothing
// (lapilli preflight, docs/design/preflight.md).
type Probed struct {
	// There says there is a ledger: the SQLite file, or a Postgres schema
	// holding schema_version.
	There bool
	// Version is the ledger's schema, 0 for none; Knows, the one this
	// lapilli brings a ledger to as it opens it.
	Version, Knows int
	// Schema, of Postgres, is the schema the ledger is in, or would be
	// made in: the first of the search_path. Create says the user may
	// create tables in it, which a migration needs.
	Schema string
	Create bool
	// Holder, of Postgres, is the session that holds the ledger's lock now,
	// nil for none: a serve running, or a copy.
	Holder *LockedError
	// Warning, of Postgres, is what the connection string asks that isn't
	// safe (TLS that checks no certificate), as serve would warn of it.
	Warning string
}

// Probe reads the SQLite ledger serve keeps in dir (DIR/ledger.db): its
// connection read-only (mode=ro) and taking no write (PRAGMA query_only),
// and opened so that SQLite neither makes nor changes a file beside it,
// whatever is there (beside). A ledger that isn't there is a new one:
// There false, and nothing made. A ledger.db that is a link is read where
// it leads, as SQLite reads it, and what is beside it is looked for there.
func Probe(ctx context.Context, dir string) (Probed, error) {
	p := Probed{}
	n, err := sqliteFiles()
	if err != nil {
		return p, err
	}
	p.Knows = n
	abs, err := filepath.Abs(filepath.Join(dir, "ledger.db"))
	if err != nil {
		return p, err
	}
	switch fi, err := os.Stat(abs); {
	case errors.Is(err, os.ErrNotExist):
		return p, nil
	case err != nil:
		return p, err
	case !fi.Mode().IsRegular():
		return p, fmt.Errorf("%s isn't a file", abs)
	}
	p.There = true
	// SQLite opens what a link leads to, and looks for its log beside
	// that: looked for beside the link, a log there would be missed, and
	// the file opened as immutable, read without it.
	if abs, err = filepath.EvalSymlinks(abs); err != nil {
		return p, err
	}
	how, err := beside(abs)
	if err != nil {
		return p, err
	}
	db, done, err := openProbe(abs, how, probeQuery(how))
	if err != nil {
		return p, err
	}
	defer done()
	db.SetMaxOpenConns(1)
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&p.Version); err != nil {
		return p, err
	}
	return p, nil
}

// probing is how a probe opens a SQLite ledger, by what is beside it.
type probing int

const (
	// alone: no write-ahead log beside it. Nothing writes it now, or it
	// would have one: all of it is in the file, which is opened as
	// immutable, and SQLite makes nothing beside it.
	alone probing = iota
	// indexed: a log and its index (-shm). A serve has it open, or had
	// and stopped hard. The index is opened read-only too (readonly_shm),
	// never written: SQLite reads through it where a serve holds it, and
	// where none does, builds an index of its own in memory from the log.
	// Opened read-write, as mode=ro alone opens it, a reader writes its
	// mark in it, and one that finds it stale writes it anew.
	indexed
	// unindexed: a log and no index (a serve that stopped hard and its
	// -shm cleared, a copy of the two files). No one has it open, and to
	// read the log SQLite would make the index beside it. The two files
	// are read through a file system of their own that has no write
	// (modernc.org/sqlite/vfs), the index kept in memory, as SQLite keeps
	// it with locking_mode EXCLUSIVE: that file system takes no lock, and
	// no one has the ledger open to need one.
	unindexed
)

// ErrCutShort is a SQLite ledger whose file is shorter than a page with a
// write-ahead log beside it. SQLite takes such a file for an empty
// database, and, opening it, deletes the log beside it, which holds the
// ledger: a probe doesn't open it.
var ErrCutShort = errors.New("the ledger's file is cut short: it is shorter than a page of SQLite's, with a write-ahead log (-wal) beside it, which opening it would delete")

// smallestPage is the smallest page SQLite writes, in bytes. A ledger with
// a write-ahead log has a page in its file at least: SQLite writes the
// first as it turns the log on.
const smallestPage = 512

// beside says how a probe opens the SQLite file at abs, by what is beside
// it; ErrCutShort for a file shorter than a page with a log beside it,
// which isn't opened at all.
func beside(abs string) (probing, error) {
	there := func(name string) (bool, error) {
		_, err := os.Stat(name)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
	wal, err := there(abs + "-wal")
	if err != nil || !wal {
		return alone, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return alone, err
	}
	if fi.Size() < smallestPage {
		return alone, ErrCutShort
	}
	shm, err := there(abs + "-shm")
	if err != nil {
		return alone, err
	}
	if shm {
		return indexed, nil
	}
	return unindexed, nil
}

// probeQuery is the query of the URI a probe opens a SQLite ledger with,
// as what is beside it says (probing).
func probeQuery(how probing) string {
	q := "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(10000)"
	switch how {
	case alone:
		q += "&immutable=1"
	case indexed:
		q += "&readonly_shm=1"
	case unindexed:
		q += "&_pragma=locking_mode(EXCLUSIVE)"
	}
	return q
}

// openProbe opens the SQLite file at abs as a probe does, with the URI's
// query q; done closes it, and what it was read through.
func openProbe(abs string, how probing, q string) (db *sql.DB, done func(), err error) {
	if how != unindexed {
		u := fileURI(abs)
		db, err = sql.Open("sqlite", u.String()+"?"+q)
		if err != nil {
			return nil, nil, err
		}
		return db, func() { db.Close() }, nil
	}
	name, fsys, err := vfs.New(readOnly{os.DirFS(filepath.Dir(abs)), filepath.Base(abs)})
	if err != nil {
		return nil, nil, err
	}
	db, err = sql.Open("sqlite", "file:"+filepath.Base(abs)+"?vfs="+name+"&"+q)
	if err != nil {
		fsys.Close()
		return nil, nil, err
	}
	return db, func() { db.Close(); fsys.Close() }, nil
}

// readOnly is a directory as a probe reads a ledger in it through a file
// system of its own: the ledger, its log and what SQLite asks of beside
// it (a journal, which isn't there), nothing else.
type readOnly struct {
	dir  fs.FS
	name string
}

func (r readOnly) Open(name string) (fs.File, error) {
	if !strings.HasPrefix(name, r.name) || strings.ContainsRune(name[len(r.name):], '/') {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return r.dir.Open(name)
}

// ProbePostgres reads the Postgres ledger a connection string names, as
// serve would connect to it, on a session whose every transaction is
// read-only (SET SESSION CHARACTERISTICS, once it is connected, as serve's
// readers set theirs: a pooler in session mode refuses the setting among
// a connection's startup parameters), so that a write fails at the server,
// whatever asked it. It takes no lock. No error it gives holds the
// connection string.
func ProbePostgres(ctx context.Context, conn string) (Probed, error) {
	p := Probed{}
	n, err := sqliteFiles()
	if err != nil {
		return p, err
	}
	p.Knows = n
	cfg, warn, err := pgConfig(conn)
	if err != nil {
		return p, err
	}
	p.Warning = warn
	secrets := secretsOf(conn, cfg)
	ctx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout+connectWithin)
	defer cancel()
	c, err := probeConnect(ctx, cfg)
	if err != nil {
		return p, connectError(err, secrets, ctx.Err())
	}
	defer c.Close(context.WithoutCancel(ctx))
	var schema, held *string
	if err := c.QueryRow(ctx, `SELECT current_schema(), (SELECT n.nspname FROM pg_class t JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE t.oid = to_regclass('schema_version'))`).Scan(&schema, &held); err != nil {
		return p, connectError(err, secrets, ctx.Err())
	}
	if schema == nil {
		return p, fmt.Errorf("%w: no schema on the search_path exists to keep the ledger in", ErrConnect)
	}
	p.Schema = *schema
	if held != nil && *held != *schema {
		return p, fmt.Errorf("%w: the search_path finds the ledger in schema %q, not in its first, %q: name %q first", ErrConnect, *held, *schema, *held)
	}
	if err := c.QueryRow(ctx, "SELECT has_schema_privilege(current_schema(), 'CREATE')").Scan(&p.Create); err != nil {
		return p, connectError(err, secrets, ctx.Err())
	}
	if held != nil {
		p.There = true
		if err := c.QueryRow(ctx, "SELECT version FROM "+pgx.Identifier{*schema, "schema_version"}.Sanitize()).Scan(&p.Version); err != nil {
			return p, connectError(err, secrets, ctx.Err())
		}
	}
	if p.Holder, err = lockHeldBy(ctx, c, keyOf32(*schema)); err != nil {
		return p, connectError(err, secrets, ctx.Err())
	}
	return p, nil
}

// probeConnect connects as a probe does: as serve would, each transaction
// of the session read-only from its first statement on.
func probeConnect(ctx context.Context, cfg *pgx.ConnConfig) (*pgx.Conn, error) {
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if _, err := c.Exec(ctx, "SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY"); err != nil {
		c.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return c, nil
}

// Hosts are the hosts a connection string may connect to, which no output
// should say, as its password isn't; none for one that doesn't read.
func Hosts(conn string) []string {
	cfg, err := pgx.ParseConfig(strings.TrimSpace(conn))
	if err != nil {
		return nil
	}
	out := []string{cfg.Host}
	for _, f := range cfg.Fallbacks {
		out = append(out, f.Host)
	}
	return out
}

// Names are the user a connection string connects as and the database it
// connects to, which a server names when it refuses a connection ("no
// pg_hba.conf entry for host ..., user ..., database ..."): no output that
// hides the hosts should say them either. They are as pgx reads them, from
// the string, a service file, PGUSER and PGDATABASE, or the user this
// process runs as, in that order; a database not named is the user's.
// None for a string that doesn't read.
func Names(conn string) (user, database string) {
	cfg, err := pgx.ParseConfig(strings.TrimSpace(conn))
	if err != nil {
		return "", ""
	}
	return cfg.User, cfg.Database
}
