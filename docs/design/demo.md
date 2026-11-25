# `lapilli demo`: a first report, with nothing to connect

Status: proposed, built in the same pull request; `--serve` (0.70), the
same way, in its own. Under [GOVERNANCE.md](../../GOVERNANCE.md) a new
command is designed first, as a pull request open for a week; this one
carries the code too, so the design can be read beside what it does. It
adds nothing to the ledger, and changes how no firing is judged.

## Why

Before anything shows, a newcomer needs a Prometheus with a week of
history, the rule files it was evaluated with, and incidents written
down. Each takes a while to find, and getting one wrong shows up only
after a run. Seeing first what a report says, on rules anyone can read,
says whether the rest is worth it.

## What it runs

`lapilli demo` is `lapilli verdict`, its own code from the flags on: the
rules read from a file, replayed, judged, the suggestions and near misses
worked out, the report written as `lapilli verdict` writes it. Only the
history is another: the sample's, answered in the process.

- **The sample**, embedded, a few KB: `cmd/lapilli/demo/rules.yaml`, seven
  rules of a made-up shop as a Prometheus rule file has them, and
  `demo/sample.yaml`, the incidents of a week and, for each expression a
  rule compares with a number, its series: a value all week and the
  spans it is another. A time there is a weekday and a time of day.
- **The history** (`demoHistory`) answers an expression the sample has a
  series of, alone or compared with a number at its end, as Prometheus
  would: the steps where it holds. Anything else has no series. So a
  rule replayed with a longer `for`, or a nearer number, is answered as
  the rule is.
- **The period** is a fixed week, Monday 9 November 2026 on, laid in the
  zone shown (`--tz`, or the machine's): a firing at 01:10 there came
  while people slept wherever it is run. The report is the same every
  run, and in every zone but for the zone it names.
- **The files**: the rules and the incidents (the week's and the week
  before's, which keep the record of its first days) are written to a
  directory of `os.MkdirTemp`'s and read from there as anyone's are, and
  the directory removed after.

It takes `--json` and `--tz`, as verdict does, and `--serve` with
`--listen` (below), and nothing else. After the
report it says how to run the same on one's own rules, and `lapilli
impact suggest` for those with no incidents written down: on stdout, or
on stderr with `--json`, so that stdout is the JSON alone.

The seam is `verdictOn`: `lapilli verdict` with a history given in place
of `--prometheus`'s, which is then never asked. `newHistory`, the only
way to one, isn't called; a test fails if it is.

`TestDemo` holds the report's lines that say what Lapilli is for: the
noisy rule and its longer `for`, the catches, the incident missed and the
rule that nearly caught it, the firings that woke people. A change to how
firings are judged that changes them fails it, and the sample is changed
with it, on purpose.

## What it never does

- Read a Prometheus, or anything over the network: there is no client in
  its path.
- Send anything: no flag of verdict's that sends (`--push`, `--slack-webhook-file`)
  is taken.
- Write anywhere but its temporary directory, which it removes, except
  for `--write DIR`: it writes `rules.yaml`, `incidents.yaml` (the
  incidents it judges by, the week's and the week before's) and a
  `README.txt` into DIR, made if missing, replays nothing and listens on
  nothing, and refuses to overwrite any of the three. It does not go with
  `--serve` or `--json`.
- Read a file of the user's: its flags name none.

With `--serve` it listens, on loopback alone, read-only; the rest holds
as it does without.

## `--serve`: the same sample in serve's pages

`lapilli demo --serve` is `lapilli serve`, its own code from the flags
on, over the same sample: the rules read from a file and replayed by its
hourly job, the firings judged and kept in a ledger, the week's digest
worked out, the pages, the API and MCP served from the ledger. As with
the report, only the history is another; and the clock.

- **The flags** (`demoServeArgs`): the rules, and the incidents of the
  sample's week, written to a directory of `os.MkdirTemp`'s, and the
  ledger kept in the same directory; `--read-only`; `--check-replay=false`,
  as the sample has no `ALERTS` to set the replay beside; `--backfill` from
  the Monday before the week's to the clock, `--settle 0s` and `--digest mon@00:00`;
  and the demo's `--tz`. No flag names anything else to read, or anywhere
  to send to.
- **Teams and a proposal**: to show what serve does with teams and with a
  change waiting for approval, which read-only would otherwise leave empty,
  the demo keeps some of its own. Before serve opens the ledger it opens it
  itself and keeps three teams by the ledger's API, as "lapilli demo":
  `checkout` (the rules named `Checkout…` and `Payments…`, and the incidents
  of those services), `search` and `platform` (`Orders…`, `Node…`,
  `Certificate…`), each by a pattern on the rule's name and by the `service`
  the sample's alerts and incidents carry (`--match`), so the pages' team
  picker shows each team's own numbers. The proposal is the longer `for`
  the report itself suggests, for `CheckoutLatencyP99` (1m to 5m), made by
  serve's own code as a person's from the rule's page is, backtested, and
  kept as "lapilli demo". The backtest needs a run to replay against, so
  it is made by `serve.Config.Seed`, which serve's leader calls after
  each round's digest until the proposal is kept, in the first round that
  has a suggestion. Read-only still refuses every request's write:
  approving it, or dropping it, is 403, and the proposal stays waiting.
  Proposals expire by `--proposals-expire`, which the demo doesn't give,
  so it never shows as expired.
- **The incidents** are the week's and the week before's, as the report
  judges by (INC-38 to INC-43). serve judges a day's firings by the
  incidents on in it or the two weeks before it (`Options.RecordUnit`), so
  the week before's keep the record of the week's first days, as in the
  report. The first run replays the week before too, so that it doesn't
  show them missed: the sample's history repeats by weekday, so INC-38 and
  INC-39 are caught as INC-41 and INC-42 are, and INC-40 is missed like
  INC-43.
- **The clock** (`serve.Config.Clock`) is pinned to Monday 16 November
  2026, 00:15, in the zone shown: the end of the week, and the 15 minutes
  (`--lead`) after it in which a firing of its last minutes could still
  catch an incident. The first run replays the week from its Monday up to
  it. The digest, which waits for the runs to be that far past the week,
  falls due at its end and works it out, its suggestions and near misses
  with it. Later runs have nothing to replay. Every page that shows weeks
  shows the sample's as the last whole one, the overview beside the week
  before, which is empty: the ledger began with it. `/api/v1/report`'s
  last seven days, from 00:15 to 00:15, hold the same firings. The wall
  clock, which proposals expire by and the outbox keeps, stays the
  machine's: the proposal the demo makes is stamped by it, and nothing
  else is made.
- **Where it listens**: `--listen`, a loopback address written as one and
  a port, `127.0.0.1:0` by default, a free port. A name (`localhost`
  among them, which the resolver could make another address), an address
  left out (`:8080`, every interface), any other address, and a port that
  isn't digits (a service's name would be looked up) are refused before
  anything is written. `serve.Config.Listening` is told the address bound,
  and the demo says the URL to open.
- **The end**: serve takes SIGINT and SIGTERM as it always does, and
  closes the ledger; the demo removes the directory after. Killed outright
  (SIGKILL), it can't: the directory, `lapilli-demo-*` under the system's
  temporary directory, stays, holding the sample and nothing else.

The seam is `serveOn`: `lapilli serve` with a `serveWith`, which gives the
history in place of `--prometheus`'s, the clock, and who is told where it
listens. `newHistory` isn't called; a test fails if it is. serve's log
says warnings and errors alone, so that the URL is what is read.

`TestDemoServe` holds what it serves to what `lapilli demo --json`
reports: the firings, stones, refires, hours and episodes of each rule,
and what caught each incident, in `/api/v1/report` over the week and by
default; and the overview, the incidents and a rule's page to the week,
its suggestion and its near miss.

### Reviewed as an attacker would

A port, however local, is reached by every process and every user of the
machine, and by every page a browser there opens.

- **Writes.** Read-only: every request is a viewer's, and every write of
  the UI's, the API's and MCP's is refused, 403 (`need`, `teamWrite`).
  Without it, serve on loopback with no roles takes everyone for an admin.
  Its writes would stay in the temporary ledger, of made-up data, removed
  at the end; but some start work (a setting changed judges every firing
  again, a proposal is backtested), anyone on the machine could make them,
  and what people write is stamped by the wall clock, a week and more
  after the week the pages show. A demo is to be looked at: read-only
  leaves nothing to start but reading. The teams and the proposal are
  seeded by the demo's own code (above), once, and nothing a request
  reaches: no route makes a team or a proposal here.
- **Another site's page, by a name (DNS rebinding).** Over loopback,
  serve answers an address, `localhost` and a name under `.localhost`
  alone (`hostOK`): a name of the attacker's pointed at 127.0.0.1 is
  refused, 421, before anything is read, and so is a name without dots.
  The demo gives no `--allowed-hosts` and no `--external-url`.
- **Another site's page, by a request.** The cross-origin check
  (`http.CrossOriginProtection`) refuses what isn't a GET or a HEAD from
  another origin, read-only aside, and `/mcp` an `Origin` not its own.
  Answers carry no CORS header, so a page can't read them, and
  `frame-ancestors 'none'` keeps them out of its frames. What such a page
  can still make it do is answer GETs, each bounded as serve bounds any (a
  heavy report one at a time), over a week of made-up data.
- **Read.** The files it reads are its directory's: the rules (again
  every minute), the incidents, the ledger; and the zone's data, as any
  program does. No flag names another file, and no request names one.
- **Sent.** Nothing. The history is the sample's, answered in the process;
  no flag gives a Grafana, a ruler, alarms, Loki, an on-call feed, a
  webhook, Slack, a sign-in's provider or another port, and
  `TestDemoServeSendsNothing` holds serve's configuration to that, a
  field at a time: one added with a default fails it until it is looked
  at. `TestDemoServe` fails every request made through Go's default
  transport, and none is.
- **Held.** Nothing of the user's: the sample, and the ledger made of it.
