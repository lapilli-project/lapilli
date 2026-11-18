# Keep judging

`lapilli serve` keeps a ledger. Every hour it replays the rules over the
hours since its last run, and the hour before that again for points that came
late, and records the firings and their stones. It reads the rules every
minute and records each change as a new version, so every hour is replayed
with the rules as they stood then, and it replays a change over the week
before it to show what the change would have done. Once a week it judges the
week before, works out longer `for`s for the noisy rules and the rules that
nearly caught what nothing caught, and posts the report to Slack. An incident
written down after that, or in the week under way, has the rules that nearly
caught it worked out within the hour.

```sh
lapilli serve --rules alert-rules/ --prometheus http://prometheus:9090 --match service \
  --impact 'sum by (service) (rate(http_requests_total{code=~"5.."}[5m])) / sum by (service) (rate(http_requests_total[5m])) > 0.01'
# then http://localhost:8080
```

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="../assets/serve-dashboard-dark.png">
  <img alt="The lapilli serve overview, with its pages in a sidebar: last week's precision, firings, incidents missed, time to catch (none), noisy rules, notifications for firings that caught nothing while people slept, re-fires and firings during deploys, each with a line of its weeks, most beside the week before, and a link to the list that makes it; under them, links to hide the charts' marks of deploys and quiet windows" src="../assets/serve-dashboard.png" width="820">
</picture>

A web UI opens on a dashboard of the last twelve weeks: last week's
precision, firings, incidents missed, time to catch, noisy rules and the
firings that woke people for nothing beside the week before; firings, precision and incidents by week; where the noise
is, rule by week; and what to look at first (a longer `for` that would quiet
a rule, an incident nothing caught, impact windows to decide on). Beyond it,
each rule over twelve weeks with its versions, the incidents and what caught
them, and each day's firings. People
write incidents down there, confirm or dismiss the windows `--impact` found,
or say a black stone was real; the stones are given again at once, for the
whole ledger, without asking Prometheus. The settings that only judge the
ledger again (`--lead`, `--match`, `--refire`, the noisy thresholds) can be
changed there and tried on the last week first; so can the impact signal,
`--impact` and `--impact-for`, by an admin, under Incident criteria, tried
on the last four weeks first: the windows it would find, and how the
verdict would change. Saved, it is read again over the last four weeks,
which are judged again, and keeps the record from then on, not of the
weeks it wasn't read over, nor of those before every metric it reads had
data. Beside it, Incident sources says where
incidents come from (written in Lapilli, each tool a job copies them in
from, the incidents file, the impact signal) and whether each is alive.
`/settings.yaml` gives the changes as Helm values to commit. Teams, quiet windows, reports and roles
can be declared in Git instead: `--config FILE` (`serve.config` in the
chart) is YAML of them, as the UI's forms make them, which the ledger is
kept holding as the file changes, without a restart; what it declares is
marked declared in the UI, which doesn't change it, and what it no longer
declares is the UI's again, or deleted with `--config-prune`, but a role,
taken away. A file is kept to only once it has been read the same twice
in a row, so one read half written changes nothing. An admin's
`/config.yaml` gives everything the ledger holds in that form, to commit
(docs/design/serveops.md). The Cleanup page (`/orphans`) lists the notes,
tasks,
comments and proposals of rules deleted or renamed, to move to the rule as
it is named now, or delete. The API gives `lapilli verdict --json`'s
report for any period (`/api/v1/report?from=&to=`), its firings as CSV
(`&format=csv`, those quiet windows excused and each rule's team among
them), and takes incidents
(`/api/v1/incidents`); `/metrics` gives `--metrics` over the last seven days,
so Prometheus scrapes it instead of a Pushgateway, and the chart has a
Grafana dashboard of them.

The pages are in a sidebar of four groups, named as a console names them
(Monitoring: Overview, Rules, Incidents, Alert history; Improve: Tasks,
Proposals, Quiet windows; Reporting: Reports, Outcomes;
Administration: Teams, Roles, Settings, Run history, Cleanup), a
menu on a narrow screen, each with a breadcrumb and a bar to pick the team
and the period, which every link carries; a reader who edits one team sees
it first. The bar's search finds rules, incidents and teams, and opens the
one it finds. The rules list gives each rule a badge on three axes (Noisy,
No record, OK, Silent, Spare; Differs, Not replayed, Recorded only; keep, tune,
retire), filters by each, counted, and a search box that takes words of a
name and `is:noisy`, `is:unrecorded`, `severity:critical`, `status:tune`, `has:proposal`,
`label:team=pay`, `group:shop` and `team:pay`; each filter in effect is a
chip that takes it away, and the URL keeps them all; each rule's own labels
are chips too, each picking the rules with it
([docs/design/ui.md](design/ui.md)).

With no incident written down over the weeks shown, or the two before
their end, the pages call no rule noisy and show no precision, and say why
once. For a week like that, the weekly digest says, of each rule replayed
that fired as often as a noisy rule, what its firings were: how many went
well past its bound, the alerts that fired most and whose labels they have;
a rule's page shows it in place of a suggestion, the overview first among
what to look at. Teams own rules, and their firings are counted by the
team whose labels they have: a rule most of whose firings came from what
another team owns says so on its page, on that team's rules list and in
its digest.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="../assets/serve-rules-dark.png">
  <img alt="The rules list filtered to the noisy rules over four weeks: the filters above it, side by side with their counts, the filter in effect as a chip, a bar to make a task for each rule picked, set their team or disposition, or propose a change for each, and the rules, each with a checkbox, its badges, its labels as chips and its numbers" src="../assets/serve-rules.png" width="820">
</picture>

Many can be changed at once: rules picked on the list get a task each, a
team or a disposition, and firings picked are written down as real, each
as its own page would change it, with the same checks and a line each in
the changes log. Up to twenty rules picked can each be proposed a change
(a new `for`, the one suggested for it, retiring it), each the proposal
its own page would make, with its backtest, waiting for a person to
approve it. Each of the overview's numbers opens the list that makes
it, over the same week and team (the noisy rules, the incidents missed);
its weekly charts mark deploys and quiet windows. A day's firings, on
Alert history (`/firings`), are counted by the hour, and each firing's
labels are chips that pick the
firings with them; an incident's page draws when each rule fired around
it.

A rule's page has its badges, what to check about it and what can be done
at its top; five numbers over the period picked (precision, firings,
incidents caught, the notifications it sent, and how alike its backtest is
to what really fired), each beside as long a span before when the ledger
holds all of it; its last 84 days, a cell a day, marked where it changed,
where it fired during a deploy of what it is about and where a quiet
window was over it; and its sections in tabs: summary, firings, versions
(each beside the one before), proposals and activity.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="../assets/serve-rule-dark.png">
  <img alt="A noisy rule's page over four weeks: its name, its badges (noisy six weeks, to tune) and three actions; its team, severity, for, group and the runbook the rule names; precision and firings beside the four weeks before, one better, one worse, incidents caught the same, the notifications it sent and its backtest with nothing beside; its last 84 days as a strip of cells, marked where the rule changed and on the days it fired during a deploy of checkout; its tabs; and what to do first" src="../assets/serve-rule.png" width="820">
</picture>

A proposal's page shows the change as a diff (unified or side by side, the
lines around it or the whole rule), its backtest as a table, now beside as
proposed, the approval, and what happened to it; the proposals and the
tasks have counted facets, as the rules do; a quiet window can be
previewed before it is saved (the rules it applies to, the firings of the
last four weeks it would have excused, when it falls next); `/manage`,
Administration, gathers the pages that set Lapilli up; and an action a
reader may not take
is shown disabled, with why.

What an operations team measures its alerts by is there too, week by week
and team by team (docs/design/noc.md): the share of critical firings that
caught something; firings that re-fired, the alert that did most, and the
whole weeks with one; firings during deploys of what they are about
(`--deploys`, from what `recipes/versions/argocd.sh` and `flux.sh` write:
a firing agreeing with a deploy on every label both have), shown apart,
with precision both ways; incidents and impact windows caught,
caught only by noisy rules or missed, and the lead of the first alert. The
rules, a rule's page, the incidents and the overview show them, the digest
and reports tell them, and `/api/v1/summary` gives them, in a shape that
keeps its schema, to dashboards and other tools.

A team holds its alerting to goals, declared in `--config` under `goals:`
(docs/design/goals.md): each a bound on one of the summary's numbers, for
everyone, a team, or each team (`team: "*"`):

```yaml
goals:
  - {name: critical-caught, measure: critical.share_caught, at_least: 0.8}
  - {name: none-missed, measure: incidents.missed, at_most: 0}
  - {name: paged, team: payments, measure: critical.firings, critical: [critical, page], at_most: 40}
```

Each whole week is met, missed or not known (with why: no record, nothing
to divide, a week the ledger holds only part of), never met for a week
nobody wrote anything down. The Goals page shows each goal's twelve weeks;
the overview, the digest, reports and `/api/v1/summary` (`goals`) the
results; `/metrics` the last whole week's (`lapilli_goal_met`,
`lapilli_goal_known`, `lapilli_goal_value`, `lapilli_goal_weeks_met`), for
a team's own Prometheus to alert on. A goal missed is told
(`goal.missed`), and becomes a task with one press, or by itself with
`--goal-tasks`. A goal of `each: sale` is checked for each sale instead
(a marked window's kind, below), and `no_worse_than: before` sets a
sale's precision or median lead beside the four weeks before it:

```yaml
goals:
  - {name: sale-no-worse, each: sale, measure: precision, no_worse_than: before, by: 0.05}
  - {name: sale-none-missed, each: sale, measure: incidents.missed, at_most: 0}
```

Some weeks aren't like the others: a sale, a launch, a campaign. Marked
windows (docs/design/marked.md) name them, each of a kind a team chooses,
from a file (`--marked`), `--config`'s `marked:`, the Marked windows page,
or a PromQL expression that returns something while one is on (`--mark
sale=shop_sale_active == 1`). A firing that starts during one and agrees
with it is marked with its kind; nothing is excused, and every count is
read inside the kind and outside it: precision during sales beside the
four weeks before, the rules noisiest during them (`during:sale` on the
rules), and the incidents missed in each.

The ledger is one SQLite file (`--ledger DIR`), or a Postgres database
(`--ledger-url-file FILE`, below). It keeps each firing's stones
and each rule's by day and by week, so reading twelve weeks costs what
reading one does, and a change gives stones again only to the firings near
it. Measured with two million firings (a thousand an hour for twelve
weeks): an hourly run 0.18 s, writing an incident 52 ms, twelve weeks by
rule 2 ms, judging everything again after a change to the settings 15 s;
the pages, worked out afresh over 200,000 firings, 1 to 21 ms.

It isn't only to look at. Teams own rules (by a note, a name pattern, or
labels, in that order: a team that owns by labels can't take a rule another
team's pattern names) and incidents, so the dashboard, the rules and the tasks can be shown for
one team. Rules that say whose they are in a label (`team`, `owner`,
`squad`, or those `--team-labels` names) make their teams with one press:
the Teams page suggests one for each value on rules no team owns, and a
rule's page says how its team was found. A rule keeps a note: its team, whether to keep, tune or retire
it, its runbook. A quiet window (a deploy, maintenance, a load test)
excuses the firings in it that caught nothing, so expected noise doesn't
weigh on a rule's precision. A suggestion on the dashboard becomes a task
with a press, with a team, an assignee, a due date, a status and a link
to the issue in your tracker, and rules, incidents and tasks take
comments. A quiet window can repeat every day or every week, for a
nightly batch or a weekly release; every month, on a day (`15`), the last
(`last`) or a weekday of the month (`first sat`), for maintenance; or by a
cron expression (`0 2 * * 6`), hourly at most. Its start and end are its
first repeat, which must be one of its schedule's times, and each repeat is
shorter than the time between two; daylight saving is counted on the
calendar, as the reports' is. A quiet window can also be a team's: it
excuses only the firings of rules the team owns, and its page names the
team of each firing it excused. A team can keep its own working hours,
hours asleep and time zone, or its shifts (on the teams page, or
`work_hours`, `sleep_hours`, `tz` and `shifts` in the teams API): its rules'
firings that caught nothing are counted asleep and off hours by those, every
other rule's by `--work-hours`, `--sleep-hours`, `--tz` and `--shifts`. All of it is in the same ledger,
through the UI or the API; see [docs/design/work.md](design/work.md).

The weekly digest can go where each team reads it. `--team-digest TEAM=FILE`
posts a team its own rules and incidents, to the Slack-style incoming webhook
whose URL is in the file, and a team nothing happened to hears nothing.
`--notify-webhook-file` gets `digest.ready`, for everyone and for each team, with
the numbers as JSON and the report as `text`, for any chat or script to pass on.
`text` is the report as written, from rule names and incident titles others
wrote, so escape it for where it goes; `slack_text` is it as the Slack digest
posts it, with mentions and links escaped. The other fields that name things
(`noisy_rules`, `spare_rules`, `sources`, `notifications.most_for_nothing`,
`notifications.by_receiver`) are as written too: escape them as well.
Over twelve weeks, the digest also names the rules that are spare.

Reports go on schedules people set themselves, on the Reports page or with
`/api/v1/reports`: `daily@09:00`, `weekly mon@09:00`, `monthly 1@09:00` (or
`last`), or a cron expression such as `0 9 * * 1-5`, hourly at most, in the
ledger's zone or the team's. A preset covers the day, the week from Monday or
the month before it; a cron expression, the time since the one before, or a
span given with it (`24h`, `7d`). A report is for everyone or one team, and
holds the sections picked: `summary` (beside the span before), `noise` (and
what turned noisy), `missed`, `woken`, `notifications`, `replay`, `sources`,
`tasks`, `content` and, for a week or longer, `suggestions`. It goes to
destinations by name, never a URL typed in a page: `--report-dest NAME=FILE`
(repeatable, a Slack-style incoming webhook; in the chart,
`serve.reports.destinations`), `slack`, `team:NAME`, and `webhook`, which gets
`report.ready` with each section's numbers as JSON and the text as `text` and
`slack_text`; the fields that name things are as written, so escape them as
`digest.ready`'s. Each report is sent once for each span, through the outbox,
when the ledger has the span, or with what there is, saying so, once it fell
due six hours ago or half the time to the one before, whichever is less;
after a restart, only the last one due, and only while less time has passed
since it fell due than between it and the one before. A time daylight saving
skips falls due when the clocks jump; a repeated time, the first time.
Admins make and change reports; everyone sees them and a preview of each.
A team's editors, and editors, make a team's, sent to the team's own
destination (`team:NAME`).

The weekly digest is shown first among them. The flags define it until an
admin changes it on the Reports page or with `PUT /api/v1/reports/digest`
(`{"schedule": "weekly tue@10:00", "dests": ["slack", "ops"], "enabled":
true}`): from then on the ledger does, `--digest` isn't used (the log says
so at start), and the flags only name where it can go, now any destination,
a `--report-dest` too. It stays weekly, of the week from Monday, and its
suggestions are worked out at its time whether it is sent or not. A week
sent isn't sent again, before or after the change, to old destinations or
new; a week that falls due while it is off is passed over; and moving its
day never sends an older week. `DELETE /api/v1/reports/digest`, or "Back to
the flags", gives it back to the flags; see
[docs/design/reports.md](design/reports.md) and
[docs/design/serveops.md](design/serveops.md).

What happens (a task made, assigned, done or overdue, an incident written
down, a quiet window kept, a comment) can be told to a webhook, as JSON,
with `--notify-webhook-file`, and to Slack, as a line in `--lang`, with
`--notify-slack`. A tracker, a chat or a script takes the webhook. To keep
tasks in step with issues in GitHub, GitLab, Jira or Linear, run the
tracker's script in [recipes/trackers](../recipes/trackers) every few minutes
with `--tasks-token-file` (below). Notices are written in the
same transaction as what they tell of and sent after, so a receiver that
is down holds nothing up and misses nothing it comes back for.

Whether Lapilli itself earns its keep is on the Outcomes page (`/outcomes`),
worked out
from the same ledger. Each change to the rules it reviewed is measured: the
replay of the week before was its prediction, and the four weeks either
side are what came of it, as firings a week, incidents caught, firings
fewer since than at the rule's rate before (a rule written counts as
more), and whether it came out as predicted. Beside them: the weeks before
`lapilli serve` began and the latest (precision, firings and ● a week,
incidents missed); whether the record is kept up (two weeks without an
incident written down is flagged, since every firing then reads as ●;
impact windows to decide; failed runs; rules not replayed); and whether it
is used (edits and by how many people,
suggestions acted on, tasks done). The overview sums it up, the digest has
a line of it, `/api/v1/outcomes` has it as JSON, and `/metrics` has
`lapilli_serve_firings_avoided`, `lapilli_serve_rule_changes`,
`lapilli_serve_incident_last_timestamp_seconds` and `lapilli_serve_edits`,
so an alert can say when the record goes quiet. Nothing is sent anywhere,
and reading a page isn't counted.

The UI is checked against WCAG 2.2 AA (axe-core over every page, and tests
for each page's structure, contrast and charts, whose numbers are in tables
too), and works at 320px and 200% zoom, with no script.

The UI speaks English, Spanish, Japanese and Korean: the browser's
language, or the one picked at the top of the page; `--lang` picks the
language of what `serve` posts to Slack and in its reports. Each language is
one file of translations, `internal/serve/i18n/LANG.json`; a test fails if a
text has none, so a new language can't be left half done. The Spanish and
Japanese were written with a machine's help, in the plain words SREs use, and
want a native speaker's review: corrections are welcome as pull requests.

It listens on `127.0.0.1:8080`. To serve it at a URL people open, sign them
in, one way of three ([docs/design/signin.md](design/signin.md)):

- `--oidc-issuer`, `--oidc-client-id` and `--oidc-client-secret-file`:
  lapilli signs people in itself with any OpenID Connect provider, as
  Grafana and Argo CD do, and keeps a session in a signed cookie;
  `POST /auth/logout` ends it. Register `--external-url`'s `/auth/callback`
  as the client's redirect URI. An API client or an agent sends an ID token
  the provider gave this client as a bearer token.
- `--proxy-jwt google-iap`, `aws-alb` or `jwks`: the JWT a sign-in in front
  signs (Google Cloud's Identity-Aware Proxy, an AWS Application Load
  Balancer's sign-in, oauth2-proxy and the rest), checked against its keys
  on every request.
- `--user-header`: the plain header a proxy that signs people in sets, so
  changes are recorded under it; the proxy must then be the only way in.

With the first two, every page and API call but the health checks needs
someone signed in, and only those a role names come in: an identity
provider signs in whoever it knows, which for a public one (Google's for
any account, GitLab.com, GitHub through Dex, a multi-tenant Entra ID app)
is anyone. `--viewers` names who may look; `--editors` and `--admins` who
may change. `--user-claim` (`email`) and `--groups-claim` (`groups`) name
the person and their groups, and `--oidc-hosted-domain` checks Google's
Workspace domain. `--read-only` takes no changes. With `--editors` and
`--admins`, people or groups (`--groups-header`, `--groups-claim`), only
editors may write and only admins may change the settings and the teams;
the rest may only look. Lists take `*@DOMAIN`, everyone whose name ends in
`@DOMAIN`, and `*`, everyone signed in, which with a sign-in of lapilli's
own gives a role only once `--viewers` says who comes in. Write
`user:NAME` or `group:NAME` where someone could pick a name that is a
group's. Sessions are signed with `--session-key-file`'s key, or one made
for the process; with an http `--external-url` (this machine only), give
it a name of its own under `.localhost`, as cookies aren't kept to a
port.

With no roles, who may change what depends on where it listens. On
loopback alone (`127.0.0.1`, `localhost`, the default), everyone who
reaches it is an admin: that is this machine's users. Beyond it (`:8080`,
as the chart listens, a pod's address), everyone may only look, since every
pod or machine that shares its network can reach it; give roles behind the
proxy, or `--everyone-admin` to let everyone change anything anyway. The
log says at start which applies. (Before 0.28.1, everyone was an admin
wherever it listened.) With `--oidc-issuer` or `--proxy-jwt`, loopback
makes no one an admin: everyone signed in may only look until roles say
otherwise, and `--everyone-admin` is refused.

With `--admins`, admins can also give roles on the Roles page (from the
Teams page) or with `/api/v1/roles`, kept in the ledger: an editor's, of
every team or of one, to `user:NAME` or `group:NAME`. A team's editors
change that team's tasks, quiet windows and reports, notes on the rules it
owns, and comments on what is its, and nothing of another team's or of
everyone's; the ledger checks each in the write. The UI never makes an
admin or an approver, and can't take away a role the flags give: those stay
`--admins`, `--editors` and `--approvers`. Only admins see who holds a
role. A team removed takes its editors with it. A team's name is unique in
any letter case. With `--config`, the roles it declares (`grants`) are
taken away there alone.

`--proposals-expire 30d` closes a proposal nobody approved within thirty
days as expired, so its rule can have another; off by default.

With `--user-header`, the proxy must be the only way in. The chart's
`serve.networkPolicy` lets in only the proxy (or the ingress controller,
the Gateway, the load balancer in front), and Prometheus only to
`/metrics` on a port of its own (`--metrics-listen`). With
`serve.userHeader`, the chart refuses to go without it. The chart serves
the UI at a URL with `serve.service` (a LoadBalancer or NodePort),
`serve.ingress` or `serve.httpRoute` (Gateway API), and `serve.signIn`;
its README has each cloud's load balancer. A proposal's backtest takes up
to `--timeout`; a proposal for many rules starts none after twenty seconds
and says which weren't tried. Where the proxy in front gives a request less
than `--timeout` (30 seconds by default for some load balancers' backends),
raise it for `POST /proposals` and `POST /rules/bulk`, or lower
`--timeout`.

Posts from other sites are refused, and the pages run no scripts. Only host
names it is reached by are answered, so a page elsewhere can't point a name
of its own at it (DNS rebinding):

- an address, `localhost`, and names under `.svc`, `.cluster.local` or
  `.internal`;
- the name in `--external-url`;
- what `--allowed-hosts` adds, such as the name a proxy passes on.

Others get 421.

In the chart, `mode: serve` runs it as one replica with the ledger on a
PersistentVolumeClaim, behind a ClusterIP Service:

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set mode=serve --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' --set 'match={service}'
kubectl -n monitoring port-forward svc/lapilli 8080
```

A release that changes the ledger's schema upgrades the ledger as it
starts, and the releases before can't open it after: keep a copy of the
ledger before such an upgrade, to go back to
([Going back after a schema upgrade](../charts/lapilli/README.md#going-back-after-a-schema-upgrade)).

## Before it is deployed

`lapilli preflight` takes serve's flags, as serve reads them, and says
what serve will meet, before it is deployed: whether it reads the rules
and how many it can replay, whether it reaches the history and signs in to
it, whether Grafana's token reads what serve reads, what the files and the
tokens hold, the ledger it would open, and how many requests an hourly
run, the backfill and what is asked beside them will make, how long they
take under `--max-rate`, over how many nights of `--catch-up-hours`, and
how much memory to give the pod (docs/design/preflight.md). Each item is
ok, a problem or unknown, with why, and for a problem what to change:

```sh
lapilli preflight --rules alert-rules/ --prometheus http://prometheus:9090 \
  --max-rate 0.5 --catch-up-hours 01:00-06:00 --catch-up-rate 0.5
```

```
lapilli preflight: 12 items: 10 ok, 0 problems, 2 unknown; 4 requests asked
ok       rules.read          203 alert rules (Prometheus), 40 recording rules, from --rules
ok       rules.replayable    203 can be replayed
ok       history.reach       [--prometheus] answered vector(1) in 38ms, with no credentials
unknown  history.alerts      no firing alert in the last day, or no ALERTS kept: memory is the caps'
                             bound
...
ok       ledger.sqlite       a new ledger: serve makes it, at schema 019
Estimates
ok       hourly run          about 253 requests a run, the most 430, 6,054 a day: at --max-rate 0.5
                             a run takes 8m at least, the most 14m; once caught up, with what is
                             asked beside them, 14% of --max-rate
ok       backfill            12,040 requests over 28d, 28 pieces of a day at most, the most 430: at
                             least 6h41m at 0.5 a second
                             why: each day's in 14m at most, within --timeout 50m
ok       nights              2 nights at least, in 01:00-06:00 at 0.5 a second, 9,000 of the
                             backfill's requests a night
ok       memory              no firing read: the caps' bound, 5.2 GB (8 x 10,000,000 x 64 bytes,
                             beside 64 MB of caches): a request of 5.2 GB, a limit of 10.5 GB
```

It writes nothing and replays nothing, held by the code: every request it
sends goes through an allowlist that refuses any other before it is sent
(the history's `vector(1)` and a day's count of ALERTS; the rules, the data
sources and the notifications Grafana is read for; the sign-in's discovery
and keys; the clouds' credentials; and, with `--changes`, what a token or a
cloud's identity may write: Grafana's list of the token's actions, IAM's
policy simulator, Cloud Resource Manager's `testIamPermissions`, Azure's
permissions, deny assignments and locks, OCI's policies), the ledger is
read through a probe that
opens SQLite read-only and Postgres in read-only transactions, and no rule,
`--impact` nor `--guard` is evaluated. What the clouds' credential chains
do of their own is outside the allowlist: Google's metadata server (and
whether it is there, off Google Cloud), the Azure CLI (which may write its
token cache), OCI's instance and resource principals and OKE's workload
identity (its auth service's POST among them), and the processes AWS's
`credential_process` and Google's executable credentials start. What it prints is redacted as a
pull request's backtest comment is. It exits 0 with no problem, 1 with
one (or an unknown, with `--fail-unknown`), 2 when the flags are wrong;
`--json` gives the report with ids and fields that stay the same
(docs/numbers.md). Run it where serve runs, with its service account and
volume, as a Job or `kubectl run` of the image; in CI, `--offline` asks
nothing, reads no secret's file nor `--ledger-url-file`, and works out what
needs no credential: the flags, the rule files, `--config` and the
estimates of requests and time. With `--changes`, it asks what
`--changes-grafana-token-file`'s token and each `--changes-*` identity may
write, of an API that says so without anything being written
(`grafana.write`, `changes.aws`, `changes.azure`, `changes.google`,
`changes.oci`): AWS's needs `iam:SimulatePrincipalPolicy` on the role's own
ARN, Azure's `Microsoft.Authorization/permissions/read`,
`denyAssignments/read` and `locks/read`, and OCI's that the user may
inspect groups, users, compartments and policies in the tenancy, or the
item is unknown; an OCI policy with a condition is never counted as a
grant. A Mimir's or a Cortex's `rules:write` can't be known without
writing: unknown.

## The ledger in Postgres

A SQLite file is one disk's. To keep the ledger where you keep the rest, with
its backups, point in time recovery and failover, give `lapilli serve` a
Postgres database, 15 or later (docs/design/postgres.md):

```sh
umask 077   # the file only you can read
cat >ledger-url <<'URL'
postgres://lapilli:PASSWORD@db.example:5432/lapilli?sslmode=verify-full
URL
lapilli ledger copy --ledger lapilli-ledger --to-url-file ledger-url   # once, with serve stopped
lapilli serve --ledger-url-file ledger-url --rules rules/ --prometheus http://prometheus:9090
```

The connection string is read from the file, as pgx reads one, and said
nowhere: not in a flag, a log, an error, a page, `/metrics` or an answer to
an agent. To another machine, say `sslmode=verify-full`: a string that says no
`sslmode`, or pgx's default `prefer`, which checks no certificate and falls
back to none, is refused at start; `require` or `disable` is taken at its word,
and serve warns. Give the database to the ledger's role alone (`REVOKE CONNECT
... FROM PUBLIC`). Every page, report and answer is what the
SQLite ledger gives from the same writes, and the tests run on both.
`lapilli ledger copy` copies a ledger into an empty one, a SQLite ledger
into Postgres or one back, in one transaction, and reads both back to
check; it is refused while serve holds the ledger. In the chart,
`serve.ledger.postgres.secret` names the Secret holding the string, and no
PersistentVolumeClaim is made.

## Several replicas

On a Postgres ledger, `lapilli serve` runs as several replicas
(docs/design/replicas.md). One leads, on a lease the database keeps, and
runs what must run once: the runs and the backfill, the jobs, the digest
and the reports, the outbox, changes carried out, the rules watched, the
on-call feeds. Every replica serves the UI, the API, MCP, sign-in and the
ports that take alerts and incidents, and writes what it is sent. A
follower takes the lease within some 17 seconds of the leader being killed
or cut off (15 s measured, the leader `SIGKILL`ed), within 2 when it
stops; the old leader stops first, by its own clock, and what it would
still write as the leader is refused in the database. Notices are claimed
one at a time, so two leaders for a moment send none twice. A write on one
replica shows on the next request to any other. In the chart:

```sh
helm upgrade lapilli ... --set serve.ledger.postgres.secret.name=lapilli-ledger --set serve.replicas=3
```

They roll one at a time, with a PodDisruptionBudget; `/readyz` and
`lapilli_serve_leader` say which pod leads. More than one replica needs
Postgres: the chart refuses `serve.replicas` above 1 on a SQLite ledger,
and a sign-in of lapilli's own needs the session key from a Secret of your
own, which every pod mounts. From 0.35 or before, upgrade with one
replica first: its pod holds the ledger alone, and refuses new ones until
it stops.

## Firings recorded elsewhere

A cloud's own alarms that aren't written in PromQL can't be replayed, but
what they fired can be kept and judged with the rest. With
`--firings-token-file FILE`, the port `--incidents-listen` opens takes
firings too, from a job holding that token, its own: a token that may copy
incidents can't write firings, and the reverse, and neither may be the
notifications token, which alert managers keep in webhook URLs.

```sh
export SINCE=2026-11-08T00:00:00Z UNTIL=2026-11-09T00:00:00Z
recipes/firings/cloudwatch.sh > firings.jsonl &&
  LAPILLI_TOKEN_FILE=firings-token recipes/firings/put.sh http://lapilli:8082 cloudwatch-prod < firings.jsonl
```

`put.sh` sends them as `PUT /api/v1/firings/SOURCE?from=&to=`, one a line
as `--firings` reads them. The source is a name the job picks
(`cloudwatch-prod`, `azure-shared`), and the period replaces: the source's
firings that start in `[from, to)` are those sent, the way a run replaces
the firings that start in its period. Sent again as they are, nothing
changes and nothing is judged again, so a job can send the last day every
hour, and everything the cloud keeps every week, which ends the alarms that
were still on. A firing without an end is still on until one is sent with
its end. A write is 31 days, 16 MB and 100,000 firings at most, nothing in
it starts or ends more than an hour ahead of lapilli's clock, and names have
no character that doesn't show. The ledger keeps 50 sources, 5,000 rules
from each and 20,000 from all, and 500,000 alerts from each; alerts with no
firing left go with `--keep`. The token writes twenty periods at once, for a
first copy of what the cloud keeps, then one each five seconds. A write
judges again as far as a period past its period: an alert's next firing
after one that ran on longer is judged again when a write or a run reaches
it.

Such a rule is named as its source names it, and told apart when another
rule has the name (`checkout-5xx [cloudwatch-prod]`); a replayed rule that
comes later takes the name. Its firings are judged as any are, beside the
replayed rules: stones, precision, what it adds, incidents written down
later, teams, hours, quiet windows, tasks, the digests, `/metrics` and the
agent's reading tools. What needs a rule's condition stays with the
replayed ones: change reviews, longer `for`s, near misses, `compare`. Its
page says where it was recorded and why. The Run history page lists the
sources,
`/api/v1/sources` has them, and `/metrics` has
`lapilli_serve_source_last_timestamp_seconds{source}`, so an alert can say
when a job stopped copying. Lapilli says so itself, too: a source nothing has
been copied in from for `--source-quiet` (26 hours unless given; give a
weekly job's 8d) is told of once, to the webhook and, with `--notify-slack`,
to Slack. The weekly digest says which sources firings were copied in from,
how many each brought that week, and which has gone quiet.

With the firings token, `--rules` and `--prometheus` are optional: a team
with only cloud alarms runs `lapilli serve --incidents-listen :8082
--firings-token-file FILE`, nothing is replayed, and what comes in is
judged as it comes. `--impact` still needs `--prometheus`. In the chart,
`serve.firings.tokenSecret` names the Secret.

`lapilli serve -h` lists the rest: how often to run (`--every`), how far to
replay on the first start (`--backfill`, 28 days) and how long to keep
firings (`--keep`, 400 days).

## Tasks kept in step with a tracker

A task says who does what by when; the work is tracked where the team tracks
the rest. With `--tasks-token-file FILE`, the port `--incidents-listen` opens
lets a job holding that token, its own, read tasks and set their link and
status, and nothing else. [recipes/trackers](../recipes/trackers) has a script
for each of GitHub, GitLab, Jira, Linear, Azure Boards and Shortcut, alike: each run makes an issue
for each open task without one, with a link back to the task's page, and
puts the issue's URL on the task; then a task whose issue was closed is
closed, `done` or `dropped` as the issue was, and one closed that way is
opened again with its issue. With `CLOSE_ISSUES=true`, an issue whose task
someone closed in Lapilli is closed too, unless the issue changed since.
Only issues the script's own account made for a task are taken for it,
followed or closed, by the marker line the script ends them with: anyone
may write a task's marker in an issue, or in a task's text, and an editor
may link a task to any issue. Who closed a task, and when, comes from who
set its status (`status_by`, below), not from whoever last edited it. A
task the tracker refuses is said and skipped, and the run fails once the
rest are done; one whose issue was deleted is said and left as it is. A
GitHub App's or Actions' token can't say who it is: set `GITHUB_LOGIN`
(`NAME[bot]`). With `COMMENTS=true`, comments are kept in step too, both
ways, each once: people's comments on a task are posted to its issue,
quoted, and the issue's comments copied to the task, shown as the
tracker's, with the token's `GET` and `POST /api/v1/tasks/{id}/comments`;
an edit after that isn't followed.

```sh
GITHUB_REPO=example/alerts GITHUB_TOKEN_FILE=gh-token \
  LAPILLI_TOKEN_FILE=tasks-token recipes/trackers/github.sh http://lapilli:8082
```

The job reads tasks with `GET /api/v1/tasks?linked=&status=&updated_after=&after=&limit=`
(`updated_after`, an RFC 3339 time, leaves out the tasks not changed since),
each with `status_by` and `status_at`: who set the status it has, and when,
as the changes kept say (a later edit of anything else doesn't count;
neither is given once the change that set it is pruned). A
link is an `https` URL of 2,048 characters at most, set once; the page
shows it with `rel="noopener noreferrer"`. The token can't make, delete or
retitle a task, read anything else, or hold the same value as another token.
What it writes is noted as `tasks-token` and isn't counted as anyone's use.
In the chart, `serve.trackers` names its Secret and, with `tracker`, runs the
script as a CronJob.
