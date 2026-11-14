# Replicas on one Postgres ledger

Status: built, for the release after 0.34.1. `lapilli serve` with its
ledger in Postgres (`--ledger-url-file`, [postgres.md](postgres.md)) runs
as several replicas on one ledger: one leads, and runs what must run once;
every replica serves the rest. With SQLite it is one process, as it always
was. The second step of [postgres.md](postgres.md)'s "What more than one
replica would add", built.

## Why

One pod is one point of failure: a node drained, an upgrade, an OOM kill
or a crash takes the UI, the API, MCP, the sign-in's callback and the ports
that take alerts and incidents away until a new pod is up, and an alert
manager's notifications sent meanwhile are retried or lost. With the
ledger in Postgres nothing is a pod's own: several pods can serve it, and
one can take over the work of another that went.

## Choosing it

Nothing to choose in serve: a Postgres ledger's `serve` is a replica,
alone or with others. In the chart:

```sh
helm upgrade lapilli ... --set serve.ledger.postgres.secret.name=lapilli-ledger --set serve.replicas=3
```

- **More than one replica needs Postgres.** The chart refuses
  `serve.replicas` above 1 without `serve.ledger.postgres.secret`: "3
  replicas share one ledger only in Postgres (serve.ledger.postgres.secret):
  a SQLite ledger is one pod's file, on a volume one node mounts". With
  SQLite nothing changed: one pod, `Recreate`, the same files.
- **They roll one at a time**: `RollingUpdate`, `maxSurge: 1`,
  `maxUnavailable: 0`, so a new pod is ready (as a follower) before an
  old one goes; a `PodDisruptionBudget` lets `serve.podDisruptionBudget.maxUnavailable`
  (1) go at once. One replica stays `Recreate`, as before.
- **From 0.34 or before, upgrade with one replica first.** A 0.34 process
  holds the ledger's lock alone (below): a new pod is refused until it has
  stopped, which `Recreate` does first and `RollingUpdate` never would.
  Then raise `serve.replicas`.
- **Every replica runs with the same flags**, as a Deployment's pods do.
  Nothing checks it: during a rolling update that changes them, either
  may lead.

## The lock, shared

The advisory lock of [postgres.md](postgres.md) is kept, now taken
**shared** by every session of every replica (`pg_try_advisory_lock_shared`),
and **alone** by `lapilli ledger copy` (`OpenPostgresAlone`), as 0.34 and
before took it. The two refuse each other: a copy is refused while any
replica runs ("holds its lock, and a ledger serve's replicas share is
written by no other process"), and a replica while a copy, or a 0.34
`serve`, holds it ("holds its lock alone, as a copy, or a lapilli of 0.34
or before, does"). Replicas don't count themselves in `schema_version.writers`:
a replica that connects again and finds the count moved finds that a
process wrote the ledger alone in between, and stops (`ErrLockLost`), as a
process did before. That is the one case a replica exits for the lock;
another replica is no reason.

## The lease

Who leads is a row, `replica_lease`: its holder (a replica's id, its host
and a random part), a **term**, and until when, **by the database's
clock**:

- **Taken** by a follower, every `campaignEvery` (2 s), when it has
  lapsed: `INSERT ... ON CONFLICT DO UPDATE ... WHERE until <
  clock_timestamp()`, the term one more. Two followers asking at once get
  one answer: the row's.
- **Renewed** by the leader every `renewEvery` (5 s), for `leaseFor` (15
  s), only while it is still its own and not lapsed; a renewal that fails
  is tried again each second.
- **Given up** as soon as the leader stops leading, whatever stopped it
  (serve shut down, a lease it couldn't renew), once its work has
  returned: `until` is set in the past, and a follower takes it at its
  next campaign. A renewal answered after the leader stopped would
  otherwise keep it from the others for its length.
- On its own pool (`lease`, one connection), so that no write, which can
  take seconds, holds a renewal up; and each renewal is asked apart from
  the clock that ends the lease, so that one waiting on the database
  doesn't hold the end up.

**The leader stops by its own clock, before the database lets another
in.** It counts each lease from just before it asked for the renewal, and
stops leading `leaseMargin` (2 s) before that lease ends: the database's
lease ends later than that, however long the round trip took. Clocks are
not compared, only how fast each runs: a drift of a part in ten thousand
moves 15 s by 1.5 ms.

**Takeover**: within `leaseFor` and a campaign of the leader's last
renewal, killed or cut off: 10 to 17 s after its end, as it renewed 0 to 5
s before it. Given up (a rolling update, `SIGTERM`): within a campaign, 2
s at most. Measured, under "As built".

### Fencing

A leader that loses the lease stops at once, and what it would still do is
refused:

- **Its work stops.** What it runs as the leader runs under a context the
  lease ends, cancelled the moment the lease would end by its clock, or at
  once when a renewal finds the lease another's. Each loop returns; each
  statement in flight is cancelled; serve waits for them all before it
  campaigns again.
- **Each job looks first** (`still`): the worker looks at the lease before
  each of its jobs, so one whose lease ended starts none.
- **Each write it makes is checked in the database**, in its transaction:
  the lease must still be this replica's, at its term, not lapsed; and,
  before it commits, by its own clock. A leader cut off, or frozen past
  its lease (a stopped process, a frozen VM), whose renewals another's
  term overtook, writes nothing. What a follower, or a person on the
  leader's pages, writes is no leader's work and isn't checked.
- **Everything sent out of the process is sent after `Hold`**, as before,
  which now also says whether this replica still leads; a notice is sent
  only once it is **claimed in the database under the lease** (below), and
  the digest only once it is claimed so too.
- **What remains**: a process frozen between its last check and the send,
  longer than the rest of its lease (seconds), can still send that one
  thing once it wakes; and a leader that sent and was killed or cut off
  before it noted it sent (one round trip) leaves it to the next leader,
  which sends it again once its claim is over. A process restarted there
  did the same. A webhook's notice carries its id (`lapilli-N`, and
  `Idempotency-Key`) to tell the two apart.

## What runs where

| | the leader | every replica |
|---|---|---|
| the runs and the backfill, judging again, the incidents and deploys files, checking the replay, near misses, proposals, pruning | yes | |
| the weekly digest and the reports, worked out and sent | yes | |
| the outbox: notices, team digests, reports | yes | |
| changes carried out: Grafana, a ruler, a cloud's alarms | yes | |
| the rules watched, and their versions recorded; the rules' history added | yes | |
| the on-call feeds read, and their turns kept | yes | |
| pages, the API, MCP, `/metrics`, `/readyz` | | yes |
| sign-in, its callback, sign-out | | yes |
| writes from the UI and the API, tokens' writes (incidents, firings, tasks, changes taken by a recipe) | | yes |
| the notifications port (alert managers, Grafana, SNS, Azure, Google, OCI) | | yes |
| the rules read for what they say (content findings, Grafana's data sources, a Loki's places), without a version | | yes, as the leader reads them |
| the on-call rotas set from the turns kept | | yes |

**The intake ports run on every replica.** Each write they make is a
write of the ledger's, in its own transaction, taking the writers' turn
(below): an alert manager's notification kept on one replica is the same
as on another, and one sent twice to two replicas is kept once
(`NotifyAll` finds it kept). A tracker's or a firings job sends to the
Service, and any replica takes it. Nothing they do must run once.

A change made on a follower that the leader's work should see at once (a
quiet window, a team, an approval, settings that decide the stones) wakes
the leader through the ledger (`replica_gens.wake`, read every second);
settings changed ask it to judge every firing again (`rejudge`, and
`rejudged` kept once it has). An admin's pause of changes is read from the
ledger by the leader before each change it carries out.

## The writers' turn

Every write, on every replica, takes one lock first, in its transaction
(`pg_advisory_xact_lock`, keyed `lapw` and the schema's): writes take turns
across the replicas as they did in one process (`Ledger.mu`), and a write
that reads what it then changes reads what no other write is changing.
**Builder's decision: one turn for every write, not `SERIALIZABLE` at each
place that reads then writes.** The places are many and not all plain:
`UpdateMeta`, a team's quiet windows and the ledger's, roles, proposals in
an hour and one at a time for a rule, approvals, a proposal moved on, a
notice's attempts, `TellOnce`, a recorded rule named apart, ids made where
none was. Each was written for one writer, and is as right with the turn
as it was, without finding each; a retry after a serialization failure
would run again code that keeps what it read in Go. What it costs: one
writer at a time across the replicas, as in one process; a write that
takes seconds (a backfill's day) holds the others' for as long, as it did.

- **A replica cut off in a write** holds the turn until the server ends
  its session. Every session of a replica asks the server to end it after
  60 s idle in a transaction (`idle_in_transaction_session_timeout`),
  behind a proxy that keeps a dead client's connection too; TCP's
  keepalives end it within a minute otherwise. No write waits that long
  between two statements.
- **This process's own earlier writer's sessions are ended** when the
  writer connects again (one the server kept, in a transaction, would hold
  the turn): known by pid and start, as before.
- **Ids are read under the turn, not trusted from a cache.** A replica
  caches rules' and alerts' ids; every write that deletes or renames some
  (pruning, a recorded rule named apart, an alarm's rule tied) counts it
  in `replica_gens.ids`, and every write reads the count under the turn
  and drops its cache when it moved. Labels are kept by their text, which
  each read checks.
- **Migrations** take the turn too: replicas started at once on an empty
  database make the ledger once.

## Caches told of the others' writes

What a replica keeps beside the ledger, kept by the ledger's generation
(the pages, the owners, the roles given, the credits, the hours by week,
the outcomes, the notifications shown, the content findings), is told of
the other replicas' writes through the ledger:

- `replica_gens` counts the writes a page shows (`gen`), the last in which
  every week's firings may have changed, the recorded rules', the ids'; and
  `replica_weeks` the generation each week's firings last changed in. Each
  write counts itself there, in its transaction.
- **Each request reads them first** (`Refresh`, one round trip): a write
  committed before the request was asked, on any replica, is on the page
  it answers. A refresh under way when the request came, begun earlier,
  isn't taken for its own. Each replica reads them every second as well,
  for what isn't a request (the leader's jobs, `/metrics`'s tail).
- **The staleness bound**: none for pages, the API and MCP; a second for
  the rest; a few seconds for what the leader says of its work (below).
- A generation that went back (a failover that lost commits) is taken for
  every week changed.
- A newer lapilli that migrated the ledger is found at the next read; the
  replica stops (`ErrNewer`), rather than write a schema it doesn't know.

**What the leader is doing** (the status line, the runs page's counts, the
check of the replay's empty series, the on-call feeds' last reads) is the
leader's process's: it says it every `beatEvery` (2 s) in its row of
`replica_members`, and a follower shows that.

## The outbox claimed

A notice is sent once it is claimed: `UPDATE outbox SET next_at = now +
claimFor WHERE id = (SELECT id ... ORDER BY id LIMIT 1 FOR UPDATE SKIP
LOCKED)`, with the lease checked in the same statement. Two senders, two
leaders for a moment, never take one notice: one skips what the other
holds. A claim lasts `claimFor` (2 min); the notice is sent within half of
it, or not at all; noted sent (or failed, to be tried later) by itself,
without waiting for the writers' turn, which a long write could hold past
the claim. A claim never noted is taken again once it is over. A round
takes each notice due once, in order, as before.

The digest posted to Slack is claimed so too, for the week
(`ClaimOnce`), and noted sent whether or not the lease lasted: what was
posted is noted, so the next leader doesn't post it again.

## Rate limits and busy guards

**Builder's decision: what guards the ledger's writes is shared through
the database; what guards a process's own memory and time stays each
replica's.**

- **Shared**: the tokens' rates (incidents, firings, tasks, changes) and
  the notifications port's, a source each: a bucket a row
  (`replica_buckets`), taken from by the database's clock, so several
  replicas let through what one did, however a load balancer spreads the
  requests and whichever pod a client's connection sticks to. A bucket
  that can't be read refuses (as the write it guards would fail), after
  one more try. The day's most alerts a source's notifications carry is
  counted again from the ledger each minute, by each replica.
- **Each replica's**: one heavy request at a time (a month's firings, the
  settings tried over a week), one comparison an agent asks for, one write
  a token at a time, the port's busy and pre-proof slots, the events' rate
  and bytes, the OCI and SNS fetches. They bound what one process holds;
  N replicas hold N times as much, each its own.

## Signing in

- **The session key is the Secret's**, mounted by every pod: a session
  signed by one replica is taken by the others, and no pod need be the one
  a person came to (no sticky sessions). The chart refuses `serve.replicas`
  above 1 with `serve.signIn.oidc` and no session key Secret. Run by hand,
  a replica with a sign-in of lapilli's own and no `--session-key-file` is
  refused while another lives, and the first warns: each would sign with a
  key of its own. The key is never kept in the ledger
  ([signin.md](signin.md)).
- **A sign-in's state is used once among the replicas** (`replica_once`):
  a callback replayed to another replica is refused as it is to the one it
  came back to.
- **Who signed out** is in the ledger already, read on each request.
- **The approval forms' key** is the ledger's (`changes-form-key`, made by
  the first replica that needed one): a form one replica shows is good on
  another. It lets no one in, and signs no session.

## Readiness and metrics

- **A follower is ready as the leader is**: once a run is recorded. A
  replica that can't reach the ledger's database within 3 s isn't ready,
  so that the others answer for it; when none can, none is, and the
  Service has no endpoint, as pages would fail anyway.
- `/readyz` says which leads: "replica ops-lapilli-6c5d-x2b-1a2b3c4d
  leads, term 7, since ... (3 live)", or "follows ...".
- `/metrics`: `lapilli_serve_leader` (1 on the leader's pod, 0 on the
  others) and `lapilli_serve_replicas` (those live). What the leader's
  work counts (`lapilli_serve_runs_total`, the last run's duration,
  notices sent, the incidents file passed over) is in its pod's metrics;
  what is read from the ledger is in every pod's alike. A ServiceMonitor
  scrapes each pod; a rule over the leader's work reads
  `max(...)` or `... and on(pod) lapilli_serve_leader == 1`.

## Failure cases

- **The leader killed** (`SIGKILL`, a node gone): its lease lapses; a
  follower takes it within 17 s, at a later term. Notices due
  meanwhile wait, kept in the outbox, and go then. One it had claimed and
  sent but not noted goes again once its claim is over (2 min).
- **The leader cut off from the database**: its renewals fail; it stops
  leading by its own clock 13 s after its last renewal at most, and a
  follower leads 2 s or more after that. What it would write as the leader
  is refused in the database once another's term is there. A write of its
  cut off in the middle holds the writers' turn until the server ends the
  session (60 s at most, above).
- **A follower cut off**: it is no longer ready; the others serve.
- **The database fails over**: every connection drops, and is made again;
  a write in flight fails, and is retried by its caller as before (a run,
  at the next round). The lease's row is the database's: if the failover
  lost its last renewal, the leader's own clock still ends it first.
  Generations that went back are taken for everything changed.
- **A rolling update**: each new pod follows; each old pod gives the lease
  up as it stops; a follower takes it within a campaign. The UI and the
  ports keep answering, from the pods still up.
- **A new release that migrates the ledger**, rolled onto older replicas:
  the new pod migrates as it starts, under the writers' turn; each older
  replica finds the newer schema at its next read (a second) and stops,
  and is replaced by the rollout. Each migration says so in the changelog.
- **A copy, or a 0.34 serve, started against a ledger replicas share** is
  refused; one that took the lock alone while every replica's sessions
  were gone (a database restart) is found by the replicas as they connect
  again, and they stop.

## Not in this

- **A read replica of Postgres** for the pages: every read is the
  primary's.
- **Leaders by kind of work** (one for the runs, another for the outbox):
  one leads it all, as one process did.
- **Rate limits divided among replicas**: shared instead, above.
- **A pooler in transaction mode** (PgBouncer's): the lock is a session's,
  and so is the writers' turn's transaction lock; in session mode, or
  directly, as before.
- **SQLite replicas**: one process, one file.

## As built

**Takeover, measured.** Three `lapilli serve` processes on one Postgres 17
ledger, the timings as shipped (a lease of 15 s, renewed every 5 s, a
campaign every 2 s), the leader's process `SIGKILL`ed: another led 15.2 s
later, and each of five tasks' notices, made on the three before and the
two after, reached the webhook once (`test/e2e/postgres.sh`, run against a
local server, its Docker steps left out). With the tests' timings (a lease
of 1.5 s, renewed every 300 ms, a campaign every 100 ms, a margin of 300
ms), 20 runs each:

| | the old leader stopped | another led |
|---|---|---|
| a leader cut off from the database (`TestReplicasTakeOver/cut_off`) | 0.99 to 1.01 s after | 1.30 to 1.42 s after |
| a leader killed (`TestReplicasTakeOver/killed`) | at once | 1.30 to 1.38 s after |
| a leader cut off, the ledger alone (`TestReplicasTakeOverFromACutOffLeader`) | 1.19 s after | 1.53 to 1.56 s after |
| a leader that stopped (`TestReplicasOneLeads`) | at once | 11 to 23 ms after |

Each time the old leader stopped before another began, at a later term.
In 20 runs of three replicas each, every run and every notice was done
once, and a former leader whose lease another took sent nothing.

**The tests**, on SQLite and on Postgres 16 and 17 locally (CI runs 15 and
18): the ledger's, serve's and the command's suites pass on all three: 698
tests on SQLite (38 of Postgres's alone skipped), 730 on Postgres 17 and
728 on 16 (the last two tests added after it ran), 6 of SQLite's alone
skipped. The replicas' own, beside the ledger's tests of
the lock: replicas started at once; writes that read then write, merged
and counted against a limit by three replicas at once; another replica's
writes on the next refresh, and on the next request's page and API; one
leader of three, handed over when it stops; a leader cut off or killed,
followed within the lease; fenced writes, by the database and by the
clock; notices claimed once by six senders on two replicas; shared rate
limits and uses once; the leader woken and asked to judge again; a newer
schema; three servers running each job once; a former leader that sends
nothing; no job once the lease ended by the clock; the admin's pause, the
settings, the forms' key, a sign-in's state and the token's burst across
replicas; the leader's status on a follower's pages; readiness without
the database; one session key. The chart's: refused without Postgres, the
rolling update, the budget, the session key.

**Each guard was taken away in turn** (31), and a test failed each time:
the lease checked in the database in a leader's write, and the clock
before it commits; the leader stopping at its lease's end by its clock;
`Hold` saying whether it leads; the lease looked at before each job; the
lease taken only once lapsed; the outbox claimed `FOR UPDATE SKIP LOCKED`,
under the lease, each notice once a round; the digest's post claimed; the
generation counted in the database, and the weeks changed; each request
reading the others' writes; a refresh begun before the request not taken
for one after; ids another replica deleted dropped; the writers' turn, for
writes and for migrations; the writer's own earlier sessions ended; the
lock shared, and alone for a copy; a newer schema stopping a replica; the
rate limits shared; the pause, the rejudge, the forms' key and a sign-in's
state across replicas; the leader's status shown; only the leader running
the singletons; readiness without the database; one session key; the
chart's refusal and its rolling update.

**Not yet run**: `test/e2e/replicas.sh` on kind (CI's `e2e-replicas`),
`test/e2e/postgres.sh` with its Postgres in Docker, and the suites on
Postgres 15 and 18, which CI runs. Both scripts pass shellcheck, and the
workflow actionlint.
