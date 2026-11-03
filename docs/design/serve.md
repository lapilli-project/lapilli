# `lapilli serve`: a ledger of verdicts

Status: proposed, November 2026. None of this is built yet.

## Why

`lapilli verdict` runs, prints its report and is gone. What it leaves is a
Slack message and a few numbers on a Pushgateway, and so it can't do three
things a team needs to keep doing week after week:

- **Show a trend.** "This rule has been noisy six weeks running", or "since
  its `for` went from 5m to 15m it fires 13 times a week, not 39", needs last
  week's verdict beside this week's.
- **Take incidents written down later.** Incidents are written down hours or
  days after, if at all. Monday's run never sees the incident written on
  Tuesday, and the thinner the incident log, the worse every verdict is.
- **Replay the rules as they stood.** The weekly run replays the rules as they
  are when it runs. `--rules FILE@TIME` fixes that, but someone has to know
  when each rule changed.

`lapilli serve` keeps the urn instead of emptying it: a ledger of the
firings, the incidents and every version of the rules, recorded as they
happen, and judged for any period when someone looks.

## What stays the same

- **One binary.** `lapilli verdict` and the CronJob chart stay as they are;
  `serve` is another command in the same binary and image.
- **One engine.** `serve` replays with `internal/replay` and judges with
  `verdict.Judge`. For the same rules, incidents and period, its report is
  the one `lapilli verdict --json` writes, and a test holds it to that.
- **Read only.** It reads history, rules and incident files, and sends only
  what it is asked to send, to Slack. It doesn't edit rules, route alerts,
  page anyone or manage incidents.
- **Not a controller.** It reads mounted ConfigMaps and HTTP APIs. It doesn't
  watch the Kubernetes API and needs no RBAC.
- **Every cloud alike.** The same `--auth aws`, `azure` and `google`, and on
  Oracle Cloud the Prometheus you run on OKE.

## Shape

```
                       ┌──────────────── lapilli serve ────────────────┐
rules: files, API, ───▶│ watch   a new version of the rules            │
       Grafana         │         each time they change                 │
history: Prometheus ──▶│ run     every hour, replay the hours since    │──▶ web UI
incidents: file, UI ──▶│         the last run                          │──▶ /api/v1 (JSON)
                       │ ledger  firings, incidents, rule versions,    │──▶ /metrics, for any Grafana
                       │         runs: SQLite on a volume              │──▶ Slack, weekly
                       │ judge   any period, when it is read           │
                       └───────────────────────────────────────────────┘
```

## The ledger keeps facts; stones are given when it is read

The ledger stores what happened (firings, incidents, versions of the rules)
and never a stone. The verdict for a period is `verdict.Judge` over the facts
in it, worked out when someone asks. Judging takes milliseconds; replaying
is what is slow, and each hour of history is replayed once (and again a few
hours later, below).

So:

- An incident written on Tuesday turns last week's black stones white at once.
- A change to `--lead` or `--match` judges the whole history again, without
  asking Prometheus.
- A trend is a judgment per week.

What did need Prometheus is stored as a result: suggestions, a noisy rule
replayed with longer `for`s; and change reviews, below.

### Tables

| Table | A row is | Written by |
|---|---|---|
| `rule_versions` | the whole set of rules, from when `serve` first saw it | the watcher |
| `firings` | rule, labels, start, end, and the version that fired it | runs |
| `incidents` | id, title, start, end, labels; where it came from (`file`, `ui`, `impact`), and for an impact window whether it was confirmed or dismissed | runs, the UI |
| `runs` | when, the period, the version, the rules unfinished or unsupported, errors | runs |
| `settings` | a judging setting changed in the UI | the UI |
| `changes` | who changed what and when, before and after | every write |
| `results` | suggestions and change reviews, with their period | runs |

Times are kept in UTC. The schema changes through numbered SQL migrations
embedded in the binary and applied at start; a ledger written by a newer
`lapilli` is refused, not touched.

### Storage: SQLite

One file on a volume, through the pure-Go driver `modernc.org/sqlite`: no
cgo, so the image stays distroless and static. Measured on linux/amd64, it
adds about 5 MB to the 13.5 MB binary and six modules it doesn't link now.
A firing is a row of a few hundred bytes, so a year of a busy set of rules
is tens of megabytes. There is one writer: the Deployment runs one replica.

Also considered: `github.com/ncruces/go-sqlite3` (about 6 MB, three new
modules); bbolt (no SQL: harder for the next maintainer to read and
query); JSON files (no transaction to replace a period's firings in). Postgres
can come later behind the same queries, for whoever needs more than one
replica.

## Runs

Every `--every` (default 1h) a run:

1. takes from the ledger the versions of the rules in effect over its period;
2. replays from the end of the last run, less `--settle` (default 6h), to
   now, starting earlier by the longest `for`, `keep_firing_for` and group
   interval among the rules, so a firing's pending time is seen;
3. replaces the firings that start in its period with the replay's, and ends
   those still open that the replay saw resolve;
4. does the same for the windows `--impact` finds;
5. records itself.

Replaying the last hours again picks up points that arrived late, as a
cloud's own metrics of its services often do.

One run at a time. A run that fails or is cut short leaves the ledger as the
run before it left it, and the next run covers its period.

On first start, with an empty ledger, a run replays `--backfill` (default
28d) with the rules as they are then, as `lapilli verdict` would. The UI marks
those weeks as replayed with later rules.

### Versions of the rules

The watcher reads the rules every `--watch` (default 1m): the files of a
mounted ConfigMap, `/api/v1/rules`, or Grafana's export. When the digest of
the set (every rule's `Key`) changes, it records a new version, in effect
from then. These are the `--rules FILE@TIME` a person would otherwise have to
work out. The time is when `serve` saw the change: a ConfigMap's update reaches
a mounted volume a minute or so after it is applied.

### Change reviews

When a new version is recorded, a run replays the rules that changed, as they
were and as they are, over the week before: `--compare --changed-only`. It
stores the comparison, for the rule's page: "had this change been in place
last week, 39 firings → 13, and both incidents still caught". A week later
the page also sets the week before the change beside the week after, from the
ledger.

### The weekly digest

At `--digest` (default `mon@09:00`, in `--tz`) `serve` judges the week before
from the ledger, works out suggestions, stores them, and posts to Slack the
same report the CronJob posts. A digest missed while `serve` was down is sent
when it is back.

## Incidents

They come from three places:

- `--incidents FILE`, read again by each run. The file owns them: the UI
  shows them but doesn't change them.
- The UI, where they are written, edited and deleted. An id the file already
  uses is refused.
- `--impact`, whose windows are candidates, as they are now. In the UI each
  can be confirmed, becoming an incident with a title, or dismissed, and it
  no longer counts.

Two shortcuts keep the incident log from staying thin. On a black stone,
"this was real" writes an incident from the firing's start, end and `--match`
labels. On an impact window that no rule caught, "confirm" does the same.

## Settings

One rule decides where a setting lives: **what needs Prometheus again is
set in the chart's values; what only judges the ledger again can also be
changed in the UI.**

| Only in values | In values, or changed in the UI |
|---|---|
| where the history, rules and incidents come from; `--auth`; tokens; Slack | `--lead`, `--match`, `--refire` |
| `--impact`, `--impact-for`, `--step` | `--noisy-firings`, `--noisy-precision` |
| `--every`, `--settle`, `--backfill`, `--digest`, retention | |

A setting changed in the UI shows who changed it and when, beside its value
in values, and can be reset. `/settings.yaml` gives the changes as values to
commit, so the UI doesn't stay the source of truth for long. Any setting on
the right can also be tried out, the period judged with it, without saving.

## Web UI

Pages are rendered on the server with `html/template` and embedded in the
binary: no JavaScript build and no npm dependencies. Charts are SVG drawn on
the server.

- **Overview**: the last twelve weeks. Per week, the stones, and the
  incidents caught, caught only by noisy rules, and missed; the noisiest
  rules; when the rules changed; the last run.
- **Rules**: each rule's stones per week, its precision, how many weeks
  running it has been noisy, and its suggestion.
- **A rule**: its versions and what changed in each, change reviews, its
  firings, the incidents it caught.
- **Incidents**: written down and found; who caught each, and how soon; add,
  edit, confirm, dismiss.
- **Firings**: by day and rule, with "this was real" on each black stone.
- **Settings** and **Runs**.

```
Lapilli                                  last run 10:00 ✓ · rules changed Tue 14:12

WEEK    FIRED  ○ CAUGHT  ● NOTHING   INCIDENTS  CAUGHT  NOISY ONLY  ✗ MISSED
10-26   148    5         143         4          1       1           2
10-19   161    3         158  ▲      2          1       0           1
10-12   190    4         186         3          2       0           1

NOISY                  WEEKS  FIRED/WEEK  SUGGESTION
CheckoutLatencyP99     6      14          for 5m → 10m: 14 → 6, catches kept
```

## API

- `GET /api/v1/report?from=&to=`: what `lapilli verdict --json` writes, for
  that period.
- `GET`, `POST`, `PUT` and `DELETE /api/v1/incidents`.
- `GET /api/v1/rules`, `/api/v1/runs` and `/api/v1/settings`.
- `GET /metrics`: the families `--metrics` writes, over the last seven days,
  and `serve`'s own (how long runs take, the last that succeeded). A
  Prometheus that scrapes it takes the Pushgateway's place.
- `GET /healthz` and `/readyz`.

A Grafana dashboard graphs `/metrics` from any Prometheus-compatible data
source. Its JSON is in the repository, and the chart can ship it as a
ConfigMap for Grafana's dashboard sidecar.

## Security

The UI shows rule expressions and incidents: internal, though not secret.

- **Closed by default.** The binary listens on `127.0.0.1:8080`; the chart's
  Service is a ClusterIP. Reach it with `kubectl port-forward`, or behind the
  sign-in you already have: oauth2-proxy, an ingress's, a cloud's
  identity-aware proxy. `serve` has no login of its own.
- **Who wrote what.** With `--user-header X-Forwarded-Email` (or whichever
  header your proxy sets), writes are recorded under that name; otherwise as
  anonymous. Set it only behind a proxy that sets the header and strips it
  from callers.
- **Writes** are POST, PUT and DELETE only, checked by Go's
  `http.CrossOriginProtection`, and limited in size. `--read-only` turns
  them off.
- **What is shown.** Rule names, labels and incident text come from other
  systems and are escaped as such. Pages send a Content-Security-Policy that
  allows no scripts. Secrets are read from files and never shown, and a URL's
  user and password are cut from it wherever it is shown.
- **What goes out** is what goes out now: queries for history and rules, and
  the digest to Slack. Redirects aren't followed.
- **The pod** runs as non-root, with a read-only root filesystem; the
  ledger's volume is the one place it writes. It gets a service account token
  only when a cloud's workload identity needs one.

## Chart

`mode: cronjob`, the default and what the chart does now, or `mode: serve`:
a Deployment of one replica (strategy `Recreate`), a ClusterIP Service, and
a PersistentVolumeClaim (`ledger.size`, `ledger.storageClass`,
`ledger.existingClaim`; `ledger.persistent: false` for an emptyDir, to try it
out). The arguments are written once, in `_helpers.tpl`, for both modes.
Optional: a ServiceMonitor, and the dashboard's ConfigMap.

Without Kubernetes, `lapilli serve --ledger DIR` runs the same way from a
container with a volume, or as a service on a host.

## Steps

Each step is one PR, merged when it is done.

| Step | What | Release |
|---|---|---|
| 0 | Move reading rules, history and suggestions out of `cmd/lapilli` into a package both commands use. Nothing changes for users. | |
| 1 | `serve` without a UI: watcher, runs, backfill, the ledger, `/api/v1/report`, `/metrics`, health. The chart's serve mode; the kind test runs it. | |
| 2 | The UI, read only: overview, rules, a rule, incidents, firings, runs. | |
| 3 | Writing: incidents, "this was real", confirm and dismiss, settings, `--user-header`, `--read-only`. | v0.6.0 |
| 4 | The weekly digest, change reviews, the Grafana dashboard. | v0.7.0 |

Tests that hold it to its word:

- For one period, rules and incidents, `serve`'s report equals
  `lapilli verdict --json`'s.
- Running the same hours twice changes nothing in the ledger.
- A run killed halfway leaves the ledger as the run before left it.
- A rule changed mid-week is replayed as each version stood, as
  `--rules FILE@TIME` would replay it.

## Not in this

A login of its own; more than one replica; Postgres; several Prometheus
servers in one ledger; firings from Alertmanager's webhook; incidents from
incident tools; a cloud's own alarms; editing rules. Each is a piece someone
can take on later. Alertmanager's webhook, beside the replay, would also show
where a replay differs from what really fired.
