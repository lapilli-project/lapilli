# Getting started

From installing Lapilli to a first report on your own alerts, with the
rules you have now and a Prometheus-compatible API that holds their
history. `lapilli verdict` only reads the history and files, and sends the
report nowhere but where you say.

## See it first, on sample data

```sh
lapilli demo
```

replays the seven made-up alert rules of a small shop over a made-up week
of their history, and judges them against its incidents, by `lapilli
verdict`'s own code. The history is made up in the process: there is
nothing to connect, and nothing is read or sent. Its report has what
the rest of this page finds in yours: a noisy rule and the longer `for`
that would quiet it, the rules that caught an incident, one that nothing
caught and the rule that nearly did, the firings that came while people
slept. `--json` gives the whole report as JSON, and `--tz` the zone the
sample's week is laid in. With Go and nothing installed, `go run
lapilli.dev/cmd/lapilli@latest demo` does the same
([docs/design/demo.md](design/demo.md)).

## Install

```sh
go install lapilli.dev/cmd/lapilli@latest
```

with Go 1.26.11 or later, as `go.mod` says; or take a binary from the
[releases](https://github.com/lapilli-project/lapilli/releases) (macOS and
Linux, amd64 and arm64), or the image `ghcr.io/lapilli-project/lapilli`,
whose entrypoint is `lapilli`.
[Checking a release](../SECURITY.md#checking-a-release) says how to check
what you took, offline too.

## Write down an incident or two

Each firing is judged against what went wrong, so Lapilli needs a record of
it. The smallest is a YAML file:

```yaml
incidents:
  - id: INC-100
    title: Login slow
    start: 2026-11-03T08:10:00Z
    end: 2026-11-03T08:50:00Z
    labels: {service: login}
  - id: INC-101
    title: Checkout errors
    start: 2026-11-11T10:02:00Z
    end: 2026-11-11T10:45:00Z
    labels: {service: checkout}
  - id: INC-102
    title: Search timeouts
    start: 2026-11-13T15:30:00Z
    labels: {service: [search, catalog]}
  - id: INC-103
    title: Orders stuck
    start: 2026-11-14T03:05:00Z
    end: 2026-11-14T03:30:00Z
    labels: {service: orders}
```

- `id` and `start` are required: an id each once in the file, and an
  RFC 3339 time with its offset (`Z`, `+09:00`).
- `end`, `title` and `labels` are optional. Without an `end`, an incident
  is taken to last an hour; one can't be before `start`. `labels` say
  where it went wrong, a value or a list of values a label: with `--match
  service`, a firing with a `service` label catches only an incident that
  lists its value or has no `service` label; a firing or an incident
  without the label isn't held to it.

Write down the incidents of the two weeks before the period too: a firing
that caught nothing is judged only when an incident was on at some time in
the two weeks before it, or `--impact` was read over its time; else it is
counted, not judged ([No record](numbers.md#no-record)). INC-100 is there
for that: it isn't listed, being before the week judged below, but every
firing of the week is judged.

A file with no `incidents` key, an empty one among them, is refused, so a
job that failed doesn't read as a week with nothing wrong: write
`incidents: []` for none. Or have incidents another way:

- [Incidents kept elsewhere](incidents.md): scripts that write this file
  from PagerDuty, incident.io, Jira, GitHub issues and other tools.
- [Incidents nobody wrote down](impact.md): `--impact PROMQL`, windows of a
  signal of user impact taken as incidents, beside the file or alone.

### No incidents written down? Start here

`lapilli impact suggest --prometheus http://prometheus:9090` asks your
Prometheus which of the stacks in front of services that
[Incidents nobody wrote down](impact.md#which-of-them-your-history-has)
lists it has the metrics of: Istio, Linkerd, Envoy, ingress-nginx,
Traefik, OpenTelemetry's HTTP server metrics. For each one found it says
how many series, how many windows of user impact its `--impact` finds
over the last week, and the `--impact '...'` to give `lapilli verdict` in
place of `--incidents`. It only reads, and chooses none: check the labels
and the threshold first.

## Judge last week

```sh
lapilli verdict --rules alert-rules/ --prometheus http://prometheus:9090 \
  --incidents incidents.yaml --match service
```

`--rules` replays the rules against the history at `--prometheus`:
Prometheus rule files, `PrometheusRule` resources, Grafana alerting
provisioning files, ConfigMaps holding them, or a directory of them. The
period is the last 7 days (`--since 7d`), or `--from` to `--to` in RFC
3339, its times shown in the machine's zone or in `--tz`. `--match service`
keeps an alert about one service from catching an incident in another
only because they came at the same time; without it, any firing in time
catches.

In place of `--rules alert-rules/`:

- `--loaded-rules`: the rules the API at `--prometheus` has loaded.
- `--grafana https://grafana.example.com --grafana-token-file grafana-token`:
  the rules a Grafana holds, read with a Viewer's service account token,
  `--prometheus` still given.
- `--alerts`: what Prometheus fired, from its `ALERTS` series, with no
  replay; so no longer `for` is suggested and no near misses are looked for.

For a cloud's managed Prometheus, `--auth aws`, `azure` or `google` sends
the credentials the machine or pod already has, and `--bearer-token-file
FILE` a token: [Use `lapilli verdict`](verdict.md) has each cloud's URL and
the permission it needs.

## Read the report

Over a made-up week, given with `--from`, `--to` and `--tz UTC`, with the
incidents above, three rules (`CheckoutErrors`, `NodeLoadHigh`,
`QueueBacklog`) and a stand-in answering for Prometheus, the command above
writes this, three of its parts left out:

```
Lapilli  2026-11-09 00:00 → 2026-11-16 00:00 UTC
14 firings, 3 incidents written down

RULE            SEVERITY  FIRED  ○ CAUGHT  ● NOTHING  PRECISION  REFIRED
NodeLoadHigh    warning   13     1         12         8%         0
CheckoutErrors  critical  1      1         0          100%       0

Noisy rules that a longer "for" would quiet, still catching every incident they caught:
RULE          FOR       FIRED   INCIDENTS  CAUGHT LATER BY
NodeLoadHigh  1m → 10m  13 → 1  1          up to 9m

INCIDENT                 START             LASTED  CAUGHT BY
INC-101 Checkout errors  2026-11-11 10:02  43m     CheckoutErrors +3m
INC-102 Search timeouts  2026-11-13 15:30  –       NodeLoadHigh -9m (noisy)
INC-103 Orders stuck     2026-11-14 03:05  25m     ✗ nothing fired

✗ 1 of 3 incidents had no alert.
! 1 of 3 incidents were caught only by noisy rules, which fire whatever happens
  (10 or more firings, under 20% of them catching anything).

Rules that nearly caught them: their condition held, but not for their "for", or fired too late; or their value came within a fifth of their threshold.
INCIDENT  RULE          HELD         CHANGE        WOULD FIRE  FIRED  ● NOTHING
INC-103   QueueBacklog  7m from -5m  for 10m → 6m  +1m         0 → 1  0 → 0
```

From the top:

- **The period** and how many firings and incidents it holds; a line under
  it says how many firings had no record to judge them by, when some had.
- **Each rule that fired**: its firings, those that caught an incident (○,
  white stones), those that caught nothing (●, black stones), its
  precision (the share of its judged firings that caught something), and
  those that came within `--refire` (1h) of the same alert resolving.
- **Noisy rules**, 10 firings or more with under 20% catching anything,
  replayed with a longer `for`: the shortest that keeps every incident
  each caught, firing at most half as often.
- **The incidents**: how long each lasted (`–` with no end written down),
  what caught it and how long after its start (`+3m`) or before (`-9m`);
  `(noisy)` marks a noisy rule's catch, `✗ nothing fired` a miss.
- **Rules that nearly caught them**: a rule whose condition held but not
  for its `for`, or whose value came within a fifth of its threshold, with
  the change that would have caught the incident and how often it would
  then have fired. With a `for` of 6m, `QueueBacklog` would have fired a
  minute into INC-103, once in the week.

Left out above: the firings that caught nothing by when they came in
`--tz` (while people slept, off hours, at work); what each rule adds (the
incidents each caught, first and alone, and the rules that are spare); and
the rules that never fired or couldn't be replayed, with why.
[How it judges](how-it-judges.md) says what the report rests on;
[The numbers](numbers.md) defines each number and how to count it again
from `--json`, the whole report, every firing included.

## Next

Before a rule is changed, `--compare` replays the rules proposed beside the
rules in place, over the same period and against the same incidents
([Before a change is merged](compare.md)):

```sh
lapilli verdict --rules proposed/ --compare alert-rules.yaml --prometheus http://prometheus:9090 \
  --incidents incidents.yaml
```

The Helm chart runs `lapilli verdict` every Monday at 09:00 over the week
before, and sends the report to Slack and the metrics to a Pushgateway
([Every week](every-week.md)).

`lapilli serve` keeps judging, every hour, into a ledger with a web UI
([Keep judging](serve.md)). `lapilli preflight` takes its flags and says
what it will meet: the rules, the history, and the requests and memory its
runs will take. It writes and replays nothing, and exits 0 with no problem
([Before it is deployed](serve.md#before-it-is-deployed)):

```sh
lapilli preflight --rules alert-rules/ --prometheus http://prometheus:9090 \
  --incidents incidents.yaml --match service
```

`lapilli serve` with the same flags replays the last 28 days on its first
start (`--backfill`), keeps the ledger in `lapilli-ledger`, and listens on
`127.0.0.1:8080`, where everyone who reaches it is an admin. In Kubernetes,
[the chart](../charts/lapilli/README.md) needs no more than this:

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set mode=serve --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' --set 'match={service}'
kubectl -n monitoring port-forward svc/lapilli 8080
```

`rules.configMaps` names the ConfigMap of rules, in the release's
namespace; `incidents` takes the same list as the file, or people write
them down in the UI. With no roles, everyone who reaches the pod may only
look ([Who people are](../charts/lapilli/README.md#who-people-are)).
