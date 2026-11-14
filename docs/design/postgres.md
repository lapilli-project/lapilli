# The ledger in Postgres

Status: built, for v0.34.0. `lapilli serve` keeps its ledger in
SQLite by default, as it always has; `--ledger-url-file FILE` keeps it in a
Postgres database instead. Since the release after 0.35.0, several replicas
of serve share it, one leading ([replicas.md](replicas.md)); 0.34 and 0.35
were one replica, as below. The companion to [serve.md](serve.md) (the ledger) and
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
- **TLS.** pgx's default, `sslmode=prefer`, encrypts when the server
  offers TLS but checks no certificate, and falls back to none: anything
  in the path can decline TLS and be sent the password in clear (a fake
  server in `TestPostgresTLSPasswordKept` asks, and gets none). The string
  is read once, by pgx, and never written again; what it asks for is read
  from what pgx made of it, to every host it may try. **Builder's
  decision: to a server not on this machine, pgx's `prefer` is refused**,
  saying to ask for `sslmode=verify-full` (`sslrootcert=FILE` for the
  server's CA), or to say `require` or `disable` to go on unchecked. No
  `sslmode` and `sslmode=prefer` are one to pgx, and telling them apart
  would mean reading the string a second time, otherwise than pgx may
  (Go's `net/url` and pgx's parser disagree on `+`, on a `#`, on a
  repeated key, on a `?` in a password), and guessing; asking for
  `verify-full` on the string's behalf meant writing it again, which
  changed what it said. A certificate checked (`verify-ca`,
  `verify-full`) is taken; `require`, `allow` or `disable`, from the
  string, a service file or `PGSSLMODE`, is taken at its word, and `serve`
  and `lapilli ledger copy` warn at start. On this machine (a Unix
  socket, `localhost`, a loopback address), as given. Asking for
  `require_auth=scram-sha-256` instead would keep the password but let
  anything in the path read the ledger.
- **A database, or a schema, of its own.** The ledger makes its tables in
  the first schema of `search_path` (`public`) when it finds no
  `schema_version` there; a schema that holds tables a ledger has but no
  `schema_version` is refused, not written over. The role needs to create
  tables there (on 15 and later, `public` no longer lets everyone). Each
  connection's `search_path` is then that schema alone, so a statement
  reads no table of the same name further along it. A ledger found
  further along the path than its first schema is refused, saying to name
  its schema first: the lock is the first schema's, and would let a second
  process write the ledger another holds.
- **Who can connect.** The lock is an advisory lock, which any role that
  can connect to the database can take, alone: one that does keeps
  `serve`'s replicas from starting, refused "in use by another process",
  which names the role that holds it. Give the database to the ledger's role alone: `REVOKE
  CONNECT ON DATABASE lapilli FROM PUBLIC; GRANT CONNECT ON DATABASE
  lapilli TO lapilli;`.
- **Timeouts.** **Builder's decision: lapilli sets no `statement_timeout`
  or `lock_timeout`** for one process; serve's replicas set `lock_timeout`
  (30 s) on their writers' sessions, for the writers' turn
  ([replicas.md](replicas.md)). A page's statements end with its
  request, which pgx cancels on the server; the writer's are the ledger's
  own, and a backfill's write of a day's firings takes seconds on a busy
  ledger: a timeout short enough to matter would fail it, and the next
  hour's run would fail it again. A string can still set them (`options=-c
  lock_timeout=10s`), as can the role or the database.
- **A server that stops answering** holds nothing up for long. Behind a
  proxy that keeps the connection while what is past it is gone (an auth
  proxy, a pooler, a load balancer), or on a server that stopped, TCP is
  well and a statement would wait forever; on a firewall or a NAT that
  drops what is sent, as long as TCP retries, some 15 minutes on Linux,
  holding up every write. Keepalives see neither. So the ledger's own
  deadlines: a connection is made, its lock taken, within 30 s (on top of
  the string's `connect_timeout`); one taken from a pool after a second
  answers pgx's ping within 10 s, or is given up and another made; a
  write's transaction, BEGIN to COMMIT, takes 5 minutes at most, some
  hundred times the largest serve makes (a copy, which writes a whole
  ledger in one, has none). CI hung so once, ten minutes, behind the test's
  own proxy (`TestPostgresSilentConnection` holds it to these now).
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

- **The tables.** Every table, column, default, key, foreign key (and what
  a delete does to it), CHECK and index SQLite's migrations make, with the
  same names, expressions and WHEREs, made at once by
  `schema/postgres/017.sql`: a Postgres ledger starts at schema 17.
  `TestPostgresSchemaSame` sets the two side by side; where they differ
  on purpose (keys longer than a btree holds, below), its `byDesign` says
  both.
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
  database's own collation; `lower()` and `upper()` of a column change
  ASCII letters alone, as SQLite's do, and `ILIKE` stands for SQLite's
  `LIKE` (which ignores the case of ASCII letters) in the one place a
  `LIKE` is kept, a grant's check. The text a JSON function gives is made
  `COLLATE "C"` too. A parameter has no collation of its own, and
  `lower(?)` would fold it as the database's does (an `É` too): what is
  compared with a column folded is folded by lapilli, in ASCII
  (`ASCIILower`), as an Azure alarm's id is.
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
  `encoding/json` writes bytes that aren't UTF-8 anyway. In a text
  Postgres reads as `jsonb` (a label set of `series` and `notified`, a
  change's before and after, a version's names; a list given as a
  parameter, `CAST(? AS jsonb)`), the escape `\u0000`, which `jsonb`
  refuses, is written U+FFFD too (`pgJSON`), wherever it is written from:
  a run's labels, a source's, a notification's, a copy. Any other text
  keeps it as written, as SQLite does: a task's body that is a JSON line,
  an incident's labels, which are then the same the next time they come.
  A label holding a NUL, which every intake takes, is the same alert the
  next time it comes, as in SQLite; two label sets that differ only by a
  NUL and a U+FFFD are one alert in Postgres, two in SQLite.
- **How big a key can be.** A Postgres btree holds some 2,700 bytes of a
  key; SQLite's hold any length, and a rule's name, a group key (1,000
  characters, up to 4,000 bytes), a severity, an alarm's id in a label
  (4 KB) or an incident's id in a file can be longer. What must be unique
  is kept unique by its md5 and found through a hash index, which holds
  any length (`rules.name`, `notes.rule`, `incidents.key`, a check's rule,
  a recorded rule's name and severity, a label set); an index on such a
  text alone is a hash index (`tasks_rule`, `comments_on`,
  `check_misses_rule`, `proposals_rule`); one with it among other keys
  holds its md5 (`notifications_group`), or, where it is searched by a
  range too, its first 500 characters (`series_alarm`, `notified_alarm`),
  and its statements ask for that as well. Every other text in a btree is
  bounded well below where it is taken in (017.sql lists them).
  `TestLongKeys` writes each, on both.
- **Space.** SQLite gives deleted pages back a few at a time after a prune
  (`PRAGMA incremental_vacuum`); Postgres's autovacuum keeps the space for
  the rows that come. `VACUUM FULL` rewrites a table under an exclusive
  lock: every read and write of it waits, `serve`'s too, for as long as it
  takes. Run it with `serve` stopped, or not at all.
- **Ties in order.** A page that breaks a tie by an alert's id has the
  same alerts first in both: a run's new alerts are made in the order
  SQLite makes them (`TestSeriesMadeInSQLitesOrder`). A notification's
  alerts are read back in the order they were kept, in either; neither
  database is asked for that order, which Postgres keeps only while
  `notified` is written to its end (a `VACUUM FULL` can change it).
- **Speed.** A statement waits a round trip to the server, which SQLite
  doesn't, so the writes that touch many rows send many a statement in
  Postgres (`batch.go`: a thousand rows each): the firings a run or a
  source's copy writes, their stones, the alerts new to the ledger, each
  alert's last firing before a span. The scale tests' budgets hold on both
  (the times are under "As built"). Across a network each round trip
  costs more: what still goes a row at a time is a notification kept and
  a recorded rule found. A page's firings, read a span about each alert at
  a time in SQLite, are read 2,000 spans a statement in Postgres, each
  searched for by `firings_rule` (`LATERAL`), their series' labels after by
  id: twelve weeks of 20,000 notifications took 6.6 s on CI's Postgres 15,
  some 13,000 round trips, and take some 15 statements now. A
  week of teams' pages reads each rule's firings by `firings_rule`
  (`LATERAL`): Postgres, its statistics taken early in a backfill, rated
  reading the whole week by `firings_start` cheaper, and took 3.4 s on CI's
  Postgres 18, 1.5 s on two CPUs, for what SQLite reads in 0.15 s; it is
  0.15 s in both now.
- **The binary** is some 3.9 MB larger for pgx (27.1 MB to 31.0 MB,
  stripped), whether a ledger is in Postgres or not.
- **What the database is told.** The writer's session asks the server to
  check its client every 30 seconds (`tcp_keepalives_*`), where it can, so
  a pod that died lets the lock go within a minute.
- **Through a pooler** in transaction mode (PgBouncer's), not at all: the
  lock is a session's, and so are prepared statements and the writers'
  turn's lock ([replicas.md](replicas.md)). In session mode, or directly.
- **Backups** are the database's (`pg_dump`, a managed service's point in
  time recovery), not the volume's.

## One writer, and then replicas

0.34 and 0.35 wrote a Postgres ledger as SQLite lets one process write one: one
connection, writes taking turns in the process (`Ledger.mu`), each in a
transaction, and an advisory lock that refused a second process. Reads go
through four other connections, each transaction of reads as of one moment
(`REPEATABLE READ`, `READ ONLY`), as SQLite's WAL gives; that is as it was.

Since, `serve`'s replicas share the ledger, one leading
([replicas.md](replicas.md)), and what stays of the one writer is:

- **The lock**, an advisory lock keyed by `lapl` and the schema's name
  (so two ledgers in two schemas of one database each have one), now
  taken **shared** by every session of every replica, and **alone** by a
  process that must be the ledger's only writer: `lapilli ledger copy`,
  and serve of 0.35 or before. The two refuse each other, saying which
  session holds the lock, from where and since when: "the ledger is in use
  by another process: Postgres session 4242 (role "lapilli", from
  10.0.3.7, connected 2026-11-13T06:00:00Z) holds its lock ...".
  **Builder's decision: refused, not waiting**, as before.
- **On shutdown** the lock is let go at once, on each of the replica's
  sessions (`pg_advisory_unlock_shared`, `pg_advisory_unlock_all`), before
  they close, so a copy doesn't wait for the server to notice.
- **A dropped connection** is made again by `database/sql`; the new session
  takes the lock again, and ends the writer's own earlier sessions the
  server still keeps (`pg_terminate_backend`, its own role's), known by
  their pid and when they began, as a pid is given again: one kept in a
  transaction would hold the writers' turn. A process alone that finds its
  own earlier session holding the lock ends it so too, as before. Each
  process that takes the lock alone counts one more in
  `schema_version.writers`; replicas don't. A connection made again that
  finds another count finds that a process wrote the ledger alone in
  between: this one writes nothing more (`ErrLockLost`), and `serve`
  stops, to be started again on what is in the ledger now. One that finds
  its own count reads its caches again, in case a failover lost what it
  last wrote.
- **What goes out of the process** is sent only while it holds the lock
  and, for what the leader sends, leads (`Ledger.Hold`): a notice, claimed
  first; the week's digest, claimed too. A rule's or an alarm's change is
  written through the writer, under the lease, before it is applied
  (`Move`); a subscription is confirmed as an incoming request asks, which
  twice does no harm. `TestSendersHoldTheLedger` reads serve's code for
  every way out of the process, and fails on one it doesn't know.
- `signin.md`'s guarantees hold across replicas: who signed out is merged
  into `meta` in one transaction, under the writers' turn, and read on
  each request.

### What more than one replica added

Built ([replicas.md](replicas.md)): a leader for what must run once, on a
lease the others take when its holder is gone; the caches told of the
others' writes through the database; writes from any replica, each taking
the writers' turn; the outbox claimed a notice at a time (`FOR UPDATE SKIP
LOCKED`); the rate limits that guard the ledger shared, the busy guards
each replica's.

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
  order of their keys, each value with its kind, from both ledgers, before
  the copy commits. A copy that differs fails, and leaves the ledger empty.
- **As of one moment**: the ledger copied from is read in one transaction,
  its ids given, its rows and their read back alike: a SQLite ledger a
  serve left running writes has its later writes left out, and the copy
  gives next the ids they took.
- The ledger copied from is brought to this lapilli's schema first, as
  `serve` would, and keeps the zone it was counted in.

## As built

`go test ./internal/ledger ./internal/serve -run Scale -v`, from a Go
container beside the server, as CI runs them (Linux, on a laptop's Docker,
the images CI pins), each figure one run's:

| | SQLite | Postgres 15 | Postgres 18 |
|---|---|---|---|
| a backfill, 201,600 firings, a day a write | 8.2 s | 7.2 s | 7.5 s |
| an hourly run | 13 ms | 18 ms | 14 ms |
| writing an incident | 8 ms | 29 ms | 26 ms |
| twelve weeks by rule | 3 ms | 26 ms | 22 ms |
| a week's report, without firings | 2 ms | 7 ms | 4 ms |
| judging everything again | 2.5 s | 1.8 s | 1.8 s |
| a source's week of 100,000 firings copied in | 2.2 s | 3.1 s | 3.1 s |
| the same again, unchanged | 0.36 s | 0.47 s | 0.39 s |
| a day's 10,000 firings beside 500 quiet windows | 0.44 s | 0.45 s | 0.42 s |
| 20,000 notifications kept, a thousand a write | 1.1 s | 10.5 s | 8.8 s |
| their pages over twelve weeks, a span a statement (since read 2,000 a statement in Postgres) | 0.37 s | 2.0 s | 1.6 s |
| fifty teams' hours of a week, afresh | 0.16 s | 0.22 s | 0.20 s |
| the pages' scale test, whole (`TestPagesScale`) | 12.3 s | 18.7 s | 19.2 s |

Every budget holds on all three, on two CPUs (`--cpus=2`, as a GitHub
runner has). Read 2,000 spans a statement, those pages took 83 to 100 ms on
Postgres 17 on the laptop itself, against 278 to 284 ms in SQLite there, and CI's
Postgres 15 and 18 hold their budget. A notification an alert manager sends is a write of its own:
20,000 so took 2.4 s, 17.4 s and 12.2 s; the scale test keeps them a
thousand a write, as a source's are copied in, since what it holds to a
budget (3 s, 6 s on CI) is reading them. Their pages are read a span of
firings at a time: that, and a notification kept, are where Postgres's
round trips show. From macOS to the
same server in Docker Desktop, a round trip of 420 µs rather than some 60,
a source's week of 100,000 firings takes 4.1 s and a day's 10,000 0.34 s;
before the writes were sent many rows a statement, 11 s and 8.9 s.

The ledger's, serve's and the command's tests pass on Postgres 15 and 18
as on SQLite (the tests of the lock and of copying back run on Postgres alone; those of
ledgers earlier lapillis wrote, which are SQLite files, on SQLite alone). Each guard was taken away in turn
(the connection string kept out of errors, the lock and what it holds,
the copy's refusals and its reading back, the migrations in pairs, the
schema compared, each choice of the dialect, the chart's): 53, each failing
a test. So was each fix of the reviews (a NUL in JSON, the `search_path`,
sending only while the lock is held, `sslmode`'s default, each key longer
than a btree holds and each statement that searches one, the ASCII fold,
the holder's role, a timeout said, this process's own sessions, the copy's
read back, a run's alerts in SQLite's order, the schema's foreign keys,
CHECKs, expressions and WHEREs): 41, each failing a test. Without the
hours read rule by rule, `TestPagesScale` failed each of four runs, two
on each major, on two CPUs; with it, it passed each of eight.
