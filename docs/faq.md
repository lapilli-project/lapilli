# Questions

### What does it need?

Read access to the history the rules are evaluated against, a
Prometheus-compatible API (`--prometheus`: Prometheus, Thanos, Mimir,
VictoriaMetrics, or a cloud's managed Prometheus with `--auth`; CI's
`e2e-compat` runs verdict and `--check-replay` against Thanos 0.42.5 and
VictoriaMetrics 1.156.0); the rules, as files or from the API that holds
them; and a record of what went wrong. Of the history it asks
`/api/v1/query_range`, and, where they are served, `/api/v1/status/config`
and `/api/v1/targets` for the scrape intervals; `--loaded-rules` asks
`/api/v1/rules` too. `--grafana` needs a service account token with
Viewer, and each cloud the read permission in the table in
[Use `lapilli verdict`](verdict.md).

### Do I need a list of incidents?

`lapilli verdict` needs incidents in a file (`--incidents`), or
`--impact`: a PromQL expression that returns something while users are
hurt, such as a 5xx ratio, whose windows become incidents; or both. A thin
log works, and scripts copy one from where it is kept
([Incidents kept elsewhere](incidents.md),
[Incidents nobody wrote down](impact.md)). `lapilli serve` also takes
incidents written down in its UI. Where nothing was written down, a firing
that caught nothing isn't judged, as it can't be told from one nobody
wrote down ([How it judges](how-it-judges.md)).

### Does it write anything to my systems?

- `lapilli verdict` reads the history and files, and writes the report to
  stdout; it sends the report nowhere but to `--push` (a Pushgateway) and
  `--slack-webhook-file`.
- `lapilli preflight` writes nothing: every request it sends goes through
  an allowlist that refuses any other before it is sent, and the ledger is
  opened read-only ([Before it is deployed](serve.md#before-it-is-deployed)).
- `lapilli serve` writes its own ledger, a SQLite file in `--ledger` or a
  Postgres database (`--ledger-url-file`). It proposes changes to rules;
  it carries one out only with `--changes`, which needs a sign-in and
  roles, once a person has approved it in a browser: a pull request,
  opened by a recipe that runs where the repository's credentials are, or
  a write through Grafana's, a ruler's or a cloud's API with credentials
  given for writing ([Changing rules](changing-rules.md)).
- `lapilli ledger copy` writes a ledger into another, empty one: with
  `--to-url-file`, into the Postgres database the file names, and it brings
  the ledger it copies from to its own schema first
  ([Commands](commands.md#lapilli-ledger)).
- The scripts in [recipes](../recipes) run apart, with tokens of their
  own: those that keep tasks in step with a tracker make an issue there
  for each open task
  ([Keep judging](serve.md#tasks-kept-in-step-with-a-tracker)).

### Does it send data anywhere?

Only where its flags and files say. It sends requests to the APIs and
feeds they name, and to what those need in turn: a cloud's credential
chain, an OpenID Connect provider's endpoints, the keys a sign-in in front
signs with (`--proxy-jwt google-iap` fetches Google's from
`www.gstatic.com`, `aws-alb` the load balancer's from
`public-keys.auth.elb.REGION.amazonaws.com`), an SNS subscription to
confirm. There is no telemetry and no update check. What is written for
people to read goes only to a destination a flag names:

- `lapilli verdict`: the report to the Slack webhook in
  `--slack-webhook-file`, and the metrics to `--push`.
- `lapilli serve`: the weekly digest to `--slack-webhook-file`,
  `--team-digest` and `--notify-webhook-file`; what happens (a task made,
  an incident written down) to `--notify-webhook-file`, and to Slack with
  `--notify-slack`; the reports people schedule to those and to
  `--report-dest`.

These hold rule names, labels, incident titles and counts. `lapilli
verdict --redact` gives every name an alias, in what it sends too
([Showing a report outside](redact.md)). The UI's pages run no scripts and
load nothing from elsewhere.

### How much history does it need, and how far back can it go?

As far back as the history keeps what the rules read. `lapilli verdict`
judges the last `--since` (7d), or `--from` to `--to`; the record of a
firing is kept by an incident in the two weeks before it, and `--impact` is
read from two weeks before the period. `lapilli serve` replays `--backfill`
(28d, ten years at most) on its first start, and keeps firings for `--keep`
(400d). Rules that changed over the period are replayed as they stood with
`--rules FILE@TIME` or `--rules-history` ([How it judges](how-it-judges.md),
"Rules that changed"). Some clouds keep less: Google Cloud keeps some
metrics six weeks only, and points older than six weeks ten minutes apart
([Use `lapilli verdict`](verdict.md)).

### How much does it ask of Prometheus?

A replay asks for each query a day at a time, one request for each query
and day shared by every rule that needs it, `--parallel` (8) rules at once
against each API. It waits when
an API pushes back (HTTP 429, 5xx), asks fewer at once after a 429 or a
503, and `--max-rate N` keeps it to N requests a second. `--max-samples`
(10,000,000) bounds what one read of a rule's query may hold, some 64
bytes a sample at the peak; past it the rule isn't replayed. `lapilli
serve` replays the hours since its last run, every hour; `--yield-on-429`,
`--guard` and `--catch-up-hours` keep it off the quota the alerts need,
and `lapilli preflight` estimates its requests, time and memory before it
is deployed ([Use `lapilli verdict`](verdict.md),
[design/quota.md](design/quota.md)).

### Which rules, data sources and clouds?

Rules: Prometheus rule files, `PrometheusRule` resources, Grafana alerting
provisioning files, each cloud's managed rule groups and its alarms written
in PromQL, ConfigMaps holding any of them, Loki ruler rule files
(`--loki-rules`), and directories of them; or as an API holds them:
`--loaded-rules` (Prometheus, Thanos, Mimir, vmalert through
VictoriaMetrics' `-vmalert.proxyURL`, Amazon Managed Service for
Prometheus), `--grafana`, `--ruler` (Mimir, Cortex, Amazon's,
Azure's), a Loki's ruler (`--loki-loaded-rules`), and a cloud's own alarms
(`--alarms`). What
Prometheus fired (`--alerts`) and firings recorded elsewhere (`--firings`)
are judged as they are ([How it judges](how-it-judges.md)).

Queries are replayed in PromQL, against any Prometheus-compatible API, and
in LogQL, against Loki ([Rules on logs: Loki](loki.md)); SQL,
Elasticsearch, InfluxDB and a cloud's own logs aren't
([What it doesn't do yet](limits.md)). AWS, Azure and Google Cloud's
managed Prometheus are read with `--auth`; Oracle Cloud has none, so on OKE
give the Prometheus you run. The four clouds' own alarms are read with
`--alarms`: those in PromQL are replayed, and the rest listed, and judged
from their history once it is copied in
([A cloud's own alarms](cloud-alarms.md)).

### Grafana-managed alerts?

Yes. `--grafana URL` reads the rules a Grafana holds, made in its UI or
not, with a Viewer's token in `--grafana-token-file`; `--rules` reads its
provisioning files. They are evaluated the way Grafana evaluates them,
reduce, threshold, math and classic conditions, and what it does when a
query returns nothing, in its own states. A query error, and a query on a
data source that is neither PromQL nor LogQL, can't be replayed
([How it judges](how-it-judges.md), [What it doesn't do yet](limits.md)).
`--check-replay` sets the replay beside Grafana's own state history
([Is the replay right?](check-replay.md)).

### Does it replay Alertmanager's routing, silences and inhibitions?

No. It counts firings, as the rules fire them; Alertmanager's routing,
grouping, inhibition and silences, and a Grafana notification policy,
aren't read from their configuration ([What it doesn't do yet](limits.md)).
`lapilli serve` can keep the notifications as they were sent, beside the
firings, from a webhook receiver or, from Alertmanager 0.33.0 on, its
event recorder ([Notifications as they were sent](notifications.md)),
which also says when a silence or an inhibition muted a firing, and so
why it paged no one. Its quiet windows are its own: they excuse the firings in them that caught
nothing.

### Does it need Kubernetes?

No. `lapilli` is one binary (`go install lapilli.dev/cmd/lapilli@latest`,
a release, or the image `ghcr.io/lapilli-project/lapilli`), and runs where
it can reach the history. `lapilli serve` listens on `127.0.0.1:8080` and
keeps its ledger in a directory (`--ledger`, `lapilli-ledger` by default)
or in Postgres. The Helm chart runs `lapilli verdict` as a weekly CronJob,
or `lapilli serve` with `mode: serve`
([charts/lapilli](../charts/lapilli/README.md)).

### Can several people use `lapilli serve`?

Yes, behind a sign-in: an OpenID Connect provider (`--oidc-issuer`), the
JWT a sign-in in front signs (`--proxy-jwt`), or the header a proxy sets
(`--user-header`). Roles say who may look (`--viewers`), change
(`--editors`, `--admins`) and approve a change (`--approvers`); admins
can give editors' roles, of every team or of one, on the Roles page. With
no roles and no sign-in of its own, everyone is an admin only when it
listens on this machine alone, and may only look beyond it. On Postgres
it runs as several replicas ([Keep judging](serve.md),
[design/signin.md](design/signin.md),
[Several replicas](serve.md#several-replicas)).

### What is the license?

Apache 2.0 ([LICENSE](../LICENSE)). The UI's font, Heebo, is under the SIL
Open Font License 1.1.

### How far along is it?

In the README's words: early, with one maintainer and room for more
([MAINTAINERS.md](../MAINTAINERS.md)). It has been checked against one
production Grafana, where all 71 of its alert rules replay
([README](../README.md)). [ROADMAP.md](../ROADMAP.md) lists what it
doesn't do yet, area by area, and [What it doesn't do yet](limits.md) the
limits of what it does.

### Where do I ask?

A question goes to
[Discussions](https://github.com/lapilli-project/lapilli/discussions), Q&A,
where the next person with it finds the answer; an idea to Ideas; a report
to show to Show and tell, made safe with `--redact`. A rule that replays
differently from how it fired, or anything else wrong, is an
[issue](https://github.com/lapilli-project/lapilli/issues/new/choose), with
a form for each. A vulnerability is reported privately, as
[SECURITY.md](../SECURITY.md) says. [SUPPORT.md](../SUPPORT.md) says what
to keep out of a post, and [CONTRIBUTING.md](../CONTRIBUTING.md) how to
build, test and send a change. There is no paid support: answers come as
time allows.
