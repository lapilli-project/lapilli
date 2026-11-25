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
  --depends 'sum by (source_workload, destination_workload) (rate(istio_requests_total{reporter="source"}[5m])) > 0' \
  --depends-labels source_workload,destination_workload
```

## The graph

`--depends PROMQL` is an expression whose series each name a caller and a
callee, by the labels `--depends-labels FROM,TO` says. An edge from a caller
to a callee holds at a time when the expression returns that pair then. The
nodes are values of one label of the match, `--depends-match LABEL` (the
first `--match` label when not given), so the expression must give the
services by the names firings and incidents carry: `label_replace` does that
where they differ.

| Stack | `--depends` | `--depends-labels` |
|---|---|---|
| Istio | `sum by (source_workload, destination_workload) (rate(istio_requests_total{reporter="source"}[5m])) > 0` | `source_workload,destination_workload` |
| Linkerd | `sum by (deployment, dst_deployment) (rate(response_total{direction="outbound"}[5m])) > 0` | `deployment,dst_deployment` |
| An OpenTelemetry or Tempo service graph | `sum by (client, server) (rate(traces_service_graph_request_total[5m])) > 0` | `client,server` |

`lapilli impact suggest` says which of them your history has, beside the
`--impact` it already suggests, and the flags to give
([Incidents nobody wrote down](impact.md)). Label names depend on how each is
scraped: check them against your own Prometheus first.

The expression is read once a run, at an hour's step (`--depends-step`, 10m
at the finest) over the period and the `--lead` before it, within
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
| `--depends-step DUR` | the step it is read at: 1h (the default), 10m at the finest |

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
noisy test is as before. Near misses are looked for by dependency too.

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
text and its Markdown; `--redact` aliases the services on a path as it aliases
the label's values. [The numbers](numbers.md) says how each is counted.

## What bounds it

- An hour of the graph keeps 20,000 edges at most. Past that the read stops,
  the graph keeps none, and the report says so ("it kept none, and no catch
  was by dependency"), rather than judge by part of a graph.
- A catch looks at most `--depends-depth` calls from the incident's service,
  over the edges of its hours: a breadth-first walk bounded by the edges
  kept.
- A graph that says everything calls everything makes every alert catch every
  incident, as dropping `--match` already could. The report's note counts the
  catches by dependency and names their rules, so it shows.
- A service name longer than 256 bytes isn't a service's, and a series with
  the same service on both sides, or without both labels, is no edge.
