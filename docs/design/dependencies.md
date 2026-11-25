# An alert on a dependency catches its dependents' incidents

Status: proposed with its implementation. Under
[GOVERNANCE.md](../../GOVERNANCE.md) this changes how a firing is judged
and adds a kind of data to the ledger, so it is a design; with one
maintainer it is merged with the code that builds it, sooner than a week,
and this note says so. "Questions for review" at the end are open.

## Why

`--match service` keeps an alert about one service from catching an
incident in another just because they came at the same time
([How it judges](../how-it-judges.md)). It also keeps the alerts that matter
most from catching anything: a database's, a queue's, a cache's. The
payments database runs out of connections; checkout's errors climb; the
incident is written down as "Checkout errors" with `service: checkout`.
The alert that fired first, `PaymentsDBConnections` with `service:
payments-db`, caught nothing, and is the rule the report suggests a longer
`for` for.

Today a team gets around it three ways, each by hand: the incident tool
links the alert ([links.md](links.md)), which most tools don't do for every
incident; a rule entry's `require:` and a signal's own lead
([judging.md](judging.md), item 1); or dropping `--match`, which lets every
alert at the time catch every incident.

The history usually knows which service calls which. A service mesh counts
each call by its caller and callee, and so does an OpenTelemetry service
graph. Read as a graph, that is the evidence the label lacks: checkout
called payments-db that hour.

## What is built

### The graph

`--depends PROMQL` (both commands; the chart's `depends`; in serve the
Settings page, below): an expression whose series each name a caller and a
callee, by the labels `--depends-labels FROM,TO` says. An edge from a
caller to a callee holds at a time when the expression returns that pair
then. The nodes are values of one label of the match, `--depends-match
LABEL` (the first `--match` label when not given), so the expression must
give the services by the names firings and incidents carry; `label_replace`
does that where they differ.

| Stack | `--depends` | `--depends-labels` |
|---|---|---|
| Istio | `sum by (source_workload, destination_workload) (rate(istio_requests_total{reporter="source"}[1h])) > 0` | `source_workload,destination_workload` |
| Linkerd | `sum by (deployment, dst_deployment) (rate(response_total{direction="outbound"}[1h])) > 0` | `deployment,dst_deployment` |
| An OpenTelemetry or Tempo service graph | `sum by (client, server) (rate(traces_service_graph_request_total[1h])) > 0` | `client,server` |

`lapilli impact suggest` says which of them the history has, beside the
`--impact` it already suggests, and the flags to give.

The expression is read at an hour's step (`--depends-step`, 10m to 1h) over the period and the `--lead` before it. A graph keeps no record
of its own: it says who calls whom, not whether users were hurt, and so it
only widens which firings catch an incident; which firings are judged is
as before.

### A catch by a dependency

A firing that doesn't catch an incident by its labels, because its value of
the label (`payments-db`) isn't the incident's (`checkout`), catches it by
dependency when:

- it starts as any firing must: between the lead before the incident and
  its end, and not already on before that;
- the incident's value depends on the firing's through edges that held
  between an hour before the incident started and its end: the incident's
  service called the firing's, or called one that did, up to
  `--depends-depth` calls (1 by default, 3 at most);
- it agrees with the incident on every other `--match` label both carry.

That is a cause catching what it caused. `--depends-direction both` also
lets a firing on a caller catch an incident in one it calls (checkout's
error alert catching the payments database's outage, the symptom that told
of it); `down`, the default, doesn't.

The order holds: a firing the incident's tool linked catches it whatever
the graph says; then one whose labels agree; then one by dependency. A
catch by dependency is a catch: it counts toward precision as any other,
the noisy test is as before, and a noisy rule's catches are marked as ever.
Near misses are looked for by dependency too.

### In the report

A catch by dependency is marked: `(via checkout → payments-db)` after the
rule's delay in the text and `--compare`'s Markdown, `dependency` with its
path on `caught_by[]` in `--json`, `/api/v1/report` and MCP. The report
says, after the incidents table, how many catches were by dependency and by
which rules, and how many edges the graph had. `--redact` aliases the
services on a path as it aliases label values. docs/numbers.md says how
each is counted.

### In the ledger

The next free schema number when the pull request opens:

- `edges`: `hour`, `from`, `to`, the hours each edge held, read each run
  for its hours and kept, so judging again asks Prometheus nothing.
- `catches.dependency`: the path as text, `''` for any other catch.

### In lapilli serve

The Settings page's Incident criteria takes `--depends`, its labels, depth
and direction, tried on the last four weeks first as `--impact` is: the
catches it would add, by rule, and how precision would move. Saved, the
graph is read over those weeks and they are judged again. The Incidents
page shows a catch by dependency with its path, and a rule's page its
catches by dependency apart.

## Cost and bounds

- 20,000 edges an hour at most; past that the read stops, keeps none, and
  says so, rather than judge by part of a graph.
- A catch looks at most `--depends-depth` calls from the incident's
  service, over the edges of its hours: a breadth-first walk bounded by the
  edges kept.
- The expression is read once a run (once for `lapilli verdict`), within
  `--max-samples` and `--max-rate`, as every query is.

## Attacker's look

- **Who sets the graph.** The flags are whoever runs Lapilli; the Settings
  page's are an admin's, as `--impact` is. A graph that says everything
  calls everything makes every alert catch every incident: as dropping
  `--match` already could, and the report says how many catches came by
  dependency, by rule, so it shows.
- **What it can't do.** It never makes a firing judged that wasn't, keeps
  no record, and never takes a catch away.
- **Names.** Services on a path are label values Lapilli already shows;
  `--redact` aliases them.

## As built

The core, `lapilli verdict` and `lapilli serve` are built.

- `internal/graph` holds the graph: `Builder.Add(hour, from, to)` and
  `Builder.Graph()` (what `lapilli serve` builds from its `edges` rows),
  `Graph.Each` (the rows to keep), `Graph.Reach(values, Window, depth, both)`
  (every service the values reach, each by the shortest path, in call order:
  one breadth-first walk for an incident's values, window, depth and
  direction, kept and answered by a map lookup, so the cost is the incidents
  and not the firings times the incidents; at most the services of the
  window's edges, and about a million reached services' paths kept before they
  are dropped and walked again), `Graph.Depends(from, to, Window, depth)`
  (a lookup in `Reach` of one service) and
  `graph.Read` (the expression read with `QueryRange`). Edges are kept by the
  hour in UTC, so a window takes the hours from the one an hour before the
  incident's start to the one its end is in: an edge that held 59 minutes
  before the hour's start still counts, as the hour's.
- The 20,000-edge bound is on distinct edges in one hour; past it the builder
  stops, the graph keeps none (`Graph.TooMany`) and `settings.depends.too_many`
  says so. So is an hour that names more than 5,000 services
  (`graph.MaxNodesPerHour`). A name that isn't a service's (`graph.ValidName`:
  empty, longer than 128 bytes (`MaxName`, a Kubernetes `namespace/workload`
  at most), not UTF-8, with a character that isn't printable (control, format
  or bidirectional-control such as U+202E, a space but the plain one), or with
  `→` or `,`) is refused and counted (`Builder.Refused`, `Graph.Refused`,
  `settings.depends.refused`), and a call from a service to itself is no edge.
  A name's limits are what keep a path's text (`via a → b`) from being forged.
- The expressions count calls over an hour (`rate(...[1h]) > 0`): a sample at T
  is stored under the hour T less a nanosecond is in, the hour it covers, and
  the read goes to `--to` plus a step to take the last hour whole. `--depends-step`
  is refused over 1h (`graph.MaxStep`), as a longer one leaves hours with no
  sample; with a short range such as `[5m]`, an hour's sample would see five
  minutes of it. A value of 0 or less, as NaN, is no call.
- The graph is read from the hour of `--from` less the longest lead (at least
  an hour) to `--to`, so an incident at the period's start has its hour before.
- `verdict.CatchesBy` gives how a firing caught (`Caught`: linked, lacked,
  `Dependency`); `CatchesLinked` is as it was, and says a catch by dependency
  as a plain catch. `Options.Depends` (`verdict.Dependencies`: graph, label,
  depth, direction) turns it on, and `Options.About` follows it.
- A catch's `dependency` is set when the rule's catches of the incident were all
  by dependency, and is the path of its earliest firing; a rule with a firing
  linked or agreeing on the labels is marked as it was, and its delay and
  whether it is loose are those of the direct firings alone: the graph only
  makes a catch where there was none. A rule's `white_by_dependency` counts
  its firings white by the graph alone (`Judged.ByDependency`), and the note
  says their total. A catch made only by dependency doesn't count where a
  change is held to the incidents or a rule is called spare, as anyone who can
  write the graph's series could make it: `HoldMissed` leaves such an
  incident out of `--fail-missed` and says how many (`Comparison.
  MissedByDependency`), `Credit` lets such a catch neither cover another
  rule's nor take its `First` and `Alone`; `--depends-count-missed`
  (`Dependencies.CountCatches`, `settings.depends.count_catches`) counts them
  as any catch (`CreditCounting`). A firing by dependency
  that lacks another label of the match the incident has is loose, as any
  firing, and `--match-strict` drops it.
- `--depends-match` must be one of the `--match` labels, so that what the
  graph names are the values firings and incidents carry.
- A near miss found by the graph has the path too (`NearMiss.Dependency`,
  `Options.AboutBy`): in `--json`, after the rule's name in the table, aliased
  by `--redact`. `--compare`'s text and Markdown say when the graph kept none
  (`too_many`). `Graph.Each` gives each hour's edges by name.
- A path is shown by `DependencyMark` (text; a name escaped of `→` and `,`
  if an older record has one) and, for a page, by `verdict.DependsHops`: each
  service HTML-escaped in its own `<bdi>`, joined by an arrow outside them.
- `settings.depends` has the label, depth, direction, edges, `too_many`,
  `refused` and `count_catches`; not
  the expression, which may hold what a report shouldn't show.
- `--compare` shows the path in its text and Markdown, and counts a catch that
  came by dependency, or no longer does, as a change in how an incident is
  caught; it has no note of its own for the graph.
- Istio's and Linkerd's expressions name a service `namespace/workload`
  (`label_join`, the labels `caller,callee`), so that same-named workloads of
  two namespaces don't merge and a workload in another namespace can't be
  named as the one an incident's service calls; the firings' and incidents'
  match label must carry that form, which `docs/dependencies.md` shows how to
  give, and the plain forms are listed there as an option. An OpenTelemetry
  service graph names what its senders set, and the docs say so.
- `lapilli impact suggest` counts one series of each stack's metric (Istio's
  `reporter="source"`, Linkerd's `direction="outbound"`, the service graph's
  `traces_service_graph_request_total`), and `docs/dependencies.md`'s table is
  held equal to the code's list by a test.

### In lapilli serve, as built

- Schema 036: `edges` (`hour`, `from_svc`, `to_svc`: `from` and `to` are not
  names both dialects take as columns), `edge_spans` (the spans the graph was
  read over, apart and whole), `edge_retries` (the spans a run couldn't read,
  read once more by the next run, as `rollout_retries` are) and
  `catches.dependency` (the path as a JSON list of the services in call order,
  `''` for any other catch), and `white_depends` on `firings`, `days` and `weeks`
  (the firings white only by the graph). Edges, spans and retries go with the
  firings' retention.
- Each run reads the graph over its hours and the lead before them
  (`From − max(lead, 1h)`) and keeps it with the run (`Found.Edges`), under the
  key of the round's graph (`EdgesRead.Key`, read from `Dependencies.Key` and
  the conf `dependsFor` kept): `keepEdges` keeps it only while `edges-key` is
  that key, so a save mid-run never keeps another expression's edges. Edges are
  inserted many to a statement (1,000), and a span is kept to the whole hour:
  one ending mid-hour ends where that hour starts, the next run reading it.
  Judging loads the ledger's edges for each incident's window apart (an hour
  before its start to its end; windows that overlap or touch are one), at most
  `ledger.MaxJudgeEdges`, 1,000,000, in all in one judgement; past it that
  judgement is made with no graph, and `meta` `edges-over` keeps the span judged
  (`Ledger.EdgesOver`), which a judgement covering it with the graph loaded
  takes away: serve's report sets `settings.depends.too_many` from it. What
  loads is the ledger's, so judging again asks the history nothing, and a read
  that failed leaves its hours judged with what the ledger holds until the run
  after reads them again.
- A catch's delay and looseness in the ledger are, as `verdict.Judge`'s, those
  of the rule's direct firings when it has any. A firing white only by the graph
  is kept (`firings.white_depends`, and `days.white_depends` and
  `weeks.white_depends` summed from it), so that `white_by_dependency` on a
  rule, the digest's note, the rule page, the overview's precision tile and MCP
  count them; `TestEdgesKeptAndJudgedAgain` holds the ledger's rules and
  incidents equal to `verdict.Judge`'s.
- Bounds in the ledger: 200,000 edges a day and 5,000,000 in all
  (`MaxEdgesPerDay`, `MaxEdgesKept`, checked by `EdgesFit` before a read is
  kept, and before one is made for the total); `edges` are pruned 5,000 at a
  time and a span begins where the edges kept do. `EdgesIn` is counted once for
  a generation of the ledger and period.
- How reading went is kept in `meta` `edges-status` (`EdgesStatus`: the key,
  the last error and when, the tries and when a read over the last four weeks is
  tried again), seen on any replica and cleared by a read that goes well.
  `edge_retries.gap` is 1 for a span that failed to be read again: a known gap,
  not read again, which a read over it takes away. The key is the step, the
  labels and a digest of the expression as `rules.Rule.Parsed` writes it.
- What the edges were read with (the step, the labels and the expression) is
  kept apart (`meta` `edges-key`). A change of it drops the edges and reads the
  last four weeks (the leader's round, before the options are read), and the
  graph is used only once the key kept is the key in effect, so no catch is
  judged by edges read with another expression. What the stones are judged by
  adds a digest of that key, the label, depth and direction (`~` and eight
  capital letters, after `--linked-only`'s and the responses' keys): a change
  of any judges every firing again, once.
- The Settings page's settings are `depends`, `depends-labels`,
  `depends-depth` and `depends-direction`, kept as the impact ones are, and
  checked as an impact expression is (`checkImpact`, whose messages say "impact
  expression"). The label whose values are the services is `--depends-match`,
  else the first label of the match in effect; with none of the match's labels
  it, the graph is not used and the page says so. The step is the command line's.
- A try reads the graph typed, and judges the last four weeks' firings with
  and without it, over the incidents in force (those that keep the record),
  and says the pairs of services it found, the catches it would add by rule
  with the path of each rule's first, and four measures.

## Questions for review

1. `down` as the default direction, `both` an option.
2. Depth 1 by default: a database two calls away from checkout is often
   the cause too, and also more often chance.
3. An edge counted from an hour before the incident: long enough for a
   call pattern that comes and goes, short enough not to bring in last
   week's.

## Sources

- Istio's standard metrics:
  https://istio.io/latest/docs/reference/config/metrics/
- Linkerd's proxy metrics:
  https://linkerd.io/2-edge/reference/proxy-metrics/
- The OpenTelemetry Collector's service graph connector:
  https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/connector/servicegraphconnector
- Tempo's service graphs:
  https://grafana.com/docs/tempo/latest/metrics-generator/service_graphs/
