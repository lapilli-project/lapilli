# Changelog

## [Unreleased]

`lapilli serve` runs as several replicas on one Postgres ledger: one leads,
and runs what must run once; every replica serves the rest
(docs/design/replicas.md). No ledger schema change: the replicas' own
tables are Postgres's alone, made as a ledger opens, and none is copied.
SQLite is as it was: one process, the same files. Read "Before you
upgrade".

### Added

- **Replicas on a Postgres ledger.** Every `serve` on a Postgres ledger is
  a replica, alone or with others, sharing the ledger's advisory lock.
  - **A leader, on a lease** the database keeps (`replica_lease`), by its
    clock, at a term one more each time it changes hands. The leader runs
    the runs and the backfill, judging again, the incidents and deploys
    files, the check of the replay, near misses, proposals, the digest and
    the reports, the outbox, changes carried out (Grafana, a ruler, a
    cloud's alarms), the rules watched and their versions, the rules'
    history, the on-call feeds, and pruning. Every replica serves the UI,
    the API, MCP, `/metrics`, sign-in, and the incidents' and
    notifications' ports, and writes what it is sent.
  - **Takeover**: a follower takes the lease within 17 s of the leader
    being killed or cut off (15.2 s measured, the leader's process
    `SIGKILL`ed), within 2 s of it stopping, which gives the lease up.
  - **Fencing**: the leader stops leading by its own clock 2 s before its
    lease ends in the database, its work cancelled at once; each of its
    jobs looks at the lease first; each write it makes checks the lease in
    the database as it begins and as it commits (a deferred trigger that
    reads the lease `FOR SHARE`), and its clock before; each send waits for
    `Hold`, and for a claim the database checks under the lease. A leader
    whose lease went elsewhere, a renewal a failover lost or a host
    suspended among them, sends and writes nothing as the leader.
  - **A leader whose work is stuck gives the lease up**: a step of its work
    that runs past any step's time, or a write held past its deadline,
    stops the renewals; `lapilli_serve_jobs_round_timestamp_seconds` says
    when it last finished a round.
  - **Each write takes the writers' turn across the replicas**: a row
    lock, which a role that may only connect can't take, waited for 30 s
    at most (`lock_timeout`), the replica not ready meanwhile. What reads
    then writes (merged values, the limits on windows, roles and
    proposals, approvals, `TellOnce`) is as right as with one process. A
    session cut off in a transaction is ended by the server after 60 s
    idle in it. A write of an older replica after a newer lapilli migrated
    the ledger writes nothing, and the replica stops.
  - **The caches told of the others' writes**: the generations pages are
    cached by, and which weeks' firings changed, kept in the ledger
    (`replica_gens`, `replica_weeks`) and read before each request, one
    read at a time, within 5 s, which no write waits for: a write on one
    replica shows on the next request to any other. Ids cached are dropped
    when another replica deleted or renamed some, and everything is read
    again after a failover that lost commits (a nonce logged with each
    generation).
  - **Notices claimed one at a time** (`FOR UPDATE SKIP LOCKED`, under the
    lease), due and claimed by the database's clock: two leaders for a
    moment never send one twice, a claim taken late in a long round still
    lasts, and a replica whose clock is ahead delays none. The digest
    posted to Slack is claimed for its week too.
  - **Rate limits shared**: the tokens' and the notifications port's
    buckets are the ledger's, so several replicas let through what one
    did; the busy guards (one heavy request, one write a token) stay each
    replica's.
  - **Sign-in across replicas**: a sign-in's state is used once among
    them, kept once the provider signed someone in (a sign-in that signs
    no one in writes nothing), swept at its time and bounded; the approval
    forms' key is the ledger's; a replica whose session key differs from a
    live replica's (a fingerprint each keeps) is refused, without
    `--session-key-file` a second one too; with replicas, a session is
    refused while who signed out can't be read.
  - **A follower shows what the leader is doing**, as the leader says it
    every 2 s; it reads the rules for what they say, and the on-call turns
    kept, as the leader does, without recording a version.
  - **`/readyz` on the metrics' port says which replica leads**; on the
    UI's, ok alone. A replica that can't reach the database, or whose
    writes wait too long, isn't ready. `/metrics` has `lapilli_serve_leader`
    and `lapilli_serve_replicas`.
- **The chart's `serve.replicas`**, more than one on Postgres alone:
  `RollingUpdate` one pod at a time (`maxSurge: 1`, `maxUnavailable: 0`),
  a PodDisruptionBudget (`serve.podDisruptionBudget.maxUnavailable`, 1,
  a percentage taken as Kubernetes rounds it, up), and a session key
  Secret of your own (`serve.signIn.sessionKeySecret.name`) required with
  `serve.signIn.oidc`. One replica stays `Recreate`.
- `test/e2e/replicas.sh`: three replicas on kind, the leader's process
  killed, another leading within the lease, every notice once, a rolling
  restart; and `test/e2e/postgres.sh` runs three replicas and kills one.

### Changed

- **A second `serve` on a Postgres ledger is a replica**, no longer
  refused. `lapilli ledger copy` takes the ledger alone, as serve did, and
  is refused while any replica runs; a replica is refused while a copy,
  or a serve of 0.34 or before, holds the ledger alone. Replicas don't
  count themselves in `schema_version.writers`.
- A Postgres ledger's migrations take the writers' turn, so replicas
  started at once migrate it once; a replica that finds the ledger
  migrated by a newer lapilli stops.

### Before you upgrade

- **From 0.34 with `serve.ledger.postgres`, upgrade with
  `serve.replicas: 1`** (the default), then raise it: the 0.34 pod holds
  the ledger alone and refuses new pods until it stops, which `Recreate`
  waits for and `RollingUpdate` would not.
- **Each request to a Postgres ledger's serve asks the database once
  more**, for the others' writes, one replica or several.
- **Give the database to the ledger's role alone** (`REVOKE CONNECT ON
  DATABASE lapilli FROM PUBLIC`): a role that may only connect can still
  take or wait for the ledger's advisory lock, and keep replicas from
  starting or connecting again (docs/design/replicas.md).
- **Going back to 0.34**: its `lapilli ledger copy` refuses a ledger with
  the replicas' tables; drop them first, with serve stopped (the
  statement is in docs/design/replicas.md, "Failure cases").

## [0.35.0] - 2026-11-14

The web UI's frame, and the rules list, redone: a sidebar in four groups,
breadcrumbs, the team and the period carried on every link, a search on
every page, a page's main action in its header, and on the rules list
badges, filters, a search box and chips (docs/design/ui.md). Still no
script, in four languages, against WCAG 2.2 AA. The API, MCP and the
ledger are as they were. What two URLs show has changed, `weeks=1` on
the incidents and no `team=` for a reader of one team, and a page's
query has bounds: see Changed.

### Added

- **A sidebar** of the pages in four groups: See (overview, rules,
  incidents, firings), Fix (tasks, proposals, quiet windows), Report
  (reports, outcomes) and Manage (teams, roles, settings, runs), with the
  open tasks and the proposals to approve beside theirs, and Roles for
  admins. On a narrow screen, or zoomed in, it is a menu that names the
  page the reader is on. Every page has a breadcrumb: group, page, item.
- **The team and the period on every link.** Picked in a bar at the top
  of the pages that show them (`team=` on the overview, the rules, the
  incidents and the tasks; `weeks=` 1, 4 or 12 on the rules and the
  incidents, which now take one week too), and carried by every link
  between pages. With no `team=`, a reader who edits one team, and only
  one, sees that team's, and the bar says it is theirs; `team=` alone is
  all teams.
- **Search**, `/search?q=`, in every page's bar: rules by name, incidents
  by id or title, teams by name, fifty of each at most. One found is
  opened, but a rule or incident named `.` or `..`, which a redirect would
  make another page; a search with a rules filter is the rules page with
  it; nothing found says so.
- **The rules list's badges**, on three axes and what is under way:
  ● Noisy (with its weeks running), ○ OK, – Silent, ⧉ Spare; ≠ Differs,
  ◌ Not replayed, ⇣ Recorded only; keep, tune, retire; ✎ Proposed,
  ⚠ n to check. Each a sign and its name.
- **Filters on the rules list** by performance, severity, trust,
  disposition and progress (`perf=`, `severity=`, `trust=`, `status=`,
  `has=`), each value counted as picking it would show, and a search box
  that takes words of a rule's name and `team:`, `severity:`, `is:`,
  `status:`, `has:`, `label:KEY=VALUE` and `group:`. Those a facet has
  become its parameters, by a redirect; what the box doesn't understand is
  named and left out. `key:"a value"` takes a value with spaces. Each
  filter in effect is a chip that takes it away; an order keeps every
  filter; "Clear filters" keeps the team, the weeks and the order; the
  header says "14 of 63 rules". A severity is the rules' own spelling,
  whatever its case in the URL.
- **A page's main action in its header**: "Write one down", "Add a task",
  "Add a quiet window", "Add a team", "Add a report". To a reader who may
  not, the button shows disabled, with why: read-only, no one signed in,
  editors only, admins only, nowhere to send a report.
- Empty states of three kinds, alike on every page: nothing judged yet,
  nothing matching (with the ways out), nothing to do.

### Changed

- **`/incidents?weeks=1` shows the last week.** It showed four, as every
  `weeks=` but 12 did.
- **With no `team=` in the URL, a reader who edits one team, and only one,
  sees that team's** on the overview, the rules, the incidents and the
  tasks, where they saw every team's. `team=` shows every team's; the API
  and MCP take a team only when asked, as before.
- **A page asked with a query of more than 8192 bytes is refused, 414**,
  before anything is read for it. A search (`q=`) of more than 500
  characters is left out, and the page says so; of its words, the first
  20 are used and the rest named; a filter (`perf=`, `severity=`,
  `trust=`, `status=`, `has=`) takes 20 values. Several `q=` are one
  search.
- An error page (403, 404, 414, 500) shows no count of open tasks or
  proposals, and carries no team or period on its links.

### Fixed

- **An error page, and the page after signing out, showed how lapilli
  serve's runs were going, and its version, to anyone (low).** A stranger
  refused (403), a page not found, and the signed-out page, which anyone
  may open, had the status line: what is being done, a failed run's error
  as the API gave it (a host inside among it), whether changes are
  paused, and the version. They have none of it now.
- **Signing in from a page asked with a long query failed.** Where to go
  back to was kept in the sign-in's cookie whole, and past about 2.9 KB of
  it the browser dropped the cookie: the sign-in "didn't start here". Past
  2048 bytes the page is gone back to without its query.
- A task's change of status, and its deletion, come back with the team and
  the period, as adding one does.
- The rules list's Rule column has badges for the sentences it had under
  a rule's name; the note's status is a badge, and the Noisy column is
  gone. The suggestion stays, as one line.
- The team is picked in the bar alone: the overview, the incidents and the
  tasks no longer have a row of team links of their own.
- The rules that didn't fire in the weeks shown, listed below the rules
  list, include those that fired in the twelve weeks kept but not in the
  weeks shown, which weren't on the page at all.
- The rules list, and `/api/v1/rules`, work out each rule's weeks with a
  re-fire for that rule alone: over two thousand rules and twelve weeks,
  330 ms became 60 ms.

## [0.34.1] - 2026-11-14

A security release, after two reviews of 0.33.0 (whose code 0.34.0
holds) from an attacker's point of view: of what comes in to lapilli serve's own sign-in, and of what the
APIs lapilli reads (a history, a ruler, a Grafana, a cloud, an on-call
feed) can make it hold, wait for, or say; and a third review of the
fixes, whose findings are fixed in what is described here. Each fix comes
with a test that failed before it. No ledger schema change. Read "Before
you upgrade".

### Fixed (security)

- **A bearer token outlived the record of its person signing out.** An ID
  token sent as a bearer token was taken until its own `exp`, which the
  identity provider sets (Dex's are 24h), while who signed out is kept for
  `--session-max-age` (12h): a token copied before its person signed out
  was taken again once the record was pruned, at a restart or when anyone
  else signed out. A bearer token is now refused once it is older than
  `--session-max-age`, as a session's cookie is.
- **Strangers signing out could push an admin's sign-out out of the
  record.** `POST /auth/logout` needs a session cookie, not a role, and the
  identity provider gives one to whoever it signs in; the record holds
  10,000 people, and dropped the oldest. With a provider people sign up
  to, 10,000 sign-outs under names of one's own made a copy of an admin's
  cookie, refused since they signed out, good again. Only people a role
  lets in are kept now, and a full record fails closed: the one let go of
  raises a time for everyone, and every session and bearer token made
  then or before is ended.
- **Signing out wrote the ledger for anyone, each time.** With no role, no
  rate and no look at whether the session was still on, under
  `--read-only` too, each POST rewrote the record (440 KB, full), and
  every request read it back. A session signed out of already now writes
  nothing (its cookie is still taken away), nor a stranger's, and a
  request reads the record again only when the ledger counts a change to
  it. Under `--read-only` a sign-out of someone a role lets in is kept:
  signing out must work, and SECURITY.md now says so.
- **Each `GET /auth/login` left a cookie, and none was taken away.** A page
  elsewhere could send a browser there again and again; past 180 cookies a
  site, browsers drop the least recently used, the session's among them.
  A login now takes away the sign-ins under way the browser holds but the
  newest three.
- **A Grafana that never answered held lapilli serve's work loop
  (medium).** The weekly proposals read the rules from Grafana again with
  no deadline, through a client with no timeout: every hourly run, digest,
  change and report waited on it. A read of the rules gets a minute,
  whatever starts it, and the client to Grafana gives up after a minute,
  on its headers after 30 seconds.
- **Replayed alerts' labels were kept as an API answered them (medium).**
  The ledger kept the labels of recorded firings, notifications and
  incidents to 4 KB, but not those of the firings it replays: a hostile
  API's few KB on the wire, gzipped, were tens of MB in the ledger and its
  cache, more each run. They are kept as the others now: 4 KB in all, a
  name of 128 bytes and a value of 256, cut past that with a hash of what
  was cut, a value with what turns text around in it likewise, and a set
  still past 4 KB with its longest labels folded into one hash
  (`__lapilli_cut__`, which an API's own label of that name is folded
  into too, as is one named as another is kept), so alerts apart stay
  apart. lapilli serve's replay check takes what really fired as it would
  be kept. A rule's replay in a run keeps 20,000 alerts at most, told
  apart by their labels (by a hash seeded as the process starts, which an
  API can't make two sets share): past that the rule is left as it was,
  as one whose query failed, and says why. The cache of ids and labels
  holds 32 MB at most.
- **A Prometheus's configuration could take hundreds of MB (medium).**
  `/api/v1/status/config`, a read lapilli does without, could be 256 MiB
  once its gzip was taken off, and its YAML was parsed whole, some 90 times
  its size: 4 KB on the wire took 360 MB. An answer over 2 MiB is refused
  before it is parsed now, and the active targets over 32 MiB; either
  said as a warning in lapilli serve's log, and on lapilli verdict's
  stderr, as the range check is off then.
- **A ruler's or a Grafana's rules took some 280 to 350 times their size
  (medium).** A list was read whole into YAML nodes, written out again
  and parsed into rules: 4 MiB of small rules took over a GB. A list, or
  a Grafana's export, is still 64 MB at most, but read at a value every 4
  bytes at most, past its first 100,000, and 6 million in all (counted
  before it is parsed: real ones are a value every 5 to 15 bytes), and of
  50,000 rules, counted before they are written out again (a list that
  doesn't parse whole too) and as read; each group is written out on its
  own, of 100,000 values at most (a namespace with one larger is passed
  over, as one that can't be read is, and so is one written whole, in
  flow style, of more), and a document's nodes let go of as they are
  read. What a ruler's API, a Grafana's export or a cloud's API
  writes holds no YAML alias, and one that does isn't read: a ruler's
  namespaces were each a document, their aliases expanding on their own,
  and 99 KiB read as 230,400 rules in 306 MiB. Those 4 MiB of rules are
  refused in 133 MiB; 40,000 rules in 2,000 groups, 1.7 MiB, read in
  132 MiB (579 MiB before); a Grafana export of 25,000 classic rules,
  62 MB, in 1.4 GiB (1.8 GiB before); and what a hostile list or export
  can be made to take within the bounds is some 85 times its size, and
  1.9 GiB, at most.
- **A range answer was kept whole, whatever it held (medium).** Samples
  outside the range asked, more than its steps, and any number of series
  were kept, and the 256 MiB an answer may be counted only once its gzip
  was off: 24 KB on the wire held 360 MB. An answer is read as it comes
  now, and only what was asked kept: a sample outside the range dropped; a
  series of more samples than the range's steps, or taking more of the
  stream than they could need, unreadable; past 50,000 series or
  5,000,000 samples, too big, and asked again over a shorter range, as
  when the API says so; past 128 MiB of labels (50,000 series of 2.7 KB
  each; a series of tiny labels repeated gzips to nothing), too many or
  too wide series to read, over any range. The answers to one range, its
  days and the halves the API asks for, hold 100,000 series, 256 MiB of
  labels and `--max-samples` samples (10,000,000 by default) in all at
  most, and past one the rule isn't replayed, said with the bound and how
  to read it all the same: an API saying every range of more than a step
  is too large, and answering each step with series of its own, had a day
  held as 72 million series. Answers read so are some 1.5 times faster, in
  half the memory.
- **A mapping of many keys took time with the square of its keys
  (medium).** A rule document is decoded with go.yaml.in/yaml/v3, which
  checks a mapping's keys for duplicates pair by pair: a rule of 96,000
  label keys, a 1 MiB answer from a ruler or a Grafana (or a cloud's
  alarms, as JSON is YAML), held a CPU for 20 seconds, past any
  `--timeout`; 64 MB, for hours. Documents are decoded to their nodes,
  which takes time linear in them, and then to values as yaml.v3 would
  (fuzzed against it: the same, but near yaml.v3's bound on what aliases
  expand to, which is kept, counting the keys a merge brings in again); a
  mapping of more than 10,000 keys, or values nested deeper than 200, are
  refused, and the context is looked at between documents and within one.
  What aliases expand to is bounded as yaml.v3 bounds one document's for a
  whole input: a stream's documents, a directory's files, the rule files
  a ConfigMap or an Amazon namespace holds, together. A fault in reading
  a document is an error, not a crash. A ruler's unit, a cloud's alarm
  and the hash of either are decoded so too. Rule files read a little
  faster.
- **An on-call feed could hold a CPU, with no deadline (low).** Replaced
  occurrences (`RECURRENCE-ID`) and a repeat's days (`BYDAY`) were
  searched each step, and a `TZID`'s zone loaded for each date: feeds of
  2 to 8 MB took 13 to 20 seconds and more, past a deadline as lines were
  read without a look at it. They are looked up now, a zone loaded once a
  feed (1,024 zones a feed at most, more than IANA has), the lines and the
  repeats read with a look at the context, and each feed read and parsed
  within a minute.
- **Two errors quoted an unreadable answer as the server wrote it
  (low),** a result type that isn't a matrix or a sample that isn't a
  number, without taking out the token, as every other error of the API's
  does; `--markdown` puts that reason in a pull request's comment. Now
  taken out, before the text is cut, so a long token's start doesn't
  stay; and from an answer that isn't the API's, said from its first
  4 KiB, the start of a token cut there.
- **What a ruler, a cloud or Grafana answered with an error kept a token
  it echoed (low),** on every page's status line, in a change's reason and
  in Slack's `change.failed`. The token, signature or credential the
  request carried, and any bearer token, are taken out as from a history
  API's errors. IAM Credentials' refusal, and the source of a namespace a
  ruler's read passes over, are said without what doesn't show.
- **One slow data source behind a Grafana held every place (low, 0.33.0).**
  The data sources behind one Grafana share `--parallel` places, held as
  long as a request is: a slow one, as a broken or hostile Loki behind it
  can be, took them all, and a healthy one's queries waited for its
  answers. One holding places doesn't take the last while another holds
  none that asked within ten seconds (looked at again when that ends, not
  only when a place is given back), and a place given back goes to the
  one waiting that holds fewest.
- **A direction override in a rule's name turned text on the pages around
  (low).** A name, a group, labels and what a rule says are read with
  what doesn't show as written or turns the text around it (a control
  character, a direction mark, embedding, override or isolate) as U+FFFD,
  as a tracker's comment is taken, from every source, its group's labels
  too; replayed alerts' labels are kept so too. An expression isn't
  changed, as its query is its own, but is shown so on the pages, and the
  pages set names apart in `<bdi>`, those in a sentence too.
- **`--metrics` wrote an escape sequence of a name as it was (low).** A
  name an API gave reached stdout raw, an OSC 52 sequence setting the
  terminal's clipboard among them. `--metrics` and serve's `/metrics` say
  a control character, and a direction mark, embedding, override or
  isolate, as every other output does (`\u{1b}`); the joiners names in
  Persian, Indic scripts and emoji are written with are as they are, as
  `ALERTS` has them.
- `*@DOMAIN` and `--oidc-hosted-domain` compare domains by their ASCII
  letters alone: Unicode's folding takes the Kelvin sign for a `k` and a
  long s for an `s`.
- A group holding a character that doesn't show (a zero-width space, a
  direction mark) is dropped, from a token, a signed header or
  `--groups-header`, as docs/design/signin.md said: before, only a token's
  spacing dropped one.

### Changed

- `--max-samples N`, for `lapilli verdict` (`--compare` and
  `--check-replay` too) and `lapilli serve`, and the chart's
  `maxSamples`: the most samples the answers to one range, a rule's query
  over the period, may hold in all (default 10,000,000, some 600 MB at the
  peak, 64 bytes a sample).
- The weekly fuzzing in CI takes five targets more: range answers, rule
  documents' values against yaml.v3's, on-call feeds, a JWKS, and a load
  balancer's signed header.

### Before you upgrade

- **Bearer tokens older than `--session-max-age` (12h by default) are
  refused**, whatever their `exp`. An API client or an agent holding a
  token of a provider that gives longer ones gets another sooner.
- **Signing out.** Only people a role lets in are kept as signed out: a
  person with no role who is given one later is let in by a cookie of
  theirs from before they signed out, until it expires. A record full of
  10,000 people within `--session-max-age` signs everyone out who signed
  in before the oldest of them: with `--viewers '*'`, or a provider people
  sign up to and a broad `*@DOMAIN`, 10,001 accounts signing in and out
  within it, again and again, sign everyone out, which takes an account
  each, and is a way to keep people signing in again, not a way in. Under
  `--read-only`, signing out writes the ledger's record of it. Two
  processes sharing a ledger should both be 0.34.1: an earlier one's
  sign-outs aren't counted, and a 0.34.1 doesn't see them until it starts
  again.
- **New bounds on what is read.** They may refuse an answer, or cut what
  is kept of it:
  - a ruler's list, a Grafana's export: 64 MB (as before), 50,000 rules
    as written and as read, a group of 100,000 values, and no YAML alias
    (nor in a cloud's alarms);
  - a rule document, from anywhere (rule files, a ruler's unit, a cloud's
    alarms): a value every 4 bytes past its first 100,000, 6 million in
    all, a mapping of 10,000 keys, values nested 200 deep, and aliases
    expanding as far as yaml.v3 allows one document, for a whole input
    (a stream's documents, a directory's files) together;
  - a range answer: 50,000 series and 5,000,000 samples, then asked over
    a shorter range; 128 MiB of labels, then not read; samples outside
    the range asked are dropped; the answers to one range together:
    100,000 series, 256 MiB of labels and 10,000,000 samples;
  - a replayed alert's labels: a name of 128 bytes, a value of 256, 4 KB
    in all, cut with a hash past them, as is one with a direction mark.
    An alert whose labels are cut is another series in the ledger from
    the upgrade on, and its earlier firings are replaced as the runs
    reach them. A rule's replay in a run: 20,000 alerts;
  - Prometheus's configuration 2 MiB, its active targets 32 MiB: past
    them, read as when it isn't served, and said;
  - an on-call feed: 1,024 time zones.
- **A rule's query over the period reads 10,000,000 samples at most**
  (`--max-samples`): 1,000 series firing all through a week at a minute,
  250 through 28 days. Past it, the rule isn't replayed, as one whose
  query failed, and the reason says so; 0.34.0 read it, in some 64 bytes
  a sample at the peak. Give a larger `--max-samples` (the chart's
  `maxSamples`), and memory for it times `--parallel`; or a longer
  `--step`, or a shorter `--since`.
- **A rule whose name holds a control character or a direction mark** is
  read with U+FFFD in its place: a new rule in the ledger, and a change
  proposed to it isn't found where the rule is written.

## [0.34.0] - 2026-11-14

The ledger where a team keeps the rest of what matters: in a Postgres
database, with its backups, failover and SQL, beside SQLite, which stays
the default. Still one replica; what more would take is in the design
note. Reviewed three times, as an attacker would, for correctness and
concurrency, and the fixes once more; what each found is fixed in what is
described here, as are two things CI's Postgres runs found (a server that
stops answering, which nothing waited on for long enough, and a page's
round trips). No change to a SQLite ledger's schema or files.

### Added

- **The ledger in Postgres** ([docs/design/postgres.md](docs/design/postgres.md)):
  `lapilli serve --ledger-url-file FILE` keeps the ledger in the Postgres
  database (15 or later) a connection string names, read from a file and
  said nowhere, in no flag, log, error, page, `/metrics` or MCP answer.
  To another machine, a string that says no `sslmode` (pgx's `prefer`) is
  refused, saying to ask for `verify-full`; one that checks no certificate
  is warned of at start. The string is read by pgx alone, as `psql` reads it.
  The same tables and statements as SQLite's, every text compared byte by
  byte, every key of any length and every label kept, so every page and
  answer is what SQLite gives; the ledger's and serve's tests run on both,
  on Postgres 15 and 18 in CI. Still one replica: the process that writes
  holds an advisory lock on the ledger's schema, a second is refused,
  saying which session and role hold it, and one whose connection dropped
  takes it again, or stops, and sends nothing more, if another process
  took it in between. SQLite stays the default, its files as they were:
  upgrading changes nothing there.
- **`lapilli ledger copy`**: a ledger copied into an empty one, a SQLite
  ledger into Postgres or one back, in one transaction, ids kept and no
  deleted id given again, both read back table by table and checked
  before it commits, the ledger copied from read as of one moment. A
  ledger that isn't empty is refused.
- **The chart**: `serve.ledger.postgres.secret` names the Secret holding
  the connection string, mounted whole so it can hold the server's CA; no
  PersistentVolumeClaim is made, and with the egress NetworkPolicy,
  `serve.ledger.postgres.to` says where the database is.

## [0.33.0] - 2026-11-13

Reading history from an API with a request quota, as a managed Prometheus
has one. Five `lapilli verdict --compare` runs at once against one were
answered HTTP 429, and three rules ended "Could not replay": the retries
gave up within 30 seconds, nothing sent fewer requests when the API pushed
back, and a run of eight minutes said nothing until its end. Fitted to
0.32.0's pull request backtests: one line between a history that couldn't
be read and a query it refused, drawn alike for `--fail-unreplayed` and for
the comment. Reviewed three times, as an attacker would and for correctness
and concurrency; what each found is fixed in what is described here. No
ledger schema change; the exit statuses, and every field `--json` gave, are
as they were, `--fail-unreplayed` aside.

### Added

- **`--max-rate N`**, for `lapilli verdict` and `lapilli serve`: each API
  the history is read from (`--prometheus`, `--loki`, its ruler included,
  and a Grafana's data sources together) is asked N requests a second at
  most, evenly spaced; `0.5` for a quota of 30 a minute. N is 0.001 at
  least.
- **A progress line.** A `lapilli verdict` run past 15 seconds says on
  stderr, every 30 seconds, the rules replayed of all (in a comparison, the
  replays before and after), the requests to the history, those asked again
  after a wait and those answered HTTP 429. `--progress=false` keeps it
  quiet. A run the history answered HTTP 429 says so at its end either way.
  stdout is unchanged. The backtest recipes copy stderr to the job's log as
  it is written, so a long backtest no longer looks hung; none of the
  progress reaches the comment.
- **`--fail-unreplayed`**: `lapilli verdict` exits 1 when a rule's replay
  failed as the history couldn't be read: not reached, not let in (HTTP
  401, 403, 407, credentials that couldn't be had), not found (404, a
  redirect to sign in, a proxy's or a gateway's page), or turned away to the
  end (408, 429, 5xx). Not for a query the history refused as written, nor
  for a Grafana rule replayed whose query outside its condition failed,
  which the report notes as before. Without it the exit is as before: 0, or
  1 when `--timeout` cut the run short. The backtest recipes refuse it:
  `FAIL_ON_ERROR=true` fails the job on every rule that differs that
  couldn't be replayed, whatever why, as before.
- `unreplayed` in `--json`, of a report and of a comparison: each rule that
  couldn't be replayed, its side in a comparison (none for a rule alike in
  both, replayed once for both), why, and `failed`, true when its replay
  failed for the run (the history unread, the time run out) and another run
  may replay it; false for a rule no run replays (a CloudWatch metric alarm,
  a rule paused, a query refused as written). In a comparison, the rules of
  it with a side are those `summary.unreplayed` counts, each once.
- `--compare --markdown` says, under its first line of counts, how many of
  the rules that differ couldn't be replayed, as `summary.unreplayed` does,
  since what follows leaves them out; the list under the tables says why, as
  before.

### Changed

- **Two kinds of failed query**, which a rule's reason says.
  `query failed: the history couldn't be read: ...` is a failure of the
  run, as above: `--fail-unreplayed` counts it, the text report names it at
  its top, and `--markdown` says only that, its whole reason going to
  stderr. `query failed: ...` otherwise is the history's answer to the query
  as written, which every run gets: refused in the API's own words (HTTP
  400 or 422), too large however short the range, not done (501), or an
  answer that can't be read. Both begin `query failed: ` as before, so what
  reads reasons by that, as the recipes' filter of the job's log does,
  reads them alike. JSON of another shape than the API's, such as a
  gateway's `{"message": ...}`, is said as `not a Prometheus API response`,
  as a page is.
- **Asked again after a wait the API says.** An answer of HTTP 429, or 503,
  with `Retry-After`, in seconds or as an HTTP date, is asked again that
  long after, a second at least and two minutes at most, plus up to a
  quarter so that requests told alike don't come back together, as often as
  it says so. A 429 without it is asked again after 4, 8, 16, 32, 60 and 60
  seconds, each less up to half, 90 seconds to 3 minutes in all, past the
  window of a quota by the minute; before, 2, 4, 8 and 16 seconds, 30 in
  all. A 5xx, or a connection the API dropped or reset before it answered,
  is asked again as a 5xx was, each wait less up to half; a 501 isn't. 429s
  and 5xx are counted apart. An answer too large to send is halved as
  before, however long the API's text that says so.
- **Five minutes at most, and within `--timeout`.** A request is asked again
  for five minutes at most from when it was first pushed back, by an answer
  or by a wait for its turn the API asked for; the API's answers and the
  waits for a turn count. A request whose turn doesn't come by then is given
  up with the last answer; one already asked is waited for. A wait that
  would end past `--timeout`, the API's or a turn by `--max-rate`, isn't
  waited for: the rule is left unfinished at once, as the deadline would
  leave it, and the run exits 1 saying to raise `--timeout`; `lapilli
  serve`'s MCP compare says so too.
- **Fewer at once after a push back.** A 429, a 503 or a connection dropped
  halves how many requests may go to that API at once: half of those in
  flight lately, or of the limit, if fewer, from 64, once a second at most,
  for the whole run (or, in `lapilli serve`, from then on), down to one;
  eight at once are four after one push back. One more may go for every
  ten seconds without a push back, asked or not. While it said to wait
  (`Retry-After`), nothing else is asked of it, a request already waiting
  for its turn included. Rules sharing a query still share one request.
- **`--parallel` is of each API.** Up to N rules are replayed at once against
  each: `--prometheus`, `--loki`, each of Grafana's data sources. Rules held
  by one API that pushes back no longer hold those of another. A Grafana is
  asked N requests at once at most for all its data sources, as one
  Grafana answers them all.
- Behind one Grafana, each data source backs off of its own, as Grafana's
  data source proxy passes on what each answers, `Retry-After` included;
  they share `--max-rate`.
- The same holds for every API history is read from: Prometheus, Thanos,
  Mimir, Cortex, VictoriaMetrics, each cloud's managed Prometheus and
  CloudWatch through `--auth`, Loki, and Grafana's data source proxy; and
  for the rules read from the same API (`--loaded-rules`, where a Loki's
  ruler runs its groups). What lapilli does without, the configuration and
  the targets Prometheus is asked for, and where a Loki's ruler runs its
  groups as `lapilli serve` reads it each minute, is asked once, within 30
  seconds, not while the API said to wait, and its answers don't change how
  the rest is asked, nor count as answers of HTTP 429 at a run's end.
  `lapilli serve` keeps where the groups run as last read when it can't be
  read, so a Loki pushing back no longer stops the rules being read. A
  request to a Loki, or to a Grafana's data source, is given up after five
  minutes, as one to a Prometheus was.
- A rule whose replay failed for the run is named, with why, at the top of
  the text report and of a comparison, ten rules at most, as well as at its
  foot. A failed query says how often it was asked, and over how long.
  `lapilli serve` keeps the reason in its ledger without that, and its
  weekly digest, whose numbers keep what a rule fired before its replay
  failed, names no rule at its top.
- The API's text in an error is cut to 300 bytes, and in a top line to 400,
  each with its "…"; what doesn't show (control and format characters) is
  taken out first, then the token, the signature or the URL's password
  lapilli sent, and any bearer token, should the API echo them back, split
  or not; "bearer" before a word, as in "missing bearer token", is left. A
  sample the API writes unreadably is quoted in the error.

## [0.32.0] - 2026-11-13

Pull request backtests in CI, on every code host: what a team had to glue
around `lapilli verdict --compare` to comment its report on a pull request
that changes alert rules, built in. Reviewed as an attacker would and for
correctness before release; what both found is fixed in what is described
here. No ledger schema change; the exit statuses, and every field `--json`
gave, are as they were.

### Added

- **Pull request backtests in CI** ([recipes/backtest](recipes/backtest)):
  a script for each code host, GitHub, GitLab, Bitbucket, Azure DevOps and
  Gitea (or Forgejo), one POSIX sh library under them, and for GitHub
  Actions an action (`uses: lapilli-project/lapilli/recipes/backtest@...`).
  Each installs a pinned release, its archive checked against the
  release's `SHA256SUMS` (and `LAPILLI_SHA256`, which a mirror needs)
  before it is unpacked; reads the rules of the commit the pull request
  goes into and of its own from git, as files (`PATHS`) or as a command
  renders them in each (`RENDER`, `RENDER_PREPARE`: a Helm chart, a
  Kustomize overlay), the base's first, each with a `HOME` of its own and
  without the variables CIs keep tokens in; reads the incidents from the
  commit the pull request goes into (`INCIDENTS`), never the pull
  request's, and refuses a file an argument names anywhere in the
  checkout; takes the branch a pull request goes into from the clone when
  the checkout fetched it, so a checkout without credentials does; runs
  `lapilli verdict --compare --changed-only --markdown`; and posts one
  comment, found again by a hidden first line and the token's own account
  (on Bitbucket, `BITBUCKET_USER_UUID`), and edited on later runs. Nothing
  changed and no comment yet: none posted. What fails is said in the
  comment, in a fenced block, nothing of the pull request's outside it,
  taken out where the history is before it is cut, never why a rule
  couldn't be replayed or what a server or the network said; the job
  succeeds unless `FAIL_ON_ERROR=true`, which a rule that couldn't be
  replayed fails too. A `PATHS` neither commit has fails the job. What it
  copies to the job's log is each line after a `| `, so none of it is a
  CI's command. Answers are capped whatever curl's version. Tested against
  a stand-in for each host's API, and end to end against a real Gitea and
  Prometheus (`test/e2e/backtest.sh`, in CI): a comment posted, edited
  after a push, none on a pull request that changes no rule, a rule named
  to mention, reference and break the table rendered by Gitea itself as
  text, a link whose path mentions a user refused without notifying
  anyone, from a checkout without credentials.
- **`lapilli verdict --compare --markdown`**: the report as Markdown to
  post on a pull request or a merge request: a heading with the period and
  its time zone, the totals, the rules that differ as a table, the
  recording rules that differ, those that couldn't be replayed (a line a
  rule), those that fire as they did folded in `<details>`, the incidents
  caught differently, what reading the files passed over; at most 64,000
  bytes, cut short with a note that counts rules. Rule names, labels,
  incidents and reasons are written as code spans, so none can format,
  link, mention (`@user`, `@org/team`), reference (`#123`), break a table
  or hide what follows; a name is cut before anything is made of it. A
  query that failed for want of reaching the history (anything but the API
  itself refusing it, 400 or 422 in its own words, not a proxy's page) says
  only that, its reason going to stderr; from the rest, the URLs, user
  names, hosts (each as a whole word, a full stop after it too), IP
  addresses, tenants and project the history is read with are taken out. When nothing changed, a short form that doesn't
  change. It doesn't go with `--check-content`.
- **A summary for tools.** `--json` with `--compare` adds `"summary"`:
  `nothing_changed`, `changed`, `added`, `removed`, `same`, `alike`,
  `unreplayed`, `recording_changed`, `firings_before`, `firings_after`,
  `incidents_given`, `incidents` and `incidents_changed`, each always
  there; `--markdown` holds the same on its first line. "No incidents
  given" is `incidents_given: false`, not zero caught. Nothing changing has
  no exit status of its own (the README says why).
- **Recording rules in a comparison.** `--compare` reads the recording rules
  beside the alert rules, wherever Prometheus's groups are, in Azure rule
  groups and in Grafana provisioning files, and one that differs
  (`"recording"` in `--json`) is a change: `nothing_changed` is false. A
  replay reads what a recording rule recorded, from the history, so the
  report says the alerts that read one are replayed against that, not what
  the change would record.

### Changed

- **`--compare` runs without incidents.** Without `--incidents` or
  `--impact` the firings are counted, not judged: the report leaves out the
  caught, nothing and precision columns and what it would say of incidents,
  and keeps fired and re-fired. A replay without `--compare` still asks for
  them.
- **A Prometheus group's `labels` and `limit` are each of its rules'.** The
  alerts a replay gives carry its labels under the rule's own, as
  Prometheus 3 does, and a change to either is a change to its rules, in a
  comparison and as a new version in lapilli serve. A limit isn't replayed
  (past it, Prometheus drops all of a rule's alerts): a rule under one is
  marked replayed in part. A rule's name, and in lapilli serve its
  versions, history and proposals, are as they were: an upgrade makes no
  new version, and a change proposed before it goes on (tested on a ledger
  v0.31.2 wrote). The rules API's group `limit` is read too.
- **A Grafana recording rule is no alert rule.** One with `record`, in a
  provisioning file, was read as an alert rule; it is now a recording rule,
  compared as one. lapilli serve, reading a Grafana with recording rules,
  stops listing them among its alert rules: one new version, on upgrade.
- A report's header names its time zone with each offset the period took,
  `Asia/Seoul (+09:00)`, `Europe/Berlin (+01:00 → +02:00 → +01:00)`, or
  `UTC` by any of its names (`Etc/UTC`, `Etc/Zulu`), never Go's bare
  `Local`: the machine's own zone is named as `TZ` or `/etc/localtime`
  says, through every link; a zone file copied, by its abbreviations. `--tz`
  takes an IANA name, as it did. lapilli serve's digest header too.
- With `--compare`, what reading the rule files passed over is said for
  each set apart: "the rule files --rules read held ..." and "the rule
  files --compare read held ...", not the same line twice.
- `lapilli verdict`'s errors show a line break, or any control character,
  in what a rule file or a flag holds as `\u{...}`, and the names of rules,
  groups, alarms and namespaces quoted: no line of an error is one of its
  own, which a CI could read as a command.
- A file of incidents is read no further than 64 MiB.

## [0.31.2] - 2026-11-13

SQLite 3.54.1, held back since 0.20.1, and a week of firings written
faster for it. No ledger schema change.

### Changed

- **SQLite 3.54.1** (`modernc.org/sqlite` v1.62.0, from v1.60.1 and
  SQLite 3.53.4). 0.20.1 held it back: with v1.61.0 a week of 100,000
  firings copied in was written a fifth slower. The driver was why, not
  SQLite or the ledger. It cleared a prepared statement's arguments after
  each run, and SQLite 3.54 then planned a statement with a `col = ?` term
  and an `ORDER BY` again at its next run, as it does the query for an
  alert's firing before those being judged: run once for each alert,
  about 99,000 times for that week, at twice the cost. v1.61.1 keeps the
  arguments in place (https://gitlab.com/cznic/sqlite/-/issues/264). With
  v1.62.0 the week is written in 1.98 s, against 2.15 s with v1.60.1 and
  2.68 s with v1.61.0 (medians of 20 runs on a laptop), and twelve weeks
  are backfilled a tenth faster. No ledger schema change.

## [0.31.1] - 2026-11-12

Two fixes the new end-to-end run found (`test/e2e/service.sh`: the whole
service on kind, with Prometheus, Grafana sending its notifications to
lapilli, Dex and a load balancer, and a browser). No ledger schema change.

### Fixed

- **`lapilli serve` didn't start with a relative `--ledger`** (since
  0.29.0), its default `lapilli-ledger` and the README's first example
  among them: written as a URI, the path's first directory was read as a
  host, and SQLite refused it (`invalid uri authority`). The path is made
  absolute first, and a Windows path keeps its drive in the path. The
  chart, which gives an absolute one, wasn't affected.
- A team or a comment written through the API was answered as it was
  sent, not as it was kept: no author, a time of 0001-01-01, a team's name
  untrimmed. It is answered as `GET` reads it back, as a quiet window, a
  task, a report and a role already were.

## [0.31.0] - 2026-11-12

What an operations team asks of its alerts (docs/design/noc.md): re-fires,
firings during deploys, user impact read as incidents, week by week and team
by team, in the UI, the digest, `/metrics`, a new summary API and the
chart's Grafana dashboard. **Ledger schema 017**, after 0.28.1's 016: tables
of deploys and of their label sets (each kept once), a mark on each firing
that came during one, counted by day and week, and an index of re-fired
firings. No change to how a firing is judged. Reviewed before release as an
attacker would and for correctness; what both found is fixed in what is
described here (docs/design/noc.md, "Settled in review").

### Added

- **Re-fires, read as a goal.** `/rules` says the share of each rule's
  firings that re-fired and in how many of the whole weeks shown one did,
  and can be put in order of re-fires (`?sort=refired`); a rule's page has
  its re-fires week by week and the alerts (label sets) that re-fired most;
  the overview a tile and a column; the digest and a report's new `refired`
  section, how many re-fired, of how many rules, the alert that did most,
  and how many of the last twelve whole weeks had one. "Whole weeks" is one
  thing everywhere: neither the week under way nor the one the ledger's
  first firing fell in, with how many were counted beside it. `/metrics`
  adds `lapilli_rule_refired_weeks`, `lapilli_refired_weeks` (everyone's and
  each team's) and `lapilli_weeks_counted`.
- **Firings during deploys.** `--deploys FILE` (the chart's `serve.deploys`)
  names what was deployed and when, as the histories
  `recipes/versions/argocd.sh` and `flux.sh` write (regular files beside it
  or below it), each labelled with the service or namespace it deployed, or
  written down with a start and an end; read again each run, and kept again
  only when it or a history changed (sha256). A firing that starts during
  one, from `--deploy-before` (0) before its start to `--deploy-after` (10m)
  after its end, and agrees with it on every label both have, one at least
  (a namespace's own alert during a service's deploy in it, not another
  service's), is marked: judged as ever, shown apart, and precision read
  with those firings and without, on `/rules` (a column and
  `?sort=deploys`), a rule's page, the overview, the digest, a report's
  `deploys` section, `/metrics` (`lapilli_rule_firings_during_deploys`,
  `lapilli_rule_caught_during_deploys`,
  `lapilli_serve_deploys_last_timestamp_seconds`,
  `lapilli_serve_deploys_read_timestamp_seconds`) and the summary. Bounded:
  50,000 deploys, 8 MB of labels in all, none starting more than an hour
  after the file is read. Without `--deploys`, a ledger that keeps deploys
  says so on the status (a file of `deploys: []` takes them away).
- **Impact windows read as incidents.** `/incidents` picks written incidents
  or windows `--impact` found (`?source=`) and a team's (`?team=`), says how
  many were caught, caught only by noisy rules or missed and the median
  lead, and gives each its lead and, for one not caught, the rule that nearly
  caught it. `/metrics` has `lapilli_incident_state` and
  `lapilli_incident_lead_seconds`, an incident a series (the latest 500), and
  `lapilli_incident_team`, which teams own each (500 series at most). An
  incident's state is judged by the rules' stones in its week everywhere,
  `/metrics` and so the dashboard included.
- **`GET /api/v1/summary`**, and MCP's `summary` tool: for each week and for
  everyone or each team, critical firings and the share that caught
  something (`?critical=`), by severity, re-fires and the alert that
  re-fired most, off hours and asleep, during deploys and precision without
  them, incidents and impact windows caught and missed, and the median lead.
  `schema: 1`: fields are added to it, never changed. Bounded: 53 weeks, each
  team once, 32 critical severities; one of every team, or of more than 53
  rows, takes the one heavy turn, through the API and MCP alike.
- **The chart's Grafana dashboard rebuilt** around that evaluation, with
  variables for the team, severities and the severities counted as critical
  (several at once, a rule whose versions' severities differ included), and
  the sources' health: how long since firings were copied in, a
  notification came, the deploys file was read, the ledger's runs, the
  sources gone quiet, failed runs. `test/e2e/dashboard.sh` asks every query
  of a real Grafana 13.3.1 over a real Prometheus 3.5.0 scraping a real
  lapilli serve, in CI too. `/metrics` adds
  `lapilli_serve_notifications_last_timestamp_seconds` and
  `lapilli_serve_source_quiet_seconds`. The chart's ServiceMonitor honours
  lapilli's labels and takes `serve.serviceMonitor.sampleLimit`.
- **Runbooks where teams keep them.** `--runbook-annotations` (the chart's
  `serve.checkContent.runbookAnnotations`) names the annotations a runbook is
  read from, the first a rule has: `runbook_url`, `runbook`, `playbook_url`
  and `playbook` by default, kept first past 32 annotations. A path in the
  rules' repository, with an anchor or not (`docs/runbooks/disk.md#full`),
  is a runbook, shown as a path, or a link under `--runbook-base URL` (the
  chart's `serve.checkContent.runbookBase`); what teams write in a runbook's
  place (`N/A`, `TODO.`, `...`) isn't one.

### Changed

- The rule metrics (`lapilli_rule_firings` and the rest, from lapilli serve)
  carry `team` when a team owns the rule. A query summing by `rule` and
  `severity` reads as before; a rule's series changes when its team does.
- The content check's `runbook_not_url` says "a runbook that isn't a link: an
  http or https URL, or a path in a repository"; a path is no longer one. A
  link with a user name (`https://wiki.example.com@evil.example/`) is one,
  and is never shown as a link.
- Before its first run, `/metrics` gives no `lapilli_last_run_timestamp_seconds`
  or `lapilli_serve_cursor_timestamp_seconds` (they were 0, or the year 1).

## [0.30.0] - 2026-11-12

lapilli serve served the way Grafana and Argo CD are, at a URL people
open, behind a load balancer, an ingress or a Gateway, signing them in
itself ([docs/design/signin.md](docs/design/signin.md)). Reviewed from an
attacker's point of view and for correctness before release. No ledger
schema change.

### Added

- **A sign-in of its own, with OpenID Connect.** `--oidc-issuer`,
  `--oidc-client-id` and `--oidc-client-secret-file` sign people in with any
  provider (Google, Microsoft Entra ID, Okta, Keycloak, Dex, AWS Cognito, an
  OCI IAM identity domain, GitLab...): the authorization code flow with
  PKCE, state and nonce; the ID token's signature, issuer, audience, `azp`
  (`--oidc-trusted-clients`), expiry and nonce checked, Google's `hd` with
  `--oidc-hosted-domain`, and `email_verified` asked of an `email` name. A
  browser keeps a session in a signed cookie (`__Host-`, HttpOnly, Secure,
  SameSite=Lax; `--session-max-age`, 12h; signed with
  `--session-key-file`, or a key made for the process: never kept in the
  ledger). `POST /auth/logout` ends every session of that person made
  until then, in every browser, and every token issued before, kept in the
  ledger. An API client or an agent sends an ID token the provider gave the
  client as a bearer token (`--oidc-bearer=false` takes none); it never
  approves a change, and a refused one gets `error="invalid_token"` with
  the reason. `/.well-known/oauth-protected-resource` (RFC 9728) names the
  provider. `--user-claim` (`email`) and `--groups-claim` (`groups`) give the
  roles.
- **Only those a role names come in.** An identity provider signs in
  whoever it knows, which for a public one (Google's for any account,
  GitLab.com, GitHub through Dex, a multi-tenant Entra ID app) is anyone.
  With a sign-in of lapilli's own, every page and API call but the health
  checks needs someone signed in whom `--viewers`, `--editors`, `--admins`,
  `--approvers` or a role given in the UI names; everyone else gets 403.
  Lists take people, `group:NAME` and `*@DOMAIN`; `'*'` gives a role only
  once `--viewers` says who comes in. `--viewers` works with
  `--user-header` too. There is no anonymous viewing with a sign-in.
- **A sign-in in front, by the header it signs.** `--proxy-jwt google-iap`
  (Google Cloud's Identity-Aware Proxy), `aws-alb` (an Application Load
  Balancer's sign-in) or `jwks` (oauth2-proxy, an Azure App Service's
  sign-in, an access proxy) takes who each request comes from from the JWT
  that sign-in puts in a header, checked against its keys on every
  request, in place of a plain header only a NetworkPolicy keeps others
  from setting. Keys are fetched only from the URLs given, or the hosts a
  load balancer's ARN names, a key not held at most once a minute, a load
  balancer's from a budget of its own, and a key id it doesn't have
  remembered.
- **The chart serves the UI at a URL.** `serve.service.type` puts the UI
  alone on `FULLNAME-ui`, a LoadBalancer or a NodePort (`port`,
  `annotations`, `loadBalancerClass`, `loadBalancerSourceRanges`,
  `externalTrafficPolicy`, `nodePort`); `FULLNAME` stays a ClusterIP with
  every port. `serve.ingress` (networking.k8s.io/v1) and `serve.httpRoute`
  (Gateway API), whose hosts become `--external-url` (https unless their
  `https` says not) and `--allowed-hosts`. `serve.signIn` gives the
  sign-in's flags and mounts its Secrets, and makes the session key's
  Secret and keeps it. `serve.roles.viewers`. The chart's README has each
  cloud's load balancer, GKE, EKS, AKS and OKE alike.
- `test/e2e/signin.sh`, against a real Dex in Docker, and
  `test/e2e/serve-lb.sh`, the chart on kind behind a LoadBalancer, a
  Gateway and an Ingress (cloud-provider-kind) with Dex in the cluster and
  the NetworkPolicy on: both in CI.

### Changed

- **`/readyz` is ready once the ledger has a run recorded**: at once after
  a restart, after the backfill's first day on a first install. Before, it
  waited for the process's first round of jobs, up to `--timeout` (the
  chart's 50 minutes) with a slow Prometheus. The chart's readiness probe
  is now `/readyz` (it was `/healthz`), so `helm install --wait`, `helm
  upgrade --wait` and Argo CD's health wait for that on a first install;
  `serve.readinessPath: /healthz` is ready at once, as before. The
  liveness probe stays `/healthz`.
- With `--oidc-issuer` or `--proxy-jwt`, loopback makes no one an admin,
  and `--everyone-admin` is refused. `--user-header` keeps the 0.28.1
  rule.
- A name from a header or a claim with spaces around it, or spacing other
  than the ASCII space (U+00A0, U+3000), is no one's, as one with a
  character that doesn't show already was.
- The chart refuses to serve the UI outside the cluster (a LoadBalancer or
  NodePort, an Ingress, an HTTPRoute) with no sign-in, unless
  `serve.exposeWithoutSignIn`; `serve.userHeader` on a LoadBalancer or a
  NodePort, unless `loadBalancerSourceRanges` names the proxy's addresses
  and `serve.service.sourceRangesAreTheProxy` says so; and, with
  `serve.signIn.oidc`, an http URL but to this machine. `serve.roles` and
  `serve.changes` take `serve.signIn` as they take `serve.userHeader`.
  `--external-url` and `serve.ingress.path` take no path but `/`.
- Upgrading the chart with `--reuse-values` from 0.28.1 works: the values
  added since are filled in where missing.
- New dependencies: `github.com/coreos/go-oidc/v3` v3.21.0 and
  `github.com/go-jose/go-jose/v4` v4.1.6, both Apache-2.0.

## [0.29.0] - 2026-11-12

Rules a repository renders, followed (docs/design/history.md). Reviewed
before release, as an attacker and for correctness; what both found is
fixed in what is described here. No ledger schema change.

### Added

- **Rules read out of rendered manifests.** `--rules`, `--rules-history`
  and `--compare` take a chart rendered whole (`helm template`, `kustomize
  build`, `kubectl get -o yaml`): of its documents, those that hold rules
  (ConfigMaps of rule files or of Grafana provisioning files,
  `PrometheusRule`s and the rest Lapilli reads, `List`s of them) are kept,
  and the others passed over and counted: `verdict` says how many on
  stderr, `serve` in its log. No `yq` is needed to filter them first.
- **`--rules-select SEL`,** a label selector as `kubectl -l` takes one
  (`key=value`, `!=`, `in`, `notin`, `key`, `!key`, `key>N`, `key<N`),
  keeps of the Kubernetes objects in the rule files those it matches:
  `grafana_alert=1` for the ConfigMaps Grafana's sidecar loads. A set with
  an empty value (`a in (x,)`), which kubectl takes, is refused as a slip.
  Labels are on objects only: a plain rule file has none, and is kept.
- **What holds rules, read closely.** A kind's own list (`ConfigMapList`,
  `PrometheusRuleList`) is read as `List` is. A Kubernetes object of a kind
  rules aren't kept in holds none, whatever else it has (a Secret with a
  top-level `groups`). A ConfigMap whose rule file doesn't parse is counted
  and said apart from what holds no rules. A file of rules is read 16 MiB
  at most.
- **Rendering each commit reliably.** `recipes/versions/git.sh` takes
  `RENDER_PREPARE`, run in each commit's files before `RENDER`, for what a
  commit lacks, such as a chart's dependencies that older commits don't
  vendor (`helm dependency build`, at the versions `Chart.lock` pins, or
  the archives as the newest commit vendors them, from `REPO`). It runs as
  `RENDER` does: without the token, with a `HOME` of its own, kept from
  commit to commit. `UNRENDERED=skip` passes over a commit that can't be
  rendered (`RENDER_PREPARE` or `RENDER` fails, or `RENDER` writes
  nothing) or read (below), saying which and why; by default it still
  stops, now saying which commit and that `skip` exists. Kustomize is
  documented. `MAX_TREE_FILES` (20000) and `MAX_TREE_BYTES` (256 MiB)
  bound what a commit is read with, by `git ls-tree` before anything is
  written.
- **Following the branch.** Each version `git.sh` writes holds `.commit`,
  its commit; `AFTER=COMMIT` writes only the commits after it.
  `recipes/versions/follow.sh` keeps a history up to date: at each look,
  the commits since the newest version's, added beside it, and with
  `KEEP`, the versions older than that taken away but the one in effect
  then. It never stops on a commit (`FOLLOWING`): one that can't be
  rendered or read is passed over, one dated after now is named now, of
  more than `MAX` new commits the newest are written, and `.head` keeps a
  commit passed over from being rendered again. A commit is named no
  earlier than when the clone's branch first held it, by the branch's
  reflog, less `LATE` (`EVERY` and a minute): one dated back takes effect
  when it was fetched. `lapilli serve` given `--rules-history` without
  `--rules` follows the directory: its newest version that reads is the
  rules in effect, and each version added later is recorded from its
  time, with what is read beside the rules (`--alarms`, `--loki-rules`),
  the hours since then replayed again by the next run (no further back
  than `--backfill`) and their days checked again. A version that can't
  be read is passed over, once, saying so; following never stops on it.
  `--changes` is refused when the rules are a followed history alone.
- **In the chart,** `serve.rulesHistory` takes `renderPrepare`,
  `unrendered` (`skip` by default), `select`, `max`, `every`,
  `maxTreeFiles`, `maxTreeBytes` and `sizeLimits`. With `render`, the
  versions are written in `renderImage`, an image with git, helm and
  kustomize, pinned by digest: Argo CD v3.5.6's by default (about 210 MB
  to pull); the README says how to give a smaller one. With no
  `rules.configMaps` the repository is the rules, followed:
  `rules-history-fetch` fetches the branch every `every` seconds (300)
  with the token, and `rules-history-follow`, without it, adds the new
  commits' versions, keeping the backfill and a day. When the start
  writes no version, serve starts all the same, and takes the history once
  it is fetched and written.
- `test/e2e/rendered.sh`, in CI: a real Gitea holding a chart of Grafana
  rules whose subchart comes from Gitea's Helm registry and isn't vendored
  at the first commit; the chart's own rules history containers, as `helm
  template` writes them, run in Docker; each version read for exactly its
  rule documents; `verdict --rules-history` and `serve`, following, against
  a real Prometheus with three days of a series, each day replayed with
  that day's rules; and a commit dated hours back, fetched, taking effect
  when it was fetched. `FuzzPick` joins the weekly fuzzing, and checks
  that nothing but documents of rules is kept.

### Changed

- `lapilli serve` keeps of each version in `--rules-history` only its
  documents of rules: a chart's Secrets and Deployments never reach the
  ledger.
- `lapilli serve --rules-history DIR` without `--rules` was refused; it now
  follows the directory.
- A version in `--rules-history` that can't be read no longer stops a
  first start: it is passed over, saying so, and the version before it
  stays in effect.

### Fixed

- **A ledger whose directory's name holds `#` or `?` opened another file**
  (main since 0.6.0): the path was cut there, so two ledgers named alike
  before it were one. The path is escaped; `%` and spaces read as they
  are too.
- **Following, found in review:** a version recorded while a run, or a
  check, begun before it was under way was lost (each note now bumps a
  sequence, and a run or a check forgets only what it saw); a good version
  found with a bad one after it in one look wasn't replayed again; sending
  the check back left what it kept of those days (a miss kept twice, a
  rule the version dropped keeping its checks); and beside `--alarms` or
  `--loki-rules`, every later commit took effect when seen.

### Security

- **In the chart, the clone is read-only where `render` runs.** It moved to
  a volume of its own (`rules-history-repo`) that only the containers
  holding the token write (`rules-history-clone`, and following,
  `rules-history-fetch`): `rules-history` and `rules-history-follow` mount
  it read-only, so nothing `render` runs can change the configuration or
  hooks the next fetch runs with. EKS's and Azure's webhooks pass the new
  containers over too, and the service account's token is mounted in none
  of them.
- **A commit is read as it is.** git runs with none of the image's or the
  user's configuration (the render image turns Git LFS on, its filter
  required, its server the commit's `.lfsconfig`), and from an empty work
  tree and index, so a commit's `.gitattributes` don't leave a rule file
  out (`export-ignore`), rewrite one (`export-subst`) or run a filter.
  With `RENDER`, a symbolic link anywhere in the commit, or one
  `RENDER_PREPARE` leaves, can't be rendered: a `file://` dependency's
  link outside `PATHS` carried what it led to (the container's
  environment) into a selected rule's annotation, and the ledger.
- **A commit's date can't rewrite the ledger.** Following, a version's
  time is bounded below by when it was fetched (above): a commit dated
  back had every hour since its date, up to `--backfill`, replayed and
  judged with rules not in effect then.
- **Bounded disk and memory.** What a commit is read with is bounded
  (`MAX_TREE_FILES`, `MAX_TREE_BYTES`): one blob named many times in a tree
  was gigabytes written out. The chart's volumes have size limits, its
  history containers ephemeral-storage requests and limits, and a
  followed directory keeps the backfill and a day.

## [0.28.1] - 2026-11-12

A security release, after two reviews of 0.28.0 from an attacker's point
of view, and a third of the fixes: the change recipes, the chart, the
release workflow, and what lapilli serve takes. Ledger schema 016 (tasks'
ids), applied at start.

### Changed (security)

- **Without roles, a server listening beyond loopback takes no changes.**
  With no `--editors` or `--admins`, everyone was an admin of the UI and the
  API wherever lapilli serve listened. The chart listens on `:8080` with no
  roles by default, so any pod in the cluster could write incidents, tasks,
  quiet windows and the settings (medium). Now, with no roles, everyone is
  an admin only on a listener on loopback alone (`127.0.0.1`, `::1`,
  `localhost`, the binary's default), as `--listen` says and the address
  it is bound to is; beyond it everyone may only look, and the log says so
  at start. To write: give roles behind a sign-in
  proxy (`--user-header` with `--editors` or `--admins`), or, to keep what
  0.28.0 did, `--everyone-admin`, in the chart `serve.everyoneAdmin: true`
  (off by default). Chart users with no `serve.roles` who wrote through
  `kubectl port-forward` need one of the two after upgrading.
- **The change recipes' branches name the ledger.** `lapilli/LEDGER/proposal-ID`,
  LEDGER twelve hex digits the ledger makes once and the changes API hands
  out, so two ledgers proposing to one repository never share a branch. A
  pull request already open is followed as before, by its link. A change
  still waiting for one goes on the new branch; a `lapilli/proposal-ID` a
  recipe before 0.28.1 pushed for it is taken when it holds the same
  change, and otherwise left as it is, which the run says (close its pull
  request by hand). A 0.28.1 recipe with an older Lapilli, which names no
  ledger, keeps `lapilli/proposal-ID`.
- **Release tags are `vX.Y.Z` on main.** Others (`v1.0.0-rc.1`, a tag off
  main) no longer build. Releases publish one at a time, and `latest`, the
  image's and GitHub's, is always the highest version released.

### Fixed (security)

- **A Lapilli someone took over could have a recipe push a CI file
  (high).** `docs/design/changes.md` assumes a stolen Lapilli: it hands the
  recipes a document and its hash. Any file under `RULES_PATHS` with that
  hash was written and pushed, but for a list of CI systems' names in one
  letter case: an executable script (its mode kept), a `Makefile`, a GitLab
  `include: local:` file, a build's configuration, a case variant of
  `.gitlab-ci.yml`, or `RULES_PATHS` itself naming one. CI that runs on a
  branch's push would run it with the repository's secrets before anyone
  reviewed it. The recipes now write only to a file of rules: mode 100644,
  named `*.yml`, `*.yaml`, `*.json` or `*.rules`, not what a CI system runs
  (a longer list, any letter case, `RULES_PATHS` too), one document of it
  at least a Prometheus rule file, a Grafana provisioning file, a
  `PrometheusRule` or a `ConfigMap`, and each document read as YAML reads
  it. And in it they change only the rules: each document as it was, or,
  rules in both, the same but for a rule file's `groups`, a
  `PrometheusRule`'s `spec`, or a `ConfigMap`'s entries, each changed one
  a file of rules alone. Anything else fails, with the reason on the
  proposal's page. (The review of the fixes found a document indented as
  a whole, a `ClusterRoleBinding` after a `PrometheusRule`, taken unseen,
  and a ConfigMap of rules rewritten as `aws-auth`, or a Grafana file
  given contact points, taken for their shape; and real files refused: a
  `ServiceMonitor` beside its `PrometheusRule`, a byte order mark, a
  `ConfigMap` with `immutable`. All fixed before release.)
- **A reviewed pull request could be changed under its approvals
  (medium).** The recipes pushed their branch with `--force` and took the
  pull request open from it: a stolen Lapilli handing an opened change out
  again as ready, or two ledgers with the same proposal id on one
  repository, rewrote the branch, keeping its approvals and auto-merge.
  Now a branch is pushed only where there is none. One holding the same
  change is a stopped run's, and taken; one holding another fails the
  change, saying why.
- **A team's editor could slow judging for everyone (medium).** Quiet
  windows had no bound, and each was set beside every firing it might
  excuse inside the ledger's one write transaction: 1,500 of a team's took
  re-judging 20,000 firings to about 106 s. The ledger now keeps 500
  windows, the last 50 for editors and admins to make, and a team's
  editors keep 20 of its windows, wherever they move them. What may
  excuse a firing is worked out once a span: by rule and team, and, for a
  window repeating by its schedule, the times it falls due in the span,
  which each firing is set beside by a search. A day's 10,000 firings
  recorded beside 500 windows that fall due in it take 0.63 s (0.34 s
  beside none; 25.9 s before), and judged again beside 500 that don't,
  58 ms (9.5 s before).
- **The release job held every write permission while it ran the
  repository's code (medium),** and restored a Go cache main's CI wrote.
  `release.yml` is now three jobs: `build`, which may write nothing and
  restores no cache (`go mod verify` before building); `verify`; and
  `publish`, which runs none of the repository's code, checks each file's
  sha256, and alone has the write permissions. The assets, attestations,
  Sigstore bundles and immutability are as before. A dry run
  (`gh workflow run release.yml --ref main -f version=vX.Y.Z`) builds and
  verifies and never publishes. Helm is pinned by version and sha256,
  BuildKit by digest, and `latest` is tagged only once the release is out,
  one release at a time, and only to the highest version.
- **`--prometheus` followed redirects (low).** Whatever answered as the
  Prometheus could send the query elsewhere, a bearer token to the same
  host's other ports, and with `--auth aws` the session token to any host.
  It now follows none, as every other client already did, and gives up on
  a request after five minutes.
- **A deleted task's id was given again (low).** The next task, maybe
  another team's, took over the deleted one's tracker issue: its comments
  both ways, and its close. Schema 016 makes task ids never repeat, and a
  proposal made into a deleted task is made into none. Upgrading, new ids
  start past the highest the ledger still knows was given (its tasks, the
  changes it keeps noted of tasks, the proposals made into them): a task
  deleted with none made after it, whose changes `--keep` has dropped
  since, leaves no trace, and its id can be given once more, its issue
  taken. The tracker recipes don't yet hold an issue to its task's time
  (`docs/design/trackers.md`).
- **The digest's preview skipped the one-heavy-request-at-a-time turn
  (low),** a regression of 0.25: any viewer could ask for many at once. It
  now waits its turn, as a report's preview does.
- **A recipe run pushed as many branches as it was handed (low).** Now
  `MAX_CHANGES`, 10 by default; the rest wait, and the run says so.
- **What a server answered reached a recipe's log raw (low),** escape
  sequences and all. It is now shown as a JSON string.
- **A token alone as `CLONE`'s user passed (low),** into git's argv and
  `.git/config`. Any user in an http or https `CLONE` is now refused.
- **The chart's NetworkPolicy limited only what comes in (low).**
  `serve.networkPolicy.egress` limits where the pod may connect, the
  cluster's DNS and what `to` names; off by default.
- **A cloud's identity reached the rules history's init containers
  (low),** which run git and `render` on a repository's content. The pod
  no longer mounts the service account's token anywhere but lapilli's own
  container (and there only for a cloud's identity), and EKS's and Azure's
  webhooks pass the init containers over. GKE's metadata server answers
  every container; the chart's README says what that means.
- **The chart ran its image by tag (low),** which can be moved. A
  release's chart now runs its image by digest (`image.digest`), and
  refuses another tag kept with its own digest.
- **`serve.trackers.image` took a tag (low),** though the docs asked for a
  digest. It must be `NAME@sha256:...` now.

### Documented

- `--mcp-compare` also sends the LogQL an agent wrote, for a rule kept in
  Loki, with Lapilli's token.
- Grafana's writes are scoped by the token alone: what an Editor's, limited
  by folder, and an Admin's, needed to turn a rule off, reach.
- Azure's metric and activity log alerts have no etag to write against:
  what may be written over between Lapilli's read and its write.

## [0.28.0] - 2026-11-11

Rules written in LogQL, replayed against Loki (docs/design/loki.md); the UI
in Spanish and Japanese; an accessibility review of the UI and its charts;
provenance beside each release; and the tests' data races gone
(docs/design/access.md). Reviewed before release, as an attacker and for
correctness; what both found is fixed in what is described here. No
ledger schema change: 0.27's 015 stays.

### Added

- **Loki's ruler's rules, replayed.** `--loki URL` (with
  `--loki-token-file` and `--loki-tenant`) is a Loki that rules written in
  LogQL are asked of, through its `query_range`. `--loki-rules PATH` reads
  a Loki ruler's rule files, `--loki-loaded-rules` the rules the Loki
  holds (`/loki/api/v1/rules`), each group placed where Loki runs it
  (`/prometheus/api/v1/rules`); beside any other source of rules, or
  alone. `lapilli verdict` needs `--prometheus` beside them only for what
  reads metrics; `lapilli serve` always needs it. A rule file's
  `query_offset` and `keep_firing_for`, which Loki 3.7.9 drops and doesn't
  run, are dropped too, and said; what Loki's API gives is trusted.
  `--check-replay` reads the `ALERTS` Loki's ruler writes through its
  `remote_write`. A Loki rule sharing a name is told apart as
  `Name [loki:namespace/group]`, and a change proposed for one, with its
  backtest, is for a person: Lapilli doesn't write to Loki.
- **Grafana's rules on Loki, replayed.** With `--grafana`, the type of each
  data source a rule reads is asked of Grafana; a Loki one's queries go
  through Grafana's data source proxy, with the rules' token, by GET: the
  Loki, tenant and credentials Grafana used. Instant and range queries at
  Grafana's step (a query's own step times its resolution; else its
  `intervalMs`, at least the window over 11,000), its Loki macros
  (`$__auto` the step, `$__interval` the query's interval, `$__range`),
  and its reducers over a range, worked out by Lapilli a batch at a time,
  since LogQL has no subqueries; no data told from no history as for
  PromQL. Read from files, a query whose model says `datasource.type: loki`
  is asked of `--loki`.
- **Said, not replayed:** a log query; a reduce asking Loki finer than a
  point a second (86,400 a series a day: a replay asks every window, and one
  rule at a millisecond made `serve` hold gigabytes); a query Grafana's own
  query fails on, at every evaluation: a range over Loki's 11,000 points (a
  step of its own of 1s over thirty days), a step Grafana can't read (a
  number, `1.5`, months), a `queryType` it doesn't run; classic conditions
  over Loki; a condition reading Prometheus and Loki together; and a
  condition reading any data source that is neither PromQL nor LogQL (SQL
  ones among them), by the type Grafana gives.
- **Measured** against Loki 3.7.9, Grafana 13.3.1 and Prometheus 3.5.0:
  over forty minutes, 88 of 88 firings of a Loki ruler's nine rules alike,
  and 166 of 166 of Grafana's seventeen on Loki, each starting and ending at
  the same evaluation, where v0.26.0 had 23% and 20%. Kept as test data
  (`TestRealLoki`, `TestRealLokiGrafana`), with a real Loki's answers at
  six millisecond steps and what Grafana 13.3.1 asked Loki for, and run
  against real servers by `test/e2e/loki.sh` in CI. The chart takes
  `--loki URL --loki-loaded-rules` (and `--loki-tenant`) through
  `extraArgs`, beside its rules or alone; a token file or rule files to
  mount need values of their own, still to do.
- **Spanish and Japanese** for the UI and for what `serve` posts
  (`--lang es`, `--lang ja`, the chart's `serve.notify.lang`), offered in the
  language picker. Written with a machine's help, in the words SREs use;
  they want a native speaker's review.
- **Provenance beside each release**, from v0.28.0 on: the attestations'
  Sigstore bundles, for the archives, `SHA256SUMS`, the chart and the
  image, as `lapilli-vX.Y.Z.intoto.jsonl`, a release's seventh asset;
  SECURITY.md says how to check a download with them, offline, with `gh`
  or cosign, held to the release's tag, as checked against v0.27.0's.

### Changed

- **An accessibility review of the UI, to WCAG 2.2 AA**
  (docs/design/access.md): a skip link; each page's title its own; the
  navigation and the language picker labelled, each language named in its
  own; a name for every field, comment box and link; header cells with
  scope; every chart read out, its numbers in a table (the precision chart's
  weren't), and checked against it; links and field borders that can be
  told apart at 3:1; one visible focus ring; no sideways scroll at 320px;
  in-page jumps clear of the header. axe-core over every page in four
  languages, light and dark, 0.27's on-call schedules and tracker comments
  among them, went from 136 failing elements to none (`test/a11y/axe.sh`);
  `TestPagesAccessible` (in more than one state of a page),
  `TestContrast`, `TestChartsInTables` and `TestCSSAccessible` hold it. No
  script added.
- The content check makes no PromQL guess of a Loki ruler's rule (a range
  shorter than two scrapes, a label an aggregation can't give).
- Before, a Grafana rule whose condition read Loki was asked of
  `--prometheus` as PromQL: it failed, or, where LogQL also parses as
  PromQL, was replayed as never firing, or as NoData.
- A time an API writes is read exactly, not as a float64: a point at a
  step in milliseconds is no longer up to 128ns off it.
- A range refused for being too large is halved on its own grid, so a group
  placed within its interval keeps its evaluations. A range refused for its
  series (Loki's `max_query_series`, Mimir's
  `max_fetched_series_per_query`, both counted over the range) is halved
  too, down to a single evaluation.

### Fixed

- `go test -race ./internal/serve` passes: the tracker recipes' test fakes
  shared their state with the tests without a lock (70 race reports, and
  one more in 0.27's comment tests).

## [0.27.0] - 2026-11-11

What comes in (docs/design/inputs.md): every delivery Alertmanager made,
on-call schedules' turns, when a change was deployed, a cloud alarm's
replay checked against its history, Compass's closes, two more trackers
and comments kept in step. **Ledger schema 015**, after
0.25's 014: a table of on-call turns, and `comments` made again with three
columns (`origin`, `ref`, `author`) and ids never used again
(AUTOINCREMENT), its rows and ids as they were; no flag changes meaning.

- **Alertmanager's event recorder** (0.33.0 on, `--enable-feature=event-recorder`):
  `POST /v1/alertmanager/events` on the notifications port, with the
  notifications token, takes its webhook output, one event or a batch.
  Each notification event, a delivery by an integration, is kept as a
  notification to `RECEIVER/INTEGRATION` (`oncall/pagerduty`,
  `oncall/slack`), so PagerDuty's, Slack's and email's deliveries are
  counted as the webhook's are; both formats are read (0.33 and 0.34's
  label pairs, 0.35's maps), as recorded from real Alertmanagers 0.33.1,
  0.34.2 and 0.35.0. A delivery sent again is kept once; other events are
  answered and passed over, at a rate of their own; `?skip=NAME` passes
  over Lapilli's own webhook receiver kept beside it.
- **Shifts from on-call schedules**: `--on-call FILE`, for `verdict` and
  `serve`, names iCalendar feeds (PagerDuty, Opsgenie, Jira Service
  Management, Grafana OnCall and IRM, Google Calendar; by a URL in a file,
  a URL and a token, or a file) and the people in them with their hours;
  `--shifts on-call=NAME`, or a team's shifts, counts each firing by the
  hours of whoever's turn it came in. `serve` reads each feed every
  `every` (30m) and keeps the turns in the ledger past what the feed
  holds; its runs page shows each schedule. Daily and weekly repeats,
  `EXDATE` and `RECURRENCE-ID` are read; what isn't is passed over and
  said. The chart's `serve.onCall` mounts the file and its Secret.
- **When a change was deployed**: `recipes/versions/argocd.sh` and
  `flux.sh` write the revisions an Argo CD Application or a Flux
  Kustomization or HelmRelease deployed and when, and with `DEPLOYS`,
  `git.sh` writes the rules each deploy put in place, by when it did,
  rather than each commit by its time.
- **A cloud's PromQL alarm's replay, checked**: `lapilli serve` sets a
  CloudWatch, Google Cloud or Azure PromQL alarm read with `--alarms`
  beside its history copied in under its source, tied by `alarm_id`, as it
  sets rules beside `ALERTS`; a CloudWatch alarm's replay is put together
  as the alarm as a whole first. `lapilli verdict --check-replay
  --check-firings FILE` does the same from a recipe's output. Without the
  history, such an alarm says why it isn't compared.
- **Compass's closes**: `recipes/incidents/compass.sh` reads each closed
  alert's log (`/v1/alerts/{id}/logs`) and ends it at its last close
  (`compass-closed.jq`), at most `MAX_LOGS` (500) alerts a run, rather
  than at its last update.
- **Trackers**: `recipes/trackers/azure-boards.sh` and `shortcut.sh`, alike
  with the others, and in the chart (`serve.trackers.tracker`). With
  `COMMENTS=true` (the chart's `serve.trackers.comments`), every tracker's
  recipe keeps comments in step both ways, each once: people's comments on
  a task are posted to its issue, quoted, and the issue's copied to the
  task as the tracker's. The tasks token gains `GET` and `POST
  /api/v1/tasks/{id}/comments`, and sends 64 KB at most a request (was 16).
- **Fixed before release, after two reviews** (docs/design/inputs.md,
  "What the reviews found"): Alertmanager 0.35's muted alerts aren't
  counted as delivered; a notification the ledger won't keep doesn't drop
  the rest of a batch, which is answered with a count of what was refused;
  a storm of other events doesn't refuse deliveries; a tracker comment
  Lapilli refuses is passed over rather than failing every run, one with no
  text or too long is made to fit, and an emoji or a soft hyphen is kept;
  Lapilli keeps the tracker's id of each comment it posted (the tasks
  token's `PATCH /api/v1/tasks/{id}/comments/{comment}`), so none is posted
  twice; restricted comments aren't copied, and a comment is posted without
  who wrote it unless `COMMENT_NAMES=true`; a comment on a closed issue
  doesn't close a task reopened in Lapilli; Azure Boards' Basic process; on
  call, people added later are found in turns kept, repeats are bounded
  and cheap, `WKST` is honoured, a removed turn at a feed's head goes, and
  events far outside the span read don't take kept turns away; deploys are
  walked as deployed (rollbacks, reverts, failed reconciles, the deploy in
  effect at `SINCE`, look-alike tags); a day held for a cloud's alarms'
  history is told of when checked.
- **Fixed before release, after a verification** (docs/design/inputs.md,
  "What the verification found"): a comment written after a posted one was
  deleted is posted, not taken for it (ids never used again); a tracker's
  comment whose text or author's name doesn't show is kept as what does
  (U+FFFD, "someone"), under its id, so such comments no longer starve the
  rest, and the job stops at the 1,000 a task keeps and counts only what
  was kept; a ref another comment has is a 409, not a 500; Shortcut's
  archived story is dated by its history, so the job's own comment doesn't
  drop a reopened task again, and a Jira without
  `statuscategorychangedate` is read by `resolutiondate`;
  `lapilli_serve_notifications_refused_total{source}` counts notifications
  refused; on call, `who` and the summary come before a lone attendee the
  file doesn't know (PagerDuty's), names are cut to the ledger's 200
  bytes, an event passed over keeps its turns kept, the ledger keeps a
  feed's 50,000 turns, a person in the file twice is refused, and two at
  once is said; deploys: a revision is found among the clone's objects (a
  tag or a branch named like it doesn't hide it), only deployed releases
  count, `MAX` counts the deploys that changed the rules, a date-only
  `SINCE` is the start of that day in UTC, and a kept file without a final
  newline is read whole.
- **With 0.25's roles and names**: a comment's author from a tracker and an
  on-call turn's name follow the ledger's rule for names (`Hidden`); a
  comment copied in is the tracker job's (`tasks-token`, a name the sign-in
  proxy can't give), so no one, an admin or a team's editor, changes it;
  team scope is people's, and the tasks token copies and marks on every
  team's tasks.
- **Reviewed as an attacker would**: the events path reads no body before
  the token, refuses what isn't the recorder's, and bounds events and
  alerts; feeds are fetched over https only, with no redirect followed, 8
  MB and 30 seconds at most, their URLs never shown, and their events,
  lines and repeats bounded; deploy histories reach `git` only as hex and
  RFC 3339; Compass's and the trackers' next pages are rebuilt from
  checked cursors; comments from a tracker are kept as text that shows,
  10,000 characters at most, 1,000 a task, never told, and can't be edited
  in Lapilli. Each refusal has a test.

## [0.26.0] - 2026-11-11

Replay fidelity (docs/design/states.md): what Grafana and Prometheus do
around a rule's condition, replayed where history can say it, and said
where it can't. Set beside a real Grafana 13.3.1 and Prometheus 3.5.0,
the replay of the same rules went from 56% of firings alike to 100% for
Grafana's, each starting and ending at the same evaluation, and from 82%
to 100% for Prometheus's read from its API; the recording is kept as test
data and replayed by every test run. Reviewed before release; what the
review found is fixed in what is described here, and measured again with
its shapes added (a series there 60s of 120s with `for` and with
`keepFiringFor`, `keepFiringFor` through no data and while its value is 0,
a pending alert whose series goes and comes back): over 46 minutes, 255 of
255 firings alike, where v0.22.2 had 37% and this branch before the review
62%. Verified again on gauges Prometheus scraped every 15s, with a data
source whose own scrape interval is 30s and three rules changed while
Grafana ran; what that found is fixed too (docs/design/states.md, "What
the verification found"): over 40 minutes, 135 of 135 firings alike,
where before the verification 35% were.

### Added

- **Grafana's states, as its state manager keeps them**: Normal, Pending,
  Alerting, Recovering and NoData, moving only on an evaluation's results,
  with the rule's `for` and `keep_firing_for` worked out there. An
  evaluation at which a query returned nothing, or the condition came to
  nothing, becomes what the rule's `noDataState` says: an alert of its own
  in the NoData state (labelled `grafana_alertstate="nodata"`, as
  Grafana's `GRAFANA_ALERTS` has it), every alert held as alerting, let go,
  or kept as it was. An alert whose series has gone stays as it was, a
  pending one never firing while gone, for
  `missing_series_evals_to_resolve` evaluations, then resolves with no
  `keep_firing_for`. A condition's NaN is Alerting, as in Grafana.
- **No data told from no history.** A query is taken to have returned
  nothing only once it has returned something, in the replay or the day
  before, looked through as often as the rule is evaluated, a minute at
  most: before a server's retention, or before a metric existed, nothing is
  NoData. A rule whose query has returned nothing for over a day is
  replayed as quiet. What the day before said is asked once for every set
  of rules replayed together, and kept, so `serve`'s next run doesn't ask
  again while it is within the day; a question that fails fails the rule,
  as a query does.
- **Only the data source a Grafana rule's condition reads** is asked, its
  queries outside the condition too, for whether they returned anything:
  Grafana takes the rule as having no data when one of them returns
  nothing, or is refused as written (as Grafana 13.3.1 was seen to). A
  query on another data source (a Loki query whose values an annotation
  shows) isn't asked, nor an expression over it; and one outside the
  condition that fails otherwise doesn't fail the rule.
- **A Grafana rule's alerts carried across an edit Grafana carries them
  across**: what it says, `keepFiringFor`,
  `missing_series_evals_to_resolve`, its group's interval. They start
  afresh only on what Grafana's fingerprint of the rule holds: its uid,
  title, folder, group, labels, condition, queries, pause, notification
  settings, `for`, dashboard and panel, and its no data and error states.
- **Classic conditions with every reducer**: `diff`, `diff_abs`,
  `percent_diff`, `percent_diff_abs` and `count_non_null` too, and the
  `gte`, `lte`, `eq`, `ne`, `no_value` and `_included` range evaluators and
  `logic-or`, evaluated as Grafana's own code does, with its no data, null
  and NaN.
- **Near misses by value** for a Grafana math expression joining
  comparisons with `&&` or `||`, a comparison beside anything else, and
  classic conditions: which comparison nearly held (`$A`, `avg(A)`), how
  close it came, and the number that would have caught the incident.
- **`--late DUR`** and `--late PATTERN=DUR`, for `verdict`, `serve` and
  what an agent compares over MCP: replay as if metrics arrived that late,
  each evaluation reading the history as it stood then; a proposal's
  backtest in `serve` too. A Prometheus group's `query_offset` is read, and
  a Grafana query's window that ends before now.
- **Groups placed where Prometheus places them**, for rules read from its
  rules API (`--loaded-rules`): evaluated at the second of the interval the
  group last ran at, not on the minute.
- **"Replayed in part"**: a rule on a cloud's own monitoring replayed
  without `--late`, a Grafana rule with a query on another data source than
  its condition's, and one whose query outside its condition failed in the
  replay are marked so in `verdict`'s output and JSON (`unseen`), and the
  first two on the rule's page in `serve`. `--check-replay` says, by rule,
  how many of Grafana's error alerts it left out of what really fired.

### Changed

- A Grafana rule that leaves out `noDataState` is replayed with Grafana's
  default, `NoData`: where its queries return nothing, having returned
  something, it now fires, as it did.
- Grafana's range queries are read at Grafana's own resolution, measured:
  `intervalMs`, 1s when not set (it was 15s), and its window with both
  ends, for the reduces that count points (`count`, `count_non_null`,
  `sum`). The rest are asked at the scrape interval, `--scrape-interval`,
  which now goes with a replay too, or else at the shortest interval the
  series they read are scraped at, as Prometheus's `/api/v1/status/config`
  and `/api/v1/targets` say for the range check, `verdict` and `serve`
  alike; where neither says, at Grafana's resolution too. The server's
  own resolution, a minute, stepped over a 15s spike Grafana's points saw.
  The step is cut to one that divides the rule's interval and the window
  too, so each evaluation is one of the subquery's: asked every 15s, on
  the epoch's grid, a firing on a spike started an evaluation after
  Grafana's.
  A window's newest number is the query's own value at the evaluation. A
  rule evaluated more often than once a minute read its newest value up to
  a minute old.
- What really fired, for Grafana's rules, leaves out its error alerts:
  `GRAFANA_ALERTS{alertstate="firing",grafana_alertstate!="error"}`; and
  reads an alert's alerting and recovering series as one firing.
- A Prometheus rule with a `query_offset` fires at its evaluation, and is
  set beside `ALERTS`, which Prometheus writes at the query's time, that
  much earlier. Its key carries the offset: it is a new version once, and a
  change proposed to it before the upgrade and not yet carried out is
  refused as made to a rule that has changed; propose it again.
- `lapilli serve` replays with this only what its runs replay from now on:
  a day it has settled keeps the firings it was replayed with before.

### Fixed

- **A Grafana rule's `$` read as `$$`.** Grafana's file provisioning takes
  `$$` for a `$` in a rule's title, labels, group, folder and other
  settings, and its provisioning export, which `--grafana` reads, writes
  every `$` there as `$$` (seen on Grafana 13.3.1: a label
  `'{{ $$labels.team }}'`, a title `Cost $$5`). Lapilli read them as
  written: such a rule went by a name Grafana's alerts don't have, its
  labels never agreed with what really fired, and a change proposed to it
  was refused as stale once read again. They are read as Grafana reads
  them now; no environment variable is put in, as Grafana would. A rule's
  annotations and queries, which Grafana takes as written and doesn't
  escape, are read as written, as before: what an alert says was checked
  right. Such a rule is a new version once, and a change proposed to it
  before the upgrade is refused as stale; propose it again. The content
  check routes it by its folder and receiver as Grafana's notification
  policies name them.
- **Grafana's `$__interval`, `$__rate_interval` and `$__range`** (and their
  `_ms`, `_s` and `${...}` forms) are put in a query as Grafana's Prometheus
  data source puts them, as measured on Grafana 13.3.1: the step; the
  larger of the query's `intervalMs` and a scrape interval together and
  four scrape intervals, the scrape interval the query's own, else the data
  source's (`jsonData.timeInterval`, read from Grafana with `--grafana`;
  `--scrape-interval`, or Grafana's 15s, for one that can't be read); and
  the window. A rule with one couldn't be replayed ("query failed").
- A rule in a Grafana provisioning file with `$$` in its folder, group, uid
  or title couldn't be changed in the file ("the change can't be written
  into this document"): it is found by what Grafana calls it, and the file
  keeps its own escaping.
- `--check-replay` counts Grafana's error alerts only of the rules checked.

## [0.25.0] - 2026-11-11

`lapilli serve` run by the people who use it
([docs/design/serveops.md](docs/design/serveops.md)): quiet windows every
month or by cron, and a team's own; the weekly digest set in the UI; roles
given in the UI, of every team or of one; the range check by each job's own
scrape interval; and Grafana's notification templates checked beside each
rule. Ledger schema 014: `grants`, and a quiet window's team and schedule;
nothing changes on upgrade until someone uses them.

- **Quiet windows every month or by a cron expression**, beside every day
  and every week: `every: month` with `on` a day (`15`), `last`, or a
  weekday of the month (`first sat`, `last fri`), at the start's time of
  day; `every: cron` with `on` five cron fields, hourly at most. The first
  repeat is the window's start and end, and must be one of the schedule's
  times (a refusal names the next); each repeat is shorter than the
  shortest time between two over the four years from the start, a leap
  year's cycle. A firing is looked up once: in the last repeat to start by
  it, or by the end of Until's day, whatever the gap. Daylight saving is
  the reports' (`internal/schedule`, which also gains the weekday of a
  month for reports: `monthly first sat@09:00`).
- **A team's quiet window** (`team_id`) excuses only the firings of rules
  the team owns, worked out in the ledger as the pages work it out; a
  change to who owns what judges every team's window again, and a team
  removed stops its windows, as it stops its reports. A window's page
  names the team of each firing it excused.
- **Who owns a rule** is decided by its name first: a note's team, or else
  the first team, by name, whose pattern matches it, and only then the
  first whose labels it has. Before, the first team by name whose pattern
  or labels matched did, so a team owning by labels could take a rule
  another team's pattern named. Where teams overlap so, upgrading moves
  such a rule to the team its pattern names, and with it everything done
  by team: the rule's page and lists filtered by team, the teams' tallies
  and overview, its team's digest and reports, whose hours count its
  firings asleep or off hours, the incidents its catches give a team, and
  which team's editors may change its notes, tasks and quiet windows.
- **Roles given in the UI** (`/roles`, from the Teams page, and
  `/api/v1/roles`), with `--admins`: an admin gives an editor's role, of
  every team or of one, to `user:NAME` or `group:NAME`. Never admin or
  approver (the table refuses any other role), never everyone, and a role
  the flags give can't be taken away. A team's editors change their team's
  tasks, quiet windows and reports (to the team's own destination), notes
  on rules it owns (keeping their team) and comments on what is its; the
  ledger checks each inside the write, before and after, so a row can't be
  moved between teams. Only admins see who holds a role; a role taken away
  stops at the next request; a team removed takes its editors with it;
  the runs page shows roles given and taken to admins alone. A team's
  editors make ten of its reports at most, and the last ten of the
  ledger's fifty are an admin's.
  `GET /api/v1/me` gives the teams the reader edits, and the pages offer
  each reader what they may.
- **The weekly digest set in the UI**: on the Reports page and with `GET`,
  `PUT` and `DELETE /api/v1/reports/digest`. The flags define it until an
  admin sets it; from then on the ledger does, `--digest` isn't used (the
  log says so at start), and it can go to any named destination, a
  `--report-dest` too. It stays weekly, and its suggestions are worked out
  at its time whether it is sent or not. Upgrading changes nothing: the
  weeks sent are kept as before, so no week is sent twice and none is lost;
  a week due while it is off is passed over; moving its day never sends an
  older week. "Back to the flags" gives it back.
- **Reports of a team** can be made by its editors and by editors, sent to
  the team's own destination (`team:NAME`); reports for everyone, or sent
  elsewhere, stay an admin's. A week of the digest sent isn't sent again
  to a destination added after it went; the digest is on the Reports page
  to set whenever a report can go anywhere.
- **The range check by each job's own scrape interval**: `serve` and
  `lapilli verdict --check-content` read each job's `scrape_interval` from
  `/api/v1/status/config` and the job label its targets end up with from
  `/api/v1/targets` (a ServiceMonitor's job is its service's name). A range
  over a selector naming its job counts with that job's interval (the
  longest a `job=~` can be); any other with the global, or the one every
  job has. The finding says whose: `rate[3m] < 2 × 2m (job "batch")`.
  Checked against a real Prometheus 3.5.0.
- **Grafana's notification templates**, with `--grafana`: its templates,
  contact points and notification policies, read once an hour through the
  provisioning API with the rules' token (a Viewer's reads them), 8 MB an
  answer, no redirects. A rule's notifications are written with the
  templates of its own contact point (`notification_settings`) or of those
  the policies route it to by its labels, `alertname` and
  `grafana_folder`; a matcher on a label the rule doesn't set is followed
  both ways, and a template counts only when every way uses it. Three new
  checks: `notify_no_text` (an annotation a template shows the rule
  hasn't; fact), `notify_label_never_seen` (measured) and
  `notify_label_not_given` (heuristic, by a rule's one query). Policies'
  `match` and `match_re` are read; a rule's label written as a template,
  and policies past the 2,000 or 32 levels read, route both ways or not at
  all; `{{ or .x "fallback" }}` is guarded. Contact points are numbered
  and a way's are a bitset; routing is charged by the work done, a
  million a rule and twenty million a check, rules alike in the labels the
  policies match on are routed once, and the check routes for five
  seconds at most: the 1,697-policy tree a real Grafana accepted gives
  each rule up in about 20 ms, and 500 rules take 0.3 s. A rule given up
  on says so on its page, and why, and the log says how many, once a
  check. Templates are parsed, never run. Checked against a real Grafana
  13.3.1.
- **Korean**: quiet windows are 메인터넌스 윈도우, as DevOps teams say it.
- **Security (from before this batch):** a name the sign-in proxy gave
  with a character that doesn't show (a zero-width space, a soft hyphen, a
  word joiner) had it taken out, so it could become another person's, an
  admin's. Such a name, or one over 200 bytes, is no one's now: it may
  only look. Refused are control characters; format characters, but the
  zero-width joiner and non-joiner between two letters of one script or
  two emoji, as Persian names and emoji use them; characters that show
  nothing on their own (the combining grapheme joiner, the Hangul
  fillers); and a variation selector but after a symbol. A grant's name
  is held to the same, 200 bytes.
- **The weekly digest's outbox guard** read a key TellOnce didn't write,
  so from the second week every round worked the whole digest out again
  before finding it told (also in 0.23 and 0.24). It reads what TellOnce
  keeps, or the week 0.24 marked as passed over, when later.
- Reviewed three times, once as an attacker would and twice for
  correctness and cost, then the fixes verified and what that found fixed
  too: no way to an admin's or an approver's role was found; what was found
  is in docs/design/serveops.md, "What the reviews found". Every refusal
  has a test, and each fix and key check, taken out, fails one. A ledger
  from 0.24 migrates to schema 014 with the same stones.

## [0.24.0] - 2026-11-11

Changing a cloud's own alarms, step 3 of changing rules with a person's
approval (docs/design/alarms.md): Amazon CloudWatch's alarms, Azure
Monitor's alert rules, Google Cloud Monitoring's alerting policies and OCI
Monitoring's alarms, read through each cloud's API with `--alarms` and
changed through it, once a person approves, only those `--changes-in`
names. The four clouds gain the same; none of the four paths has run
against an account, only against stand-ins answering as each cloud's
documents show. No ledger schema change; but a CloudWatch alarm whose
actions are off now reads as off, so a ledger reading such an alarm from a
file gains a version on upgrade.

- **`--alarms SOURCE=URL`**, for `lapilli verdict` and `lapilli serve`,
  repeatable, beside `--rules`, `--loaded-rules`, `--grafana` or `--ruler`,
  or alone: a cloud's alarms through its API, the URL's host saying which
  (`https://monitoring.REGION.amazonaws.com`, Azure Resource Manager's
  subscription or resource group, a Google Cloud project's
  `alertPolicies`, OCI's `telemetry.REGION` with a `compartmentId`), read
  with that cloud's credentials whatever `--auth` says. One document an
  alarm, its state left out, so a version is a change to an alarm, not to
  its state; a read that fails anywhere keeps the version as it was. An
  alarm and a rule of one name are told apart by the source
  (`HighCPU [cloudwatch-prod]`). `lapilli serve` reads them every
  `--alarms-every` (10m), so the clouds' request quotas hold.
- **Read too**: an Azure metric alert in PromQL (one static criterion) is
  replayed, from files too; OCI's alarms say what they say (`GetAlarm` has
  the body the list hasn't); a CloudWatch alarm with `ActionsEnabled: false`
  reads as off.
- **An alarm and its history, one rule.** An alarm Lapilli doesn't replay,
  read through `--alarms`, and its history copied in under its source are
  one rule, judged under the alarm's name: tied by the alarm's own ID,
  which each firings recipe now labels its alerts with (`alarm_id`: the
  ARN, the resource id, the policy's name, the OCID), or by name where none
  is given, and at the severity the alarm has now where it changed (one
  that fired at the other within twelve weeks has no record). Google's
  policies are compared by the policy, their project named by ID or by
  number. Its record is twelve weeks its history was copied in for
  without a gap, up to now: each write's period is kept (in the ledger's
  meta table; no schema change), so copies that stopped for a while, or
  stopped longer ago than `--source-quiet`, are no record until the
  weeks missed are copied in. A ledger from before keeps its source's
  last period alone; a job copying weekly wants `--source-quiet 8d`.
  Without one, it has no record: it isn't among the rules that never
  fired, and retiring it isn't proposed.
- **The alarm path.** A proposal for such an alarm is applied through its
  cloud's API when `--changes-in` names it as `SOURCE:NAME` (CloudWatch's
  alarm name, Azure's `RG/NAME`, Google's display name, OCI's display name;
  a prefix ends in `*`) and no tag says another tool keeps it
  (CloudFormation's own, Config Connector's and Terraform's labels, or one
  `--changes-managed` names); otherwise it is for a person, and its page
  says why. A `for` and a threshold where the alarm is PromQL, backtested;
  what it says on all four, a runbook its own form; turning it off. What
  is written is the least each API takes: CloudWatch's whole alarm as read
  (`PutMetricAlarm`), its actions alone off and on, Azure's properties
  changed (`PATCH`), Google's fields in `updateMask`, OCI's fields with
  `if-match`. The rest of the alarm changed since, the change is made
  afresh; the rule itself changed, it is refused as stale. Read back, once
  more a round later where the cloud may be a moment behind.
- **Not every alarm**: CloudWatch's whose actions scale or act on an
  instance aren't changed; one that warms up isn't rewritten; a composite
  alarm is turned off and on alone; one with a field this Lapilli's model
  doesn't have isn't rewritten, nor deleted from here (its undo couldn't
  make it again whole). A Google policy of several conditions takes a
  condition's `for` and threshold alone.
- **Retiring**: an alarm Lapilli can't replay is turned off and no
  further, never deleted from here: the page says to delete it in the
  console once it has been off a while. A PromQL alarm is turned off, then
  deleted four weeks later, its replay checked.
- **Turning off and deleting wait while the rule fires**, on every path:
  the git path, Grafana, the rulers and the alarms alike, while the ledger
  has an alert of it still on (an alarm's by its ID too), or the cloud
  says so, read just before the write: CloudWatch's `ALARM`; an Azure
  alert of the rule whose monitor condition is `Fired` and that no one
  closed (one of a stateless rule, an activity log alert's or one that
  doesn't resolve itself, only within its evaluation window); an open
  Google alert of the policy; OCI's `FIRING`. A cloud that throttles or
  fails a while holds the change a round, uncounted. It says it stopped
  once it is carried out.
- **One API path with the rulers'**: an alarm is changed by the engine
  v0.23's rulers are, with each of their guarantees: one change to an
  alarm at a time, read back only where it was sent and again for two
  minutes, an undo put back only over the change as it left it and
  checked as any write. A CloudWatch alarm is written only in its own
  account (the role's, or the pod's own as STS says), and a Google policy
  deleted and made again by a person isn't made a second time by an undo;
  made again twice alike after a lost answer, it is closed as settled in
  the console. One rule whose API won't answer leaves the week's other
  proposals made.
- **Write identities**: `--changes-aws-role` and `--changes-azure-client-id`
  for alarms too; `--changes-google-service-account`, a service account
  impersonated through IAM Credentials; `--changes-oci-config`, an OCI
  config file and its API key. Each needs an `--alarms` of its cloud.
- **The chart**: `rules.alarms`, `rules.alarmsEvery`, and
  `serve.changes.googleServiceAccount` and `ociConfigSecret`, the key
  mounted only with changes on. Its README gives each cloud's least
  privilege.
- The weekly fuzzing reads the four clouds' alarm answers too.
- Reviewed as an attacker would and for correctness; what the reviews
  found is fixed: a notification ties to its alarm's firing by `alarm_id`;
  an undo is refused where the change it undoes would be; a composite
  alarm's parents are asked as AWS takes it; the waits above; ties by
  severity and across a policy's conditions; a record up to now; an Azure
  description added and undone; the chart's OCI URL with `compartmentId`
  anywhere in its query. A verification of those fixes found nine more,
  fixed too: Azure's stateless alerts, a record across two severities and
  across a gap in the copies, Google's two forms of a policy's name, an
  alarm with a field this Lapilli doesn't know deleted, a cloud that
  throttles, a failing lookup stopping the week's proposals, and the two
  policies alike. Each is held by a test that fails without it.

## [0.23.0] - 2026-11-11

Changing rules through a managed Prometheus ruler, step 2 of changing rules
with a person's approval (docs/design/rulers.md): Mimir and Cortex, Amazon
Managed Service for Prometheus and Azure's Prometheus rule groups, read
with `--ruler` and changed through their own APIs, once a person approves,
only in the namespaces `--changes-in` names. Mimir's path runs against a
real Mimir 3.2.4 in CI; Cortex's, Amazon's and Azure's against stand-ins
answering as their documents show, not an account. No ledger schema
change: a rule read otherwise keeps its key, so no ledger sees a new
version of its rules on upgrade.

- **`--ruler URL`**, for `lapilli verdict` and `lapilli serve`: the alert
  rules a ruler holds, as written, from the API that writes them. Mimir's
  and Cortex's configuration API (`--ruler-token-file`, and
  `--ruler-tenant` for one with tenants, sent as `X-Scope-OrgID`), Amazon's
  control plane (`https://aps.REGION.amazonaws.com/workspaces/ID`, with
  `--auth aws`), or Azure Resource Manager's rule groups in a subscription
  or a resource group (with `--auth azure`). A rule knows its namespace;
  rules of one name in two namespaces are told apart by both
  (`ScrapeSlow [payments/shop]`).
- **The ruler path.** A proposal for a rule read with `--ruler` is applied
  through the ruler's API when `--changes-in` names its namespace (Azure:
  `RG/NAME`; a prefix ends in `*`) and no tag says another tool keeps it
  (CloudFormation's own, or one `--changes-managed KEY=VALUE` names);
  otherwise it is for a person, and its page says why. What one write
  replaces (a group, an Amazon namespace, an Azure rule group) is read
  again just before: the rule itself changed since is refused as stale;
  other rules in it changed, the change is made afresh. It is kept for
  undo, written with its kind's write credential
  (`--changes-ruler-token-file`, `--changes-aws-role`,
  `--changes-azure-client-id`), and read back as rules: an Amazon namespace
  stays applying while it is `UPDATING`, and a unit that reads as neither
  it was nor as proposed fails. A group goes with its last rule; an Amazon
  namespace's or an Azure rule group's last rule is for a person.
- **Undo** puts back the rule as it was kept in the group as it is now,
  others' changes since left as they are.
- **Retiring on Azure** turns the rule off (its own `enabled: false`) and
  deletes it four weeks later, approved again, as Grafana's.
- **Every request** goes to `--ruler`'s host only, follows no redirect
  (nor an Azure `nextLink` elsewhere), and reads at most 64 MB for a list
  and 4 MB for one unit, within 30 seconds; names read from a ruler go into
  a path as one escaped segment.
- **The chart**: `rules.ruler` (`url`, `tenant`, `tokenSecret`), and
  `serve.changes.in`, `managed`, `rulerTokenSecret`, `awsRole` and
  `azureClientId`, the write credential mounted only with changes on. Its
  README gives the IAM policy and the Azure role.
- Google Cloud's `Rules`, `ClusterRules` and `GlobalRules` stay on the git
  path: a test now holds that a new `for` and a new threshold are written
  into one, the rest of the file as it was.
- `test/e2e/changes.sh` starts Mimir 3.2.4, which its Prometheus writes to,
  and runs the ruler path against it. The weekly fuzzing reads the three
  rulers' answers too.
- Reviewed before release as an attacker would and for correctness; what
  the reviews found is fixed: undo puts back one rule, only over the rule
  as the change left it, every key of it, never a group's other rules
  with it; a unit is read back by its rules' whole entries; one change to
  a unit at a time, read back only where it was sent; names counted by
  namespace and group together and escaped, so two can't read alike, and
  none with a `..` segment written to, nor a rule's own name taken for one
  Lapilli makes; one group Mimir writes in a form YAML can't read back
  (`|4`), or one Amazon namespace whose file doesn't read, is passed over
  and logged, not the whole read, and a Mimir namespace of more than 128
  bytes is read; an Azure rule group's id is kept in one letter case; a
  Mimir read back as it was is read again for two minutes.

## [0.22.2] - 2026-11-11

### Fixed

- **A rule Lapilli doesn't replay was proposed for retirement.** A cloud
  alarm that isn't PromQL, a Grafana rule on Loki: its firings, copied in,
  are kept under a name of their own, so after twelve weeks the rule read
  as never fired. With `--changes`, Lapilli proposed deleting it, its
  record empty, and the rules page listed it as never fired. Such a rule
  is neither now; one turned off still is.
- **One rule with a proposal waiting stopped the week's proposals.**
  Lapilli's own proposals stopped at the first rule a person had a
  proposal open for, and the rest weren't made that week. That rule is
  passed over now.

## [0.22.1] - 2026-11-11

Changes to rules carried out against real servers, not only stand-ins:
Prometheus 3.5, Gitea 28.3, Grafana 13.3 and GitLab CE 19.5. Two of the
paths v0.22.0 shipped didn't work against them; both are fixed, and
`test/e2e/changes.sh` now runs the git and Grafana paths against real
servers in CI.

### Fixed

- **Grafana: every change was refused as stale.** A rule was read again
  before it was changed from its own export, which Grafana 13 gives without
  its folder, so its group read differently from the export of every rule
  it was proposed against. It is read from that export now.
- **GitLab: `recipes/trackers/gitlab.sh` stopped at the first task.**
  GitLab 18 and later link an issue as a work item (`-/work_items/N`), which
  the recipe didn't take as its own, after making the issue. Both links are
  taken. Quick actions in a task's text (`/assign`, `/confidential`), which
  GitLab 19.5 was seen to carry out on an issue made through its API, stay
  escaped, as v0.22.0 made them.
- **Grafana: turning a rule off takes an Admin service account.** Grafana
  13 answers a pause from an Editor as taken and leaves the rule running.
  Lapilli found it when it read the rule back, and the change failed; now
  it also says why. The help, the design and the chart say which role is
  needed: an Editor's token changes a `for` or what a rule says.
- A Grafana rule's deletion proposed before the rule has been off four weeks
  is refused then, not once approved.

## [0.22.0] - 2026-11-11

Lapilli reads by default, and now changes only what a person approves: the
first principle in GOVERNANCE.md, rewritten with its design (#40). Changes
are off until turned on. With them: reports people schedule themselves, what
an alert says checked, and tasks kept in step with a tracker. The ledger
gains schemas 012 (reports) and 013 (proposals and approvals). Reviewed
before release as an attacker would and for correctness, twice, then
checked again once merged; what the reviews found is fixed in what is
described here.

- **Changing rules, with a person's approval** (docs/design/changes.md,
  step 1). With `--changes`, Lapilli proposes changes to rules from what it
  finds: a longer `for` for a noisy rule, a threshold or `for` for a near
  miss, and retiring a spare rule or one that never fires (off first, then
  deleted no sooner than four weeks later, approved again). People propose
  too, and agents over MCP behind `--mcp-propose`. Each proposal comes with
  its backtest over twelve weeks, on `/proposals` and the rule's page, and
  can be made into a task. A person approves it in the UI only, never
  through the API, MCP or a token, and never their own (`--approvers`;
  `--approvals 2` for two). An approved change is read and replayed again,
  then opened as a pull request by `recipes/changes/` (GitHub, GitLab,
  Bitbucket, Gitea or Forgejo, Azure DevOps), which hold the repository's
  credentials and take changes with `--changes-token-file`, or applied
  through Grafana's provisioning API (`--changes-grafana-token-file`), with
  the rule as it was kept for undo. Limits (`--changes-limits`, 5 deletions
  a day and 20 changes an hour by default) and a stop (`--changes-paused`, a
  file, or an admin's switch, looked at again just before each write) apply.
  One proposal waits for a rule at a time, and a person, with their agents,
  makes twenty an hour at most. A change sent to Grafana is kept as
  `applying`, with the rule as it was and counted against the limits, until
  it is read back. A Grafana rule is deleted only after its disable, and
  only while it is still off. The change recipes take only their own pull
  requests (same repository, into `BASE`, by the token's own account; Gitea
  tokens need `read:user`, and `GITHUB_LOGIN` names a GitHub App), and
  write only files under `RULES_PATHS`, never one a CI system runs, so a
  Lapilli someone took over can't push a workflow that would run with the
  repository's secrets.
  A pull request changed in review is `amended` (told as `change.amended`)
  rather than holding up its rule, and an admin can close one stuck opened,
  merged or applying, with a reason. Raising `--approvals` holds proposals
  approved by fewer people. `--changes` needs roles (`--editors` or
  `--admins`) and someone to approve (`--approvers` or `--admins`). The
  recipes take the pull request already open from their branch when the host
  refuses another; a proposal's task shows its pull request but keeps its
  own link. The chart adds
  `serve.changes`; its pause is a ConfigMap named `FULLNAME-changes-paused`,
  and `helm install` prints the command that makes it.
  A fix to what a rule says is proposed the same way: one annotation
  (`summary`, `description`, `runbook_url` or `runbook`) set to text a
  person or an agent wrote, or taken out, offered on the rule's page beside
  each content-check finding text can fix and carrying that finding as its
  evidence; it is written as one quoted line, the rest of the file byte for
  byte, and must parse as a template in the rule's language, call only that
  language's own functions (for Prometheus, those every version since 2.45
  has) and no `query`, and can't grow without bound when the alert fires
  (no `range`, no templates, no variables set; an escape or a replacement
  only on a value as it is). Lapilli never writes the text itself.
- **Reports people schedule themselves** (docs/design/reports.md). A Reports
  page and `/api/v1/reports` take reports on a preset (`daily@09:00`,
  `weekly mon@09:00`, `monthly 1@09:00` or `last`) or a cron expression,
  hourly at most, in the ledger's zone or the team's, over the calendar span
  before it or a span given (`24h`, `7d`). A report is for everyone or one
  team and holds the sections picked: summary, noise, missed, woken,
  notifications, replay, sources, tasks, content and, for a week or longer,
  suggestions. It goes by name, never a URL, to `--report-dest NAME=FILE`
  (the chart's `serve.reports.destinations`), `slack`, `team:NAME`, or
  `webhook` as `report.ready`. It is sent once for each span through the
  outbox, when the ledger has the span, or after it fell due by six hours or
  half the time to the one before, whichever is less; after a restart, only
  the last one due, if it isn't later than that time again. A time daylight
  saving skips falls due when the clocks jump, and a repeated time the first
  time. A report whose team is removed stays stopped. Admins write reports;
  everyone sees them and a preview. The weekly digest is unchanged and shown
  with them.
- **What an alert says, checked** (docs/design/content.md). Lapilli now
  reads what each rule says when it fires: summaries, descriptions, runbook
  links, templates and severities, from every source's own fields. Azure
  Monitor's alert rules and OCI's alarms are now read as rules too, listed
  but not replayed. It checks them. Facts: no summary, no runbook, a
  template that doesn't parse, no severity or one not in `--severities`, the
  same condition as another rule. Measured: a template label no firing had,
  a noisy rule with no `for`. Guesses, kept apart, each with why: a label an
  aggregation can't give, a rare severity, a range shorter than two scrapes,
  `absent()` with no `for`. Templates are parsed, never run; links are
  checked, never visited. Each rule's page has "What it says", the rules
  list marks problems of fact, `/metrics` has
  `lapilli_rule_content_problems{rule, check}`, and MCP carries it all as
  data; a report's `content` section gives the problems now, by check. The
  check takes time in proportion to an expression's length, is worked out
  again only when the rules or firings change, and leaves out a rule it
  can't read rather than failing. `lapilli verdict --check-content` does the
  same without a ledger. `--check-content=false` turns it off.
- **Tasks kept in step with a tracker** (docs/design/trackers.md).
  `--tasks-token-file` lets a job, on the `--incidents-listen` port, read
  tasks (with who set each one's status, `status_by` and `status_at`, and
  `updated_after=`) and set their link and status, and nothing else. The
  link must be https, at most 2,048 characters, and is set once. The token
  has its own rate and turn and is refused if another token's file holds the
  same value.
  Its writes are recorded as `tasks-token`, don't count as anyone's use, and
  aren't sent to the notice webhook. `recipes/trackers` has scripts for
  GitHub, GitLab, Jira and Linear. Each run makes an issue for every open
  task without one and links it. A task follows its issue when the issue is
  closed, and reopens if the job had closed it. With `CLOSE_ISSUES=true`, an
  issue is closed when a person closes its task. A recipe keeps in step only
  the issues its own account made, labelled `lapilli` where labels are by
  name: an issue filed ahead with a task's marker, or one linked by hand, is
  neither followed nor closed. An issue is a task's only by the recipe's own
  last marker line, which the task's text can't forge; Jira team labels are
  `lapilli-team-NAME`. A GitHub App or Actions token names its account with
  `GITHUB_LOGIN`. A task the tracker refuses is skipped and the run fails at
  the end; a deleted issue is reported and its task left as it is. The
  chart's `serve.trackers` runs the script as a CronJob. Task links typed in
  the UI can now be 2,048 characters long.

### Security

- **`go 1.26.11`.** Go's `net/http` could crash an HTTP/1 server whose
  handler reads a request's body while writing its headers, past a
  `MaxBytesReader` (GO-2026-6859, CVE-2026-97054, published on the day
  v0.21.0 was). Lapilli's handlers read a body before they answer, so no
  way to reach it was found, but v0.21.0's archives and image were built
  with Go 1.27.3 and carry the flaw. This release's are built with Go
  1.27.4, and `go install` now needs 1.26.11 at least.

### Fixed

- `lapilli serve --read-only=false` and `--notify-slack=false` were taken as
  `--read-only` and `--notify-slack`: the value after `=` was ignored. They
  now read it, as `--check-content=false` does.

## [0.21.0] - 2026-11-11

The replay set beside what really fired, as it goes; rule versions from
before the ledger began; a cloud's own alarms' notifications, every cloud
alike; and four more incident recipes. The first release reviewed as an
attacker would before it shipped, as CONTRIBUTING.md now asks.

- **The replay beside what really fired, kept** (docs/design/fidelity.md).
  `lapilli serve` sets each day the runs won't replay again beside
  `ALERTS` and `GRAFANA_ALERTS` (or `--check-series`), each firing counted
  on the day it started, and keeps it (schema 010). A rule's page says, over
  twelve weeks, how many were alike, how many only in the replay and how
  many only in what really fired, with the last of those; the rules list
  and the dashboard mark a rule whose replay differed over the last week
  (`--check-differs N/RATIO`, 5/0.9 by default), and `replay.differs` tells
  the webhook and Slack, once a week while it does. The runs page says how
  far it has got, which series held nothing, and what really fired with no
  rule by that name. `lapilli_rule_replay_check{rule, way}` in `/metrics`,
  in MCP's `rules` and `rule`, and a line in the weekly digest. No stone
  changes. On with rules to replay; `--check-replay=false`, or the chart's
  `serve.checkReplay.enabled: false`, turns it off.
- **Rule versions from before the ledger began** (docs/design/history.md).
  `--rules-history DIR`, for `serve` and `verdict`, reads earlier versions
  of the rules, one directory each, named by when it took effect
  (`2026-10-14T16:40:31Z` or `20261014T164031Z`). `verdict` reads it as each
  version given as `--rules DIR@TIME`. `serve` adds them to a ledger that
  has no version yet, so its first backfill replays each day with that
  day's rules rather than all of it with today's; a ledger with versions of
  its own keeps them, and a version that can't be read stops the start with
  the ledger as it was. `recipes/versions/git.sh` writes the directory from
  a clone: the rules as they stood at `SINCE`, then each commit on the
  branch that changed them, a merge at the merge's time, with `RENDER` for
  rules rendered from a chart. The chart's `serve.rulesHistory` runs it in
  an init container on each start.
- **A cloud's own alarms' notifications, every cloud alike**
  (docs/design/notifications.md). CloudWatch alarms' messages to SNS,
  directly or through EventBridge, are taken on `/v1/sns`, told from Amazon
  Managed Service for Prometheus's by their shape, a contributor's
  attributes among an alarm's labels. Azure Monitor's other alerts are
  taken on `/v1/azure`, their resource now the alert's target, lower-cased
  here and in `recipes/firings/azure-monitor.jq`. Google Cloud's other
  policies are taken on `/v1/google`. OCI Notifications is taken on a new
  `/v1/oci/SOURCE`, with the token as the password, checked before the body
  is read; a subscription is confirmed only from such a request, only over
  https to a Notifications API host, each URL once; the path waits two
  seconds for its turn, then answers 503. `?source=NAME` (the path for OCI)
  ties an alert to the rule copied in from that source by the name the
  alarm sent, though the ledger names the rule apart; an OCI alert, by its
  alarm's OCID, which `recipes/firings/oci.jq` now gives its firings as
  `alarm_id`. A message of an alarm resolving or repeating takes its start
  from the last notification of its group. Schema 011.
- **What takes input from outside is reviewed as an attacker would**
  before a release, as CONTRIBUTING.md now says. This release's reviews
  found, and it fixes before shipping: a notification whose alerts don't
  say their start is read with one bounded query (it held the ledger's
  writer for up to a minute with 1,000 alerts against a busy rule), from
  notifications of the same kind and labels in the last month. An OCI
  alert's alarm id names its rule when read, so a message that comes before
  its firing is copied in still ties. EventBridge's other CloudWatch events
  are kept as nothing. Azure resources are compared without case, so
  v0.20's rows still tie. The SNS certificate and OCI confirmation fetches
  reach only internet addresses, and only Oracle's own realm domains. A
  history version that links outside itself is refused, one dated in the
  future is skipped, and an empty directory, or a restart that can't reach
  git, starts with today's rules; `git.sh` keeps versions in the branch's
  order, refuses links, and runs `RENDER` without the token. `verdict`'s
  `--rules` beside `--rules-history` is the newest version. Rootly's recipe
  waits out a 429, Rootly's and FireHydrant's de-duplicate across pages,
  and Compass's stops before 20,000. The replay check counts rules that
  share an alert name together, starts from the first whole day the runs
  replayed, pairs firings across midnight alike from either day, and keeps
  a day a day where clocks skip midnight (the ledger's midnight is now a
  day's first instant there). A review of those fixes found more, fixed
  too: a history version that is itself a link is checked where it leads;
  the chart's `RENDER` runs in an init container without the token
  mounted; a commit dated after now stops `git.sh` rather than hide every
  later version; a link elsewhere in the repository no longer stops
  `RENDER`; rules that share an alert name only some days are summed day
  by day; a start is found however long ago its alarm went off; the fetches
  refuse a few more ranges that aren't the internet (NAT64, 6to4,
  benchmarking, reserved, Azure's host address) and keep no connection
  open.
- **Four more incident recipes**: Grafana IRM, Rootly, FireHydrant, and
  Compass's alerts, for Opsgenie's users as its support ends on 5 April 2027
  (its incidents move to Jira Service Management, which `jira.sh` reads).
  Each checks `SINCE` before asking, bounds its paging, and writes nothing
  when the tool fails; tested against each API's published schema with a
  server standing in, not against real accounts.

## [0.20.1] - 2026-11-10

A security release for the two ports 0.20.0 opened, after an audit from an
attacker's point of view: the notifications port, which may face the
internet, and the firings a token copies in. Nothing let anyone past a
token or a signature; what was found let them take the port down, drop
SNS's messages, or make a write cost far more than it should.

- **The notifications port read a body before its proof (high).** Anyone
  who could reach it could send many bodies of 1 MB at once, unproven, and
  run lapilli out of memory. A path a token proves is now refused before
  its body is read, and only 64 bodies signed by SNS or Grafana are read
  at once, each within ten seconds, and only when its headers say it could
  be: SNS's message type and a topic taken, or Grafana's signature. Past
  that the port answers 503, which SNS sends again.
- **SNS's messages could be made to fail (medium).** A certificate URL
  spelled many ways filled the certificate cache, and fetching spent a
  budget all shared, so a genuine message got 403, which SNS drops for
  good. A certificate is now taken only from the SNS host of its topic's
  region, spelled as SNS spells it; one that has verified a message stays
  kept; one that can't be fetched now gets 503; a URL with no certificate
  behind it doesn't use up the fetches. A message signed more than
  an hour ago isn't taken, and the same message, or the same Grafana
  signature, sent again is answered as kept without using a turn. Each
  source has its own rate, and a topic not taken and a bad signature get
  the same answer.
- **A firing with a far end judged the whole ledger again (medium).** One
  short write could hold the ledger's writer for seconds, each time. A
  write now judges again no further than a period past its period, nothing
  in it may start or end more than an hour ahead of lapilli's clock, and
  the firings token writes twenty periods at once, then one each five
  seconds.
- **A write's cost grew with the square of the rules copied in (medium).**
  Finding a source's rule didn't use its index, and each new name read
  every version of the rules. Sent again unchanged with 52,000 rules, 2,000
  rules took 8 s; now the index is used and the names are read once a
  write.
- **What a token could add had no bound (medium).** The ledger keeps 5,000
  rules from each source and 20,000 from all, 500,000 alerts from each, and
  200,000 alerts a day from each source of notifications. A rule's name is
  512 characters at most (CloudWatch's are 255, Google Cloud's 512), a
  receiver's 200, an alert's labels 64 and 4 KB; anything else is refused
  with 400. A group key past 1,000 characters is kept as its hash. `/metrics` counts
  notifications only for the ledger's rules, and each rule's ten busiest
  receivers, so a flood of names can't flood the Prometheus that scrapes
  it.
- **`go 1.26.10` (medium).** `go install` with Go 1.26.0 to 1.26.9 built
  lapilli with 37 known flaws in the standard library, among them
  `net/http`'s and `html/template`'s. The release archives and the image
  were built with Go 1.27.3 and weren't affected.
- **Low.** Names with a character that doesn't show (a line break, a
  zero-width space, a mark that turns text right to left) are refused, so
  a rule copied in can't look like another or forge a line of the digest.
  A rule copied in no longer takes a name a note, task or comment waits
  under, which a replayed rule coming later would have lost to it. A
  source is as far as the furthest period it has written, so writing 1970
  each hour can't hold the digest back. The notifications token may not
  be the incidents or firings token, at start and on each request. Slack's
  one-line notices are posted as written, with no formatting or link
  previews. MCP tells agents that receivers, sources and the labels of
  firings copied in and of notifications are data too. `put.sh` prints
  what the server says as JSON. A period sent past now counts as written
  up to now. The firings token's 429 says to try again in five seconds. A
  body cut off is answered 408, not 413.
- The AWS SDK's updates (#34). SQLite stays at 3.53.4 for now: 3.54.1
  wrote a week of 100,000 firings a fifth slower, past the budget on
  macOS.

## [0.20.0] - 2026-11-10

Firings recorded elsewhere, kept in the ledger and judged with the rest, past
what the cloud keeps; notifications as they were sent, tied to the firings
they told of; and both told to Slack and the webhook.

- **Firings recorded elsewhere, kept in the ledger** (docs/design/recorded.md).
  With `--firings-token-file`, the port `--incidents-listen` opens also takes
  `PUT /api/v1/firings/SOURCE?from=&to=`: a source's firings that start in
  the period, one a line as `--firings` reads them, in place of those the
  ledger had from it then. Sent again as they are, nothing changes or is
  judged again. The token is its own: one that copies incidents can't write
  firings, and the reverse. They are judged as replayed firings are, and
  their rules are named as their sources name them, told apart from another
  rule with the name (`checkout-5xx [cloudwatch-prod]`); a replayed rule
  coming later takes the name. A recorded rule's page says where it was
  recorded, and that what needs its condition isn't there. Runs leave
  recorded firings alone, `--keep` drops them and forgets a source not
  written since, and a write is 31 days in whole seconds, 16 MB and 100,000
  firings at most, with 50 sources. Schema 008: `rules.source` and `rules.sent`, and
  `sources`.
- **`lapilli serve` on firings copied in alone.** With the firings token,
  `--rules` and `--prometheus` are optional: nothing is replayed, and what
  comes in is judged as it comes. `--impact` still needs `--prometheus`.
- **Told to Slack and the webhook.** A source nothing has been copied in
  from for `--source-quiet` (default 26h; the chart's
  `serve.firings.quietAfter`) is told of once, as `source.quiet`, to the
  webhook and, with `--notify-slack`, to Slack. The weekly digest says which
  sources firings were copied in from, how many each brought and which has
  gone quiet, and what the notifications sent came to (how many, repeats,
  while people slept, for nothing, the rules that sent the most for
  nothing); a team's digest, for its rules. The webhook's `digest.ready`
  carries both as `sources` and `notifications`.
- `recipes/firings/put.sh` sends a recipe's output as a source's firings for
  `SINCE` to `UNTIL`. The runs page lists the sources, `/api/v1/sources`
  has them, and `/metrics` has `lapilli_serve_source_last_timestamp_seconds`
  and `lapilli_serve_source_firings` by source. The chart takes
  `serve.firings.tokenSecret`, with `rules` and `prometheus` optional then.
- **Notifications as they were sent.** `lapilli serve --notify-listen ADDR`
  opens a port that takes notifications, and nothing else, a path for each
  source: `/v1/alertmanager` (a webhook, with the token in
  `--notify-token-file` as its bearer token), `/v1/grafana` (a webhook
  contact point, with the token or its HMAC signature with the secret in
  `--notify-grafana-secret-file`), `/v1/sns` (Amazon SNS's signature, from a
  topic `--notify-sns` names, for Amazon Managed Service for Prometheus; the
  subscription is confirmed at an SNS host only), `/v1/azure` (an action
  group's webhook, common alert schema, the token as the URL's password) and
  `/v1/google` (a Cloud Monitoring webhook channel, basic auth after a 401).
  Each alert is tied to its rule's firing whose labels are all among its own,
  started within two evaluations; a notification the same as one from
  another replica within a minute is kept once; repeats are as the source
  says, or the same alerts again with nothing new and nothing resolved. Rule
  pages and the rules list show notifications, repeats, those while people
  slept and those for nothing, by receiver; firings, how many told of each,
  or that one paged no one; the dashboard's "Woken for nothing" counts
  notifications once there are any. `lapilli_rule_notifications{rule,
  receiver, reason}` in `/metrics`, and in MCP's `rules` and `rule`. Schema
  009. The chart's `serve.notificationsAPI`.

## [0.19.0] - 2026-11-09

A cloud's own alarms: replayed where they are written in PromQL, on every
cloud that has such alarms, and judged from their history where they aren't.

- **CloudWatch PromQL alarms.** `--rules` reads the AWS CLI's
  `describe-alarms` answer: a PromQL alarm is replayed with its pending
  period as `for`, its recovery period as `keep_firing_for` and its
  evaluation interval; metric and composite alarms are listed as not
  replayed. `--prometheus https://monitoring.REGION.amazonaws.com --auth aws`
  queries CloudWatch's PromQL API, signed for the service `monitoring`, as
  is its dual-stack (`REGION.api.aws`) or VPC endpoint name; `aps` stays for
  Amazon Managed Service for Prometheus. A period given as NaN is refused.
- **Google Cloud alerting policies with a PromQL condition.** `--rules`
  reads them as `gcloud monitoring policies list` writes them, in YAML or
  JSON, or as the API's list answers: the condition's `duration` is `for`,
  its evaluation interval (30 s when it doesn't say) the rule's, its
  `alertRule` and `ruleGroup` the name and group (the policy's display name
  otherwise), its labels the rule's, and the policy's severity becomes
  `severity`. An alert closes at the first evaluation at least the larger of
  270 s and two evaluations after its condition last held, as Google Cloud
  closes them. A disabled policy, and one with another kind of condition, is
  listed as not replayed. Replay with `--gcp-project`, whose PromQL API
  answers for the same metrics, and `--step 30s`, of which every policy's
  interval is a multiple.
- **An answer cut short is an error.** An API that caps the series in an
  answer and says so only in a warning (CloudWatch: 500) no longer gives a
  replay of what was left: the rule's query fails, saying why.
- **Firings from a cloud's alarm history.** `recipes/firings` has a script
  for each cloud that writes its alarms' history as `--firings` takes it:
  `cloudwatch.sh` (`describe-alarm-history`), `azure-monitor.sh` (Azure
  Resource Graph), `google-cloud.sh` (`projects.alerts.list`) and `oci.sh`
  (`get-alarm-history`), each with the cloud's own CLI, tested against the
  clouds' documented answers with stand-ins for the CLIs. Times are checked
  to be times before they reach a query or a filter (UTC for Azure), the
  token goes only to https, a CloudWatch move that says neither its old nor
  its new state stops the script, and a stateless Azure alert (activity log,
  service and resource health) ends as it fires.

## [0.18.0] - 2026-11-09

For an incident nothing caught, the rules that nearly caught it by one of
their several numbers; and hours that take turns, for teams on call in
shifts.

- **Nearly caught, by one of several numbers.** A Grafana range threshold
  (`within_range`, `outside_range` and their `_included` forms) has each end
  looked at, the other as it is; PromQL joining comparisons with `and`, `or`
  or `unless` has each comparison with a number looked at, the others as
  they are, four at most. The first comparison of an `and` or `unless` is
  read by its values, as a single comparison is; any other by replaying the
  rule with nearer round numbers, the furthest within a fifth first, then
  searching back towards the rule's own for the nearest that fires during
  the incident. A comparison on the right of `unless` kept the rule quiet,
  and its number moves the other way. An expression over 16 KiB, or a
  comparison nested more than 32 levels (brackets, and, or, unless), isn't
  read for numbers. The near-miss table's `CHANGE`
  names the comparison (`rate(requests[5m]) > 10 → 8`, `unless requests < 10
  → 9`); JSON's `value` gains `of` and `unless`, and MCP the same.
- **Shifts.** For a team on call in shifts, or by turns across time zones,
  `--shifts` (the chart's `hours.shifts`), or a team's `shifts` in `lapilli
  serve` (the teams page and API; schema 007), gives hours that take turns:
  how long each turn is, in hours, days or weeks, when the first starts, and
  each turn's working hours, hours asleep and zone, those it leaves out as
  they are otherwise, `-` for a turn on them all (`every=1w
  from=2026-11-02T09:00; tz=Asia/Seoul; tz=Europe/Berlin sleep=23:00-07:00`).
  The first turn starts from 1970 to 2200. A firing is counted asleep or off
  hours by the turn it came in. A team's own hours or shifts replace the
  server's shifts. Reports name the shifts (`hours.shifts` in JSON).
- Working hours may run past midnight, as a night shift's do:
  `mon-fri@22:00-06:00` works the mornings after. They were refused.
- A near miss by value whose value wasn't read leaves out `reached`, in JSON
  and MCP; the table shows `–`.
- A firing a near miss's window made up at its start, the condition holding
  from before it, and what kept on from it through a dip by
  `keep_firing_for`, isn't taken for one that caught the incident: when it
  really started isn't known. Windows start further before the incident,
  and a near miss by `for` is checked by replaying the rule with the shorter
  `for`, as `keep_firing_for` keeps a firing on. Both were so in 0.17 too,
  with a group interval longer than the step, `keep_firing_for`, or a lead
  over a day.
- In `lapilli serve`, hours that change more than six times a day are counted
  with one read of the firings, row by row, rather than a query for each
  stretch.

## [0.17.0] - 2026-11-09

For an incident nothing caught, the rules whose value nearly reached their
threshold; near misses for incidents written down after the digest; and
working and sleeping hours for each team.

- **Nearly caught, by value.** For an incident nothing caught, a rule whose
  condition compares a value with a number (`errors > 0.9`, `0.9 < errors`, a
  Grafana threshold or a math expression with one number) nearly caught it
  when its value came within a fifth of the number for the rule's `for`. The
  report gives the round number short of what the value reached that would
  have caught it (`> 0.9 → 0.87`), when the rule would then have fired, and
  what it would cost over the period, as near misses by `for` do. A rule with
  no `for` is looked at this way too. In `lapilli verdict`'s report and
  `--json` (`value`), and in serve on the incident's page, in the digest and
  through MCP (`op`, `threshold`, `reached`, `new_threshold`).
- **Near misses for incidents written down later.** `lapilli serve` works out
  the near misses of a week again when its incidents change after the digest,
  or as they are written down in the week under way, within the hour, rather
  than at the next digest; and once more when the ledger's firings reach the
  end of an incident written down before it was over. Only the incidents that
  changed are looked at again; when the history fails, what was kept stays,
  and they are tried again later.
- **Hours for each team.** In `lapilli serve` a team can have its own working
  hours, hours asleep and time zone (the teams page, or `work_hours`,
  `sleep_hours` and `tz` in the teams API; each empty one is the server's).
  Its rules' firings are counted by them on the rules pages, the overview's
  tile, `/metrics`, `/api/v1/report`, MCP and its digest; a report says which
  teams went by their own hours (`hours.teams` in JSON). Schema 006 adds the
  columns.
- The near-miss table's `FOR` column is now `CHANGE`: `for 30m → 10m`, or
  `> 0.9 → 0.87`.
- The teams page's "Rules matching" column showed how many rules a team owns;
  it shows the pattern.
- A rule's `for`, `keep_firing_for` or group `interval` below nothing, or past
  a hundred years, is refused when the rules are read; from a rules API it is
  taken as nothing, or a hundred years.

## [0.16.0] - 2026-11-06

When the firings that caught nothing came, those that woke people first; and,
for an incident nothing caught, the rules that nearly did.

- **When the firings came.** The firings that caught nothing are counted by when
  they started: while people slept, off hours, or at work, in `--tz`.
  `--work-hours` (default `mon-fri@08:00-18:00`) and `--sleep-hours` (default
  `22:00-08:00`) say when those are, or `none` (in the chart, `hours.work` and
  `hours.sleep`). The report lists the rules that woke people for nothing first;
  `--json` has each rule's `asleep`, `asleep_black`, `off_hours` and
  `off_hours_black`; `--metrics` has `lapilli_rule_nothing_by_hours`. In
  `lapilli serve`, the dashboard has a tile for the firings that woke people for
  nothing, the rules pages and MCP have each rule's, and so does `/metrics`.
- **What nearly caught it.** For an incident nothing caught, or only noisy rules
  did, the rules whose condition held about then but not for their `for` are
  named, with the longest shorter `for` that would have caught it, when the rule
  would then have fired, and what that would have cost over the period:
  `lapilli verdict`'s report and `--json` (`near_misses`), and, worked out with
  `lapilli serve`'s weekly digest, the digest, the incident's page and MCP. A
  rule that held long enough, but fired only after the incident was over, is
  one too. The cost is of the rules as they stood, with quiet windows excusing
  what they excuse. It is bounded: twenty incidents, five rules each, ten
  replays tried, 2,000 questions, within the period; in `serve` the suggestions
  and these share one `--timeout`.
- A noisy rule's suggested longer `for` now counts, in `lapilli serve`, the
  firings a quiet window would excuse as the ledger does.
- `/api/v1/report?firings=false` over 31 days or less reads the firings again to
  count them by the hours, so it now takes its turn, one at a time, as a report
  with every firing does. Over longer periods it counts no hours.

## [0.15.0] - 2026-11-06

What each rule adds to the others, a check that the replay is right, incidents
copied in from where they are kept, and digests for each team and for a
webhook. Read "Before you upgrade": an incidents file with nothing in it is now
refused.

- **What each rule adds.** Each rule now gets the incidents it caught, those it
  caught before any other rule, and those no other rule caught, in the report,
  `--json`, `--metrics` (`lapilli_rule_incidents`,
  `lapilli_rule_incidents_first`, `lapilli_rule_incidents_alone`), the rules
  pages and MCP. A noisy rule's catch doesn't count beside one by a rule that
  isn't noisy. A rule is called spare when, for each of two incidents or more,
  another rule that can still fire caught it as soon, at the same severity or
  above; of rules tied, the first by name stays, so all the spare rules could go
  at once. The dashboard asks whether a spare rule still earns its place, after
  the incidents nothing caught, and offers a task to retire it. With `--rule` or
  `--changed-only`, only how many incidents each rule caught is given.
- **Is the replay right?** `lapilli verdict --check-replay` sets a replay beside
  what really fired, rule by rule, from `ALERTS`, `GRAFANA_ALERTS`, or the series
  `--check-series` names. It exits 1 when nothing was compared.
- **Incidents from where they are kept.** `recipes/incidents` copies them from
  PagerDuty, incident.io, Jira and GitHub issues. `lapilli serve
  --incidents-listen` and `--incidents-token-file` open a port that takes
  incidents from whoever holds the token, and does nothing else (in the chart,
  `serve.incidentsAPI`). What it copies in is the tool's (origin `copy`): the UI
  can't change it, and the token can't change what people wrote. It takes one
  write at a time, 20 a second, and keeps 20,000 copied incidents at most; they
  aren't told to the notice webhook or Slack, since the tool they come from has
  told people already.
- **An incident written again as it was changes nothing:** no change noted, no
  notice, no judging again.
- **The weekly digest goes further.** `--team-digest TEAM=FILE` posts a team its
  own rules and incidents (in the chart, `serve.teamDigests`), and
  `--notify-webhook-file` gets `digest.ready`, for everyone and for each team,
  with the numbers and the report as `text`. Webhooks that take Slack's format
  work; any other chat can take the JSON. Over twelve weeks, the digest names the
  rules that are spare. Its lines on tasks and outcomes now come right after the
  report's first lines, so a long report cut short for Slack keeps them.
  `digest.ready` has the report as written, `text`, and escaped as Slack takes
  it, `slack_text`.
- **A rule with `keep_firing_for`** is replayed from that much earlier too, so an
  alert held on across the start of a period is seen.
- The report's "never fired" no longer counts a rule that caught an incident
  with a firing from just before the period, or whose firings a quiet window
  excused.
- An incident's labels are 4 KB at most in all, as kept, and kept as written
  rather than with `<`, `>` and `&` escaped; its times are from 1970 to 9999. A
  change to an incident's title alone judges nothing again.
- An id in the incidents file that a person's incident holds is passed over and
  reported, on the status until the file reads cleanly and in
  `lapilli_serve_incidents_passed_over`; before, it kept the whole file from
  being read. One that a copy holds, the file takes.
- The README says plainly that Lapilli counts firings, not the notifications
  they become.

### Before you upgrade

- **An incidents file with no `incidents` key is refused**, by `lapilli verdict`
  and by `lapilli serve`, where the ledger then keeps the incidents it had. Before,
  such a file, an empty one among them, read as no incidents; and since a file
  owns what it holds, `serve` took every incident from it out of the ledger. A
  job that failed while writing the file leaves just that. For a period with no
  incidents, write `incidents: []`.

## [0.14.0] - 2026-11-05

A security release, after an audit from an attacker's point of view. Read
"Before you upgrade".

- **DNS rebinding (high).** A web page could point a name of its own at a
  local `lapilli serve`, or at a `kubectl port-forward`. The browser then
  took the server for the page's own site, past the cross-origin check. The
  page could read everything, change the settings, and write or delete
  incidents. `serve` now answers only these host names:
  - an address and `localhost`;
  - a name without dots, or under `.localhost`, `.internal`, `.svc` or
    `.cluster.local`;
  - `--external-url`'s name;
  - those in `--allowed-hosts` (in the chart, `serve.allowedHosts`).

  Others get 421; the health checks answer whatever the name. Over
  loopback (this machine, a port-forward, a sidecar), names without dots
  or under `.internal`, `.svc` or `.cluster.local` aren't answered either:
  someone with a hand in the company's or the cluster's DNS could point
  them here.
- **Forged headers inside a cluster (medium).** Any pod could reach `serve`
  and set the user or groups header, and so become an admin.
  - The chart now refuses `serve.userHeader` without
    `serve.networkPolicy`, unless `serve.networkPolicy.othersKeptOut` says
    something else keeps the rest out.
  - `/metrics` and the health checks are served alone on a second port:
    `--metrics-listen`, in the chart `serve.metricsPort` (9091). The probes
    and the ServiceMonitor use it, and `serve.networkPolicy.metricsFrom`
    lets Prometheus in there and nowhere else.
- **Running out of memory (medium).** A report with every firing of a busy
  month could take gigabytes, and several at once the pod. A report now
  gives 100,000 firings at most, excused or not, as do the settings tried
  over a week. Both take one turn at a time, held until the answer is
  written, with 30 seconds to write it, so a client that doesn't read can't
  hold answers in memory. The next waits 30 seconds, then gets 503.
- **A crash from a Grafana rule (medium).** A math expression naming a
  function without its parenthesis (`log`) dereferenced nil while the rules
  were read. A Grafana alert rule could crash `serve` reading Grafana, or
  `verdict --grafana`. Fuzzing found it.
- **Slack messages forged (low).** A task's assignee and a comment's key
  went into Slack lines unquoted, so newlines in them made a message of an
  editor's own under lapilli's name. Both are quoted now. An assignee is
  one line, and a comment must be on a rule, an incident or a task that is
  there.
- **A person taking a group's role (low).** In `--editors` and `--admins`,
  `user:NAME` is now a person alone and `group:NAME` a group alone. A name
  with neither is either, as before.
- **Hardening:**
  - Task and runbook links can't carry a user name.
  - The language cookie is Secure over HTTPS.
  - The MCP instructions name every field that people or the measured
    systems wrote, as data.
- **Fuzzing.** Every parser of outside input is fuzzed: rule files and
  APIs, scoping PromQL, Grafana expressions, and incident and firing files.
  CI runs each for two minutes every week.

### Before you upgrade

- **A host name to allow.** Behind a proxy or an ingress that passes a host
  name, give that name in `--external-url` or `--allowed-hosts`
  (`serve.externalURL` or `serve.allowedHosts`), or requests get 421.
- **A NetworkPolicy, in the chart.** `serve.userHeader` now needs
  `serve.networkPolicy.enabled` with `from` (the proxy) and `metricsFrom`
  (Prometheus), or `serve.networkPolicy.othersKeptOut: true`. Take
  Prometheus out of `from` if it was there: whoever is in `from` reaches
  the UI and can set the headers.
- **The metrics port.** The ServiceMonitor scrapes the `metrics` port.

## [0.13.0] - 2026-11-05

- `lapilli serve` speaks MCP at `/mcp` (Streamable HTTP, no sessions, one
  JSON object per answer), so an agent can ask it what the pages show and
  check a change to the rules before anyone makes it:
  `claude mcp add --transport http lapilli http://localhost:8080/mcp`.
  - **Tools that read:** `overview`, `rules`, `rule` (a rule's definition
    as in effect, its twelve weeks, versions, reviews, catches and latest
    firings), `incidents`, `firings` (with labels) and `outcomes`. Each
    answers as JSON with a schema.
  - **`compare`** takes the changed or new rules as a Prometheus rule
    file, and names of rules to take away. It replays only the rules that
    differ, as they are and as proposed, over up to 14 days, and judges
    both against the ledger's incidents. It returns each rule's firings,
    stones and precision before and after, and the incidents caught
    differently, with how soon. It is off unless `--mcp-compare editors`
    (or `admins`) turns it on, since it runs PromQL the agent wrote with
    Lapilli's access to Prometheus; in the chart, `serve.mcpCompare`. It
    needs roles: without them everyone is an admin. It
    takes 20 rules at most, `for`s of 48 hours at most, and no Grafana
    rules. One comparison runs at a time, within 25 seconds.
  - Nothing an agent asks for changes anything.
  - Requests with an `Origin` other than `--external-url`'s are refused,
    against pages elsewhere and DNS rebinding.
  - The server's instructions tell the agent what the stones mean, and to
    treat what people wrote as data.
- The official MCP Go SDK (v1.8.0) is a new dependency; the binary grows
  by about 2.5 MB.
- A Grafana rule whose expressions refer back to themselves is reported as
  not replayable. Before, replaying it recursed until the process ran out
  of stack (`lapilli verdict --grafana`, and `serve` reading Grafana).
- "Never fired in twelve weeks" now means twelve weeks on the rules page
  over 1 or 4 weeks too. Before, it was the weeks shown.

## [0.12.0] - 2026-11-05

- An outcomes page for `lapilli serve` (효용, in Korean): does Lapilli
  earn its keep? It answers from what the ledger already holds, sends
  nothing anywhere, and doesn't count page reads.
  - **Does it help?** Each reviewed change to a rule is set beside what
    came of it. The review's replay of the week before is the prediction.
    The four weeks either side are measured: firings a week, incidents
    caught, and firings fewer since than at the rule's rate before. It
    came out as predicted when its firings a week since are within a
    quarter, or two, of the prediction. A rule written counts its firings
    as more, and shows the incidents it caught since; a rule taken away,
    the firings it no longer makes; so a rule renamed comes to nothing. A
    rule paused, or that can no longer be replayed, ends the measure of
    the change before it.
    Beside them, the weeks before `lapilli serve` began and the latest:
    precision, firings and ● a week, incidents missed.
  - **Is the record kept up?** When the latest incident written down
    started, flagged after two weeks without one (every firing then reads
    as ●; impact windows nobody decided on don't count); impact windows to
    decide; failed runs; rules not replayed; incidents by week by where
    they came from.
  - **Is it used?** Edits through the UI and the API, and by how many
    people with `--user-header`; suggestions acted on (a task made, or the
    rule changed); tasks done and how long they took.
- The overview has an outcomes card, and the weekly digest a line of them.
- `/api/v1/outcomes` gives them as JSON. `/metrics` adds
  `lapilli_serve_firings_avoided`, `lapilli_serve_nothing_avoided`,
  `lapilli_serve_rule_changes{outcome}`,
  `lapilli_serve_incident_last_timestamp_seconds` and
  `lapilli_serve_edits`: an alert on the last can say when incidents stop
  reaching Lapilli.
- The ledger keeps when `lapilli serve` first ran on it, and indexes each
  rule's days (schema 5). A ledger opened by this release can't be opened
  by an older one.
- Charts of small counts are ticked one by one, where before 1 incident
  could be ticked 0, 0.2, 0.5, 0.8.

## [0.11.1] - 2026-11-04

- `lapilli serve` is set in Heebo, the typeface of Argo CD's UI, served from
  the binary (its Latin, 45 KB, under the SIL Open Font License 1.1, whose
  text is served beside it). Korean uses the system's font, as before. The
  serif headings are gone; code and labels use the system's monospace. The
  CSP allows fonts from the server itself and nowhere else.
- After an upgrade, browsers fetch the new stylesheet at once: the pages ask
  for `/static/...?v=` a hash of the files, kept for good, where before the
  old one could be used for up to a day.
- A page kept by a browser is no longer answered "not modified" by another
  `serve` process on the same ledger: the ledger's generation starts again
  with each process, so after a restart or an upgrade an old page could be
  shown; every ETag now carries the process too.

## [0.11.0] - 2026-11-04

- A calmer look for `lapilli serve`.
  - **Colours:** cream surfaces like river pebbles, and a muted olive for buttons, links and the current page. Caught firings are a quiet blue, firings that caught nothing a warm grey, and the heatmap of where the noise is runs from light to deep lavender.
  - **Headings:** page headings and the dashboard's numbers are set in a serif.
  - **Layout:** the header is a pill that stays at the top; cards and tiles have soft edges; task statuses are tinted pills.
  - **Dark mode:** in the same family.
  - **Checks:** chart colours were checked again for colour blindness and contrast in both modes, and all text is 4.5:1 or more. Incident statuses keep their fixed colours, each with its icon.
  - On a phone, the header stays in place and the tiles sit two to a row.

## [0.10.0] - 2026-11-03

- Quiet windows repeat every day or every week, until a day or for good:
  a nightly batch, a weekly release. Each repeat keeps its local time
  across a change to or from daylight saving.
- Notices of what happens: a task made, assigned, its status changed, or
  overdue; an incident written down; a quiet window kept; a comment. To a
  webhook, as JSON with a link to the page (`--notify-webhook-file`,
  `--external-url`), and to Slack, as a line in `--lang` (`--notify-slack`).
  They go through an outbox written with the change, a sender for each
  place, and are tried again, further apart, for about eight hours; each
  carries an `id`, the same each time it is sent. `/metrics` counts them,
  and how many are unsent.
- Roles: with `--editors` and `--admins` (people, or groups the proxy lists
  in `--groups-header`, or `*`), only editors write, only admins change the
  settings and the teams, and everyone else may only look. Pages offer only
  what the reader may do, and say who they are; `GET /api/v1/me` too.
- A task keeps a link to its issue in a tracker. `PATCH /api/v1/tasks/{id}`
  changes only the fields given, as the task is when written, so a tracker
  can put the link back without undoing a change made meanwhile; marking a
  task's status in the UI does the same.
- The weekly Slack digest says how the tasks stand: open, overdue, done in
  the week.
- The chart takes `serve.roles`, `serve.notify` and `serve.externalURL`,
  and can add a NetworkPolicy that lets in only the sign-in proxy and
  Prometheus (`serve.networkPolicy`).

## [0.9.0] - 2026-11-03

What people keep, beside what the runs find, made and changed in the UI
and the API, in the same ledger:

- Teams own rules by a name pattern or labels, and incidents by labels;
  the dashboard, the rules and the tasks can be shown for one team.
- A rule keeps a note: its team, whether to keep, tune or retire it, its
  runbook, a note.
- Quiet windows (a deploy, maintenance, a load test) excuse the firings in
  them that caught nothing: counted apart, neither white nor black, so
  expected noise doesn't weigh on precision. A firing that caught an
  incident still counts. Reports give each rule's `excused`.
- Tasks follow up on a rule or an incident, with a kind, team, assignee,
  due date and status; a suggestion on the dashboard becomes one with a
  press, and shows when one is open.
- Comments on rules, incidents and tasks; only their author changes them.
- Every write is a same-site POST or an API call, refused with
  `--read-only`, and noted with who made it.

## [0.8.0] - 2026-11-03

- `lapilli serve`'s UI speaks Korean as well as English: the browser's
  language, or the one picked at the top of the page. Each language is one
  file of translations, and a test renders every page in every language
  and fails on a text without one.
- Larger text throughout the UI, and in its charts; on a narrow screen,
  or in a narrow card, the charts' text is larger still and every other
  week's label is left out.

## [0.7.0] - 2026-11-03

- `lapilli serve` opens on a dashboard: last week's precision, firings,
  incidents missed, time to catch and noisy rules beside the week before;
  firings, precision and incidents by week; where the noise is, rule by
  week; and what to look at first. It reads the weekly tallies, so it takes
  about 10 ms over 200,000 firings.

## [0.6.0] - 2026-11-03

`lapilli serve`: keep judging, with a ledger, a web UI, an API and metrics.

- `lapilli serve` replays the rules every hour over the hours since its last
  run, and the hour before again for points that came late, and records the
  firings in a ledger (one SQLite file, through the pure-Go
  `modernc.org/sqlite`). It reads the rules every minute and records each
  change as a version, so each hour is replayed with the rules as they stood;
  a change is replayed over the week before it, as it was and as it is.
  Once a week it judges the week before, suggests longer `for`s, and posts
  to Slack. On first start it replays `--backfill` (28 days); it keeps
  `--keep` (400 days).
- The ledger keeps each firing's stones, given against every incident it
  has, and each rule's by day and week: twelve weeks read as fast as one,
  and a change gives stones again only near it. Over random weeks its
  verdict is `verdict.Judge`'s. With two million firings an hourly run
  takes 0.18 s, writing an incident 52 ms, reading twelve weeks 2 ms.
- A rule a run couldn't replay, a query failing or time running out, is
  replayed by later runs from where it was missed; a run that runs out of
  time is tried over half of what is left.
- A web UI, rendered on the server with no scripts: the stones by week, each
  rule's with its versions and change reviews, incidents (written down
  there, impact windows confirmed or dismissed, a black stone said to be
  real), the firings by day, the settings that only judge the ledger again
  (tried on the last week before saving), and the runs. An API gives
  `lapilli verdict --json`'s report for any period and takes incidents;
  `/metrics` gives the weekly metrics without a Pushgateway.
- It listens on localhost, has no sign-in of its own, records changes under
  the header a sign-in proxy sets (`--user-header`), refuses posts from
  other sites, and with `--read-only` takes no changes.
- The chart's `mode: serve` runs it with its ledger on a volume; a Grafana
  dashboard of the metrics ships with the chart.

## [0.5.1] - 2026-11-03

- Release binaries keep their symbol table, so `govulncheck -mode=binary`
  can tell which vulnerable functions one holds. Without it, v0.5.0's
  binaries are reported as using `golang.org/x/crypto/openpgp`
  (GO-2026-5932), which lapilli doesn't import; its code is not affected.
- A release is published only if govulncheck finds nothing in the code or
  in any of its binaries, and the latest release is checked again every
  day. SECURITY.md says how fixes ship and how to check the binary you have.
- Dependabot follows the Go modules and the image's base too, two weeks
  after a release, and security updates at once.
- The chart has a README, an icon, and the annotations Artifact Hub reads,
  among them the image it scans for each chart version.

## [0.5.0] - 2026-11-03

What to change, replays closer to the real thing, and rules read where they
live.

- What to change: each noisy rule is replayed again with a longer `for`, and
  the shortest that still catches every incident it caught while firing at
  most half as often is suggested, with how much later those incidents
  would be caught. Rules show their `severity` label in reports,
  comparisons (firings counted by severity before → after) and `--metrics`.
  `--noisy-firings` and `--noisy-precision` set when a rule counts as noisy.
- Rules that share a name in one set, in different groups or with
  different labels, are told apart (`HighErrorRate [severity=critical]`),
  alike in every set compared or replayed.
- `--compare --changed-only` replays only the rules that differ. A directory
  with no rules in it is an empty set when comparing.
- `keep_firing_for` holds a firing alert past its condition, a group's
  `interval` is when it is evaluated (when a whole number of `--step`s), and
  Grafana's classic conditions are replayed, written as one PromQL
  expression the way Grafana evaluates them.
- `--loaded-rules` replays the alert rules the API at `--prometheus` has
  loaded (Prometheus, Thanos, Mimir, vmalert, Amazon Managed Service for
  Prometheus), and `--grafana URL` those a Grafana holds, through its
  provisioning export API. The chart takes `rules.loaded` and
  `rules.grafana`.
- `--rules` reads each cloud's managed rule groups: an AWS rule groups
  namespace, Azure's prometheusRuleGroups (scoped to their `clusterName`),
  and Google Cloud's Rules (scoped to their namespace), ClusterRules and
  GlobalRules; `kubectl get -o yaml` lists; and the directories below a
  directory, or behind a link to one.
- The README has `--impact` expressions for Istio, Linkerd, Envoy,
  ingress-nginx, Traefik and OpenTelemetry HTTP server metrics.

## [0.4.0] - 2026-11-03

- `--compare FILE`: judge a change to the rules before it is merged. The
  rules in place and those proposed are replayed over the same period and
  judged against the same incidents; the report shows each rule that
  differs, its stones before → after, and each incident caught differently.
  Rules alike in both are replayed once. The README has a pull request
  check to copy.

- Columns line up when rule or incident names are in Chinese, Japanese or
  Korean, whose characters take two cells in a terminal.

- Oracle Cloud: it has no managed Prometheus, so on OKE lapilli reads the
  Prometheus you run there, and `--auth oci` says so. The README says which
  of a cloud's own alarms aren't read yet: CloudWatch, Azure Monitor, Google
  Cloud alerting and OCI Monitoring alike.

## [0.3.0] - 2026-11-02

- The managed Prometheus of every major cloud: `--auth aws` signs requests to
  Amazon Managed Service for Prometheus with SigV4, `--auth azure` sends a
  token for Azure Monitor managed service for Prometheus, and `--auth google`
  one for Google Cloud Managed Service for Prometheus, each from the
  credentials the machine or pod already has. `--gcp-project` stays, short
  for Google Cloud's URL and `--auth google`. The chart takes
  `prometheus.auth`, `prometheus.awsRegion` and `podLabels`, and documents
  the workload identity of EKS, AKS and GKE alike.

## [0.2.0] - 2026-11-02

Rules as they stood at each time, incidents by service, replays three times
faster, and a weekly run.

- `--rules FILE@TIME` replays the rules as they stood at each time, for a
  period in which they changed. Where a version takes over, alerts carry over
  as Prometheus and Grafana carry them over on a reload.
- A Grafana reduce over a window sees the data from before the replay starts,
  as Grafana's query would, instead of a window cut short at the start.
- A rule is listed as not replayed if any rule of that name couldn't be.
- An incident's label can list several values (`service: [a, b]`), any of
  which agrees for `--match`.
- `--impact` looks at each series on its own, and its incidents carry the
  series' labels: by service, an incident says which services were hit, so
  `--match` works for them too. An impact window must now last
  `--impact-for` in one series. In `--json`, incident labels are lists.
- Replays are faster and take less memory. Rules that need the same query
  over the same day share one request; a day's queries are asked for
  together; Grafana math pairs series by key rather than each with each; and
  `--parallel` (8) rules run at once, the busiest first. A replay is fetched
  and evaluated a day at a time, so a long period needs no more memory than a
  day.
- A Helm chart runs `lapilli verdict` every week as a CronJob
  (`oci://ghcr.io/lapilli-project/charts/lapilli`), and an image holds the
  release's Linux binaries (`ghcr.io/lapilli-project/lapilli`); both are
  attested like the archives.
- `--push URL` puts the metrics on a Pushgateway (job `lapilli`, or the group
  a URL ending `/metrics/job/...` names), and `--slack-webhook-file FILE`
  posts the report to a Slack incoming webhook whose URL is in the file; the
  URL is never shown, errors included, and redirects aren't followed.
- An incidents file with the same id twice is refused.
- `--rules DIR` reads every file with alert rules in a directory, such as a
  mounted ConfigMap.

## [0.1.0] - 2026-11-02

The first release of `lapilli verdict`, which judges alert rules against what
actually went wrong.

- Firings from replaying alert rules against a Prometheus-compatible API
  (`--rules`), from the `ALERTS` series (`--alerts`), or from a JSONL file
  (`--firings`).
- Rules from Prometheus rule files, `PrometheusRule` resources, Grafana
  alerting provisioning files, and ConfigMaps holding them. Grafana rules are
  evaluated the way Grafana does: reduce, threshold and math, with
  comparisons giving 1 or 0 and sets of series joined by labels.
- Incidents from a YAML file and from windows where an impact expression
  holds (`--impact`).
- A white stone for each firing that caught an incident, a black one for each
  that caught nothing; refired alerts; incidents no rule caught, and how long
  each took to catch.
- Catches by noisy rules (10+ firings, under 20% catching anything) are marked;
  incidents caught only by them are reported apart.
- Text, JSON (`--json`) and Prometheus metrics (`--metrics`).
- Busy APIs are asked again; answers too large are asked for in halves; a
  replay that runs out of `--timeout` still reports what finished.
