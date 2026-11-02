# Contributing

```sh
go build .            # the lapilli binary
go test ./...
gofmt -l . && go vet ./...
```

Where things live:

- `internal/rules` reads rule files. A Grafana rule becomes a `Graph`: its
  queries as PromQL, and its threshold, math and reduce steps.
- `internal/sse` evaluates Grafana's math expressions and joins the way
  Grafana does.
- `internal/replay` runs rules against history; `internal/prom` talks to the
  API.
- `internal/verdict` gives each firing its stone and writes the reports.

A rule Lapilli can't replay, or replays differently from how it really fired,
is the most useful report there is. Include the rule (with names and
thresholds changed if you like) and, if you can, what really fired.
