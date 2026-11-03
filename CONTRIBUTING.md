# Contributing

```sh
go build ./cmd/lapilli   # the lapilli binary
go test ./...
gofmt -l . && go vet ./...
test/e2e/run.sh          # the Helm chart on a kind cluster; needs Docker, kubectl, helm
```

Where things live:

- `internal/rules` reads rule files. A Grafana rule becomes a `Graph`: its
  queries as PromQL, and its threshold, math and reduce steps.
- `internal/sse` evaluates Grafana's math expressions and joins the way
  Grafana does.
- `internal/replay` runs rules against history; `internal/prom` talks to the
  API.
- `internal/verdict` gives each firing its stone and writes the reports.
- `internal/source` reads the history and rules for both commands;
  `internal/report` sends to Slack and a Pushgateway.
- `internal/ledger` is lapilli serve's SQLite ledger: what runs found, the
  stones, and the tallies by day and week. `go test ./internal/ledger -run
  TestScale -v` times it over a busy ledger; `LAPILLI_SCALE=10` makes it
  busier.
- `internal/serve` runs lapilli serve: the runs, the web UI (templates and
  CSS embedded, no scripts), the API and /metrics.

A rule Lapilli can't replay, or replays differently from how it really fired,
is the most useful report there is. Include the rule (with names and
thresholds changed if you like) and, if you can, what really fired.
