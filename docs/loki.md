# Rules on logs: Loki

Rules written in LogQL are replayed against Loki as PromQL rules are
against Prometheus: each metric query (`count_over_time`, `rate`,
`bytes_over_time`, `quantile_over_time` over an `unwrap`, `sum by`, ...)
asked at every evaluation through Loki's `query_range`, and the rule's
`for` and Grafana's states as for any rule
([docs/design/loki.md](design/loki.md)).

```sh
# a Loki ruler's rules, as it holds them, beside the Prometheus rules
lapilli verdict --rules rules/ --loki-loaded-rules --loki http://loki:3100 \
  --prometheus http://prometheus:9090 --since 28d --incidents incidents.yaml

# Grafana's rules on a Loki data source: nothing more to give
lapilli verdict --grafana https://grafana.example.com --grafana-token-file token \
  --prometheus http://prometheus:9090 --check-replay
```

- **Loki's ruler.** `--loki-rules PATH` reads its rule files, beside any
  other source of rules or alone; `--loki-loaded-rules` reads the rules the
  Loki at `--loki` holds, from its ruler's API, with where it runs each
  group. `--loki-token-file` and `--loki-tenant` (`X-Scope-OrgID`) are
  sent to it. `lapilli verdict` needs `--prometheus` beside them only for
  what reads metrics; `lapilli serve` always needs it, as it checks the
  replay against what really fired there. Loki 3.7.9 drops a rule file's
  `query_offset` and `keep_firing_for` and runs without them; so does the
  replay, and says which rules had them. `--check-replay` reads the `ALERTS`
  Loki's ruler writes where its `remote_write` sends them, the Prometheus at
  `--prometheus`. A rule sharing a name with another is told apart as
  `Name [loki:namespace/group]`. A change proposed for one read with
  `--loki-loaded-rules`, with its backtest, is written through Loki's
  ruler once approved, where `lapilli serve --changes-in` names its
  namespace as `loki:NAMESPACE`, with `--changes-loki-token-file`
  ([changing rules](changing-rules.md)); otherwise, and for one read from
  its files, it is for a person.
- **Grafana's rules.** With `--grafana`, the type of each data source a
  rule reads is asked of Grafana; a Loki one's queries are asked through
  Grafana's data source proxy (`/api/datasources/proxy/uid/UID`, a Viewer's
  token, by GET), so the Loki, tenant and credentials are those Grafana
  used. Instant and range queries, Grafana's step and its macros
  (`$__auto`, `$__interval`, `$__range`), and its reducers over a range,
  worked out by Lapilli as LogQL has no subqueries. Read from provisioning
  files, a query whose model says `datasource.type: loki` is asked of
  `--loki`.
- **Not replayed, and listed with why:** log queries, which return lines,
  not numbers; a reduce asking Loki finer than a point a second (86,400 a
  series a day); a query Grafana can't run (over Loki's 11,000 points a
  window, or a step or `queryType` Grafana can't read); classic conditions
  over Loki; a condition reading
  Prometheus and Loki together; and every data source that is neither
  PromQL nor LogQL, SQL ones among them, by the type Grafana gives.
- **Late lines.** A log shipper sends a line a second or two after it was
  written; Loki's ruler saw what had arrived. `--late 2s` replays as if
  each line arrived that late. A range Loki refuses for its series
  (`max_query_series`) is asked again in shorter ranges, down to one
  evaluation; an evaluation over the limit fails, as it does in Loki's
  ruler. The chart takes `--loki URL --loki-loaded-rules` (and
  `--loki-tenant`) through `extraArgs`, beside its rules or alone.

Measured against Loki 3.7.9 and Grafana 13.3.1: see the design for the
recordings kept as test data. `test/e2e/loki.sh` runs the same against
real servers in Docker.
