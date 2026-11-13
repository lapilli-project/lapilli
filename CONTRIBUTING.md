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
test/e2e/changes.sh      # changes to rules on a real Prometheus, Gitea and Grafana; needs Docker, jq
test/e2e/loki.sh         # rules in LogQL replayed against a real Loki and Grafana; needs Docker, jq
test/e2e/signin.sh       # lapilli's own sign-in against a real Dex; needs Docker, curl
test/e2e/serve-lb.sh     # the chart at a URL on kind: a LoadBalancer, Gateway API, an Ingress, Dex; Linux
test/e2e/rendered.sh     # rules a chart renders, from a real Gitea, replayed and followed; needs Docker, jq, helm, yq
test/e2e/dashboard.sh    # the chart's Grafana dashboard on a real Grafana and Prometheus; needs Docker, jq, python3
test/e2e/service.sh      # the whole service on kind: Grafana's alerts, Lapilli at a LoadBalancer, Dex, a browser; Linux
test/e2e/ratelimit.sh    # runs at once against a real Prometheus behind nginx's rate limit (429); needs Docker
test/a11y/axe.sh         # axe-core (WCAG 2.2 AA) over every page, in a headless Chromium; needs Docker
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
  - the chart end to end on kind, and served at a URL there
  - the whole service on kind: a scenario of user impact, a re-fire and a
    deploy, Grafana's alerts and notifications, the dashboard, a browser
  - the sign-in against a real identity provider
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
- `internal/prom` talks to Prometheus-compatible APIs, and to Loki's, which
  answers the same way. `internal/cloud` signs requests for AWS, Azure and
  Google Cloud.
- `internal/source` reads the history and the rules for both commands.
- `internal/incident` reads incidents. `recipes/incidents` copies them from
  the tools they are kept in; `internal/serve/recipes_test.go` runs each
  against a server standing in for the tool. The other recipes are tested
  the same way: `recipes/firings` (a cloud's alarm history), `recipes/trackers`
  (tasks kept in step with a tracker, `trackers_recipes_test.go`) and
  `recipes/changes` (pull requests for approved changes, one script a git
  host).
- `internal/content` checks what a rule says when it fires
  (docs/design/content.md). `internal/schedule` reads the presets and cron
  expressions reports are scheduled by.
- `internal/verdict` gives each firing its stone and writes the reports.
- `internal/signin` says who a request comes from when serve signs people
  in itself: OpenID Connect, the signed headers of a sign-in in front, and
  sessions in signed cookies (docs/design/signin.md). `idptest` is a
  provider for tests.
- `internal/report` sends to Slack, a Pushgateway and webhooks. Their URLs are
  secrets: they are read from files and never shown.
- `internal/ledger` is lapilli serve's SQLite ledger:
  - what runs found, the stones, and the tallies by day and week
  - teams, notes, quiet windows, tasks and comments
  - scheduled reports, and proposals to change rules with their approvals
  - the outbox of notices, written in the same transaction as what they tell
    of

  Numbered migrations are in `schema/`.
- `internal/serve` runs lapilli serve:
  - the hourly runs
  - the web UI: templates and CSS embedded, no scripts
  - the API and `/metrics`
  - roles from the sign-in: its own (`signin.go`), or a proxy's headers
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
4.5:1 or more on the surface it sits on, and borders and the focus ring at
3:1 (`TestContrast` reads the tokens). Every page is checked for its
structure in every language (`TestPagesAccessible`: one h1, landmarks, a
skip link, a name for every control, headers with scope, a text for every
chart, which has its numbers in a table too); `test/a11y/axe.sh` runs
axe-core over them in a browser.

## Releases

A maintainer tags `vX.Y.Z` on `main`. `.github/workflows/release.yml` builds
the binaries, the image and the chart, checks them with govulncheck,
attests their provenance, and puts the attestations' Sigstore bundles
beside the release as `lapilli-vX.Y.Z.intoto.jsonl` (SECURITY.md). The job
that runs the repository's code (`build`) may write nothing; the one that
publishes (`publish`) runs none of it, and checks the sha256 of each file
`build` hands on. Before tagging, a dry run builds and checks everything and
publishes nothing:

```sh
gh workflow run release.yml --ref main -f version=vX.Y.Z
``` `lapilli.dev` is built from `site/` by
`.github/workflows/pages.yml`.
