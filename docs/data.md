# What it reads, keeps and sends

Out of the box Lapilli reads: your alert rules, your incidents, and the
history of the metrics the rules are evaluated against. It sends nothing
anywhere until a flag names where. There is no telemetry and no update
check: every request it makes goes to an API, a feed, a webhook or a
provider you named, or to what those need in turn (a cloud's credential
chain, a sign-in's keys). This page gathers what the other pages say of
data; each section links to where it is told in full.

## What `lapilli verdict` reads

- Rules, from files (`--rules`, `--rules-history`, `--compare`), or from the
  API that holds them: `--loaded-rules`, `--grafana`, `--ruler`,
  `--loki-loaded-rules`, and a cloud's own alarms with `--alarms`.
- The history: the Prometheus-compatible API at `--prometheus`, and Loki at
  `--loki` for rules on logs. It asks for the rules' queries a day at a
  time, and, where they are served, `/api/v1/status/config` and
  `/api/v1/targets` for the scrape intervals
  ([Questions](faq.md#what-does-it-need),
  [How it asks the history](verdict.md#how-it-asks-the-history)).
- Incidents, from the file `--incidents` names, or found in the history by
  `--impact`. Scripts in [recipes/incidents](../recipes/incidents) copy that
  file from a tracker or an incident tool, each with a token of its own,
  and run apart from Lapilli ([Incidents kept elsewhere](incidents.md)).
- Firings: what Prometheus fired (`--alerts`), or firings recorded
  elsewhere (`--firings`).
- With `--on-call`, the iCalendar feeds the file names, whose URLs hold
  their secret. With `--promtool`, it runs promtool on the test files an
  expectations file names.

The credentials it uses are the ones you give it: a token in a file
(`--bearer-token-file`, `--grafana-token-file`, `--ruler-token-file`,
`--loki-token-file`), or, with `--auth aws`, `azure` or `google`, the cloud
credentials the machine or pod already has. A Grafana token needs only the
Viewer role, and each cloud only its read permission
([Questions](faq.md#what-does-it-need)).

## What `lapilli verdict` writes

The report goes to stdout, as text, `--json`, `--csv`, `--markdown` or
`--metrics`. Beside that it writes only what a flag names:

- `--push URL` puts the metrics on a Pushgateway;
- `--slack-webhook-file FILE` posts the report to the Slack webhook whose
  URL is in the file;
- `--draft-expectations FILE` writes a file of expectations drafted from
  the period.

It writes nothing to the systems it reads: it asks them questions and
changes none of them ([Commands](commands.md#lapilli-verdict)).
`lapilli preflight` writes nothing at all: every request it sends goes
through an allowlist that refuses any other, and it opens the ledger
read-only ([Before it is deployed](serve.md#before-it-is-deployed)).

## What `lapilli serve` keeps

`lapilli serve` keeps a ledger: one SQLite file (`ledger.db` in the
directory `--ledger` names), or a Postgres database whose connection
string is in the file `--ledger-url-file` names
([What the ledger keeps](serve.md#what-the-ledger-keeps),
[The ledger in Postgres](serve.md#the-ledger-in-postgres)). The schema
under [internal/ledger/schema](../internal/ledger/schema) says what is in
it:

- each firing, with its labels, and the stones it was judged with, by day
  and by week;
- the versions of the rules, as the files held them, and the proposals to
  change them;
- the incidents, those written down, copied in, and found by an impact
  signal;
- notes, tasks, comments, teams, quiet and marked windows, goals, and
  people's verdicts on firings;
- the notifications as they were sent, if you point your alert manager at
  `--notify-listen`, with the mutes of silences and inhibitions, by ID, but
  not a silence's comment or who made it
  ([Notifications as they were sent](notifications.md));
- the activity log: who changed what, and when, through the UI, the API,
  `--config` and Lapilli itself
  ([Sources and the activity log](serve.md#sources-and-the-activity-log));
- the names of people, as the sign-in gives them: in the activity log, on
  tasks and comments, in the roles given in the UI, and, for someone who
  signed out, that they did and when
  ([design/signin.md](design/signin.md));
- the notices waiting to be sent, and the runs it made.

The session key is never kept in the ledger: it is a file
(`--session-key-file`) or made for the process
([design/signin.md](design/signin.md)).

How long: `--keep` (400 days unless given) drops firings and what hangs on
them: people's verdicts, runs, results, notifications, mutes, on-call turns,
and the incidents found by an impact signal. The activity log is kept for
`--keep-activity` (800 days unless given, or `--keep` when longer). With
`--keep 0` nothing is dropped. Incidents written down, the versions of the
rules, notes, tasks and comments are not dropped by `--keep`: they stay until
someone removes them.

To remove all of it, stop `serve` and delete the `--ledger` directory, or
drop the Postgres database. An upgrade may change the ledger's schema as
`serve` starts, and the release before can't open it after: take a copy
first ([Upgrading](upgrading.md#before-an-upgrade-that-changes-the-schema)).

## Who can see and change it

`lapilli serve` listens on `127.0.0.1:8080` unless told otherwise
([Signing people in](serve.md#signing-people-in),
[Who may change what](serve.md#who-may-change-what)).

- Sign-in, one of three: `--oidc-issuer` (Lapilli signs people in with an
  OpenID Connect provider); `--proxy-jwt` (the JWT a sign-in in front
  signs, checked against its keys); `--user-header` (a header a proxy sets,
  which Lapilli trusts, so the proxy must be the only way in).
- Roles: viewer (`--viewers`; with `--oidc-issuer`, `--proxy-jwt` or
  `--viewers`, only those a role names come in), editor (`--editors`, and a team's editors, given in the UI), admin
  (`--admins`), and approver (`--approvers`, or the admins). A viewer
  reads; an editor writes incidents, tasks, comments, notes and windows;
  an admin also the settings, teams and roles. `--read-only` refuses every
  write ([The HTTP API](api.md#roles)).
- With no roles: on loopback alone, everyone who reaches it is an admin,
  since that is the machine's users. Beyond it (`:8080`, as the chart
  listens), everyone may only look, unless `--everyone-admin`. With
  `--oidc-issuer` or `--proxy-jwt`, everyone signed in may only look until
  roles say otherwise.
- Posts from other sites are refused, the pages run no scripts, and only
  the host names it is reached by are answered
  ([Behind a proxy](serve.md#behind-a-proxy)).

The other ports exist only when a flag turns them on, each with a way in
of its own, none of which is the UI's sign-in
([The HTTP API](api.md)):

| Port | Takes | Protected by |
|---|---|---|
| `--incidents-listen` | incidents, firings copied in, tasks kept in step with a tracker, and the changes recipes read | a bearer token for each kind (`--incidents-token-file`, `--firings-token-file`, `--tasks-token-file`, `--changes-token-file`), 16 characters at least, no two alike ([The incidents port](api.md#the-incidents-port)) |
| `--notify-listen` | notifications from Alertmanager, Grafana, Amazon SNS, Azure, Google Cloud and OCI | a token (`--notify-token-file`), Grafana's HMAC signature, or SNS's signature ([The notifications port](api.md#the-notifications-port)) |
| `--backtest-listen` | a pull request's rules from a CI job | an ID token checked against the issuers, audiences and claims `--backtest-trust` names, or a plain token it names ([The backtest port](api.md#the-backtest-port)) |
| `--metrics-listen` | `/metrics` and the health checks, nothing else | nothing: let only Prometheus reach it ([Behind a proxy](serve.md#behind-a-proxy)) |

`/mcp` is on the UI's port, behind the same sign-in. An agent reads what
the pages show; with `--mcp-compare` it can replay a change, and with
`--mcp-propose` keep a proposal for a person to approve, and no tool
approves or applies anything ([Ask it from an agent](mcp.md)).

## What it sends, and where

Each of these is off until you give the flag or setting.

| What | Where | Turned on by |
|---|---|---|
| The report | a Slack webhook | `lapilli verdict --slack-webhook-file` |
| The metrics | a Pushgateway | `lapilli verdict --push` |
| The weekly digest, a team's digest, and reports people schedule | Slack webhooks, the notice webhook | `--slack-webhook-file`, `--team-digest`, `--report-dest` ([Keep judging](serve.md)) |
| What happens (a task made, an incident written down, a source down) | a webhook, and Slack | `--notify-webhook-file`, `--notify-slack`, `--webhook-dest` ([Notices to a webhook and Slack](serve.md#notices-to-a-webhook-and-slack)) |
| Requests for the history and the rules | the APIs you name | `--prometheus`, `--loki`, `--grafana`, `--ruler`, `--alarms` |
| Requests for sign-in | your OpenID Connect provider; with `--proxy-jwt google-iap` or `aws-alb`, the keys' URLs of Google or of the load balancer's region | `--oidc-issuer`, `--proxy-jwt` |
| One request, to confirm a subscription | an Amazon SNS host only | `--notify-sns` |
| Changes to rules | a recipe's pull request, Grafana, a ruler, a cloud's API | `--changes` (next section) |

A post to a webhook can be signed with `--notify-webhook-secret-file` (and
`--webhook-dest-secret` for each `--webhook-dest`), so a receiver can tell
it came from your Lapilli
([Signed posts to the webhook](serve.md#signed-posts-to-the-webhook)).
Without the flag a post carries no signature. `serve` follows no redirects
on what it posts.

What is posted holds rule names, labels, incident titles and counts.
`lapilli verdict --redact` gives every name an alias, in what it sends too
([Showing a report outside](redact.md)).

The scripts in [recipes](../recipes) run apart from Lapilli, with tokens of
their own, and send where they are told. Those that keep tasks in step with
a tracker make an issue there for each open task
([Tasks kept in step with a tracker](serve.md#tasks-kept-in-step-with-a-tracker)).

## What it can change in your systems

Nothing by default. `lapilli serve` proposes changes to rules; it carries
one out only with `--changes`, which needs a sign-in, roles and someone to
approve, and only after a person approved it
([Changing rules](changing-rules.md)):

- The approval is a form on the proposal's page, in a browser. The API,
  MCP and every token can't approve, nobody approves their own proposal or
  one their agent made, and `--approvals 2` asks for two people
  ([Approving a proposal](changing-rules.md#approving-a-proposal)).
- The change is checked again first: refused if the rule changed, or if the
  backtest now says something worse than what was approved.
- It goes to one of: a pull request, opened by a recipe that runs where the
  repository's credentials are (Lapilli holds none); Grafana's provisioning
  API, with `--changes-grafana-token-file`; a ruler's API or a cloud's
  alarm API, only in the namespaces and for the alarms `--changes-in` names
  ([Carrying out a change](changing-rules.md#carrying-out-a-change)).
- At most five deletions a day and twenty changes an hour
  (`--changes-limits`). An admin can pause it in the settings, and anyone
  who runs the pod can by making the file `--changes-paused` names
  ([Limits and pausing](changing-rules.md#limits-and-pausing)).
- Turning a rule off, or deleting it, waits while it fires; where a rule
  can be turned off, deleting it is proposed only four weeks after, if it
  is still off ([Retiring a rule](changing-rules.md#retiring-a-rule)). Lapilli writes
  rules and alarms, not mutes, suppressions or snoozes: making one would be
  a vulnerability ([SECURITY.md](../SECURITY.md)).

## Secrets

Tokens, webhook URLs, the Postgres connection string and signing keys are
read from files, not given as flags. The connection string is said
nowhere: not in a log, an error, a page, `/metrics` or an answer to an
agent. A signing key and the signatures are never shown or logged, and the
Sources page shows an error without a credential to anyone
([The ledger in Postgres](serve.md#the-ledger-in-postgres),
[Signed posts to the webhook](serve.md#signed-posts-to-the-webhook),
[Sources and the activity log](serve.md#sources-and-the-activity-log)).
Showing a token or a webhook's URL is treated as a vulnerability
([SECURITY.md](../SECURITY.md)).

To show a report outside, use `--redact`, with `--redact-key` for aliases
that hold across outputs ([Showing a report outside](redact.md)).

## Reporting a vulnerability and checking a release

Report a vulnerability privately, as [SECURITY.md](../SECURITY.md) says.
It also says how to check a release's signatures and attestations
([Checking a release](../SECURITY.md#checking-a-release)).
