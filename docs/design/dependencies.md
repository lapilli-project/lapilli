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
| Istio | `sum by (source_workload, destination_workload) (rate(istio_requests_total{reporter="source"}[5m])) > 0` | `source_workload,destination_workload` |
| Linkerd | `sum by (deployment, dst_deployment) (rate(response_total{direction="outbound"}[5m])) > 0` | `deployment,dst_deployment` |
| An OpenTelemetry or Tempo service graph | `sum by (client, server) (rate(traces_service_graph_request_total[5m])) > 0` | `client,server` |

`lapilli impact suggest` says which of them the history has, beside the
`--impact` it already suggests, and the flags to give.

The expression is read at an hour's step (`--depends-step`, 10m at the
finest) over the period and the `--lead` before it. A graph keeps no record
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
