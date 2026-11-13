# The ledger in Postgres

Status: built, for the next release. `lapilli serve` keeps its ledger in
SQLite by default, as it always has; `--ledger-url-file FILE` keeps it in a
Postgres database instead. Still one replica: what more than one would take
is at the end. The companion to [serve.md](serve.md) (the ledger) and
[signin.md](signin.md) (who signed out, kept in the ledger).

## Why

A SQLite ledger is a file on one disk: in Kubernetes, a ReadWriteOnce
volume bound to one node, backed up and restored as the volume is, if at
all. A team that runs `serve` for long wants the ledger where it keeps the
rest of what matters: a managed Postgres, with its backups, point in time
recovery, failover, monitoring and access control, read with the SQL tools
it has. A database across the network also lets a pod move to
another node without its disk.

## Choosing it

```sh
lapilli serve --ledger-url-file /var/run/lapilli/ledger/url ...
```

- **The connection string is a secret.** It is read from a file, once, at
  start, as the other secrets are; it is in no flag, no pod spec, no log, no
  error, no page, no `/metrics`, no MCP answer and no report. A string pgx
  can't read is refused with a fixed sentence: pgx's own error repeats it,
  its password masked "best effort" (pgx's words). A connection that fails
  says the server's address and what the server answered (which can name
  the user or the database, as `password authentication failed for user`
  does), never more of the string; a password the server's answer holds
  is taken out. `--ledger` and `--ledger-url-file` are refused together.
- **As pgx reads it**: a `postgres://` URL or `key=value` pairs, the `PG*`
  environment variables and `~/.pgpass` among it, and the schema the ledger
  is in from `search_path`. The connections are named `lapilli` unless
  `application_name` says otherwise.
- **TLS.** pgx's default, `sslmode=prefer`, encrypts when the server offers
  TLS but checks no certificate, and falls back to none: across a network,
  ask for `sslmode=verify-full`, with `sslrootcert=FILE` for a CA of your
  own. `disable` is for a server on the same machine. lapilli changes none
  of pgx's defaults: a string means what it means to `psql`.
- **A database, or a schema, of its own.** The ledger makes its tables in
  the first schema of `search_path` (`public`) when it finds no
  `schema_version` there; a schema that holds tables a ledger has but no
  `schema_version` is refused, not written over. The role needs to create
  tables there (on 15 and later, `public` no longer lets everyone).
- **Postgres 15 or later**: the oldest major the PostgreSQL project still
  supports (14's last release was in November 2026). CI runs the ledger's,
  serve's and the command's tests on 15 and 18. Nothing newer than 15 is
  needed; nothing older is tested.
- **The chart**: `serve.ledger.postgres.secret` (name and key) mounts the
  Secret whole, so it can hold the server's CA beside the string, and makes
  no PersistentVolumeClaim. With the egress NetworkPolicy on,
  `serve.ledger.postgres.to` and `port` say where the database is.

## What is the same

Everything a page, the API, `/metrics`, MCP or a report gives, from the
same writes. The ledger's and serve's tests run on both: with
`LAPILLI_TEST_POSTGRES` naming a server each test gets a schema of its own
there, and CI runs them so on Postgres 15 and 18
(`.github/workflows/ci.yml`, `postgres`), beside `test/e2e/postgres.sh`.

- **The tables.** Every table, column, default, key and index SQLite's
  migrations make, with the same names, made at once by
  `schema/postgres/017.sql`: a Postgres ledger starts at schema 17.
  `TestPostgresSchemaSame` sets the two side by side.
- **The statements.** They are SQLite's, parameters and all (`?`, `?NNN`).
  A connection of pgx's numbers them for Postgres as SQLite numbers them,
  keeps a bool as 1 or 0, and leaves out arguments past a statement's last
  parameter, as SQLite does. Where the two databases differ, the statement
  is given twice, `dialect.pick(sqlite, postgres)`, or made by a method of
  the dialect (`dialect.go`): JSON (`json_each`, `json_extract`), the max
  of two, `INSERT OR IGNORE` and `UPDATE OR IGNORE`, `rowid`, `LIMIT -1`,
  `PRAGMA`: about forty places, of some 340 statements.
- **Comparing and sorting text.** Every text column is `COLLATE "C"`:
  compared and sorted byte by byte, as SQLite's are, whatever the
  database's own collation; `lower()` and `upper()` change ASCII letters
  alone, as SQLite's do, and `ILIKE` stands for SQLite's `LIKE` (which
  ignores the case of ASCII letters) in the one place a `LIKE` is kept, a
  grant's check. The text a JSON function gives is made `COLLATE "C"` too.
- **Numbers.** Every integer is a `BIGINT`, 64 bits as SQLite's are; a sum
  Postgres gives as `numeric` is read into the same Go integer.
- **The schema version**, kept in `schema_version` rather than `PRAGMA
  user_version`: a ledger written by a newer lapilli is refused
  (`ErrNewer`), one at an older schema migrated. Each later migration comes
  in a pair, SQLite's `schema/018.sql` and Postgres's
  `schema/postgres/018.sql`; `TestPostgresMigrationsKeepUp` fails on one
  alone, without a server.
- **The time zone**: opened in another zone, a ledger counts its days and
  weeks again, as SQLite's does.
- **One writer**, below.

So that a new statement isn't SQLite's alone, `TestDialectSeam` reads the
package's source: SQLite's own words (`json_each`, `PRAGMA`, `GLOB`,
`rowid`, `INSERT OR`, `instr(`, `ifnull(`, `sqlite_`, ...) outside a
pick's first argument or the dialect, a `max` or `min` of two, a subquery
in `FROM` without a name (which Postgres refuses before 16), and
`LastInsertId` (which pgx doesn't give) each fail it.

## What is not the same

- **Ids are never given twice.** Postgres's identity columns go on
  counting; SQLite gives the highest id again once its row is deleted, but
  for tasks and comments (AUTOINCREMENT). A write that fails leaves a gap
  in Postgres, none in SQLite. Nothing reads meaning into an id's value.
- **A NUL, or bytes that aren't UTF-8,** in a text: Postgres refuses both,
  SQLite keeps them. In Postgres each is kept as U+FFFD, which is how
  `encoding/json` writes bytes that aren't UTF-8 anyway. Nothing lapilli
  writes holds either; a title or a body someone sends can.
- **How big a key can be.** A Postgres index holds a value of some 2,700
  bytes at most, compressed. A label set has no such bound: `series` and
  `deploy_labels` are kept unique by the md5 of their labels beside the
  text (`series_labels`), and found through it. A name used as a key (a
  rule's, an incident's id, a group key's 1,000 characters at most) over
  that is refused; none comes near.
- **Space.** SQLite gives deleted pages back a few at a time after a prune
  (`PRAGMA incremental_vacuum`); Postgres's autovacuum keeps the space for
  the rows that come, and `VACUUM FULL` is the operator's.
- **Speed.** A statement waits a round trip to the server, which SQLite
  doesn't, so the writes that touch many rows send many a statement in
  Postgres (`batch.go`: a thousand rows each): the firings a run or a
  source's copy writes, their stones, the alerts new to the ledger, each
  alert's last firing before a span. The scale tests' budgets hold on both
  (the times are under "As built"). Across a network each round trip
  costs more: what still goes a row or a span at a time is a notification
  kept, a page's firings read a span at a time, a recorded rule found.
- **The binary** is some 3.9 MB larger for pgx (27.1 MB to 31.0 MB,
  stripped), whether a ledger is in Postgres or not.
- **What the database is told.** The writer's session asks the server to
  check its client every 30 seconds (`tcp_keepalives_*`), where it can, so
  a pod that died lets the lock go within a minute.
- **Through a pooler** in transaction mode (PgBouncer's), not at all: the
  lock is a session's, and so are prepared statements. In session mode, or
  directly.
- **Backups** are the database's (`pg_dump`, a managed service's point in
  time recovery), not the volume's.

## One writer, and why still one replica

`serve` writes as SQLite lets one process write: one connection, writes
taking turns in the process (`Ledger.mu`), each in a transaction. A write
reads what it wrote, and nothing else changes under it: no one else
writes. Reads go through four other connections, each transaction of reads
as of one moment (`REPEATABLE READ`, `READ ONLY`), as SQLite's WAL gives.

More than that is assumed by what the process keeps beside the ledger: the
ids of rules and series it has made, the generation pages are cached by,
which weeks changed when, the outbox it sends from, the hourly runs'
schedule, the rate limits. A second process writing the same ledger would
send every notice twice, run every hour twice, and serve pages from caches
the other made stale. With SQLite, the volume (ReadWriteOnce) and the
chart's `Recreate` keep it to one; with Postgres anyone who has the string
can start another.

So the ledger has a **lock**: an advisory lock, taken by the writer's
session on every connection it makes (`pg_try_advisory_lock`, keyed by
`lapl` and the schema's name, so two ledgers in two schemas of one
database each have one).

- **A second process is refused**, saying which session holds the lock,
  from where and since when: "the ledger is in use by another process:
  Postgres session 4242 ("lapilli", from 10.0.3.7, connected
  2026-11-13T06:00:00Z) holds its lock". `lapilli ledger copy` is refused
  the same way. **Builder's decision: refused, not waiting.** With the
  chart's `Recreate` the old pod has stopped, and let the lock go, before
  the new one starts; a pod that died lets it go within a minute, and
  Kubernetes starts the refused pod again, with backoff, until then. A
  process that waited would hide a second deployment, or an operator's
  `lapilli serve` run by hand, behind a pod that never becomes ready.
- **On shutdown** the lock is let go at once (`pg_advisory_unlock`), before
  the connections close, so the next pod doesn't wait for the server to
  notice.
- **A dropped connection** is made again by `database/sql`; the new session
  takes the lock again. Should the server still hold it for this process's
  old session, which it can for minutes after a network fault, that session
  is ended (`pg_terminate_backend`, its own role's) and the lock taken. Each
  process that takes the lock counts one more in `schema_version.writers`;
  a connection made again that finds another count finds that another
  process took the ledger in between, and wrote: this one writes nothing
  more (`ErrLockLost`), and `serve` stops, to be started again on what is
  in the ledger now. One that finds its own count reads its caches again,
  in case a failover lost what it last wrote.
- `signin.md`'s guarantees hold: who signed out is merged into `meta` in
  one transaction and read on each request; with one process, there is no
  other to sign out at.

### What more than one replica would add

What would let several processes serve one ledger, which is not in this:

- **A leader** for what must run once: the hourly runs, the digest and
  reports, the outbox, the trackers' and checks' jobs. The advisory lock
  becomes a lease the others wait on, and take over when its holder is
  gone.
- **The caches told of others' writes.** The generation pages are cached
  by, and which weeks changed, kept in the database (or sent with
  `LISTEN`/`NOTIFY`) rather than in the process.
- **Writes from any replica**: the UI's and the API's, each in its own
  transaction, as now, but with ids read back rather than cached, and the
  places that read-modify-write (`UpdateMeta`, the limits on windows and
  grants) at `SERIALIZABLE` or under row locks.
- **The outbox claimed** a notice at a time (`FOR UPDATE SKIP LOCKED`), so
  two senders never send one twice.
- **Rate limits and the busy guards** shared, or kept per replica and
  divided.

## Moving a ledger

```sh
lapilli ledger copy --ledger /var/lib/lapilli --to-url-file /var/run/lapilli/ledger/url
```

copies a SQLite ledger into an empty Postgres one, and `--ledger-url-file
FILE --to DIR` copies one back. With `serve` stopped: a Postgres ledger
`serve` holds is refused, and what a SQLite ledger is written after the
copy reads it is left out.

- **Into an empty ledger only**: new, or only opened (it holds the zone it
  was opened in). One that holds anything is refused, saying what.
- **In one transaction**, a table before those that refer to it, every row
  with its id; each table's next id comes after the highest it ever gave
  (SQLite's `sqlite_sequence`, Postgres's sequences), so a deleted task's
  id, which a tracker's issue may still name, isn't given again. A copy
  that fails leaves the ledger empty.
- **Read back**: every table, row count and SHA-256 of its rows read in the
  order of their keys, each value with its kind, from both ledgers. A copy
  that differs fails.
- The ledger copied from is brought to this lapilli's schema first, as
  `serve` would, and keeps the zone it was counted in.

## As built

`go test ./internal/ledger ./internal/serve -run Scale -v`, from a Go
container beside the server, as CI runs them (Linux, on a laptop's Docker,
the images CI pins), each figure one run's:

| | SQLite | Postgres 15 | Postgres 18 |
|---|---|---|---|
| a backfill, 201,600 firings, a day a write | 7.4 s | 9.5 s | 9.1 s |
| an hourly run | 12 ms | 86 ms | 57 ms |
| writing an incident | 8 ms | 29 ms | 10 ms |
| twelve weeks by rule | 3 ms | 25 ms | 23 ms |
| a week's report, without firings | 7 ms | 4 ms | 3 ms |
| judging everything again | 2.4 s | 2.0 s | 1.6 s |
| a source's week of 100,000 firings copied in | 2.1 s | 2.7 s | 3.2 s |
| the same again, unchanged | 0.35 s | 0.28 s | 0.44 s |
| a day's 10,000 firings beside 500 quiet windows | 0.43 s | 0.41 s | 0.39 s |
| 20,000 notifications kept, one a write | 2.4 s | 17.4 s | 12.2 s |
| their pages over twelve weeks | 0.36 s | 1.8 s | 1.6 s |
| the pages' scale test, whole (`TestPagesScale`) | 11.2 s | 16.0 s | 15.1 s |

Every budget holds on all three. A notification is a write of its own,
as an alert manager sends it, and its pages are read a span of firings at
a time: those are where Postgres's round trips show. From macOS to the
same server in Docker Desktop, a round trip of 420 µs rather than some 60,
a source's week of 100,000 firings takes 4.1 s and a day's 10,000 0.34 s;
before the writes were sent many rows a statement, 11 s and 8.9 s.

The ledger's, serve's and the command's tests pass on Postgres 15 and 18
as on SQLite (the tests of the lock and of copying back run on Postgres alone; those of
ledgers earlier lapillis wrote, which are SQLite files, on SQLite alone). Each guard was taken away in turn
(the connection string kept out of errors, the lock and what it holds,
the copy's refusals and its reading back, the migrations in pairs, the
schema compared, each choice of the dialect, the chart's): 53, each failing
a test.
