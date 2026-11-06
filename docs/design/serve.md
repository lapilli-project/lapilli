# `lapilli serve`: a ledger of verdicts

Status: built, for v0.6.0. This is how it works and why, and the rules that
keep it fast as more is added.

## Why

`lapilli verdict` runs, prints its report and is gone. What it leaves is a
Slack message and a few numbers on a Pushgateway, and so it can't do three
things a team needs week after week:

- **Show a trend.** "This rule has been noisy six weeks running", or "since
  its `for` went from 5m to 15m it fires 13 times a week, not 39", needs last
  week's verdict beside this week's.
- **Take incidents written down later.** Incidents are written down hours or
  days after, if at all. Monday's run never sees the incident written on
  Tuesday, and the thinner the incident log, the worse every verdict is.
- **Replay the rules as they stood.** The weekly run replays the rules as they
  are when it runs. `--rules FILE@TIME` fixes that, but someone has to know
  when each rule changed.

`lapilli serve` keeps the urn instead of emptying it: a ledger of the
firings, the incidents and every version of the rules, recorded as they
happen, and judged for any period when someone looks.

## What stays the same

- **One binary.** `lapilli verdict` and the CronJob chart stay as they are;
  `serve` is another command in the same binary and image.
- **One engine.** `serve` replays with `internal/replay` and gives stones
  with `verdict.Catches` and `verdict.Refired`, the rules `verdict.Judge`
  follows. A test holds its verdict on random weeks to `verdict.Judge`'s.
- **Read only, outward.** It reads history, rules and incident files, and
  sends only the weekly digest and notices of what people did, where it is told
  to. It doesn't edit rules, route alerts, page anyone or manage incidents.
- **Not a controller.** It reads mounted ConfigMaps and HTTP APIs. It doesn't
  watch the Kubernetes API and needs no RBAC.
- **Every cloud alike.** The same `--auth aws`, `azure` and `google`, and on
  Oracle Cloud the Prometheus you run on OKE.

## Shape

```
                       ┌──────────────── lapilli serve ────────────────┐
rules: files, API, ───▶│ watcher  every minute: a new version of the   │
       Grafana         │          rules when they change               │
history: Prometheus ──▶│ worker   one job at a time: hourly runs,      │──▶ web UI
incidents: file, UI ──▶│          change reviews, the weekly digest,   │──▶ /api/v1 (JSON)
                       │          judging again, pruning               │──▶ /metrics, for any Grafana
                       │ ledger   SQLite: facts, stones, tallies       │──▶ Slack, weekly
                       └───────────────────────────────────────────────┘
```

Packages: `internal/source` reads history and rules for both commands;
`internal/ledger` is the ledger; `internal/serve` the watcher, the worker,
the UI, the API and the metrics; `cmd/lapilli/serve.go` the flags.

## The ledger

One SQLite file, through the pure-Go driver `modernc.org/sqlite`: no cgo, so
the image stays distroless and static. It adds about 5 MB to the binary.
Also considered: `github.com/ncruces/go-sqlite3` (about 6 MB); bbolt (no SQL:
harder for the next maintainer to read and query); JSON files (no
transaction to replace a period's firings in).

| Table | A row is |
|---|---|
| `versions`, `docs` | a version of the rules, from when it was seen, with the documents it was read from (each kept once, gzipped) and the name each rule is judged under |
| `rules`, `series` | a rule; an alert, that is a rule and a label set |
| `firings` | an alert from start to end, and its stones: `white`, `refired` |
| `incidents` | from the incidents file, the UI, or `--impact`; for an impact window, whether it was confirmed or dismissed, and whether it counts |
| `catches` | each rule's first firing that caught an incident |
| `days`, `weeks` | each rule's stones by the day and the week its firings started |
| `runs`, `results` | what each run did; what needed Prometheus: suggestions, change reviews |
| `settings`, `changes` | judging settings changed in the UI; who changed what, when |

Times are Unix seconds. Days and weeks are counted in `--tz`; opened in
another zone, the ledger counts them again. The schema changes through
numbered SQL files embedded in the binary, applied at start; a ledger
written by a newer `lapilli` is refused, not touched.

### Stones are kept, and given again only where they can change

A firing's stones depend on the incidents near its start (white) and on the
same alert's previous firing (refired). The ledger keeps them, and every
write ends by giving stones again over the span it changed, and only there:

- a run: the firings from its period on;
- an incident written, changed or deleted: the firings from `--lead` before
  its start to its end, before and after the change;
- an impact window confirmed or dismissed, or covered by an incident
  written down: the same, for the window.

For that span it gives the firings their stones, works out again which
rules caught each incident whose window meets it, and counts those days and
weeks again. Everything happens in one transaction, or none of it does.

A firing is judged against every incident the ledger has, so a firing
just before a week starts can catch an incident in that week; `lapilli
verdict` over that week alone sees only the week's firings. Inside a period,
the two agree.

A change to `--lead`, `--match` or `--refire` gives every firing its stones
again: the worker does it a week at a time, each week its own transaction,
so UI writes get their turns, and the page says how far it has got. The
noisy thresholds aren't kept in stones; they are read when stones are
counted, so changing them costs nothing.

## Runs

Every `--every` (default 1h) a run replays from the end of the last run, less
`--settle` (default 1h) for points that arrived late, to now. Each rule's
replay starts earlier by its `for`, `keep_firing_for` and group interval,
rounded up to 15m, 1h, 6h, 1d or 7d, so a firing's pending time is seen;
rules that start alike are replayed together and share their requests, the
latest first. The `--impact` query goes before them, with time of its own.
The run then, in one transaction:

1. ends the alerts the ledger has as still firing that the replay saw end,
   and those it no longer sees at all;
2. replaces the firings that start in its period;
3. does the same for the windows `--impact` finds, keeping what people
   decided about the windows they overlap;
4. gives stones again over the period, and moves the cursor.

Recording the same hours twice changes nothing. A firing the replay saw
start from pending is matched only to the same firing; one it saw already
on, to the alert the ledger has on.

A rule whose queries failed, or that the run had no time left for, keeps
the firings it had, and the ledger keeps where it was missed: later runs
replay that rule from there (for up to a week), so no hour of it is lost.
While catching up (the first start replays `--backfill`, 28 days; or after
being down), a run covers a day at a time; one that doesn't finish within
`--timeout` is tried over half of what is left, and the next grows again
only after one that finished.

Each hour of history is replayed about twice (once when new, once settled),
plus each rule's margin, so `serve` asks Prometheus for about two to three
times what the weekly CronJob does, spread over the week instead of all on
Monday morning.

### Versions of the rules

The watcher reads the rules every `--watch` (default 1m): the files of a
mounted ConfigMap, `/api/v1/rules`, or Grafana's export; when the
documents are as they were, that is all. When the digest of the rules (every rule's
`Key`) changes, it records a new version, in effect from then: the
`--rules FILE@TIME` a person would otherwise have to work out. A rule keeps
the name it had in the version before (by group, name, labels and uid); a
new one sharing a name is named apart alongside the rules before. The rules
are read before the first run, and runs wait until a version is recorded;
the rules before the first start are taken to be those found then.

### Change reviews

Each new version is replayed over the week before it, the rules that changed
as they were and as they are (`--compare --changed-only`), and the comparison
is kept for the rule's page and the overview. The ledger keeps the last
version reviewed, so a restart doesn't drop a review.

### The weekly digest

At `--digest` (default `mon@09:00`, in `--tz`), once the week before is all
in the ledger, `serve` judges it, replays its noisy rules with longer
`for`s for suggestions, and keeps them, once; then posts the same report the
CronJob posts to Slack, trying the post again at most hourly until it goes.
A digest missed while `serve` was down is worked out when it is back.

It also looks at the week's incidents that nothing caught, or only noisy rules
did, as `lapilli verdict` does: for each of the first twenty, it asks when each
rule in effect then held its condition, on the steps the runs replayed it on,
from the rule's `for` before the incident to as long after, within the week,
and keeps the longest shorter `for` that would have caught it, five rules at
most for each. The first ten such rules tried are replayed over the week with
it, as each version stood and with the quiet windows excusing what they
excuse, to say what it would cost. A rule the same across versions is asked
once, the questions are 2,000 at most, and the suggestions and these share one
`--timeout`, so the digest holds the runs up no longer than a run may. They are
kept as a result (`near_misses`) beside the suggestions, not in a table of
their own, and shown on the incident's page, in the digest (a team's, for its
incidents) and through MCP. Like the suggestions, they are worked out once: an
incident written down for that week after the digest gets none, and
`lapilli verdict` works them out for any period.

The numbers and the text also go through the outbox ([work.md](work.md)), once
(a webhook added on a Friday doesn't hear of Monday's digest):
`digest.ready` to the webhook, for everyone and for each team something
happened to, and to each team's own webhook (`--team-digest TEAM=FILE`), its
own rules and the incidents it owns. A team's URL is in a file named on the
command line, not worked out from a team's name, which anyone who may change
teams can write. A week rarely has the incidents to call a rule spare, so the
digest says which are over twelve weeks instead. `digest.ready` carries the
report as written (`text`), which a receiver escapes for where it sends it, and
as the Slack digest posts it (`slack_text`).

### Outcomes

The reviews are predictions, and the ledger holds what came after them, so
`serve` can say whether a change did what its review said, from what it
already keeps. A rule changed at a time is measured over up to four weeks
before it (from its last change, if later) and up to four after (to its
next): firings a week, incidents caught, and firings fewer since than at
its rate before (none, for a rule written: a rename nets out). It came
out as predicted when its firings a week since are within a quarter, or
two, of the review's after, scaled by how the weeks before compare with
the review's week. Under a week either side, it is still being measured.
A rule that fires alike in the review makes no prediction, and the change
before it goes on being measured; one paused, or that can no longer be
replayed, ends that change's measure and has none of its own.

Beside the changes: the four weeks before `serve` first ran (kept in the
ledger from the first run on), or its first four with less kept from
before, and the latest four; whether the record is kept up (the latest
incident written down, flagged after two weeks without one, impact
windows to decide, failed runs, rules not replayed); and whether it is
used (edits through the UI and API, by whom with `--user-header`,
suggestions a task was made for or whose rule changed, tasks done). Page
reads aren't counted, and nothing is sent anywhere: the outcomes are on
`/outcomes`, summed up on the overview and in the digest, at
`/api/v1/outcomes` and in `/metrics`. They are worked out once for each
version of the ledger, for all of these; a rule's days are indexed by rule
so that measuring a change reads only its own.

## Incidents and settings

Incidents come from `--incidents FILE` (read again each run; the file owns
them), from the UI and API (ids `L-1`, `L-2`, … unless given), and from
`--impact`. An impact window can be confirmed, which writes it down as an
incident, or dismissed; one an incident written down covers doesn't count.
On a black stone, "this was real" writes down an incident from the firing's
start, end and `--match` labels.

An incident written again as it was, to the second, is no write: no change
noted, no notice, no judging again, and no page thrown away. That is what lets
a job copy incidents from where they are kept, every hour. An incidents file
with no `incidents` key is refused and the ledger keeps what it had, since a job
that failed leaves an empty one; the file owns what it holds, and it would
otherwise take every incident away.

Such a job has a port of its own, `--incidents-listen`, that takes
`PUT` and `DELETE /api/v1/incidents/{id}` and `POST /api/v1/incidents` from
whoever holds the token in `--incidents-token-file`, and has nothing else on it.
Letting the job reach the UI's port instead would let it name itself anyone, in
the headers the sign-in proxy sets; here they mean nothing, and what it writes
is noted as `incidents-token`, which outcomes don't count as use, and new
incidents from it aren't told: the tool has told people already, and a first
copy of months of history shouldn't go one line at a time. The name can't be
taken through the sign-in proxy's header.

What it copies in has an origin of its own, `copy`, and is the tool's, as the
file's incidents are the file's: the UI and API on the other port can't change
it, and the token can change nothing else. So whoever holds the token can't
quietly move or take away an incident a person wrote, and a tool's id that
happens to be `L-1` can't write over one. The incidents file is whoever runs
`serve`'s, so it takes an id a copy got to first; an id a person wrote, or an
impact window's, is passed over and the rest of the file read. That stays on the
status until the file reads cleanly, and `/metrics` counts it
(`lapilli_serve_incidents_passed_over`).

The port takes one write at a time, 20 a second, with bursts of 40, and answers
429 past that: each write holds the ledger's one writer, and is kept. The
ledger keeps 20,000 incidents copied in at most, and notes the token's changes
without their bodies, since the tool keeps what its incidents were. An
incident's labels are 4 KB at most in all, as they are kept, and its times from
1970 to 9999 in UTC; a change to an incident's title alone judges nothing again. The token is read for each request, so it can
be rotated, refused when shorter than 16 characters, and both are hashed before
they are compared. The port is off unless both flags are given.

**What needs Prometheus again is set where `serve` starts; what only judges
the ledger again can also be changed in the UI**: `--lead`, `--match`,
`--refire`, `--noisy-firings`, `--noisy-precision`. A change is kept with who
made it, can be tried on the last week before it is saved, and
`/settings.yaml` gives the changes as Helm values to commit.

## Web UI, API, metrics

Pages are rendered on the server with `html/template`, embedded in the
binary with their CSS: no scripts, no JavaScript build. Charts are SVG drawn
on the server. Pages are in the reader's language (English or Korean, from
a choice kept in a cookie or the browser's), kept apart in the cache by it;
a language is one JSON file of translations.

- `/`: a dashboard of the last twelve weeks, read from the weekly tallies
  and the weeks' incidents: last week's precision, firings, incidents
  missed, median time to catch, noisy rules and the firings that caught
  nothing while people slept beside the week before, each with its weeks drawn
  small; firings (caught something, caught nothing),
  precision and incidents (caught, caught only by noisy rules, missed) by
  week; the noisiest rules' black stones, rule by week; what to look at
  first; and the same numbers as a table. Its colours were checked for
  colour blindness and contrast in both modes, and every chart has a legend
  and the table, so no colour carries a meaning alone.
- `/rules`, `/rules/NAME`: each rule's stones over 1, 4 or 12 weeks, how long
  it has been noisy, its suggestion, what it adds to the others, how many of
  its firings that caught nothing came while people slept or off hours; a
  rule's versions, reviews, catches and latest firings. When firings came is
  worked out as pages are read, from when each started: by the hours given,
  `--work-hours` and `--sleep-hours`, in `--tz`. SQLite counts a stretch of
  hours at a time along the index on starts, and each week is kept until the
  ledger says that week's firings changed, so a run counts the last week or
  two again, not twelve. `/api/v1/report` counts them for periods of 31 days
  at most, one such report at a time, as reports with every firing are.
- `/outcomes`: whether Lapilli earns its keep: the reviewed changes, measured;
  then and now; the record; the use (see Outcomes).
- `/incidents`, `/incidents/ID`, `/firings?day=`, `/settings`, `/runs`.
- `/api/v1/report?from=&to=`: `lapilli verdict --json`'s report for a period
  (up to 31 days with every firing, a year without: `firings=false`).
  `/api/v1/incidents` (GET, POST, PUT, DELETE), `/api/v1/rules`, `/runs`,
  `/settings`, `/outcomes`.
- `/metrics`: `--metrics` over the last seven days, and `serve`'s own (the
  cursor, how long the last run took, runs by kind and result; the firings
  the reviewed changes avoided, the changes by how they came out, when the
  latest incident written down started, the edits of four weeks). The chart
  ships a Grafana dashboard of them that works on any Prometheus-compatible
  data source.

### MCP

`/mcp` serves the [Model Context Protocol](https://modelcontextprotocol.io)
over Streamable HTTP, with the official Go SDK. It keeps no sessions, and
each answer is one JSON object, so any proxy in front passes it as it
passes the API.

The tools are the pages' views turned into JSON with a schema: `overview`,
`rules`, `rule`, `incidents`, `firings`, `outcomes`. They read through the
same code as the pages, in English.

`compare` takes changed or new rules as a Prometheus rule file, and names
of rules to take away.

- **Matching.** Each rule given takes the place of the rule of its name in
  effect. Where several share a name, its group picks one. A rule matching
  none is added.
- **Replay.** It replays only the rules that differ, both ways, over up to
  14 days before now less `--settle`. This is what the review of a change
  does (`replayChange`).
- **Limits.** It takes 20 rules given and 50 taken away at most. A `for`
  or `keep_firing_for` (which start a replay that much earlier) is 48 hours
  at most, an interval an hour, and Grafana rules aren't taken. It has 25
  seconds, so that it answers within one request and within the time a
  sign-in proxy waits, and one comparison runs at a time.
- **The expression.** A rule's definition (the `rule` tool) carries its
  expression as replayed, with any scope its source adds, such as a
  namespace. The agent is told to copy it from there rather than from a
  file.

Everyone who reaches `/mcp` gets the tools that read: what they give, the
pages and the API give too, less the files rules were read from.
`compare` is off unless `--mcp-compare editors` or `admins` turns it on,
because it runs PromQL the agent wrote with Lapilli's access to Prometheus;
and the flag is refused without `--editors` or `--admins`, under which
everyone would be an admin.
That PromQL can learn, a count at a time, whatever that access can read.
`--read-only` gives no one `compare`.

A tool's panic is turned into its error, as `net/http` does a page's.

A browser always sends `Origin` and an agent doesn't, so a request with an
`Origin` other than `--external-url`'s is refused. A page elsewhere, or a
name a page pointed at this server (DNS rebinding), can't use an agent's
access. The SDK's own localhost check is off: behind a sign-in proxy on the
same host, requests arrive at 127.0.0.1 under the proxy's name. Bodies are
1 MB at most.

The server's instructions tell the agent what the stones mean, the order
to use the tools in, and to treat incident titles and notes as data, not
as instructions.

## Security

- **Closed by default.** The binary listens on `127.0.0.1:8080`; the chart's
  Service is a ClusterIP. There is no sign-in of its own: reach it with
  `kubectl port-forward`, or behind the sign-in proxy you have. Listening
  beyond localhost without `--user-header` or `--read-only`, it warns.
- **Who.** With `--user-header`, changes are recorded under the name that
  header carries; otherwise as anonymous. Set it only behind a proxy that
  sets it and strips it from callers.
- **Writes** are POST, PUT and DELETE, checked by Go's
  `http.CrossOriginProtection`, limited to 1 MB, their fields to sane sizes.
  `--read-only` refuses them.
- **Host names.** Every route but the health checks answers only:
  - an address, `localhost`, a name without dots, or a name under
    `.localhost`, `.internal`, `.svc` or `.cluster.local`;
  - `--external-url`'s name and `--allowed-hosts`.

  A page elsewhere could otherwise point a name of its own at a local serve
  or a port-forward (DNS rebinding). The browser would then take it for the
  page's own site and let it read and write, past the cross-origin check.
  Over loopback (this machine, a port-forward, a sidecar), names without
  dots or under `.internal`, `.svc` or `.cluster.local` aren't answered
  either: someone with a hand in the company's or the cluster's DNS could
  point them here.
- **Roles from headers** hold only if nothing but the proxy can reach the
  pod. The chart refuses `serve.userHeader` without its NetworkPolicy,
  unless `serve.networkPolicy.othersKeptOut` says something else keeps the
  rest out. `/metrics` and the health checks are served alone on a second
  port (`--metrics-listen`), so Prometheus can be let in there and nowhere
  else. In `--editors` and `--admins`, `user:` and `group:` keep a person
  from taking a group's role by its name.
- **Heavy requests.** A report gives 100,000 firings at most, excused or
  not, and so do the settings tried over a week. Both take one turn at a
  time, which the next waits up to 30 seconds for and then gets 503. The
  turn is held until the answer is written, with 30 seconds to write it:
  otherwise a client that doesn't read keeps one answer after another in
  memory.
- **Told to Slack.** What people wrote is quoted, so a newline can't start
  a message of their own.
- **Links.** Task and runbook links can't carry a user name, which makes a
  link look like one site's and go to another.
- **Parsers.** Rule files, rule APIs, Grafana expressions, and incident and
  firing files are fuzzed in CI each week.
- **Pages** send a Content-Security-Policy that allows no scripts and no
  framing; rule names, labels and incident text are escaped as the
  untrusted text they are. Secrets are read from files and never shown.
- **Outbound** is what it was: queries for history and rules, and the digest
  and notices to Slack and a webhook, without following redirects.
- **The pod** runs as non-root with a read-only root filesystem; the ledger's
  volume is the one place it writes.

## Keeping it fast

Measured on a laptop (`go test ./internal/ledger -run TestScale -v`, and
`./internal/serve -run TestPagesScale`), with 400 rules over twelve weeks:

| | 200,000 firings | 2,000,000 firings |
|---|---|---|
| an hourly run, recorded | 15 ms | 0.18 s |
| writing an incident | 8 ms | 52 ms |
| twelve weeks by rule | 2 ms | 2 ms |
| a week's report, without firings | 2 ms | 4.5 ms |
| judging everything again | 2.5 s | 15 s |
| the ledger on disk | 30 MB | 180 MB |

Pages, worked out afresh over 200,000 firings, take 1 to 21 ms. Both tests
run in CI with budgets, so a change that makes one slow fails.

What keeps it so, and what a new feature has to keep to:

1. **Read tallies or indexed ranges, never everything.** A page reads the
   day or week tallies (rules × weeks rows) or one index range, a page at a
   time (`Firings` takes a limit and a cursor); a report takes its whole
   days from the daily tallies. Nothing a person waits on reads the whole
   ledger: what a query needs often enough gets an index of its own (the
   alerts still on have one).
2. **Write where it changes.** A write gives stones again only over the span
   it changed (`judge(lo, hi)`), and a write from a person is bounded (an
   incident lasts 31 days at most). Anything that touches the whole ledger
   goes to the worker, a week per transaction.
3. **Ask Prometheus from one place.** Only the worker asks, one job at a
   time; a run's cost per hour is bounded by `--every`, `--settle` and the
   rules' margins; a run that doesn't finish in time is halved, not
   repeated; what is worked out once (a week's suggestions) isn't worked
   out again when only sending it failed.
4. **Cache by what changed.** Pages and answers are kept until the ledger
   (or what the worker is doing) changes, up to 32 MB and none over 2 MB,
   and carry an ETag, so a page read twice between runs is worked out once
   and sent once. Pages show times, never "4 minutes ago", so they stay the
   same between changes.
5. **One writer, many readers.** Writes take turns on one connection; reads
   use a pool of their own, in WAL mode, so a long run doesn't hold up a
   page.
6. **Bound what grows.** `--keep` drops old firings, runs, impact windows
   and versions of the rules a few thousand rows at a time and gives the
   space back; incidents written down are kept. A document is kept once,
   however many versions hold it.
7. **Hold it to a budget.** A new page goes in `TestPagesScale`, a new kind
   of write in `TestScale`.

What a rule adds to the others is worked out when read, from `catches` and the
week's noisy rules, so a change to what counts as noisy costs nothing. Noisy is
judged over the weeks shown, as on the rest of the page, and against every
team's rules even when one team's are shown: another team's rule catching an
incident as soon is as good. A rule the latest version no longer holds, or that
can't be replayed, covers nothing and isn't called spare. With `--rule`, only
how many incidents each caught can be said.

What people keep beside the facts (teams, notes on rules, quiet windows,
tasks, comments) is in [work.md](work.md).

## Not in this

A sign-in of its own; more than one replica; Postgres; several Prometheus
servers in one ledger; firings from Alertmanager's webhook; a cloud's own
alarms; editing rules. Each is a piece someone can take on. Alertmanager's webhook, beside the replay, would also show
where a replay differs from what really fired.
