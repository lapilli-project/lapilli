# Contributing

Thank you for looking. The most useful contribution of all is a report of a
rule that Lapilli can't replay, or replays differently from how it really
fired, which `lapilli verdict --check-replay` finds. Include the rule, with names and thresholds changed if you like, and,
if you can, what really fired. After that, see [ROADMAP.md](ROADMAP.md) for
areas that need someone, and [MAINTAINERS.md](MAINTAINERS.md) for how
contributing leads to maintaining.

## Build and test

```sh
go build ./cmd/lapilli   # the lapilli binary
go test ./...            # everything, in seconds
gofmt -l . && go vet ./...
test/e2e/run.sh          # the Helm chart on a kind cluster; needs Docker, kubectl, helm
```

`go test ./internal/ledger ./internal/serve -run Scale -v` times the ledger
and the pages over a busy ledger: two million firings in the ledger, 200,000
for the pages. `LAPILLI_SCALE=10` makes it busier. The budgets are part of the
tests, so a change that slows a run or a page fails.

## Pull requests

- **Keep them small.** One change per pull request, with a test that would
  have failed without it.
- **Discuss larger changes first.** A new command, a new kind of data in the
  ledger, or a change to how a firing is judged starts as a design note in
  `docs/design/` ([GOVERNANCE.md](GOVERNANCE.md)).
- **What takes input from outside is reviewed as an attacker would.** A
  change that opens a port or a path, takes a token, or reads what another
  system sends (a webhook, a file a job writes, a name a cloud gives) gets a
  review from that point of view before it is released, apart from the
  review of what it does: what can someone send before they prove who they
  are, how much can they make lapilli read, fetch, keep or judge again, and
  where do the names they choose end up (pages, Slack, `/metrics`, an
  agent). Each fix comes with a test that fails without it. 0.20.1 is why:
  three reviews of 0.20.0's new ports, all of what they did, missed that
  one read a body before its token (GHSA-ggwv-m6g7-jchx).
- **CI runs:**
  - the tests on Linux, macOS and the latest Go
  - the chart end to end on kind
  - govulncheck
  - CodeQL

  All of them must pass.
- **Dependencies.** A new one needs a reason. Versions are at least two weeks
  old, except for security fixes.
- **Nothing private.** Real rule names, hostnames, incident titles and
  company data don't belong in tests, examples or screenshots; use neutral
  ones.
- **The changelog.** Add a line to `CHANGELOG.md` under Unreleased for
  anything a user would notice.

## Where things live

- `cmd/lapilli`: the flags of `lapilli verdict` and `lapilli serve`.
- `internal/rules` reads rule files. A Grafana rule becomes a `Graph`: its
  queries as PromQL, and its threshold, math and reduce steps.
- `internal/sse` evaluates Grafana's math expressions and joins the way
  Grafana does.
- `internal/replay` runs rules against history.
- `internal/prom` talks to Prometheus-compatible APIs. `internal/cloud` signs
  requests for AWS, Azure and Google Cloud.
- `internal/source` reads the history and the rules for both commands.
- `internal/incident` reads incidents. `recipes/incidents` copies them from
  the tools they are kept in; `internal/serve/recipes_test.go` runs each
  against a server standing in for the tool.
- `internal/verdict` gives each firing its stone and writes the reports.
- `internal/report` sends to Slack, a Pushgateway and webhooks. Their URLs are
  secrets: they are read from files and never shown.
- `internal/ledger` is lapilli serve's SQLite ledger:
  - what runs found, the stones, and the tallies by day and week
  - teams, notes, quiet windows, tasks and comments
  - the outbox of notices, written in the same transaction as what they tell
    of

  Numbered migrations are in `schema/`.
- `internal/serve` runs lapilli serve:
  - the hourly runs
  - the web UI: templates and CSS embedded, no scripts
  - the API and `/metrics`
  - roles from the sign-in proxy's headers
  - the notifier

  Design notes: [docs/design/serve.md](docs/design/serve.md) and
  [docs/design/work.md](docs/design/work.md).

## The UI and its languages

The UI is written in English, with every text going through `t` in the
templates or `tr` in Go. `internal/serve/i18n/LANG.json` gives each English
text in another language.

To add a language, add its file and its name to `names` in
`internal/serve/i18n.go`. `go test ./internal/serve` renders every page in
every language. It fails on a text without a translation, or on a
translation that takes different arguments from its English.

The UI's colours are tokens at the top of `internal/serve/static/lapilli.css`.
Chart colours are checked for colour blindness and contrast; keep text at
4.5:1 or more on the surface it sits on.

## Releases

A maintainer tags `vX.Y.Z` on `main`. `.github/workflows/release.yml` builds
the binaries, the image and the chart, checks them with govulncheck, and
attests their provenance. `lapilli.dev` is built from `site/` by
`.github/workflows/pages.yml`.
