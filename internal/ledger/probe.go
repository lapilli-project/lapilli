package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"
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

// Probe reads the SQLite ledger serve keeps in dir (DIR/ledger.db): opened
// read-only (mode=ro), its connection taking no write (PRAGMA query_only);
// and, with no write-ahead log beside it, as immutable, so that SQLite
// makes nothing beside it either. A ledger that isn't there is a new one:
// There false, and nothing made.
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
	u := fileURI(abs)
	q := "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(10000)"
	if _, err := os.Stat(abs + "-wal"); errors.Is(err, os.ErrNotExist) {
		// Nothing writes it now, or it would have a log: all of it is in
		// the file.
		q += "&immutable=1"
	}
	db, err := sql.Open("sqlite", u.String()+"?"+q)
	if err != nil {
		return p, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&p.Version); err != nil {
		return p, err
	}
	return p, nil
}

// ProbePostgres reads the Postgres ledger a connection string names, as
// serve would connect to it, on a session whose every transaction is
// read-only (default_transaction_read_only, set as it connects), so that
// a write fails at the server, whatever asked it. It takes no lock. No
// error it gives holds the connection string.
func ProbePostgres(ctx context.Context, conn string) (Probed, error) {
	p := Probed{}
	n, err := sqliteFiles()
	if err != nil {
		return p, err
	}
	p.Knows = n
	cfg, warn, err := probeConfig(conn)
	if err != nil {
		return p, err
	}
	p.Warning = warn
	secrets := secretsOf(conn, cfg)
	ctx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout+connectWithin)
	defer cancel()
	c, err := pgx.ConnectConfig(ctx, cfg)
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

// probeConfig is how a probe connects: as serve would, each transaction of
// the session read-only, set as it connects.
func probeConfig(conn string) (*pgx.ConnConfig, string, error) {
	cfg, warn, err := pgConfig(conn)
	if err != nil {
		return nil, "", err
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	return cfg, warn, nil
}
