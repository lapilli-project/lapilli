# Replicas on one Postgres ledger

Status: built, for the release after 0.35.0. `lapilli serve` with its
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
- **From 0.35 or before, upgrade with one replica first.** Such a process
  holds the ledger's lock alone (below): a new pod is refused until it has
  stopped, which `Recreate` does first and `RollingUpdate` never would.
  Then raise `serve.replicas`.
- **Every replica runs with the same flags**, as a Deployment's pods do.
  Nothing checks it: during a rolling update that changes them, either
  may lead.

## The lock, shared

The advisory lock of [postgres.md](postgres.md) is kept, now taken
**shared** by every session of every replica (`pg_try_advisory_lock_shared`),
and **alone** by `lapilli ledger copy` (`OpenPostgresAlone`), as 0.35 and
before took it. The two refuse each other: a copy is refused while any
replica runs ("holds its lock, and a ledger serve's replicas share is
written by no other process"), and a replica while a copy, or a 0.35
`serve`, holds it ("holds its lock alone, as a copy, or a lapilli of 0.35
or before, does"). Replicas don't count themselves in `schema_version.writers`:
a replica that connects again and finds the count moved finds that a
process wrote the ledger alone in between, and stops (`ErrLockLost`), as a
process did before. Only a holder that refuses it makes a replica take
the ledger for lost: another replica is no reason, nor a session that
**waits** for the lock alone. No lapilli waits so (each asks, and is
refused), so a waiter holds nothing and wrote nothing; but while it waits,
Postgres gives no session the lock shared, so no new replica starts and
no running one connects again. Both say which session waits and why
(`LockWaitError`), and the running replica goes on trying.

**What a role that may only connect can still do**, which is why the
database should be the ledger's role's alone (`REVOKE CONNECT ON DATABASE
lapilli FROM PUBLIC; GRANT CONNECT ... TO lapilli`): take or wait for the
ledger's lock alone, and keep replicas from starting or connecting again
(an advisory lock is anyone's who can connect, and its keys are in
`pg_locks`); take the advisory lock that makes an empty schema a ledger,
and hold the first start up. It can't hold the writers' turn, the lease,
a claim or a bucket: each is a row of the ledger's, which it can't lock.
A lapilli of 0.35 or before, or a copy, alone, is refused while such a
role holds the lock, and one whose connection drops finds it taken, and
stops, as before.

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
  (serve shut down, a lease it couldn't renew, work that is stuck), before
  its work has returned, which stuck work may never do: `until` is set in
  the past, and a follower takes it at its next campaign. What the old
  work would still write, claim or apply is refused in the database
  (below). The replica leads again only once that work returned. A
  renewal answered after the leader stopped would otherwise keep the
  lease from the others for its length.
- On its own pool (`lease`, one connection), so that no write, which can
  take seconds, holds a renewal up; and each renewal is asked apart from
  the clock that ends the lease, so that one waiting on the database
  doesn't hold the end up.

**The leader stops by its own clock, before the database lets another
in**, as a rule: it counts each lease from just before it asked for the
renewal, and stops leading `leaseMargin` (2 s) before that lease ends: the
database's lease ends later than that, however long the round trip took.
Clocks are not compared, only how fast each runs: a drift of a part in ten
thousand moves 15 s by 1.5 ms. Not always: a failover that lost the last
renewal ends the database's lease up to a renewal (5 s) earlier than the
leader believes, and a suspended host's clock stops. So the database has
the last word on everything the leader does (below).

**A leader whose work is stuck gives the lease up.** Reaching the database
isn't leading. The leader's work, in a round, notes its progress: a job
begun, a run's piece, and, in its context, each request asked of a
history (Prometheus, Loki, a Grafana's) and each rule replayed. Work that
makes none for 15 min (`stepWithin`: three times the longest one step
takes, a request answered within 5 min or a wait of 2 min before asking
again, or a write within its deadline of 5 min), or a write that has held
the writer past its deadline and a minute (6 min), stops the renewals at
the next one due: the lease is given up, and the replica asks for none
while it stays stuck. Another leads at its next campaign: **15 min 7 s
after the leader's last progress, at most.** A round can take long on
purpose (a first backfill of weeks, a review of many versions): it makes
progress all along, and isn't taken for stuck.
`lapilli_serve_jobs_round_timestamp_seconds` says when the leader last
finished a round of its jobs. A write that waits for the writers' turn
isn't stuck work: it gives up after 30 s (`lock_timeout`, below), and the
turn is every replica's, so another leading wouldn't write sooner.

**Takeover**: within `leaseFor` and a campaign of the leader's last
renewal, killed or cut off: 10 to 17 s after its end, as it renewed 0 to 5
s before it. Given up (a rolling update, `SIGTERM`): within a campaign, 2
s at most. Stuck: 15 min 7 s after its last progress at most. Measured,
under "As built".

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
- **Each write it makes is checked in the database**, as it begins and
  again **as it commits**: a deferred constraint trigger (`replica_fenced`),
  armed by a row each fenced write adds, reads the lease `FOR SHARE` in the
  commit, and refuses it unless the lease is still this replica's, at its
  term, unlapsed by the database's clock. A take of the lease isn't held up
  by the leader's open write (no lock is held until the commit), but waits
  for its commit, a moment, and one that came first refuses it. And by the
  leader's own clock, before it commits. A leader cut off, suspended past
  its lease, or whose renewal a failover lost, commits nothing after
  another's term began. What a follower, or a person on the leader's pages,
  writes is no leader's work and isn't checked.
- **Everything sent out of the process is sent after `Hold`**, as before,
  which now also says whether this replica still leads by its clock; and
  only after a step the database checks: a notice once it is **claimed
  under the lease** (the lease read `FOR SHARE` in the claim's statement),
  the digest once it is claimed so too, a change once it is moved to
  applying by a fenced write.
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

Every write, on every replica, takes one lock first, in its transaction:
`replica_gens`'s one row, `FOR UPDATE`. Writes take turns across the
replicas as they did in one process (`Ledger.mu`), and a write that reads
what it then changes reads what no other write is changing. **Builder's
decision: one turn for every write, not `SERIALIZABLE` at each place that
reads then writes.** The places are many and not all plain: `UpdateMeta`,
a team's quiet windows and the ledger's, roles, proposals in an hour and
one at a time for a rule, approvals, a proposal moved on, a notice's
attempts, `TellOnce`, a recorded rule named apart, ids made where none
was. Each was written for one writer, and is as right with the turn as it
was, without finding each; a retry after a serialization failure would
run again code that keeps what it read in Go. What it costs: one writer at
a time across the replicas, as in one process; a write that takes seconds
holds the others' for as long, as it did. No leader's job holds it long:
a backfill is a write a day, judging again a write a week; while a leader
backfilled four weeks of 70,000 firings, writes on the follower and the
leader waited 274 ms at most (`TestReplicasWritesDontWaitOnTheBackfill`
holds them to 1 s).

- **A row lock, not an advisory lock**: locking it needs UPDATE on the
  ledger's table, which a role that may only connect hasn't; an advisory
  lock is anyone's who can connect, and its key is in `pg_locks`.
- **Bounded.** Each replica's writer session waits `lock_timeout` (30 s)
  for any lock, the turn among them: a write that waits longer fails
  (`ErrTurnWaited`), and the replica says its writes wait (metrics and
  status, below) until a write takes the turn again. It stays ready: the
  turn is every replica's, so a session holding it would take every
  replica out of the Service at once, though each answers its pages.
  **Builder's decision: replicas set `lock_timeout`** (postgres.md's one
  process sets none): what a replica's write waits for is the turn, a
  row another replica holds for a statement, or a table a migration
  changes, which waits 2 s at most for its own lock (below). The
  replicas' own tables are changed outside the turn, and only by a start
  that finds them older.
- **A replica cut off in a write** holds the turn until the server ends
  its session, a minute after its last statement ended at most: 60 s idle
  in a transaction (`idle_in_transaction_session_timeout`), behind a proxy
  that keeps a dead client's connection too; 60 s with its answer not
  taken in (`tcp_user_timeout`, on Linux servers), which TCP would
  otherwise retry some 15 min; TCP's keepalives end an idle one within a
  minute. No write waits that long between two statements.
- **This process's own earlier writer's sessions are ended** when the
  writer connects again (one the server kept, in a transaction, would hold
  the turn): known by pid and start, as before.
- **Ids are read under the turn, not trusted from a cache.** A replica
  caches rules' and alerts' ids; every write that deletes or renames some
  (pruning, a recorded rule named apart, an alarm's rule tied) counts it
  in `replica_gens.ids`, and every write reads the count under the turn
  and drops its cache when it moved. Labels are kept by their text, which
  each read checks.
- **A newer schema**: each write reads the schema's version under the
  turn, in a statement of its own. A migration takes the turn too, with
  `schema_version` locked: a write of an older replica's waits for it, finds
  the newer schema, writes nothing, and the replica stops (`ErrNewer`).
- **Migrations** lock `schema_version`, which needs the ledger's
  privileges, so replicas started at once migrate it once. On an empty
  schema, where there is none yet, an advisory lock instead, and the
  catalog asked afresh once it is held (`to_regclass` would answer from
  what the session cached at connect). Every lock but the turn is taken
  before it, or waited for 2 s at most: a migration's DDL waits that long
  for a table's lock (`pg_dump` holds one on each table for the whole
  dump), the turn held meanwhile, then gives up, and is tried again every
  10 s, for 5 min, after which the start fails, and is tried again as the
  pod is.
- **The replicas' own tables** have a version of their own
  (`replica_schema`): a start that finds them as it makes them, or newer,
  runs no DDL, which would wait on a dump, and needs no owner of them.
  One that finds them older changes them outside the turn, which no
  write of the ledger's needs, its DDL bounded as a migration's.

## Caches told of the others' writes

What a replica keeps beside the ledger, kept by the ledger's generation
(the pages, the owners, the roles given, the credits, the hours by week,
the outcomes, the notifications shown, the content findings), is told of
the other replicas' writes through the ledger:

- `replica_gens` counts the writes a page shows (`gen`), the last in which
  every week's firings may have changed, the recorded rules', the ids'; and
  `replica_weeks` the generation each week's firings last changed in. Each
  write counts itself there, in its transaction, and logs a random nonce
  with its generation (`replica_log`).
- **Each request reads them first** (`Refresh`, one round trip): a write
  committed before the request was asked, on any replica, is on the page
  it answers. A refresh under way when the request came, begun earlier,
  isn't taken for its own. One refresh is under way at a time, within 5 s
  of the database, whoever asked; each caller waits for it as long as its
  own context lets, and a write that commits waits for none (what it saw
  moves on in memory alone). Each replica reads them every second as well,
  for what isn't a request (the leader's jobs, `/metrics`'s tail).
- **The staleness bound**: none for pages, the API and MCP; a second for
  the rest; a few seconds for what the leader says of its work (below).
- **A failover that lost commits**: the nonce logged with the generation
  a replica last saw is no longer the ledger's, whether or not as many
  writes were made since: every week is taken for changed, and the ids
  cached are dropped. The log keeps the last 100,000 generations; a replica
  that saw an older one reads everything again.
- **Wakes and rejudges** are counted in a row of their own
  (`replica_signals`), which no write's turn holds: one asked during the
  leader's own write is read at its next refresh.

**What the leader is doing** (the status line, the runs page's counts, the
check of the replay's empty series, the on-call feeds' last reads) is the
leader's process's: it says it every `beatEvery` (2 s) in its row of
`replica_members`, and a follower shows that.

## The outbox claimed

A notice is sent once it is claimed: `UPDATE outbox SET next_at = <now> +
claimFor WHERE id = (SELECT id ... ORDER BY id LIMIT 1 FOR UPDATE SKIP
LOCKED)`, with the lease checked in the same statement, `FOR SHARE`. Two
senders, two leaders for a moment, never take one notice: `FOR UPDATE`
alone keeps that (a second sender rereads the row the first changed);
`SKIP LOCKED` has it take the next at once rather than wait for the first
(`TestReplicasClaimSkipsWhatIsHeld`).

- **By the database's clock**: a notice is due, and a claim lasts, by the
  later of the sender's clock and the database's, which every replica
  reads; a notice told is due by the database's (not by the clock of the
  replica that told it, which may be ahead), and one that failed is tried
  again by it. A claim lasts `claimFor` (2 min) from when it is taken,
  however long the round that takes it has run.
- The notice is sent within half its claim, or not at all; noted sent (or
  failed, to be tried later) by itself, without waiting for the writers'
  turn, which a long write could hold past the claim. A claim never noted
  is taken again once it is over. A round takes each notice due once, in
  order, as before.
- Each statement of the pool that claims and notes (two connections, which
  the rate limits share) waits 30 s at most, whatever its caller's
  context: one noted after the lease ended has no other deadline.

The digest posted to Slack is claimed so too, for the week
(`ClaimOnce`), and noted sent whether or not the lease lasted: what was
posted is noted, so the next leader doesn't post it again. A leader that
finds it claimed by the one before tries again once the claim is over,
not an hour after.

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
- **What Prometheus is asked, each replica's too.** `--max-rate` is a
  process's: the leader's runs ask the history at it, and each follower's
  page-time replays (a proposal's backtest, the settings tried, an agent's
  comparison), one at a time behind the guards above, at it too. A history
  that holds to a quota (a managed Prometheus) can be asked N times
  `--max-rate` at once, and turn the leader's runs away. **Builder's
  decision: kept a process's**: dividing it among the replicas would slow
  the leader's runs, which are most of what is asked, for what people ask
  at times. Give such a history `--max-rate` of its quota divided by the
  replicas.

## Signing in

- **The session key is the Secret's**, mounted by every pod: a session
  signed by one replica is taken by the others, and no pod need be the one
  a person came to (no sticky sessions). The chart refuses `serve.replicas`
  above 1 with `serve.signIn.oidc` unless `serve.signIn.sessionKeySecret.name`
  names a Secret of your own: one the chart makes is made again by a render
  that can't look it up (`helm template`, Argo CD, Flux), and a pod started
  after would sign with another key. The key is never kept in the ledger
  ([signin.md](signin.md)).
- **Replicas compare the keys they sign with.** Each replica keeps a
  fingerprint of its key (an HMAC of a fixed text with it) in its row of
  `replica_members`, writing it before it reads the others', so that of
  two started at once one at least sees the other. A key is as new as the
  first live replica started with it. One whose key is the newest joins
  while others sign with an older one, as a rollout of a new key brings
  it in, and says so; `lapilli_serve_session_keys` and the status line
  say how many keys the live replicas sign with until the older replicas
  go, and meanwhile a session works on the replicas of its key alone. One
  whose key is older than another live, as a pod the rollout replaces,
  started again with the old Secret, is refused. **Builder's decision:
  the newer key joins, rather than every other key being refused**,
  which stalled a rollout of a new key with `maxUnavailable: 0`, and kept
  a pod restarted after an edit of the Secret from coming back. What it
  costs: replicas given different keys on purpose run, each warning, and
  the metric says 2 or more for longer than a rollout. Without
  `--session-key-file` each process makes a key of its own, never a
  rotation: a second replica is refused, and the first warns. Changing
  the key: [signin.md](signin.md).
- **A sign-in's state is used once among the replicas** (`replica_once`),
  kept once the provider signed in someone a role lets in: a callback
  replayed to another replica is refused there, as the provider refuses
  its code again. A sign-in that signs no one in, which anyone can make
  without signing in, writes nothing to the ledger, nor one of a stranger
  the provider signs in (a provider anyone can sign in to); each process's
  own set refuses them again. The uses once kept are swept at their time
  and bounded (100,000): past it, this replica's own set checks a state.
- **Who signed out** is in the ledger already, read on each request; with
  replicas, one that can't be read is taken for a sign-out: another
  replica may have kept one this one hasn't read. A sign-out made while
  it can't be read is kept all the same: on this replica at once, and in
  the ledger, tried again when it can't take it (for some 20 min, then
  with the next sign-out); meanwhile another replica takes a copy of the
  cookie.
- **The approval forms' key** is the ledger's (`changes-form-key`, made by
  the first replica that needed one): a form one replica shows is good on
  another. It lets no one in, and signs no session.

## Readiness and metrics

- **A follower is ready as the leader is**: once a run is recorded. A
  replica that can't reach the ledger's database within 3 s isn't ready:
  the others answer for it. When none can, none is, and the Service has no
  endpoint, as pages would fail anyway. One whose writes wait for the
  writers' turn is ready, as is every other: the turn is one for them all,
  and their pages answer; they say so (below).
- **Which replica leads** is said on the metrics' port's `/readyz`, which
  Prometheus alone need reach: "replica ops-lapilli-6c5d-x2b-1a2b3c4d
  leads, term 7, since ... (3 live)", or "follows ...", and whether its
  writes wait on the turn, and how many keys sign sessions when more than
  one. The UI's port's, which answers anyone whatever the host, says ok.
- `/metrics`: `lapilli_serve_leader` (1 on the leader's pod, 0 on the
  others), `lapilli_serve_replicas` (those live),
  `lapilli_serve_ledger_writes_waiting` (1 since a write gave up on the
  turn, until one takes it), `lapilli_serve_session_keys` (with a sign-in
  of lapilli's own), and on the leader's,
  `lapilli_serve_jobs_round_timestamp_seconds`. What the leader's work
  counts (`lapilli_serve_runs_total`, the last run's duration, notices
  sent, the incidents file passed over) is in its pod's metrics; what is
  read from the ledger is in every pod's alike. A ServiceMonitor scrapes
  each pod; a rule over the leader's work reads `max(...)` or `... and
  on(pod) lapilli_serve_leader == 1`.

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
  session (a minute after its last statement, above): every replica's
  writes wait meanwhile, and say so; each stays ready.
- **The leader's work stuck** (a history that stops answering, a request
  that doesn't return): it gives the lease up 15 min after its last
  progress, and another leads within 7 s more.
- **A backup** (`pg_dump`, which holds a lock on each table for the whole
  dump) while replicas run, or start: a start makes nothing where the
  replicas' tables are as it makes them, so it starts, and no write
  waits. A start that must migrate, or change the replicas' tables, waits
  2 s at a time for the dump's locks, tried again for 5 min.
- **A follower cut off**: it is no longer ready; the others serve.
- **The database fails over**: every connection drops, and is made again,
  to a server that started at another time, which has the writer read what
  it caches again (a connection made again to the same server, as after a
  write cancelled in the middle, loses nothing, and keeps it);
  a write in flight fails, and is retried by its caller as before (a run,
  at the next round). The lease's row is the database's: if the failover
  lost its last renewal, its lease ends up to 5 s sooner than the leader
  believes, and another may take it while the old leader still leads by
  its clock: what it would write, claim or apply as the leader is refused
  in the database, at commit. Generations that went back, or that lost
  commits others' numbers took, are taken for everything changed.
- **A rolling update**: each new pod follows; each old pod gives the lease
  up as it stops; a follower takes it within a campaign. The UI and the
  ports keep answering, from the pods still up.
- **A new release that migrates the ledger**, rolled onto older replicas:
  the new pod migrates as it starts, under the writers' turn; each older
  replica finds the newer schema at its next read (a second) and stops,
  and is replaced by the rollout. Each migration says so in the changelog.
- **A copy, or a 0.35 serve, started against a ledger replicas share** is
  refused; one that took the lock alone while every replica's sessions
  were gone (a database restart) is found by the replicas as they connect
  again, and they stop.
- **Going back to 0.35 or 0.34**: its `lapilli ledger copy` takes a Postgres
  ledger's tables to be the ledger's, the replicas' among them, and refuses
  ("the two ledgers' tables differ"). Drop them first, with every serve
  stopped: `DROP TABLE replica_lease, replica_gens, replica_signals,
  replica_log, replica_weeks, replica_members, replica_buckets,
  replica_once, replica_fence, replica_schema CASCADE; DROP FUNCTION
  replica_fenced();`.
  **Builder's decision: in the ledger's schema, not one of their own**: a
  schema of their own would need the role to create schemas, and the
  search path pinned to two.

## Not in this

- **A read replica of Postgres** for the pages: every read is the
  primary's.
- **Leaders by kind of work** (one for the runs, another for the outbox):
  one leads it all, as one process did.
- **Rate limits divided among replicas**: shared instead, above.
- **A pooler in transaction mode** (PgBouncer's): the lock is a session's;
  in session mode, or directly, as before.
- **SQLite replicas**: one process, one file.

## As built

**Takeover, measured.** Three `lapilli serve` processes on one Postgres 17
ledger, the timings as shipped (a lease of 15 s, renewed every 5 s, a
campaign every 2 s), the leader's process `SIGKILL`ed: another led 15.2 s
later, and each of five tasks' notices, made on the three before and the
two after, reached the webhook once (`test/e2e/postgres.sh`, run against a
local server, its Docker steps left out). With the tests' timings (a lease
of 1.5 s, renewed every 300 ms, a campaign every 100 ms, a margin of 300
ms), 20 runs each after the reviews' changes:

| | the old leader stopped | another led |
|---|---|---|
| a leader cut off from the database (`TestReplicasTakeOver/cut_off`) | 0.98 to 1.02 s after | 1.30 to 1.36 s after |
| a leader killed (`TestReplicasTakeOver/killed`) | 10 to 14 ms after | 1.31 to 1.36 s after |
| a leader cut off, the ledger alone (`TestReplicasTakeOverFromACutOffLeader`) | 1.19 s after | 1.54 to 1.57 s after |
| a leader that stopped (`TestReplicasOneLeads`) | at once | 11 to 69 ms after |

Each time the old leader stopped before another began, at a later term.
In 20 runs of three replicas each, every run and every notice was done
once, and a former leader whose lease another took sent nothing; a write
fenced at commit, a claim, and a stuck leader giving its lease up, 20
times each, held as well.

**Writes beside the backfill.** Three processes on one ledger while the
leader backfilled 28 days: 1,278 API writes on the three, the longest 71
ms. `TestReplicasWritesDontWaitOnTheBackfill` holds a write to 1 s while
a leader backfills four weeks of 70,000 firings: 65 to 274 ms in its
runs; with the backfill taken in one write, 1.23 to 1.38 s, and the test
fails.

**Started together.** `TestReplicasStartTogether` (eight replicas on an
empty schema, five rounds, then on a ledger without the replicas' tables)
passed 60 times of 60 while the whole suite ran on the same Postgres 16
server, twice; before the reviews' change it failed 3 times of 60 so
(the review's count).

**The tests**, every package's, on SQLite and on Postgres 16 and 17
locally (CI runs 15 and 18): 1,719 tests and subtests pass on Postgres 17
and on 16, 11 skipped (SQLite's own, and an accessibility check run on
its own); 1,661 on SQLite, 67 skipped (Postgres's own). The
replicas' own, beside the ledger's tests of the lock: replicas started at
once, and one that waits while another makes the ledger; writes that read
then write, merged and counted against a limit by three replicas at once;
another replica's writes on the next refresh, and on the next request's
page and API; one refresh at a time, within its deadline, which no write
waits for; deadlines on the side pool; one leader of three, handed over
when it stops; a leader cut off or killed, followed within the lease; a
stuck leader giving its lease up; fenced writes, by the database at the
write's start and at its commit, and by the clock; notices claimed once by
six senders on two replicas, from when each is taken, due by the
database's clock, the next taken while another holds the first; shared
rate limits and uses once, swept and bounded; the leader woken, during
its own write as well, and asked to judge again, from a follower; a newer
schema, found by a refresh and by a write under the turn; a migration
taking the turn; failovers that lost commits; ids another replica deleted;
a role that may only connect, and a session that waits for the lock alone;
three servers running each job once; a former leader that sends nothing;
no job once the lease ended by the clock; the admin's pause, the settings,
the forms' key, a sign-in's state and the token's burst across replicas;
no state kept for a sign-in that signed no one in; sign-outs that can't be
read refusing a session; the leader's status on a follower's pages;
readiness without the database, while writes wait, and saying ok alone
on the UI's port; session keys compared; the digest after another's claim;
stepping down. The chart's: refused without Postgres, the rolling update,
the budget and its percentages, the session key's Secret.

**Each guard was taken away in turn** (68), and a test failed for 67:
those above, and the chart's. The one left is the lease checked as a
leader's write begins, with the check at commit kept: the commit's catches
what it would, so no test fails without it. It is kept as the cheaper
refusal, before a write does its work.

**Not yet run**: `test/e2e/replicas.sh` on kind (CI's `e2e-replicas`),
`test/e2e/postgres.sh` with its Postgres in Docker, and the suites on
Postgres 15 and 18, which CI runs. Both scripts pass shellcheck, and the
workflow actionlint. The kind run failed in CI on a port: the script
asked the Service on port 80, where it answers on `serve.port` (8080);
it now reads the Service's ports, times each write, and prints the pods,
their logs and the Service's endpoints when it fails.
