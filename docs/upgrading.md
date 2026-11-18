# Upgrading

`lapilli verdict` keeps no state: each run reads the rules and the history
afresh, and writes only where it is told to (its output, a Pushgateway
group that each push replaces, Slack). Upgrading it is running the new
binary or image; the changelog says which flags and outputs changed.

`lapilli serve` keeps a ledger, and a release may change the ledger's
schema. It upgrades the ledger as it starts, and a release from before the
change can't open it after.

## What happens as serve starts

The schema is a list of numbered files,
[internal/ledger/schema](../internal/ledger/schema), each applied once, in
order; a ledger's schema is the number of the last one applied. serve
applies the ones the ledger hasn't had as it opens it, before it serves a
page or runs anything.

- **SQLite** (`--ledger DIR`, the file `DIR/ledger.db`): the number is
  SQLite's `PRAGMA user_version`. Each file is applied in a transaction of
  its own, which sets `user_version` to its number, so a file that fails
  leaves the ledger at the schema before it, and serve stops, naming the
  file.
- **Postgres** (`--ledger-url-file FILE`): the number is in a table,
  `schema_version`. An empty database is given the schema up to 017 at once
  (a Postgres ledger, from 0.34.0, starts there), then each later file in
  turn, each in a transaction that locks `schema_version` (an empty
  database, an advisory lock) and, where replicas run, holds the writers'
  turn: replicas started at once migrate the ledger once, and a write of
  another replica waits for the migration and finds it. A step that waits
  more than 2 s for a lock on a table (a `pg_dump` holds one on each)
  gives up and is tried again every 10 s, for 5 min; then the start fails.
  A database whose schema holds a ledger's tables but no
  `schema_version` is refused, not written over. The replicas' own tables
  (`replica_*`) have a version of their own, and are changed only where
  they are older ([design/replicas.md](design/replicas.md)).

A ledger at a newer schema than the release knows is not touched. serve
stops as it starts, exits 1, and says, for a SQLite ledger:

```
lapilli serve: --ledger: the ledger was written by a newer lapilli (schema 22; this one knows 21)
```

and on Postgres the same after `--ledger-url-file:`. In a pod, it restarts
over and over. On a Postgres ledger that replicas share, a newer release
rolled onto older replicas migrates as its first pod starts; each older
replica finds the newer schema at its next read or write, writes nothing,
and stops (`the ledger was written by a newer lapilli (schema 22; this one
knows 21): stopping`), and the rollout replaces it
([Several replicas](serve.md#several-replicas)).

`lapilli ledger copy` brings the ledger it copies from to its own schema
first, as serve would. `lapilli preflight`, given serve's flags where serve
runs, opens the ledger read-only and says, from 0.53 on, which of these it
is ([Before it is deployed](serve.md#before-it-is-deployed)):

```
ok       ledger.sqlite       schema 022, this release's
ok       ledger.sqlite       schema 021: serve upgrades the ledger from 021 to 022 as it starts, and
                             a release before 022 can't open it after: keep a copy first
problem  ledger.sqlite       schema 023, newer than this release's 022
```

On Postgres the item is `ledger.postgres`.

## The ledger's schemas

Each schema, the first release that has it, and what it adds. A Postgres
ledger is given 001 to 017 from one file of its own, then each later one in
turn.

| Schema | First release | What it adds |
|---|---|---|
| 001 | 0.6.0 | The ledger: rules and their versions, alerts, firings and their stones, incidents and what caught them, each rule's tallies by day and by week, runs, settings, the changes made in the UI and the API, and results worked out |
| 002 | 0.6.0 | An index of the alerts still firing |
| 003 | 0.9.0 | Teams, notes on rules, quiet windows (and the firings they excuse), tasks and comments |
| 004 | 0.10.0 | Quiet windows that repeat; a task's link to a tracker, its overdue notice and when it was done; the outbox of notices |
| 005 | 0.12.0 | An index of each rule's days, read when a change to a rule is measured |
| 006 | 0.17.0 | A team's own working hours, hours asleep and time zone |
| 007 | 0.18.0 | A team's shifts |
| 008 | 0.20.0 | Firings recorded elsewhere: a rule's source and the name it was sent under (`rules.source`, `rules.sent`), and `sources` |
| 009 | 0.20.0 | Notifications as they were sent, and the alerts each told of |
| 010 | 0.21.0 | The replay beside what really fired, by rule and day |
| 011 | 0.21.0 | The source a notification's alerts were copied in as, the kind of message it came as, and indexes of a cloud alarm's `alarm_id` |
| 012 | 0.22.0 | Reports people schedule |
| 013 | 0.22.0 | Proposals and their approvals, and an index of the changes by what they were made to, for who set a task's status |
| 014 | 0.25.0 | Roles given in the UI (`grants`); a quiet window's team, and repeats by month or by a cron expression |
| 015 | 0.27.0 | On-call turns; `comments` made again with `origin`, `ref` and `author`, and ids never used again |
| 016 | 0.28.1 | `tasks` made again, with ids never used again |
| 017 | 0.31.0 | Deploys and their label sets, a mark on each firing that came during one, counted by day and week, and an index of re-fired firings |
| 018 | 0.41.0 | An index of when incidents ended |
| 019 | 0.44.0 | `declared` and `given_back` on teams, quiet windows, reports and roles, for `--config`; a team's name unique in any letter case |
| 020 | 0.53.0 | Goals, and the goal a task is about |
| 021 | 0.53.0 | Marked windows: their kinds, the windows, the kinds each firing is marked with, and tallies of them by day and week |
| 022 | 0.55.0 | Named impact signals and the rule entries in effect; an incident's signal and match labels; the signals each alert is tied to; and the tallies by day and week keyed by them too, every row copied in once |
| 023 | 0.61.0 | Pull request backtests (`--backtest-listen`): a row each backtest asked for, kept a day after it ends, and the tokens that started one, kept past their hour |

The image has no shell and no `sqlite3`, so the schema a ledger is at is
read where its data can be reached: `sqlite3 COPY/ledger.db 'PRAGMA
user_version'` on a copy of a SQLite ledger's files, or `SELECT version
FROM schema_version` in its Postgres database.

## Before an upgrade that changes the schema

- Read the changelog's entries for each release between the two. From
  0.12.0 on, the entry of a release that changes the schema says so, as
  0.55.0's does: **Ledger schema 022**, after 021.
- Take a copy of the ledger, and upgrade once it is taken: a snapshot of
  the disk behind the PersistentVolumeClaim, the files copied out with
  serve stopped, or a backup of the Postgres database. The chart's README
  says how for each, under [Going back after a schema
  upgrade](../charts/lapilli/README.md#going-back-after-a-schema-upgrade).
  Not with the new release's `lapilli ledger copy`: it brings the ledger to
  its own schema first.
- Run the new release's `lapilli preflight`, with serve's flags, against
  the ledger: for SQLite, the new binary on a machine that holds the
  copied files, with `--ledger` naming their directory; for Postgres, with
  `--ledger-url-file`, from where the database can be reached (it connects
  read-only). Its `ledger.sqlite` or `ledger.postgres` line says whether
  serve will upgrade the ledger; off the cluster, the items that need what
  only the cluster has (the history, the tokens' files) may say problem or
  unknown. The running pod's `lapilli preflight` is the old release's, and
  one run in a pod that mounts the claim read-only says the directory isn't
  writable, and reads no schema.

## Going back after a schema upgrade

A release from before the change refuses the upgraded ledger, so going back
is going back to the ledger as it was: the copy taken before the upgrade,
with the release before. Not with `helm rollback`; the steps are in [the
chart's README](../charts/lapilli/README.md#going-back-after-a-schema-upgrade).
What came in after the copy that can't be replayed isn't in it.

## The Helm chart

A release's chart has the release's version without the `v` (`0.59.0` for
`v0.59.0`), and the release as its `appVersion`; it runs the image that
release built, by its digest
(`ghcr.io/lapilli-project/lapilli:v0.59.0@sha256:...`). Upgrade by the
chart's version:

```sh
helm upgrade lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring --version 0.59.0 -f values.yaml
```

`image.tag` set to another version needs that version's `image.digest`
too, or `image.digest` empty: the chart refuses its own image's digest
under another tag. With one replica the Deployment is `Recreate`, so the
pod before stops before the new one starts; with `serve.replicas` above 1
(Postgres only), it rolls one pod at a time. From 0.35 or before, upgrade
with `serve.replicas: 1` first, then raise it
([Several replicas](../charts/lapilli/README.md#several-replicas)). In
CronJob mode the chart runs `lapilli verdict`, which keeps nothing to
upgrade.

## Release notes

Each release has its section in [CHANGELOG.md](../CHANGELOG.md), and its
[GitHub release](https://github.com/lapilli-project/lapilli/releases) has
that section as its notes. While the version is 0.x, a minor release may
change flags or the API, and its entry says how
([GOVERNANCE.md](../GOVERNANCE.md#releases)); what keeps its name and its
meaning across releases, and how a change to it is made, is in
[What stays the same](numbers.md#what-stays-the-same). Only the latest
release gets fixes ([SECURITY.md](../SECURITY.md#versions)).

Each release's archives, `SHA256SUMS`, chart and image are attested by the
workflow that built them, with Sigstore, and from v0.28.0 the attestations
are also an asset of the release, `lapilli-vX.Y.Z.intoto.jsonl`.
[SECURITY.md](../SECURITY.md#checking-a-release) says how to check them,
offline too.
