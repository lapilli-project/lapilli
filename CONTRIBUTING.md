# Contributing

Thank you for looking. The most useful contribution of all is a report of a
rule that Lapilli can't replay, or replays differently from how it really
fired, which `lapilli verdict --check-replay` finds. Include the rule, with names and thresholds changed if you like, and,
if you can, what really fired; the issue form "A replay that differs" asks
for each. After that, see [ROADMAP.md](ROADMAP.md) for areas that need
someone, and [MAINTAINERS.md](MAINTAINERS.md) for how contributing leads to
maintaining. To start small, an issue labelled
[good first issue](https://github.com/lapilli-project/lapilli/labels/good%20first%20issue) says where its change goes
and when it's done; one labelled
[help wanted](https://github.com/lapilli-project/lapilli/labels/help%20wanted) needs an account or a language
the maintainer doesn't have. Say in a comment which one you're taking.

Questions and ideas are welcome in
[Discussions](https://github.com/lapilli-project/lapilli/discussions), and
an idea for a larger change is best talked over there before its design
note. Everyone taking part follows the
[Code of Conduct](CODE_OF_CONDUCT.md).

## Build and test

Go 1.26.11 or later, as `go.mod` says; CI tests the latest Go too, which
releases are built with. The recipes' tests need `sh`, `curl` and `jq`, and
are skipped without them.

```sh
go build ./cmd/lapilli   # the lapilli binary
go test -timeout 20m ./...   # everything: ten minutes or more, most of it serve's; go test's own ten is too short
test -z "$(gofmt -l .)" && go mod tidy -diff && go vet ./...   # as CI checks them
test/e2e/run.sh          # the Helm chart on a kind cluster; needs Docker, kubectl, helm
test/e2e/changes.sh      # changes to rules on a real Prometheus, Gitea, Grafana and Mimir; needs Docker, git, curl, jq
test/e2e/loki.sh         # rules in LogQL replayed against a real Loki and Grafana; needs Docker, jq
test/e2e/signin.sh       # lapilli's own sign-in against a real Dex; needs Docker, curl
test/e2e/postgres.sh     # serve on a real Postgres: a restart, a second serve that follows, a copy in and out; needs Docker, curl, jq, python3
test/e2e/replicas.sh     # three serves on one Postgres ledger on kind, the leader killed; needs Docker, kubectl, helm, jq
test/e2e/backtest.sh     # pull request backtests on a real Gitea and Prometheus: the comment and the commit status; needs Docker, git, curl, jq
test/e2e/serve-lb.sh     # the chart at a URL on kind: a LoadBalancer, Gateway API, an Ingress, Dex; Linux
test/e2e/rendered.sh     # rules a chart renders, from a real Gitea, replayed and followed; needs Docker, jq, helm, yq
test/e2e/dashboard.sh    # the chart's Grafana dashboard on a real Grafana and Prometheus; needs Docker, jq, python3
test/e2e/service.sh      # the whole service on kind: Grafana's alerts, Lapilli at a LoadBalancer, Dex, a browser; Linux
test/e2e/ratelimit.sh    # runs at once against a real Prometheus behind nginx's rate limit (429); needs Docker
test/e2e/compat.sh       # verdict and --check-replay against a real Thanos, and VictoriaMetrics with vmalert; needs Docker, jq
test/e2e/kubernetes-rules.sh # --check-replay of kube-prometheus-stack's default rules beside what Prometheus fired, on kind; about 40 minutes; needs Docker, kubectl, helm, jq
test/e2e/rollouts.sh # --rollouts read from a real kube-state-metrics on kind: restarts, scales, a rollback, an autoscaled Deployment; about 35 minutes; same needs
test/e2e/routing.sh  # the notifications worked out from an alert manager's configuration beside those two real Alertmanagers (newest, oldest held to) sent to a webhook; about nine minutes; needs Docker, jq
test/a11y/axe.sh         # axe-core (WCAG 2.2 AA) over every page, in a headless Chromium; needs Go and Docker
```

With `LAPILLI_TEST_POSTGRES` naming a Postgres server (15 or later), the
ledger's, serve's and the command's tests keep their ledgers there, each in
a schema of its own, which the test drops:

```sh
docker run -d --name lapilli-pg -e POSTGRES_PASSWORD=pw -p 127.0.0.1:55432:5432 postgres:18
LAPILLI_TEST_POSTGRES='postgres://postgres:pw@127.0.0.1:55432/postgres?sslmode=disable' \
  go test ./internal/ledger ./internal/serve ./cmd/lapilli
```

CI runs them in two shards on each of Postgres 15 and 18, as
`test/shard.sh 1` and `test/shard.sh 2` do: the second every other of
serve's tests, the first the rest, with the ledger's and the command's.
Each checks first that every test is in one shard, and one only.

A statement that Postgres takes otherwise than SQLite is given for both
(`internal/ledger/dialect.go`); `TestDialectSeam` says where one isn't. A
migration comes in a pair: `internal/ledger/schema/NNN.sql` and
`internal/ledger/schema/postgres/NNN.sql`.
Its changelog entry says **Ledger schema NNN**, which release can't open
the ledger after, and to keep a copy first, pointing to the chart's
README, "Going back after a schema upgrade"; and it gets a row in
[docs/upgrading.md](docs/upgrading.md)'s table, as `TestSchemasDocumented`
holds. So do a route of `/api/v1` in [docs/api.md](docs/api.md), a metric
in [docs/metrics.md](docs/metrics.md), and an option in
[docs/commands.md](docs/commands.md), which is the help itself:
`go test ./cmd/lapilli -run TestCommandsDocumented -write-docs` writes it
again.

`go test ./internal/ledger ./internal/serve -run Scale -v` measures the
ledger and the pages over twelve weeks of a busy ledger, about 200,000
firings; `LAPILLI_SCALE=10` makes it two million. The ledger's runs and
reads are held to times, doubled on CI; the pages to how many statements
and rows they cost the ledger, not to time, so a page that reads every
firing fails on any machine.

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
  - the tests on Linux, macOS and the latest Go, govulncheck, and
    staticcheck (with the checks `staticcheck.conf` names): the checks a
    pull request must pass to merge
  - the tests on Postgres 15 and 18, in two shards each
  - the chart end to end on kind, and served at a URL there
  - the whole service on kind: a scenario of user impact, a re-fire and a
    deploy, Grafana's alerts and notifications, the dashboard, a browser
  - the sign-in against a real identity provider
  - three replicas on one Postgres ledger, on kind
  - the replay checked against what a real Prometheus fired, on the default
    rules of kube-prometheus-stack, on kind (about 40 minutes)
  - the other end-to-end jobs, against real servers in Docker
  - CodeQL
  - on a pull request that changes `site/`, `assets/`, `docs/` or
    `test/a11y/`, the website built and every page checked with axe
    (`pages.yml`)
  - every day, govulncheck on the latest release (`latest-release.yml`);
    weekly, OpenSSF Scorecard
  - weekly, the end-to-end jobs that use real servers, in Docker and on
    kind (the chart's CronJob, the whole service, Kubernetes's default rules), against each server's
    latest image instead of the pinned one (`upstream.yml`).
    By hand, one script against another image:
    `GRAFANA_IMAGE=grafana/grafana:latest test/e2e/loki.sh`

  On a pull request, an end-to-end job runs only when the changes reach
  it: lapilli itself, which each one builds (`go.mod`, `go.sum`, `cmd/`
  and `internal/`, but their tests and testdata), or its script and what
  the script reads (`test/e2e/reached.sh` says which). Of lapilli,
  `e2e-compat` is reached only by what reads the history and replays it
  (`internal/prom`, `internal/replay`, `internal/rules`,
  `internal/source`), and `go.mod` and `go.sum`; `e2e-kubernetes` by
  those and by what sets the replay beside what really fired
  (`internal/verdict`, `internal/firing`, the verdict command); `e2e-routing`
  by those, by `internal/routing`, `internal/content` and the notifications
  command; `e2e-rollouts`
  by what reads the history (`internal/prom`, `internal/source`),
  `internal/response`, and the commands that use it. A change to the
  docs alone runs none of them. A new push to a pull request cancels the
  run in progress. On each push to main every job runs, and weekly the
  fuzzing too. The five checks above must pass to merge; everything that
  runs must pass on main before a release.
- **Dependencies.** A new one needs a reason. Versions are at least two weeks
  old, except for security fixes.
- **Nothing private.** Real rule names, hostnames, incident titles and
  company data don't belong in tests, examples or screenshots; use neutral
  ones.
- **The changelog.** Add a line to `CHANGELOG.md` under Unreleased for
  anything a user would notice.

## Where things live

To read your way in: [docs/how-it-judges.md](docs/how-it-judges.md) for
what a verdict is, then `internal/replay` and `internal/verdict`, which
are small; [docs/design/README.md](docs/design/README.md) lists the design
notes by area, so the one for the part you are changing is the only one to
read.

- `cmd/lapilli`: the commands and their flags: `verdict`, `serve`,
  `preflight`, `ledger copy`, `impact suggest` and `demo`, whose made-up
  rules and history are in `cmd/lapilli/demo/`.
- `internal/rules` reads rule files. A Grafana rule becomes a `Graph`: its
  queries as PromQL, and its threshold, math and reduce steps.
- `internal/sse` evaluates Grafana's math expressions and joins the way
  Grafana does.
- `internal/replay` runs rules against history.
- `internal/prom` talks to Prometheus-compatible APIs, and to Loki's, which
  answers the same way. `internal/cloud` signs requests for AWS, Azure and
  Google Cloud. Every HTTP client goes out through `internal/egress`, so
  that a guard the process sets sees each request before it is sent: a new
  client is made with `egress.Wrap`.
- `internal/preflight` is `lapilli preflight`: what serve will meet before
  it is deployed, read through an allowlist, the ledger through
  `ledger.Probe` (docs/design/preflight.md).
- `internal/source` reads the history and the rules for every command.
- `internal/incident` reads incidents. `recipes/incidents` copies them from
  the tools they are kept in; `internal/serve/recipes_test.go` runs each
  against a server standing in for the tool. The other recipes are tested
  the same way: `recipes/firings` (a cloud's, Datadog's or New Relic's alarm
  history, `internal/firing/recipes_test.go`), `recipes/trackers`
  (tasks kept in step with a tracker, `trackers_recipes_test.go`),
  `recipes/changes` (pull requests for approved changes, one script a git
  host) and `recipes/events` (Alertmanager's event recorder's file and
  Kafka outputs, `events_recipe_test.go`, Kafka by a `kcat` standing in).
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
- `internal/ledger` is lapilli serve's ledger, in SQLite or Postgres
  (`dialect.go`, `postgres.go`):
  - what runs found, the stones, and the tallies by day and week
  - teams, notes, quiet windows, tasks and comments
  - scheduled reports, and proposals to change rules with their approvals
  - the outbox of notices, written in the same transaction as what they tell
    of

  Numbered migrations are in `schema/`, and Postgres's in
  `schema/postgres/`.
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
axe-core over them in a browser. CI doesn't run it over serve's pages: a
change to a page runs it before its pull request. [ACCESSIBILITY.md](ACCESSIBILITY.md) says what
the pages aim for, and what a change to them should check.

## Releases

First, Unreleased in `CHANGELOG.md` becomes `## [X.Y.Z] - date`: the
release fails unless that is the first version it names. A maintainer with
the admin role, which tags `v*` need (the `release tags` ruleset), then
tags `vX.Y.Z` on a commit of `main` whose CI run passed in full: a pull
request's run leaves out the end-to-end jobs its changes don't reach, so
main's is the run that has them all.
`.github/workflows/release.yml` builds
the binaries, the image and the chart, checks them with govulncheck,
attests their provenance, and puts the attestations' Sigstore bundles
beside the release as `lapilli-vX.Y.Z.intoto.jsonl` (SECURITY.md); the
chart goes to `oci://ghcr.io/lapilli-project/charts/lapilli`. The job that
runs the repository's code (`build`) may write nothing; `verify` checks
what it handed on and can write nothing either; the one that publishes
(`publish`) runs none of it, and checks the sha256 of each file `build`
hands on. A published release can't be changed, and its tag can't be
moved: a fix is the next patch. A run that failed before publishing is run
again, with the tag left where it is. Before tagging, a dry run builds and
checks everything and publishes nothing:

```sh
gh workflow run release.yml --ref main -f version=vX.Y.Z
```

`lapilli.dev` is built from `site/` by `.github/workflows/pages.yml`. Its
front page is in English, Spanish, Japanese and Korean (`site/index.md`,
`site/es/`, `site/ja/`, `site/ko/`), with the header's and footer's words in
`site/_data/i18n.yml`; the docs are in English, and some pages in Korean
too (below). A new page in `docs/`
gets a line in `docs/README.md`: the site's sidebar and each page's
description come from it, and the site's build fails on a page it doesn't
list (`site/docs.sh`). A page's `##` and `###` headings are its "On this
page" list on the site and what the docs' search finds in it, so a page
longer than a screen or two has them, no two alike. A change to the English
front page changes the other three with it, or says in the pull request
which it left. Their sections keep the English page's ids, which the header
links to, and the build fails when they don't.

Translations of docs pages live in `docs/LANG/` (Korean in `docs/ko/` and
Japanese in `docs/ja/` so far, a language being one of
`site/_data/i18n.yml`'s), each starting with the line
`<!-- translated from docs/NAME.md at blob HASH -->`, and are put on the site
at `/LANG/docs/NAME/`. In one, `NAME.md` links to the page in the same
language, `../NAME.md` to the English one, `../../assets/NAME.png` is an
image, `../../PATH` is a file of the repository and `../PATH` one of `docs/`.
`docs/LANG/README.md` of each lists every page of `docs/`, as `NAME.md` where
it is translated and `../NAME.md` where it isn't, so a new page of `docs/`
gets a line in each, in English if you don't write the language; the build
and `TestDocsLinks` fail when a page is missing from one. A translation can
lag the English page, and the site then says so on it; to refresh one, update
it and put `git hash-object docs/NAME.md` in its marker as HASH.
