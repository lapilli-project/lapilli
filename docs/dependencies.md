# An alert on a dependency

`--match service` keeps an alert about one service from catching an incident
in another just because they came at the same time. It also keeps the alerts
that matter most from catching anything: a database's, a queue's, a cache's.
The payments database runs out of connections, checkout's errors climb, and
the incident is written down as "Checkout errors" with `service: checkout`.
The alert that fired first, `PaymentsDBConnections` with `service:
payments-db`, catches nothing, and is the rule the report suggests a longer
`for` for.

`--depends` gives Lapilli the evidence the label lacks: who calls whom. A
service mesh counts each call by its caller and callee, and so does an
OpenTelemetry service graph. Read as a graph, it says checkout called
payments-db that hour; an alert on payments-db then catches checkout's
incident. How a firing catches an incident is in
[How it judges](how-it-judges.md#a-catch-by-dependency).

```sh
lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 \
  --incidents incidents.yaml --match service \
  --depends 'sum by (caller, callee) (label_join(label_join(rate(istio_requests_total{reporter="source"}[1h]), "caller", "/", "source_workload_namespace", "source_workload"), "callee", "/", "destination_workload_namespace", "destination_workload")) > 0' \
  --depends-labels caller,callee
```

## The graph

`--depends PROMQL` is an expression whose series each name a caller and a
callee, by the labels `--depends-labels FROM,TO` says. An edge from a caller
to a callee holds at a time when the expression returns that pair then. The
nodes are values of one label of the match, `--depends-match LABEL` (the
first `--match` label when not given), so the expression must give the
services by the names firings and incidents carry: `label_replace` or
`label_join` does that where they differ.

| Stack | `--depends` | `--depends-labels` |
|---|---|---|
| Istio | `sum by (caller, callee) (label_join(label_join(rate(istio_requests_total{reporter="source"}[1h]), "caller", "/", "source_workload_namespace", "source_workload"), "callee", "/", "destination_workload_namespace", "destination_workload")) > 0` | `caller,callee` |
| Linkerd | `sum by (caller, callee) (label_join(label_join(rate(response_total{direction="outbound", deployment!="", dst_deployment!=""}[1h]), "caller", "/", "namespace", "deployment"), "callee", "/", "dst_namespace", "dst_deployment")) > 0` | `caller,callee` |
| An OpenTelemetry or Tempo service graph | `sum by (client, server) (rate(traces_service_graph_request_total[1h])) > 0` | `client,server` |

Linkerd's selector drops the calls of a workload that isn't a Deployment (no `deployment` label), which `label_join` would otherwise name `shop/`. Istio's and Linkerd's expressions name a service `namespace/workload`
(`shop/checkout`), joined from the series' namespace and workload labels, on
both ends. Their own labels are the workload's alone, so `checkout` in `shop`
and in `staging` would be one node, their calls would join, and a workload in
another namespace could be taken for the one an incident's service calls. The
**`--match` label of your firings and incidents must then carry the same
form**: `shop/checkout`, not `checkout`. In an alert rule:

```yaml
expr: label_join(sum by (namespace, deployment) (rate(http_requests_total{code=~"5..", deployment!=""}[5m])) > 0.05, "service", "/", "namespace", "deployment")
```

or once, where the series are scraped:

```yaml
metric_relabel_configs:
  - source_labels: [namespace, deployment]
    separator: /
    regex: (.+/.+)
    target_label: service
```

The `regex` matters: without it Prometheus's default, `(.*)`, matches the join
of a series that has no `deployment` (`shop/`, or `/` with no namespace
either), and writes that as `service`, replacing the `service` such a series
already had. With it, `service` is written only where both the namespace and
the workload are there, and every other series keeps its own. (The
relabelling's `replacement` is `$1` by default, the whole matched join, which
is what the group holds.) The same holds in the alert rule above: a series
without `deployment` has no `service` worth the name, so filter it out
(`deployment!=""` in the selector) rather than let it join as `shop/`.

Give the incidents the same `service: shop/checkout` in their labels. A firing whose
label is the plain `checkout` agrees with no node, and catches nothing by
dependency. If that is too much to change, the **plain form** is an option,
at the cost above (it is safe where each name is in one namespace only):

| Stack | `--depends` | `--depends-labels` |
|---|---|---|
| Istio, plain | `sum by (source_workload, destination_workload) (rate(istio_requests_total{reporter="source"}[1h])) > 0` | `source_workload,destination_workload` |
| Linkerd, plain | `sum by (deployment, dst_deployment) (rate(response_total{direction="outbound"}[1h])) > 0` | `deployment,dst_deployment` |

An OpenTelemetry or Tempo service graph names its services as the senders set
them (`service.name`, and `peer.service` for a callee the sender names):
whoever can send OTLP (or write the metric) shapes the graph, with any service
they like on either end. That is why a catch made only by dependency doesn't
count as a catch for `--fail-missed` or a rule being spare unless
`--depends-count-missed` says so, and why the graph keeps names of a bounded
kind (see [What bounds it](#what-bounds-it)).

`lapilli impact suggest` says which of them your history has, beside the
`--impact` it already suggests, and the flags to give
([Incidents nobody wrote down](impact.md)). Label names depend on how each is
scraped: check them against your own Prometheus first.

Things that differ by stack, to check in your own Prometheus:

- **Istio ambient mode.** Requests are counted by waypoint proxies, with
  `reporter="waypoint"` rather than `"source"` (a mesh with no waypoint has
  only ztunnel's TCP metrics, `istio_tcp_*`, which this expression doesn't
  read). Give `reporter=~"source|waypoint"` for a mesh of both; the check
  `lapilli impact suggest` makes looks for `reporter="source"` alone.
- **Calls out of the mesh.** Istio names a destination it can't tell
  `destination_workload="unknown"`: every service calling outside the mesh
  then calls one `unknown`, and an alert whose `service` is `unknown` would
  catch them all. Add `destination_workload!="unknown"` to the selector.
- **Names that repeat across namespaces.** The expressions in the table name
  a node `namespace/workload`, so they don't merge; the plain ones do.
- **Linkerd's StatefulSets.** A StatefulSet's calls carry `statefulset` and
  `dst_statefulset`, not `deployment` and `dst_deployment`, so the table's
  expression leaves them out; a run reads one pair of labels, so unify the
  names first (`label_replace`) if services of both kinds call each other.
  Linkerd's labels are `namespace`, `deployment` and, for the callee,
  `dst_namespace`, `dst_deployment`: check them in your own Prometheus.

Count the calls over an hour (`[1h]`), as the table does. A sample at 10:00
is stored as the hour it covers, 09:00 to 10:00; with `[5m]` a sample would
say only whether the last five minutes held a call, and the hour the incident
fell in would be looked at for five of its sixty minutes. The step is at most
1h, as a longer one leaves hours with no sample; a range shorter than the
step does the same. An expression with a range that is the step or longer
sees every minute.

The expression is read once a run, at an hour's step (`--depends-step`, from
10m to 1h) over the period and the `--lead` before it, within
`--max-samples` and `--max-rate` as every query is. The graph keeps no
record of its own: it says who calls whom, not whether users were hurt, so
it only widens which firings catch an incident. Which firings are judged is
as before, and it never takes a catch away.

| Flag | |
|---|---|
| `--depends PROMQL` | the expression; needs `--depends-labels` and a `--match` label |
| `--depends-labels FROM,TO` | the labels of its series that name the caller and the callee |
| `--depends-match LABEL` | the label of `--match` whose values the services are |
| `--depends-depth N` | how many calls away still count: 1 (the default) to 3 |
| `--depends-direction down\|both` | `down` (the default), or `both` |
| `--depends-step DUR` | the step it is read at: 1h (the default), 10m to 1h |
| `--depends-count-missed` | count an incident caught only by dependency as caught, for `--fail-missed` and spare rules (not by default) |

## A catch by dependency

A firing that doesn't catch an incident by its labels, because its value of
the label (`payments-db`) isn't the incident's (`checkout`), catches it by
dependency when:

- it starts as any firing must: between the lead before the incident and its
  end, and not already on before that;
- the incident's service called the firing's, or called one that did, up to
  `--depends-depth` calls (1 by default, 3 at most), over the edges that held
  from an hour before the incident started to its end;
- it agrees with the incident on every other `--match` label both carry.

That is a cause catching what it caused. `--depends-direction both` also lets
a firing on a caller catch an incident in one it calls (checkout's error
alert catching the payments database's outage, the symptom that told of it).

The order holds: a firing the incident's tool linked catches it whatever the
graph says; then one whose labels agree; then one by dependency. A catch by
dependency is a catch: it counts toward precision as any other, and the
noisy test is as before. Near misses are looked for by dependency too, and carry the path
(`dependency` on a near miss in `--json`, `(via checkout → payments-db)` after
the rule's name in the table, aliased by `--redact`).

Edges are kept by the hour, in UTC: an edge held at any time in an hour holds
for that hour, and an incident takes the hours from the one an hour before its
start to the one its end is in.

## In the report

A catch by dependency is marked `(via checkout → payments-db)` after the
rule's delay, the path from the caller to the callee. After the incidents
table the report says how many catches were by dependency and by which rules,
and how many edges the graph had:

```
! 1 catch was by dependency, a rule on a service the incident's depends on (the service graph had 2 edges): PaymentsDBConnections.
```

A rule is marked only when no firing of it was linked or agreed on the labels:
with one that did, its delay and whether it was loose are that firing's, and
the graph's firings of the rule add nothing to them. The note ends with how
many firings the graph made white that nothing else caught ("It made 2
firings white that no labels or link caught"), `white_by_dependency` on a
rule in `--json`, so the precision it widened can be told from the rest.
`--json` has `dependency` on `caught_by[]`, with the path, and `settings.depends`
with the label, depth, direction and edges; `--compare` marks the same in its
text and its Markdown (each service in a code span, as a rule's name is); `--redact` aliases the services on a path as it aliases
the label's values. [The numbers](numbers.md) says how each is counted.

## In lapilli serve

`lapilli serve` takes the same flags (the chart's `depends`, `dependsLabels`,
`dependsMatch`, `dependsDepth`, `dependsDirection` and `dependsStep`), and the
Settings page's Incident criteria has a **Service graph** with the expression,
its labels, depth and direction, as `--impact` has: an admin's to change, tried
on the last four weeks first (the catches by dependency it would add, by rule,
with the way each reached its incident, and how the verdict, its precision, its
noisy rules and its missed incidents, would move), and read by the same
bounds. A try is counted with the impact tries, twelve an hour for an admin,
and asked for from the Settings page on the site.

- Each run reads the graph for the hours it judges and the lead before them,
  once, and keeps its edges by the hour in the ledger (`edges`), with the spans
  it was read over: whole hours, so a run that ends in the middle of one leaves
  it to the next run, which reads from an hour before its own start. A run reads
  and keeps the graph its round judges by: a save made meanwhile is read by the
  next round, never kept as the edges of the one before. A read that fails is
  kept as a span the next run reads once more, until then the incidents there
  are judged by what the ledger holds; one that fails again is a known gap, not
  read again. The Settings page lists the stretches not read, the spans to be
  read once more and the gaps, and why the last read failed, kept in the
  ledger, so any replica's page says it, until a read goes well.
- Judging, the first time and again, asks the history nothing. It loads the
  ledger's edges for each incident's window apart, the hours from one before
  its start to the one its end is in, not the hours between them: at most
  1,000,000 edges in all in one judgement, which a graph of 5,950 pairs an hour,
  the most seen, fills with 168 hours of windows. Past that, that judgement is
  made with no graph, as one with too many edges in an hour is, and says so:
  `settings.depends.too_many` of a report over those hours, a note on the rule's
  page and the Settings page, and a warning in the log. A judgement that covers
  them again, with the edges loaded, takes it away.
- A catch writes its path (`catches.dependency`), which the Incidents page
  shows after the rule (`by dependency, via checkout → payments-db`), a rule's
  page lists apart ("Caught by dependency"), and MCP and `/api/v1/incidents`
  carry as `dependency`. `/api/v1/report` has it on `caught_by[]` and
  `settings.depends` with the edges of the period.
- Turning the graph on, off, or changing its expression, labels, step,
  label, depth or direction judges every firing again, once, as a change of
  `--linked-only` does. A new expression, labels or step is first read over the
  last four weeks, by the replica that leads, which keeps those weeks' edges
  in place of the ones before; one that can't be read is tried again at the
  next round, and meanwhile the ledger is judged with no graph. Changing the
  depth or direction reads nothing.
- A new expression, labels or step is a new key: what the ledger holds is
  discarded, not kept beside it, and the graph is read back over the last four
  weeks, or to where the runs began if that is later, a week at a time, each
  within 90 seconds, whatever `--timeout` says, so that a history that fails
  holds the leader's jobs for little. The weeks that read are kept, and those
  that didn't are read once more by the next run, then are known gaps. A read
  that reaches none leaves the ledger as it was and is tried again an hour
  later, then two, up to a day ("Tried 3 times; tried again at ..." on the
  Settings page). The expression is compared as Lapilli reads PromQL, so
  spaces, comments and the order of matchers don't make a new key. An
  expression with no `--depends-labels` is refused.
- Edges are kept as long as the firings are, pruned five thousand rows at a
  time with the spans they were read over. Turned off, they are dropped.

## What bounds it

- An hour of the graph keeps 20,000 edges at most. Past that the read stops,
  the graph keeps none, and no catch is by dependency, rather than judge by
  part of a graph: `lapilli verdict`'s report says so ("it kept none, and no
  catch was by dependency"), and `lapilli serve` says it on the Settings page
  and in its log, with the hours a gap in the spans read.
- `lapilli serve` keeps 200,000 edges a day and 5,000,000 in all (counting each
  for every hour it held; four weeks of the most seen, 5,950 pairs an hour, is
  4 million). A read with a day past the first is not kept, and while the
  ledger holds the second the hours are not read: the Settings page says so
  until what is past `--keep` goes, or the expression is narrowed to the calls
  between services (`sum by` the two labels). Edges are written many to a
  statement.
- A catch looks at most `--depends-depth` calls from the incident's service,
  over the edges of its hours. The walk is made once for an incident (its
  services, its window, the depth and the direction), to every service they
  reach with the shortest way to it, and each firing is a lookup in that: the
  work is the incidents, not the firings times the incidents. What an incident
  reaches is at most the services of its window's edges (5,000 an hour at
  most), and a run keeps at most about a million reached services' ways
  before it drops them and works them out again as asked.
- A graph that says everything calls everything makes every alert catch every
  incident, as dropping `--match` already could. The report's note counts the
  catches by dependency and names their rules, so it shows.
- A name that isn't a service's is refused, and counted (`settings.depends.refused`
  in `--json`): longer than 128 bytes (a Kubernetes `namespace/workload` is at
  most 127), not valid UTF-8, with a character that isn't printable (a control
  character, a format or bidirectional-control character such as U+202E, a
  space but the plain one), or holding `→` or `,`, which the report writes a
  path and its lists by: so a name can't forge a path's mark. A series with
  the same service on both sides, or without both labels, is no edge.
- An hour of the graph names 5,000 services at most; past that the read stops
  and the graph keeps none, as past 20,000 edges.
- A catch made only by dependency doesn't count where a change is held to the
  incidents. A pull request that removes the only alert that caught an
  incident by its labels would otherwise pass `--fail-missed` for another
  rule's catch by dependency, which whoever can write the graph's series can
  make; and a rule is not called spare for such a catch either, nor does it
  take a rule's `FIRST` or `ALONE`. Such an incident is missed, and
  `--fail-missed` fails; the comparison says how many of the missed were
  caught after the change only that way ("caught after it only by
  dependency", `missed_by_dependency` in `--json`, a subset of `missed`), and
  `--depends-count-missed` counts them as caught, as any catch.
